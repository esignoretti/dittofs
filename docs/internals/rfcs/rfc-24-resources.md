---
rfc: 24
title: "RFC 24 — resources and concurrency"
component: resources
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-3-syncer]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-20-adapters]]"
  - "[[rfc-21-nfs]]"
  - "[[rfc-22-smb]]"
  - "[[rfc-23-control-plane]]"
  - "[[rfc-25-observability]]"
  - "[[rfc-29-test-harness]]"
aliases:
  - RFC 24
  - resources and concurrency
tags:
  - rfc
---
# RFC 24 — resources and concurrency

**Status:** draft. [§12](#12.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone adding a buffer, a cache, a queue, a goroutine or a worker
to any component, and anyone sizing a node or reading why it refused work.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix B](#Appendix%20B%20%E2%80%94%20where%20the%20current%20code%20differs) says where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

A DittoFS node is one process with a fixed amount of memory, a fixed number of
cores and a fixed number of open files. Every component wants some of each: the
adapters hold request and reply buffers, the filesystem service holds requests
it is waiting on, open state holds opens and locks, the metadata store keeps a
cache, the journal keeps an index of what it holds, and the syncer keeps chunks
in flight to and from the remote tier. Each of those RFCs already bounds its
own part. None of them can say whether the parts add up to less than the
machine.

This RFC is the arithmetic. It takes **one memory limit** for the process,
divides it into named **pools**, gives each pool a share, a floor and a rule
for what happens when it is full, and fixes where in a request's path each pool
is charged. It does the same, more briefly, for CPU, goroutines and file
descriptors. And it states the rule that makes overload survivable: **no queue
anywhere is unbounded**, so a node that has more work than it can do slows its
clients down or tells them to retry, and never grows until the kernel kills it.

**The problem, with one example.** P1 runs in a container with an 8 GiB memory
limit, serving `builds` over NFS and `profiles` over SMB. On Monday morning the
remote store behind `builds` slows to a crawl, and build01 starts a large build
that reads hundreds of cold files at once.

Each component is within its own bounds. The NFS adapter allows 64 requests per
session, and build01 has four sessions; each cold read waits up to the request
deadline for its chunks, holding a 1 MiB reply buffer. The syncer's fetch pool
is full, so further fetches wait in its bounded queues. Meanwhile 40 users sign
in to their profile disks over SMB, each with 512 credits, and their writes
arrive while the per-share journal limit is near and writes are paced. The
metadata store sized its cache from the machine's memory, and so did the
journal's index; neither knows the other did.

Without a node budget, the sum of those bounds is larger than 8 GiB. The heap
grows, the garbage collector cannot keep up, and the container is killed. Every
share on the node is gone, and on restart every client reconnects at once into
the same state.

With this RFC:

1. P1 reads its 8 GiB limit from the container at start and tells the runtime
   to keep its heap under it (§2.1).
2. The 8 GiB is split into pools before any request is served: 512 MiB for
   receive buffers, 512 MiB for reply buffers, 512 MiB for held requests, and
   so on down to a quarter left unpooled for the runtime (§2.3).
3. build01's cold reads take reply buffers until `builds` holds three quarters
   of the reply pool. Its next read is answered `NFS3ERR_JUKEBOX` and build01
   retries it later (§4.2). The last quarter stays free for `profiles`.
4. alice-pc's writes take receive buffers in turns with every other client
   (§4.1), so the 40 profile users keep writing at the pace the journal admits
   ([RFC 8 §10.2.1](rfc-8-engine.md#10.2.1%20Writes%20are%20paced%20before%20the%20limit%2C%20not%20stopped%20at%20it)),
   while the `builds` slowdown costs `builds`.

Outcome: P1 stays inside its limit, `profiles` keeps working, and `builds`
slows down with retry-later answers instead of taking the node with it.

![The node memory budget: one process memory limit divided into eight named pools and an unpooled quarter, each pool with its share of the limit and what happens at its ceiling — readers wait, calls are refused retry-later, caches evict, or the owning RFC's refusal applies](img/rfc24-budget.svg)

**The words you need.**

- **process memory limit** (*L*) — the memory this process may use, learned
  once at start (§2.1). Every share below is a fraction of it.
- **pool** — a named part of *L* that one kind of memory is charged to (§2.3).
  A pool is a counter of bytes reserved, not an allocator.
- **reservation** — bytes charged to a pool **before** the memory is used, and
  released when it is freed (§2.5). Memory nobody reserved is the unpooled
  quarter's.
- **ceiling** — a pool's share of *L*. **floor** — a size a pool keeps however
  small *L* is (§2.4).
- **client key** — what fairness counts as one client: the connection's remote
  address (§4.1).
- **backpressure** — slowing the producer instead of buffering its output: not
  reading a socket, pacing a write, making a transfer wait for a worker (§5).

**What this RFC promises.**

- The memory the pools may hold adds up to at most three quarters of *L*,
  whatever the number of shares, clients, connections or journals.
- Every pool says what happens at its ceiling, and it is one of four things: a
  reader stops reading, a call waits within its deadline, a call is refused
  retry-later with a named cause, or a cache evicts.
- No client, connection or share can take more than three quarters of a request
  pool while another is waiting for it, and the next free buffer goes round in
  turns.
- No queue is unbounded, and the [table in §5.1](#5.1%20No%20queue%20is%20unbounded)
  names every one with its bound.
- Each bound another RFC already states — the journal's index, the syncer's
  pools — is composed here, not redefined: this RFC sets its
  share and checks at start that the configuration fits.

**How the rest is organised.** §1 gives the purpose and non-goals. §2 is the
budget: the limit, the pools, floors and reservations. §3 is request and reply
buffers, the largest pool on the request path. §4 is admission and fairness —
across clients, shares, held requests, connections and open state. §5 is
backpressure: the queue inventory and the chain from the socket to the remote
tier. §6 is goroutines, CPU and file descriptors. §7 composes the storage RFCs'
bounds into the budget. §8 says what the reference box measures to set the
defaults. §9 to §11 are invariants, metrics and conformance; §12 lists open
questions. Appendix A is prior art, Appendix B where the current code differs.

### Scenarios

**A share's remote store hangs while another share is busy.** P1, 8 GiB, the
remote behind `builds` stops answering; `profiles` is on a different store.

1. build01's cold reads each take a 1 MiB reply reservation (§3.3) and wait in
   the fetcher's queue for `builds`'s flow
   ([RFC 3 §2.9](rfc-3-syncer.md#2.9%20Workers%20are%20shared%20fairly%20across%20flows)).
2. The reply pool, 512 MiB, passes half full. From then on the share cap binds:
   once `builds`'s calls hold 384 MiB of it, the service refuses the next
   `builds` call with `ErrDelay` naming `DelayResources` (§4.2), before it
   takes any more memory. build01 is answered `NFS3ERR_JUKEBOX` and retries
   seconds later.
3. Each waiting read ends at its deadline
   ([RFC 0 §10.3](rfc-0-data-lifecycle.md#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)),
   releasing its reservation; build01's retries take them again, up to the cap.
4. alice-pc's reads and writes on `profiles` find 128 MiB of the reply pool and
   all of the receive pool's turns they are owed.

Outcome: the hung store costs `builds` its throughput and nothing else.

**P1 starts in a container.** The container's memory limit is 8 GiB; no
runtime soft limit is set in the environment.

1. P1 reads the limit from its control group: 8 GiB, source `container`
   (§2.1).
2. It sets the runtime's soft memory limit to 8 GiB, so the collector works
   harder as the heap nears it rather than letting it pass.
3. It derives every pool (§2.3) and the defaults that follow from them: the
   journal's index bound, 1 GiB at 51 bytes an entry for its one journal, about
   19 million entries with their excess (§7.1); the syncer's worker counts, reduced from 128 so
   both halves and their staging fit 1 GiB (§7.2); the connection limit (§4.4).
4. It logs each pool, its share and its source, and exports them as gauges
   (§10).

Outcome: an operator reads in one place what the node will hold, and why.

**One client opens thousands of connections.** A misconfigured load generator
on one address opens connections to P1 in a loop.

1. Each connection is charged its fixed cost to the connection-state pool at
   accept (§4.4).
2. Once the address holds a quarter of the connection limit, its further
   connections are closed at accept, before any byte is read.
3. Every other address can still connect: the limit's other three quarters, and
   the pool behind them, are not this address's to take.

Outcome: one address cannot lock other clients out of the node.

**A share is frozen while SMB clients keep writing.** `profiles` is frozen for
a move for 20 s, and the SMB adapter holds each write pending
([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)).

1. Each SMB write the service holds keeps its receive reservation while its
   connection is within its floor of 256 requests and 32 MiB
   ([RFC 22 §3.3](rfc-22-smb.md#3.3%20Sizes%20and%20credits)).
2. Past the floor, the held write's bytes move from the receive pool to the hold
   pool, a sixteenth of *L* (§4.3); the receive pool gets them back for other
   clients.
3. With the hold pool full too, the adapter answers a further past-floor write
   `STATUS_INSUFFICIENT_RESOURCES` at once, as RFC 20 §4.6 and RFC 22 state.

Outcome: a freeze holds what the floors and the hold pool allow, and the rest of
the node keeps reading.

## In short

- One process memory limit *L*: the runtime soft limit if one is set, else the
  container's, else the host's memory. Learned once; the runtime is told.
- Eight pools take at most three quarters of *L*: receive buffers 1/16, reply
  buffers 1/16, held requests 1/16, connection state 1/16, open state 1/16,
  metadata store 3/16, journal index 1/8, transfers 1/8. The last quarter is
  the runtime's. The fractions are fixed.
- Memory is reserved before it is used. A reservation that cannot be met waits
  (receive and reply buffers, transfers), is refused retry-later (open state,
  share cap, hold pool), or evicts (the metadata store's cache).
- Reservations are taken in one order along a request's path and never waited
  for while holding a later one, so no two waits form a cycle.
- Fairness: buffers go to waiting clients in deficit round robin; no client and
  no share holds more than three quarters of a buffer pool while another waits.
- No queue is unbounded; a full link waits or refuses, and the slow end of the
  chain slows the client through TCP, pacing or a retry-later answer.
- One goroutine per admitted request, two per connection, one per pool worker,
  and none per queued item. Background CPU work holds at most three quarters
  of the cores at once.
- The storage RFCs' bounds — index entries, workers, staging — keep their rules;
  their defaults now derive from their pool, and a configuration that does not
  fit is refused at start.

---

## 1. Purpose

Every component RFC bounds its own memory. The syncer bounds its workers
([RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound)),
the journal its index
([RFC 1 §4.2](rfc-1-journal.md#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)),
the adapters their held requests
([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)) and their
flow control ([RFC 20 §6.3](rfc-20-adapters.md#6.3%20Flow%20control%20is%20the%20protocol%27s%20own)).
A bound that is correct for its component says nothing about the process:
eight correct bounds, each sized from the machine's whole memory, add up to
eight machines. This RFC owns the sum. It fixes one limit, divides it, says
where each part is charged, and says what each refusal looks like on the wire,
so that overload has a specified outcome instead of an out-of-memory kill.

### 1.1 Non-goals

This RFC **MUST NOT** be read as specifying:

- the internals of a bound another RFC owns — the index's entry rules, the
  syncer's scheduling, the adapters' counting of held requests — only their share of the
  budget and how their defaults derive from it;
- local disk space — the journal's capacity and its pacing are
  [RFC 1 §6](rfc-1-journal.md#6.%20Capacity)'s and
  [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s;
  disk appears here only where it holds back memory;
- quotas, which are logical limits on what users store
  ([RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota)), not resources of a node;
- per-share or per-user quality-of-service weights; all clients and shares
  weigh the same (§12);
- the protocols' own flow-control numbers — slots, credits, transfer sizes —
  which are [RFC 21](rfc-21-nfs.md)'s and [RFC 22](rfc-22-smb.md)'s; this RFC
  states what they cost.

## 2. The node memory budget

### 2.1 One limit, learned once

A node has one **process memory limit**, *L*. It is a bootstrap fact
([RFC 13 §2.2](rfc-13-configuration.md#2.2%20A%20host%20holds%20only%20its%20bootstrap)),
read once at start, in this order, the first that is set winning:

1. the soft memory limit the runtime is told by the environment;
2. the container's limit: the lowest memory limit on the process's control-group
   path, taking the lower of the throttling limit and the hard limit where both
   are set, and treating "unlimited" as unset;
3. the host's physical memory.

The node **MUST** record which source it used, and **MUST** expose *L* and its
source (§10). A limit changed while the node runs takes effect at the next
start; the pools are never resized while serving.

**The runtime is told.** When *L* comes from the second or third source, the
node **MUST** set the runtime's soft memory limit to *L* before it serves, so
the collector works harder as the heap approaches *L* rather than letting a
burst of garbage pass it. The node **MUST NOT** change the collector's growth
target from its default.

> decision: *L* is read once, not tracked. A pool that shrinks while serving
> must take back memory it has handed out, which means evicting live requests,
> and a resize that only ever grows is no help when a container is resized
> down. Restarting to resize costs a drain. Revisit if an orchestrator is shown
> resizing a running node's memory as routine.

> decision: on a host shared with other processes, the host's memory is the
> wrong limit and the node cannot tell. The operator sets the runtime soft
> limit, which is source 1. The node does not guess a fraction of the host for
> its neighbours.

### 2.2 What is outside the budget

*L* is the process's own memory. Three things the kernel charges to the
process, or to its container, are not pooled:

- **the page cache** of journal and store files: the kernel reclaims clean
  pages before it kills a process, and the journal's syncs bound the dirty ones
  ([RFC 1 §5.2](rfc-1-journal.md#5.2%20Sync%20policy));
- **goroutine stacks, the runtime's own structures and the collector's
  headroom** — the garbage that lives between two collections;
- **code, and memory-mapped files** whose pages the kernel can drop.

Two bounded consumers are not pooled either, because they are fixed by count:

- **the event rings** of [RFC 25 §4.3](rfc-25-observability.md#4.3%20From%20the%20emit%20point%20to%20the%20spool):
  16,384 `access` and 4,096 `quota` entries of at most 1 KiB each, allocated at
  start — at most 20 MiB per node, whatever the load;
- **management-API calls**, at most 64 at once
  ([RFC 23 §4.3](rfc-23-control-plane.md#4.3%20Writes)), which draw on none of
  the pools.

The unpooled quarter of *L* (§2.3) is the margin for all of these, and, during
grace only, for the open-state pool's reclaim allowance (§4.6), at most one more
sixteenth. It is not reserved by anyone and nothing waits for it.

### 2.3 The pools

| Pool | Holds | Share of *L* | Floor | At its ceiling |
| --- | --- | --- | --- | --- |
| **receive** | request buffers from frame until the call returns, writes' payloads included (§3.2) | 1/16 | 16 MiB | the connection's reader waits; the socket is not read, and TCP pushes back on the client |
| **reply** | reply buffers from before the call until the writer has sent them (§3.3) | 1/16 | 16 MiB | the call waits, within its deadline; at the deadline `ErrDelay` with `DelayResources` |
| **hold** | held requests' bytes past their connection's floor ([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)) | 1/16 | none | the adapter answers the request at once, holding nothing: `STATUS_INSUFFICIENT_RESOURCES` over SMB (§4.3) |
| **connection state** | each connection's fixed cost; sessions and their reply caches; security contexts (§4.4, §4.5) | 1/16 | 4 MiB | new connections closed at accept; a new session refused retry-later or granted fewer slots |
| **open state** | opens, byte-range locks, lock waiters, caching grants, quota reservations; **(cluster)** the routed-request dedup table (§4.6) | 1/16 | 4 MiB | new state refused with `ErrDelay` and `DelayResources`; no caching grant offered; never a close or unlock; a reclaim only past its allowance |
| **metadata store** | the store's in-process memory: caches, write buffers, compaction (§7.3) | 3/16 | 64 MiB | the store evicts; it never refuses |
| **journal index** | the placement index of every journal ([RFC 1 §4.2](rfc-1-journal.md#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)) (§7.1) | 1/8 | none | the index's own refusal ([RFC 1 §4.2](rfc-1-journal.md#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)), answered retry-later |
| **transfers** | both syncer halves and the staging budget ([RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound)) (§7.2) | 1/8 | eight workers per half, and the staging they imply | the syncer's own wait or refusal ([RFC 3 §2.3](rfc-3-syncer.md#2.3%20Backpressure%20propagates%3B%20it%20does%20not%20buffer)) |
| *unpooled* | §2.2 | 1/4 | — | — |

Five pools are counted at run time — receive, reply, hold, connection state,
open state — and are `Budget`s (§2.5). The other three are **sizes**: the
composition root hands each its figure at construction, and its owner enforces
its own bound (§7). Nothing reserves from them.

The shares are **fixed**, not settings. On a 64 GiB node they are 4 GiB each
for the five small pools, 12 GiB for the metadata store, 8 GiB each for the
index and transfers, and 16 GiB unpooled; on an 8 GiB container, 512 MiB, 1.5 GiB,
1 GiB and 2 GiB.

A pool belongs to the role that composes what it holds
([RFC 15 §2.4](rfc-15-topology.md#2.4%20Composition%20by%20role)): a
`protocol`-only node has no journal index, transfers or open state, and their
shares are left unpooled; a `storage`-only node serves no client connection.
The shares do not move between roles.

> ponytail: fixed fractions, the same on every node. The metadata store's share
> is the guess most likely wrong — a namespace-heavy workload wants more, a
> streaming one less — and a `protocol`-only node leaves about five sixteenths
> idle. Move shares by measurement when §8's M3 shows the store's hit rate
> limiting a workload while another pool sits below half its ceiling, or a split
> deployment's protocol node is shown memory-bound; make a share a setting only
> when two named workloads want different values on the same hardware.

> decision: the hold pool is a sixteenth of *L*, derived, not a setting; a
> test sizes it by setting the process limit, 128 MiB giving 8 MiB. Its size is
> argued from a freeze's worth of held profile-disk writes
> ([RFC 22 §3.3](rfc-22-smb.md#3.3%20Sizes%20and%20credits)): on a 64 GiB node
> it is 4 GiB, and a multi-session host with 40 users at queue depth 8 of 1 MiB
> writes holds 320 MiB through a freeze. Each request drawn on it is charged
> its write's bytes, and at least 64 KiB, one SMB credit's size, so it bounds
> held requests as well as bytes. Revisit the fraction when §8's measurements
> show freezes or stalls refusing writes with the pool full while other pools
> sit below half their ceilings.

> ponytail: the hold pool is first come, first served past the floors, so one
> connection's burst can take all of it and leave another refused past its
> floor while that one holds gigabytes. Cap each connection's share of the pool
> when `adapter_hold_refusals_total` counts refusals on connections other than
> the one holding most of it.

### 2.4 Floors

A pool's ceiling is its share of *L* or its floor, whichever is larger. Floors
exist so that a small *L* still serves: the receive and reply floors are two of
the largest request any adapter accepts, SMB's 8 MiB
([RFC 22 §3.3](rfc-22-smb.md#3.3%20Sizes%20and%20credits)), so one such request
always fits with another beside it.

**The budget is checked on the ceilings, not on the floors.** The shares
already sum to three quarters of *L*, so any floor that binds takes the pools
past it, however small the floors' own sum. At start the node **MUST** compute
the **ceilings' sum** — every pool's larger of its share of *L* and its floor —
and compare it, not the floors' sum, with three quarters of *L*.

**A limit below the floors does not stop the node.** When the ceilings' sum
exceeds three quarters of *L* — below about 640 MiB, where the transfers floor,
about 80 MiB at §7.2's figures, binds first, and the metadata store's below
about 340 MiB — the node starts, runs each pool at its ceiling, and raises the
health condition `memory_limit_below_floors` naming *L* and the ceilings' sum.
It may then exceed *L*. A pool without a floor keeps its share of *L*, so a test
that sets *L* to size one pool, as RFC 17's hold-pool test sets 128 MiB to get an
8 MiB pool, still gets the pool it asked for.

### 2.5 Memory is reserved before it is used

A pool is a counter, not an allocator. Each consumer of a counted pool **MUST**
reserve the bytes it is about to hold from its pool **before** it holds them,
and release them when it frees them:

```go
package resources // internal/resources

// Budget is one counted pool (§2.3): waits or refuses at its ceiling, as the pool's row says.
type Budget struct{ /* name, ceiling, waits bool, reserved, per-key DRR queues */ }
func NewBudget(name string, ceiling int64, waits bool) *Budget
// Reserve charges n bytes for key. A waiting budget waits within ctx; a refusing one refuses at once.
func (b *Budget) Reserve(ctx context.Context, key netip.Addr, n int64) (Reservation, error)
func (b *Budget) Reserved() int64 // for gauges and the leak check (§2.7)

// Reservation is a value; Release is idempotent.
type Reservation struct{ b *Budget; n int64 }
func (r *Reservation) Release()

// Pools is what an adapter is handed (RFC 20 §4.7): the four budgets on the request path.
type Pools struct{ Receive, Reply, Conn, Hold *Budget }
```

- Whether a budget waits or refuses is fixed by its pool's row (§2.3), so one
  `Reserve` serves both. A refusing budget ignores `key`; its callers pass the
  zero `netip.Addr`.
- `Reservation` is a value, so taking one allocates nothing.
- A reservation is in bytes of memory held, rounded up to the buffer class
  (§3.1) where a pooled buffer backs it.
- **Charged, not measured.** No pool reads the heap to decide. A charge is
  exact for buffers, and a fixed per-kind cost for state (§4.4, §4.6), which
  §8 measures.
- Every reservation **MUST** be released on every path, a cancelled context and
  a panic included. A pool's reserved bytes are exported (§10), and a pool that
  reads above zero on an idle node is a leak.

### 2.6 Reservations are taken in one order

Along one request's path, reservations are taken in this order and released in
the reverse:

1. **connection state**, at accept, for the connection's life (§4.4);
2. the connection's own **flow control** — slots, credits, the fixed cap —
   checked before the next request is read
   ([RFC 20 §6.3](rfc-20-adapters.md#6.3%20Flow%20control%20is%20the%20protocol%27s%20own));
3. **receive**, at frame, before the request's body is read (§3.2);
4. **reply**, once the arguments are decoded and the reply's bound is known,
   before the service is called (§3.3);
5. the **share cap**, at the service's admission (§4.2);
6. **open state**, when the call creates state (§4.6);
7. below the service: the journal's capacity and its index, then the syncer's
   transfers, in the engine's own order.

A wait for a reservation **MUST NOT** be entered while holding a reservation
later in this order. Each pool is freed by work that needs nothing earlier in
the order — a reply by the writer sending it, a transfer by its store — so no
set of waits forms a cycle and none waits forever. Steps 5 and 6 never wait:
they refuse.

### 2.7 Wiring and lifecycle

The composition root
([RFC 15 §2.4](rfc-15-topology.md#2.4%20Composition%20by%20role)) builds the
budget in two phases, inside the start order
[RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop) owns:

- **(a) before that order opens the metadata store**, because the store's size
  is one of the figures: learn *L* (§2.1), set the runtime's soft limit, and
  compute the fixed fractions, which need only *L* and the bootstrap roles;
- **(b) once the settings are read**: check `upload_workers`, `fetch_workers`,
  `ExtentLimit` and `connection_limit` against their figures (§4.4, §7), and
  build the budgets.

```go
lim, err := resources.Detect()                     // (a) §2.1; refuses an unreadable control group
debug.SetMemoryLimit(lim.Bytes * 15 / 16)          // (a) before any pool fills
sp := resources.SplitFor(lim, roles)               // (a) §2.3, §2.4: the fractions and floors
st, _ := metadata.Open(ctx, kv, metadata.Options{CacheBytes: sp.Meta /* … */}) // a size, §7.3
err = sp.Check(settings)                           // (b) §4.4, §7: refuses what does not fit
pools := &resources.Pools{
	Receive: resources.NewBudget("receive", sp.Receive, true),
	Reply:   resources.NewBudget("reply", sp.Reply, true),
	Conn:    resources.NewBudget("connection", sp.Conn, false),
	Hold:    resources.NewBudget("hold", sp.Hold, false),
}
ost := openstate.New(openstate.Deps{Budget: resources.NewBudget("open", sp.Open, false) /* … */})
svc, _ := vfs.New(vfs.Deps{Shares: resources.NewShareCap(pools.Receive, pools.Reply) /* … */})
// every adapter is handed pools as core.Deps.Pools (RFC 20 §3.4); the journals take
// sp.Index and the syncer sp.Transfers as sizes (§7.1, §7.2); CPU tokens are a
// semaphore.Weighted over sp.Tokens (§6.2)
```

| Component | Built by | Owned by | Lifecycle |
| --- | --- | --- | --- |
| `Limit` | the root, first | the root | read once; never changes |
| `Split` | the root, from `Limit` and roles | the root, a value | recomputed only at the next start |
| `Pools`: receive, reply, connection state, hold | the root | shared by every adapter's `core` | none; checked at zero after the adapters stop |
| open-state `Budget` | the root | the open-state table; **(cluster)** also the dedup table | as above |
| `ShareCap` | the root, over receive and reply | the service's front | entries dropped at zero |
| CPU tokens | the root, from `Split` | shared by background workers | none |
| sizes: metadata store, index, transfers | the root, from `Split` | the store, each journal, the syncer | handed at construction; the owner sizes itself |

- **A consumer takes only the budgets it charges**, as fields of its own
  `Deps`, never the whole set: `core.Deps` takes `Pools`, `openstate.Deps` the
  open-state budget, `vfs.Deps` the share cap; the store, the journals and the
  syncer take a size, not a budget.
- **Nothing here has a lifecycle.** `Detect`, `SplitFor` and `NewBudget` start
  no goroutine and open nothing; a release grants the next waiter on the
  releasing goroutine. There is no `Start` or `Close`. After the adapters and
  the service have stopped, the root logs any budget whose `Reserved()` is not
  zero, and the test harness fails on it.
- **Shared budgets are the root's.** The receive, reply, connection-state and
  hold budgets are shared by every adapter; no adapter may replace or close
  one, and an adapter restarted by the runtime gets the same ones back.
- **Reconfiguration.** *L* and the split change only at a restart (§2.1). A
  live setting a figure bounds — `connection_limit` — is checked against the
  split when the runtime applies it, and refused there as at start.
- **Failure halfway.** Phase (a) fails before anything is opened, and phase (b)
  before anything serves, so a start refused here closes only what RFC 23's
  order already opened.

| Failure | Behaviour |
| --- | --- |
| a control-group file exists but cannot be parsed | start refused, naming the file |
| a budget not at zero at close | logged at error with the pool and bytes; tests fail on it |
| a release past zero | panics in tests; clamps to zero and is counted in production |

### 2.8 Package layout

```text
internal/resources/
  limit.go        Limit, Detect: the runtime limit, the control group, the host (§2.1)
  split.go        Split, SplitFor, Check: fractions, floors, the ceilings' sum, settings (§2.3, §2.4, §7)
  budget.go       Budget, Reservation, Pools: counted pools and their turns (§2.5, §4.1)
  sharecap.go     ShareCap, WithCharge, ChargeOf (§4.2)
  fd.go           the open-file split and fd_limit_low (§6.3)
  metrics.go      memory_*, cpu_* (§10)
  budget_test.go  the turns property test and B1
```

- **One package, no subpackages, a leaf.** It imports only the standard
  library and the metrics registry; its consumers import it, never the reverse.

## 3. Request and reply buffers

### 3.1 Buffer classes

Receive and reply buffers come in a few classes, as
[RFC 20 §6.6](rfc-20-adapters.md#6.6%20Throughput%20and%20latency) has them, each
its size plus the protocol's headers:

| Class | Size | Drawn for |
| --- | --- | --- |
| small | 4 KiB | headers with attributes; most metadata operations |
| directory | 64 KiB | a directory page (`dtpref`, [RFC 21 §2.3](rfc-21-nfs.md#2.3%20Transfer%20sizes)), large attribute and security replies, a one-credit SMB transfer |
| transfer | 1 MiB | `READ` and `WRITE` up to 1 MiB: NFS's largest ([RFC 21 §2.3](rfc-21-nfs.md#2.3%20Transfer%20sizes)), and the size SMB clients commonly use |
| large | 8 MiB | SMB `READ` and `WRITE` past 1 MiB, up to SMB's largest ([RFC 22 §3.3](rfc-22-smb.md#3.3%20Sizes%20and%20credits)) |

A request draws the smallest class that holds it. A buffer larger than the
largest class its protocol uses is not pooled, and is charged its exact size.

Each request is charged its buffer's class size plus a fixed **per-request
overhead** for what the request holds besides its buffer — its goroutine's
stack, its decoded arguments, its context. The overhead is proposed at 8 KiB,
and settled by §8's M1. So a node's requests in flight are bounded by the
receive pool divided by the smallest charge: on an 8 GiB node, 512 MiB over
12 KiB, about 43,000.

### 3.2 Receive: charged at frame, and the reader waits

The frame stage
([RFC 20 §4.1](rfc-20-adapters.md#4.1%20One%20pipeline)) learns the record's
length from its header — the RPC record mark, the NetBIOS session header —
before it reads the body. The reader **MUST** reserve the record's charge from
the receive pool before it reads the body, and **MUST** refuse a length past the
protocol's cap before reserving anything
([RFC 20 §5.2](rfc-20-adapters.md#5.2%20Decoding%20is%20bounded)).

**A record in several fragments is reserved at its cap.** An RPC record mark
gives one fragment's length, not the record's, and a record may span several
fragments. When the first fragment's header does not mark it the last, the
reader **MUST** reserve the class that holds the protocol's largest request
before it reads the first fragment, and **MUST NOT** grow that reservation
later; a fragment that would take the record's total past that cap, or a
record past a fixed number of fragments, is a framing error and closes the
connection ([RFC 20 §5.2](rfc-20-adapters.md#5.2%20Decoding%20is%20bounded)).
The remaining fragments **MUST** arrive within the frame deadline
([RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits)), counted
from the first fragment's header; a record still partial then closes the
connection and releases its reservation. So no reader waits for receive bytes
while holding part of a record. A NetBIOS session header gives the whole message's length, so SMB is
always reserved exactly.

> ponytail: a multi-fragment record is charged its protocol's largest class
> however small it turns out — for NFS, one transfer-class buffer. Common NFS
> clients send each record as one fragment. Reserve incrementally, with a
> cumulative cap, when §8's M1 shows multi-fragment records a measurable share
> of the receive pool on a named client.

**At the receive ceiling the reader waits; it does not refuse.** A reader that
cannot reserve stops reading its connection. The client's data stays in the
socket, TCP's window closes, and the client slows. No request is half-read and
no reply is owed for it. Waiting readers are granted in turns (§4.1).

Nothing on the node waits on a waiting reader: it holds no reservation beyond
its connection's, no worker and no request. Its wait ends when the requests
holding the pool finish, and each of those ends at its deadline
([RFC 0 §10.3](rfc-0-data-lifecycle.md#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)),
so the wait is bounded by the longest deadline in flight, though no deadline of
its own runs: the request it would read has not started.

> decision: memory at the receive ceiling is backpressure, not an error. An
> answer the client must retry costs a read of the whole request it then
> discards — up to 8 MiB of a link's time — and an NFSv3 client pauses
> seconds after `NFS3ERR_JUKEBOX`; a closed window costs the client only the
> wait. This is what a kernel NFS server does when it has no thread or reply
> space: it leaves the transport unread. Refuse instead only if a client is
> shown to time out a connection whose window stays closed within the request
> deadline.

A request's receive reservation is released when its call returns, when its
buffer is no longer lent to the service
([RFC 20 §5.3](rfc-20-adapters.md#5.3%20No%20copies%20on%20the%20data%20path)),
and, where it also carries the request's small reply (§3.3), when the writer has
sent or discarded that reply — whichever is latest — except for a held request,
whose reservation follows §4.3.

### 3.3 Reply: charged before the call, and the call waits

The reply's size is known before the call runs: it is the operation's **reply
bound**, a column of the operation table
([RFC 20 §4.2](rfc-20-adapters.md#4.2%20The%20operation%20table)) — for a `READ`
the count asked plus its header, for a compound the bound the reply budget
already computes. Once its arguments are decoded, a request **MUST** reserve its
reply bound from the reply pool before the service is called. The service and
the engine fill that buffer and add no copy of their own
([RFC 17 §4.5](rfc-17-vfs.md#4.5%20No%20copies%20on%20the%20data%20path)).

A reply that fits the small class needs no wait: it is charged with the receive
reservation, as part of the request's overhead, and that reservation is then
held until the writer has sent or discarded the reply (§3.2), so a small reply
queued to a client that does not read stays charged. A larger reply waits for its
reservation, in the same turns as receive (§4.1), and the wait ends at the
call's deadline with `ErrDelay` naming `DelayResources`.

The reservation is released when the writer has sent the reply, or discarded it
with its connection
([RFC 20 §6.2](rfc-20-adapters.md#6.2%20The%20writer)). A reply queued at a
writer whose client does not read holds its reservation until the write
deadline closes the connection
([RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits)); that is
the slowest way a reply is freed, and it needs no other reservation.

Receive and reply are two pools rather than one so that a flood of large writes
cannot leave no room for the reads that would let clients finish, and so that a
reply's wait never waits on a receive reservation (§2.6).

### 3.4 The per-connection cap for protocols without flow control

On every NFS connection, NFSv4.1's included, and on NFS's auxiliary services
(MOUNT, NLM, NSM) — since the minor version and slot are known only after the
arguments are decoded, and NFSv3 and NFSv4.0 have no flow control of their
own — the per-connection cap of
[RFC 20 §6.3](rfc-20-adapters.md#6.3%20Flow%20control%20is%20the%20protocol%27s%20own)
is **64 requests in flight**, fixed: the same as one NFSv4.1 session's fore
channel ([RFC 21 §3.1](rfc-21-nfs.md#3.1%20Slot%20tables)), so a connection
without sessions is allowed what a connection with them is. A connection at its
cap is not read.

The cap bounds one connection; the receive and reply pools bound the node. At
the transfer class, 64 requests is about 64 MiB per connection, so on an
8 GiB node eight connections at their caps fill the receive pool, and the
turns of §4.1 decide who goes next.

### 3.5 What a transfer size costs

A transfer is charged its whole class, whatever bytes it actually moves, so
[RFC 21 §2.3](rfc-21-nfs.md#2.3%20Transfer%20sizes)'s 1 MiB NFS transfer and
[RFC 22 §3.3](rfc-22-smb.md#3.3%20Sizes%20and%20credits)'s 8 MiB SMB transfer
are what bound how many large requests a node runs:

| Workload on an 8 GiB node | Charge per request | In flight at the ceiling |
| --- | --- | --- |
| NFS `WRITE` of 1 MiB | receive: transfer class and overhead, about 1 MiB | about 500 node-wide |
| NFS `READ` of 1 MiB | reply: transfer class; receive: small | about 500 node-wide |
| SMB `WRITE` of 8 MiB | receive: large class, about 8 MiB | about 64 node-wide |
| SMB `WRITE` of 64 KiB, one credit | receive: directory class and overhead, about 72 KiB | about 7,000 node-wide |
| `GETATTR`, `QUERY_INFO` | small class and overhead, about 12 KiB | about 43,000 node-wide |

Raising a transfer size raises every large request's charge with it, so fewer
fit. That is the cost [RFC 21 §2.3](rfc-21-nfs.md#2.3%20Transfer%20sizes)'s
ponytail names, and the reason a transfer size is a constant of the protocol's
RFC and not a share setting.

> ponytail: a request is charged its whole class, not its bytes, so a 1 MiB
> class buffer charged for a 72 KiB write wastes the difference. Classes keep
> the pool's arithmetic and the buffer allocator the same thing. Add a class
> between 64 KiB and 1 MiB when §8's M1 shows the receive pool filling with
> partly used transfer buffers on a mixed workload.

## 4. Admission and fairness

### 4.1 Across clients: turns at the buffer pools

The receive and reply pools are shared by every client. Whoever waits gets the
next free bytes by **deficit round robin** over **client keys**, the design
[RFC 3 §2.9](rfc-3-syncer.md#2.9%20Workers%20are%20shared%20fairly%20across%20flows)
already uses for transfers:

- **One waiter per connection, one queue per client key.** A connection's reader
  waits for at most one receive reservation at a time, and a request for at most
  one reply reservation, so a key's queue is bounded by its connections and
  their flow-control bounds.
- **Turns in bytes.** When bytes free, the pool visits the next key with a
  waiter, adds a quantum to its deficit, and grants from the head of its queue
  while the head's charge fits. The quantum is the largest class any adapter
  uses, so every waiting key is granted at least once per round.
- **No key holds more than three quarters of a pool while another key waits
  for it.** A key at that cap is skipped until it releases; with no other key
  waiting the cap does not bind, so one busy client on an idle node uses the
  whole pool.
- **The client key is the connection's remote address.** NFS clients open
  several connections, an NFSv4.1 client trunks sessions, an SMB client opens
  channels; counting by connection would give each connection its own turn, and
  a client with sixteen connections sixteen turns.

> decision: clients are told apart by address, not by identity. The receive
> pool is charged before anything authenticates the request, and an address is
> all the frame stage knows. A NAT or a multi-user host behind one address is
> one client to the turns; its users compete within its share. Key by the
> session's client identity at the reply pool, where it is known, if a
> multi-user host is shown starved by its own address's cap while the node has
> room.

### 4.2 Across shares: the share cap at admission

A share that cannot finish its calls — its remote hung, its metadata stalled —
holds buffers for as long as their deadlines allow, and the turns of §4.1 do not
see shares. The service therefore counts, per share, the receive and reply bytes
of the calls it has admitted to that share, and at admission
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)):

- a call whose admission would take its share past **three quarters** of the
  receive pool or of the reply pool — the share's count **plus this call's own
  receive and reply reservations** — **MUST** be refused with `ErrDelay` naming
  `DelayResources`, while that pool is more than half full;
- below half full, the cap does not bind;
- the check and the addition of the call's bytes to the share's count **MUST**
  be one atomic step, so that concurrent admissions to one share cannot each
  pass against the same total;
- the refusal frees the call's buffers at once, and is counted with the share in
  a log line, at most once a minute per share (§10).

The cap is a refusal, not a wait, because the call already holds its receive
reservation: waiting would keep it, and the memory is what the cap protects.

> decision: the share is known only at admission, after the reply wait, so a
> call may wait for reply bytes to its deadline and then be refused by its
> share's cap. It holds only a small-class receive charge meanwhile. Check the
> cap before the reply wait if the adapter is ever told a call's share before
> calling the service.

**How the service learns a call's bytes.** `core` attaches the call's receive
and reply charge to the call's context once, before the service is called, as
it already passes a call's hold bound (`vfs.WithHold`,
[RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)). The front's admission reads it
and adds it to the share's count in the same step that checks the cap:

```go
func WithCharge(ctx context.Context, bytes int64) context.Context
func ChargeOf(ctx context.Context) int64 // 0 for an internal caller no adapter reserved for

// ShareCap is the front's per-share count (§4.2): one per service, built over the
// receive and reply budgets, whose fill it reads. A map entry exists only while a share
// has a call in flight, so a removed share leaves nothing behind.
type ShareCap struct{ /* mu; per share: bytes */ }
func NewShareCap(receive, reply *Budget) *ShareCap
func (c *ShareCap) Admit(ctx context.Context, share uint64, exempt bool) (release func(), err error)
```

The share key is a plain integer, because `resources` imports nothing of the
service's.

> decision: the charge rides in the context because otherwise every `Service`
> method grows a parameter that only admission reads. It is a number the service
> reads, not a policy it applies, so it is not the hold-policy hook RFC 17
> rejected. Make it a field of the call if a second value has to travel the
> same way.

**A call that releases or reclaims state is not refused by the cap.** A call
whose operations only end state — a close, an unlock, a lock cancel, a grant or
layout return, a freed state ID — or reclaim it during grace, with the
sequencing, filehandle and attribute operations that carry them, **MUST** be
admitted past the cap, and its bytes still counted with its share. Steps 1–4 of
[RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees) — the
share's state, its flavour and client rules, resolution — and authorisation
apply to it unchanged; only the memory check is skipped. A compound that also
carries any other operation is admitted against the cap as a whole. Such a call
needs only a small reply (§3.3), so no reply wait can refuse it either.

> decision: release and reclaim are exempt from the share cap because refusing
> them keeps the very state the share is short of memory for, and a reclaim
> refused until grace ends is a lost lock. The exemption is worth only what
> those calls hold — one small-class request each, bounded by the receive pool's
> turns. Withdraw it if a release operation is shown to carry a payload, which
> none of the three protocols defines.

`DelayResources` is the one class for every node-pool refusal — the share cap
and the open-state pool, which the service refuses, and a reply reservation's
deadline, which the adapter's pipeline refuses before the call and answers with
the same value. It is never held (`HoldNone`,
[RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)): `NFS4ERR_DELAY` over NFSv4,
`NFS3ERR_JUKEBOX` over NFSv3, `NLM4_DENIED_NOLOCKS` over NLM, and
`STATUS_INSUFFICIENT_RESOURCES` over SMB
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)). Holding it
would spend the hold pool on the very share that is short of memory.

Below the service, every share-wide resource already has its own share rule:
the journal's capacity per share ([RFC 1 §6](rfc-1-journal.md#6.%20Capacity)),
the syncer's per-flow caps and turns
([RFC 3 §2.9](rfc-3-syncer.md#2.9%20Workers%20are%20shared%20fairly%20across%20flows)),
the speculation budget per share
([RFC 8 §7.4](rfc-8-engine.md#7.4%20The%20speculator)), and the work scheduler's
weights ([RFC 8 §6.1.1](rfc-8-engine.md#6.1.1%20The%20shared%20work%20scheduler)).
The share cap is the one that was missing: the request-path memory above them.

![Admission fairness: clients waiting at the receive pool are served in deficit round robin by address; the reply pool past half full refuses a call of the builds share once builds holds three quarters, while a call of profiles is admitted](img/rfc24-fairness.svg)

### 4.3 Held requests: where their bytes go

The SMB adapter counts the requests it holds per connection against a floor,
and past it against the node's hold pool
([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)), whether a request
waits between re-drives or inside the service behind a clone. The service
counts nothing about holds. This RFC fixes which pool holds the bytes:

- **Within its connection's floor**, a held call keeps its receive reservation. The
  floor is a guarantee that the call is held, and its memory stays in the
  receive pool, under the receive pool's turns and caps.
- **Past the floor**, the adapter reserves the call's whole receive charge —
  buffer class and per-request overhead, as charged at frame — from the hold
  pool and, only once that succeeds, releases the receive reservation, so the
  charge is never absent. Failing, it refuses the call, and the receive
  reservation is released with it. The charge never shrinks: the buffer it
  backs is not shrunk by being held, so a 100 KiB write in a transfer-class
  buffer charges the hold pool about 1 MiB.
- The adapter's per-connection floors in requests and bytes count the write's
  bytes, at least 64 KiB; that is a logical count of what a connection may have
  held, and it is kept apart from the hold pool, which counts memory. Past
  both, the adapter refuses the request at once and holds nothing.
- A held call holds no reply reservation between attempts; each re-run takes one
  as a new call would (§3.3).

So held requests can never take the receive pool from other clients beyond the
floors, and the floors themselves are bounded by the receive pool's
three-quarter cap per key.

### 4.4 Connections

**Each connection is charged a fixed cost** to the connection-state pool at
accept — its reader's and writer's state, the reader's small-class read buffer
([RFC 20 §6.8](rfc-20-adapters.md#6.8%20Buffer%20pools%2C%20in%20code)) included, its ID tables when empty, its
security context — proposed at 64 KiB and settled by §8's M2. A connection that
cannot be charged is closed at accept, before any byte is read
([RFC 20 §3.2](rfc-20-adapters.md#3.2%20A%20connection%27s%20lifecycle)).

**The connection limit** of
[RFC 20 §6.4](rfc-20-adapters.md#6.4%20Deadlines%20and%20limits) is the setting
`connection_limit`, per node. Its default is derived, the least of:

- what the connection-state pool can charge: its ceiling over the connection
  cost — 8,192 on an 8 GiB node;
- half the process's open-file limit (§6.3).

A value set above either is refused at start, naming the bound it passes.

**No address holds more than a quarter of the connection limit.** A connection
from an address already holding a quarter is closed at accept, counted under its
own reason (§10).

> decision: a quarter per address. A multi-session desktop host or a build
> farm's NAT legitimately opens many connections, but no one address needs more
> than a quarter of a node, and without a per-address bound one client in a
> reconnect loop takes every connection. Raise it when a named deployment shows
> one address needing more of one node; never remove it.

### 4.5 Sessions and reply caches

An NFSv4.1 session is charged, at `CREATE_SESSION`, to the connection-state
pool:

- a fixed **session cost**, for the session object and its channel state;
- a fixed **slot cost** per slot of each channel, for its slot-table entry,
  whatever reply it caches;
- each channel's reply cache — slots times the cached reply size, at most
  64 × 8 KiB = 512 KiB on the fore channel
  ([RFC 21 §3.1](rfc-21-nfs.md#3.1%20Slot%20tables)).

The two fixed costs are proposed at 4 KiB and 256 bytes and settled by §8's M2.
They are charged however small a cache the client asks for, so a session that
caches nothing still costs its slots. Whether the session fits is decided on
the sum of all three. Where the pool cannot charge the slots asked for,
the session is granted the slots it can charge, at least one; where it cannot
charge one, `CREATE_SESSION` is answered `NFS4ERR_DELAY`. While the pool is
above seven eighths of its ceiling, the adapter **SHOULD** lower each session's
target slot count by half, as
[RFC 21 §3.1](rfc-21-nfs.md#3.1%20Slot%20tables) allows under memory pressure,
and release the charge of every slot the client retires.

An SMB session is charged the same session cost at setup, beside its
connection's;
its credits cost nothing here, because what they let a client send is charged to
the receive pool as it arrives.

### 4.6 Open state

Opens, byte-range locks, lock waiters, caching grants and quota reservations at
a primary are each charged a fixed per-kind cost to the open-state pool when
created, and released when they end. The costs are proposed at 1 KiB per open
and 256 bytes per lock range, waiter, grant or reservation, settled by §8's M6.

The open-state table ([RFC 14](rfc-14-open-state.md)) is the pool's one owner:
it takes the open-state `*Budget` in its `Deps`, charges at each create and
releases at each end. **(cluster)** The route envelope's dedup table takes the
same budget in its own `Deps`
([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)) and is charged
per entry.

At the ceiling:

- a call that would create an open, a lock or a lock waiter is refused with
  `ErrDelay` naming `DelayResources`, before it changes anything;
- an open that would be offered a caching grant is answered without one, as at
  the per-client grant budget
  ([RFC 14 §5.5](rfc-14-open-state.md#5.5%20A%20client%27s%20grants%20are%20bounded));
- a close, unlock, grant return, cancel or expiry is **never** refused: each
  frees the pool;
- a **reclaim** during grace may take the pool past its ceiling, up to a
  **reclaim allowance** of one more ceiling — the pool at twice its share, the
  excess taken from the unpooled quarter. A reclaim of state a durable record
  holds — a durable or persistent open and its locks
  ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)) — is never
  refused, since the records bound it. A reclaim of volatile state that would
  pass the allowance **MUST** be refused with `ErrDelay` naming
  `DelayResources` before it changes anything, and is counted; the client
  retries it within grace, as it retries any `ErrDelay`. The pool is over its
  ceiling until the restored state ends, and new state waits for it.

> decision: open state is charged by count of kinds, not by bytes measured, and
> a reclaim is bounded by an allowance rather than proved. Volatile opens and
> lock ranges leave no durable count behind
> ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)): a reclaim is
> checked against the client's eligibility and its current permissions
> ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)),
> not against what it held, so an eligible client could claim more than it had.
> Twice the ceiling covers a restart that halves *L* with every reclaim honoured.
> The ceiling of this rule: one eligible client claiming state it never held can
> spend the allowance and leave another's reclaims refused until it closes.
> Charge each client's reclaims against a durable per-client count in RFC 14's
> client record if a client is shown doing that, or a restart is shown cutting
> *L* by more than half.

## 5. Backpressure

### 5.1 No queue is unbounded

Every queue, wait list, table and buffer that grows with load **MUST** have a
stated bound, and a link at its bound **MUST** either wait, holding nothing
later in §2.6's order, or refuse with a named cause. No component may add a
queue without adding its row here, or to the RFC that owns it with a citation
from here. The inventory, from the socket down:

| Queue or wait | Bound | Owner | At the bound |
| --- | --- | --- | --- |
| listen backlog | the system's maximum | [RFC 20 §6.7](rfc-20-adapters.md#6.7%20TCP) | the kernel refuses |
| connections | `connection_limit`, a quarter per address | §4.4 | closed at accept |
| socket receive buffer | the kernel's autotuning | [RFC 20 §6.7](rfc-20-adapters.md#6.7%20TCP) | TCP window closes |
| requests read on one connection | its flow control: slots, credits, 64 | [RFC 20 §6.3](rfc-20-adapters.md#6.3%20Flow%20control%20is%20the%20protocol%27s%20own), §3.4 | not read |
| readers waiting for receive bytes | one per connection | §3.2, §4.1 | turns |
| requests waiting for reply bytes | the requests admitted | §3.3 | the call's deadline, then `DelayResources` |
| the writer's queue | the connection's flow-control bound | [RFC 20 §6.2](rfc-20-adapters.md#6.2%20The%20writer) | the reader stops; the write deadline closes |
| datagram listeners (the port mapper, NLM and NSM over UDP) | a fixed queue | [RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states) | dropped; the client retries |
| held SMB requests | the floor per connection, the hold pool | [RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests), §4.3 | `STATUS_INSUFFICIENT_RESOURCES`, from the adapter |
| blocked lock waiters | the open-state pool | [RFC 14](rfc-14-open-state.md), §4.6 | `DelayResources` |
| callbacks to clients | the writer's queue, the callback deadline | [RFC 20 §3.3](rfc-20-adapters.md#3.3%20Callbacks%20are%20the%20adapter%27s%20to%20deliver) | the client is not answering |
| access-audit and quota events | rings of 16,384 and 4,096 entries of at most 1 KiB, allocated at start (§2.2) | [RFC 25 §4.4](rfc-25-observability.md#4.4%20Bounds%2C%20drops%20and%20gap%20records) | dropped, counted, a gap record written |
| a write paced before capacity | the caller's deadline | [RFC 8 §10.2.1](rfc-8-engine.md#10.2.1%20Writes%20are%20paced%20before%20the%20limit%2C%20not%20stopped%20at%20it) | `ErrDelay` |
| group commit | `G` files | [RFC 8 §5.2](rfc-8-engine.md#5.2%20Group%20commit%20is%20bounded%2C%20and%20retries%20only%20the%20files%20that%20conflict) | the next group |
| background work items | one entry per key, and a fixed total per scheduler instance (below) | [RFC 8 §6.1.1](rfc-8-engine.md#6.1.1%20The%20shared%20work%20scheduler) | coalesced; past the total, the hint is dropped, counted, and re-derived from durable records |
| transfers waiting for a worker | four times the pool per flow and class, eight times per half | [RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed) | refused |
| staging reservations | the staging budget, `backups.spill_max` | [RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound) | waits, holding no worker |
| background CPU | three quarters of the cores (§6.2) | §6.2 | waits, holding its worker |
| **(cluster)** a forwarded call's stream | the call's own reservation | §5.4 | the stream's window closes |

A queue whose entries hold a reference rather than bytes — a transfer waiting
for a worker, a work item — is bounded by count, and its memory is in the
unpooled quarter. A queue whose entries hold bytes is bounded by a pool.

**The work scheduler's entries have a total.** One entry per key bounds
duplicates, not keys: a mass removal or a GC backlog posts one entry for each of
millions of distinct keys. Each instance of
[RFC 8 §6.1.1](rfc-8-engine.md#6.1.1%20The%20shared%20work%20scheduler)'s
scheduler **MUST** hold at most a fixed number of pending entries, proposed at
262,144 and settled by §8's M8. A post that finds the instance full and its key
absent is dropped, never blocked, and counted
(`work_items_dropped_total{source}`, §10). That is safe because every item is a
hint about durable state its source already holds: the source **MUST** re-derive
dropped work from those records within a bound of its own — offload by its age
tick, removals and GC by re-reading their indexes and cursors once the instance
has room — so a dropped item costs latency, never work. The instances are
bounded by count too: one per journal for the engine, and one per process for
GC, whose sources serve every GC partition the node leases
([RFC 9 §7.1](rfc-9-gc.md#7.1%20GC%20bounds%20its%20own%20work)), so a node
holds at most its journals plus one times the total.

> ponytail: a dropped item waits for its source's next re-derivation, so a
> backlog past the total drains at the rate the source re-reads its records,
> not at the scheduler's. Raise the total, or charge entries to a pool, when §8's
> M8 shows a source's re-derivation, not its workers, limiting how fast a
> backlog drains.

### 5.2 The chain from the socket to the remote

![The backpressure chain: client socket, frame and receive pool, admission and flow control, the service's share cap and hold pool, the engine's pacing and capacity, the syncer's workers, and the remote store; each link either waits or refuses, and the client sees a closed TCP window, a paced acknowledgement or a retry-later answer, never an unbounded queue](img/rfc24-chain.svg)

When the remote tier slows, every link from it back to the client is already
bounded, so the slowdown arrives at the client in one of three forms:

1. **The remote slows.** Transfers hold their workers longer; the syncer's pools
   saturate and its queues fill to their bounds
   ([RFC 3 §2.3](rfc-3-syncer.md#2.3%20Backpressure%20propagates%3B%20it%20does%20not%20buffer)).
2. **Offload stalls**, so dirty extents stay in the journal; its share and
   device capacity rise, and writes are **paced**, then refused retry-later
   ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)).
   The client sees a slower acknowledgement, then a retry-later answer.
3. **Cold reads wait** for fetches, holding reply reservations until their
   deadlines. The share cap refuses the share's further calls once it holds three
   quarters of a buffer pool (§4.2). The client sees a retry-later answer.
4. **Requests in flight hold receive bytes** while they wait, so the receive pool
   fills; readers stop reading their connections (§3.2). The client sees TCP's
   window close.
5. **SMB requests held** for capacity or a stall take their floors, then the
   hold pool, then are refused (§4.3). The client sees `STATUS_PENDING`, then
   `STATUS_INSUFFICIENT_RESOURCES`.

At no link does the node accept work it has no room for in order to answer it
later. That is RFC 3's chain extended from the journal to the socket.

### 5.3 Waiting or refusing

Which links wait and which refuse follows one rule: **a link waits when what
it would hold is not yet held, and refuses when it is.** A reader that has not
read the request waits, holding nothing; a call that already holds its buffers
and would hold more refuses, freeing them. A wait is chosen where the client
pays only time, a refusal where waiting would keep memory another client needs.

| Link | Waits or refuses | Because |
| --- | --- | --- |
| receive pool | waits | the request is still in the socket |
| reply pool | waits, to the deadline | the request is admitted, and its reply is the next thing it needs |
| share cap | refuses | the call already holds the share's memory |
| open state | refuses | creating state is the thing that would exceed the pool |
| hold pool | refuses | [RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests), §4.3 |
| journal capacity | paces, then refuses | [RFC 8 §10.2.1](rfc-8-engine.md#10.2.1%20Writes%20are%20paced%20before%20the%20limit%2C%20not%20stopped%20at%20it) |
| transfers | waits, then refuses a full queue | [RFC 3 §2.9](rfc-3-syncer.md#2.9%20Workers%20are%20shared%20fairly%20across%20flows) |

### 5.4 Forwarding **(cluster)**

A `protocol` node that forwards a call to its primary
([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)) streams its
payload. Its in-flight bytes are bounded as follows, answering
[RFC 15 §11](rfc-15-topology.md#11.%20Open%20questions)'s first question:

- at the forwarding node, the payload is the request's receive buffer, already
  reserved at frame (§3.2); the stream adds no buffer of its own beyond one
  chunk in flight, charged to the same reservation;
- at the primary, a forwarded call is charged to the primary's receive and reply
  pools as any call is, under the client key of the original client the envelope
  carries, not the forwarding node's — otherwise every client of one protocol
  node would be one client to the primary's turns;
- a stream whose receiver cannot reserve closes its window, which reaches the
  forwarding node as a slow call, and through it the client.

([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile):
binds only once a second storage node can serve a share.)

## 6. Concurrency

### 6.1 Goroutines

Every goroutine the process runs is one of these, and no other:

| Goroutine | Count bound |
| --- | --- |
| a connection's reader and writer | two per connection: `connection_limit` × 2 |
| an admitted request | the receive pool over the smallest charge (§3.1) |
| a pool worker — syncer halves, the work scheduler's runs, GC's | the pools' sizes ([RFC 3](rfc-3-syncer.md), [RFC 8 §6.1.1](rfc-8-engine.md#6.1.1%20The%20shared%20work%20scheduler)) |
| a fixed loop — listeners, a journal's sync and seal loops, timers, health, each sidecar's loop ([RFC 20 §3.5](rfc-20-adapters.md#3.5%20Sidecars)) | a constant per component, or per journal |
| an outbound NLM callback connection's reader and writer | two per host with a grant or reply to send ([RFC 20 §3.3](rfc-20-adapters.md#3.3%20Callbacks%20are%20the%20adapter%27s%20to%20deliver)), closed after; counted in `connection_limit` |

A held request, a blocked lock, a queued transfer, a work item, a waiting reader
and a pending callback **MUST NOT** have a goroutine of their own: each is a
record and a timer, and is run by one of the goroutines above when its turn
comes ([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests),
[RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). A component **MUST NOT** start a
goroutine per item of work whose count grows with load.

There is no worker pool on the request path
([RFC 20 §6.6](rfc-20-adapters.md#6.6%20Throughput%20and%20latency)'s ponytail):
the receive pool bounds how many requests run, so a pool of goroutines would
bound nothing more.

### 6.2 CPU

The runtime's parallelism **MUST** be the process's CPU quota — the container's
CPU limit where one is set, rounded up, else the host's cores — learned at start
as *L* is.

Hashing, compression, encryption, signing and checksums run on the goroutine
that needs them, with no pool of their own. They are bounded by the request
count on the request path, and by the pools' workers below it. One more limit
applies to **background** CPU work — offload's encoding, relocation, a copying
backup's re-encoding, repack's checksums, GC's verification:

- at most **three quarters of the runtime's parallelism**, and at least one,
  may be inside background CPU work at once; a worker takes one **CPU token**
  for the encode or hash of one chunk and returns it before its next network
  wait;
- a worker waiting for a token holds its worker and nothing else; tokens are
  granted in arrival order;
- request-path CPU — a demand read's verification and decoding, a client
  message's signing and encryption — takes no token.

So offload's 128 upload workers, each encoding a chunk at once on an
eight-core node, cannot take every core from clients' requests. The token is not
a second limiter on transfers in [RFC 3 §2.1](rfc-3-syncer.md#2.1%20A%20worker%20pool%20is%20the%20only%20concurrency%20control)'s
sense: it bounds the CPU time of a transfer's encode, not how many transfers run,
and on a node whose link is the bottleneck no token is ever waited for.

> decision: a quarter of the cores is kept for the request path, by holding back
> background work, not by reserving cores. The scheduler gives the rest to
> whoever runs. Move the fraction when §8's M5 shows foreground p99 latency
> rising under full offload with tokens never waited for, or offload throughput
> limited by tokens while foreground cores idle.

### 6.3 File descriptors

At start the node **MUST** raise its soft open-file limit to its hard limit, and
divide the result:

| Share of the open-file limit | Holds |
| --- | --- |
| a quarter | journal segment descriptors, `DescriptorLimit` ([RFC 1 §7.4](rfc-1-journal.md#7.4%20Open%20descriptors)) |
| a half | client connections: the ceiling on `connection_limit` (§4.4) |
| a quarter | everything else: listeners, the metadata store's files, remote-store connections, one per transfer worker, logs |

If the last quarter is smaller than `upload_workers` plus `fetch_workers`
plus 256, the node starts and raises the health condition `fd_limit_low`
naming the limit and the figure it needs. A descriptor refused by the system
anyway is a failed accept or a failed transfer, counted, never a crash.

## 7. Composing the storage bounds

### 7.1 The journal's index

[RFC 1 §4.2](rfc-1-journal.md#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)
bounds each journal's index by `ExtentLimit` entries of about 51 bytes. This RFC
gives the index pool's ceiling to the node's journals in proportion to each
journal's maximum footprint
([RFC 1 §6](rfc-1-journal.md#6.%20Capacity)):

- `ExtentLimit`'s default is the journal's part of the index pool divided by the
  entry cost and by nine eighths, so the bound and its excess together fit: on a
  64 GiB node with one journal, 8 GiB, about 150 million entries; on an 8 GiB
  node, about 19 million;
- an `ExtentLimit` set so that the node's journals together exceed the pool —
  the bound plus its eighth of excess
  ([RFC 1 §4.2](rfc-1-journal.md#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)),
  times the entry cost, summed over journals — **MUST** be refused at start,
  naming the journals and the pool.

The index's own refusal, answered retry-later, is its ceiling rule; nothing here
adds a second one.

### 7.2 The syncer and staging

[RFC 3 §2.2](rfc-3-syncer.md#2.2%20The%20pool%20size%20is%20a%20memory%20bound)
gives the syncer's peak memory from its two worker counts, and the staging
budget from the fetcher's per-flow cap. The transfers pool holds both:

- when `upload_workers` and `fetch_workers` are unset, each defaults to
  [RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)'s
  128, reduced in equal proportion, if needed, until both halves' peak and the
  staging budget fit the pool, and to at least eight each. At about 2 MiB per
  upload worker and twice that per fetch worker, 128 of each and their staging
  come to about 1.25 GiB, so on an 8 GiB node each defaults to about 100;
- a count set explicitly whose total does not fit **MUST** be refused at start,
  naming the pool, *L* and the figure the counts imply, as
  [RFC 8 §2.4](rfc-8-engine.md#2.4%20Settings%20are%20validated%20once%2C%20and%20refused%20rather%20than%20replaced)
  refuses an invalid setting rather than replace it;
- the sizing tool ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool))
  prints, beside its counts, whether they fit this node's transfers pool.

The speculation budget
([RFC 8 §7.4](rfc-8-engine.md#7.4%20The%20speculator)) holds no memory of its
own: speculative bytes in flight are fetch workers' chunks, already in the
transfers pool, and the journal's space once filled.

### 7.3 The metadata store

The metadata store's in-process memory — its block and index caches, its write
buffers and memtables, its compaction's working set — **MUST** be sized from the
metadata store pool, one pool for the process however many store instances it
opens, divided equally between them. The store evicts at its ceiling and never
refuses a call for memory. Memory-mapped store files are the page cache's
(§2.2), not the pool's.

There is no read cache of file content in memory. The journal is the read cache,
on its device, with the page cache above it.

> decision: no in-memory content cache. A content cache in the heap competes
> with every pool here for the same *L*, duplicates what the page cache already
> holds of the journal's segments, and is one more thing to invalidate on
> write. Add one, as its own pool, when a read workload on the reference box is
> shown limited by journal reads that the page cache does not absorb.

## 8. Defaults, and what the reference box measures

Each proposed value here is settled on the reference box
([index](rfc-index.md#Test%20tiers)) by one measurement:

| # | Measures | Settles |
| --- | --- | --- |
| M1 | heap per request in flight beyond its buffer, at 1,000 and 10,000 small requests in flight per protocol; the share of transfer-class buffers less than half used on the mixed workload | the per-request overhead, 8 KiB; whether a fourth buffer class is needed |
| M2 | heap per idle connection, per protocol and dialect, with an empty and a typical ID table, with and without transport encryption; heap per NFSv4.1 session and per slot with a zero-size reply cache, and per SMB session | the connection cost, 64 KiB; the session cost, 4 KiB; the slot cost, 256 bytes |
| M3 | the metadata store's hit rate and operation latency against its pool size, at 1/16, 3/16 and 5/16 of *L*, on the many-small-files and the profile workloads | the metadata store's share |
| M4 | resident memory and collector CPU against *L* in the overload soak (§11, R2), at 2 GiB and 16 GiB | the unpooled quarter, and the claim that pools at their ceilings stay under *L* |
| M5 | foreground p99 latency and offload throughput with compressible data, at a quarter, half and three quarters of the cores in background work | the background CPU fraction |
| M6 | heap per open, per lock range, per grant and per waiter, at 10⁵ of each | the open-state costs, 1 KiB and 256 bytes |
| M7 | small-operation rate and 1 MiB throughput on one NFSv3 connection at caps of 32, 64 and 128 | the per-connection cap, 64 |
| M8 | heap per pending work-scheduler entry, and the time to drain a removal of 10⁷ files and a GC backlog of 10⁷ keys at totals of 2¹⁶, 2¹⁸ and 2²⁰ | the work scheduler's total per instance, 262,144 |

A share of *L* that a measurement moves is changed here, for every node; it does
not become a setting (§2.3's ponytail).

## 9. Invariants

| # | Invariant |
| --- | --- |
| R1 | The process memory limit is learned once at start, from the runtime's soft limit, else the container's, else the host's memory, and the runtime is told it. |
| R2 | The pools' ceilings — each pool's larger of its share of *L* and its floor — sum to at most three quarters of *L*; where they sum to more, the node raises a health condition naming that sum. |
| R3 | Memory held on behalf of a request, a connection, a session, open state or a held call is reserved from its pool before it is held, and released on every path; a size-bounded owner stays within the figure it was handed (§7). |
| R4 | Reservations along a request's path are taken in §2.6's order, and no wait is entered while holding a later one. |
| R5 | No queue, wait list or table that grows with load is unbounded; each has a row in §5.1 or in the RFC it cites. |
| R6 | At the receive ceiling the socket is not read; no request is read that cannot be reserved, and a record in several fragments is reserved at its protocol's largest class before its first fragment is read. |
| R7 | No client key holds more than three quarters of the receive or reply pool while another key waits for it, and waiting keys are granted in turns. |
| R8 | While the receive or reply pool is more than half full, no call is admitted that would take its share past three quarters of it, counting the call's own reservations and checked atomically with their charge; it is refused with `DelayResources` at once. A call that only releases or reclaims state is exempt (§4.2). |
| R9 | A held call past its connection's floor holds its bytes in the hold pool, not the receive pool, charged its whole receive reservation — buffer class and overhead — never less. |
| R10 | No address holds more than a quarter of the connection limit. |
| R11 | A close, unlock, grant return, or reclaim of durable state is never refused for memory; a reclaim of volatile state is refused, retry-later, only once the open-state pool holds twice its ceiling. |
| R12 | No held request, blocked lock, queued transfer, work item or waiting reader has a goroutine of its own. |
| R13 | Background CPU work holds at most three quarters of the runtime's parallelism at once; request-path CPU takes no token. |
| R14 | A configured worker count or index bound that does not fit its pool is refused at start, never reduced silently. |

## 10. Observability

Metric names are shown without the `dittofs_` prefix the exporter adds; names,
labels and histogram buckets follow
[RFC 25 §2](rfc-25-observability.md#2.%20Metrics).

| Answers | Metric | Type |
| --- | --- | --- |
| What limit is the node running under, and where did it come from? | `memory_limit_bytes{source}` | gauge |
| How full is each pool, against its ceiling? | `memory_pool_reserved_bytes{pool}`, `memory_pool_ceiling_bytes{pool}` | gauge, gauge |
| How often, and how long, does a reservation wait? | `memory_pool_waits_total{pool}`, `memory_pool_wait_seconds{pool}` | counter, histogram (`request`) |
| How often is a call refused for memory, and by which rule? | `memory_pool_refusals_total{pool, rule}` — `rule` one of `share_cap`, `ceiling`, `deadline` | counter |
| How close is the heap to the limit? | `memory_heap_live_bytes`, `memory_resident_bytes` | gauge, gauge |
| Is background work dropping hints for want of room? | `work_items_dropped_total{source}` | counter |
| Is background work waiting for CPU? | `cpu_background_tokens_in_use`, `cpu_background_waits_total` | gauge, counter |
| How many descriptors are open, against the limit? | `fd_open`, `fd_open_limit` | gauge, gauge |

Connections closed at accept for a pool, the limit or the per-address quarter
are `adapter_connection_closes_total{reason}`
([RFC 20 §10](rfc-20-adapters.md#10.%20Observability)), with the reasons
`pool`, `limit` and `per_address`. Held-request metrics are the adapters'
([RFC 20 §10.1](rfc-20-adapters.md#10.1%20Metrics)).

No metric carries a share or client label. A share refused by its cap, and an
address at its connection quarter, are named in a log line at warning, at most
once a minute each, so the noisy share or client is found without a label per
share.

Health conditions: `memory_limit_below_floors`, when the ceilings' sum passes
three quarters of *L* (§2.4), `fd_limit_low` (§6.3),
and `memory_near_limit` while resident memory stays above fifteen sixteenths of
*L* for a minute — which, with every pool within its ceiling, means memory
outside the pools has grown, and is a bug.

## 11. Conformance

The checks below are evidence for the requirements that fail silently. They run
at the tiers [RFC 29 §4.3](rfc-29-test-harness.md#4.3%20Tiers) names; every one
runs on the composed node, against a real journal and a remote that can be
slowed and stopped, because a fake store that always answers at once cannot fill
a pool.

| Check | Does | Catches |
| --- | --- | --- |
| R1 limit learned | Start in a container limited to 512 MiB with no runtime limit set: assert *L* is 512 MiB, source `container`, and the runtime's soft limit reads 512 MiB. Set the runtime limit to 256 MiB: assert it wins. | a node sized from the host inside a container, or a runtime never told |
| R2 overload soak *(daily)* | In a 2 GiB container, for an hour: 200 NFS clients writing 1 MiB, the `builds` remote stalled, an SMB share frozen every five minutes, 10⁵ opens, a metadata walk of 10⁷ entries. Assert the process is never killed, resident memory stays under *L*, every pool stays within its ceiling, every request is answered or retried, and the goroutine count stays within §6.1's bounds. Revert one pool's reservation to a no-op: assert the run fails on resident memory. | a pool that does not hold its memory, or memory outside every pool |
| R5 no unbounded queue | Under constant write load, stop the remote for ten minutes: assert the heap plateaus within the first minute and does not grow after it. Insert an unbounded queue at offload's posting in a test build: assert the check fails on the slope. Remove 10⁶ files with the remote stopped: assert the engine's scheduler holds at most its total, `work_items_dropped_total` rises, and every removal completes once the remote returns. | a queue the inventory missed, which grows only while something downstream is stopped |
| R4 no cycle | Fill the reply pool with replies to a client that stops reading, and the receive pool with writes waiting on a stalled journal: assert both drain once the write deadline closes the stalled client, and no reservation waits past its deadline. | a wait entered while holding a later reservation |
| R7 turns | One address with 16 connections at their caps of 1 MiB writes; 20 other addresses sending `GETATTR`: assert each other address's operation is granted within one round, and the busy address holds at most three quarters of the receive pool. | fairness counted per connection, or no cap |
| R8 share cap | `builds`'s remote black-holed, build01 reading cold at depth 64 on four connections, alice-pc writing to `profiles`: assert `builds` holds at most three quarters of the reply pool, build01 is answered `NFS3ERR_JUKEBOX` and `memory_pool_refusals_total{rule="share_cap"}` rises, and alice-pc's p99 write latency stays within twice its unloaded figure. With `builds` one call below its cap, 64 1 MiB reads on new connections arriving at once: assert `builds` still holds at most three quarters. Close an open and unlock a range on `builds`: assert both succeed. | a hung share taking the node; a cap checked before the call's own bytes, or not atomically; a release refused for memory |
| R9 held bytes move | With the receive pool at 64 MiB, freeze `profiles` and hold 300 64 KiB writes on one connection: assert the receive pool holds the floor's 256 and the hold pool the rest. Repeat with 100 KiB writes: assert the hold pool is charged a transfer-class buffer and its overhead per write past the floor. | held writes past the floor kept in the receive pool, or moved at less than the buffer they keep |
| R10 per-address | One address opening connections in a loop: assert it is held at a quarter of the limit and another address still connects. | one client locking out the rest |
| R11 reclaim allowance | Fill the open-state pool to nine tenths, restart with half the *L*, reclaim: assert every reclaim succeeds and a new open is `DelayResources` until the pool falls. Then have one eligible NFSv4.0 client reclaim opens it never held: assert its reclaims are refused `DelayResources` once the pool reaches twice its ceiling, resident memory stays under *L*, and a persistent open's reconnect still succeeds. | a restart that loses locks; a reclaim that takes the node past its limit |
| R13 CPU tokens | Offload compressible data with 128 workers on four cores while a client sends `GETATTR`: assert at most three tokens are held at once, and the `GETATTR` p99 stays within twice its idle figure. | background work taking every core |
| R14 refused at start | Set `upload_workers` to 4096 under a 1 GiB *L*, and an `ExtentLimit` past the index pool: assert each start is refused, naming the pool and the figure. | a setting silently reduced or allowed past memory |

## 12. Open questions

1. **The hold pool in a split deployment.** Resolved: the adapter counts held
   requests at the `protocol` node that decoded them, where their bytes live
   ([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests)), so the count
   and the memory are one budget on every deployment.
2. **Shares that move.** Fixed fractions waste a role's idle pools (§2.3). A
   budget that lends an idle pool's ceiling to a busy one must take it back
   under load, which is eviction by another name; whether a measured workload
   pays for that is open.
3. **Weights.** Every client and share weighs the same. Deficit round robin
   takes a weight per key without other change; who sets it, and how it meets
   RFC 13's scopes, is open until an operator asks.
4. **Kernel pressure signals.** The pools are sized once from *L*. Reading the
   kernel's memory-pressure signal to shrink the metadata store's cache early,
   before the page cache is squeezed, is unmeasured.
5. **Registered memory for RDMA.** If NFS over RDMA or SMB Direct is offered
   ([RFC 21 §14](rfc-21-nfs.md#14.%20Open%20questions)), registered buffers are
   pinned and cannot be shared with the heap's pools; they would be a pool of
   their own, sized by connection, and the buffer classes of §3.1 would be
   theirs.

## Appendix A — prior art

*Non-normative.*

- **The Linux kernel NFS server** — a transport is not read until a thread is
  free and the reply's space can be reserved on its socket (`svc_reserve`), so
  memory pressure becomes a closed TCP window rather than an error: the model
  for §3.2 and §3.3.
- **API-server priority and fairness** in Kubernetes — one shared concurrency
  limit, per-flow queues, fair turns, a borrowing cap, and a full queue refused;
  also the model [RFC 3 §2.9](rfc-3-syncer.md#2.9%20Workers%20are%20shared%20fairly%20across%20flows) took.
- **The Go runtime's soft memory limit** (`GOMEMLIMIT`, Go 1.19) and its
  container-aware `GOMAXPROCS` (Go 1.25) — the runtime mechanisms §2.1 and §6.2
  rely on; neither bounds what a program holds, only how hard the collector
  works.
- **Ceph's OSD memory target** — one memory target per daemon, with its caches
  resized to keep the process under it; DittoFS fixes the split instead of
  autotuning it.
- **Envoy's overload manager** — resource monitors with thresholds that trigger
  named actions: stop accepting connections, shrink buffers, refuse requests.
  §2.3's "at its ceiling" column is the same idea, one action per pool.
- **Windows Server's SMB server** — answers `STATUS_INSUFFICIENT_RESOURCES`
  when asynchronous work would exceed its resources (MS-SMB2 3.3.4.2), which
  §4.2 and RFC 22 use.
- **ZFS's ARC** — a content cache sized as a fraction of memory and shrunk under
  pressure: the content cache §7.3 decides not to have, the page cache over the
  journal taking its place.

## Appendix B — where the current code differs

*Non-normative.* The code on the branch this RFC was written against, by section.

| § | This RFC | The code today |
| --- | --- | --- |
| §2.1 | one limit, from the runtime limit, the container or the host; the runtime told | a system detector reads the cgroup v2 `memory.max` only (no `memory.high`, no cgroup v1), else `/proc/meminfo` or `sysctl`, with a 4 GiB fallback on failure; nothing reads or sets `GOMEMLIMIT` |
| §2.3 | one budget, fixed shares | each consumer sizes itself from the detected memory, independently, and nothing sums them: a per-share read buffer of 1/8 of memory **each** (64 MiB floor), Badger block and index caches of 15% and 7.5% **per store** (clamped to 512–4096 and 256–2048 MiB), and a per-share append-log pressure budget of 1/4 of memory (on disk) |
| §3.1, §3.2 | charged at frame, before the body is read | a three-class `sync.Pool` (4 KiB, 64 KiB, 1 MiB), neither bounded nor charged; NFS reads each record into a pooled buffer before taking its per-connection semaphore, so one record past the cap is held per connection; SMB reads each message into a fresh allocation of up to `max_message_size`, 64 MiB by default, before its semaphore, then copies it into a pooled buffer |
| §3.3 | reply reserved before the call | no reply accounting; NFS replies are copied into a fresh buffer |
| §3.4 | a fixed cap of 64 | `max_requests_per_connection`, 100 by default, for both protocols, SMB included alongside its credits |
| §4.1, §4.2 | turns across addresses, a cap per share | no node-wide in-flight bound and no fairness across clients or shares above the block layer |
| §4.4 | a derived connection limit, a quarter per address | NFS `max_connections` 1024, SMB unlimited by default with a live-settings check at accept; no per-address bound; the soft open-file limit is not raised |
| §4.5 | sessions charged their reply cache | SMB `max_sessions` exists as a setting; NFSv4.1 sessions are not charged |
| §4.6 | open state charged, new state refused at the ceiling | no bound on opens or locks; RFC 14's grant budget not implemented |
| §6.2 | background CPU tokens | `GOMAXPROCS` is the runtime's default (Go 1.26, container-aware); no background CPU limit; fetch parallelism defaults to the larger of 8 and twice the CPU count |
| §7.2 | two fixed worker pools within the transfers share | upload concurrency is adaptive — a goodput controller resizing a semaphore — not a fixed pool; the read buffer runs four prefetch workers per share with a 64-entry request channel |
| §7.3 | no in-memory content cache | a per-share in-memory LRU of blocks, the read buffer above, consulted on every read |
