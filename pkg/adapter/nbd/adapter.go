package nbd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

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
func (a *Adapter) NewConnection(conn net.Conn) adapter.ConnectionHandler {
	return &nbdConn{conn: conn}
}

// nbdGreeting is the 18-byte server greeting: the NBDMAGIC and IHAVEOPT magics
// followed by the handshake flag word (fixed-newstyle | no-zeroes).
//
// The wire layer writes the same bytes in its own writeGreeting, which is
// unexported; a placeholder is not a reason to widen that package's API, so
// the bytes are restated here and go away with the placeholder.
var nbdGreeting = []byte{
	'N', 'B', 'D', 'M', 'A', 'G', 'I', 'C',
	'I', 'H', 'A', 'V', 'E', 'O', 'P', 'T',
	0x00, 0x03,
}

// nbdConn is a placeholder connection handler: it greets the client, drains
// whatever the client sends, and closes. It serves no commands.
//
// decision: this exists only so the adapter lifecycle is exercisable end to
// end before the wire layer is wired in — the accept path, the per-connection
// goroutine, and the shutdown drain are all shared with the real handler, and
// the real handler replaces this type without changing any of them.
type nbdConn struct {
	conn net.Conn
}

// Serve greets the client and then waits for it to hang up. The base adapter
// interrupts the blocking read on shutdown, so the drain does not outlive the
// adapter.
func (c *nbdConn) Serve(_ context.Context) {
	defer func() { _ = c.conn.Close() }()

	if _, err := c.conn.Write(nbdGreeting); err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, c.conn)
}
