package nbd

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// Idle bounds the wait for the next request header: a connection that has
// completed its handshake and sends nothing, or stops part-way through a header,
// is held only that long. Read bounds finishing a request that has already
// started — the payload read in handleWrite — so it is armed afresh once a
// header has been parsed. Write bounds writing one reply. The zero value of any
// field means no bound, which is why Idle cannot be folded into Read: zero is a
// sentinel for "no idle timeout", not a duration.
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

	// sizeRefresh bounds how stale Export.Size can be.
	//
	// A volume is an ordinary regular file in a share, so its size is mutable
	// over NFS and SMB while an NBD connection is live — the handshake's
	// snapshot is not "fixed at volume creation" as the Resolve doc comment used
	// to claim. Per-command re-resolution would take a registry lock and decode
	// a handle on every 4 KiB read, which is the exact cost the handshake's
	// one-time resolve exists to avoid, so a bounded interval is the compromise.
	// The residual staleness is stated in Export.Size's own doc comment.
	//
	// Zero means "re-read before every command": it falls out of the
	// `time.Since(lastResolve) >= h.sizeRefresh` comparison with no extra
	// branch, and it is what makes the truncate test deterministic instead of a
	// race against the window.
	sizeRefresh time.Duration

	// maxInFlight is how many commands a connection may have in flight, and
	// maxInFlightBytes bounds the payload memory they may hold at once.
	//
	// Both are per connection, not per adapter: one client's backlog must not
	// starve another's, and the adapter's MaxConnections cap already bounds the
	// total.
	maxInFlight      int
	maxInFlightBytes int64

	// peakInFlightBytes records the highest payload memory this handler's
	// pipelines held at once, for the backpressure test. It is an atomic because
	// it is updated from the pipeline's reader while a test reads it after the
	// connection closes.
	peakInFlightBytes atomic.Int64
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

		sizeRefresh: time.Second,

		maxInFlight:      DefaultMaxInFlight,
		maxInFlightBytes: DefaultMaxInFlightBytes,
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
// metaFor returns the metadata service an export's handles belong to.
//
// Production resolves every export out of the one service the runtime holds, so
// this is h.meta for all of them. A snapshot export carries its own service over
// a replayed store, and using h.meta for it would resolve its handles against
// the live volume's metadata — the reads would return production's bytes while
// the connection believed it was serving a snapshot.
//
// The nil check is not defensive habit: tests build handlers with a nil service
// on purpose (newTestHandler passes nil for meta), and those tests exercise the
// store path without a metadata layer.
func (h *Handler) metaFor(exp *Export) *metadata.Service {
	if exp != nil && exp.Meta != nil {
		return exp.Meta
	}
	return h.meta
}

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
	// Bound the handshake before it starts. Nothing else in the path does:
	// ServeHandshake arms no deadline of its own, the base adapter's accept
	// loop sets none on the accepted socket (pkg/adapter/base.go), and the
	// dispatch loop's first arm is below, after the handshake has already
	// returned. Without this a client can connect, send nothing, and hold a
	// goroutine and a file descriptor indefinitely — before it has named an
	// export, so before any authorisation decision exists to fail.
	//
	// The budget is the Read timeout, not the Idle one: a handshake is work the
	// server is actively doing on the client's behalf, so it belongs with the
	// budget for finishing started work rather than the budget for waiting on an
	// idle client. It is the same choice NFS makes for its own handshake
	// (pkg/adapter/nfs/connection.go:214, tlsHandshakeTimeout).
	//
	// SetDeadline rather than SetReadDeadline: the greeting and the option
	// replies are server writes, so a client that stops reading has to be bounded
	// as well as one that stops writing. The dispatch loop below re-arms only the
	// read deadline, which is why this does not leak into it.
	if err := conn.SetDeadline(time.Now().Add(h.timeouts.Read)); err != nil {
		return err
	}

	exp, err := ServeHandshake(ctx, conn, h.exporter)
	if err != nil {
		// The handshake already told the client why, where the protocol has a
		// way to.
		return err
	}

	// A snapshot export acquired an open registration and two private stores, so
	// the connection that resolved it is what releases them. Deferred here rather
	// than in Serve so it runs for a connection that fails to resolve its store
	// too — the export was already opened by then, and a leaked registration
	// makes the snapshot undeletable for the process lifetime.
	if exp.closeFn != nil {
		defer func() {
			if cerr := exp.closeFn(); cerr != nil {
				logger.Warn("NBD snapshot export did not close cleanly",
					"export", exp.Name, "error", cerr)
			}
		}()
	}

	// Resolved once, not per command: the handle is fixed for the connection's
	// life, so a per-command resolve would decode a handle and take a lock on
	// every 4 KiB read. An export that brought its own store (a snapshot) skips
	// resolution entirely: its store is private to it and already open.
	bs, err := h.storeFor(ctx, exp)
	if err != nil {
		// Not a wire error: the connection ends, because every subsequent
		// command would fail the same way. It is counted so a probe can report
		// the adapter as degraded.
		h.degraded.Add(1)
		return fmt.Errorf("nbd: resolving the block store for export %q: %w", exp.Name, err)
	}

	// The pipeline owns the socket from here: it reads, dispatches and writes,
	// and it is the only place that may do any of the three. maxInFlight: 1 makes
	// it a faithful reproduction of the serial loop this replaced, which is what
	// the existing suite runs against unchanged.
	//
	// Task 11 raises that number. Nothing else about this call changes when it
	// does, because the three roles are already separated: a worker never touches
	// the connection, and the reader is the only goroutine that advances it.
	return newPipeline(h, conn, bs, exp, ctx, h.maxInFlight).run()
}

