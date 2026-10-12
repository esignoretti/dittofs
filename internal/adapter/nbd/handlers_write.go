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
	// is where a hostile or stalled client parks. io.CopyN rather than a buffer
	// of req.Length bytes, so a client that declares a gigabyte costs a bounded
	// buffer instead of an allocation.
	if err := conn.SetReadDeadline(time.Now().Add(h.timeouts.Read)); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, conn, int64(req.Length)); err != nil {
		// The stream position is unknown now, so the connection cannot be
		// reused for another command: returning ends Serve, which closes it.
		return err
	}
	// Drained: the next header is read from the right offset, so the loop
	// continues and the connection stays usable.
	return nil
}
