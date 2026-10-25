package snapshot

import (
	"errors"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// TestRunRehydrate_PassesBothNames pins that the two names reach the wire
// unaltered. A snapshot holds every volume in the share, so a dropped --source
// would make the server copy the wrong one, and a dropped --volume would create
// a volume nobody named.
func TestRunRehydrate_PassesBothNames(t *testing.T) {
	fc := &fakeClient{
		snapshots: map[string]*apiclient.Snapshot{"snap-1": {ID: "snap-1"}},
		rehydrateJob: &apiclient.RehydrateJob{
			ID: "job-1", State: "done", Share: "/vm",
			SnapshotID: "snap-1", SourceVolume: "boot", Volume: "boot-2",
			StartedAt: "2026-01-02T00:00:00Z",
		},
	}
	withFakeClient(t, fc)

	rehydrateSource, rehydrateDest = "boot", "boot-2"
	rehydrateWait = true
	defer func() { rehydrateSource, rehydrateDest = "", "" }()

	if err := runRehydrate(nil, []string{"/vm", "snap-1"}); err != nil {
		t.Fatalf("runRehydrate: %v", err)
	}
	if len(fc.rehydrateReqs) != 1 {
		t.Fatalf("rehydrate called %d time(s), want 1", len(fc.rehydrateReqs))
	}
	got := fc.rehydrateReqs[0]
	if got.SourceVolume != "boot" || got.Volume != "boot-2" {
		t.Errorf("request = %+v, want source boot volume boot-2", got)
	}
}

// TestRunRehydrate_FailedJobIsACommandFailure pins that a job which reached
// "failed" makes the command exit non-zero. A command that printed "failed" and
// exited 0 would let a script continue as if the volume existed.
func TestRunRehydrate_FailedJobIsACommandFailure(t *testing.T) {
	fc := &fakeClient{
		snapshots: map[string]*apiclient.Snapshot{"snap-1": {ID: "snap-1"}},
		rehydrateJob: &apiclient.RehydrateJob{
			ID: "job-2", State: "failed", Share: "/vm",
			SnapshotID: "snap-1", SourceVolume: "boot", Volume: "boot-2",
			StartedAt: "2026-01-02T00:00:00Z",
			Error:     "rehydrate: no remote store configured",
		},
	}
	withFakeClient(t, fc)

	rehydrateSource, rehydrateDest = "boot", "boot-2"
	rehydrateWait = true
	defer func() { rehydrateSource, rehydrateDest = "", "" }()

	err := runRehydrate(nil, []string{"/vm", "snap-1"})
	if err == nil {
		t.Fatal("runRehydrate returned nil for a failed job")
	}
	// The server's message is the only explanation of why the copy did not
	// happen, so it must survive into the error.
	if !strings.Contains(err.Error(), "no remote store configured") {
		t.Errorf("error %q does not carry the server's reason", err)
	}
}

// TestRunRehydrate_NoWaitDoesNotPoll pins that --wait=false returns after the
// POST. Polling anyway would make the flag a lie and block a script that asked
// not to block.
func TestRunRehydrate_NoWaitDoesNotPoll(t *testing.T) {
	fc := &fakeClient{
		snapshots: map[string]*apiclient.Snapshot{"snap-1": {ID: "snap-1"}},
		rehydrateJob: &apiclient.RehydrateJob{
			ID: "job-3", State: "pending", Share: "/vm",
			SnapshotID: "snap-1", SourceVolume: "boot", Volume: "boot-2",
			StartedAt: "2026-01-02T00:00:00Z",
		},
	}
	withFakeClient(t, fc)

	rehydrateSource, rehydrateDest = "boot", "boot-2"
	rehydrateWait = false
	defer func() {
		rehydrateSource, rehydrateDest = "", ""
		rehydrateWait = true
	}()

	if err := runRehydrate(nil, []string{"/vm", "snap-1"}); err != nil {
		t.Fatalf("runRehydrate: %v", err)
	}
	if len(fc.rehydrateReqs) != 1 {
		t.Errorf("rehydrate called %d time(s), want exactly 1 with no polling", len(fc.rehydrateReqs))
	}
}

// TestRunRehydrate_ResolvesAPartialSnapshotID pins the same prefix resolution
// the other snapshot commands offer.
func TestRunRehydrate_ResolvesAPartialSnapshotID(t *testing.T) {
	full := "3f2a1b4c-0000-0000-0000-000000000001"
	fc := &fakeClient{
		snapshots: map[string]*apiclient.Snapshot{full: {ID: full}},
		rehydrateJob: &apiclient.RehydrateJob{
			ID: "job-4", State: "done", Share: "/vm", SnapshotID: full,
			SourceVolume: "boot", Volume: "boot-2", StartedAt: "2026-01-02T00:00:00Z",
		},
	}
	withFakeClient(t, fc)

	rehydrateSource, rehydrateDest = "boot", "boot-2"
	rehydrateWait = true
	defer func() { rehydrateSource, rehydrateDest = "", "" }()

	if err := runRehydrate(nil, []string{"/vm", "3f2a1b4c"}); err != nil {
		t.Fatalf("runRehydrate: %v", err)
	}
	if len(fc.rehydrateReqs) != 1 {
		t.Fatalf("rehydrate called %d time(s), want 1", len(fc.rehydrateReqs))
	}
}

// TestRunRehydrate_ServerRefusalIsReported pins that a rejected POST (a 409 on
// an existing volume, say) is surfaced rather than treated as a started job.
func TestRunRehydrate_ServerRefusalIsReported(t *testing.T) {
	fc := &fakeClient{
		snapshots:    map[string]*apiclient.Snapshot{"snap-1": {ID: "snap-1"}},
		rehydrateErr: errors.New("volume already exists"),
	}
	withFakeClient(t, fc)

	rehydrateSource, rehydrateDest = "boot", "boot-2"
	defer func() { rehydrateSource, rehydrateDest = "", "" }()

	if err := runRehydrate(nil, []string{"/vm", "snap-1"}); err == nil {
		t.Fatal("runRehydrate swallowed a refused request")
	}
}

// TestRunRehydrateStatus_ReportsTheJob pins the status subcommand, which is the
// only way to check a job started with --wait=false.
func TestRunRehydrateStatus_ReportsTheJob(t *testing.T) {
	fc := &fakeClient{
		rehydrateJob: &apiclient.RehydrateJob{
			ID: "job-5", State: "copying", Share: "/vm",
			SnapshotID: "snap-1", SourceVolume: "boot", Volume: "boot-2",
			StartedAt: "2026-01-02T00:00:00Z",
		},
	}
	withFakeClient(t, fc)

	if err := runRehydrateStatus(nil, []string{"/vm", "job-5"}); err != nil {
		t.Fatalf("runRehydrateStatus: %v", err)
	}
	if err := runRehydrateStatus(nil, []string{"/vm", "job-unknown"}); err == nil {
		t.Fatal("runRehydrateStatus accepted an unknown job id")
	}
}
