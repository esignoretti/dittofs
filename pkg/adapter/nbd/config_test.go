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
//
// The assertion is deliberately indirect, and the reason is worth recording: the
// adapter exposes no accessor for its resolved MaxConnections, so the observable
// property is that a remote bind with an omitted count is accepted while one with
// -1 is refused. Adding an accessor for this would be a production API added for
// a test. The pair is the test — this one proves the default is not treated as
// "unlimited", and TestNew_RefusesAnUncappedRemoteBind proves the refusal path
// exists at all.
func TestNew_ZeroMaxConnectionsDefaultsToACap(t *testing.T) {
	if nbd.DefaultMaxConnections <= 0 {
		t.Fatalf("DefaultMaxConnections = %d, want a positive cap", nbd.DefaultMaxConnections)
	}
	cfg := minimalConfig()
	cfg.AllowRemoteBind = true // MaxConnections left at 0
	if a := nbd.New(cfg); a == nil {
		t.Fatal("New returned nil for a remote bind with the default connection cap")
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
