package nbd_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/adapter/nbd"
	"github.com/marmos91/dittofs/pkg/health"
)

func TestNBDAdapter_Healthcheck_DisabledWhenConfigOff(t *testing.T) {
	cfg := minimalConfig()
	cfg.Enabled = false
	a := nbd.New(cfg)

	rep := a.Healthcheck(context.Background())
	if rep.Status != health.StatusDisabled {
		t.Fatalf("disabled config: got %q (%q), want disabled", rep.Status, rep.Message)
	}
	if rep.Message == "" {
		t.Fatal("expected non-empty message describing the disabled state")
	}
}

func TestNBDAdapter_Healthcheck_UnknownBeforeStart(t *testing.T) {
	a := nbd.New(minimalConfig())

	rep := a.Healthcheck(context.Background())
	// The base adapter has not been started, so the override delegates and
	// returns unknown.
	if rep.Status != health.StatusUnknown {
		t.Fatalf("enabled but not started: got %q (%q), want unknown", rep.Status, rep.Message)
	}
}

// The Degraded branch needs the handler to exist, so this test starts Serve. It
// does not need a failed resolution: removing the share after the handshake is
// not reachable from here (see connection_test.go's note), so the count is
// produced by the only external lever there is — resolving an export that does
// not exist, which is a handshake failure rather than a store failure and does
// NOT increment. That is the point: this test pins the OBSERVABLE contract,
// that a probe with a zero count reports healthy, and leaves the increment to
// the internal test.
func TestNBDAdapter_Healthcheck_NotDegradedWithoutFailures(t *testing.T) {
	cfg := minimalConfig()
	cfg.Port = freePort(t)
	a, done := serveWithRuntime(t, cfg)
	select {
	case <-a.ListenerReady():
	case err := <-done:
		t.Fatalf("Serve returned before the listener was ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("listener was never ready")
	}

	// A probe must not invent degradation, and TakeDegraded on a fresh handler
	// must not block or panic.
	for i := range 2 {
		rep := a.Healthcheck(context.Background())
		if rep.Status == health.StatusDegraded {
			t.Fatalf("probe %d reported degraded with no failures: %q", i+1, rep.Message)
		}
	}
}
