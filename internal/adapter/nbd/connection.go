package nbd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// Timeouts bounds the three waits one NBD connection has.
//
// Read bounds reading one request header or payload. Idle bounds a connection
// that has completed its handshake but sends nothing, so Read must be the
// shorter of the two or a stalled client is held open by the wrong timer. Write
// bounds writing one reply. The zero value of any field means no bound.
type Timeouts struct {
	Read  time.Duration
	Write time.Duration
	Idle  time.Duration
}

// BlockStore is the engine surface the wire handlers use.
//
// An interface rather than *engine.Store so the dispatch tests can drive
// failure modes the engine cannot be made to produce on demand. The durability
// accessors are here because the commit rule (common.CommitBlockStore) reads
// them, and that rule must be the same one NFS COMMIT and SMB Flush use.
// *engine.Store satisfies all seven methods.
type BlockStore interface {
	ReadAt(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error)
	WriteAt(ctx context.Context, payloadID string, currentBlocks []block.ChunkRef, data []byte, offset uint64) ([]block.ChunkRef, error)
	PunchHole(ctx context.Context, payloadID string, currentBlocks []block.ChunkRef, offset, length uint64) ([]block.ChunkRef, error)
	Flush(ctx context.Context, payloadID string) (*block.FlushResult, error)
	LocalDurable() bool
	RemoteDurable() bool
	RequireDurableCommit() bool
}

// StoreResolver returns the block store that holds an export's bytes.
//
// A function type rather than a method on Handler so the dispatch tests can
// inject a fake store. The production implementation wraps
// common.ResolveForWrite, which is why this package never imports
// pkg/controlplane/runtime.
type StoreResolver func(ctx context.Context, exp *Export) (BlockStore, error)

// Handler serves the NBD transmission phase: it resolves one export, resolves
// that export's block store, and then dispatches commands until the connection
// ends.
//
// It deliberately holds no *runtime.Runtime — RuntimeExporter owns that, and
// keeping the runtime out of the wire layer is what makes the dispatch tests
// possible without a control plane. It also holds no net.Conn, which is forced
// by the framework rather than chosen: adapter.ConnectionHandler.Serve takes
// only a context (pkg/adapter/base.go:38), so the socket is bound on Conn
// instead.
type Handler struct {
	exporter Exporter
	stores   StoreResolver
	meta     *metadata.Service
	id       Identity
	timeouts Timeouts

	// draining reports whether the adapter has begun shutting down. It is a
	// closure over the base adapter's Shutdown channel rather than a flag of its
	// own, so the two can never disagree.
	draining func() bool

	// degraded counts export and block-store resolutions that failed. A probe
	// consumes the count (see TakeDegraded), so it is "since the last probe"
	// rather than a running total.
	degraded atomic.Int64
}

// NewHandler returns a handler for one adapter's connections.
//
// draining is a closure over the base adapter's shutdown signal, passed as a
// function rather than as the base adapter itself so this package stays free of
// any pkg/adapter import.
func NewHandler(exporter Exporter, stores StoreResolver, meta *metadata.Service, id Identity, timeouts Timeouts, draining func() bool) *Handler {
	return &Handler{
		exporter: exporter,
		stores:   stores,
		meta:     meta,
		id:       id,
		timeouts: timeouts,
		draining: draining,
	}
}

// TakeDegraded returns how many export and block-store resolutions have failed
// since the previous call, and resets the count.
//
// Swap rather than Load followed by Store: two steps let a concurrent increment
// vanish between them.
func (h *Handler) TakeDegraded() int64 { return h.degraded.Swap(0) }

// authContext builds the metadata context one command runs under.
//
// It is built per command and never cached: it embeds the command's Context, so
// a cached one would carry the first command's deadline and cancellation into
// every later command on the connection. ShareReadOnly is the client-facing
// ceiling and is applied here, not during the handshake's export resolution,
// which deliberately resolves an export the operator already configured.
func (h *Handler) authContext(ctx context.Context, exp *Export) *metadata.AuthContext {
	uid, gid := h.id.UID, h.id.GID
	return &metadata.AuthContext{
		Context:       ctx,
		Identity:      &metadata.Identity{UID: &uid, GID: &gid},
		ShareReadOnly: exp.ReadOnly,
	}
}

