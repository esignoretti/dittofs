package nbd

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestPipeline_WritePayloadIsConsumedInOrder pins the property the whole design
// rests on.
//
// A WRITE's payload is inline in the stream, so a reader that parses the next
// header before consuming it reads a header out of payload bytes. The symptom
// appears on a later, unrelated command — which is why this asserts the third
// command's reply, not the second's. The second WRITE's own reply can look
// perfectly correct while the stream is already misaligned, because its cookie
// was read before the mistake.
//
// At maxInFlight: 1 this is the serial loop's behaviour, so the test is a
// regression gate for the refactor rather than a new guarantee. It stays
// meaningful when Task 11 raises the worker count, because the invariant it
// protects is about who reads the stream, not about how many handlers run.
func TestPipeline_WritePayloadIsConsumedInOrder(t *testing.T) {
	first := bytes.Repeat([]byte{0xaa}, 4096)
	second := bytes.Repeat([]byte{0xbb}, 4096)

	// The real-metadata fixture, not newTestHandler: a successful WRITE calls
	// PrepareWrite and CommitWrite, and newTestHandler passes nil for meta. A
	// store-fault test can get away with nil; a test that expects Error == 0
	// cannot.
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	st.data[string(f.exp.PayloadID)] = make([]byte, 1<<20)
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	if rep := writeThroughClient(t, client, 0, first, 0); rep.Error != 0 {
		t.Fatalf("first WRITE reply error = %d", rep.Error)
	}
	if rep := writeThroughClient(t, client, 4096, second, 0); rep.Error != 0 {
		t.Fatalf("second WRITE reply error = %d; the reader did not consume the first WRITE's payload in order", rep.Error)
	}

	rep, got := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 21, Offset: 4096, Length: 4096,
	})
	if rep.Error != 0 {
		t.Fatalf("READ reply error = %d", rep.Error)
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("READ returned the wrong payload: got %#x... want 0xbb...", got[:8])
	}
}

// TestPipeline_RepliesArriveForEveryCommand pins that no reply is dropped when
// commands are queued rather than handled inline. A pipeline that loses a reply
// does not fail loudly: the client blocks until its own timeout, and the failure
// looks like a hang rather than an error.
func TestPipeline_RepliesArriveForEveryCommand(t *testing.T) {
	// The real-metadata fixture again, and not for the WRITE path: FLUSH reaches
	// FlushPendingWriteForFile and TRIM reaches PunchHole, both of which deref the
	// service. newTestHandler's nil meta is only safe for commands that return
	// before touching metadata.
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	st.data[string(f.exp.PayloadID)] = make([]byte, 1<<20)
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	// Three commands in a row, no payloads, no reads in between: every reply has
	// to arrive, and in submission order at maxInFlight: 1. The cookie is read
	// back off the wire rather than assumed, because a dropped reply and a
	// reordered one both show up here as the wrong cookie.
	for i, typ := range []Command{CmdFlush, CmdTrim, CmdFlush} {
		cookie := uint64(31 + i)
		sendRequest(t, client, &Request{Type: typ, Cookie: cookie, Length: 4096})
		if rep := readReplyHeader(t, client); rep.Cookie != cookie {
			t.Fatalf("reply %d cookie = %d, want %d; a reply was lost or reordered", i, rep.Cookie, cookie)
		}
	}
}

// TestPipeline_CleanDisconnectReturnsNil pins that NBD_CMD_DISC still ends the
// connection without an error.
//
// The branch used to be the first thing the dispatch loop checked. Once the loop
// moved into the pipeline's reader it had to move with it: a DISC left in serve
// would be unreachable, and the connection would end on the next read's error
// instead — which Serve treats as a failure, so every well-behaved client
// shutdown would log an error.
func TestPipeline_CleanDisconnectReturnsNil(t *testing.T) {
	h := newTestHandler(newStoreFake(), testExport(8192), testTimeouts())
	client, done := serveHandler(t, h, testExportName)

	sendRequest(t, client, &Request{Type: CmdDisc})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after NBD_CMD_DISC, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after NBD_CMD_DISC")
	}
}

