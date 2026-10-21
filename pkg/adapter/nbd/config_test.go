package nbd_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/adapter/nbd"
)

// Both tests go through New rather than through applyDefaults/validate: those
// are unexported and this file is package nbd_test, matching the package's two
// existing test files. New is also the real caller (adapter.go:55-60 runs
// applyDefaults then validate and panics on failure), so asserting through it
// tests the path an operator's config actually takes.

// TestNew_RefusesAnUncappedRemoteBind pins the one combination the adapter must
// not accept.
//
// NBD has no authentication and no per-path scoping: one connection returns
// every byte of the volume. MaxConnections: 0 used to mean unlimited, and
// pkg/adapter/base.go:159 only allocates the semaphore when the value is
// positive — so an unlimited, unauthenticated, remotely reachable adapter was
// one accept loop away from exhausting file descriptors.
//
// -1 stays available as the explicit spelling of "unlimited", because an
// operator may have a real reason and this is the one place to write it down.
// Pairing it with a reachable interface is the part that gets refused.
func TestNew_RefusesAnUncappedRemoteBind(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New accepted a remote bind with an explicitly unlimited connection count")
		}
		// The message has to name the setting, or an operator staring at a
		// startup panic has nothing to grep for.
		if msg := fmt.Sprint(r); !strings.Contains(msg, "max_connections") {
			t.Fatalf("panic %q does not name max_connections, so an operator cannot find the setting", msg)
		}
	}()
	nbd.New(nbd.Config{
		Enabled: true, Port: 11089, BindAddress: "127.0.0.1",
		AllowRemoteBind: true, MaxConnections: -1,
	})
}

// TestNew_RejectsAnyOtherNegativeCount pins that -1 is the only negative value
// with a meaning. -2 is a typo, and treating it as "unlimited" would give an
// operator who fat-fingered a limit the opposite of what they asked for.
func TestNew_RejectsAnyOtherNegativeCount(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New accepted max_connections: -2")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "max_connections") {
			t.Fatalf("panic %q does not name max_connections", msg)
		}
	}()
	nbd.New(nbd.Config{
		Enabled: true, Port: 11089, BindAddress: "127.0.0.1", MaxConnections: -2,
	})
}

// TestNew_ZeroMaxConnectionsDefaultsToACap pins that 0 is no longer "unlimited".
// An operator who wants unlimited must say -1, which is a decision written down
// rather than the accidental meaning of an omitted key.
func TestNew_ZeroMaxConnectionsDefaultsToACap(t *testing.T) {
	if nbd.DefaultMaxConnections <= 0 {
		t.Fatalf("DefaultMaxConnections = %d, want a positive cap", nbd.DefaultMaxConnections)
	}
	a := nbd.New(minimalConfig()) // MaxConnections left at 0
	if got := a.ConfigForTest().MaxConnections; got != nbd.DefaultMaxConnections {
		t.Fatalf("MaxConnections = %d, want DefaultMaxConnections (%d)", got, nbd.DefaultMaxConnections)
	}
}

// TestDefaultMaxConnectionsIsLowerThanNFS pins the deliberate divergence from
// pkg/adapter/nfs/adapter.go:46 (DefaultMaxConnections = 1024). An NBD connection
// holds a block-store handle and up to 32 MiB of read buffer where an NFS
// connection does not, so matching NFS's number would be the wrong kind of
// consistency. Recording it here means the next reader sees it was a decision.
func TestDefaultMaxConnectionsIsLowerThanNFS(t *testing.T) {
	const nfsDefaultMaxConnections = 1024
	if nbd.DefaultMaxConnections >= nfsDefaultMaxConnections {
		t.Fatalf("DefaultMaxConnections = %d; NBD should stay well below NFS's %d because each connection holds a payload buffer",
			nbd.DefaultMaxConnections, nfsDefaultMaxConnections)
	}
}

// TestConfig_UIDZeroIsExpressible pins that "run as root" can be configured.
//
// 0 doubled as "unset", so an operator who set uid: 0 silently got 65534 — the
// one value the field exists to allow or forbid was the one it could not
// express. The fix is a set-pointer rather than a sentinel: every uint32 is a
// legal UID, so no in-band value can mean "unset".
func TestConfig_UIDZeroIsExpressible(t *testing.T) {
	zero := uint32(0)
	a := nbd.New(nbd.Config{
		Enabled: true, Port: 11089, BindAddress: "127.0.0.1",
		UID: 0, UIDSet: &zero,
	})
	if got := a.ConfigForTest().UID; got != 0 {
		t.Fatalf("UID = %d, want 0", got)
	}
}

