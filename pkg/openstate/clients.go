package openstate

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

const (
	// leasePeriod is the NFSv4 lease, and every period derived from it.
	//
	// decision: 90 s, long enough that a client is not expired across a brief
	// network or suspend gap, short enough that a failover's grace stays near a
	// minute and a half. It is not measured. Change the one value, not a
	// setting, if failover time or clients expired by survivable gaps say so.
	leasePeriod = 90 * time.Second

	// maxOwner is the longest owner a client may name itself by: the
	// protocols' own opaque limit.
	maxOwner = 1024

	// ponytail: a node holds at most this many proposals; past it a proposal
	// is answered ErrDelay. There is no per-address cap yet, so one source can
	// fill it. Add one, and raise this, if mounts are refused below it.
	maxProposals = 4096
)

// Proposal is what Propose answers and Connect confirms.
type Proposal struct {
	Client   entity.ClientID
	Confirm  [8]byte // NFSv4.0: the confirm verifier, 64 random bits
	Sequence uint32  // NFSv4.1: the sequence ID the first CREATE_SESSION carries
}

type proposalKey struct {
	protocol entity.Protocol
	owner    string
}

// unconfirmed is a proposal not yet confirmed. It is volatile, held only by
// the node that answered it, and never written: a client whose node restarts
// between the two calls repeats its proposal, as both protocols' clients
// already do on a stale client ID.
type unconfirmed struct {
	Proposal
	verifier  [8]byte
	principal entity.Principal
	expires   time.Time
}

// Propose answers an NFSv4 SETCLIENTID or EXCHANGE_ID. It changes no state: a
// new verifier does not end the old client ID until its confirm. The ID is an
// existing client's when the owner, verifier and principal match its record,
// and a new one otherwise. An owner a record holds under another principal is
// refused ErrClientInUse and leaves no proposal. A later proposal for the same
// owner replaces this one, and one expires a lease period after the request.
func (t *Table) Propose(ctx context.Context, c entity.Client) (Proposal, error) {
	if len(c.Owner) == 0 || len(c.Owner) > maxOwner {
		return Proposal{}, ErrBadOwner
	}
	p := Proposal{Sequence: 1}
	rec, found, err := t.byOwner(ctx, c)
	switch {
	case err != nil:
		return Proposal{}, err
	case found && rec.Principal != c.Principal:
		return Proposal{}, ErrClientInUse
	case found && rec.Verifier == c.Verifier:
		p.Client, p.Sequence = rec.ID, rec.CSSeq+1
	default:
		if p.Client, err = t.d.Store.NextClientID(ctx); err != nil {
			return Proposal{}, err
		}
	}
	_, _ = rand.Read(p.Confirm[:]) // never fails: crypto/rand aborts the process instead

	key := proposalKey{c.Protocol, string(c.Owner)}
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if _, replaces := t.proposals[key]; !replaces && len(t.proposals) >= maxProposals {
		return Proposal{}, ErrDelay
	}
	t.proposals[key] = unconfirmed{
		Proposal:  p,
		verifier:  c.Verifier,
		principal: c.Principal,
		expires:   t.d.Clock.Now().Add(leasePeriod),
	}
	return p, nil
}

// Connect makes a client current. An NFSv4 client confirms the proposal p
// this node answered: from the proposal's principal, naming its client ID and
// its confirm verifier and sequence, within its lease period. A reboot, a new
// verifier under a known owner, replaces the old record and ends the old ID.
// SMB and NLM clients confirm in one step with a nil p; an SMB record names a
// machine, so it is never refused for its principal.
func (t *Table) Connect(ctx context.Context, c entity.Client, p *Proposal) (entity.ClientID, error) {
	if len(c.Owner) == 0 || len(c.Owner) > maxOwner {
		return 0, ErrBadOwner
	}
	if p != nil {
		key := proposalKey{c.Protocol, string(c.Owner)}
		t.cmu.Lock()
		u, ok := t.proposals[key]
		if !ok || u.Proposal != *p || u.principal != c.Principal || !t.d.Clock.Now().Before(u.expires) {
			t.cmu.Unlock()
			return 0, ErrStaleClient
		}
		delete(t.proposals, key)
		t.cmu.Unlock()
		c.ID, c.Verifier, c.CSSeq = u.Client, u.verifier, u.Sequence-1
	} else {
		c.ID = 0
	}

	rec, found, err := t.byOwner(ctx, c)
	switch {
	case err != nil:
		return 0, err
	case !found:
		if c, err = t.d.Store.Clients().Create(ctx, c); err != nil {
			return 0, err
		}
	case p != nil && rec.Principal != c.Principal:
		return 0, ErrClientInUse
	case rec.ID == c.ID || (p == nil && rec.Verifier == c.Verifier):
		c = rec
	default:
		if c, err = t.d.Store.Clients().Reboot(ctx, rec.ID, c); err != nil {
			return 0, err
		}
		t.endLease(rec.ID)
	}
	if c.Protocol == entity.ProtocolNFSv4 {
		t.cmu.Lock()
		t.leases[c.ID] = t.d.Clock.Now().Add(leasePeriod)
		t.cmu.Unlock()
	}
	return c.ID, nil
}

// CreateSession applies an NFSv4.1 CREATE_SESSION: a replay of the last one
// applied returns its cached reply, the next one records reply, any other is
// ErrSeqMisordered. It renews the client's lease.
func (t *Table) CreateSession(ctx context.Context, c entity.ClientID, seq uint32, reply []byte) ([]byte, error) {
	if err := t.Renew(ctx, c); err != nil {
		return nil, err
	}
	return t.d.Store.Clients().CreateSession(ctx, c, seq, reply)
}

// Renew renews an NFSv4 client's lease for another lease period. It writes
// nothing. A client whose lease has ended, or that this node never connected,
// is ErrStaleClient.
func (t *Table) Renew(_ context.Context, c entity.ClientID) error {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	now := t.d.Clock.Now()
	if end, ok := t.leases[c]; !ok || !now.Before(end) {
		return ErrStaleClient
	}
	t.leases[c] = now.Add(leasePeriod)
	return nil
}

// Expire ends a client: its lease and its record go.
func (t *Table) Expire(ctx context.Context, c entity.ClientID) error {
	t.endLease(c)
	if err := t.d.Store.Clients().Delete(ctx, c); err != nil && !merrs.IsNotFoundError(err) {
		return err
	}
	return nil
}

func (t *Table) endLease(c entity.ClientID) {
	t.cmu.Lock()
	delete(t.leases, c)
	t.cmu.Unlock()
}

// byOwner finds the record c's protocol and owner name, if there is one.
func (t *Table) byOwner(ctx context.Context, c entity.Client) (entity.Client, bool, error) {
	rec, err := t.d.Store.Clients().ByOwner(ctx, c.Protocol, c.Owner)
	if merrs.IsNotFoundError(err) {
		return entity.Client{}, false, nil
	}
	if err != nil {
		return entity.Client{}, false, err
	}
	return rec, true, nil
}
