package nbd

import (
	"context"
	"io"
	"math"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
)

// handleWrite serves one NBD_CMD_WRITE.
//
// The checks run in this order — flags, read-only, zero length, overflow, past
// the end, payload, then the store — and the order is the point. The range
// check reads the request's own fields, so it cannot be trusted until they have
// been shown to make sense; and a rejected write still has its payload in
// flight, so the reply and the drain have to be arranged around bytes the
// client has already sent.
//
// decision: the payload is written with bs.WriteAt on a deadline-wrapped
// context and every error is routed through the one mapError, rather than
// through common.WriteToBlockStore. That helper takes *engine.Store concretely,
// and widening it to an interface would force a second helper for the read
// path; what it adds over the raw call is deadline wrapping and error
// normalization, which is two lines here. The commit seam is the one that must
// stay shared across the adapters, and it is: common.CommitBlockStore.
func (h *Handler) handleWrite(ctx context.Context, bs BlockStore, exp *Export, req *Request, conn net.Conn) error {
	// A flag that is not documented for WRITE is a malformed request. FUA is
	// the exception: it is valid on every command once SEND_FUA is advertised,
	// and on WRITE it is honoured rather than ignored.
	if req.Flags&^FlagFUA != 0 {
		return h.rejectWrite(conn, req, ErrEINVAL)
	}

	// Checked before the length and the range: a read-only export refuses the
	// write wherever it lands.
	if exp.ReadOnly {
		return h.rejectWrite(conn, req, ErrEPERM)
	}

	// A zero-length write carries no payload, so it is answered without a
	// drain. Draining here would consume bytes that were never sent and park
	// the connection until the read deadline.
	if req.Length == 0 {
		return h.writeReply(conn, req.Cookie, ErrEINVAL, nil)
	}

	// A subtraction rather than offset+length, which wraps: a request far past
	// the end would otherwise look like it starts inside the export.
	if req.Offset > math.MaxUint64-uint64(req.Length) {
		return h.rejectWrite(conn, req, ErrEIO)
	}

	// A write past the end is NBD_ENOSPC, not NBD_EINVAL: the spec reserves
	// EINVAL for a read or trim past the end, because a write past the end
	// means the client's view of the device is too large while a read or trim
	// past the end is a malformed request. The export size is fixed at volume
	// creation, so a write never grows it.
	if req.Offset+uint64(req.Length) > exp.Size {
		return h.rejectWrite(conn, req, ErrENOSPC)
	}

	// A payload beyond the advertised maximum is refused before allocating it.
	//
	// decision: EINVAL here, not EOVERFLOW, even though READ answers the same
	// overage with EOVERFLOW. The spec's EOVERFLOW clause is specifically "when a
	// command WITHOUT payload requests a length larger than an advertised
	// maximum" — WRITE carries a payload, so that clause does not cover it and
	// EINVAL is the general answer for a request that violates the advertised
	// contract. The asymmetry is deliberate and is the spec's, not an oversight.
	//
	// The payload is in flight regardless ("The client MUST follow the request
	// header with length number of bytes"), so rejectWrite drains it: replying
	// and returning without draining would parse payload bytes as the next
	// header.
	if payloadTooLarge(req.Length) {
		return h.rejectWrite(conn, req, ErrEINVAL)
	}

	// Read bounds finishing a request that has already started. The header read
	// consumed the budget armed in the dispatch loop, and that budget is the
	// Idle one when the operator has separated the two — so without a fresh arm
	// here the payload read inherits the wrong timer and Read bounds nothing at
	// all. The same arm, from the same field, is what makes the split real.
	if err := conn.SetReadDeadline(time.Now().Add(h.timeouts.Read)); err != nil {
		return err
	}

	// The payload is read into a buffer sized to the request. LimitReader
	// bounds the read to the declared length, so a client that sends more than
	// it declared cannot make the server consume the next request header as
	// payload.
	data := make([]byte, req.Length)
	if _, err := io.ReadFull(io.LimitReader(conn, int64(req.Length)), data); err != nil {
		// The client stopped mid-payload on a request the server had already
		// accepted, so the stream position is unknown: returning ends Serve,
		// which closes the connection. A success reply for bytes the server
		// never saw is the one answer that must not be sent.
		return err
	}

	// Built per command and never cached: it embeds the command's Context, so a
	// cached one would carry the first command's deadline and cancellation into
	// every later command on the connection.
	authCtx := h.authContext(ctx, exp)

	// newSize is offset+length, already shown not to wrap. It is a ceiling
	// rather than a truncation: the metadata layer applies the maximum of the
	// old and new sizes, so a write at a lower offset never shrinks the file.
	intent, err := h.meta.PrepareWrite(authCtx, exp.Handle, req.Offset+uint64(req.Length))
	if err != nil {
		return h.writeReply(conn, req.Cookie, mapError(err), nil)
	}

	// The returned []block.ChunkRef is discarded: the canonical chunk
	// projection is produced at Flush time, not once per write
	// (pkg/block/engine/readwrite.go).
	writeCtx, cancel := common.WithRequestDeadline(ctx)
	defer cancel()
	if _, err := bs.WriteAt(writeCtx, string(intent.PayloadID), nil, data, req.Offset); err != nil {
		return h.writeReply(conn, req.Cookie, mapError(err), nil)
	}

	if _, err := h.meta.CommitWrite(authCtx, intent); err != nil {
		return h.writeReply(conn, req.Cookie, mapError(err), nil)
	}

	// FUA must be durable before the reply: the spec says the server "MUST NOT
	// send its reply to that command until all write operations ... have been
	// completed and persisted to non-volatile storage". The commit runs before
	// the reply write, never after it, so a client that has seen the reply has
	// already had the guarantee it asked for.
	if req.Flags&FlagFUA != 0 {
		if err := h.commit(ctx, bs, exp); err != nil {
			return h.writeReply(conn, req.Cookie, mapError(err), nil)
		}
	}

	return h.writeReply(conn, req.Cookie, 0, nil)
}