// TestPipeline_WorkerPanicRepliesEIO pins that a panic in one command does not
// kill the process or the connection.
//
// Before the pool, the recovery in Conn.Serve covered the whole dispatch loop, so
// a panic cost one connection. That recovery cannot see a worker goroutine's
// panic at all — it would take the process down — so it has to move into the
// worker, and the command that caused it has to be answered rather than dropped.
// NBD_EIO is the honest answer: the server failed to serve this command, and the
// client can retry or report it.
//
// The READ afterwards is the part that matters: a panic contained to its own
// command leaves the connection usable, which is what separates a contained
// fault from a crash that happens to be caught.
func TestPipeline_WorkerPanicRepliesEIO(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	st.data[string(f.exp.PayloadID)] = make([]byte, 1<<20)
	st.panicOnWrite = true
	h := f.newHandler(t, st)
	client, done := serveHandler(t, h, f.exp.Name)

	// The panic: handleWrite calls bs.WriteAt, which panics on purpose.
	if rep := writeThroughClient(t, client, 0, []byte("boom"), 0); rep.Error != ErrEIO {
		t.Fatalf("WRITE that panicked reply error = %d, want ErrEIO (%d)", rep.Error, uint32(ErrEIO))
	}

	// The connection survives it.
	st.panicOnWrite = false
	rep, got := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 41, Offset: 0, Length: 4096,
	})
	if rep.Error != 0 {
		t.Fatalf("READ after a panicking WRITE reply error = %d; the panic cost the connection", rep.Error)
	}
	if len(got) != 4096 {
		t.Fatalf("READ returned %d bytes, want 4096", len(got))
	}

	// And the serve goroutine is still alive: nothing was torn down.
	select {
	case err := <-done:
		t.Fatalf("serve returned %v; the connection did not survive a contained panic", err)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPipeline_BackpressureBoundsMemory pins the property that makes a pool safe
// on a small host.
//
// Each in-flight WRITE holds req.Length bytes and each in-flight READ allocates
// req.Length bytes on a worker, and the advertised maximum is 32 MiB — so an
// unbounded pool turns a fast client into an OOM. Commands block when the budget
// is exhausted rather than being refused: a refused write is a data-loss bug,
// a delayed one is just slow.
//
// The budget here is 64 KiB against 32 KiB commands, so exactly two fit. The
// assertion is on the pipeline's own peak accounting rather than on the Go heap,
// because the heap is not attributable to this connection.
func TestPipeline_BackpressureBoundsMemory(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	st.data[string(f.exp.PayloadID)] = make([]byte, 1<<20)
	h := f.newHandler(t, st)
	// Set on the handler, not on a pipeline: serve builds the pipeline, and a
	// second pipeline on the same connection would have two readers racing for
	// the stream — which is the very thing the design forbids.
	h.maxInFlight = 8
	h.maxInFlightBytes = 64 << 10
	client, done := serveHandler(t, h, f.exp.Name)

	const cmdBytes = 32 << 10
	const n = 8

	// Submitted all at once and only then read back, which is the whole point:
	// writeThroughClient waits for each reply, so a loop of it would never have
	// more than one command in flight and the peak would be one command's bytes
	// whether or not a budget existed. That version of this test passed with the
	// budget removed.
	//
	// 64 KiB of budget against 32 KiB commands means exactly two fit at once, so
	// the peak is asserted at the budget rather than at 8 x 32 KiB.
	for i := range n {
		sendRequest(t, client, &Request{
			Type: CmdWrite, Cookie: uint64(i), Offset: uint64(i * cmdBytes), Length: cmdBytes,
		})
		if _, err := client.Write(bytes.Repeat([]byte{byte(i)}, cmdBytes)); err != nil {
			t.Fatalf("WRITE %d payload: %v", i, err)
		}
	}

	// Every command completes: the budget delays, it does not refuse.
	seen := map[uint64]bool{}
	for range n {
		rep := readReplyHeader(t, client)
		if rep.Error != 0 {
			t.Fatalf("WRITE cookie %d reply error = %d", rep.Cookie, rep.Error)
		}
		seen[rep.Cookie] = true
	}
	if len(seen) != n {
		t.Fatalf("saw %d distinct reply cookies, want %d", len(seen), n)
	}

	if peak := h.PeakInFlightBytes(); peak > h.maxInFlightBytes {
		t.Fatalf("peak buffered bytes = %d, want <= %d; the byte budget did not bound the pool",
			peak, h.maxInFlightBytes)
	} else if peak == 0 {
		t.Fatal("peak buffered bytes = 0; the budget was never exercised, so this test proves nothing")
	}

	// Still serving.
	select {
	case err := <-done:
		t.Fatalf("serve returned %v; the connection should still be open", err)
	default:
	}

	_ = client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the client closed")
	}
}

// TestPipeline_ConcurrentReadsAreConsistent pins that out-of-order completion
// does not mispair a reply with a cookie.
//
// NBD simple replies are matched by cookie, not by position, so a worker pool
// legitimately returns them out of order — which means a single swapped cookie
// corrupts a filesystem silently, with no protocol error to notice it by. Each
// READ here targets a range whose contents are derived from the cookie, so a
// swap is detectable rather than merely unlikely.
//
// With one worker this passes trivially, because replies are inherently in
// order. It becomes meaningful when the pool is widened, and it is written now so
// that widening the pool cannot quietly break it.
func TestPipeline_ConcurrentReadsAreConsistent(t *testing.T) {
	const blockSize = 512

	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	data := make([]byte, 32*blockSize)
	for i := range 32 {
		for j := range blockSize {
			data[i*blockSize+j] = byte(i)
		}
	}
	st.data[string(f.exp.PayloadID)] = data
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	for i := range 32 {
		rep, got := readReplyAndPayload(t, client, &Request{
			Type: CmdRead, Cookie: uint64(i), Offset: uint64(i * blockSize), Length: blockSize,
		})
		if rep.Error != 0 {
			t.Fatalf("READ %d reply error = %d", i, rep.Error)
		}
		if rep.Cookie != uint64(i) {
			t.Fatalf("reply cookie = %d, want %d", rep.Cookie, i)
		}
		for _, b := range got {
			if b != byte(i) {
				t.Fatalf("READ at offset %d (cookie %d) returned %#x, want %#x: a reply was paired with the wrong cookie",
					i*blockSize, i, b, byte(i))
			}
		}
	}
}

