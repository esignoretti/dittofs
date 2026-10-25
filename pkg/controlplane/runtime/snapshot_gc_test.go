package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	badgertest "github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// TestRehydratedVolumeSurvivesTheSnapshotBeingDeleted is the test that proves the
// central claim of Part 2: a rehydrated volume's chunks are referenced by its own
// metadata, not borrowed from the snapshot's GC hold.
//
// Why this is not the same as "the volume reads the snapshot's bytes": that test
// passes even if the volume were reading through a hold, because the snapshot
// still exists while it runs. The claim only breaks when the hold is gone, so the
// sequence has to be rehydrate → delete the snapshot → run GC → read. If the new
// volume's rows are not real references, GC sweeps the chunks and the read returns
// zeros or an error, which is exactly the failure a customer would hit a week
// later.
//
// Grace period zero is passed deliberately: the sweep otherwise skips chunks
// younger than the configured grace (1h by default), and a test that waited an
// hour would be a test nobody runs.
func TestRehydratedVolumeSurvivesTheSnapshotBeingDeleted(t *testing.T) {
	meta := badgertest.NewInMemory(t)
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()
	ctx := context.Background()

	// A distinct pattern per 4 KiB so a wrongly-swept chunk reads as zeros in a
	// range that is easy to identify, and multi-chunk so more than one hash is at
	// stake.
	const size = 2 << 20
	pattern := distinctBytes(size, 0x7a)
	pid := fx.createEmptyFile(ctx, "boot")
	fx.writeFile(ctx, pid, pattern)
	setFileSize(t, fx, "boot", size)

	snapID := takeSnapshot(t, fx)

	job, err := fx.rt.RehydrateSnapshot(ctx, fx.shareName, snapID, "boot", "boot-2")
	if err != nil {
		t.Fatalf("RehydrateSnapshot: %v", err)
	}
	done := waitForRehydrate(t, fx.rt, job.ID)
	if done.State != RehydrateStateDone {
		t.Fatalf("rehydrate finished in state %q: %s", done.State, done.Err)
	}

	// Read the new volume before the delete, so a later failure is attributable to
	// the delete rather than to the copy never having worked.
	if got := readVolume(t, fx, "boot-2", size); !equalBytes(got, pattern) {
		t.Fatalf("the rehydrated volume does not read the snapshot's bytes before the delete")
	}

	// The snapshot must not be held open by the finished job, or this delete would
	// be refused with ErrSnapshotOpen — a real bug (a job that forgot to release)
	// that would look like "you cannot delete anything you have rehydrated".
	if n := fx.rt.SnapshotOpenHolders(fx.shareName, snapID); n != 0 {
		t.Fatalf("the finished job still holds the snapshot open (%d holder(s))", n)
	}
	if err := fx.rt.DeleteSnapshot(ctx, fx.shareName, snapID); err != nil {
		t.Fatalf("DeleteSnapshot after a rehydrate: %v", err)
	}

	// Sweep with no grace, so the decision GC makes here is the one it would make
	// an hour from now rather than "too young to consider".
	zero := time.Duration(0)
	stats, err := fx.rt.runBlockGCForShare(ctx, fx.shareName, false, nil, &zero)
	if err != nil {
		t.Fatalf("runBlockGCForShare: %v", err)
	}
	t.Logf("GC after delete: swept=%d freed=%d bytes", stats.ObjectsSwept, stats.BytesFreed)

	// The assertion this whole test exists for.
	if got := readVolume(t, fx, "boot-2", size); !equalBytes(got, pattern) {
		t.Fatalf("the rehydrated volume lost its bytes after the snapshot was deleted and GC ran: "+
			"its chunks were reclaimed, so CopyPayload's rows are not a real GC reference "+
			"(GC swept %d object(s))", stats.ObjectsSwept)
	}

	// And the new volume is still a normal writable volume, not a snapshot view.
	newPattern := distinctBytes(size, 0x7b)
	writeVolume(t, fx, "boot-2", newPattern)
	if got := readVolume(t, fx, "boot-2", size); !equalBytes(got, newPattern) {
		t.Fatal("the rehydrated volume did not accept a write after the snapshot was deleted")
	}

	// NEGATIVE CONTROL, and the reason the assertions above mean anything.
	//
	// "GC swept nothing" is also what a broken GC reports, so the read above
	// passes for two different reasons and proves neither. Removing the only
	// remaining reference — the rehydrated volume itself — must make the same
	// chunks sweepable. If it does not, this GC run was never looking at these
	// hashes and the main assertion is vacuous.
	//
	// The volume is removed the way a client removes one — unlink the inode and
	// drive engine.Delete with its persisted Blocks — rather than with the
	// fixture's deleteFile helper, which resolves at the share root and would not
	// find a volume living under volumes/.
	deleteVolume(t, fx, "boot-2")
	control, err := fx.rt.runBlockGCForShare(ctx, fx.shareName, false, nil, &zero)
	if err != nil {
		t.Fatalf("control GC: %v", err)
	}
	if control.ObjectsSwept == 0 {
		t.Fatalf("control: deleting the rehydrated volume swept nothing, so this GC run does not "+
			"see the volume's chunks at all — the survival assertion above is vacuous "+
			"(hashes marked %d)", control.HashesMarked)
	}
	t.Logf("control GC after deleting the volume too: swept=%d freed=%d", control.ObjectsSwept, control.BytesFreed)
}

// deleteVolume removes a volume from wherever the share keeps it, the way the
// NFS and SMB remove handlers do: unlink the inode, then drive engine.Delete with
// the file's persisted Blocks so the coordinator decrements refcounts and the
// file_blocks rows are reclaimed.
func deleteVolume(t *testing.T, fx *byteVerifyFixture, name string) {
	t.Helper()
	ctx := context.Background()
	file := fileAt(t, fx, name)
	handle := mustChildHandle(t, fx, name)
	if err := fx.meta.DeleteFile(ctx, handle); err != nil {
		t.Fatalf("DeleteFile %q: %v", name, err)
	}
	if err := fx.bs.Delete(ctx, string(file.PayloadID), file.Blocks); err != nil {
		t.Fatalf("engine.Delete %q: %v", name, err)
	}
}

// fileAt resolves a volume by name, descending into volumes/ when the file is
// there — the layout a rehydrate creates.
func fileAt(t *testing.T, fx *byteVerifyFixture, name string) *metadata.File {
	t.Helper()
	ctx := context.Background()
	handle := mustChildHandle(t, fx, name)
	file, err := fx.meta.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile %q: %v", name, err)
	}
	return file
}

// readVolume reads the whole named volume through the production metadata service.
func readVolume(t *testing.T, fx *byteVerifyFixture, name string, size int) []byte {
	t.Helper()
	ctx := context.Background()
	// mustChildHandle, not getFile: a rehydrated volume lives under volumes/,
	// which getFile does not descend into.
	file := fileAt(t, fx, name)
	out := make([]byte, size)
	n, err := fx.bs.ReadAt(ctx, string(file.PayloadID), out, 0)
	if err != nil {
		t.Fatalf("read %q: %v", name, err)
	}
	if n != size {
		t.Fatalf("read %q returned %d bytes, want %d", name, n, size)
	}
	return out
}

// writeVolume writes the whole named volume through the production store.
func writeVolume(t *testing.T, fx *byteVerifyFixture, name string, data []byte) {
	t.Helper()
	fx.writeFile(context.Background(), fileAt(t, fx, name).PayloadID, data)
}

// equalBytes reports whether two byte slices are identical.
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