// Conn is one accepted TCP connection.
//
// It exists because adapter.ConnectionHandler.Serve takes no net.Conn
// (pkg/adapter/base.go:38), so the socket has to be bound at construction. It
// holds no protocol state of its own: the handshake result and the resolved
// store live on the stack of serve, which is the only thing that can outlive
// them safely.
type Conn struct {
	handler *Handler
	conn    net.Conn
}

// NewConn returns the connection handler for one accepted socket.
func NewConn(h *Handler, conn net.Conn) *Conn { return &Conn{handler: h, conn: conn} }

// Serve runs one connection and recovers panics.
//
// The recover is not defensive habit: the base adapter's accept loop has none
// (grep recover pkg/adapter/base.go returns nothing) and it invokes this on a
// bare goroutine (pkg/adapter/base.go:373), so an unrecovered panic here takes
// down the whole DittoFS process — every NFS and SMB connection with it. Both
// sibling adapters recover for the same reason and in the same place: NFS at
// pkg/adapter/nfs/connection.go:420 (handleConnectionClose, deferred at :111)
// and SMB at pkg/adapter/smb/connection.go:474. Log at Error, not Debug: a
// panic is unexpected by definition, and it is the only trace of why the
// connection died.
func (c *Conn) Serve(ctx context.Context) {
	// The socket is closed here, after the recover defer below has logged a
	// panic, because nothing else in the path closes it: the base adapter's
	// accept loop only unregisters the connection, and its force-close runs only
	// when the drain deadline expires. Without this the server's end would sit
	// in CLOSE_WAIT until the finalizer ran. Both sibling adapters close in
	// their deferred connection-close handler for the same reason.
	defer func() { _ = c.conn.Close() }()
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Panic in NBD connection handler",
				"remote", c.conn.RemoteAddr(), "error", r, "stack", string(debug.Stack()))
		}
	}()
	if err := c.handler.serve(ctx, c.conn); err != nil && ctx.Err() == nil {
		logger.Debug("NBD connection ended", "remote", c.conn.RemoteAddr(), "error", err)
	}
}

// serve runs the handshake and then the transmission phase for one connection.
//
// It is unexported because it is the test entry point, not the framework's: the
// dispatch tests call it directly so they keep the error return and never need
// the Conn wrapper.
func (h *Handler) serve(ctx context.Context, conn net.Conn) error {
	exp, err := ServeHandshake(ctx, conn, h.exporter)
	if err != nil {
		// The handshake already told the client why, where the protocol has a
		// way to.
		return err
	}

	// Resolved once, not per command: the handle is fixed for the connection's
	// life, so a per-command resolve would decode a handle and take a lock on
	// every 4 KiB read.
	bs, err := h.stores(ctx, exp)
	if err != nil {
		// Not a wire error: the connection ends, because every subsequent
		// command would fail the same way. It is counted so a probe can report
		// the adapter as degraded.
		h.degraded.Add(1)
		return fmt.Errorf("nbd: resolving the block store for export %q: %w", exp.Name, err)
	}

	for {
		// The read deadline is armed before the read, and deliberately not
		// while draining. initiateShutdown sets a 100 ms deadline on every
		// active connection to unblock pending reads (pkg/adapter/base.go:447),
		// and a fresh SetReadDeadline replaces it — so arming the full request
		// budget here would hold the drain open for that whole budget. Leaving
		// the short deadline in place is what lets an idle connection close
		// inside the drain window.
		if !h.draining() {
			if err := conn.SetReadDeadline(time.Now().Add(h.timeouts.Read)); err != nil {
				return err
			}
		}

		req, err := ReadRequest(conn)
		if err != nil {
			return err
		}

		// DISC is handled before the draining check because it has no reply in
		// any state: "There is no reply to an NBD_CMD_DISC." Offset and length
		// MUST be zero; a nonzero one is a malformed request, answered rather
		// than acted on so the connection stays usable.
		if req.Type == CmdDisc {
			if req.Offset != 0 || req.Length != 0 {
				if err := h.writeReply(conn, req.Cookie, ErrEINVAL, nil); err != nil {
					return err
				}
				continue
			}
			return nil
		}

		if h.draining() {
			// The request arrived before the read unblocked, so answer it rather
			// than serve it: NBD_ESHUTDOWN is the protocol's "reconnect" signal,
			// which is what the kernel's dead-connection timeout waits for. The
			// connection is not closed here — closing on the first observation
			// makes the client's kernel error the device instead of pausing it,
			// and the base adapter's drain deadline is the backstop.
			if err := h.writeReply(conn, req.Cookie, ErrESHUTDOWN, nil); err != nil {
				return err
			}
			continue
		}

		switch req.Type {
		case CmdRead:
			if err := h.handleRead(ctx, bs, exp, req, conn); err != nil {
				return err
			}

		default:
			// Every command the adapter advertises but does not yet serve
			// (WRITE, FLUSH, TRIM, WRITE_ZEROES, CACHE, BLOCK_STATUS) and every
			// value the spec does not define. An unimplemented command is an
			// invalid command: NBD_ENOTSUP is reserved by the spec for
			// WRITE_ZEROES with FAST_ZERO, which the adapter does not advertise.
			if err := h.writeReply(conn, req.Cookie, ErrEINVAL, nil); err != nil {
				return err
			}
		}
	}
}

