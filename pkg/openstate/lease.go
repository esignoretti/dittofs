package openstate

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// sweepEvery is how often expired leases and proposals are looked for.
//
// ponytail: each sweep scans every lease and proposal, O(clients), and a client
// is expired up to one interval after its lease ends. Replace it with a timer
// heap when a profile shows the sweep, or a client must expire on the instant.
const sweepEvery = 10 * time.Second

// Start arms the lease timer: a client silent for a whole lease period is
// expired, and a proposal not confirmed within one is dropped.
func (t *Table) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.d.Clock.After(sweepEvery):
				t.sweep(ctx)
			}
		}
	})
	t.cmu.Lock()
	t.stop = func() { cancel(); wg.Wait() }
	t.cmu.Unlock()
}

// Stop stops the timers Start armed. Open state is left as it is.
func (t *Table) Stop() {
	t.cmu.Lock()
	stop := t.stop
	t.stop = nil
	t.cmu.Unlock()
	if stop != nil {
		stop()
	}
}

func (t *Table) sweep(ctx context.Context) {
	var ended []entity.ClientID
	t.cmu.Lock()
	now := t.d.Clock.Now()
	for c, end := range t.leases {
		if !now.Before(end) {
			ended = append(ended, c)
		}
	}
	for k, u := range t.proposals {
		if !now.Before(u.expires) {
			delete(t.proposals, k)
		}
	}
	t.cmu.Unlock()
	for _, c := range ended {
		// A failed expiry is tried again at the next sweep: the lease stays
		// ended, so the client is already refused.
		if err := t.Expire(ctx, c); err != nil {
			t.cmu.Lock()
			t.leases[c] = now
			t.cmu.Unlock()
		}
	}
}
