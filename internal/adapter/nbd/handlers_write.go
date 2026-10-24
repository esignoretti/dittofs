package nbd

import (
	"context"
	"math"

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
func (h *Handler) handleWrite(ctx context.Context, bs BlockStore, exp *Export, req *Request, data []byte) reply {
	// data is the payload the caller already read, sized to req.Length. This
	// function never touches the connection: reading the stream is the
	// dispatcher's job, because it is the only goroutine allowed to advance it.
	// A handler that read here would race the reader for the same bytes.

	// Built per command and never cached: it embeds the command's Context, so a
	// cached one would carry the first command's deadline and cancellation into
	// every later command on the connection.
	authCtx := h.authContext(ctx, exp)

	// newSize is offset+length, already shown not to wrap. It is a ceiling
	// rather than a truncation: the metadata layer applies the maximum of the
	// old and new sizes, so a write at a lower offset never shrinks the file.
	intent, err := h.metaFor(exp).PrepareWrite(authCtx, exp.Handle, req.Offset+uint64(req.Length))
	if err != nil {
		return errorReply(req.Cookie, mapError(err))
	}

	// The returned []block.ChunkRef is discarded: the canonical chunk
	// projection is produced at Flush time, not once per write
	// (pkg/block/engine/readwrite.go).
	writeCtx, cancel := common.WithRequestDeadline(ctx)
	defer cancel()
	if _, err := bs.WriteAt(writeCtx, string(intent.PayloadID), nil, data, req.Offset); err != nil {
		return errorReply(req.Cookie, mapError(err))
	}

	if _, err := h.metaFor(exp).CommitWrite(authCtx, intent); err != nil {
		return errorReply(req.Cookie, mapError(err))
	}

	// FUA must be durable before the reply: the spec says the server "MUST NOT
	// send its reply to that command until all write operations ... have been
	// completed and persisted to non-volatile storage". The commit runs before
	// the reply is written, never after it, so a client that has seen the reply
	// has already had the guarantee it asked for.
	if req.Flags&FlagFUA != 0 {
		if err := h.commit(ctx, bs, exp); err != nil {
			return errorReply(req.Cookie, mapError(err))
		}
	}

	return reply{cookie: req.Cookie}
}

// validateWrite runs every check that decides whether a WRITE is served at all,
// and returns (reply, true) when it is refused.
//
// It is split from handleWrite because the payload read has to happen in the
// dispatcher, and the dispatcher needs to know whether to read a payload before
// it does. A refused WRITE's payload is still in flight — "The client MUST
// follow the request header with length number of bytes" holds whatever the
// server answers — so the caller drains req.Length bytes after writing the
// refusal. For the zero-length case that drain is a no-op, which is why the
// single "refused ⇒ drain" rule covers every branch here.
//
// The signature carries neither ctx nor bs, unlike the handlers: nothing in this
// function reaches a store or a context, and threading them in would be two
// parameters kept alive for the sake of matching a shape.
func (h *Handler) validateWrite(exp *Export, req *Request) (reply, bool) {
	// A flag that is not documented for WRITE is a malformed request. FUA is
	// the exception: it is valid on every command once SEND_FUA is advertised,
	// and on WRITE it is honoured rather than ignored.
	if req.Flags&^FlagFUA != 0 {
		return errorReply(req.Cookie, ErrEINVAL), true
	}

	// Checked before the length and the range: a read-only export refuses the
	// write wherever it lands.
	if exp.ReadOnly {
		return errorReply(req.Cookie, ErrEPERM), true
	}

	// A zero-length write carries no payload, so the drain that follows a
	// refusal consumes nothing: drainWritePayload returns nil for length 0.
	if req.Length == 0 {
		return errorReply(req.Cookie, ErrEINVAL), true
	}

	// A subtraction rather than offset+length, which wraps: a request far past
	// the end would otherwise look like it starts inside the export.
	if req.Offset > math.MaxUint64-uint64(req.Length) {
		return errorReply(req.Cookie, ErrEIO), true
	}

	// A write past the end is NBD_ENOSPC, not NBD_EINVAL: the spec reserves
	// EINVAL for a read or trim past the end, because a write past the end
	// means the client's view of the device is too large while a read or trim
	// past the end is a malformed request. NBD has no command that grows an
	// export (NBD_CMD_RESIZE exists only in an experimental extension no
	// mainline client implements, see protocol.go), so a write never grows it —
	// the size it is checked against can be up to one sizeRefresh stale, which
	// is the bound Export.Size's doc states.
	if req.Offset+uint64(req.Length) > exp.Size {
		return errorReply(req.Cookie, ErrENOSPC), true
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
	// header with length number of bytes"), so the caller drains it: replying
	// and returning without draining would parse payload bytes as the next
	// header. That is why validateWrite returns a bool rather than a bare reply.
	if payloadTooLarge(req.Length) {
		return errorReply(req.Cookie, ErrEINVAL), true
	}

	return reply{}, false
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
func (h *Handler) handleTrim(ctx context.Context, bs BlockStore, exp *Export, req *Request) reply {
	return h.handleDiscard(ctx, bs, exp, req, FlagFUA, ErrEINVAL)
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
func (h *Handler) handleWriteZeroes(ctx context.Context, bs BlockStore, exp *Export, req *Request) reply {
	return h.handleDiscard(ctx, bs, exp, req, FlagFUA|FlagNoHole, ErrENOSPC)
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
func (h *Handler) handleDiscard(ctx context.Context, bs BlockStore, exp *Export, req *Request, allowed CommandFlags, outOfRange NBDError) reply {
	// A flag that is not documented for this command is a malformed request.
	// FUA is accepted on both, because the spec requires the server to accept it
	// on every command once SEND_FUA is advertised. NO_HOLE is accepted on
	// WRITE_ZEROES only.
	if req.Flags&^allowed != 0 {
		return errorReply(req.Cookie, ErrEINVAL)
	}

	// Checked before the length and the range: a read-only export refuses the
	// mutation wherever it lands. The export still advertises SEND_TRIM and
	// SEND_WRITE_ZEROES while read-only (handshake.go sets both unconditionally
	// and only adds READ_ONLY), so this is not a client violating the
	// advertisement — it is the case the spec's error list names explicitly: "it
	// SHOULD return NBD_EPERM if it receives a write or trim request on a
	// read-only export".
	if exp.ReadOnly {
		return errorReply(req.Cookie, ErrEPERM)
	}

	// Neither command carries a payload, so a rejection is answered without a
	// drain: there are no bytes in flight to consume, and draining here would
	// read the next request header as payload.
	if req.Length == 0 {
		return errorReply(req.Cookie, ErrEINVAL)
	}

	// A subtraction rather than offset+length, which wraps: a request far past
	// the end would otherwise look like it starts inside the export. The same
	// guard handleRead uses, for the same reason.
	if req.Offset > math.MaxUint64-uint64(req.Length) {
		return errorReply(req.Cookie, ErrEIO)
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
		return errorReply(req.Cookie, outOfRange)
	}

	// The last byte of the export is addressable, so a range that ends exactly
	// at the end is served.
	if req.Offset+uint64(req.Length) > exp.Size {
		return errorReply(req.Cookie, outOfRange)
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
	res, err := h.metaFor(exp).PunchHole(authCtx, exp.Handle, req.Offset, uint64(req.Length))
	if err != nil {
		return errorReply(req.Cookie, mapError(err))
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
		return errorReply(req.Cookie, mapError(err))
	}

	// FUA must be durable before the reply, exactly as WRITE does: the spec says
	// the server "MUST NOT send its reply to that command until all write
	// operations ... have been completed and persisted to non-volatile storage".
	if req.Flags&FlagFUA != 0 {
		if err := h.commit(ctx, bs, exp); err != nil {
			return errorReply(req.Cookie, mapError(err))
		}
	}

	// Neither command has a reply payload.
	return reply{cookie: req.Cookie}
}
