package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	badgertest "github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// TestSnapshotUsage_CountsSharingNotSize is the report's whole reason to exist.
//
// Snapshots of a slowly-changing volume re-reference the same chunks, so the sum
// of the per-snapshot counts overstates the footprint by however much they
// overlap. The report must show both numbers and the gap between them, because
// that gap is the answer to "why do 40 snapshots not cost 40 volumes".
//
// The fixture is built so the sharing is exact and checkable by hand: three
// snapshots where the second shares almost everything with the first, and the
// third adds one new file. Every expected number below is derived from that, not
// from running the code.
func TestSnapshotUsage_CountsSharingNotSize(t *testing.T) {
	meta := badgertest.NewInMemory(t)
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()

	ctx := context.Background()

	// Snapshot 1: one file, multi-chunk so there is more than one hash to share.
	// 2 MiB splits into several chunks at this chunk size; 1 MiB does not, and a
	// single-chunk file would make the sharing arithmetic untestable.
	const baseSize = 2 << 20
	base := distinctBytes(baseSize, 0x31)
	pid := fx.createEmptyFile(ctx, "base.bin")
	fx.writeFile(ctx, pid, base)
	setFileSize(t, fx, "base.bin", uint64(len(base)))
	snap1 := takeSnapshot(t, fx)

	baseChunks := chunksOf(t, fx, "base.bin")
	if baseChunks < 2 {
		t.Fatalf("base.bin produced %d chunk(s), want >= 2 so sharing is measurable", baseChunks)
	}

	// Snapshot 2: nothing changed. It must reference exactly the same set.
	snap2 := takeSnapshot(t, fx)

	// Snapshot 3: one extra file, so it shares snap1's chunks and adds its own.
	extra := distinctBytes(4096, 0x32)
	pidExtra := fx.createEmptyFile(ctx, "extra.bin")
	fx.writeFile(ctx, pidExtra, extra)
	setFileSize(t, fx, "extra.bin", uint64(len(extra)))
	extraChunks := chunksOf(t, fx, "extra.bin")
	snap3 := takeSnapshot(t, fx)

	report, err := fx.rt.SnapshotUsage(ctx, fx.shareName, "")
	if err != nil {
		t.Fatalf("SnapshotUsage: %v", err)
	}
	if len(report.Skipped) != 0 {
		t.Fatalf("report skipped snapshots: %v", report.Skipped)
	}
	if len(report.Snapshots) != 3 {
		t.Fatalf("report covers %d snapshots, want 3", len(report.Snapshots))
	}

	// The union is snap1's chunks plus snap3's extra file. Snapshots 1 and 2 are
	// identical, so they contribute nothing beyond snap1.
	wantTotal := baseChunks + extraChunks
	if report.TotalChunks != wantTotal {
		t.Errorf("TotalChunks = %d, want %d (base %d + extra %d)",
			report.TotalChunks, wantTotal, baseChunks, extraChunks)
	}
	// The sum counts every snapshot separately, so snap1 and snap2 both count in
	// full: that is the overstatement the report exists to expose.
	wantSum := baseChunks + baseChunks + (baseChunks + extraChunks)
	if report.SumOfSnapshotChunks != wantSum {
		t.Errorf("SumOfSnapshotChunks = %d, want %d", report.SumOfSnapshotChunks, wantSum)
	}
	if report.SumOfSnapshotChunks <= report.TotalChunks {
		t.Errorf("SumOfSnapshotChunks (%d) <= TotalChunks (%d): the report is not measuring sharing",
			report.SumOfSnapshotChunks, report.TotalChunks)
	}

	byID := map[string]SnapshotUsage{}
	for _, u := range report.Snapshots {
		byID[u.ID] = u
	}

	// Snapshot 1 and 2 hold the same set, so NOTHING in either is unique: deleting
	// one of them frees zero chunks. This is the case an operator most needs to
	// see, and the one a naive "chunks = space" report would get wrong.
	for _, id := range []string{snap1, snap2} {
		u := byID[id]
		if u.Chunks != baseChunks {
			t.Errorf("snapshot %s: Chunks = %d, want %d", id, u.Chunks, baseChunks)
		}
		if u.Unique != 0 {
			t.Errorf("snapshot %s: Unique = %d, want 0 — another snapshot holds the same chunks",
				id, u.Unique)
		}
		if u.Shared != baseChunks {
			t.Errorf("snapshot %s: Shared = %d, want %d", id, u.Shared, baseChunks)
		}
	}

	// Snapshot 3 holds base (shared with 1 and 2) plus its own file (unique to it).
	u3 := byID[snap3]
	if u3.Chunks != baseChunks+extraChunks {
		t.Errorf("snapshot 3: Chunks = %d, want %d", u3.Chunks, baseChunks+extraChunks)
	}
	if u3.Unique != extraChunks {
		t.Errorf("snapshot 3: Unique = %d, want %d — only the extra file is unique to it",
			u3.Unique, extraChunks)
	}
	if u3.Shared != baseChunks {
		t.Errorf("snapshot 3: Shared = %d, want %d", u3.Shared, baseChunks)
	}
	if report.UniqueToSomeSnapshot != extraChunks {
		t.Errorf("UniqueToSomeSnapshot = %d, want %d", report.UniqueToSomeSnapshot, extraChunks)
	}

	// Every snapshot is closed, so none should report as open.
	for _, u := range report.Snapshots {
		if u.Open || u.Holders != 0 {
			t.Errorf("snapshot %s reports open=%v holders=%d with no holder registered",
				u.ID, u.Open, u.Holders)
		}
	}

	// Newest first.
	if report.Snapshots[0].ID != snap3 || report.Snapshots[len(report.Snapshots)-1].ID != snap1 {
		t.Errorf("snapshots not ordered newest first: %v", usageIDs(report.Snapshots))
	}
}

