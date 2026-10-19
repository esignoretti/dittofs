package nbd

import (
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/testruntime"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// integrationShare is the share the fixture creates, and integrationVolume the
// volume inside it, so the export name a client asks for is
// integrationShare + "/" + integrationVolume.
const (
	integrationShare  = "/export"
	integrationVolume = "boot"
)

// integrationExportName is the name the handler's exporter resolves.
const integrationExportName = integrationShare + "/" + integrationVolume

// integrationIdentity is the synthetic identity the adapter serves as, and the
// identity the write fixture creates its volume with.
//
// The two must match, and that is a property of the adapter rather than of the
// test: NBD carries no client identity, so every metadata operation runs as the
// one configured identity, and a volume the adapter must write has to be owned
// by it. A fixture that creates the volume as one identity and writes as
// another is refused by the metadata layer's permission gate before the block
// store is reached — correctly, and that refusal has its own test.
var integrationIdentity = Identity{UID: 65534, GID: 65534}

// writeVolumeFixture is the runtime, the resolved export, and the real block
// store for a volume a write test can reach.
//
// It exists because the connection tests' newTestHandler passes nil for meta,
// which is correct for READ (nothing on that path touches metadata) and wrong
// for WRITE, which calls PrepareWrite and CommitWrite. A write test that
// expects Error == 0 needs a real service whose store holds the same share the
// fake block store stands in for. Tests that assert a rejection do not need
// this: handleWrite returns before metadata is reached.
type writeVolumeFixture struct {
	rt  *runtime.Runtime
	exp *Export
	bs  *engine.Store
}

// newWriteVolumeFixture builds the runtime, volume file and export once, with
// the volume owned by the identity the handler serves as.
func newWriteVolumeFixture(t *testing.T) *writeVolumeFixture {
	t.Helper()
	rt, name, bs := newIntegrationFixtureAs(t, integrationIdentity)
	exp, err := NewRuntimeExporter(rt, integrationIdentity).Resolve(context.Background(), name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	return &writeVolumeFixture{rt: rt, exp: exp, bs: bs}
}

// newHandler builds a handler over st with the fixture's real metadata service.
// The handle on exp is the real volume file's, which is what lets PrepareWrite
// resolve the share and the payload.
func (f *writeVolumeFixture) newHandler(t *testing.T, st BlockStore) *Handler {
	t.Helper()
	return NewHandler(exporterFor(f.exp), resolverFor(st), f.rt.GetMetadataService(),
		integrationIdentity, testTimeouts(), func() bool { return false })
}

// newIntegrationFixture builds a runtime with one enabled share holding one
// volume file, and returns the runtime, the export name, and the share's real
// block store.
//
// The journal root comes from testruntime.New, so the local tier is an fs
// store — which is what makes LocalDurable() true and the eviction assertions
// below meaningful.
func newIntegrationFixture(t *testing.T) (*runtime.Runtime, string, *engine.Store) {
	t.Helper()
	// The read tests create the volume as UID 1000 and read it through a
	// handler serving as integrationIdentity: the file is mode 0o644, so the
	// reading identity gets the "other" read bits and the read is permitted.
	return newIntegrationFixtureAs(t, Identity{UID: 1000, GID: 1000})
}

// newIntegrationFixtureAs is newIntegrationFixture with id as the identity the
// share root and the volume are created as. The creating identity matters: the
// file is owned by whoever created it, and a write through the handler runs as
// integrationIdentity.
func newIntegrationFixtureAs(t *testing.T, id Identity) (*runtime.Runtime, string, *engine.Store) {
	t.Helper()
	return newIntegrationFixtureAsSized(t, id, 1<<20)
}

// newIntegrationFixtureAsSized is newIntegrationFixtureAs with an explicit
// export size. The size is fixed at creation and the handler bounds-checks
// against it, so a test whose command sequence reaches past the export's end
// needs its own: a WRITE past the end is NBD_ENOSPC and a READ or TRIM past it
// is NBD_EINVAL, and those are indistinguishable from a real codec bug when
// they show up as a transcript divergence.
func newIntegrationFixtureAsSized(t *testing.T, id Identity, size uint64) (*runtime.Runtime, string, *engine.Store) {
	t.Helper()
	rt, bsID := testruntime.NewWithShareStore(t)
	store := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("nbd-meta", store); err != nil {
		t.Fatal(err)
	}
	// Enabled: true is load-bearing. AddShare copies the flag verbatim and
	// nothing defaults it, so a share built without it is disabled — and the
	// exporter refuses to resolve a disabled share, which would fail every test
	// here with an ErrUnknownExport that names nothing about the real cause.
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name: integrationShare, MetadataStore: "nbd-meta", BlockStoreID: bsID,
		Enabled:           true,
		DefaultPermission: string(models.PermissionReadWrite),
		RootAttr:          &metadata.FileAttr{UID: id.UID, GID: id.GID},
	}); err != nil {
		t.Fatal(err)
	}

	root, err := rt.GetRootHandle(integrationShare)
	if err != nil {
		t.Fatal(err)
	}
	metaSvc := rt.GetMetadataService()
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &id.UID, GID: &id.GID},
	}
	vols, _, err := metaSvc.CreateDirectory(authCtx, root, "volumes", &metadata.FileAttr{Mode: 0o755})
	if err != nil {
		t.Fatal(err)
	}
	volsHandle, err := metadata.EncodeFileHandle(vols)
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := metaSvc.CreateFile(authCtx, volsHandle, integrationVolume, &metadata.FileAttr{Mode: 0o644})
	if err != nil {
		t.Fatal(err)
	}
	createdHandle, err := metadata.EncodeFileHandle(created)
	if err != nil {
		t.Fatal(err)
	}
	// Creation zeroes the size, so the export size is set explicitly: it is
	// fixed at creation and the handler bounds-checks against it.
	if _, err := metaSvc.SetFileAttributes(authCtx, createdHandle, &metadata.SetAttrs{Size: ptr(size)}); err != nil {
		t.Fatal(err)
	}

	bs, err := rt.GetBlockStoreForShare(integrationShare)
	if err != nil {
		t.Fatal(err)
	}
	return rt, integrationExportName, bs
}

