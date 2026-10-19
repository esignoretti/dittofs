package journalv2

import "io"

// Offer is a frozen view of one file's dirty content, taken when an offload
// call starts. A write or a removal while the call's callback runs changes what
// ReadAt returns, never what the offer returns: the offered records stay in
// their segments, unmoved, until the callback returns. They are synced before
// the callback is called.
type Offer struct {
	ID         FileID
	Dirty      []Extent // the extents offered
	Neighbours []Extent // offloaded held extents contiguous with Dirty, offered on request (widen)
	Superseded []Held   // superseded versions the journal still retains for offload
	Oldest     Version  // the oldest content version among the offered records, neighbours included
	Newest     Version  // the newest

	// Offered reads the offered bytes at file offsets, for as long as the
	// callback runs. A read outside the offered extents fails with
	// ErrNotOffered, and one after the callback returns with ErrOfferEnded,
	// rather than returning other bytes. It returns no unverified byte: before
	// it returns any byte of a record it verifies the record's whole payload,
	// and a record that does not verify fails the read with ErrCorrupt.
	Offered io.ReaderAt
}

// Offload offers the dirty content of file id and marks exactly the extents
// reported offloaded. It is OffloadMany over one file.
func (h *Handle) Offload(id FileID, limit, widen int64, fn func(o Offer, report func(offloaded []Extent) error) error) error {
	return ErrNotImplemented
}

// OffloadMany offers the dirty content of the files ids in one callback, so
// the caller can pack several files into one block, and marks exactly the
// extents reported offloaded. Offloading is how dirty content becomes
// evictable; the journal itself uploads nothing.
//
// What is offered: extents whose offloaded bit is unset, in offset order,
// until the next would pass limit, the bound on the dirty bytes offered across
// all the files; an extent larger than what remains is offered as a prefix
// ending at the limit. With widen above zero, each offer also carries the
// offloaded held extents contiguous with an offered one, up to widen bytes on
// each side, frozen like the dirty bytes. An offer stops adding extents once
// the segments it pins would pass a fixed per-call bound, or the segments all
// running offers pin together would pass a journal-wide bound; the bounds
// apply to the whole call, and the call's first extent always fits. An offer
// takes no extent from a segment a running repack pass has selected.
//
// report marks the extents it names as offloaded at once, so they become
// evictable without waiting for the rest of the call. It may be called any
// number of times while fn runs, and costs O(k log n) for k reported extents
// of a file holding n, never scanning the file's other extents. It intersects
// what it is given with what was offered and nothing more: an extent not
// offered is rejected with ErrNotOffered; a neighbour, or an extent superseded
// or removed meanwhile, marks nothing. It appends an offloaded record and
// returns that append's error, marking nothing on failure; below half the
// reserved headroom it appends none. After fn returns, report fails with
// ErrOfferEnded.
//
// Partial success is honoured: every extent reported before fn returns stays
// marked, whatever fn returns, and the error fn returns covers only what was
// not reported. No lock is held while fn runs.
func (h *Handle) OffloadMany(ids []FileID, limit, widen int64, fn func(offers []Offer, report func(id FileID, offloaded []Extent) error) error) error {
	return ErrNotImplemented
}

// MarkOffloaded sets the offloaded bit of the held extents of file id that
// extents names, for content whose versions lie between oldest and newest,
// the versions recorded on the ref that covers it. It never clears a bit. It
// appends a record so the bits survive a restart, except below half the
// reserved headroom, where it sets the bits and appends none, so a crash
// loses them. It may be called on any file at any time.
func (h *Handle) MarkOffloaded(id FileID, extents []Extent, oldest, newest Version) error {
	return ErrNotImplemented
}

// Unmark clears the offloaded bit of the held extents of file id that extents
// names, and appends a record so the change survives a restart. It may be
// called on any file at any time.
func (h *Handle) Unmark(id FileID, extents []Extent) error {
	return ErrNotImplemented
}

// Release stops holding extents of file id. It is how content is evicted, and
// it is all or nothing: if any named extent's offloaded bit is unset, it fails
// with a NotOffloadedError listing them, and releases nothing. The caller
// releases only content whose offload commit has itself committed; the journal
// cannot check that ordering.
//
// Release frees no storage: the released bytes become unreclaimed, still
// allocated in their segment, until repack or the segment's retirement returns
// the space. It appends one release record per extent, drawn on the reserved
// headroom, and leaves no removal marker. It is refused by a NoSpaceError
// naming LimitHeadroom when the headroom is exhausted, or LimitIndex when
// splitting a held extent would pass the index's cap on what removals add. It
// takes the file append domain.
func (h *Handle) Release(id FileID, extents []Extent) error {
	return ErrNotImplemented
}
