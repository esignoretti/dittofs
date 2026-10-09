package nbd_test

import (
	"context"
	"testing"

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
