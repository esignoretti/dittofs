package runtime

import (
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// snapshotKey identifies one snapshot within one share. Snapshots are
// share-scoped (models.Snapshot carries ShareName), so a bare ID is not unique
// across shares and a registry keyed on it would let an open in share A refuse
// a delete in share B.
type snapshotKey struct {
	share  string
	snapID string
}

// openSnapshotRegistry tracks snapshots that are currently held open by an
// export or a rehydrate job.
//
// It exists to turn a race into a refusal. DeleteSnapshot wipes the snapshot's
// directory, and an NBD snapshot export reads that directory's manifest for the
// whole duration of a boot; without a fence the reader fails partway through
// with a missing manifest, which an operator sees as a corrupt snapshot rather
// than as a delete that raced it.
//
// The obvious alternative — have the open export hold the shared side of the
// per-share snapshotDeleteLock — is worse. DeleteSnapshot would then block for
// the length of a boot instead of refusing, with no context and no timeout, and
// because Go's RWMutex blocks new readers once a writer is waiting, a second
// snapshot open on the same share would hang behind it too. Refusal is scoped
// to the one snapshot, which is also what retention pruning needs: pruning must
// be able to delete snapshot B while snapshot A is open.
//
// Process-local and deliberately not persisted. A crash clears every
// registration, which is correct: nothing is reading after a crash, so nothing
// should be refused.
type openSnapshotRegistry struct {
	// refsMu guards refs. A plain Mutex rather than an RWMutex because every
	// operation here is a short read-modify-write on a map and none of them is
	// a hot path.
	refsMu sync.Mutex
	// refs counts holders per snapshot. Nil until first use.
	refs map[snapshotKey]int
}

// openSnapshot registers a holder of {share, snapID} and returns the function
// that releases it.
//
// The returned release is idempotent: a caller that both defers it and calls it
// on an error path must not drive the count negative, because a negative count
// would make a later delete succeed while a real holder is still reading. It is
// also the caller's only obligation — the registry does not know what a holder
// is doing and must not try to stop it.
//
// refs is nil until first use, so the zero value is ready. Callers must not
// copy a registry; Runtime holds the only instance.
func (r *openSnapshotRegistry) open(share, snapID string) (release func()) {
	key := snapshotKey{share: share, snapID: snapID}
	r.refsMu.Lock()
	if r.refs == nil {
		r.refs = make(map[snapshotKey]int)
	}
	r.refs[key]++
	r.refsMu.Unlock()

	var released bool
	return func() {
		r.refsMu.Lock()
		defer r.refsMu.Unlock()
		if released {
			return
		}
		released = true
		switch n := r.refs[key]; {
		case n > 1:
			r.refs[key] = n - 1
		default:
			// Deleting rather than storing 0 keeps the map from growing one
			// entry per snapshot ever opened, which on a share taking an hourly
			// snapshot would otherwise be unbounded for the process lifetime.
			delete(r.refs, key)
		}
	}
}

// holders reports how many holders currently have {share, snapID} open.
func (r *openSnapshotRegistry) holders(share, snapID string) int {
	r.refsMu.Lock()
	defer r.refsMu.Unlock()
	return r.refs[snapshotKey{share: share, snapID: snapID}]
}

// OpenSnapshot marks a snapshot as in use and returns the function that
// releases it. While any holder is registered, DeleteSnapshot refuses that
// snapshot with models.ErrSnapshotOpen.
//
// The NBD snapshot export calls this before it reads anything from the
// snapshot's directory, and releases it only after the export is torn down, so
// the manifest cannot disappear mid-boot. The rehydrate job holds it for the
// same reason: a rehydrate reads the whole manifest.
//
// It does not validate that the snapshot exists or is ready. That check belongs
// to whoever is about to read it, and doing it here would make the registry
// depend on the store for a decision that is already being made one frame up.
func (r *Runtime) OpenSnapshot(share, snapID string) (release func()) {
	return r.openSnapshots.open(share, snapID)
}

// SnapshotOpenHolders reports how many holders currently have the snapshot
// open. Exposed so a caller can explain a refusal to an operator (the REST
// layer puts the count in its 409) instead of only reporting that one exists.
func (r *Runtime) SnapshotOpenHolders(share, snapID string) int {
	return r.openSnapshots.holders(share, snapID)
}

// refuseIfSnapshotOpen is the delete-path fence. It is called under the
// already-held per-share snapshotDeleteLock, so the answer it reads cannot
// change between the check and the wipe: openSnapshot's only caller registers
// BEFORE it reads anything, and a registration that arrives after this check
// sees a directory that is already being removed.
func (r *Runtime) refuseIfSnapshotOpen(share, snapID string) error {
	if n := r.openSnapshots.holders(share, snapID); n > 0 {
		return fmt.Errorf("delete snapshot %q on share %q: %w (held open by %d)",
			snapID, share, models.ErrSnapshotOpen, n)
	}
	return nil
}
