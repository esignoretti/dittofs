package openstate

import (
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
)

// fakeClock moves only when a test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan struct{}
}

func newClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan struct{})
	c.waiters = append(c.waiters, waiter{c.now.Add(d), ch})
	return ch
}

// advance moves the clock by d and fires every timer now due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
		} else {
			close(w.ch)
		}
	}
	c.waiters = kept
}

// fixture is a table over a fresh in-memory store, with the store's write
// counts and the clock the table reads.
type fixture struct {
	*Table
	fs    *kvstoretest.FS
	clock *fakeClock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fs := kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(badgerkv.Options{})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = db.Close() })
		return db
	})
	return reopen(t, fs, newClock())
}

// reopen builds a new table over fs's store, as a restart does: the store
// keeps what is durable, and the table starts empty.
func reopen(t *testing.T, fs *kvstoretest.FS, clock *fakeClock) *fixture {
	t.Helper()
	tab, err := New(Deps{Store: fs.Store, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{Table: tab, fs: fs, clock: clock}
}
