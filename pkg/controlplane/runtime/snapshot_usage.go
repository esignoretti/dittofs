package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// SnapshotUsage is one snapshot's share of what the share's snapshots hold.
//
// Counts, not bytes, and that is a deliberate limit rather than an unfinished
// job. A manifest line is a content hash and nothing else, so a manifest walk
// knows exactly how many chunks exist and nothing about their size. Byte sizes
// live in the remote objects; converting would mean one remote HEAD per distinct
// hash, which turns a local file read into thousands of network round trips. The
// operator who wants bytes reads bytes_freed from the gc-status after a real
// delete, which the reclaimer measures for free.
type SnapshotUsage struct {
	// ID and CreatedAt identify the snapshot.
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	// Chunks is how many distinct hashes this snapshot's manifest references.
	Chunks int `json:"chunks"`
	// Unique is how many of them no OTHER snapshot references. This is the
	// number that answers "what would deleting this one free": chunks are shared
	// between snapshots of a slowly-changing volume, so Unique is usually far
	// smaller than Chunks, and that gap is the whole reason retention is cheap.
	Unique int `json:"unique"`
	// Shared is Chunks - Unique: the part deleting this snapshot would NOT free,
	// because something else still holds it.
	Shared int `json:"shared"`
	// Open reports whether a holder currently has this snapshot registered (an
	// NBD export serving a boot, or a running rehydrate). A delete would be
	// refused with 409 while this is true, so the report says so rather than
	// letting the operator discover it by trying.
	Open bool `json:"open"`
	// Holders is the current registration count, for explaining a refusal.
	Holders int `json:"holders"`
}

// SnapshotUsageReport is the whole picture for one share.
type SnapshotUsageReport struct {
	Share string `json:"share"`
	// Snapshots is one entry per snapshot with a readable manifest, ordered
	// newest first.
	Snapshots []SnapshotUsage `json:"snapshots"`
	// TotalChunks is the union: how many distinct chunks the share's snapshots
	// hold between them. Not the sum of the per-snapshot counts — that sum is
	// the point of the sharing and would overstate the footprint by however much
	// the snapshots overlap.
	TotalChunks int `json:"total_chunks"`
	// SumOfSnapshotChunks is the sum of every snapshot's Chunks. The gap between
	// it and TotalChunks is the sharing, stated as one number.
	SumOfSnapshotChunks int `json:"sum_of_snapshot_chunks"`
	// UniqueToSomeSnapshot is how many chunks exactly one snapshot references.
	// Deleting that snapshot frees them; deleting anything else does not.
	UniqueToSomeSnapshot int `json:"unique_to_some_snapshot"`
	// Skipped names snapshots whose manifest could not be read, so a report is
	// never silently smaller than the snapshot list.
	Skipped []string `json:"skipped,omitempty"`
}

