package journalv2

import "iter"

// Inspect returns a way to read out the journal's placement index, per file
// and in bulk, read its per-segment accounting, and verify the index against
// the segments. None of it mutates the journal or stops it.
func (j *Journal) Inspect() Inspector {
	return Inspector{}
}

// Inspector reads out a journal's index and segments. Its methods are safe for
// concurrent use with every other operation on the journal.
type Inspector struct{}

// File returns the index entries of file id, in offset order.
func (in Inspector) File(id FileID) ([]IndexEntry, error) {
	return nil, ErrNotImplemented
}

// All yields the index entries of every file in the index, one file at a time.
func (in Inspector) All() iter.Seq2[FileID, []IndexEntry] {
	return func(func(FileID, []IndexEntry) bool) {}
}

// Segments returns the accounting of every segment the journal owns. Stats
// gives the totals; this is the breakdown behind them.
func (in Inspector) Segments() []SegmentInfo {
	return nil
}

// Verify checks the index against the segments and reports what does not
// agree. It reports and never repairs: repair is a separate, explicit action.
func (in Inspector) Verify() (VerifyReport, error) {
	return VerifyReport{}, ErrNotImplemented
}

// EntryKind is what an index entry records about its extent.
type EntryKind uint8

// The entry kinds.
const (
	// EntryHeld is an extent the journal holds: a read serves it.
	EntryHeld EntryKind = iota + 1

	// EntryRetained is superseded content kept until its superseding write is
	// synced, or while a snapshot hold keeps it. A read does not serve it.
	EntryRetained

	// EntryMarker is a removal not yet settled: a truncate, deallocate or
	// delete the caller has not settled.
	EntryMarker
)

// IndexEntry is one entry of a file's placement index.
type IndexEntry struct {
	Kind    EntryKind
	Extent  Extent
	Version Version

	// Location is where in local storage the entry's bytes sit.
	//
	// decision: an entry is one piece of one record (RFC 1 §4.1), so its
	// location is that record's, the same RecordRef Verify reports.
	Location RecordRef

	// Offloaded is the entry's offloaded bit. It is never an answer about
	// whether the content is offloaded: the journal is not authoritative for
	// that.
	Offloaded bool
}

// RecordRef locates a record: the segment holding it and its offset there.
type RecordRef struct {
	Segment SegmentID
	Offset  int64
}

// SegmentInfo is one segment's accounting.
type SegmentInfo struct {
	ID     SegmentID
	Stream uint32
	Sealed bool

	// Failed reports that a sync or an append into the segment failed, so it
	// takes no further append.
	Failed bool

	// Allocated is the storage the segment is accounted at, in bytes.
	Allocated int64

	HeldBytes     int64
	RetainedBytes int64

	// Damaged reports that the segment holds records that do not verify.
	Damaged bool
}

// VerifyReport is what Verify found.
type VerifyReport struct {
	// Unsupported are the index entries no record on the segments supports.
	Unsupported []IndexEntry

	// Unreferenced are the records the segments hold that no index entry
	// references.
	Unreferenced []RecordRef

	// BadFooters are the segments whose footer did not verify.
	BadFooters []SegmentID

	// FootprintMismatch is the accounted footprint minus the sum of the
	// segments' sizes, in bytes, when the two differ; nil when they agree.
	FootprintMismatch *int64
}