// handleTrim serves one NBD_CMD_TRIM.
//
// decision: TRIM is served by PunchHole, which zero-overwrites the range rather
// than leaving its contents undefined. The spec permits either — "A server MAY
// discard length bytes starting at offset, but is not required to" — and it also
// says a client "MUST NOT make any assumptions about the contents of the export
// affected by this command", so zeros are a legal answer and the only
// deterministic one. The alternative, a cheaper discard that leaves reads
// non-deterministic, is the far worse property: the zero chunks dedup to a
// single CAS object, so the over-delivery costs almost nothing in aggregate.
func (h *Handler) handleTrim(ctx context.Context, bs BlockStore, exp *Export, req *Request, conn net.Conn) error {
	return h.handleDiscard(ctx, bs, exp, req, conn, FlagFUA, ErrEINVAL)
}

// handleWriteZeroes serves one NBD_CMD_WRITE_ZEROES.
//
// decision: the range is punched, never written as a zero buffer. Writing zeros
// through WriteAt is the reference library's bug: on a content-addressed store
// it MATERIALIZES real zero chunks, so a filesystem's discard path allocates
// storage instead of freeing it. PunchHole reclaims the chunks that fall
// entirely inside the range and zero-overwrites the rest, which is why the unit
// test's `st.punches == 1` and `st.writes == 0` is the guard on this decision.
//
// NO_HOLE is accepted: the spec says "The server MUST support the use of this
// flag if it advertises NBD_FLAG_SEND_WRITE_ZEROES", and the adapter does. It is
// the client asking that the range be genuinely allocated rather than left
// sparse, and PunchHole already zero-overwrites rather than leaving a hole, so
// honouring it takes no extra work. Refusing it would break a MUST on the
// strength of the rule that makes NO_HOLE invalid on WRITE.
func (h *Handler) handleWriteZeroes(ctx context.Context, bs BlockStore, exp *Export, req *Request, conn net.Conn) error {
	return h.handleDiscard(ctx, bs, exp, req, conn, FlagFUA|FlagNoHole, ErrENOSPC)
}

