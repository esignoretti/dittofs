package snapshot

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// TestRunUsage_WholeShareNeedsNoResolution pins that the common invocation makes
// exactly one call and does not resolve anything. Resolving would list the
// snapshots on every usage request for no reason, and would also make the
// whole-share report fail on a share whose snapshot list is unreadable — even
// though the usage endpoint itself might have answered.
func TestRunUsage_WholeShareNeedsNoResolution(t *testing.T) {
	fc := &fakeClient{usageReport: &apiclient.SnapshotUsageReport{Share: "/archive", TotalChunks: 3}}
	withFakeClient(t, fc)

	usageID = ""
	if err := runUsage(nil, []string{"/archive"}); err != nil {
		t.Fatalf("runUsage: %v", err)
	}
	if len(fc.usageCalls) != 1 {
		t.Fatalf("usage called %d time(s), want 1", len(fc.usageCalls))
	}
	if fc.usageCalls[0] != "" {
		t.Errorf("usage called with id %q, want empty", fc.usageCalls[0])
	}
}

// TestRunUsage_ResolvesAPartialID pins that --id takes a prefix, the same way
// show/remove do. Without resolution the endpoint would 404 on an id the list
// command itself printed.
func TestRunUsage_ResolvesAPartialID(t *testing.T) {
	fc := &fakeClient{
		snapshots: map[string]*apiclient.Snapshot{
			"3f2a1b4c-0000-0000-0000-000000000001": {ID: "3f2a1b4c-0000-0000-0000-000000000001"},
			"9e8d7c6b-0000-0000-0000-000000000002": {ID: "9e8d7c6b-0000-0000-0000-000000000002"},
		},
		usageReport: &apiclient.SnapshotUsageReport{Share: "/archive"},
	}
	withFakeClient(t, fc)

	usageID = "3f2a1b4c"
	defer func() { usageID = "" }()
	if err := runUsage(nil, []string{"/archive"}); err != nil {
		t.Fatalf("runUsage: %v", err)
	}
	if len(fc.usageCalls) != 1 {
		t.Fatalf("usage called %d time(s), want 1", len(fc.usageCalls))
	}
	if got, want := fc.usageCalls[0], "3f2a1b4c-0000-0000-0000-000000000001"; got != want {
		t.Errorf("usage called with id %q, want the resolved %q", got, want)
	}
}

// TestRunUsage_RefusesAnAmbiguousID pins that a prefix matching two snapshots is
// an error rather than a guess. Reporting on the wrong snapshot's space would be
// worse than failing: the operator would delete the wrong one.
func TestRunUsage_RefusesAnAmbiguousID(t *testing.T) {
	fc := &fakeClient{
		snapshots: map[string]*apiclient.Snapshot{
			"abc1-0000": {ID: "abc1-0000"},
			"abc2-0000": {ID: "abc2-0000"},
		},
	}
	withFakeClient(t, fc)

	usageID = "abc"
	defer func() { usageID = "" }()
	if err := runUsage(nil, []string{"/archive"}); err == nil {
		t.Fatal("runUsage accepted an ambiguous snapshot id")
	}
	if len(fc.usageCalls) != 0 {
		t.Errorf("usage was called despite an ambiguous id: %v", fc.usageCalls)
	}
}

// TestRunUsage_SurfacesTheServerError pins that a refused request is reported,
// not swallowed into an empty table. An empty table would read as "no snapshots"
// and hide a 409 or a 500.
func TestRunUsage_SurfacesTheServerError(t *testing.T) {
	fc := &fakeClient{usageErr: errors.New("boom")}
	withFakeClient(t, fc)

	usageID = ""
	if err := runUsage(nil, []string{"/archive"}); err == nil {
		t.Fatal("runUsage swallowed a server error")
	}
}

// TestPrintUsageTable_RendersTheInterpretation pins that the table carries the
// numbers the operator came for, not just the per-snapshot rows: the union, the
// sum, and the count that a delete would free.
func TestPrintUsageTable_RendersTheInterpretation(t *testing.T) {
	// Nothing to assert about formatting beyond "it does not error and does not
	// panic on the empty and populated cases"; the numbers themselves are
	// covered by the runtime tests, which own the arithmetic.
	report := &apiclient.SnapshotUsageReport{
		Share:                "/archive",
		TotalChunks:          7,
		SumOfSnapshotChunks:  20,
		UniqueToSomeSnapshot: 5,
		Snapshots: []apiclient.SnapshotUsage{
			{ID: "3f2a1b4c-0000-0000-0000-000000000001", CreatedAt: "2026-01-02T00:00:00Z", Chunks: 7, Unique: 2, Shared: 5},
			{ID: "9e8d7c6b-0000-0000-0000-000000000002", CreatedAt: "2026-01-01T00:00:00Z", Chunks: 7, Unique: 0, Shared: 7, Open: true, Holders: 1},
		},
	}
	if err := printUsageTable("/archive", report); err != nil {
		t.Fatalf("printUsageTable: %v", err)
	}
	if err := printUsageTable("/archive", &apiclient.SnapshotUsageReport{Share: "/archive"}); err != nil {
		t.Fatalf("printUsageTable(empty): %v", err)
	}
	if err := printUsageTable("/archive", &apiclient.SnapshotUsageReport{
		Share:   "/archive",
		Skipped: []string{"snap-gone"},
	}); err != nil {
		t.Fatalf("printUsageTable(skipped): %v", err)
	}
}

// TestShortID pins the prefix length so usage and list agree on it.
func TestShortID(t *testing.T) {
	if got := shortID("3f2a1b4c-0000-0000-0000-000000000001"); got != "3f2a1b4c" {
		t.Errorf("shortID = %q, want %q", got, "3f2a1b4c")
	}
	if got := shortID("short"); got != "short" {
		t.Errorf("shortID(short) = %q, want %q", got, "short")
	}
}
