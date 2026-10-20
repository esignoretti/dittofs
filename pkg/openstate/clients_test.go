package openstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

var ctx = context.Background()

func nfsClient(owner string, verifier byte, principal entity.Principal) entity.Client {
	return entity.Client{Protocol: entity.ProtocolNFSv4, Owner: []byte(owner), Verifier: [8]byte{verifier}, Principal: principal}
}

func principal(n byte) entity.Principal {
	return entity.Principal{Kind: entity.PrincipalUser, ID: entity.PrincipalID{n}}
}

// connect proposes c and confirms the proposal, failing the test on an error.
func (f *fixture) connect(t *testing.T, c entity.Client) entity.ClientID {
	t.Helper()
	p, err := f.Propose(ctx, c)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	id, err := f.Connect(ctx, c, &p)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if id != p.Client {
		t.Fatalf("Connect answered %d, the proposal %d", id, p.Client)
	}
	return id
}

// A client is found by (protocol, owner): the same verifier and principal
// keep its ID, across a restart too; a new verifier is a reboot with a new ID
// and the old record gone; another principal is refused.
func TestClientFoundByOwner(t *testing.T) {
	f := newFixture(t)
	alice := principal(1)
	id := f.connect(t, nfsClient("host-a", 1, alice))

	if again := f.connect(t, nfsClient("host-a", 1, alice)); again != id {
		t.Errorf("the same owner, verifier and principal: ID %d, want %d", again, id)
	}
	if _, err := f.Propose(ctx, nfsClient("host-a", 1, principal(2))); !errors.Is(err, ErrClientInUse) {
		t.Errorf("another principal: %v, want ErrClientInUse", err)
	}
	if len(f.proposals) != 0 {
		t.Errorf("a refused proposal left %d proposals", len(f.proposals))
	}

	restarted := reopen(t, f.fs, f.clock)
	if again := restarted.connect(t, nfsClient("host-a", 1, alice)); again != id {
		t.Errorf("after a restart: ID %d, want %d", again, id)
	}

	rebooted := restarted.connect(t, nfsClient("host-a", 2, alice))
	if rebooted == id {
		t.Fatalf("a new verifier kept ID %d", id)
	}
	if _, err := f.fs.Store.Clients().Get(ctx, id); err == nil {
		t.Errorf("the rebooted client's old record %d is still there", id)
	}
	if err := restarted.Renew(ctx, id); !errors.Is(err, ErrStaleClient) {
		t.Errorf("renewing the old ID after the reboot: %v, want ErrStaleClient", err)
	}
}

// A proposal changes nothing until its confirm, and only its own principal,
// naming its ID and its confirm verifier within a lease period, confirms it.
func TestProposalChangesNothingUntilConfirmed(t *testing.T) {
	f := newFixture(t)
	alice := principal(1)
	id := f.connect(t, nfsClient("host-a", 1, alice))

	reboot := nfsClient("host-a", 2, alice)
	p, err := f.Propose(ctx, reboot)
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := f.fs.Store.Clients().ByOwner(ctx, entity.ProtocolNFSv4, []byte("host-a")); err != nil || rec.ID != id {
		t.Fatalf("after the proposal the owner's record is %d (%v), want the old %d", rec.ID, err, id)
	}
	if err := f.Renew(ctx, id); err != nil {
		t.Fatalf("the old ID after a proposal for a new verifier: %v", err)
	}

	for _, tc := range []struct {
		name string
		c    entity.Client
		p    Proposal
	}{
		{"another principal", nfsClient("host-a", 2, principal(2)), p},
		{"another client ID", reboot, Proposal{Client: p.Client + 1, Confirm: p.Confirm, Sequence: p.Sequence}},
		{"another confirm verifier", reboot, Proposal{Client: p.Client, Confirm: [8]byte{^p.Confirm[0]}, Sequence: p.Sequence}},
		{"another sequence", reboot, Proposal{Client: p.Client, Confirm: p.Confirm, Sequence: p.Sequence + 1}},
	} {
		if _, err := f.Connect(ctx, tc.c, &tc.p); !errors.Is(err, ErrStaleClient) {
			t.Errorf("confirm with %s: %v, want ErrStaleClient", tc.name, err)
		}
	}

	f.clock.advance(leasePeriod)
	if _, err := f.Connect(ctx, reboot, &p); !errors.Is(err, ErrStaleClient) {
		t.Errorf("confirm a lease period after the proposal: %v, want ErrStaleClient", err)
	}
}

