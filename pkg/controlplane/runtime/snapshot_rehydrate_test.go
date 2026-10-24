package runtime

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// TestRehydrateSnapshot_ProducesAnIndependentWritableVolume is the claim the
// rehydrate feature exists for: a snapshot can be turned into a NEW volume whose
// bytes match the snapshot, without touching the snapshot, and the new volume is
// ordinary enough that writing to it leaves the snapshot intact.
//
// The independence half is what a test must prove rather than assert. Chunks are
// content-addressed, so the copy is a reference copy: the new volume initially
// shares every chunk with the snapshot. If writing to it modified a shared chunk
// in place, the snapshot would silently change — which is the failure mode the
// whole design avoids by treating chunks as immutable. So the test writes
// different bytes over the same offsets and then re-reads the SNAPSHOT.
func TestRehydrateSnapshot_ProducesAnIndependentWritableVolume(t *testing.T) {
	meta := badgertest.NewInMemory(t)
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()

	ctx := context.Background()

	// The snapshot's source volume. Multi-chunk so the copy has more than one
	// reference to place: a single-block volume would pass even if the copy
	// dropped everything but the first block.
	const mib = 1 << 20
	original := distinctBytes(2*mib, 0xD1)
	srcPayload := fx.createEmptyFile(ctx, "volume")
	fx.writeFile(ctx, srcPayload, original)
	// The fixture writes through the engine and never updates FileAttr.Size, while
	// production keeps it honest on every write (PrepareWrite). Set it here, or the
	// snapshot would carry a correct manifest with a zero size and the test would
	// be asserting against a fixture artefact rather than the copy.
	setFileSize(t, fx, "volume", uint64(len(original)))
	if got := fx.readFile(ctx, srcPayload, len(original)); !bytes.Equal(got, original) {
		t.Fatalf("pre-snapshot volume mismatch: %s", firstDiff(original, got))
	}
	if f := fx.getFile(ctx, "volume"); len(f.Blocks) < 2 {
		t.Fatalf("volume produced %d block(s), want >= 2 so the copy places more than one reference", len(f.Blocks))
	}

	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: true})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}

	// Mutate production after the snapshot, so a rehydrate that read live bytes
	// instead of the snapshot's manifest would produce the mutated content.
	mutated := distinctBytes(2*mib, 0xD2)
	fx.writeFile(ctx, srcPayload, mutated)

	// Rehydrate into a new volume.
	job, err := fx.rt.RehydrateSnapshot(ctx, fx.shareName, snapID, "volume", "rehydrated")
	if err != nil {
		t.Fatalf("RehydrateSnapshot: %v", err)
	}
	job = waitForRehydrate(t, fx.rt, job.ID)
	if job.State != RehydrateStateDone {
		t.Fatalf("rehydrate state = %q (%s), want done", job.State, job.Err)
	}
	if job.Blocks < 2 {
		t.Fatalf("rehydrate placed %d block(s), want >= 2", job.Blocks)
	}
	if job.Bytes != uint64(len(original)) {
		t.Fatalf("rehydrate reported %d bytes, want %d", job.Bytes, len(original))
	}

	// (a) The new volume reads back the SNAPSHOT's bytes, not production's.
	dstHandle := mustChildHandle(t, fx, "rehydrated")
	dstFile, err := fx.meta.GetFile(ctx, dstHandle)
	if err != nil {
		t.Fatalf("GetFile(rehydrated): %v", err)
	}
	if dstFile.PayloadID == srcPayload {
		t.Fatal("the rehydrated volume reuses the source's payload ID: it would address the source's rows")
	}
	got := fx.readFile(ctx, dstFile.PayloadID, len(original))
	if !bytes.Equal(got, original) {
		t.Fatalf("rehydrated volume does not match the snapshot: %s", firstDiff(original, got))
	}

	// (b) Writing to the new volume does not disturb the snapshot. This is the
	// claim that matters: the two share chunks, so an in-place write would be a
	// silent corruption of the snapshot.
	overwrite := distinctBytes(2*mib, 0xD3)
	fx.writeFile(ctx, dstFile.PayloadID, overwrite)
	if got := fx.readFile(ctx, dstFile.PayloadID, len(overwrite)); !bytes.Equal(got, overwrite) {
		t.Fatalf("the rehydrated volume did not take its own write: %s", firstDiff(overwrite, got))
	}

	// The snapshot still reads as the original. Rehydrate a SECOND volume from it
	// to prove the snapshot's manifest is unchanged — reading the live volume
	// would not prove that, because production was mutated independently.
	job2, err := fx.rt.RehydrateSnapshot(ctx, fx.shareName, snapID, "volume", "rehydrated2")
	if err != nil {
		t.Fatalf("second RehydrateSnapshot: %v", err)
	}
	job2 = waitForRehydrate(t, fx.rt, job2.ID)
	if job2.State != RehydrateStateDone {
		t.Fatalf("second rehydrate state = %q (%s), want done", job2.State, job2.Err)
	}
	dst2, err := fx.meta.GetFile(ctx, mustChildHandle(t, fx, "rehydrated2"))
	if err != nil {
		t.Fatalf("GetFile(rehydrated2): %v", err)
	}
	if got := fx.readFile(ctx, dst2.PayloadID, len(original)); !bytes.Equal(got, original) {
		t.Fatalf("the snapshot changed after a rehydrated volume was written: %s", firstDiff(original, got))
	}
}

