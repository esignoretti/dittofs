package nbd

import (
	"context"

	"github.com/marmos91/dittofs/internal/adapter/common"
)

// handleFlush serves one NBD_CMD_FLUSH.
//
// The spec requires offset and length to be zero on FLUSH, so a nonzero one is
// a malformed request and is answered with NBD_EINVAL. It is answered without a
// drain: the command's own definition says there is no payload, so a client
// that declared one has left the protocol rather than sent bytes the server
// could consume.
//
// A read-only export is not refused. Refusing would make a client error a
// device that has nothing to flush, and there is nothing to protect: the
// command carries no data.
func (h *Handler) handleFlush(ctx context.Context, bs BlockStore, exp *Export, req *Request) reply {
	// FUA is accepted because the spec requires the server to accept it on every
	// command once SEND_FUA is advertised; no other flag is documented for
	// FLUSH, so one is a malformed request. This is the same rule handleDiscard
	// applies (handlers_write.go:191) and it was missing here only because FLUSH
	// is the one command whose payload is nothing at all — there was no length
	// check to hang it on.
	//
	// Checked before the length/offset check so a request that is wrong in both
	// ways is still reported once, and before h.commit so a rejected FLUSH has
	// not flushed.
	if req.Flags&^FlagFUA != 0 {
		return errorReply(req.Cookie, ErrEINVAL)
	}
	if req.Length != 0 || req.Offset != 0 {
		return errorReply(req.Cookie, ErrEINVAL)
	}
	if err := h.commit(ctx, bs, exp); err != nil {
		return errorReply(req.Cookie, mapError(err))
	}
	return reply{cookie: req.Cookie}
}

// commit is the durability barrier a FLUSH and a FUA write both need.
//
// It is common.FlushStableWrite, the same function NFSv3/NFSv4 WRITE and
// COMMIT and SMB CLOSE/FLUSH call, so the durability rule — requireDurable →
// localDurable || (Finalized && remoteDurable), and ErrNotDurableYet otherwise
// — has exactly one implementation across the adapters instead of one per
// protocol. That rule has two halves and the barrier needs both: the block
// store's bytes and the file's pending metadata (size and mtime, which
// deferred commits hold back). Committing only the block-store half would leave
// a client told "durable" with metadata that a restart can lose. mapError turns
// ErrNotDurableYet into NBD_EIO: the command promised durability and could not
// deliver it, so there is no honest success to send.
//
// decision: the FLUSH barrier is correct without tracking which writes are
// outstanding because dispatch is serialized. A command is handled and replied
// before the next one is read, so every write whose reply the client has
// already seen was written and committed by the time this runs, and "all
// already-replied writes are durable" follows from the ordering rather than
// from bookkeeping. If the connection loop is ever changed to pipeline
// commands, that stops being true and this barrier needs explicit tracking of
// in-flight writes.
func (h *Handler) commit(ctx context.Context, bs BlockStore, exp *Export) error {
	return common.FlushStableWrite(h.authContext(ctx, exp), h.meta, bs, exp.Handle, exp.PayloadID, true)
}
