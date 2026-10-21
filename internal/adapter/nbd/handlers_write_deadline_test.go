package nbd

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// deadlineRecordingStore records whether PunchHole's context carried a deadline.
// It embeds storeFake so only the one method under test is overridden — and so
// the embedded pointer's counters (punches, flushes) stay observable.
//
// The pointer matters. newStoreFake() returns *storeFake, so embedding the value
// would give this store its own zero-valued counters and the punches assertion
// below would read 0 no matter what the handler did.
type deadlineRecordingStore struct {
	*storeFake
	punchHoleHadDeadline bool
}

func (s *deadlineRecordingStore) PunchHole(ctx context.Context, payloadID string, currentBlocks []block.ChunkRef, offset, length uint64) ([]block.ChunkRef, error) {
	_, s.punchHoleHadDeadline = ctx.Deadline()
	return s.storeFake.PunchHole(ctx, payloadID, currentBlocks, offset, length)
}

// TestHandleDiscard_PunchHoleCarriesRequestDeadline pins the one block-store
// call in this package that had no deadline. A discard that reaches a remote
// tier and stalls would otherwise hold the connection's goroutine for as long as
// the tier takes, because the command's own context carries no budget.
//
// Every other block-store call already wraps its context: handlers_read.go:93
// and handlers_write.go:120 both use common.WithRequestDeadline. This one was
// missed because handleDiscard's metadata call above it *is* bounded — by the
// metadata service's own timeout — which made the function look covered.
//
// It needs the real-metadata fixture: handleDiscard calls h.meta.PunchHole
// first, and newTestHandler passes nil for meta.
func TestHandleDiscard_PunchHoleCarriesRequestDeadline(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := &deadlineRecordingStore{storeFake: newStoreFake()}
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	// WRITE_ZEROES at offset 0, 4096 bytes: inside the fixture's 1 MiB export
	// (newWriteVolumeFixture -> newIntegrationFixtureAs -> 1<<20), so it passes
	// handleDiscard's bounds check and reaches the store. A range past exp.Size
	// would be answered NBD_ENOSPC and PunchHole would never be called, leaving
	// the assertion below vacuously true.
	if rep := writeZeroesThroughClient(t, client, 0, 4096, 0); rep.Error != 0 {
		t.Fatalf("WRITE_ZEROES reply error = %d, want 0", rep.Error)
	}
	if !st.punchHoleHadDeadline {
		t.Fatal("PunchHole was called without a request deadline on its context")
	}
	if st.punches != 1 {
		t.Fatalf("the store saw %d punches, want 1", st.punches)
	}
}