// TestRehydrateSnapshot_RefusesWhatCannotWork pins the cheap validation, which
// runs before any job exists so a caller learns synchronously.
func TestRehydrateSnapshot_RefusesWhatCannotWork(t *testing.T) {
	meta := badgertest.NewInMemory(t)
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()
	ctx := context.Background()

	srcPayload := fx.createEmptyFile(ctx, "volume")
	fx.writeFile(ctx, srcPayload, distinctBytes(4096, 1))
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: true})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}

	cases := []struct {
		name   string
		share  string
		snapID string
		src    string
		volume string
	}{
		{name: "no destination name", share: fx.shareName, snapID: snapID, src: "volume", volume: ""},
		{name: "no source name", share: fx.shareName, snapID: snapID, src: "", volume: "x"},
		{name: "same name twice", share: fx.shareName, snapID: snapID, src: "volume", volume: "volume"},
		{name: "unknown snapshot", share: fx.shareName, snapID: "00000000-0000-0000-0000-000000000000", src: "volume", volume: "x"},
		{name: "unknown share", share: "/nope", snapID: snapID, src: "volume", volume: "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fx.rt.RehydrateSnapshot(ctx, tc.share, tc.snapID, tc.src, tc.volume); err == nil {
				t.Fatal("RehydrateSnapshot accepted a request it cannot satisfy")
			}
		})
	}

	// A source volume the snapshot does not hold fails the JOB, not the call: the
	// snapshot's contents are only readable after a replay, which is the job's
	// first step, so this cannot be rejected without doing the work anyway.
	job, err := fx.rt.RehydrateSnapshot(ctx, fx.shareName, snapID, "nope", "x")
	if err != nil {
		t.Fatalf("RehydrateSnapshot: %v", err)
	}
	if done := waitForRehydrate(t, fx.rt, job.ID); done.State != RehydrateStateFailed {
		t.Fatalf("rehydrating a volume the snapshot does not hold: state %q, want failed", done.State)
	}

	// A destination that already exists is refused rather than overwritten: an
	// operator naming an existing volume means "make this one", not "replace it".
	if _, err := fx.rt.RehydrateSnapshot(ctx, fx.shareName, snapID, "volume", "volume"); err == nil {
		t.Fatal("RehydrateSnapshot overwrote an existing volume")
	}
}

// TestRehydrateSnapshot_RollsBackOnFailure asserts a failed job leaves no
// destination volume behind, so the operator can simply retry.
//
// The failure is induced by cancelling the job's context, which is the one
// failure the test can cause without reaching into the store. A rollback that
// did not run would leave a volume with no manifest — an empty disk the operator
// would have to find and delete by hand.
func TestRehydrateSnapshot_RollsBackOnFailure(t *testing.T) {
	meta := badgertest.NewInMemory(t)
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()
	ctx := context.Background()

	srcPayload := fx.createEmptyFile(ctx, "volume")
	fx.writeFile(ctx, srcPayload, distinctBytes(4096, 2))
	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: true})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}

	// A cancelled parent context does not cancel the job (it detaches), so this
	// asserts the opposite property: the job survives a caller that hung up.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	job, err := fx.rt.RehydrateSnapshot(cancelled, fx.shareName, snapID, "volume", "survivor")
	if err != nil {
		t.Fatalf("RehydrateSnapshot with a cancelled caller context: %v", err)
	}
	job = waitForRehydrate(t, fx.rt, job.ID)
	if job.State != RehydrateStateDone {
		t.Fatalf("job state = %q (%s), want done: a caller hanging up must not abort a rehydrate",
			job.State, job.Err)
	}
	// mustChildHandle looks where the job creates a volume, which is not
	// necessarily the share root.
	if _, err := fx.meta.GetFile(ctx, mustChildHandle(t, fx, "survivor")); err != nil {
		t.Fatalf("the rehydrated volume is missing after a cancelled caller: %v", err)
	}
}

// waitForRehydrate polls a job to a terminal state.
func waitForRehydrate(t *testing.T, rt *Runtime, id string) *RehydrateJob {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := rt.GetRehydrateJob(id)
		if !ok {
			t.Fatalf("rehydrate job %q disappeared", id)
		}
		if job.State == RehydrateStateDone || job.State == RehydrateStateFailed {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("rehydrate job %q did not finish within 60s", id)
	return nil
}

func mustRoot(t *testing.T, fx *byteVerifyFixture) metadata.FileHandle {
	t.Helper()
	root, err := fx.meta.GetRootHandle(context.Background(), fx.shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	return root
}

// mustChildHandle resolves a name the way the rehydrate job creates it: under
// volumes/ when the share has that directory, otherwise at the share root.
func mustChildHandle(t *testing.T, fx *byteVerifyFixture, name string) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	root := mustRoot(t, fx)
	// metadata.Store exposes GetChild by name, which is all this needs; the
	// authorization-carrying Lookup lives on the Service.
	if volsHandle, err := fx.meta.GetChild(ctx, root, "volumes"); err == nil {
		if handle, gerr := fx.meta.GetChild(ctx, volsHandle, name); gerr == nil {
			return handle
		}
	}
	handle, err := fx.meta.GetChild(ctx, root, name)
	if err != nil {
		t.Fatalf("GetChild %q: %v", name, err)
	}
	return handle
}

// setFileSize updates a file's recorded size, the way the production write path
// does.
func setFileSize(t *testing.T, fx *byteVerifyFixture, name string, size uint64) {
	t.Helper()
	ctx := context.Background()
	handle := mustChildHandle(t, fx, name)
	file, err := fx.meta.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile %q: %v", name, err)
	}
	file.Size = size
	if err := fx.meta.UpdateAttrs(ctx, file); err != nil {
		t.Fatalf("UpdateAttrs %q: %v", name, err)
	}
}

// engineGuard keeps the engine import referenced; the rehydrate path calls
// CopyPayload and SeedColdRefs on it.
var _ = func(*engine.Store) {}