// integrationHandler builds the handler the way the adapter does: the real
// runtime exporter, and a store resolver wrapping common.ResolveForWrite.
func integrationHandler(rt *runtime.Runtime) *Handler {
	id := Identity{UID: 65534, GID: 65534}
	resolver := func(ctx context.Context, exp *Export) (BlockStore, error) {
		return common.ResolveForWrite(ctx, rt, exp.Handle)
	}
	return NewHandler(NewRuntimeExporter(rt, id), resolver, rt.GetMetadataService(),
		id, testTimeouts(), func() bool { return false })
}

// readThroughHandler drives one READ over the wire and returns the reply header
// and payload.
func readThroughHandler(t *testing.T, h *Handler, offset uint64, length uint32) (*Reply, []byte) {
	t.Helper()
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, integrationExportName)
	return readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 42, Offset: offset, Length: length,
	})
}

// TestIntegration_ReadAfterWriteOverRealStore is the end-to-end path over a
// real engine.Store: bytes written into the volume come back byte-exact through
// the handler's READ.
func TestIntegration_ReadAfterWriteOverRealStore(t *testing.T) {
	rt, _, bs := newIntegrationFixture(t)
	h := integrationHandler(rt)

	const size = 64 << 10
	want := make([]byte, size)
	for i := range want {
		// Non-zero throughout, so a read that returns zeros fails rather than
		// matching an unwritten range.
		want[i] = byte(i%251) + 1
	}
	ctx := context.Background()
	if _, err := bs.WriteAt(ctx, integrationPayloadID(t, rt), nil, want, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	rep, got := readThroughHandler(t, h, 0, size)
	if rep.Error != 0 {
		t.Fatalf("READ reply error = %d, want 0", rep.Error)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("READ returned %d bytes that do not match what was written", len(got))
	}
}

// TestIntegration_ReadEvictedButManifestCoveredRange pins the hydrate path: a
// range whose bytes have been evicted locally but are covered by the manifest
// must read back real bytes, not zeros and not an error.
func TestIntegration_ReadEvictedButManifestCoveredRange(t *testing.T) {
	rt, _, bs := newIntegrationFixture(t)
	h := integrationHandler(rt)

	const size = 64 << 10
	want := make([]byte, size)
	for i := range want {
		want[i] = byte(i%251) + 1
	}
	ctx := context.Background()
	payloadID := integrationPayloadID(t, rt)
	if _, err := bs.WriteAt(ctx, payloadID, nil, want, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// DrainAllUploads is what makes the bytes evictable: it force-flushes every
	// dirty range to the remote and waits for the uploads to settle. Flush alone
	// is not enough — with a durable local store and no strict enforcement it
	// returns early without draining the remote, so the bytes stay dirty and
	// journal.Evict skips any segment holding a dirty record.
	if err := bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}
	ev, err := bs.DrainLocalSynced(ctx)
	if err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}
	// This assertion is the test's reason for existing: without it a fixture
	// change that keeps the bytes local turns this into a test that passes while
	// proving nothing.
	if ev.SegmentsEvicted == 0 {
		t.Fatalf("nothing was evicted, so this test cannot exercise the hydrate path: %+v", ev)
	}

	rep, got := readThroughHandler(t, h, 0, size)
	if rep.Error != 0 {
		t.Fatalf("READ of an evicted range returned error %d, want 0", rep.Error)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("READ of an evicted but manifest-covered range did not return the written bytes")
	}
}

