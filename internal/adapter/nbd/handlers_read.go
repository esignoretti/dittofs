package nbd

import (
	"context"
	"math"
	"net"

	"github.com/marmos91/dittofs/internal/adapter/common"
)

// handleRead serves one NBD_CMD_READ.
//
// The checks run in this order — overflow, zero length, past the end, flags —
// because a request that is both malformed and out of range must fail on the
// malformation: the range check reads the request's own fields, so it cannot be
// trusted until they have been shown to make sense.
func (h *Handler) handleRead(ctx context.Context, bs BlockStore, exp *Export, req *Request, conn net.Conn) error {
	// A client-supplied offset and length can overflow. The check is a
	// subtraction rather than `req.Offset+uint64(req.Length) > exp.Size`,
	// because that add wraps and lets a request that is far past the end look
	// like it starts inside the export.
	//
	// decision: the answer is NBD_EIO, not NBD_EOVERFLOW. The spec reserves
	// EOVERFLOW for READ with NBD_CMD_FLAG_DF and for a payload-less request
	// over an advertised maximum, and the adapter advertises neither. The
	// request is not out of range; it cannot be interpreted at all.
	if uint64(req.Length) > math.MaxUint64-req.Offset {
		if err := h.writeReply(conn, req.Cookie, ErrEIO, nil); err != nil {
			return err
		}
		return nil
	}

	// The spec declines to specify a zero-length read beyond "SHOULD NOT
	// disconnect". One uniform answer is chosen over a per-command table
	// because no correct client sends one and the connection stays usable.
	if req.Length == 0 {
		if err := h.writeReply(conn, req.Cookie, ErrEINVAL, nil); err != nil {
			return err
		}
		return nil
	}

	// The last byte of the device is addressable, so the range is out of bounds
	// only when offset+length exceeds the size. A read that ends exactly at the
	// end is served.
	if req.Offset+uint64(req.Length) > exp.Size {
		if err := h.writeReply(conn, req.Cookie, ErrEINVAL, nil); err != nil {
			return err
		}
		return nil
	}

	// Flags are validated before the store is touched, so a malformed request
	// never reaches it.
	//
	// FUA is accepted and ignored: the spec requires the server to accept it on
	// every command once SEND_FUA is advertised ("the server MUST accept all
	// commands with this bit set (even by ignoring the bit)") and notes that
	// existing clients set it on commands other than WRITE. A read writes
	// nothing, so there is nothing to persist. Every other flag is gated behind
	// an advertisement the adapter does not make.
	if req.Flags&^FlagFUA != 0 {
		if err := h.writeReply(conn, req.Cookie, ErrEINVAL, nil); err != nil {
			return err
		}
		return nil
	}

	// A length beyond the maximum advertised in the handshake is refused before
	// anything is allocated. The spec names this exact case as the one where
	// NBD_EOVERFLOW is correct: "when a command without payload requests a length
	// larger than an advertised maximum payload length". READ carries no payload
	// from the client, so it is that case. Without this check the only bound was
	// the export size, so a client could make the server allocate the whole
	// export in one request — measured: a 256 MiB READ against a 1 GiB export was
	// accepted despite a 32 MiB advertised maximum.
	if payloadTooLarge(req.Length) {
		if err := h.writeReply(conn, req.Cookie, ErrEOVERFLOW, nil); err != nil {
			return err
		}
		return nil
	}

	// Allocated only after the bounds checks: a client that sends
	// Length: 0xffffffff against a small export gets NBD_EINVAL without a
	// multi-gigabyte allocation.
	buf := make([]byte, req.Length)

	// The deadline wrapper is applied per command, not per connection: a
	// connection is long-lived, so a connection-wide budget would expire a boot
	// disk mid-boot. It is a no-op when the context already carries a deadline.
	readCtx, cancel := common.WithRequestDeadline(ctx)
	defer cancel()

	if _, err := bs.ReadAt(readCtx, string(exp.PayloadID), buf, req.Offset); err != nil {
		// No payload on the error path: the reply shape for a failed READ is the
		// header alone.
		return h.writeReply(conn, req.Cookie, mapError(err), nil)
	}

	return h.writeReply(conn, req.Cookie, 0, buf)
}
