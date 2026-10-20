# NBD request pipelining — implementation plan

**Goal:** remove the one-command-at-a-time bottleneck in the NBD adapter so that a
single connection can have several commands in flight, converting the per-command
latency floor into throughput.

**Why it matters (measured):** the adapter dispatches commands inline. At queue
depth 16 the server reached 14,027 random-read IOPS against an engine that does
~250,000 IOPS for the same 4 KiB read in-process. So ~94% of the random-read cost
at depth is the serialization, not the storage. A warm cache hit costs ~115 µs on
the wire and ~4 µs of actual work.

**Scope:** `internal/adapter/nbd/` only. No engine, metadata, or control-plane
changes. No new protocol commands. `NBD_FLAG_CAN_MULTI_CONN` stays OFF.

---

## The constraint that shapes everything

`NBD_CMD_WRITE` carries its payload **inline in the stream**, immediately after
the 28-byte header. A reader cannot skip ahead to the next header without
consuming that payload first. This is why the current loop is serial: it reads a
header, then reads exactly `req.Length` bytes, then replies.

So pipelining has to be **split by phase**, not "read headers in a loop":

1. **Reader goroutine** — owns the socket's read side. Reads one request header,
   then (for WRITE) reads its payload, then hands a fully-materialized command to
   a work queue. It never blocks on a handler.
2. **Worker pool** — N goroutines that take commands, do the store work, and hand
   a reply to the writer.
3. **Writer goroutine** — owns the socket's write side. Drains a reply queue.

This preserves stream integrity by construction: payloads are consumed in the
order the client sent them, before the next header is parsed.

## Why the writer must be single and ordered

NBD simple replies are matched to requests by **cookie**, not by position, so
out-of-order replies are legal. But a single writer goroutine is still required:
`conn.Write` is not safe for concurrent callers to interleave, and a reply header
plus its payload must be written as one unit. One writer, one queue, no lock.

Ordering across cookies does not need to be preserved. Two exceptions:

- **FLUSH is a barrier.** `NBD_CMD_FLUSH` must not complete before every WRITE
  that was submitted before it has reached the store. Today this is free because
  dispatch is serialized. With a pool it becomes explicit bookkeeping.
- **FUA must be durable before its own reply.** Already true per-command; it
  stays per-command.

---

## Design

### Concurrency limit

A fixed worker count, defaulting to `runtime.NumCPU()` and capped by a config
field `MaxInFlight` (default 4, max 64). Not unbounded: each in-flight WRITE
holds a payload buffer, so an unbounded pool turns a fast client into an OOM.
`MaxInFlight` also bounds the reply queue.

### Memory bound — the part that bites

A WRITE at `MaxInFlight` depth holds `req.Length` bytes. The advertised
`maxBlockSize` is 32 MiB, so 4 in flight is 128 MiB worst case. This is the
single most dangerous property of the change on a 3.9 GB host, and it needs both
a limit and a measurement.

**Decision:** cap total in-flight payload bytes with a semaphore
(`maxInFlightBytes`, default `MaxInFlight × 1 MiB`, floor of one max-size
request). The reader blocks acquiring bytes *before* allocating the payload
buffer, so a client sending 32 MiB writes cannot make the server allocate more
than the budget. This mirrors the journal's existing backpressure: block the
reader, do not fail the command.

### FLUSH barrier

Each command gets a monotonically increasing sequence number from the reader.
A FLUSH records the sequence of the last WRITE handed to the queue before it,
then waits for all writes with sequence ≤ that to have completed.

Implementation: a counter of outstanding writes plus a broadcast. Simple version
that is easy to reason about:

```go
type barrier struct {
    mu      sync.Mutex
    cond    *sync.Cond
    pending uint64 // writes submitted but not yet committed
}
```

FLUSH: `mu.Lock(); for pending > 0 { cond.Wait() }; mu.Unlock(); then commit`.
Each WRITE decrements `pending` and broadcasts when its store commit returns.

This is coarse — a FLUSH waits for *all* in-flight writes, not just earlier ones —
but with a bounded pool that is at most `MaxInFlight` writes, and it is obviously
correct. The precise version (per-sequence waiting) is a later optimisation if
the coarse one measures badly.

**Careful:** the FLUSH must wait for writes to *complete their commit*, not just
to be dequeued. The decrement belongs immediately before the reply is queued,
after `FlushStableWrite` returns.

### TRIM / WRITE_ZEROES ordering

These mutate the store and must not race a WRITE to an overlapping range. Two
options:

- **A (chosen):** treat them like writes for barrier purposes — they decrement
  the same `pending` counter. Overlapping concurrent writes are then still
  possible, which is the *same* hazard that exists today across two connections.
- **B (rejected):** serialize all mutating commands. Correct, but throws away
  most of the gain, since a filesystem issues TRIM interleaved with writes.

Option A is defensible because NBD gives no ordering guarantee between commands
without a FLUSH, and a client that needs ordering uses FLUSH. Worth stating in
the code, because it looks like an oversight.

### READ and the engine's own concurrency

`engine.Store.ReadAt` is already safe for concurrent use (the in-process probe
did 200 sequential calls at 250k IOPS with no lock contention, and the store is
used concurrently by NFS/SMB today). No engine change needed.

### Deadlines

Today the loop arms a read deadline before every header, and `handleWrite` arms
one before the payload. With a reader goroutine:

- the reader arms the header deadline, and re-arms before the payload read
- the payload read is bounded by `timeouts.Read` (unchanged)
- workers do not touch the read deadline at all
- the writer arms `timeouts.Write` before each reply (unchanged)

