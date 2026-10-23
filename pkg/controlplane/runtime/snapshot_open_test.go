package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// TestOpenSnapshotRegistry_Refcount covers the registry's own arithmetic before
// it is wired to anything: nesting, idempotent release, and a count that never
// goes negative. A negative count is the failure that matters — it would make a
// later delete succeed while a real holder is still reading.
func TestOpenSnapshotRegistry_Refcount(t *testing.T) {
	var reg openSnapshotRegistry

	rel1 := reg.open("alpha", "s1")
	rel2 := reg.open("alpha", "s1")
	if got := reg.holders("alpha", "s1"); got != 2 {
		t.Fatalf("holders after two opens = %d, want 2", got)
	}

	// Releasing twice must count once. A caller that defers release and also
	// calls it on an error path is the normal shape.
	rel1()
	rel1()
	if got := reg.holders("alpha", "s1"); got != 1 {
		t.Fatalf("holders after double release = %d, want 1", got)
	}

	rel2()
	if got := reg.holders("alpha", "s1"); got != 0 {
		t.Fatalf("holders after releasing all = %d, want 0", got)
	}
	// Released keys are deleted, not stored as 0, so the map does not grow one
	// entry per snapshot ever opened.
	if len(reg.refs) != 0 {
		t.Fatalf("registry retains %d entries after full release, want 0", len(reg.refs))
	}
}

// TestOpenSnapshotRegistry_ScopedToTheOneSnapshot asserts the registry keys on
// {share, snapshot} and not on either alone: an open snapshot must not refuse a
// delete of a different snapshot on the same share (retention pruning depends on
// it), and the same snapshot ID on another share must not collide.
func TestOpenSnapshotRegistry_ScopedToTheOneSnapshot(t *testing.T) {
	var reg openSnapshotRegistry
	defer reg.open("alpha", "s1")()

	if got := reg.holders("alpha", "s2"); got != 0 {
		t.Errorf("holders(alpha, s2) = %d, want 0: opening s1 must not affect s2", got)
	}
	if got := reg.holders("beta", "s1"); got != 0 {
		t.Errorf("holders(beta, s1) = %d, want 0: a share's snapshot ID must not collide across shares", got)
	}
	if got := reg.holders("alpha", "s1"); got != 1 {
		t.Errorf("holders(alpha, s1) = %d, want 1", got)
	}
}

// TestRuntimeDeleteSnapshot_RefusesWhileOpen asserts DeleteSnapshot refuses with
// ErrSnapshotOpen while an export holds the snapshot, and succeeds once the
// export releases it.
//
// The refusal must be a REFUSAL, not a wait: a boot reads a snapshot for minutes,
// so a delete that blocked would stall retention pruning for that whole time. The
// test therefore bounds the call with its own context rather than relying on the
// runtime to return promptly, so a blocking implementation shows up as a
// deadline failure instead of a hang.
func TestRuntimeDeleteSnapshot_RefusesWhileOpen(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
	defer cancel()

	const shareName = "alpha"
	localStoreDir := t.TempDir()
	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: shareName})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, localStoreDir); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}

	snap := &models.Snapshot{ShareName: shareName, State: models.StateCreating, MetadataEngine: "memory"}
	snapID, err := rt.store.CreateSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	release := rt.OpenSnapshot(shareName, snapID)
	if got := rt.SnapshotOpenHolders(shareName, snapID); got != 1 {
		t.Fatalf("SnapshotOpenHolders = %d, want 1", got)
	}

	derr := rt.DeleteSnapshot(ctx, shareName, snapID)
	if !errors.Is(derr, models.ErrSnapshotOpen) {
		t.Fatalf("DeleteSnapshot while open = %v, want errors.Is(ErrSnapshotOpen)", derr)
	}
	// The message must name the holder count, so an operator reading a 409 can
	// tell an export holding it from a stale registration.
	if !strings.Contains(derr.Error(), "held open by 1") {
		t.Errorf("refusal message %q does not report the holder count", derr.Error())
	}
	// The row must survive: a refused delete that still deleted would be the
	// worst outcome, since the client is still reading it.
	if _, gerr := rt.store.GetSnapshot(ctx, shareName, snapID); gerr != nil {
		t.Fatalf("row deleted despite open refusal: %v", gerr)
	}

	// A second snapshot on the same share is unaffected — the case retention
	// pruning actually exercises. It must be StateReady: the store's unique
	// index allows only one `creating` row per share.
	other := &models.Snapshot{ShareName: shareName, State: models.StateReady, MetadataEngine: "memory"}
	otherID, err := rt.store.CreateSnapshot(ctx, other)
	if err != nil {
		t.Fatalf("CreateSnapshot (other): %v", err)
	}
	if derr := rt.DeleteSnapshot(ctx, shareName, otherID); derr != nil {
		t.Fatalf("DeleteSnapshot of a different snapshot while one is open = %v, want nil", derr)
	}

	// After release the delete goes through.
	release()
	if derr := rt.DeleteSnapshot(ctx, shareName, snapID); derr != nil {
		t.Fatalf("DeleteSnapshot after release = %v, want nil", derr)
	}
}

// TestRuntimeDeleteSnapshot_OpenRefusalDoesNotBlock asserts the delete returns
// immediately rather than waiting for the holder, which is the whole reason the
// fence is a registry and not a shared lock. A regression to the lock design
// would block here until the context expired.
func TestRuntimeDeleteSnapshot_OpenRefusalDoesNotBlock(t *testing.T) {
	rt := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), snapTestTimeout)
	defer cancel()

	const shareName = "alpha"
	rt.sharesSvc.InjectShareForTesting(&shares.Share{Name: shareName})
	if err := rt.sharesSvc.SetLocalStoreDirForTesting(shareName, t.TempDir()); err != nil {
		t.Fatalf("SetLocalStoreDirForTesting: %v", err)
	}
	snapID, err := rt.store.CreateSnapshot(ctx, &models.Snapshot{
		ShareName: shareName, State: models.StateCreating, MetadataEngine: "memory",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	release := rt.OpenSnapshot(shareName, snapID)
	defer release()

	start := time.Now()
	if derr := rt.DeleteSnapshot(ctx, shareName, snapID); !errors.Is(derr, models.ErrSnapshotOpen) {
		t.Fatalf("DeleteSnapshot = %v, want ErrSnapshotOpen", derr)
	}
	// Generous bound: the point is "did not wait for the holder", not latency.
	// A lock-based implementation would sit here until snapTestTimeout.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("DeleteSnapshot took %v while a holder was open; it must refuse, not wait", elapsed)
	}
}
