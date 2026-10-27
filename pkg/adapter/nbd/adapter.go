package nbd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"github.com/marmos91/dittofs/internal/adapter/common"
	nbd "github.com/marmos91/dittofs/internal/adapter/nbd"
	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/netutil"
)

// Adapter serves NBD exports out of a DittoFS share. A guest's kernel NBD
// client attaches to the adapter's port, names an export of the form
// <share>/<volume>, and reads and writes that volume's bytes.
//
// It embeds [adapter.BaseAdapter] for the TCP lifecycle and adds the two
// things the base cannot know: the bind gate (NBD is unauthenticated, so a
// remotely reachable export is an explicit opt-in) and the configuration the
// wire layer is built from.
type Adapter struct {
	*adapter.BaseAdapter

	// config is the resolved configuration. New applies the defaults before
	// storing it, so every read here sees a complete config.
	config Config

	// runtime is the control-plane runtime that resolves export names to
	// shares. Set by SetRuntime before Serve, which refuses to start without
	// it.
	runtime *runtime.Runtime

	// handler is built once in Serve, before the listener exists, and read by
	// NewConnection and Healthcheck — both of which run on other goroutines.
	// An atomic pointer, not a plain field: Serve runs in its own goroutine
	// (pkg/controlplane/runtime/adapters/service.go:611) while Healthcheck is
	// reached from an HTTP handler
	// (internal/controlplane/api/handlers/adapters.go:240) via
	// Runtime.AdapterChecker, so a plain field is a data race the race detector
	// reports on any probe that lands during startup.
	handler atomic.Pointer[nbd.Handler]
}

// New returns an adapter for config with the defaults applied.
//
// It panics on an invalid configuration, matching the other adapters: the
// values come from the operator's config file and the control-plane row, and
// models.AdapterConfig.Validate rejects the same ranges before an adapter is
// built, so an invalid value reaching this constructor is a programming error
// rather than something a client can provoke.
func New(config Config) *Adapter {
	config.applyDefaults()
	if err := config.validate(); err != nil {
		panic(fmt.Sprintf("invalid NBD config: %v", err))
	}

	base := adapter.NewBaseAdapter(adapter.BaseConfig{
		BindAddress:     config.BindAddress,
		Port:            config.Port,
		MaxConnections:  config.MaxConnections,
		ShutdownTimeout: config.Timeouts.Shutdown,
	}, "nbd")

	return &Adapter{BaseAdapter: base, config: config}
}

// SetRuntime stores the control-plane runtime. It panics on any other type,
// matching [adapter.BaseAdapter.SetRuntime]'s documented contract.
//
// decision: the export resolver and the wire handler are built in Serve, not
// here. They are built from the runtime and the configuration together, and
// building them in Serve is what makes a bad configuration or a missing
// runtime a startup error rather than a failure on the first connection.
func (a *Adapter) SetRuntime(rt any) {
	r, ok := rt.(*runtime.Runtime)
	if !ok {
		panic(fmt.Sprintf("nbd: SetRuntime wants *runtime.Runtime, got %T", rt))
	}
	// The base adapter keeps its own reference for the shared accept loop,
	// which reaches it for connection metrics.
	a.BaseAdapter.SetRuntime(rt)
	a.runtime = r
}

// Serve binds the configured address and serves NBD connections until ctx is
// cancelled.
//
// decision: the remote-bind gate runs first — before the listener exists and
// before the runtime is consulted — so a refused bind opens no socket and
// reports the one setting the operator has to change. The order is part of the
// contract, not an accident of how the function was written: a runtime check
// that ran first would report a missing runtime for a bind that is going to be
// refused anyway.
func (a *Adapter) Serve(ctx context.Context) error {
	// An empty BindAddress is a non-loopback bind: it listens on every
	// interface. netutil.IsNonLoopbackHost reports true for it because the
	// value is not an IP literal, so it falls through to the branch that
	// assumes a non-IP host resolves off-host. That is the behaviour this gate
	// wants, so it needs no special case.
	if netutil.IsNonLoopbackHost(a.config.BindAddress) && !a.config.AllowRemoteBind {
		return fmt.Errorf("nbd: refusing to bind %q without allow_remote_bind: "+
			"NBD has no authentication and no per-path scoping, so a remotely "+
			"reachable export hands every byte of the volume to anyone who can "+
			"reach the port", a.config.BindAddress)
	}

	if a.runtime == nil {
		return errors.New("nbd: SetRuntime was not called before Serve")
	}

	// The wire handler is built once, before the listener exists, so a bad
	// configuration fails as a startup error rather than as a failed first
	// connection.
	id := nbd.Identity{UID: a.config.UID, GID: a.config.GID}
	exporter := nbd.NewRuntimeExporter(a.runtime, id)
	// The store resolver wraps common.ResolveForWrite rather than reimplementing
	// resolution, so the wire layer holds a function and never the runtime. It
	// returns *engine.Store as nbd.BlockStore implicitly.
	resolver := func(ctx context.Context, exp *nbd.Export) (nbd.BlockStore, error) {
		return common.ResolveForWrite(ctx, a.runtime, exp.Handle)
	}
	// draining reads the base adapter's shutdown signal rather than keeping a
	// flag of its own, so the two can never disagree. Shutdown is a promoted
	// field on the embedded base adapter, so the selector is redundant.
	draining := func() bool {
		select {
		case <-a.Shutdown:
			return true
		default:
			return false
		}
	}
	handler := nbd.NewHandler(exporter, resolver, a.runtime.GetMetadataService(),
		id, a.config.timeouts(), draining)
	// applyDefaults has already resolved both of these (4 and 64 MiB), so the
	// handler's own defaults would agree — but only by coincidence, and a
	// configured value would be silently ignored. This is the one place they are
	// connected.
	handler.SetPipelining(a.config.MaxInFlight, a.config.MaxInFlightBytes)

	// The runtime's metrics handle is nil-safe, but storing a nil *Metrics in
	// this interface would make it a non-nil interface holding a nil pointer, and
	// then dispatch's own nil check would pass and RecordRequest would be called
	// on a nil receiver. Guard on the handle, not on the interface.
	if m := a.runtime.Metrics(); m != nil {
		handler.SetMetrics(m)
	}

	a.handler.Store(handler)

	return a.ServeWithFactory(ctx, a, nil, nil)
}

// Stop initiates graceful shutdown and waits for in-flight connections to
// drain. It is safe to call more than once: the base adapter's shutdown is
// guarded by a sync.Once, so a repeat call only waits for the drain the first
// one started.
func (a *Adapter) Stop(ctx context.Context) error {
	return a.BaseAdapter.Stop(ctx)
}

// NewConnection returns the handler for one accepted TCP connection. It
// implements [adapter.ConnectionFactory].
//
// The socket is bound here rather than on the shared handler because
// [adapter.ConnectionHandler.Serve] takes only a context; the handler itself is
// shared and stateless, so the wrapper carries nothing but the socket and the
// handler pointer.
func (a *Adapter) NewConnection(conn net.Conn) adapter.ConnectionHandler {
	return nbd.NewConn(a.handler.Load(), conn)
}
