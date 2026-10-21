package nbd

import (
	"context"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// pipeline owns one connection's three roles.
//
// They used to be one role: a single loop read a header, handled the command,
// and wrote the reply, so a connection could never have more than one command in
// flight. That is the measured ceiling — depth 1 and depth 8 read the same
// number of IOPS — because the client can queue work faster than one goroutine
// can retire it.
//
// Splitting the roles is what makes concurrency possible, and the split is not
// optional even at one worker:
//
//   - The reader is the only goroutine that advances the stream. A WRITE's
//     payload is inline, so two readers would consume each other's bytes.
//   - The workers call handlers and never touch the connection.
//   - The writer is the only goroutine that writes. Two writers would interleave
//     a header and its payload, and the client would read one reply's bytes as
//     the other's.
//
// maxInFlight == 1 reproduces the serial loop exactly, which is what makes it
// the regression gate for this change: the existing suite runs against it
// unchanged, so any behaviour difference is a refactor bug and not a tuning
// question.
type pipeline struct {
	h    *Handler
	conn net.Conn
	bs   BlockStore

	// exp is the export as of the handshake, owned by the reader. Workers never
	// read it: the reader stamps each command with the export that was current
	// when that command's header was read, so a size refresh cannot race a
	// handler. See command.exp.
	exp *Export

	ctx         context.Context
	maxInFlight int

	// maxInFlightBytes bounds the payload memory the connection may hold at
	// once, in both directions. Zero means unbounded, which is only ever right
	// for a test that is asserting something else.
	//
	// It is a separate limit from maxInFlight because the two fail differently:
	// too many workers is a latency problem, too many bytes is an OOM. A pool of
	// 4 holding 32 MiB reads is 128 MiB per connection, and the default
	// connection cap is 64.
	maxInFlightBytes int64

	barrier *barrier

	// bytes is the in-flight payload budget, and peakBytes the high-water mark a
	// test reads to prove the budget held.
	bytes     *byteBudget
	peakBytes int64
}

func newPipeline(h *Handler, conn net.Conn, bs BlockStore, exp *Export, ctx context.Context, maxInFlight int) *pipeline {
	if maxInFlight < 1 {
		maxInFlight = 1
	}
	return &pipeline{
		h:                h,
		conn:             conn,
		bs:               bs,
		exp:              exp,
		ctx:              ctx,
		maxInFlight:      maxInFlight,
		maxInFlightBytes: h.maxInFlightBytes,
		barrier:          newBarrier(),
		bytes:            newByteBudget(h.maxInFlightBytes),
	}
}

// command is one request plus everything the reader already settled for it.
//
// exp is stamped per command rather than read from p.exp because the size
// refresh reassigns the export on the reader goroutine, so a worker reading
// p.exp would race it. Stamping is also the right semantics: a command is
// bounds-checked against the size that was current when its header was read,
// which is the size the client could have known.
//
// data is nil for every command that carries no payload, and for a WRITE the
// reader refused before reading one — the refusal rides along in pre.
//
// pre is non-nil when the request was refused before dispatch: a malformed
// WRITE, or any command at all on a draining connection. The worker writes it
// out unchanged; it exists so that a refusal goes through the same single writer
// as every other reply instead of being written inline by the reader.
type command struct {
	req  *Request
	exp  *Export
	data []byte
	pre  *reply

	// bytes is the in-flight payload budget this command holds, released by the
	// writer once the reply is sent. See reply.release for why it is carried
	// rather than derived.
	bytes int64
}

// barrier is the FLUSH ordering guarantee.
//
// Serialized dispatch gave it for free: a command was handled and replied before
// the next was read, so every already-replied write had been committed by the
// time a FLUSH was handled. A pool breaks that argument — a FLUSH can reach a
// worker while a WRITE is still inside the store — so the guarantee becomes
// explicit: FLUSH waits until the count of submitted but uncommitted writes
// reaches zero.
//
// The counter is over WRITE-like commands only (WRITE, TRIM, WRITE_ZEROES),
// because those are the ones whose effects FLUSH promises to make durable. It is
// incremented by the reader before queueing and decremented by the worker after
// the handler returns, so a write that is still queued counts as pending: the
// FLUSH cannot overtake work the client already sent.
type barrier struct {
	mu      sync.Mutex
	cond    *sync.Cond
	pending uint64
}

func newBarrier() *barrier {
	b := &barrier{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// submitted marks one write-like command as in flight. Called by the reader
// before the command is queued, not by the worker: a command sitting in the work
// channel is still a write the client has sent, and a FLUSH must not pass it.
func (b *barrier) submitted() {
	b.mu.Lock()
	b.pending++
	b.mu.Unlock()
}

// committed marks one write-like command as finished. Called by the worker after
// the handler returns, whatever the outcome: a write that failed has no effect
// for a later FLUSH to publish, so holding the barrier for it would block the
// client for no reason.
func (b *barrier) committed() {
	b.mu.Lock()
	if b.pending > 0 {
		b.pending--
	}
	if b.pending == 0 {
		b.cond.Broadcast()
	}
	b.mu.Unlock()
}

// wait blocks until every write-like command submitted so far has finished.
//
// Called on the reader goroutine, which is what makes it safe: no new work is
// queued while it blocks, so the count can only fall. Waiting on a worker
// instead would let the reader queue more writes and starve the wait.
func (b *barrier) wait() {
	b.mu.Lock()
	for b.pending > 0 {
		b.cond.Wait()
	}
	b.mu.Unlock()
}

// byteBudget bounds the payload memory in flight on one connection.
//
// A mutex and a condition variable rather than a channel: a channel of empty
// structs cannot express "N bytes held", and a channel of int64 would have to be
// refilled on every release. The pattern is the same as barrier's, deliberately.
//
// Acquiring blocks instead of failing. A refused WRITE is a data-loss bug; a
// delayed one is only slow, and the client is already applying backpressure of
// its own by not sending the next request until this one is answered.
type byteBudget struct {
	mu    sync.Mutex
	cond  *sync.Cond
	held  int64
	limit int64 // 0 means unbounded
	peak  int64
}

func newByteBudget(limit int64) *byteBudget {
	b := &byteBudget{limit: limit}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire blocks until n bytes fit, then holds them.
//
// It must be called BEFORE the bytes are allocated or read, never after: the
// point of the budget is to bound what the reader has already committed to
// buffering, and a gate consulted after the allocation bounds nothing.
//
// ctx is polled by a goroutine rather than selected on, because sync.Cond has no
// context support. The goroutine is created only when the acquire actually has
// to wait, so the common uncontended case costs one mutex and no goroutine.
func (b *byteBudget) acquire(ctx context.Context, n int64) error {
	if b == nil || b.limit <= 0 || n <= 0 {
		return nil
	}

	b.mu.Lock()
	if b.held+n <= b.limit {
		b.take(n)
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	// Slow path: wait, but stay cancellable. Without this a cancelled connection
	// with a full budget would park the reader forever, because nothing will
	// release bytes it is not going to write.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			b.cond.Broadcast()
			b.mu.Unlock()
		case <-stop:
		}
	}()

	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if b.held+n <= b.limit {
			b.take(n)
			return nil
		}
		b.cond.Wait()
	}
}

// take records n bytes as held. The caller holds b.mu.
func (b *byteBudget) take(n int64) {
	b.held += n
	if b.held > b.peak {
		b.peak = b.held
	}
}

// release gives n bytes back and wakes anyone waiting.
//
// Every acquire needs a release on every path, including the ones that never
// reach the writer — a reader error, a drained refusal, a contained panic. A
// leaked budget is not a slow connection: it is a connection that wedges at zero
// bytes the moment the leak reaches the limit.
func (b *byteBudget) release(n int64) {
	if b == nil || b.limit <= 0 || n <= 0 {
		return
	}
	b.mu.Lock()
	b.held -= n
	if b.held < 0 {
		b.held = 0
	}
	b.cond.Broadcast()
	b.mu.Unlock()
}

// highWater returns the peak bytes held, for a test asserting the budget held.
func (b *byteBudget) highWater() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

// run reads commands, dispatches them, and writes replies until the client
// disconnects, the context is cancelled, or the stream fails.
//
// It returns the error that ended the connection, or nil for a clean
// NBD_CMD_DISC. The reader runs on the calling goroutine; the workers and the
// writer are spawned here and joined before run returns, so no goroutine outlives
// the connection.
func (p *pipeline) run() error {
	// Derived, not the connection's own context: the writer cancels it when the
	// socket fails, so workers stop calling into a store whose replies have
	// nowhere to go.
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()

	// Buffered at maxInFlight so the reader can queue ahead of the workers by
	// exactly the amount the pool can absorb. That buffer IS the backpressure:
	// a full channel blocks the reader, which stops reading headers, which makes
	// the client's own send buffer fill. Sizing it larger would only move the
	// memory bound without raising throughput.
	work := make(chan command, p.maxInFlight)
	replies := make(chan reply, p.maxInFlight)

	var workers sync.WaitGroup
	for range p.maxInFlight {
		workers.Add(1)
		go func() {
			defer workers.Done()
			p.work(ctx, work, replies)
		}()
	}

	writeDone := make(chan error, 1)
	go func() { writeDone <- p.writeLoop(replies) }()

	// Cancellation has to reach a reader parked in ReadRequest, and the only way
	// to interrupt a blocking read on a live socket is the deadline: cancelling a
	// context does not touch the syscall.
	//
	// This is the piece whose absence hung shutdown for the full idle timeout
	// (5 minutes by default). The base adapter's interruptBlockingReads
	// (pkg/adapter/base.go:452) does the same thing on the shutdown path, so
	// production rarely hit it — but a cancelled context with the socket still
	// open did, and that is exactly what a caller of Serve sees when it shuts
	// down by cancelling rather than by closing.
	//
	// The deadline is set to now, not to a budget: this is an unblock, not a
	// grace period. Giving it a budget would hold the connection open past the
	// shutdown window, which is the mistake the draining branch's payload drain
	// is careful not to make. The commands already in flight are unaffected — a
	// read deadline does not touch writes — so they still get their replies,
	// which is the whole point of draining rather than abandoning.
	unblock := make(chan struct{})
	defer close(unblock)
	go func() {
		select {
		case <-ctx.Done():
			_ = p.conn.SetReadDeadline(time.Now())
		case <-unblock:
		}
	}()

	readErr := p.readLoop(ctx, work, cancel)

	// Orderly shutdown: stop accepting, let queued work finish, then close the
	// reply channel so the writer can return. Closing replies before the workers
	// are joined would panic on a send.
	close(work)
	workers.Wait()
	close(replies)

	writeErr := <-writeDone

	// The read error is the one that explains the connection's end: a failed
	// write usually causes the failed read that follows it, not the reverse. A
	// clean DISC returns nil here and the writer's error is the only one left.
	if readErr != nil {
		return readErr
	}
	return writeErr
}

// readLoop is the reader: the only goroutine that advances the stream.
//
// Everything that decides what happens to a command before a handler sees it
// lives here — the idle deadline, the size refresh, DISC, the draining check,
// WRITE validation and the WRITE payload read. That is not an accident of
// layout: each of them either reads the socket or reassigns the export, so each
// is unsafe anywhere else.
func (p *pipeline) readLoop(ctx context.Context, work chan<- command, cancel context.CancelFunc) error {
	// lastResolve is when p.exp was last read from the metadata layer, so the
	// refresh below can bound how stale it gets.
	lastResolve := time.Now()

	for {
		// The read deadline is armed before the read, and deliberately not
		// while draining. initiateShutdown sets a 100 ms deadline on every
		// active connection to unblock pending reads (pkg/adapter/base.go:447),
		// and a fresh SetReadDeadline replaces it — so arming the full request
		// budget here would hold the drain open for that whole budget. Leaving
		// the short deadline in place is what lets an idle connection close
		// inside the drain window.
		if !p.h.draining() {
			// Idle bounds the wait for a header; Read bounds finishing a request
			// that has already started (the payload read below). At the defaults
			// both are 5m, so the distinction only shows for an operator who
			// separates them — which is exactly when the field's own doc has to
			// be true rather than nearly true.
			//
			// decision: the two are separate arms rather than a min(). Idle == 0
			// means "no idle timeout", so a min() would turn an unset Idle into
			// an immediate timeout; the guard has to be independent, which is the
			// shape NFS uses at pkg/adapter/nfs/connection.go:312. A consequence
			// is that an operator who sets Idle > Read gets an idle wait longer
			// than the read budget, and that is the intended reading: Read is a
			// budget for finishing work, not a ceiling on waiting for a client.
			budget := p.h.timeouts.Read
			if p.h.timeouts.Idle > 0 {
				budget = p.h.timeouts.Idle
			}
			if err := p.conn.SetReadDeadline(time.Now().Add(budget)); err != nil {
				return err
			}
		}

		// See Handler.sizeRefresh. Cheap enough to keep the size honest within a
		// second, rare enough that the 4 KiB read path never pays for it.
		//
		// A failed refresh keeps the previous export rather than failing the
		// command: the export was valid a second ago, and a transient registry
		// error should not error the client's device. The bound that buys is "up
		// to one second stale", which is what Export.Size's doc states.
		//
		// p.exp is reassigned here and only here, which is why the reader stamps
		// commands with it and workers never read it.
		if time.Since(lastResolve) >= p.h.sizeRefresh {
			if refreshed, err := p.h.exporter.Resolve(ctx, p.exp.Name); err == nil {
				p.exp = refreshed
			}
			lastResolve = time.Now()
		}

		req, err := ReadRequest(p.conn)
		if err != nil {
			return err
		}

		// DISC is handled before the draining check because it has no reply in
		// any state: "There is no reply to an NBD_CMD_DISC." Offset and length
		// MUST be zero; a nonzero one is a malformed request, answered rather
		// than acted on so the connection stays usable.
		if req.Type == CmdDisc {
			if req.Offset != 0 || req.Length != 0 {
				if err := p.submit(work, command{req: req, exp: p.exp,
					pre: &reply{cookie: req.Cookie, nbdErr: ErrEINVAL}}); err != nil {
					return err
				}
				continue
			}
			// Nil, not an error: a client that says goodbye did what the protocol
			// asked. Returning an error here would make every orderly shutdown
			// log a failure.
			return nil
		}

		if p.h.draining() {
			// The request arrived before the read unblocked, so answer it rather
			// than serve it: NBD_ESHUTDOWN is the protocol's "reconnect" signal,
			// which is what the kernel's dead-connection timeout waits for. The
			// connection is not closed here — closing on the first observation
			// makes the client's kernel error the device instead of pausing it,
			// and the base adapter's drain deadline is the backstop.
			if err := p.submit(work, command{req: req, exp: p.exp,
				pre: &reply{cookie: req.Cookie, nbdErr: ErrESHUTDOWN}}); err != nil {
				return err
			}

			// A WRITE's payload is already in flight: the spec requires the
			// client to follow the request header with length bytes whatever the
			// server answers. Replying without consuming them leaves the next
			// header to be read out of payload bytes, which desynchronizes the
			// stream — the connection then dies on a later command with a
			// confusing NBD_EBADMAGIC instead of pausing as intended, which is
			// exactly what this branch exists to prevent. Only WRITE carries a
			// payload, so every other command skips the drain.
			//
			// decision: the drain does NOT arm the Read budget, unlike a refused
			// WRITE below. initiateShutdown has already put a short deadline on
			// the connection to unblock the pending read (pkg/adapter/base.go:452)
			// and that deadline is what bounds this drain too; replacing it with
			// the full request budget would hold the drain open for minutes on a
			// client that announced a payload and stalled.
			if req.Type == CmdWrite {
				if err := p.h.drainWritePayload(p.conn, req.Length); err != nil {
					return err
				}
			}
			continue
		}

		if req.Type == CmdWrite {
			// A refused WRITE still has its payload in flight — "The client MUST
			// follow the request header with length number of bytes" holds
			// whatever the server answers — so the refusal is queued and the
			// payload drained here. Replying and moving on without draining would
			// parse payload bytes as the next header.
			//
			// The refusal goes through the queue rather than straight to the
			// socket because the writer owns the socket: a second writer is how a
			// header and a payload get interleaved.
			//
			// The drain re-arms the Read budget, unlike the draining path above:
			// that path has already had a short shutdown deadline put on the
			// connection, while this one is an ordinary command and the drain is
			// where a hostile or stalled client parks.
			if pre, refused := p.h.validateWrite(p.exp, req); refused {
				if err := p.submit(work, command{req: req, exp: p.exp, pre: &pre}); err != nil {
					return err
				}
				if err := p.conn.SetReadDeadline(time.Now().Add(p.h.timeouts.Read)); err != nil {
					return err
				}
				if err := p.h.drainWritePayload(p.conn, req.Length); err != nil {
					return err
				}
				continue
			}

			// The payload read stays on the reader: this is the only goroutine
			// allowed to advance the stream, and a handler reading here would race
			// it for the same bytes.
			// Acquired BEFORE the read, never after: the budget bounds what the
			// reader has already committed to buffering, and a gate consulted
			// after the allocation bounds nothing.
			if err := p.bytes.acquire(ctx, int64(req.Length)); err != nil {
				return err
			}
			data, err := p.h.readWritePayload(p.conn, req)
			if err != nil {
				// Never queued, so the writer will never release this budget.
				p.bytes.release(int64(req.Length))
				return err
			}
			if err := p.submitWrite(work, command{req: req, exp: p.exp, data: data, bytes: int64(req.Length)}); err != nil {
				return err
			}
			continue
		}

		// READ allocates too, so it takes the budget as well. handleRead does
		// buf := make([]byte, req.Length) on the worker, so a WRITE-only budget
		// would leave maxInFlight concurrent READs free to allocate
		// maxInFlight x 32 MiB with the counter untouched — the same OOM the
		// budget exists to prevent, on the direction a boot disk spends most of
		// its time in.
		if req.Type == CmdRead {
			if err := p.bytes.acquire(ctx, int64(req.Length)); err != nil {
				return err
			}
			if err := p.submit(work, command{req: req, exp: p.exp, bytes: int64(req.Length)}); err != nil {
				p.bytes.release(int64(req.Length))
				return err
			}
			continue
		}

		// FLUSH is a barrier, so it is queued only once every write submitted
		// before it has finished. The wait is on this goroutine on purpose: while
		// it blocks no new command is read, so the pending count can only fall and
		// the wait terminates. Waiting on a worker instead would let the reader
		// keep queueing writes and starve the flush.
		//
		// It must happen BEFORE the queueing, not inside handleFlush: a worker
		// that waited there would deadlock the pool it is part of.
		if req.Type == CmdFlush {
			p.barrier.wait()
		}

		if err := p.submit(work, command{req: req, exp: p.exp}); err != nil {
			return err
		}
	}
}

// recordPeak publishes this pipeline's high-water mark to the handler, so a test
// can assert the budget held after the connection has closed. The atomic max is
// a compare-and-swap loop rather than a plain store: several pipelines may exist
// for one handler over the connection's life, and the largest peak is the one
// that matters.
func (p *pipeline) recordPeak() {
	peak := p.bytes.highWater()
	for {
		cur := p.h.peakInFlightBytes.Load()
		if peak <= cur || p.h.peakInFlightBytes.CompareAndSwap(cur, peak) {
			return
		}
	}
}

// SetPipelining sets how many commands one connection may have in flight and
// how many payload bytes they may hold at once.
//
// It is a method rather than two more NewHandler parameters because the handler
// is built in one place in production and ten in tests, and every test wants the
// defaults. Zero means "keep the current value", so a caller can set one knob
// without having to know the other's default.
//
// Call it before the adapter starts serving: serve reads these fields on each
// new connection, and a handler is built once per adapter.
func (h *Handler) SetPipelining(maxInFlight int, maxInFlightBytes int64) {
	if maxInFlight > 0 {
		h.maxInFlight = maxInFlight
	}
	if maxInFlightBytes > 0 {
		h.maxInFlightBytes = maxInFlightBytes
	}
}

// PeakInFlightBytes returns the highest payload memory this handler's pipelines
// held at once. It exists for the backpressure test: the pipeline itself is gone
// by the time serve returns, so the observation has to be recorded somewhere the
// test still holds.
func (h *Handler) PeakInFlightBytes() int64 { return h.peakInFlightBytes.Load() }

// submit queues a command, returning the error that should end the connection.
//
// The select on ctx.Done is what keeps a cancelled connection from wedging here:
// once the writer has failed nobody drains the reply channel, so a worker blocks
// on its send and the work channel fills, and a bare channel send would block the
// reader forever on a socket that is already gone.
func (p *pipeline) submit(work chan<- command, cmd command) error {
	select {
	case work <- cmd:
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

// submitWrite is submit for a write-like command, and additionally registers it
// with the barrier before queueing.
//
// The order matters: registered before the send, so a FLUSH that is already
// waiting cannot observe a count of zero while this write sits in the queue.
func (p *pipeline) submitWrite(work chan<- command, cmd command) error {
	p.barrier.submitted()
	if err := p.submit(work, cmd); err != nil {
		// Never queued, so no worker will ever call committed for it.
		p.barrier.committed()
		return err
	}
	return nil
}

// work is one worker: take a command, produce its reply, hand it to the writer.
//
// It touches neither the connection nor p.exp. The only shared state it reads is
// the barrier, which is mutex-guarded, and the handler, which is stateless
// between commands.
func (p *pipeline) work(ctx context.Context, work <-chan command, replies chan<- reply) {
	for cmd := range work {
		if !p.serve(ctx, cmd, replies) {
			return
		}
	}
}

// serve handles one command and reports whether the worker should keep going.
//
// The recover is why this is a separate function and not an inline loop body: a
// deferred recover only catches a panic in the frame it was deferred in, so the
// handler call has to be inside a function that owns a defer.
//
// A panic is one command's failure, not the connection's. Before the pool, the
// recovery in Conn.Serve covered the whole dispatch loop, so a panic cost one
// connection; that recovery cannot see a worker goroutine's panic at all — it
// would take the process down — so the containment lives here now and Conn.Serve
// keeps its recover as the outer backstop for the handshake and the reader.
func (p *pipeline) serve(ctx context.Context, cmd command, replies chan<- reply) (keepGoing bool) {
	defer func() {
		if r := recover(); r != nil {
			// The stack is the only record of what went wrong: the client sees
			// NBD_EIO and nothing else, and the panic value alone rarely names
			// the line.
			logger.Error("Panic in NBD command handler",
				"cookie", cmd.req.Cookie,
				"command", uint32(cmd.req.Type),
				"panic", r,
				"stack", string(debug.Stack()))

			// The send is guarded because a bare send in a deferred recover
			// blocks forever when the writer has already returned — which is
			// exactly the state a panic tends to arrive in — and a goroutine
			// parked in a deferred send never reaches its WaitGroup.Done, so
			// shutdown hangs instead of failing.
			panicReply := errorReply(cmd.req.Cookie, ErrEIO)
			panicReply.release = cmd.bytes
			keepGoing = p.post(ctx, cmd, panicReply, replies)
			if !keepGoing && isWriteLike(cmd.req.Type) && cmd.pre == nil {
				// Released here because serve's caller never reaches its own
				// release on this path.
				p.barrier.committed()
			}
		}
	}()

	rep := p.dispatch(ctx, cmd)
	// Set here rather than inside dispatch so every path carries it: a handler
	// that returns an error reply still spent the budget its command acquired.
	rep.release = cmd.bytes
	if p.post(ctx, cmd, rep, replies) {
		return true
	}
	// The writer is gone, so nobody will read this reply. Returning is the only
	// way to stop; the barrier must still be released or a FLUSH on another
	// goroutine would wait forever.
	if isWriteLike(cmd.req.Type) && cmd.pre == nil {
		p.barrier.committed()
	}
	return false
}

// post hands a reply to the writer, reporting whether it was delivered.
//
// The non-blocking attempt first is not an optimization. A plain select over both
// cases picks RANDOMLY when both are ready, and during a drain both always are:
// the context is cancelled and the reply channel has buffer room. So half the
// replies of a draining connection were dropped by their own escape hatch — the
// client got EOF where it should have got its answer, which is precisely the
// failure draining exists to prevent. Trying the send unconditionally first makes
// the escape hatch reachable only when the send would actually block.
func (p *pipeline) post(ctx context.Context, cmd command, rep reply, replies chan<- reply) bool {
	select {
	case replies <- rep:
		return true
	default:
	}
	select {
	case replies <- rep:
		return true
	case <-ctx.Done():
		return false
	}
}

// dispatch turns one command into one reply. It never returns an error: a
// command that cannot be served has an error reply, which is a different thing
// from a broken connection.
func (p *pipeline) dispatch(ctx context.Context, cmd command) reply {
	// A command the reader already refused: its answer is fixed, and the payload
	// (if any) was drained by the reader.
	if cmd.pre != nil {
		return *cmd.pre
	}

	if isWriteLike(cmd.req.Type) {
		// Released whatever the outcome: a write that failed has no effect for a
		// later FLUSH to publish, so holding the barrier for it would block the
		// client for no reason.
		defer p.barrier.committed()
	}

	switch cmd.req.Type {
	case CmdRead:
		return p.h.handleRead(ctx, p.bs, cmd.exp, cmd.req)

	case CmdWrite:
		// cmd.data was read by the reader, which is the only goroutine allowed
		// to advance the stream.
		return p.h.handleWrite(ctx, p.bs, cmd.exp, cmd.req, cmd.data)

	case CmdFlush:
		return p.h.handleFlush(ctx, p.bs, cmd.exp, cmd.req)

	case CmdTrim:
		return p.h.handleTrim(ctx, p.bs, cmd.exp, cmd.req)

	case CmdWriteZeroes:
		return p.h.handleWriteZeroes(ctx, p.bs, cmd.exp, cmd.req)

	default:
		// Every command the adapter does not serve (CACHE, BLOCK_STATUS) and
		// every value the spec does not define. An unimplemented command is an
		// invalid command: NBD_ENOTSUP is reserved by the spec for WRITE_ZEROES
		// with FAST_ZERO, which the adapter does not advertise.
		return errorReply(cmd.req.Cookie, ErrEINVAL)
	}
}

// writeLoop is the writer: the only goroutine that writes to the connection.
//
// Replies are not reordered here. They arrive in whatever order the workers
// finish, which is legal — NBD matches replies to requests by cookie, and a
// client with more than one command in flight has already accepted that. At
// maxInFlight == 1 they arrive in submission order regardless.
func (p *pipeline) writeLoop(replies <-chan reply) error {
	// The release happens here, after the reply is written, because that is when
	// the payload buffer's ownership ends: the reader filled it and the writer
	// has now sent it, so nothing else can be holding a reference.
	//
	// It is deferred so a write error still releases: a connection that fails
	// mid-reply has already spent the budget, and the pipeline is about to unwind
	// anyway — but the defer keeps the accounting true rather than merely
	// irrelevant.
	defer p.recordPeak()
	for rep := range replies {
		if err := rep.write(p.conn, p.h); err != nil {
			return err
		}
		p.bytes.release(rep.release)
		// Published as each reply retires, not only at the end: the test reads
		// the peak while the connection is still open, and a peak published only
		// on shutdown would be invisible to it.
		p.recordPeak()
	}
	return nil
}

// isWriteLike reports whether a command mutates the export, and so participates
// in the FLUSH barrier.
//
// The three mutating commands, and nothing else: FLUSH is the barrier itself,
// READ does not change the device, and DISC changes nothing at all. A command
// missing from this list would let a FLUSH reply before its effects are durable,
// which is the one guarantee NBD_FLAG_SEND_FUA and NBD_CMD_FLUSH exist to make.
func isWriteLike(t Command) bool {
	return t == CmdWrite || t == CmdTrim || t == CmdWriteZeroes
}

// DefaultMaxInFlight is how many commands one connection may have in flight.
//
// Four, not the dozens a filesystem server might use, because the measured
// plateau is at depth 4: the depth sweep in docs/nbd-benchmarks.md shows
// 7,686 IOPS at depth 1 against 13,000-16,900 from depth 4 on, and nothing above
// it. More workers buy latency under load and cost memory; they do not raise the
// ceiling, which is the block store's.
const DefaultMaxInFlight = 4

// DefaultMaxInFlightBytes bounds the payload memory one connection may hold.
//
// 64 MiB is two maximum-size payloads (the advertised block maximum is 32 MiB),
// so a single large request cannot be starved by its own neighbours. It is per
// connection, and the default connection cap is 64, so the adapter-wide ceiling
// is 4 GiB of payload buffers — which is why MaxConnections defaults to 64 rather
// than NFS's 1024.
const DefaultMaxInFlightBytes int64 = 64 << 20