// TestIntegration_ReadNeverWrittenRangeIsZeros pins the engine's hole contract:
// a range inside the export with no write ever reads as zeros, not as an error.
func TestIntegration_ReadNeverWrittenRangeIsZeros(t *testing.T) {
	rt, _, _ := newIntegrationFixture(t)
	h := integrationHandler(rt)

	rep, got := readThroughHandler(t, h, 0, 4096)
	if rep.Error != 0 {
		t.Fatalf("READ of an unwritten range returned error %d, want 0", rep.Error)
	}
	if !bytes.Equal(got, make([]byte, 4096)) {
		t.Fatal("READ of an unwritten range did not return zeros")
	}
}

// integrationPayloadID resolves the volume's payload ID the way the handler
// does, through the exporter, so the test writes where the handler reads.
func integrationPayloadID(t *testing.T, rt *runtime.Runtime) string {
	t.Helper()
	exp, err := NewRuntimeExporter(rt, Identity{UID: 65534, GID: 65534}).Resolve(context.Background(), integrationExportName)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return string(exp.PayloadID)
}

// TestIntegration_WriteDeniedByPermissionIsEPERM covers the arm that is easiest
// to leave out and hardest to notice: the metadata permission funnel returns
// ErrAccessDenied (POSIX EACCES) for a denial by file permission bits, which is
// a different code from ErrPermissionDenied (POSIX EPERM). With no arm for it
// the error falls through mapError's default and reaches the client as NBD_EIO
// — a hardware failure reported for an authorization decision.
//
// The fixture creates the volume as UID 1000 with mode 0o644 and serves the
// write as integrationIdentity, so only the "other" bits apply and the write is
// refused. That mismatch is the point of the test, not an accident: the
// identity the adapter serves as is not the identity that owns the volume.
func TestIntegration_WriteDeniedByPermissionIsEPERM(t *testing.T) {
	rt, name, _ := newIntegrationFixtureAs(t, Identity{UID: 1000, GID: 1000})
	exp, err := NewRuntimeExporter(rt, integrationIdentity).Resolve(context.Background(), name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	st := newStoreFake()
	h := NewHandler(exporterFor(exp), resolverFor(st), rt.GetMetadataService(),
		integrationIdentity, testTimeouts(), func() bool { return false })

	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, name)

	if rep := writeThroughClient(t, client, 0, make([]byte, 4096), 0); rep.Error != ErrEPERM {
		t.Fatalf("WRITE refused by file permissions reply error = %d, want ErrEPERM (%d)", rep.Error, uint32(ErrEPERM))
	}
	// The refusal happens before the store is touched.
	if st.writes != 0 {
		t.Fatalf("the store saw %d writes for a refused request", st.writes)
	}
}

