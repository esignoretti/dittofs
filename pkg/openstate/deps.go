package openstate

import (
	"errors"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// Clock is open state's only time source: every lease, deadline and grace
// period is read from it, so a test runs a 90 s lease in microseconds.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan struct{}
}

// Deps is what the table is built over.
type Deps struct {
	Store *kvstore.Store // its client records, through the Clients view
	Clock Clock
}

// New builds an empty table. It starts nothing: Start arms its timers.
func New(d Deps) (*Table, error) {
	if d.Store == nil || d.Clock == nil {
		return nil, errors.New("openstate: a table needs a store and a clock")
	}
	return &Table{
		d:         d,
		files:     map[entity.FileID]*entry{},
		refs:      map[refKey]holder{},
		shards:    map[ShardID]shardGrace{},
		leases:    map[entity.ClientID]time.Time{},
		proposals: map[proposalKey]unconfirmed{},
	}, nil
}
