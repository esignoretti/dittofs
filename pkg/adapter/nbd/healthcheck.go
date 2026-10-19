package nbd

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck reports the adapter's health.
//
//   - A disabled adapter reports [health.StatusDisabled]: it was turned off
//     deliberately, so there is nothing to probe.
//   - A serving adapter reports [health.StatusDegraded] when export or
//     block-store resolutions have failed since the previous probe. The count
//     is read-and-reset, so it describes the window since that probe rather
//     than the adapter's lifetime.
//   - Otherwise the base adapter's derivation applies: unknown before the
//     listener is bound, unhealthy once shutdown has been initiated and Serve
//     has not yet returned, healthy while running.
//
// The Degraded branch must come before the delegation, because the base adapter
// has no way to know about a counter it does not own.
func (a *Adapter) Healthcheck(ctx context.Context) health.Report {
	if !a.config.Enabled {
		return health.Report{
			Status:    health.StatusDisabled,
			Message:   "NBD adapter is disabled in config",
			CheckedAt: time.Now().UTC(),
		}
	}
	if h := a.handler.Load(); h != nil {
		if n := h.TakeDegraded(); n > 0 {
			return health.Report{
				Status:    health.StatusDegraded,
				Message:   fmt.Sprintf("NBD adapter is serving but %d export resolutions have failed", n),
				CheckedAt: time.Now().UTC(),
			}
		}
	}
	return a.BaseAdapter.Healthcheck(ctx)
}
