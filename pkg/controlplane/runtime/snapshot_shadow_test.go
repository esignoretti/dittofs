package runtime

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// shadowFixture is a runtime whose share root is owned by the same synthetic
// identity the NBD handshake resolves exports as.
//
// The byteVerify fixture cannot be reused here: it creates its share root with a
// zero FileAttr, so the root carries a DACL granting OWNER@/SYSTEM@/
// ADMINISTRATORS@ to UID 0 and a 65534 export identity is denied execute on it.
// That is correct behaviour — the exporter is meant to be configured with an
// identity that owns the share — so the fixture sets the root's owner rather
// than weakening the permission check.
type shadowFixture struct {
	t         *testing.T
	rt        *Runtime
	meta      metadata.Store
	bs        *engine.Store
	shareName string
	identity  metadata.Identity
}

func newShadowFixture(t *testing.T, withRemote bool) *shadowFixture {
	t.Helper()
	ctx := context.Background()

	cp, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	t.Cleanup(func() { _ = cp.Close() })

	rt := New(cp)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: t.TempDir()})

	metaStore := badgertest.NewInMemory(t)
	const metaName = "shadow-meta"
	metaCfg := &models.MetadataStoreConfig{Name: metaName, Type: "badger"}
	if err := metaCfg.SetConfig(map[string]any{"path": t.TempDir()}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	metaID, err := cp.CreateMetadataStore(ctx, metaCfg)
	if err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if err := rt.RegisterMetadataStore(metaName, metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	// Two shapes, both reached in production:
	//   withRemote: an "fs" local CAS store wired to a remote, so hydration is
	//     possible and a snapshot export can serve real bytes.
	//   without:    a local-only store, where a snapshot export must be REFUSED
	//     because its journal starts empty and there is nothing to hydrate from.
	// A "memory" store reports a remote even when none was configured, so it
	// cannot express the local-only case.
	bsCfg := &models.BlockStoreConfig{Name: "shadow-local", Type: "fs"}
	if err := bsCfg.SetConfig(map[string]any{"path": t.TempDir()}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	localID, err := cp.CreateBlockStore(ctx, bsCfg)
	if err != nil {
		t.Fatalf("CreateBlockStore(local): %v", err)
	}
	// A share must reference a block store, so the local-only case points at the
	// local store itself rather than leaving the id empty.
	bsID := localID
	if withRemote {
		remoteCfg := &models.BlockStoreConfig{Name: "shadow-remote", Type: "memory"}
		bsID, err = cp.CreateBlockStore(ctx, remoteCfg)
		if err != nil {
			t.Fatalf("CreateBlockStore(remote): %v", err)
		}
	}

	uid, gid := uint32(65534), uint32(65534)
	const shareName = "/shadow"
	// A share with no block store id still needs the persisted row for
	// DisableShare and the snapshot paths to resolve it.
	if _, err := cp.CreateShare(ctx, &models.Share{
		Name: shareName, MetadataStoreID: metaID, BlockStoreID: bsID, Enabled: true,
	}); err != nil {
		t.Fatalf("cpstore CreateShare: %v", err)
	}
	if err := rt.AddShare(ctx, &ShareConfig{
		Name:              shareName,
		MetadataStore:     metaName,
		BlockStoreID:      bsID,
		Enabled:           true,
		DefaultPermission: string(models.PermissionReadWrite),
		// The load-bearing line: the root is owned by the identity the exporter
		// resolves as, so traversal is permitted without a root bypass.
		RootAttr: &metadata.FileAttr{UID: uid, GID: gid},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	t.Cleanup(func() { _ = rt.RemoveShare(shareName) })

	share, err := rt.sharesSvc.GetShare(shareName)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}

	return &shadowFixture{
		t:         t,
		rt:        rt,
		meta:      metaStore,
		bs:        share.BlockStore,
		shareName: shareName,
		identity:  metadata.Identity{UID: &uid, GID: &gid},
	}
}

// makeVolume creates <share>/volumes/<name> through the production create path.
func (f *shadowFixture) makeVolume(ctx context.Context, name string, size uint64) metadata.PayloadID {
	f.t.Helper()
	svc := f.rt.GetMetadataService()
	owner := &metadata.AuthContext{Context: ctx, Identity: &f.identity}
	root, err := svc.GetRootHandle(ctx, f.shareName)
	if err != nil {
		f.t.Fatalf("GetRootHandle: %v", err)
	}
	vols, _, err := svc.CreateDirectory(owner, root, "volumes", &metadata.FileAttr{Mode: 0o755})
	if err != nil {
		f.t.Fatalf("CreateDirectory volumes: %v", err)
	}
	volsHandle, err := metadata.EncodeFileHandle(vols)
	if err != nil {
		f.t.Fatalf("EncodeFileHandle volumes: %v", err)
	}
	created, _, err := svc.CreateFile(owner, volsHandle, name, &metadata.FileAttr{Mode: 0o644})
	if err != nil {
		f.t.Fatalf("CreateFile %q: %v", name, err)
	}
	createdHandle, err := metadata.EncodeFileHandle(created)
	if err != nil {
		f.t.Fatalf("EncodeFileHandle %q: %v", name, err)
	}
	// Creation zeroes the size (ApplyCreateDefaults), so the export size is set
	// explicitly afterwards — exactly as volume_test.go does.
	if _, err := svc.SetFileAttributes(owner, createdHandle, &metadata.SetAttrs{Size: &size}); err != nil {
		f.t.Fatalf("SetFileAttributes %q: %v", name, err)
	}
	return created.PayloadID
}

// write drives the real engine path AND drains it, so the manifest the snapshot
// captures is the one production would have written.
//
// DrainRollups is not optional: WriteToBlockStore only appends to the journal,
// and the FileChunk rows are written by the carve commit. Without the drain the
// bytes live in the append log and the manifest stays empty, so a snapshot taken
// then captures no rows and every shadow read is a hole — which looks exactly
// like a broken shadow and is not one.
func (f *shadowFixture) write(ctx context.Context, pid metadata.PayloadID, data []byte) {
	f.t.Helper()
	if err := common.WriteToBlockStore(ctx, f.bs, pid, data, 0); err != nil {
		f.t.Fatalf("WriteToBlockStore: %v", err)
	}
	if err := common.CommitBlockStore(ctx, f.bs, pid); err != nil {
		f.t.Fatalf("CommitBlockStore: %v", err)
	}
	if err := f.bs.DrainRollups(ctx); err != nil {
		f.t.Fatalf("DrainRollups: %v", err)
	}
}

func (f *shadowFixture) read(ctx context.Context, pid metadata.PayloadID, n int) []byte {
	f.t.Helper()
	res, err := common.ReadFromBlockStore(ctx, f.bs, pid, 0, uint32(n))
	if err != nil {
		f.t.Fatalf("ReadFromBlockStore: %v", err)
	}
	out := make([]byte, len(res.Data))
	copy(out, res.Data)
	res.Release()
	return out
}

// TestSnapshotShadow_ReadsTheSnapshotsBytes is the end-to-end proof that a
// snapshot export serves the snapshot, not production.
//
// The sequence is the one an operator actually runs: populate a volume, take a
// snapshot, keep writing, then boot the snapshot. The assertion that matters is
// the third read — the snapshot must return the bytes as of the snapshot while
// production returns the newer ones. A shadow that accidentally pointed at
// production's metadata store or its journal would pass every other check here
// (it would read *something*, and the size would match) and fail only that one.
func TestSnapshotShadow_ReadsTheSnapshotsBytes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fx := newShadowFixture(t, true)

	const volumeSize = 256 << 10
	pid := fx.makeVolume(ctx, "boot", volumeSize)
	original := distinctBytes(volumeSize, 0x51)
	fx.write(ctx, pid, original)
	if got := fx.read(ctx, pid, volumeSize); !bytes.Equal(got, original) {
		t.Fatalf("pre-snapshot volume mismatch: %s", firstDiff(original, got))
	}

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
	if werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}
	if snap.State != models.StateReady {
		t.Fatalf("snapshot state = %q, want ready", snap.State)
	}

	// Keep writing. This is the whole point: production must diverge from the
	// snapshot AFTER the snapshot was taken, or the test cannot tell them apart.
	mutated := distinctBytes(volumeSize, 0x77)
	fx.write(ctx, pid, mutated)
	if got := fx.read(ctx, pid, volumeSize); !bytes.Equal(got, mutated) {
		t.Fatalf("post-mutate production read mismatch: %s", firstDiff(mutated, got))
	}

	exp, err := fx.rt.OpenSnapshotAsExport(ctx, fx.shareName, snapID, "boot", &fx.identity)
	if err != nil {
		t.Fatalf("OpenSnapshotAsExport: %v", err)
	}
	t.Cleanup(func() { _ = exp.Close() })

	if len(exp.RootHandle) == 0 {
		t.Fatal("shadow export has no root handle")
	}
	if exp.PayloadID != pid {
		t.Fatalf("shadow payload ID = %q, want %q: the dump must preserve it verbatim", exp.PayloadID, pid)
	}
	if exp.Size != volumeSize {
		t.Fatalf("shadow size = %d, want %d", exp.Size, volumeSize)
	}

	// The read goes through the shadow's own block store. Its journal is empty, so
	// every range is a hole that hydrates from the snapshot's manifest — no
	// seeding step, by design.
	res, err := common.ReadFromBlockStore(ctx, exp.BlockStore(), exp.PayloadID, 0, volumeSize)
	if err != nil {
		t.Fatalf("read through shadow block store: %v", err)
	}
	got := make([]byte, len(res.Data))
	copy(got, res.Data)
	res.Release()

	// THE assertion: the snapshot's bytes, not production's.
	if !bytes.Equal(got, original) {
		t.Fatalf("snapshot export read the wrong bytes: %s\n"+
			"(if it matched the post-snapshot content, the shadow is reading production's journal or metadata)",
			firstDiff(original, got))
	}
	if bytes.Equal(got, mutated) {
		t.Fatal("snapshot export returned the POST-snapshot bytes: the shadow is not isolated from production")
	}

	// Production is untouched by the snapshot export having read.
	if cur := fx.read(ctx, pid, volumeSize); !bytes.Equal(cur, mutated) {
		t.Fatalf("production read changed after a snapshot export read it: %s", firstDiff(mutated, cur))
	}
}

// TestSnapshotShadow_RefusesWhileOpenAndReleasesOnClose asserts the two halves of
// the open/delete contract: while the export lives the snapshot cannot be
// deleted, and after Close it can.
func TestSnapshotShadow_RefusesWhileOpenAndReleasesOnClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fx := newShadowFixture(t, true)
	pid := fx.makeVolume(ctx, "boot", 64<<10)
	fx.write(ctx, pid, distinctBytes(64<<10, 0x22))

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}

	exp, err := fx.rt.OpenSnapshotAsExport(ctx, fx.shareName, snapID, "boot", &fx.identity)
	if err != nil {
		t.Fatalf("OpenSnapshotAsExport: %v", err)
	}

	if derr := fx.rt.DeleteSnapshot(ctx, fx.shareName, snapID); !errors.Is(derr, models.ErrSnapshotOpen) {
		t.Fatalf("DeleteSnapshot while exported = %v, want ErrSnapshotOpen", derr)
	}

	// Close twice: the open path's own teardown and an explicit caller Close can
	// both run, and a double Close must not release the registration twice.
	if err := exp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := exp.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n := fx.rt.SnapshotOpenHolders(fx.shareName, snapID); n != 0 {
		t.Fatalf("open holders after Close = %d, want 0", n)
	}

	if derr := fx.rt.DeleteSnapshot(ctx, fx.shareName, snapID); derr != nil {
		t.Fatalf("DeleteSnapshot after Close = %v, want nil", derr)
	}
}