// TestIntegration_FlushWithCommitAckJournalSucceeds pins the production shape
// the adapter ships in: a share whose local tier is a real journal-backed fs
// store (testruntime.New points JournalRoot at a temp dir) and no strict
// durability enforcement. The flush is answered success because
// engine.Store.Flush fsyncs the local store and returns on the local-durable
// fast path (pkg/block/engine/flush.go:554) without waiting on the remote
// mirror.
func TestIntegration_FlushWithCommitAckJournalSucceeds(t *testing.T) {
	f := newWriteVolumeFixture(t)
	// The precondition is asserted, not assumed: with strict enforcement on,
	// this test would be asserting the opposite policy.
	if f.bs.RequireDurableCommit() {
		t.Fatal("fixture share requires durable commits; this test pins the default async policy")
	}
	if !f.bs.LocalDurable() {
		t.Fatal("fixture local store is not durable; this test pins the fs-local fast path")
	}
	h := integrationHandler(f.rt)

	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, integrationExportName)

	if rep := flushThroughClient(t, client); rep.Error != 0 {
		t.Fatalf("FLUSH on a durable local store reply error = %d, want 0", rep.Error)
	}
}

// TestIntegration_FlushRequireDurableWithUnhealthyRemoteIsEIO pins the honest
// failure: a share that requires durable commits, whose local store is volatile
// and whose remote is not durable, must answer FLUSH with an error rather than
// acknowledge a durability it did not provide.
//
// The local store is forced non-durable because that is the only way to reach
// the rule over a real engine: the fixture's local tier is a journal, and
// engine.Store.Flush returns on the localDurable fast path before the remote is
// consulted at all, so strict enforcement alone changes nothing. This is the
// same lever internal/adapter/common/commit_durability_test.go uses.
func TestIntegration_FlushRequireDurableWithUnhealthyRemoteIsEIO(t *testing.T) {
	f := newWriteVolumeFixture(t)

	setter, ok := f.bs.Local().(interface{ SetDurable(bool) })
	if !ok {
		t.Fatal("the fixture's local store cannot report a durability override")
	}
	setter.SetDurable(false)
	t.Cleanup(func() { setter.SetDurable(true) })
	f.bs.SetRequireDurableCommit(true)
	if f.bs.LocalDurable() {
		t.Fatal("the local store still reports durable after the override")
	}
	if f.bs.RemoteDurable() {
		t.Fatal("the fixture's remote reports durable; the memory remote is volatile by default")
	}

	h := integrationHandler(f.rt)
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, integrationExportName)

	if rep := flushThroughClient(t, client); rep.Error != ErrEIO {
		t.Fatalf("FLUSH that cannot reach a durable store reply error = %d, want ErrEIO (%d)", rep.Error, uint32(ErrEIO))
	}
}

// TestIntegration_WriteThenFlushThenReadBackByteExact is the round trip over a
// real engine.Store: bytes go out through WRITE, the durability barrier is
// crossed by FLUSH, and the same bytes come back through READ on the same
// connection. It is the only test that exercises all three commands against a
// store that really stores.
func TestIntegration_WriteThenFlushThenReadBackByteExact(t *testing.T) {
	f := newWriteVolumeFixture(t)
	h := f.newHandler(t, f.bs)

	const size = 128 << 10
	want := make([]byte, size)
	// Pseudorandom rather than a repeating pattern, so a store that returns
	// the right bytes at the wrong offset cannot match.
	rng := uint32(0x9e3779b9)
	for i := range want {
		rng ^= rng << 13
		rng ^= rng >> 17
		rng ^= rng << 5
		want[i] = byte(rng)
	}

	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, integrationExportName)

	if rep := writeThroughClient(t, client, 0, want, 0); rep.Error != 0 {
		t.Fatalf("WRITE reply error = %d, want 0", rep.Error)
	}
	if rep := flushThroughClient(t, client); rep.Error != 0 {
		t.Fatalf("FLUSH reply error = %d, want 0", rep.Error)
	}

	rep, got := readReplyAndPayload(t, client, &Request{Type: CmdRead, Cookie: 0x79, Offset: 0, Length: size})
	if rep.Error != 0 {
		t.Fatalf("READ reply error = %d, want 0", rep.Error)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("READ returned %d bytes that do not match what was written", len(got))
	}
}