// TestConfig_GIDZeroIsExpressible is the same for GID, which had the same
// sentinel bug at config.go:127-130.
func TestConfig_GIDZeroIsExpressible(t *testing.T) {
	zero := uint32(0)
	a := nbd.New(nbd.Config{
		Enabled: true, Port: 11089, BindAddress: "127.0.0.1",
		GID: 0, GIDSet: &zero,
	})
	if got := a.ConfigForTest().GID; got != 0 {
		t.Fatalf("GID = %d, want 0", got)
	}
}

// TestConfig_UIDUnsetStillDefaults pins that the new pointer did not break the
// ordinary case: no uid key at all must still give the unprivileged default.
func TestConfig_UIDUnsetStillDefaults(t *testing.T) {
	a := nbd.New(nbd.Config{Enabled: true, Port: 11089, BindAddress: "127.0.0.1"})
	if got := a.ConfigForTest().UID; got != nbd.DefaultUID {
		t.Fatalf("UID = %d, want DefaultUID (%d)", got, nbd.DefaultUID)
	}
}

// TestConfig_UIDSetWinsOverTheBareField pins the precedence rule the two fields
// create: the *Set pointer is what the config file said, the bare field is the
// resolved value. An operator who sets only the bare UID to a non-zero value
// still gets it, because a non-zero bare value is unambiguous; a zero bare value
// with no pointer means "unset" and gets the default.
func TestConfig_UIDSetWinsOverTheBareField(t *testing.T) {
	a := nbd.New(nbd.Config{Enabled: true, Port: 11089, BindAddress: "127.0.0.1", UID: 1234})
	if got := a.ConfigForTest().UID; got != 1234 {
		t.Fatalf("UID = %d, want 1234", got)
	}

	five := uint32(5)
	b := nbd.New(nbd.Config{Enabled: true, Port: 11089, BindAddress: "127.0.0.1", UID: 1234, UIDSet: &five})
	if got := b.ConfigForTest().UID; got != 5 {
		t.Fatalf("UID = %d, want 5 (UIDSet is what the config file said)", got)
	}
}

// TestNew_ClampsAndDefaultsPipelining pins the two pipelining knobs.
//
// Both are per connection, so their product with MaxConnections is the
// adapter-wide memory ceiling — 64 connections x 64 MiB of payload budget is
// already 4 GiB, which is why MaxConnections defaults to 64 rather than NFS's
// 1024. Getting the clamp wrong is therefore a memory bug, not a tuning one.
func TestNew_ClampsAndDefaultsPipelining(t *testing.T) {
	cfg := minimalConfig()
	if got := nbd.New(cfg).ConfigForTest().MaxInFlight; got != nbd.DefaultMaxInFlight {
		t.Fatalf("MaxInFlight = %d, want DefaultMaxInFlight (%d)", got, nbd.DefaultMaxInFlight)
	}
	if got := nbd.New(cfg).ConfigForTest().MaxInFlightBytes; got != nbd.DefaultMaxInFlightBytes {
		t.Fatalf("MaxInFlightBytes = %d, want DefaultMaxInFlightBytes (%d)", got, nbd.DefaultMaxInFlightBytes)
	}

	// A value past the clamp is clamped, not refused: the operator asked for more
	// concurrency than the adapter will honour, and the resolved value is
	// observable through ConfigForTest.
	cfg.MaxInFlight = 10_000
	if got := nbd.New(cfg).ConfigForTest().MaxInFlight; got != nbd.MaxMaxInFlight {
		t.Fatalf("MaxInFlight = %d, want the clamp (%d)", got, nbd.MaxMaxInFlight)
	}

	// A negative value is a typo and panics, like every other invalid config.
	for _, bad := range []struct {
		name string
		set  func(*nbd.Config)
	}{
		{"negative max_in_flight", func(c *nbd.Config) { c.MaxInFlight = -1 }},
		{"negative max_in_flight_bytes", func(c *nbd.Config) { c.MaxInFlightBytes = -1 }},
	} {
		t.Run(bad.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("New accepted %s", bad.name)
				}
			}()
			c := minimalConfig()
			bad.set(&c)
			nbd.New(c)
		})
	}
}