// handleDiscard is the shared body of TRIM and WRITE_ZEROES: the two commands
// differ only in which flags they document and which error the spec gives an
// out-of-range range, so those two values are the parameters and everything else
// is one implementation.
//
// The checks run in this order — flags, read-only, zero length, overflow, past
// the end, then the store — the same order handleWrite uses and for the same
// reason: the range check reads the request's own fields, so it cannot be
// trusted until they have been shown to make sense.
func (h *Handler) handleDiscard(ctx context.Context, bs BlockStore, exp *Export, req *Request, conn net.Conn, allowed CommandFlags, outOfRange NBDError) error {
	// A flag that is not documented for this command is a malformed request.
	// FUA is accepted on both, because the spec requires the server to accept it
	// on every command once SEND_FUA is advertised. NO_HOLE is accepted on
	// WRITE_ZEROES only.
	if req.Flags&^allowed != 0 {
		return h.writeReply(conn, req.Cookie, ErrEINVAL, nil)
	}

	// Checked before the length and the range: a read-only export refuses the
	// mutation wherever it lands. The export still advertises SEND_TRIM and
	// SEND_WRITE_ZEROES while read-only (handshake.go sets both unconditionally
	// and only adds READ_ONLY), so this is not a client violating the
	// advertisement — it is the case the spec's error list names explicitly: "it
	// SHOULD return NBD_EPERM if it receives a write or trim request on a
	// read-only export".
	if exp.ReadOnly {
		return h.writeReply(conn, req.Cookie, ErrEPERM, nil)
	}

	// Neither command carries a payload, so a rejection is answered without a
	// drain: there are no bytes in flight to consume, and draining here would
	// read the next request header as payload.
	if req.Length == 0 {
		return h.writeReply(conn, req.Cookie, ErrEINVAL, nil)
	}

	// A subtraction rather than offset+length, which wraps: a request far past
	// the end would otherwise look like it starts inside the export. The same
	// guard handleRead uses, for the same reason.
	if req.Offset > math.MaxUint64-uint64(req.Length) {
		return h.writeReply(conn, req.Cookie, ErrEIO, nil)
	}

	// Out of range is refused, never clamped, and these two checks are what make
	// the clamp unnecessary. A clamp written as end-offset underflows when the
	// offset is already past the end (offset=10000, length=100, size=8192 gives
	// 18446744073709549808), and the engine's own overflow guard would then
	// report a confusing "punch range overflow" instead of the right answer.
	//
	// decision: the two commands answer different codes here, because the spec
	// gives them different rules — "it SHOULD return NBD_EINVAL if it receives a
	// read or trim request including one or more sectors beyond the size of the
	// device", while "The server SHOULD return NBD_ENOSPC if it receives a write
	// zeroes request including one or more sectors beyond the size of the
	// device". outOfRange carries the caller's code.
	if req.Offset >= exp.Size {
		return h.writeReply(conn, req.Cookie, outOfRange, nil)
	}

	// The last byte of the export is addressable, so a range that ends exactly
	// at the end is served.
	if req.Offset+uint64(req.Length) > exp.Size {
		return h.writeReply(conn, req.Cookie, outOfRange, nil)
	}

	// The metadata punch is not optional, and it is not only about reclaim.
	//
	// It is the only authorization check on this path: metadata.Service.PunchHole
	// calls checkWritePermission (pkg/metadata/sparse.go:80) while
	// engine.Store.PunchHole checks nothing, so a discard that went straight to
	// the engine would be the one mutating command in the adapter that no
	// permission funnel sees — and it destroys data.
	//
	// And without it the storage is never freed: engine.Store.PunchHole does the
	// reap (DecrementRefCountAndReapMany plus ReprojectBlocks) only inside
	// `if len(currentBlocks) > 0` (pkg/block/engine/readwrite.go:308), so calling
	// it with nil skips the reap and only zero-overwrites the range. The command
	// would look like it worked and free nothing, which is the one thing a
	// discard exists to do. res.PreOpBlocks is the pre-op snapshot the reap needs
	// and res.PayloadID is the payload the file actually addresses.
	//
	// Built per command and never cached, for the same reason handleWrite builds
	// its own: it embeds the command's Context.
	authCtx := h.authContext(ctx, exp)
	res, err := h.meta.PunchHole(authCtx, exp.Handle, req.Offset, uint64(req.Length))
	if err != nil {
		return h.writeReply(conn, req.Cookie, mapError(err), nil)
	}

	// The deadline wrapper is applied per command, not per connection, and it is
	// a no-op when the context already carries one. The metadata call above is
	// bounded by its own service timeout; the block-store call is not, so this is
	// the only thing standing between a stalled remote tier and a connection
	// goroutine held open indefinitely. Same wrapper, same reason, as
	// handlers_read.go:93 and handlers_write.go:120.
	punchCtx, cancel := common.WithRequestDeadline(ctx)
	defer cancel()
	if _, err := bs.PunchHole(punchCtx, string(res.PayloadID), res.PreOpBlocks, req.Offset, uint64(req.Length)); err != nil {
		return h.writeReply(conn, req.Cookie, mapError(err), nil)
	}

	// FUA must be durable before the reply, exactly as WRITE does: the spec says
	// the server "MUST NOT send its reply to that command until all write
	// operations ... have been completed and persisted to non-volatile storage".
	if req.Flags&FlagFUA != 0 {
		if err := h.commit(ctx, bs, exp); err != nil {
			return h.writeReply(conn, req.Cookie, mapError(err), nil)
		}
	}

	// Neither command has a reply payload.
	return h.writeReply(conn, req.Cookie, 0, nil)
}

