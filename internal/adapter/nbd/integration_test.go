package nbd

import (
	"bytes"
	"context"
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

// newIntegrationFixture builds a runtime with one enabled share holding one
// volume file, and returns the runtime, the export name, and the share's real
// block store.
//
// The journal root comes from testruntime.New, so the local tier is an fs
// store — which is what makes LocalDurable() true and the eviction assertions
// below meaningful.
func newIntegrationFixture(t *testing.T) (*runtime.Runtime, string, *engine.Store) {
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
		RootAttr:          &metadata.FileAttr{UID: 1000, GID: 1000},
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
		Identity: &metadata.Identity{UID: ptr(uint32(1000)), GID: ptr(uint32(1000))},
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
	if _, err := metaSvc.SetFileAttributes(authCtx, createdHandle, &metadata.SetAttrs{Size: ptr(uint64(1 << 20))}); err != nil {
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
