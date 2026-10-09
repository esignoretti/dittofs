package nbd_test

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/testruntime"
	"github.com/marmos91/dittofs/pkg/adapter/nbd"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func minimalConfig() nbd.Config {
	return nbd.Config{Enabled: true, Port: 11089, BindAddress: "127.0.0.1"}
}

func TestNew_AppliesDefaults(t *testing.T) {
	a := nbd.New(nbd.Config{})
	if a.Port() != nbd.DefaultPort {
		t.Fatalf("Port() = %d, want %d", a.Port(), nbd.DefaultPort)
	}
	if a.Protocol() != "nbd" {
		t.Fatalf("Protocol() = %q, want %q", a.Protocol(), "nbd")
	}
}

func TestNew_PanicsOnOutOfRangePort(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with port 70000 did not panic; the adapter constructor contract is to panic on programmer error")
		}
	}()
	nbd.New(nbd.Config{Enabled: true, Port: 70000})
}

// The bind gate. An NBD export is unauthenticated by construction: there is no
// AUTH_SYS, no Kerberos, and a single READ hands over every byte with no path
// scoping. A non-loopback bind must therefore be an explicit opt-in.
//
// The empty string is in the list deliberately: IsNonLoopbackHost("") is true
// (net.ParseIP("") is nil and the final branch assumes a hostname resolves
// off-host), which is the behaviour the gate wants — an empty BindAddress binds
// every interface. Do not add a special case for it.
func TestServe_RefusesNonLoopbackBindWithoutOptIn(t *testing.T) {
	for _, bind := range []string{"", "0.0.0.0", "::", "192.168.1.10"} {
		t.Run(bind, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.BindAddress = bind
			a := nbd.New(cfg)
			err := a.Serve(context.Background())
			if err == nil {
				t.Fatal("Serve on a non-loopback address without AllowRemoteBind returned nil")
			}
			if !strings.Contains(err.Error(), "allow_remote_bind") {
				t.Fatalf("error %q does not name allow_remote_bind, so an operator cannot find the setting", err)
			}
		})
	}
}

// The gate runs before the listener is created and before the runtime is
// consulted, so this test needs neither a free port nor a fixture.
func TestServe_RefusesRemoteBindEvenWithNoRuntime(t *testing.T) {
	cfg := minimalConfig()
	cfg.BindAddress = "0.0.0.0"
	a := nbd.New(cfg)
	if err := a.Serve(context.Background()); err == nil {
		t.Fatal("the bind gate must refuse before Serve touches the runtime")
	} else if strings.Contains(err.Error(), "SetRuntime") {
		t.Fatalf("the runtime check ran before the bind gate: %v", err)
	}
}

// Serve needs a runtime once it gets past the gate, so the listener tests use
// the real fixture. This also exercises SetRuntime, which nothing else does.
func serveWithRuntime(t *testing.T, cfg nbd.Config) (*nbd.Adapter, chan error) {
	t.Helper()
	rt, _ := testruntime.NewWithShareStore(t)
	a := nbd.New(cfg)
	a.SetRuntime(rt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after context cancellation")
		}
	})
	return a, done
}

func TestServe_AllowsLoopbackBind(t *testing.T) {
	// "::1" is deliberately absent and "[::1]" stands in for it. BaseAdapter
	// builds its listen address as fmt.Sprintf("%s:%d", BindAddress, Port)
	// (pkg/adapter/base.go:216), so a BARE IPv6 literal yields "::1:0" and
	// net.Listen rejects it with "too many colons in address"; the bracketed
	// form is what binds. That is a property of the shared base adapter, not
	// something this task changes, so the test does not assert a capability the
	// code lacks. Both spellings pass netutil.IsNonLoopbackHost as loopback, so
	// the gate admits either — only the listen address differs.
	for _, bind := range []string{"127.0.0.1", "[::1]", "localhost"} {
		t.Run(bind, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.BindAddress = bind
			cfg.Port = 0 // ephemeral: the test must not need a fixed free port
			a, done := serveWithRuntime(t, cfg)
			select {
			case <-a.ListenerReady():
			case err := <-done:
				t.Fatalf("Serve returned before the listener was ready: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("listener was never ready")
			}
			// GetListenerAddr, not Port. Port() returns the CONFIGURED port
			// (pkg/adapter/base.go:598 is "return b.Config.Port") and nothing ever
			// writes the bound port back into Config, so Port() is legitimately 0
			// whenever the test asked for an ephemeral one. GetListenerAddr reports
			// the socket's real address, and it blocks on listenerReady too.
			if a.GetListenerAddr() == "" {
				t.Fatal("GetListenerAddr is empty after ListenerReady; the adapter did not bind a listener")
			}
		})
	}
}

func TestServe_AllowsNonLoopbackWithOptIn(t *testing.T) {
	cfg := minimalConfig()
	cfg.BindAddress = "0.0.0.0"
	cfg.AllowRemoteBind = true
	cfg.Port = 0
	a, done := serveWithRuntime(t, cfg)
	select {
	case <-a.ListenerReady():
	case err := <-done:
		t.Fatalf("Serve with AllowRemoteBind returned early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("listener was never ready")
	}
}

func TestStop_IsIdempotent(t *testing.T) {
	a := nbd.New(minimalConfig())
	ctx := context.Background()
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// The adapter must not treat a per-connection failure as fatal: Serve returning
// before cancellation makes DittoServer stop every other adapter.
//
// The runtime comes from serveWithRuntime rather than being wired inline:
// Serve refuses to bind without one, so a listener test cannot skip it. Using
// the shared helper keeps every listener test on one construction path.
func TestServe_ClientDisconnectDoesNotStopTheAdapter(t *testing.T) {
	cfg := minimalConfig()
	cfg.Port = 0
	a, done := serveWithRuntime(t, cfg)
	select {
	case <-a.ListenerReady():
	case <-time.After(5 * time.Second):
		t.Fatal("listener was never ready")
	}

	// Dial the socket's real address rather than the configured port: this test
	// asks for an ephemeral port, and Port() reports the configured value.
	addr := a.GetListenerAddr()
	for range 3 {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		// Read the greeting, then hang up mid-handshake.
		buf := make([]byte, 18)
		_, _ = io.ReadFull(conn, buf)
		_ = conn.Close()
	}

	select {
	case err := <-done:
		t.Fatalf("Serve returned after client disconnects: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestControlPlane_DefaultPortKnowsNBD(t *testing.T) {
	if got := models.DefaultPort("nbd"); got != nbd.DefaultPort {
		t.Fatalf("models.DefaultPort(\"nbd\") = %d, want %d", got, nbd.DefaultPort)
	}
	if err := (&models.AdapterConfig{Type: "nbd", Port: 10809}).Validate(); err != nil {
		t.Fatalf("Validate rejected an nbd adapter: %v", err)
	}
}