// TestSnapshotShadow_RefusesUnusableSnapshots asserts the preconditions are
// checked before anything is allocated, and that a refusal leaves nothing
// registered — a refused open that still held the snapshot would make it
// permanently undeletable.
func TestSnapshotShadow_RefusesUnusableSnapshots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fx := newShadowFixture(t, true)

	creating, err := fx.rt.store.CreateSnapshot(ctx, &models.Snapshot{
		ShareName: fx.shareName, State: models.StateCreating, MetadataEngine: "badger",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot row: %v", err)
	}
	if _, err := fx.rt.OpenSnapshotAsExport(ctx, fx.shareName, creating, "boot", &fx.identity); err == nil {
		t.Fatal("opened a snapshot that is still being created")
	}
	if n := fx.rt.SnapshotOpenHolders(fx.shareName, creating); n != 0 {
		t.Fatalf("refused open left %d holders registered", n)
	}

	degraded, err := fx.rt.store.CreateSnapshot(ctx, &models.Snapshot{
		ShareName: fx.shareName, State: models.StateReady, MetadataEngine: "badger",
		Degraded: true, DegradedEntries: 7,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot row: %v", err)
	}
	if _, err := fx.rt.OpenSnapshotAsExport(ctx, fx.shareName, degraded, "boot", &fx.identity); err == nil {
		t.Fatal("opened a degraded snapshot: its manifest is short by 7 rows and a read touching one returns zeros")
	}
	if n := fx.rt.SnapshotOpenHolders(fx.shareName, degraded); n != 0 {
		t.Fatalf("refused open left %d holders registered", n)
	}
}

// TestSnapshotShadow_RefusesAShareWithNoRemote covers checkShareCanHydrate, the
// guard that refuses a share whose block store cannot supply bytes to a shadow
// journal.
//
// The guard cannot be reached through OpenSnapshotAsExport: every share must
// reference a block store, and both store types this release builds (fs, memory)
// wire a remote — a share configured with no remote is refused at AddShare
// ("failed to create remote store"). It is kept, and tested here directly,
// because the failure it prevents is silent: a shadow with no remote hydrates
// nothing and every read returns zeros with a nil error, i.e. a blank disk that
// looks like a healthy snapshot.
func TestSnapshotShadow_RefusesAShareWithNoRemote(t *testing.T) {
	// No block store at all.
	if err := checkShareCanHydrate("snap", "/share", &shares.Share{}); err == nil {
		t.Fatal("the guard accepted a share with no block store")
	}

	// A nil share (a lookup that returned nothing) must be refused too, not
	// dereferenced.
	if err := checkShareCanHydrate("snap", "/share", nil); err == nil {
		t.Fatal("the guard accepted a nil share")
	}

	// The reachable case: a share whose store DOES have a remote must pass, or
	// every snapshot export would be refused.
	fx := newShadowFixture(t, true)
	share, err := fx.rt.sharesSvc.GetShare(fx.shareName)
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if err := checkShareCanHydrate("snap", fx.shareName, share); err != nil {
		t.Fatalf("the guard refused a share with a remote: %v", err)
	}
}