// TestIntegration_FlushPersistsPendingMetadata is the assertion that the FLUSH
// barrier has both halves. With deferred commits on (the default,
// pkg/metadata/service.go:131), a WRITE's size/mtime change is held in pending
// state and never reaches the store; only FlushPendingWriteForFile publishes it.
// A barrier that commits the block store alone therefore answers FLUSH with
// success while the file's metadata stays unwritten — a client told "durable"
// whose size and mtime a restart can lose.
//
// The observable is the RAW store's mtime, read through
// rt.GetMetadataStoreForShare rather than the metadata service: the service
// merges pending state into its reads, so a read through it would show the
// write's mtime before any flush happened and the test would pass while
// proving nothing. The middle assertion pins exactly that distinction — the
// mtime must NOT move across the WRITE, only across the FLUSH.
func TestIntegration_FlushPersistsPendingMetadata(t *testing.T) {
	f := newWriteVolumeFixture(t)
	store, err := f.rt.GetMetadataStoreForShare(integrationShare)
	if err != nil {
		t.Fatalf("GetMetadataStoreForShare: %v", err)
	}
	ctx := context.Background()

	before, err := store.GetFile(ctx, f.exp.Handle)
	if err != nil {
		t.Fatalf("GetFile before the write: %v", err)
	}

	// The fixture's export is 1 MiB and the write is well inside it, so the
	// metadata layer's durable-extent clamp cannot withhold the size. The write
	// is what creates the pending mtime this test requires to move.
	st := newStoreFake()
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	if rep := writeThroughClient(t, client, 0, make([]byte, 4096), 0); rep.Error != 0 {
		t.Fatalf("WRITE reply error = %d, want 0", rep.Error)
	}

	// Deferred commit: the write is acked but its metadata is still pending, so
	// the raw store must be untouched. This is the assertion that makes the
	// test about the gap rather than about writes in general.
	pending, err := store.GetFile(ctx, f.exp.Handle)
	if err != nil {
		t.Fatalf("GetFile after the write: %v", err)
	}
	if !pending.Mtime.Equal(before.Mtime) {
		t.Fatalf("the raw store's mtime moved across an unflushed WRITE: %v -> %v (deferred commits are off, so this test cannot exercise the flush)",
			before.Mtime, pending.Mtime)
	}

	if rep := flushThroughClient(t, client); rep.Error != 0 {
		t.Fatalf("FLUSH reply error = %d, want 0", rep.Error)
	}

	after, err := store.GetFile(ctx, f.exp.Handle)
	if err != nil {
		t.Fatalf("GetFile after the FLUSH: %v", err)
	}
	if !after.Mtime.After(before.Mtime) {
		t.Fatalf("the raw store's mtime did not move across a FLUSH: %v -> %v, so the barrier committed the block store but not the file's pending metadata",
			before.Mtime, after.Mtime)
	}
}

// manifestBytes is the number of payload bytes the file's block list
// references. It is the observable a punch moves: metadata.Service.PunchHole
// prunes file.Blocks and persists the pruned manifest, and that pruning is the
// reclaim.
func manifestBytes(file *metadata.File) uint64 {
	var total uint64
	for _, b := range file.Blocks {
		total += uint64(b.Size)
	}
	return total
}