// SnapshotUsage computes what each of a share's snapshots holds and what
// deleting one would free.
//
// One pass over the manifests, counting per-snapshot membership instead of
// unioning: SnapshotHoldProvider.HeldHashes already walks every manifest and
// deduplicates into a union for the GC mark phase, and the same walk with a
// reference count per hash answers this. Nothing is read twice, nothing is
// fetched from the remote, and no storage is added.
//
// A snapshot with no readable manifest is skipped and named in the report rather
// than counted as empty. The two are very different: an empty manifest holds
// nothing, a missing one means the report does not know, and reporting the second
// as the first would tell an operator a delete is free when it is not.
//
// When snapID is non-empty the report covers just that snapshot — its Unique and
// Shared are still computed against ALL of the share's snapshots, because
// "unique" is meaningless otherwise.
func (r *Runtime) SnapshotUsage(ctx context.Context, share, snapID string) (*SnapshotUsageReport, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("snapshot usage: runtime has no store")
	}
	localStoreDir, err := r.sharesSvc.LocalStoreDir(share)
	if err != nil {
		return nil, fmt.Errorf("snapshot usage: resolve local store dir for share %q: %w", share, err)
	}
	if localStoreDir == "" {
		return nil, fmt.Errorf("snapshot usage: share %q has no on-disk data directory, so it holds no snapshots", share)
	}

	// The same fence the GC mark phase uses. A delete in flight would remove a
	// manifest mid-walk, and reporting a half-read set of snapshots is worse than
	// waiting for the delete to finish.
	lock := r.snapshotDeleteLock(share)
	lock.RLock()
	defer lock.RUnlock()

	snaps, err := r.store.ListSnapshots(ctx, share)
	if err != nil {
		return nil, fmt.Errorf("snapshot usage: list snapshots for share %q: %w", share, err)
	}

	// Per-snapshot manifests, kept so membership can be counted once the whole
	// set is known. Only the hashes are retained, and only for the share being
	// reported on.
	type loaded struct {
		snap *models.Snapshot
		set  *block.HashSet
	}
	var (
		all     []loaded
		skipped []string
	)

	// EVERY snapshot's manifest is loaded, even when the report covers one id.
	// "Unique" means "no OTHER snapshot references it", so a filtered walk would
	// mark every chunk of the reported snapshot unique and turn the report into a
	// lie about how much a delete frees.
	for _, snap := range snaps {
		set, lerr := readManifestSet(snap.ManifestPath(localStoreDir))
		if lerr != nil {
			if errors.Is(lerr, fs.ErrNotExist) {
				// No manifest = no hold, the same rule the GC uses. Named rather
				// than counted as zero.
				skipped = append(skipped, snap.ID)
				continue
			}
			return nil, fmt.Errorf("snapshot usage: read manifest for snapshot %q: %w", snap.ID, lerr)
		}
		all = append(all, loaded{snap: snap, set: set})
	}

	// membership[h] = how many of the loaded snapshots reference h.
	membership := make(map[block.ContentHash]int)
	for _, l := range all {
		_ = l.set.ForEach(func(h block.ContentHash) error {
			membership[h]++
			return nil
		})
	}

	report := &SnapshotUsageReport{Share: share, Skipped: skipped}
	report.TotalChunks = len(membership)
	for _, l := range all {
		report.SumOfSnapshotChunks += l.set.Len()
	}
	for _, n := range membership {
		if n == 1 {
			report.UniqueToSomeSnapshot++
		}
	}

	report.Snapshots = make([]SnapshotUsage, 0, len(all))
	for _, l := range all {
		// The filter applies only to what is REPORTED, never to what was measured.
		if snapID != "" && l.snap.ID != snapID {
			continue
		}
		u := SnapshotUsage{
			ID:        l.snap.ID,
			CreatedAt: l.snap.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			Chunks:    l.set.Len(),
		}
		_ = l.set.ForEach(func(h block.ContentHash) error {
			if membership[h] == 1 {
				u.Unique++
			}
			return nil
		})
		u.Shared = u.Chunks - u.Unique
		u.Holders = r.SnapshotOpenHolders(share, l.snap.ID)
		u.Open = u.Holders > 0
		report.Snapshots = append(report.Snapshots, u)
	}

	// Newest first, so the report reads like the retention policy it informs.
	// Sorted in place rather than by sorting the snapshots before loading, because
	// the load order is the store's and a caller must not depend on it.
	sortUsageNewestFirst(report.Snapshots)

	return report, nil
}

// readManifestSet parses one manifest into a set of hashes. fs.ErrNotExist
// propagates unchanged so the caller can distinguish "holds nothing" from "not
// there".
func readManifestSet(path string) (*block.HashSet, error) {
	f, err := openManifestWithRetry(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readManifestFrom(f)
}

// readManifestFrom is the parse step, separated so it can be exercised without a
// file.
func readManifestFrom(f *os.File) (*block.HashSet, error) {
	return snapshot.ReadManifest(f)
}

// sortUsageNewestFirst orders by CreatedAt descending, keeping the input order
// for equal timestamps so the result is deterministic.
func sortUsageNewestFirst(u []SnapshotUsage) {
	// Insertion sort: a share has tens of snapshots, and this avoids importing
	// sort for a comparator that would need a stable variant anyway.
	for i := 1; i < len(u); i++ {
		for j := i; j > 0 && u[j].CreatedAt > u[j-1].CreatedAt; j-- {
			u[j], u[j-1] = u[j-1], u[j]
		}
	}
}