// storeFor returns the block store an export reads through.
//
// A volume has none of its own: its store belongs to the share, and
// common.ResolveForWrite finds it from the handle, so two connections to one
// export reach the same journal and the same flush locks (which is what
// NBD_FLAG_CAN_MULTI_CONN rests on). A snapshot carries its own, because its
// bytes must come from its replayed manifest over the shared remote and not from
// the volume the snapshot was taken of.
func (h *Handler) storeFor(ctx context.Context, exp *Export) (BlockStore, error) {
	if exp.store != nil {
		return exp.store, nil
	}
	return h.stores(ctx, exp)
}

// writeReply writes one simple reply, header then payload.
//
// It is now a wrapper over reply.write, not a second implementation: the loop's
// own refusals (a malformed command, a draining connection, an unknown type) are
// replies like any other, and two writers would be two places to get the write
// deadline and the header-before-payload order wrong.
func (h *Handler) writeReply(conn net.Conn, cookie uint64, nbdErr NBDError, payload []byte) error {
	return reply{cookie: cookie, nbdErr: nbdErr, payload: payload}.write(conn, h)
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
		case merrs.ErrPermissionDenied, merrs.ErrAccessDenied:
			// Both POSIX denials reach the wire as NBD_EPERM. They are distinct
			// in the metadata layer — ErrPermissionDenied is EPERM ("requires
			// ownership or root privileges") and ErrAccessDenied is EACCES ("the
			// permission bits do not allow it") — but NBD has one permission
			// error, and the client's action is the same either way. The
			// ErrAccessDenied arm is the one that matters in practice: the
			// metadata permission funnel returns it for every denial by POSIX
			// mode bits, so without it a write refused by file permissions
			// would reach the client as NBD_EIO — a hardware failure reported
			// for an authorization decision.
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

// readWritePayload reads exactly req.Length bytes of WRITE payload.
//
// It arms the read deadline first, and that is why this cannot be a bare
// io.ReadFull at the call site: the header read consumed the budget armed in the
// dispatch loop, and that budget is the Idle one when the operator has separated
// the two — so without a fresh arm the payload read inherits the wrong timer and
// Read bounds nothing at all.
func (h *Handler) readWritePayload(conn net.Conn, req *Request) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(h.timeouts.Read)); err != nil {
		return nil, err
	}

	// The payload is read into a buffer sized to the request. LimitReader bounds
	// the read to the declared length, so a client that sends more than it
	// declared cannot make the server consume the next request header as
	// payload.
	data := make([]byte, req.Length)
	if _, err := io.ReadFull(io.LimitReader(conn, int64(req.Length)), data); err != nil {
		// The client stopped mid-payload on a request the server had already
		// accepted, so the stream position is unknown: returning ends Serve,
		// which closes the connection. A success reply for bytes the server
		// never saw is the one answer that must not be sent.
		return nil, err
	}
	return data, nil
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