// TestSnapshotUsage_SingleSnapshotIsStillMeasuredAgainstTheRest pins the one
// subtlety of the per-id form: "unique" is only meaningful relative to the other
// snapshots, so a single-snapshot report must still read all of them.
func TestSnapshotUsage_SingleSnapshotIsStillMeasuredAgainstTheRest(t *testing.T) {
	meta := badgertest.NewInMemory(t)
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()
	ctx := context.Background()

	base := distinctBytes(2<<20, 0x41)
	pid := fx.createEmptyFile(ctx, "base.bin")
	fx.writeFile(ctx, pid, base)
	setFileSize(t, fx, "base.bin", uint64(len(base)))
	baseChunks := chunksOf(t, fx, "base.bin")
	snap1 := takeSnapshot(t, fx)

	extra := distinctBytes(4096, 0x42)
	pidExtra := fx.createEmptyFile(ctx, "extra.bin")
	fx.writeFile(ctx, pidExtra, extra)
	setFileSize(t, fx, "extra.bin", uint64(len(extra)))
	extraChunks := chunksOf(t, fx, "extra.bin")
	snap3 := takeSnapshot(t, fx)

	// Ask about snap3 alone. It shares base.bin's chunks with snap1, so its Unique
	// must be just the extra file even though snap1 is not in the report.
	report, err := fx.rt.SnapshotUsage(ctx, fx.shareName, snap3)
	if err != nil {
		t.Fatalf("SnapshotUsage: %v", err)
	}
	if len(report.Snapshots) != 1 || report.Snapshots[0].ID != snap3 {
		t.Fatalf("report covers %v, want just %s", usageIDs(report.Snapshots), snap3)
	}
	got := report.Snapshots[0]
	if got.Chunks != baseChunks+extraChunks {
		t.Errorf("Chunks = %d, want %d", got.Chunks, baseChunks+extraChunks)
	}
	if got.Unique != extraChunks {
		t.Errorf("Unique = %d, want %d: uniqueness must be measured against the OTHER snapshots, "+
			"not just the reported one", got.Unique, extraChunks)
	}
	_ = snap1
}

// TestSnapshotUsage_ReportsAnOpenSnapshot pins the field that saves an operator a
// failed delete: a snapshot held open by a boot reports as open with a holder
// count, which is exactly what the 409 will say.
func TestSnapshotUsage_ReportsAnOpenSnapshot(t *testing.T) {
	meta := badgertest.NewInMemory(t)
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()
	ctx := context.Background()

	pid := fx.createEmptyFile(ctx, "base.bin")
	fx.writeFile(ctx, pid, distinctBytes(4096, 0x51))
	setFileSize(t, fx, "base.bin", 4096)
	snapID := takeSnapshot(t, fx)

	release := fx.rt.OpenSnapshot(fx.shareName, snapID)
	defer release()

	report, err := fx.rt.SnapshotUsage(ctx, fx.shareName, snapID)
	if err != nil {
		t.Fatalf("SnapshotUsage: %v", err)
	}
	if len(report.Snapshots) != 1 {
		t.Fatalf("report covers %d snapshots, want 1", len(report.Snapshots))
	}
	if !report.Snapshots[0].Open || report.Snapshots[0].Holders != 1 {
		t.Errorf("open=%v holders=%d, want open=true holders=1",
			report.Snapshots[0].Open, report.Snapshots[0].Holders)
	}
}

