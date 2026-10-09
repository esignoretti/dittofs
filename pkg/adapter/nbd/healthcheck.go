package nbd

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck reports the adapter's health.
//
//   - A disabled adapter reports [health.StatusDisabled]: it was turned off
//     deliberately, so there is nothing to probe.
//   - Otherwise the base adapter's derivation applies: unknown before the
//     listener is bound, unhealthy once shutdown has been initiated and Serve
//     has not yet returned, healthy while running.
//
// No error instrumentation is tracked yet, so a running NBD adapter always
// reports healthy; there is nothing that can degrade until export resolution
// starts counting its failures.
func (a *Adapter) Healthcheck(ctx context.Context) health.Report {
	if !a.config.Enabled {
		return health.Report{
			Status:    health.StatusDisabled,
			Message:   "NBD adapter is disabled in config",
			CheckedAt: time.Now().UTC(),
		}
	}
	return a.BaseAdapter.Healthcheck(ctx)
}