// rejectWrite answers a write that failed a check and then consumes the payload
// the client has already sent.
//
// The spec requires the client to follow the request header with length bytes
// unconditionally, so a rejection that returns without consuming them leaves
// the next request header to be read out of payload bytes. The connection then
// desynchronizes into garbage, and the symptom appears on an unrelated command
// minutes later.
//
// decision: the reply is written before the drain. The reply travels
// server→client and the payload client→server, so neither orders the other,
// and replying first is what lets a client that declared a huge length and
// stalled learn the reason instead of seeing only a closed socket. The drain is
// still required, and unchanged by when the reply goes out: its whole job is to
// put the next header at the right offset.
func (h *Handler) rejectWrite(conn net.Conn, req *Request, nbdErr NBDError) error {
	if err := h.writeReply(conn, req.Cookie, nbdErr, nil); err != nil {
		return err
	}

	// Re-armed from the request budget rather than left to the deadline set
	// before ReadRequest: that read consumed part of the budget, and the drain
	// is where a hostile or stalled client parks.
	if err := conn.SetReadDeadline(time.Now().Add(h.timeouts.Read)); err != nil {
		return err
	}
	return h.drainWritePayload(conn, req.Length)
}

// drainWritePayload consumes length bytes of a WRITE payload the client has
// already sent, so that the next request header is read from the right offset.
//
// It arms no deadline of its own, and that is the point of the split: the caller
// owns the choice. rejectWrite arms the Read budget because the connection is
// otherwise idle and a stalled client would park in the drain; the draining path
// deliberately does not, because the shutdown path has already put a short
// deadline on the connection and replacing it would hold the drain open (see the
// call site in serve).
//
// io.CopyN rather than a buffer of length bytes, so a client that declares a
// gigabyte costs a bounded buffer instead of an allocation.
func (h *Handler) drainWritePayload(conn net.Conn, length uint32) error {
	if length == 0 {
		return nil
	}
	if _, err := io.CopyN(io.Discard, conn, int64(length)); err != nil {
		// The stream position is unknown now, so the connection cannot be
		// reused for another command: returning ends Serve, which closes it.
		return err
	}
	// Drained: the next header is read from the right offset, so the loop
	// continues and the connection stays usable.
	return nil
}