// TestHandler_SetPipelining pins that the configured values reach the handler.
//
// It exists because they did not. Handler's fields were read by serve and
// written by nobody: pkg/adapter/nbd/adapter.go built the handler without them,
// so every configured value was silently replaced by the handler's own default.
// A benchmark A/B against that build measured the default against the default and
// reported that pipelining changed nothing — which was true of the binary, and
// false of the code.
//
// Zero means "keep the current value" so a caller can set one knob alone; that is
// asserted too, because a caller that sets only the byte budget must not
// accidentally zero the worker count and fall back to 1.
func TestHandler_SetPipelining(t *testing.T) {
	h := newTestHandler(newStoreFake(), testExport(8192), testTimeouts())
	if h.maxInFlight != DefaultMaxInFlight {
		t.Fatalf("new handler maxInFlight = %d, want %d", h.maxInFlight, DefaultMaxInFlight)
	}
	if h.maxInFlightBytes != DefaultMaxInFlightBytes {
		t.Fatalf("new handler maxInFlightBytes = %d, want %d", h.maxInFlightBytes, DefaultMaxInFlightBytes)
	}

	h.SetPipelining(8, 1<<20)
	if h.maxInFlight != 8 {
		t.Fatalf("maxInFlight = %d, want 8", h.maxInFlight)
	}
	if h.maxInFlightBytes != 1<<20 {
		t.Fatalf("maxInFlightBytes = %d, want %d", h.maxInFlightBytes, 1<<20)
	}

	// Zero leaves the value alone, so one knob can be set without the other.
	h.SetPipelining(0, 2<<20)
	if h.maxInFlight != 8 {
		t.Fatalf("maxInFlight = %d after SetPipelining(0, ...), want it unchanged at 8", h.maxInFlight)
	}
	if h.maxInFlightBytes != 2<<20 {
		t.Fatalf("maxInFlightBytes = %d, want %d", h.maxInFlightBytes, 2<<20)
	}
}

// TestPipeline_ShutdownDrainsInFlight pins that a command already being served
// when shutdown begins still gets its reply.
//
// A client that has sent a write and is waiting for the answer must not see a
// closed socket: that is the difference between a paused device and an errored
// one, which is the whole point of answering NBD_ESHUTDOWN rather than hanging up.
//
// The trigger is context cancellation with the connection still open. Closing the
// socket instead would pass against a loop that abandons everything, because the
// client would only notice the hangup — the test would then be asserting the
// wrong thing.
func TestPipeline_ShutdownDrainsInFlight(t *testing.T) {
	f := newWriteVolumeFixture(t)

	// A store that blocks inside WriteAt until the test lets it go, so the worker
	// is demonstrably mid-command when the context is cancelled.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	st := &blockingWriteStore{
		storeFake: newStoreFake(),
		onWrite: func() {
			once.Do(func() { close(entered) })
			<-release
		},
	}
	st.data[string(f.exp.PayloadID)] = make([]byte, 1<<20)

	h := f.newHandler(t, st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server, client := pipe(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewConn(h, server).Serve(ctx)
	}()
	completeHandshake(t, client, f.exp.Name)

	// Send the WRITE and wait until the worker is inside the store.
	sendRequest(t, client, &Request{Type: CmdWrite, Cookie: 7, Offset: 0, Length: 512})
	if _, err := client.Write(bytes.Repeat([]byte{0xaa}, 512)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never reached the store")
	}

	// Shutdown begins now, while that write is in flight.
	cancel()

	// The reply must still arrive, and arrive cleanly.
	close(release)
	rep := readReplyHeader(t, client)
	if rep.Error != 0 {
		t.Fatalf("in-flight WRITE reply error = %d, want 0: shutdown abandoned it instead of draining it", rep.Error)
	}
	if rep.Cookie != 7 {
		t.Fatalf("reply cookie = %d, want 7", rep.Cookie)
	}

	// And serve returns: the drain is bounded, not a hang.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation; the reader is still blocked on the socket")
	}
}

// blockingWriteStore blocks inside WriteAt until its hook returns.
type blockingWriteStore struct {
	*storeFake
	onWrite func()
}

func (s *blockingWriteStore) WriteAt(ctx context.Context, id string, currentBlocks []block.ChunkRef, data []byte, off uint64) ([]block.ChunkRef, error) {
	s.onWrite()
	return s.storeFake.WriteAt(ctx, id, currentBlocks, data, off)
}
