package journal

import (
	"context"
	"math"
	"testing"
)

// pinnedSurvivors reports the segments still holding an unsynced record after a
// pass, and their on-disk bytes. Empty replacement actives are excluded: a
// force-seal leaves one behind and it pins nothing.
func pinnedSurvivors(s *Store) (int, int64) {
	var n int
	var bytes int64
	for _, sh := range s.shards {
		sh.mu.Lock()
		if a := sh.active; a != nil && a.records.Load() > 0 && a.syncedRecords.Load() != a.records.Load() {
			n++
			bytes += a.tail.Load()
		}
		for _, seg := range sh.sealed {
			if seg.syncedRecords.Load() != seg.records.Load() {
				n++
				bytes += seg.tail.Load()
			}
		}
		sh.mu.Unlock()
	}
	return n, bytes
}

// TestStatsPinnedSegmentsMatchesWhatEvictRefuses ties Stats.PinnedSegments to
// eviction's own behaviour rather than to a second copy of the synced-gate
// predicate. The counter exists to answer "how much local disk does an unsynced
// residue actually hold down", so what it must agree with is the set of
// segments an exhaustive evict pass leaves behind.
func TestStatsPinnedSegmentsMatchesWhatEvictRefuses(t *testing.T) {
	s, _ := evictStore(t, Config{})
	ctx := context.Background()

	// Two sealed segments holding unsynced records, then more holding only
	// synced ones. Distinct FileIDs keep the offsets disjoint so no record
	// supersedes another and every segment stays live.
	fillUntilSealed(t, s, "dirty", false, 2)
	fillUntilSealed(t, s, "clean", true, 4)

	before := s.Stats()
	if before.PinnedSegments == 0 {
		t.Fatalf("no segment reported as pinned, so the assertion below would pass vacuously")
	}
	if before.PinnedBytes <= 0 {
		t.Fatalf("PinnedSegments=%d but PinnedBytes=%d", before.PinnedSegments, before.PinnedBytes)
	}

	// Drain everything eviction is willing to drop.
	if _, err := s.Evict(ctx, math.MaxInt64); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	survived, survivedBytes := pinnedSurvivors(s)
	if survived != before.PinnedSegments {
		t.Errorf("PinnedSegments = %d, but eviction left %d segments holding unsynced records",
			before.PinnedSegments, survived)
	}
	if survivedBytes != before.PinnedBytes {
		t.Errorf("PinnedBytes = %d, but the segments eviction left behind hold %d bytes",
			before.PinnedBytes, survivedBytes)
	}
}

// TestStatsPinnedSegmentsCountsUnsealableActive covers the case the sealed-set
// walk alone reports as zero: a working set below the rotation threshold sits
// entirely in active segments, and one unsynced record there is refused by
// sealableActive, so the force-seal fall-through never moves it where eviction
// looks. That is the shape behind "9 uncarved bytes left a 68 MB journal
// untouched at segments=0 freed=0" — nothing sealed, nothing evictable, and a
// counter that only walked sh.sealed would have called it unpinned.
func TestStatsPinnedSegmentsCountsUnsealableActive(t *testing.T) {
	s, _ := evictStore(t, Config{})
	ctx := context.Background()

	// One unsynced write, far below the segment size: no seal, no rotation.
	if err := s.WriteAt(ctx, "tail", 0, make([]byte, 4096)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	st := s.Stats()
	if st.PinnedSegments != 1 {
		t.Fatalf("PinnedSegments = %d, want 1: the active segment holds an unsynced record", st.PinnedSegments)
	}
	if st.PinnedBytes <= 0 {
		t.Fatalf("PinnedBytes = %d, want the active segment's on-disk size", st.PinnedBytes)
	}

	// And eviction must in fact not reclaim it, which is what makes it pinned.
	res, err := s.Evict(ctx, math.MaxInt64)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted != 0 || res.BytesFreed != 0 {
		t.Fatalf("Evict reclaimed %d segments / %d bytes from a store whose only record is unsynced",
			res.SegmentsEvicted, res.BytesFreed)
	}
	survived, survivedBytes := pinnedSurvivors(s)
	if survived != st.PinnedSegments || survivedBytes != st.PinnedBytes {
		t.Errorf("reported %d segments / %d bytes pinned, eviction left %d / %d",
			st.PinnedSegments, st.PinnedBytes, survived, survivedBytes)
	}
}
