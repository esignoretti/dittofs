package engine

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// HealthCheck verifies the store is operational by checking the syncer health
// (which in turn checks the remote store).
//
// Deprecated: use [Store.Healthcheck] (lowercase 'c'), which returns a
// structured [health.Report] derived from both the local and remote stores
// and satisfies [health.Checker]. This method only collapses the structured
// state into a single error and is retained for backward compatibility.
func (bs *Store) HealthCheck(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	return bs.syncer.HealthCheck(ctx)
}

// Healthcheck returns the engine's overall health, computed as the
// worst-of of its underlying local and remote stores. The result
// satisfies [health.Checker] so the API layer can wrap the engine in
// a [health.CachedChecker] for /status routes.
//
// Derivation rules (worst-of)
//
//   - If the local store reports unhealthy → engine is unhealthy
//     (we can't even serve cached blocks).
//   - If a remote store is configured and reports unhealthy → engine
//     is degraded (local reads still work, but new uploads will queue
//     and the system is operating in offline-write mode).
//   - Otherwise → healthy.
//
// The combined message preserves the worst-status component's message
// so operators can see exactly which subsystem is at fault.
func (bs *Store) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()

	// Pin against Close teardown. This method has no error return, so a
	// closed store reports unhealthy rather than racing the local/remote
	// teardown that Close performs under closeMu.Lock.
	bs.closeMu.RLock()
	defer bs.closeMu.RUnlock()
	if bs.closed {
		return health.NewUnhealthyReport(ErrStoreClosed.Error(), time.Since(start))
	}

	if err := ctx.Err(); err != nil {
		return health.NewUnknownReport(err.Error(), time.Since(start))
	}

	// The local tier answers a bool, not a report: closed is the only failure
	// mode it has that costs no IO to observe, and the engine is the component
	// that knows enough (local AND remote) to shape a report at all.
	if bs.local.Closed() {
		return health.NewUnhealthyReport("local: block store is closed", time.Since(start))
	}

	if bs.remote != nil {
		remoteRep := bs.remote.Healthcheck(ctx)
		if remoteRep.Status == health.StatusUnhealthy {
			// Local works, remote is unreachable: degraded — reads
			// still served from local cache, writes will queue.
			return health.Report{
				Status:    health.StatusDegraded,
				Message:   "remote unreachable: " + remoteRep.Message,
				CheckedAt: time.Now().UTC(),
				LatencyMs: time.Since(start).Milliseconds(),
			}
		}
	}

	return health.NewHealthyReport(time.Since(start))
}

// HasRemoteStore returns true if this Store has a remote store configured.
//
// decision: this predicate reads as dead code — a share is refused unless it
// names a block store, so a running server's stores all have a remote. It is
// not dead, and its branches must not be collapsed to the true arm. Remote is
// nillable at this package's boundary (see BlockStoreConfig.Remote), and the
// branches decide integrity behaviour rather than a fast path: a hole
// reconciles only with a remote to hydrate from and otherwise reads as the
// zeros it is (readAtInternal), a warm read whose bytes fail their checksum
// heals from the remote and otherwise fails closed instead of returning
// zero-filled or corrupt bytes (healCorruptWarmRead), and a cold range is
// recorded only where something can hydrate it (SeedColdRefs). Forcing the
// true arm turns each of those into a silent wrong answer. Withdraw this
// only once Remote cannot be nil here — a constructor that refuses it — at
// which point the compiler, not a grep, shows the branches are unreachable.
func (bs *Store) HasRemoteStore() bool {
	return bs.remote != nil
}

// SetEvictionPinned pins the local store's bytes in place for a pin-retention
// share. It survives the health-driven SetEvictionEnabled calls the syncer and
// Start make, so it is the only correct way to express a retention pin.
func (bs *Store) SetEvictionPinned(pinned bool) {
	bs.local.SetEvictionPinned(pinned)
}

// SetEvictionAllowed lets a caller enable eviction on a store whose syncer
// cannot justify it itself, and returns whether eviction is now allowed.
//
// Every other caller should not use this. Start and the health callback set
// eviction from evictionPermitted, which on an ordinary share is
// syncer.CanEvict() — requiring BOTH a healthy remote AND a wired carver
// (carveActive), because on a normal share, evicting local bytes that were
// never uploaded loses them. This method exists for the one store shape where
// that requirement does not apply: a read-only shadow whose local tier holds
// only bytes hydrated from a remote that is the sole source of truth. There,
// everything evictable is remote-durable by construction, so a missing carver
// (a read-only store never writes, so it never carves) is not a reason to
// withhold eviction.
//
// The assertion is standing, not one-shot: it is remembered so the health
// callback keeps honouring it. A shadow that allowed eviction and then had its
// remote blip would otherwise have eviction switched off by the callback's
// CanEvict and never switched back on, because a read-only store's carver never
// activates — turning a transient outage into a store that can no longer evict
// and so dies with ErrLocalStoreFull on the next full local tier.
//
// It still refuses when the remote is unhealthy or absent, because those are
// the only conditions under which eviction would strand a byte: the health
// callback suspends eviction during an outage and this method's permission
// resumes it on recovery.
//
// The returned bool reports what was decided rather than what was asked, so a
// caller can log a refusal instead of assuming the store will evict.
func (bs *Store) SetEvictionAllowed(allowed bool) bool {
	bs.evictionAllowed.Store(allowed)
	canEvict := bs.evictionPermitted()
	bs.local.SetEvictionEnabled(canEvict)
	return canEvict
}

// evictionPermitted is the single decision every eviction gate goes through: a
// caller's standing assertion that the local tier's bytes are all
// remote-durable, or the syncer's own carve-and-health gate. Both require a
// remote to re-fetch an evicted byte from.
//
// Remote presence and health are read from the syncer, not from bs.remote: the
// syncer is what every other remote-dependent decision consults (readahead,
// the carve recompute), and a store can be built with a real remote on its
// syncer and a nil bs.remote — the retention-pin and cold-read fixtures do
// exactly that. Gating on bs.remote would refuse eviction on those stores,
// which is the failure this method exists to avoid.
func (bs *Store) evictionPermitted() bool {
	if !bs.syncer.hasRemote.Load() {
		// Nothing to re-fetch an evicted byte from, so no assertion can make
		// eviction safe.
		return false
	}
	if !bs.syncer.IsRemoteHealthy() {
		return false
	}
	return bs.evictionAllowed.Load() || bs.syncer.CanEvict()
}
