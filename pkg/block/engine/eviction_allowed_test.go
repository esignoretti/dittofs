package engine

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/journal/journaltest"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestStore_SetEvictionAllowed covers the one store shape that needs to enable
// eviction on its own authority: a read-only shadow whose local tier holds only
// bytes hydrated from a remote, so everything evictable is remote-durable by
// construction and a missing carver is not a reason to withhold eviction.
//
// The observable is journal.EvictResult.Held, not the byte count. A store whose
// gate is open but which has nothing sealed to reclaim also reports 0 bytes, so
// bytes alone cannot tell "eviction is off" from "eviction is on and found no
// candidate" — Held is the field that separates them, and it is what the
// admin-facing DrainLocalSynced reports to an operator.
func TestStore_SetEvictionAllowed(t *testing.T) {
	ctx := context.Background()

	t.Run("local-only store cannot allow eviction", func(t *testing.T) {
		// A local-only store has no remote, so every byte it holds is the only
		// copy. Eviction must stay off however the caller asks: allowing it
		// would let the local tier drop the sole copy of a range.
		bs := newTestEngine(t, 0, 0)
		if got := bs.SetEvictionAllowed(true); got {
			t.Fatal("SetEvictionAllowed(true) = true on a store with no remote, want false")
		}
		if res, err := bs.DrainLocalSynced(ctx); err != nil {
			t.Fatalf("DrainLocalSynced: %v", err)
		} else if !res.Held {
			t.Error("eviction gate is open on a local-only store: Held = false, want true")
		}
	})

	t.Run("remote-backed store without a carver is allowed on request", func(t *testing.T) {
		// This is the shadow's exact shape: a remote exists, the syncer cannot
		// justify eviction itself (carve is not wired, so CanEvict is false),
		// and the caller asserts that everything local came from that remote.
		bs, local := newRemoteBackedNoCarveEngine(t)

		// Baseline: with no caller assertion, eviction stays off. This is the
		// pre-change behaviour and it is what made a shadow hit
		// ErrLocalStoreFull mid-boot.
		if canEvict := bs.syncer.CanEvict(); canEvict {
			t.Fatal("fixture precondition: CanEvict = true, want false so the baseline is meaningful")
		}
		if res, err := bs.DrainLocalSynced(ctx); err != nil {
			t.Fatalf("DrainLocalSynced (baseline): %v", err)
		} else if !res.Held {
			t.Fatal("eviction gate is open before SetEvictionAllowed: Held = false, want true")
		}

		if got := bs.SetEvictionAllowed(true); !got {
			t.Fatal("SetEvictionAllowed(true) = false on a healthy remote-backed store, want true")
		}
		if res, err := bs.DrainLocalSynced(ctx); err != nil {
			t.Fatalf("DrainLocalSynced (after allowing): %v", err)
		} else if res.Held {
			t.Error("eviction gate is still held after SetEvictionAllowed(true): Held = true, want false")
		}

		// And it is reversible, because a caller that loses its authority over
		// the bytes (a shadow being torn down) must be able to put the gate back.
		if got := bs.SetEvictionAllowed(false); got {
			t.Fatal("SetEvictionAllowed(false) = true, want false")
		}
		if res, err := bs.DrainLocalSynced(ctx); err != nil {
			t.Fatalf("DrainLocalSynced (after revoking): %v", err)
		} else if !res.Held {
			t.Error("eviction gate is open after SetEvictionAllowed(false): Held = false, want true")
		}

		// The local tier is the thing that actually gates eviction, so assert on
		// it directly as well: the engine method must not be a no-op that only
		// returns the right bool. journal.Evict reports a held gate through
		// EvictResult.Held rather than an error, so an error here is a genuine
		// failure worth surfacing.
		if held, err := local.Evict(ctx, 1<<62); err != nil {
			t.Fatalf("local.Evict: %v", err)
		} else if !held.Held {
			t.Error("local.Evict on a revoked gate: Held = false, want true")
		}
	})

	t.Run("permission survives a health blip", func(t *testing.T) {
		// The reason this method exists at all. A read-only shadow's carver
		// never activates, so syncer.CanEvict() is false for its whole life. If
		// the health callback re-derived the gate from CanEvict alone, one
		// transient remote outage would switch eviction off and never switch it
		// back — turning a blip into a store that dies with ErrLocalStoreFull
		// when its local tier next fills. The permission must be standing, so
		// recovery restores eviction.
		bs, _ := newRemoteBackedNoCarveEngine(t)
		if got := bs.SetEvictionAllowed(true); !got {
			t.Fatal("SetEvictionAllowed(true) = false, want true")
		}

		// Drive the monitor's own transition sequence — the same three writes
		// and callback its loop performs on a threshold breach, so the path
		// under test is the production one (fireCallback → the store's callback
		// → evictionPermitted → SetEvictionEnabled), not a direct poke at the
		// gate.
		hm := bs.syncer.healthMonitor
		hm.healthy.Store(false)
		hm.unhealthySince.Store(time.Now().UnixNano())
		hm.fireCallback(false)

		if res, err := bs.DrainLocalSynced(ctx); err != nil {
			t.Fatalf("DrainLocalSynced (outage): %v", err)
		} else if !res.Held {
			t.Error("eviction stayed enabled during a remote outage: Held = false, want true — " +
				"an evicted byte could not be re-fetched from an unreachable remote")
		}

		hm.healthy.Store(true)
		hm.unhealthySince.Store(0)
		hm.fireCallback(true)

		if res, err := bs.DrainLocalSynced(ctx); err != nil {
			t.Fatalf("DrainLocalSynced (recovery): %v", err)
		} else if res.Held {
			t.Error("eviction did not resume after the remote recovered: Held = true, want false — " +
				"the standing permission was lost, so this store can no longer evict and will fail with ErrLocalStoreFull when full")
		}
	})
}

// newRemoteBackedNoCarveEngine builds the store shape a snapshot shadow has: a
// remote and a healthy syncer, but no carver wired, so CanEvict is false.
func newRemoteBackedNoCarveEngine(t *testing.T) (*Store, journal.LocalStore) {
	t.Helper()
	localStore := journaltest.New(t)
	fbs := newStubFileChunkStore()
	mem := remotememory.New()
	syncer := NewRemoteSync(localStore, mem, fbs, DefaultConfig())

	bs, err := New(BlockStoreConfig{
		Local:          localStore,
		Remote:         mem,
		RemoteSync:     syncer,
		FileChunkStore: fbs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs, localStore
}
