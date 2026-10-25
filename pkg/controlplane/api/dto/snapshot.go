// Package dto holds the wire-level data transfer objects shared
// between the REST handlers and the typed apiclient. Both sides
// import this package; neither depends on the other.
package dto

import "time"

// Snapshot is the wire representation of a share snapshot.
type Snapshot struct {
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	Share         string    `json:"share"`
	State         string    `json:"state"`
	RemoteDurable bool      `json:"remote_durable"`
	Scheduled     bool      `json:"scheduled"`
	ManifestCount int       `json:"manifest_count,omitempty"`
	DumpBytes     int64     `json:"dump_bytes,omitempty"`
	RetryOf       string    `json:"retry_of,omitempty"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// SnapshotUsage is one snapshot's share of what a share's snapshots hold.
//
// Counts, not bytes: a manifest line is a content hash with no size, so a
// manifest walk knows exactly how many chunks exist and nothing about their
// bytes. Converting would mean one remote request per distinct hash, so the
// report stays in counts and the byte figure comes from the reclaimer after a
// real delete.
type SnapshotUsage struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	// Chunks is how many distinct chunks this snapshot's manifest references.
	Chunks int `json:"chunks"`
	// Unique is how many of those no OTHER snapshot references: what deleting
	// this one would actually free.
	Unique int `json:"unique"`
	// Shared is the rest — what it holds that something else holds too.
	Shared int `json:"shared"`
	// Open reports whether a holder has it registered, so a delete would be
	// refused right now.
	Open    bool `json:"open"`
	Holders int  `json:"holders"`
}

// SnapshotUsageReport is the whole picture for one share.
type SnapshotUsageReport struct {
	Share     string          `json:"share"`
	Snapshots []SnapshotUsage `json:"snapshots"`
	// TotalChunks is the union across every snapshot: what they hold between
	// them.
	TotalChunks int `json:"total_chunks"`
	// SumOfSnapshotChunks counts each snapshot separately. The gap between it
	// and TotalChunks is the sharing, which is why retention is cheap.
	SumOfSnapshotChunks int `json:"sum_of_snapshot_chunks"`
	// UniqueToSomeSnapshot is how many chunks exactly one snapshot references.
	UniqueToSomeSnapshot int `json:"unique_to_some_snapshot"`
	// Skipped names snapshots whose manifest could not be read, so a report is
	// never silently smaller than the snapshot list.
	Skipped []string `json:"skipped,omitempty"`
}

// CreateSnapshotRequest is the body for POST .../snapshots.
type CreateSnapshotRequest struct {
	Name     string `json:"name,omitempty"`
	NoVerify bool   `json:"no_verify,omitempty"`
	RetryOf  string `json:"retry_of,omitempty"`
}

// CreateSnapshotResponse is the 202 body returned by POST .../snapshots.
type CreateSnapshotResponse struct {
	SnapshotID string `json:"snapshot_id"`
	Share      string `json:"share"`
}

// RestoreSnapshotRequest is the body for POST .../snapshots/{id}/restore.
type RestoreSnapshotRequest struct {
	AllowNonDurable bool `json:"allow_non_durable,omitempty"`
}

// RestoreSnapshotResponse is the 200 body returned by POST .../snapshots/{id}/restore.
// SafetySnapshotID is the ID of the pre-restore safety snapshot taken
// before the destructive reset step. Empty if no safety snap was
// created (precheck or pre-verify failure).
type RestoreSnapshotResponse struct {
	SnapshotID       string `json:"snapshot_id"`
	SafetySnapshotID string `json:"safety_snapshot_id,omitempty"`
	Share            string `json:"share"`
}