// A CREATE_SESSION replay gets the first reply, also on a table built after a
// restart; one that skips a sequence ID is misordered. Renewing writes nothing.
func TestCreateSessionSequenceAndRenewalCost(t *testing.T) {
	f := newFixture(t)
	c := nfsClient("host-a", 1, principal(1))
	p, err := f.Propose(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.Connect(ctx, c, &p)
	if err != nil {
		t.Fatal(err)
	}
	if cached, err := f.CreateSession(ctx, id, p.Sequence, []byte("first")); err != nil || cached != nil {
		t.Fatalf("the first CREATE_SESSION: cached %q, %v", cached, err)
	}
	if cached, err := f.CreateSession(ctx, id, p.Sequence, []byte("second")); err != nil || !bytes.Equal(cached, []byte("first")) {
		t.Errorf("its replay: cached %q, %v; want the first reply", cached, err)
	}
	if _, err := f.CreateSession(ctx, id, p.Sequence+2, nil); !errors.Is(err, ErrSeqMisordered) {
		t.Errorf("sequence + 2: %v, want ErrSeqMisordered", err)
	}

	f.fs.Counts.Take()
	for range 100 {
		if err := f.Renew(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.fs.Counts.Take(); n.Sets+n.Deletes != 0 {
		t.Errorf("100 renewals wrote %d keys, want none", n.Sets+n.Deletes)
	}
}

// A client silent for a lease period is expired by the lease timer, with its
// record; one that renews within it is kept.
func TestSilentClientExpires(t *testing.T) {
	f := newFixture(t)
	quiet := f.connect(t, nfsClient("quiet", 1, principal(1)))
	busy := f.connect(t, nfsClient("busy", 1, principal(2)))

	f.clock.advance(leasePeriod - time.Second)
	if err := f.Renew(ctx, busy); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(2 * time.Second)
	if err := f.Renew(ctx, quiet); !errors.Is(err, ErrStaleClient) {
		t.Errorf("renewing a lease that ended before the sweep: %v, want ErrStaleClient", err)
	}
	f.sweep(ctx)

	if err := f.Renew(ctx, quiet); !errors.Is(err, ErrStaleClient) {
		t.Errorf("a client silent for a lease period: %v, want ErrStaleClient", err)
	}
	if _, err := f.fs.Store.Clients().Get(ctx, quiet); err == nil {
		t.Error("the expired client's record is still there")
	}
	if err := f.Renew(ctx, busy); err != nil {
		t.Errorf("a client that renewed within the lease: %v", err)
	}
}

// Start's timer runs the sweep on the table's clock, and Stop stops it.
func TestStartSweepsOnTheClock(t *testing.T) {
	f := newFixture(t)
	id := f.connect(t, nfsClient("host-a", 1, principal(1)))
	f.Start(ctx)
	defer f.Stop()
	for deadline := time.Now().Add(5 * time.Second); ; {
		f.clock.advance(sweepEvery)
		if _, err := f.fs.Store.Clients().Get(ctx, id); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the client was not expired after the clock passed its lease")
		}
		time.Sleep(time.Millisecond)
	}
}

// An owner is 1 to 1,024 bytes, and a node holds a bounded number of
// proposals, a replacement for an owner already proposed included.
func TestProposalBounds(t *testing.T) {
	f := newFixture(t)
	for _, n := range []int{0, maxOwner + 1} {
		if _, err := f.Propose(ctx, nfsClient(string(make([]byte, n)), 1, principal(1))); !errors.Is(err, ErrBadOwner) {
			t.Errorf("an owner of %d bytes: %v, want ErrBadOwner", n, err)
		}
	}
	if _, err := f.Propose(ctx, nfsClient(string(make([]byte, maxOwner)), 1, principal(1))); err != nil {
		t.Errorf("an owner of %d bytes: %v", maxOwner, err)
	}
	for i := len(f.proposals); i < maxProposals; i++ {
		if _, err := f.Propose(ctx, nfsClient(fmt.Sprint("owner-", i), 1, principal(1))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Propose(ctx, nfsClient("one-too-many", 1, principal(1))); !errors.Is(err, ErrDelay) {
		t.Errorf("a proposal past the cap: %v, want ErrDelay", err)
	}
	if _, err := f.Propose(ctx, nfsClient("owner-1", 2, principal(1))); err != nil {
		t.Errorf("replacing a held proposal at the cap: %v", err)
	}
}