// integrationManifest reads the volume's manifest through the metadata service,
// which is how a test sees what a punch pruned.
func integrationManifest(t *testing.T, rt *runtime.Runtime, exp *Export) *metadata.File {
	t.Helper()
	file, err := rt.GetMetadataService().GetFile(context.Background(), exp.Handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	return file
}

// integrationWriteFixtureBytes writes size bytes of incompressible-looking data
// into the fixture's volume through the handler and returns the handler's
// connection so the caller can drive the next command on it.
//
// The manifest is populated before returning, and asserted to be populated:
// Flush alone is not enough — with a durable local store it returns before the
// remote drain (pkg/block/engine/flush.go:554), so file.Blocks is still empty
// and a punch would have nothing to prune. A test written without
// DrainAllUploads passes for the wrong reason.
func integrationWriteFixtureBytes(t *testing.T, f *writeVolumeFixture, size int) (net.Conn, chan error) {
	t.Helper()
	h := f.newHandler(t, f.bs)
	client, done := serveHandler(t, h, f.exp.Name)

	want := make([]byte, size)
	rng := uint32(0x9e3779b9)
	for i := range want {
		rng ^= rng << 13
		rng ^= rng >> 17
		rng ^= rng << 5
		want[i] = byte(rng)
	}
	if rep := writeThroughClient(t, client, 0, want, 0); rep.Error != 0 {
		t.Fatalf("WRITE reply error = %d, want 0", rep.Error)
	}
	if rep := flushThroughClient(t, client); rep.Error != 0 {
		t.Fatalf("FLUSH reply error = %d, want 0", rep.Error)
	}
	ctx := context.Background()
	if err := f.bs.DrainAllUploads(ctx); err != nil {
		t.Fatalf("DrainAllUploads: %v", err)
	}
	if got := len(integrationManifest(t, f.rt, f.exp).Blocks); got == 0 {
		t.Fatal("the manifest is empty after Flush and DrainAllUploads, so a punch has nothing to prune")
	}
	return client, done
}

// TestIntegration_TrimFreesAndReadsZero is TRIM over a real engine.Store: the
// punched range reads back as zeros and the manifest shrinks.
//
// The manifest is asserted rather than the extent map on purpose. The punch
// zero-overwrites the range through the local append log, so the local tier
// keeps reporting an interval over it, and DataExtents over-reports by
// documented contract (pkg/block/engine/dataextents.go:41, "may over-report data
// but must never under-report it"). Asserting that DataExtents stops reporting
// the punched range is unsatisfiable. The manifest is the observable: it is what
// the reclaim prunes.
func TestIntegration_TrimFreesAndReadsZero(t *testing.T) {
	f := newWriteVolumeFixture(t)
	client, _ := integrationWriteFixtureBytes(t, f, 1<<20)

	before := integrationManifest(t, f.rt, f.exp)

	// The export is exactly 1 MiB, so the punch covers the whole device and
	// contains the single chunk the fixture's write produced. An out-of-range
	// punch is refused (and has its own tests), so the range is the export's.
	if rep := trimThroughClient(t, client, 0, 1<<20, 0); rep.Error != 0 {
		t.Fatalf("TRIM reply error = %d, want 0", rep.Error)
	}

	after := integrationManifest(t, f.rt, f.exp)
	if len(after.Blocks) >= len(before.Blocks) {
		t.Fatalf("manifest has %d blocks after the punch, want fewer than the %d before it",
			len(after.Blocks), len(before.Blocks))
	}
	if got, was := manifestBytes(after), manifestBytes(before); got >= was {
		t.Fatalf("manifest references %d bytes after the punch, want fewer than the %d before it", got, was)
	}

	rep, got := readReplyAndPayload(t, client, &Request{Type: CmdRead, Cookie: 0x7a, Offset: 0, Length: 4096})
	if rep.Error != 0 {
		t.Fatalf("READ of the trimmed range returned error %d, want 0", rep.Error)
	}
	if !bytes.Equal(got, make([]byte, 4096)) {
		t.Fatal("READ of the trimmed range did not return zeros")
	}
}

// TestIntegration_WriteZeroesDoesNotMaterializeChunks is the assertion that
// keeps WRITE_ZEROES from becoming a zero-buffer WriteAt. On a
// content-addressed store that call MATERIALIZES a real zero chunk, so a
// filesystem's discard path would allocate storage instead of freeing it — the
// reference library's bug. The manifest must shrink, not grow.
func TestIntegration_WriteZeroesDoesNotMaterializeChunks(t *testing.T) {
	f := newWriteVolumeFixture(t)
	client, _ := integrationWriteFixtureBytes(t, f, 1<<20)

	before := integrationManifest(t, f.rt, f.exp)

	if rep := writeZeroesThroughClient(t, client, 0, 1<<20, 0); rep.Error != 0 {
		t.Fatalf("WRITE_ZEROES reply error = %d, want 0", rep.Error)
	}

	after := integrationManifest(t, f.rt, f.exp)
	if len(after.Blocks) >= len(before.Blocks) {
		t.Fatalf("manifest has %d blocks after WRITE_ZEROES, want fewer than the %d before it: a zero-buffer WriteAt would materialize a new chunk",
			len(after.Blocks), len(before.Blocks))
	}
	if got, was := manifestBytes(after), manifestBytes(before); got >= was {
		t.Fatalf("manifest references %d bytes after WRITE_ZEROES, want fewer than the %d before it", got, was)
	}

	rep, got := readReplyAndPayload(t, client, &Request{Type: CmdRead, Cookie: 0x7b, Offset: 0, Length: 4096})
	if rep.Error != 0 {
		t.Fatalf("READ of the zeroed range returned error %d, want 0", rep.Error)
	}
	if !bytes.Equal(got, make([]byte, 4096)) {
		t.Fatal("READ of the zeroed range did not return zeros")
	}
}