// TestSnapshotUsage_NamesASnapshotWithNoManifest pins the difference between
// "holds nothing" and "not readable". Reporting a snapshot with a missing
// manifest as zero chunks would tell an operator a delete is free when the report
// simply does not know.
func TestSnapshotUsage_NamesASnapshotWithNoManifest(t *testing.T) {
	meta := badgertest.NewInMemory(t)
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()
	ctx := context.Background()

	pid := fx.createEmptyFile(ctx, "base.bin")
	fx.writeFile(ctx, pid, distinctBytes(4096, 0x61))
	setFileSize(t, fx, "base.bin", 4096)
	keep := takeSnapshot(t, fx)
	gone := takeSnapshot(t, fx)

	// Remove one manifest behind the store's back, which is the state a delete
	// leaves in its window and the state a partial restore can leave.
	snap, err := fx.rt.store.GetSnapshot(ctx, fx.shareName, gone)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if err := os.Remove(snap.ManifestPath(fx.localStoreDir)); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}

	report, err := fx.rt.SnapshotUsage(ctx, fx.shareName, "")
	if err != nil {
		t.Fatalf("SnapshotUsage: %v", err)
	}
	if len(report.Skipped) != 1 || report.Skipped[0] != gone {
		t.Errorf("Skipped = %v, want [%s]", report.Skipped, gone)
	}
	if len(report.Snapshots) != 1 || report.Snapshots[0].ID != keep {
		t.Errorf("report covers %v, want just %s", usageIDs(report.Snapshots), keep)
	}
}

// TestSnapshotUsage_RefusesAShareWithoutADataDirectory pins that a share holding
// no snapshots possible is an error, not an empty report: an empty report reads
// as "you have no snapshots", which is a different statement.
func TestSnapshotUsage_RefusesAShareWithoutADataDirectory(t *testing.T) {
	meta := badgertest.NewInMemory(t)
	fx := newByteVerifyFixture(t, meta, "memory")
	defer fx.close()

	if _, err := fx.rt.SnapshotUsage(context.Background(), "/nope", ""); err == nil {
		t.Fatal("SnapshotUsage accepted a share that does not exist")
	}
}

// takeSnapshot creates a snapshot and waits for it to be ready.
func takeSnapshot(t *testing.T, fx *byteVerifyFixture) string {
	t.Helper()
	ctx := context.Background()
	id, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{NoVerify: true})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, err := fx.rt.WaitForSnapshot(ctx, fx.shareName, id); err != nil {
		t.Fatalf("WaitForSnapshot: %v", err)
	}
	return id
}

// chunksOf reports how many chunks the manifest of a file currently references,
// which is what the snapshot will record.
func chunksOf(t *testing.T, fx *byteVerifyFixture, name string) int {
	t.Helper()
	return len(fx.getFile(context.Background(), name).Blocks)
}

func usageIDs(u []SnapshotUsage) []string {
	out := make([]string, 0, len(u))
	for _, s := range u {
		out = append(out, s.ID)
	}
	return out
}

// TestReadManifestSet_DistinguishesMissingFromEmpty pins the two outcomes the
// report depends on: a missing manifest is an error the caller turns into
// "skipped", an empty one is a legitimate zero.
func TestReadManifestSet_DistinguishesMissingFromEmpty(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "absent.hashes")
	if _, err := readManifestSet(missing); err == nil {
		t.Fatal("readManifestSet accepted a missing manifest")
	} else if !os.IsNotExist(err) {
		t.Fatalf("readManifestSet error = %v, want a not-exist error", err)
	}

	empty := filepath.Join(dir, "empty.hashes")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty manifest: %v", err)
	}
	set, err := readManifestSet(empty)
	if err != nil {
		t.Fatalf("readManifestSet(empty): %v", err)
	}
	if set.Len() != 0 {
		t.Fatalf("empty manifest parsed to %d hashes, want 0", set.Len())
	}
}

// TestSortUsageNewestFirst_IsStableForEqualTimestamps pins that the ordering is
// deterministic, so a report is reproducible and a diff between two runs means
// something.
func TestSortUsageNewestFirst_IsStableForEqualTimestamps(t *testing.T) {
	in := []SnapshotUsage{
		{ID: "a", CreatedAt: "2026-01-02T00:00:00Z"},
		{ID: "b", CreatedAt: "2026-01-03T00:00:00Z"},
		{ID: "c", CreatedAt: "2026-01-03T00:00:00Z"},
		{ID: "d", CreatedAt: "2026-01-01T00:00:00Z"},
	}
	sortUsageNewestFirst(in)
	want := []string{"b", "c", "a", "d"}
	for i, id := range want {
		if in[i].ID != id {
			t.Fatalf("order = %v, want %v", usageIDs(in), want)
		}
	}
}

var _ = block.ContentHash{}