// writeReply writes one simple reply header, followed by the payload when there
// is one.
//
// The write deadline is armed here, before every reply write, so a client that
// stops reading cannot hold the connection past the configured budget.
func (h *Handler) writeReply(conn net.Conn, cookie uint64, nbdErr NBDError, payload []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(h.timeouts.Write)); err != nil {
		return err
	}
	if err := writeSimpleReply(conn, cookie, nbdErr); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := conn.Write(payload)
	return err
}

// mapError translates a block-store error to the transmission-phase value the
// client sees.
//
// It normalizes first, and that is not optional: the engine returns raw
// sentinels as well as *merrs.StoreError values, and normalization is what turns
// a raw one into a *merrs.StoreError with the right code while leaving an
// already-wrapped one alone. A classifier that skipped it would report NBD_EIO
// for a full disk, which is exactly the class of bug this mapping exists to
// prevent.
func mapError(err error) NBDError {
	if err == nil {
		return 0
	}
	normalized := common.NormalizeBlockStoreError(err)

	// The store failures are tested before the context pair, because a store
	// failure wrapped in a cancellation should report the store failure: the
	// operator needs to know the store is gone, not that a context expired.
	// errors.Is walks the whole wrap chain, so one error can match several
	// cases and the first match wins.
	switch {
	case errors.Is(normalized, engine.ErrStoreClosed):
		// The store is gone, so the client should error the device. NBD_EIO
		// rather than NBD_ESHUTDOWN: ESHUTDOWN means "reconnect", and a closed
		// store is not a restart.
		logger.Debug("nbd: block store closed under an in-flight operation", "error", err)
		return ErrEIO
	case errors.Is(normalized, common.ErrNotDurableYet):
		// FLUSH promised durability and it could not be delivered, so there is
		// no honest success to send.
		logger.Debug("nbd: flush did not reach a durable store", "error", err)
		return ErrEIO
	}

	var storeErr *merrs.StoreError
	if errors.As(normalized, &storeErr) {
		switch storeErr.Code {
		case merrs.ErrNoSpace:
			logger.Debug("nbd: block store is full", "error", err)
			return ErrENOSPC
		case merrs.ErrPermissionDenied:
			logger.Debug("nbd: block store denied the operation", "error", err)
			return ErrEPERM
		}
	}

	if errors.Is(normalized, context.Canceled) || errors.Is(normalized, context.DeadlineExceeded) {
		// Expected: the request budget expired or the adapter is shutting down.
		logger.Debug("nbd: command context ended", "error", err)
		return ErrESHUTDOWN
	}

	// Everything left is an opaque engine fault, which is unexpected by
	// definition, so it is the one class logged at Error.
	logger.Error("nbd: unmapped block store error", "error", err)
	return ErrEIO
}
