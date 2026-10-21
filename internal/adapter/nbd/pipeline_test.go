package nbd

import (
	"bytes"
	"testing"
	"time"
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