The drain-on-shutdown path gets simpler in one way and harder in another: the
reader must stop handing out new work, in-flight work must finish, and replies
must still be written. `initiateShutdown`'s 100 ms read deadline still unblocks
the reader, which then stops and drains.

### Error handling and shutdown

- A reader error (EOF, reset, bad header) stops the reader and closes the work
  queue. Workers drain what is already queued, then exit. The writer finishes
  the replies it has, then exits. `serve` returns the reader's error.
- A worker error that is a *connection* error (write failed) cancels everything.
  A worker error that is a *command* error (EIO, ENOSPC) is just a reply — it must
  not tear down the connection.
- Panic in a worker: recover, log, and reply `NBD_EIO` for that cookie rather than
  killing the process. Today the connection-level recover covers the whole loop;
  with a pool it has to move into the worker.

---

## Files

| File | Change |
|---|---|
| `internal/adapter/nbd/connection.go` | split `serve` into reader/worker/writer; add `barrier` |
| `internal/adapter/nbd/pipeline.go` | **new** — the pool, queues, semaphore, barrier |
| `internal/adapter/nbd/handlers_*.go` | handlers stop taking `conn`; they return a reply value |
| `internal/adapter/nbd/config.go` | add `MaxInFlight`, `MaxInFlightBytes` |
| `internal/adapter/nbd/pipeline_test.go` | **new** — ordering, barrier, backpressure, panic |
| `internal/adapter/nbd/connection_test.go` | existing tests keep working (pool size 1 == today) |

### The handler signature change

Today: `func (h *Handler) handleRead(ctx, bs, exp, req, conn) error` — it writes
to `conn` itself. With a pool the handler must not touch the socket:

```go
type reply struct {
    cookie  uint64
    nbdErr  NBDError
    payload []byte
}
func (h *Handler) handleRead(ctx, bs, exp, req) reply
```

The payload buffer ownership passes to the writer, which is where the semaphore
release happens. This is the largest mechanical change and it touches every
handler, but each one is a small, local edit.

---

## Testing

The existing suite must pass unchanged with `MaxInFlight: 1` — that is the
regression gate, and it is why the config default is a *separate* concern from
the correctness of the change.

New tests, each of which must be shown to fail before the fix:

1. **`TestPipeline_WritePayloadIsConsumedInOrder`** — send WRITE(4k), WRITE(4k),
   READ; assert all three replies arrive with the right cookies and the read
   returns what the second write put there. Catches a reader that parses a header
   before draining a payload.
2. **`TestPipeline_FlushIsABarrier`** — issue a slow WRITE (store fake with a
   delay) then FLUSH; assert the FLUSH reply is not written until the WRITE's
   commit has returned. **This is the test that must fail on a naive pool.**
3. **`TestPipeline_BackpressureBoundsMemory`** — `MaxInFlightBytes` = 64 KiB,
   send 8 × 32 KiB writes; assert peak allocation stays under the budget and the
   commands still complete (blocked, not refused).
4. **`TestPipeline_WorkerPanicRepliesEIO`** — store fake panics; assert the
   connection survives and that cookie gets `NBD_EIO`.
5. **`TestPipeline_ShutdownDrainsInFlight`** — cancel during a slow write; assert
   the in-flight command's reply is written and the loop returns cleanly.
6. **`TestPipeline_ConcurrentReadsAreConsistent`** — 32 concurrent reads of known
   distinct ranges; assert every reply matches its own cookie's range.

Test 2 is the load-bearing one: it is the property the serial loop gave for free,
and it is the one a plausible-looking implementation gets wrong.

---

## Measurement (the point of the whole exercise)

Re-run `bench-fio.sh` at depth 1 and 16, plus the guest benchmark, and compare
against the baseline recorded here:

| Workload | Baseline (serial) |
|---|---|
| Random 4K read, depth 1 | 3,609 IOPS |
| Random 4K read, depth 16 | 14,027 IOPS |
| Sequential read 64 MiB | 35–52 MiB/s |
| Sequential write 64 MiB | 169 MiB/s |
| fsync per 4K write | 208 IOPS |

**Success criterion:** depth-16 random read should move by more than the depth-1
number, since that is where serialization is the binding constraint. If both move
by the same amount, the change bought nothing and the real bottleneck is
elsewhere — which is worth knowing before touching anything else.

**Honest expectation:** sequential reads are capped by the 80 MiB/s device, so
pipelining will barely move them. The gain should appear in random I/O and in
fsync-heavy workloads. If it does not, the next thing to check is whether the
store path has its own internal serialization.

---

## Risks

| Risk | Mitigation |
|---|---|
| Memory blowup from buffered payloads | `MaxInFlightBytes` semaphore, measured in test 3 |
| FLUSH no longer a barrier | test 2, which fails without explicit bookkeeping |
| Stream desync from a header read before a payload drain | test 1 |
| Panic in a worker killing the process | test 4, recover moved into the worker |
| Subtle regression in the existing suite | `MaxInFlight: 1` must reproduce today's behaviour exactly |
| Shutdown hanging on in-flight work | test 5, plus the existing drain-deadline backstop |

## Order of work

1. Extract the `reply` type and change handler signatures, keeping the loop
   serial. Suite must pass unchanged. *(Mechanical, no behaviour change.)*
2. Add the pipeline with `MaxInFlight: 1` hardcoded. Suite must still pass.
3. Add the FLUSH barrier + test 2. Prove test 2 fails without it.
4. Raise `MaxInFlight`, add the byte semaphore + test 3.
5. Worker panic recovery + test 4.
6. Benchmarks.

Steps 1–2 are the risky ones for regressions and should be separate commits.
