package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// storeFake is the minimum surface a handler needs. It is deliberately not an
// engine.Store: these tests are about dispatch and error mapping, and a real
// engine would make every failure mode a store failure mode.
//
// The durability accessors are here because BlockStore declares them; the
// constructor's values describe the common production shape (a durable local
// store with a durable remote and no strict enforcement), so a commit takes the
// localDurable fast path. A test that needs the strict path sets them itself.
type storeFake struct {
	// mu guards everything below.
	//
	// It is not paranoia: before the pipeline, dispatch was serialized, so a
	// counter could be a plain int and a test could read st.writes after a reply
	// and know nothing else was running. With maxInFlight > 1 several workers
	// reach the same fake at once, and -race finds it immediately. The counters
	// stay readable through the accessors at the bottom of this file.
	mu       sync.Mutex
	data     map[string][]byte
	readErr  error
	writeErr error
	flushErr error
	punchErr error
	reads    int
	writes   int
	flushes  int
	punches  int

	localDurable   bool
	remoteDurable  bool
	requireDurable bool

	// onFlush runs inside Flush before it returns, so a test can observe the
	// store at the moment it is asked to commit. It is set before the server
	// goroutine starts, never after.
	onFlush func()

	// panicOnRead makes ReadAt panic instead of returning, so a test can drive
	// a panic from below the dispatcher — exactly where a real one would (a nil
	// map, a slice bounds error, a bad type assertion on an engine-returned
	// store).
	panicOnRead bool

	// panicOnWrite does the same for WriteAt. It exists for the worker-panic
	// test: before the pipeline, a panic anywhere in dispatch was caught by
	// Conn.Serve, which covers the loop but not a worker goroutine.
	panicOnWrite bool
}

func newStoreFake() *storeFake {
	return &storeFake{
		data:          map[string][]byte{},
		localDurable:  true,
		remoteDurable: true,
	}
}

func (s *storeFake) ReadAt(_ context.Context, id string, buf []byte, off uint64) (int, error) {
	s.mu.Lock()
	s.reads++
	panicOnRead := s.panicOnRead
	s.mu.Unlock()
	if panicOnRead {
		panic("storeFake: ReadAt panicked on purpose")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return 0, s.readErr
	}
	b := s.data[id]
	if off >= uint64(len(b)) {
		for i := range buf {
			buf[i] = 0
		}
		return len(buf), nil
	}
	n := copy(buf, b[off:])
	for i := n; i < len(buf); i++ {
		buf[i] = 0
	}
	return len(buf), nil
}

func (s *storeFake) WriteAt(_ context.Context, id string, currentBlocks []block.ChunkRef, data []byte, off uint64) ([]block.ChunkRef, error) {
	s.mu.Lock()
	s.writes++
	panicOnWrite := s.panicOnWrite
	s.mu.Unlock()
	if panicOnWrite {
		panic("storeFake: WriteAt panicked on purpose")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return currentBlocks, s.writeErr
	}
	if s.data[id] == nil {
		s.data[id] = nil
	}
	end := off + uint64(len(data))
	if end > uint64(len(s.data[id])) {
		grown := make([]byte, end)
		copy(grown, s.data[id])
		s.data[id] = grown
	}
	copy(s.data[id][off:], data)
	return currentBlocks, nil
}

func (s *storeFake) PunchHole(_ context.Context, id string, currentBlocks []block.ChunkRef, off, length uint64) ([]block.ChunkRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.punches++
	if s.punchErr != nil {
		return currentBlocks, s.punchErr
	}
	b := s.data[id]
	for i := off; i < off+length && i < uint64(len(b)); i++ {
		b[i] = 0
	}
	return currentBlocks, nil
}

func (s *storeFake) Flush(_ context.Context, _ string) (*block.FlushResult, error) {
	s.mu.Lock()
	s.flushes++
	onFlush := s.onFlush
	flushErr := s.flushErr
	s.mu.Unlock()
	// Called without the lock: onFlush is a test hook that inspects the store, so
	// holding mu here would deadlock the moment it reads a counter.
	if onFlush != nil {
		onFlush()
	}
	if flushErr != nil {
		return nil, flushErr
	}
	return &block.FlushResult{}, nil
}

func (s *storeFake) LocalDurable() bool         { return s.localDurable }
func (s *storeFake) RemoteDurable() bool        { return s.remoteDurable }
func (s *storeFake) RequireDurableCommit() bool { return s.requireDurable }

// testExportName is the export every connection test resolves.
const testExportName = "/export/boot"

// testPayloadID is the block-store payload the fixture export addresses.
const testPayloadID = "p1"

// testExport is the export the fixture resolves: a volume of size bytes whose
// data lives at testPayloadID. It is writable; readOnlyExport is the read-only
// variant.
func testExport(size uint64) *Export {
	return &Export{Name: testExportName, Size: size, PayloadID: testPayloadID}
}

// readOnlyExport is testExport with the export-level read-only ceiling set.
//
// A second constructor rather than a mutation of testExport's return value:
// tests share the value they build, so a test that flipped the flag on a shared
// export would change what every other test resolves.
func readOnlyExport(size uint64) *Export {
	exp := testExport(size)
	exp.ReadOnly = true
	return exp
}

// exporterFor returns an exporter that resolves testExportName to exp. It
// reuses fakeExporter from handshake_test.go, which is in this package.
func exporterFor(exp *Export) Exporter {
	return &fakeExporter{exports: map[string]*Export{testExportName: exp}}
}

// resolverFor returns a StoreResolver that always hands back st.
func resolverFor(st BlockStore) StoreResolver {
	return func(context.Context, *Export) (BlockStore, error) { return st, nil }
}

// testTimeouts is what the fixture arms. Read is generous enough that no test
// races it; TestConnection_PartialHeaderTimesOut sets its own.
func testTimeouts() Timeouts {
	return Timeouts{Read: 5 * time.Second, Write: 5 * time.Second, Idle: 5 * time.Second}
}

// newTestHandler builds a handler over st for one export, with draining always
// false.
func newTestHandler(st BlockStore, exp *Export, timeouts Timeouts) *Handler {
	return NewHandler(exporterFor(exp), resolverFor(st), nil,
		Identity{UID: 65534, GID: 65534}, timeouts, func() bool { return false })
}

// serveConn runs h.serve on server and returns a channel carrying its result.
// The tests drive the client side over the pipe(t) helper's other end.
func serveConn(h *Handler, server net.Conn) chan error {
	done := make(chan error, 1)
	go func() { done <- h.serve(context.Background(), server) }()
	return done
}

// completeHandshake reads the greeting, sends client flags, asks for name with
// NBD_OPT_GO, and drains the option replies through the terminating ACK. On
// return the connection is in the transmission phase.
func completeHandshake(t *testing.T, client net.Conn, name string) {
	t.Helper()
	readGreeting(t, client)
	sendClientFlags(t, client, 1) // NBD_FLAG_C_FIXED_NEWSTYLE
	sendOption(t, client, OptGO, goPayload(name))
	for {
		var hdr [20]byte
		if _, err := io.ReadFull(client, hdr[:]); err != nil {
			t.Fatalf("read handshake reply: %v", err)
		}
		typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16]))
		data := make([]byte, binary.BigEndian.Uint32(hdr[16:20]))
		if len(data) > 0 {
			if _, err := io.ReadFull(client, data); err != nil {
				t.Fatalf("read handshake reply payload: %v", err)
			}
		}
		if typ == RepAck {
			return
		}
	}
}

// sendRequest encodes one transmission-phase request. The magic is filled in
// when the caller leaves it zero, since every request carries the same value.
func sendRequest(t *testing.T, w io.Writer, req *Request) {
	t.Helper()
	if req.Magic == 0 {
		req.Magic = requestMagic
	}
	if err := req.Encode(w); err != nil {
		t.Fatalf("encode request: %v", err)
	}
}

// readReplyHeader reads one transmission-phase reply header and checks its
// magic.
func readReplyHeader(t *testing.T, r io.Reader) *Reply {
	t.Helper()
	rep, err := ReadReply(r)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if rep.Magic != simpleReplyMagic {
		t.Fatalf("reply magic = %#x, want %#x", rep.Magic, uint32(simpleReplyMagic))
	}
	return rep
}

// readPayload reads n bytes of reply payload.
func readPayload(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read %d-byte payload: %v", n, err)
	}
	return buf
}

// writeThroughClient sends one WRITE together with its payload and returns the
// reply. The payload always goes out: the spec requires a client to follow the
// request header with length bytes, whatever the server answers.
func writeThroughClient(t *testing.T, client net.Conn, offset uint64, data []byte, flags CommandFlags) *Reply {
	t.Helper()
	sendRequest(t, client, &Request{
		Type: CmdWrite, Flags: flags, Cookie: 0x77, Offset: offset, Length: uint32(len(data)),
	})
	if len(data) > 0 {
		if _, err := client.Write(data); err != nil {
			t.Fatalf("write payload: %v", err)
		}
	}
	return readReplyHeader(t, client)
}

// flushThroughClient sends one FLUSH and returns the reply. FLUSH carries no
// payload, so there is nothing to send after the header.
func flushThroughClient(t *testing.T, client net.Conn) *Reply {
	t.Helper()
	sendRequest(t, client, &Request{Type: CmdFlush, Cookie: 0x78})
	return readReplyHeader(t, client)
}

// assertNoPayload asserts nothing arrives on the connection within d. An error
// reply carries no payload, and a closed connection is also "nothing".
func assertNoPayload(t *testing.T, client net.Conn, d time.Duration) {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	defer func() { _ = client.SetReadDeadline(time.Time{}) }()
	var b [1]byte
	if n, err := client.Read(b[:]); err == nil {
		t.Fatalf("expected no payload after the reply, read %d byte(s)", n)
	}
}

// readReplyAndPayload sends a READ and returns its reply header plus the
// payload that follows a successful one.
func readReplyAndPayload(t *testing.T, client net.Conn, req *Request) (*Reply, []byte) {
	t.Helper()
	sendRequest(t, client, req)
	rep := readReplyHeader(t, client)
	if rep.Error != 0 {
		return rep, nil
	}
	return rep, readPayload(t, client, int(req.Length))
}

func TestConnection_ReadServesBytes(t *testing.T) {
	want := make([]byte, 4096)
	for i := range want {
		want[i] = byte(i % 251)
	}
	st := newStoreFake()
	st.data[testPayloadID] = want

	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	rep, got := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 7, Offset: 0, Length: 4096,
	})
	if rep.Error != 0 {
		t.Fatalf("READ reply error = %d, want 0", rep.Error)
	}
	if rep.Cookie != 7 {
		t.Fatalf("READ reply cookie = %d, want 7", rep.Cookie)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("READ returned %d bytes that do not match the fixture", len(got))
	}
}

func TestConnection_ReadServesZerosForUnwrittenRange(t *testing.T) {
	st := newStoreFake()
	st.data[testPayloadID] = make([]byte, 4096)

	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	rep, got := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 1, Offset: 4096, Length: 4096,
	})
	if rep.Error != 0 {
		t.Fatalf("READ past the written range returned error %d; a hole reads as zeros, not as an error", rep.Error)
	}
	if !bytes.Equal(got, make([]byte, 4096)) {
		t.Fatal("READ past the written range did not return zeros")
	}
}

func TestConnection_UnknownCommandIsEINVAL(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	sendRequest(t, client, &Request{Type: Command(99), Cookie: 3, Length: 4096})
	rep := readReplyHeader(t, client)
	if rep.Error != ErrEINVAL {
		t.Fatalf("unknown command reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	if rep.Cookie != 3 {
		t.Fatalf("unknown command reply cookie = %d, want 3", rep.Cookie)
	}
	// A reply header for a command whose reply shape is unknown must not be
	// followed by data.
	assertNoPayload(t, client, 100*time.Millisecond)
}

func TestConnection_UnimplementedCommandIsEINVAL(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	sendRequest(t, client, &Request{Type: CmdCache, Cookie: 4})
	rep := readReplyHeader(t, client)
	// NBD_ENOTSUP is reserved by the spec for WRITE_ZEROES with FAST_ZERO,
	// which the adapter does not advertise; a command it does not implement is
	// an invalid command.
	if rep.Error != ErrEINVAL {
		t.Fatalf("unimplemented command reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
}

func TestConnection_ErrorsCarryTheCookie(t *testing.T) {
	st := newStoreFake()
	st.readErr = errors.New("boom")

	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	sendRequest(t, client, &Request{Type: CmdRead, Cookie: 0xdeadbeef, Offset: 0, Length: 4096})
	rep := readReplyHeader(t, client)
	if rep.Error != ErrEIO {
		t.Fatalf("failing READ reply error = %d, want ErrEIO (%d)", rep.Error, uint32(ErrEIO))
	}
	// A dropped cookie makes a client hang forever waiting for the reply to a
	// request it cannot match.
	if rep.Cookie != 0xdeadbeef {
		t.Fatalf("failing READ reply cookie = %#x, want 0xdeadbeef", rep.Cookie)
	}
	assertNoPayload(t, client, 100*time.Millisecond)
}

func TestConnection_DiscClosesWithoutReply(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	done := serveConn(h, server)
	completeHandshake(t, client, testExportName)

	sendRequest(t, client, &Request{Type: CmdDisc})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after DISC; a deliberate disconnect is not an error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return after DISC")
	}
	// "There is no reply to an NBD_CMD_DISC."
	assertNoPayload(t, client, 100*time.Millisecond)
}

// The base adapter's accept loop has no recover (pkg/adapter/base.go:373 runs
// the handler on a bare goroutine), so a panic that escapes does not kill the
// connection — it kills the process, and with it every NFS and SMB connection.
// Both sibling adapters recover for this reason: NFS at
// pkg/adapter/nfs/connection.go:420, SMB at pkg/adapter/smb/connection.go:474.
//
// The assertion is "it came back", and that is sufficient: if the recover is
// missing the panic unwinds the goroutine and the test binary dies, so the
// failure mode is a crashed package rather than a failed assertion. The test
// deliberately does not wrap the call in its own recover — that would make it
// pass whether or not Conn.Serve has one.
//
// The panic now arrives from a worker goroutine, where Conn.Serve's recover
// cannot see it: a recover only covers the frame it was deferred in, and the
// handler call moved out of that frame when dispatch became a pool. Containment
// therefore lives in the worker (pipeline.serve), and the observable difference
// is that the connection now SURVIVES — the client gets NBD_EIO for the one
// command and keeps using the connection. This test asserts that, which is the
// stronger property, and TestPipeline_WorkerPanicRepliesEIO asserts the reply
// that comes back. Conn.Serve's own recover is still the backstop for a panic in
// the handshake or the reader, neither of which this can drive.
func TestConn_PanicInHandlerDoesNotKillTheProcess(t *testing.T) {
	st := newStoreFake()
	st.panicOnRead = true

	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewConn(h, server).Serve(context.Background())
	}()

	completeHandshake(t, client, testExportName)
	sendRequest(t, client, &Request{Type: CmdRead, Cookie: 1, Offset: 0, Length: 4096})

	// The one command that panicked is answered, not dropped.
	if rep := readReplyHeader(t, client); rep.Error != ErrEIO {
		t.Fatalf("panicking READ reply error = %d, want ErrEIO (%d)", rep.Error, uint32(ErrEIO))
	}

	// And the connection is still alive: Conn.Serve has not returned. If the
	// containment were missing, the process would already have died.
	select {
	case <-done:
		t.Fatal("Conn.Serve returned after the handler panicked; the connection should have survived a contained panic")
	case <-time.After(50 * time.Millisecond):
	}
}

// The store is resolved after the handshake, so a resolver that fails is the
// only way to reach this path deterministically: the window between the
// handshake ACK and the resolution is a few microseconds on the server's own
// goroutine, so no external test can land a share-removal inside it.
func TestConnection_FailedStoreResolutionDegradesAndCounts(t *testing.T) {
	sentinel := errors.New("store unavailable")
	h := NewHandler(exporterFor(testExport(8192)),
		func(context.Context, *Export) (BlockStore, error) { return nil, sentinel },
		nil, Identity{UID: 65534, GID: 65534}, testTimeouts(), func() bool { return false })

	server, client := pipe(t)
	done := serveConn(h, server)
	completeHandshake(t, client, testExportName)

	// Asserted in this order: checking the consumption first would pass on a
	// counter that was never incremented.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil after the store resolution failed; the connection must end")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return after the store resolution failed")
	}
	if got := h.TakeDegraded(); got != 1 {
		t.Fatalf("TakeDegraded() = %d, want 1", got)
	}
	if got := h.TakeDegraded(); got != 0 {
		t.Fatalf("second TakeDegraded() = %d, want 0: the probe must consume the count", got)
	}
}

// The last byte of the device is addressable. Only offset+length > size is out
// of range.
func TestConnection_ReadAtExactEndIsServed(t *testing.T) {
	want := make([]byte, 4096)
	for i := range want {
		want[i] = byte(i%251) ^ 0x5a
	}
	st := newStoreFake()
	st.data[testPayloadID] = append(make([]byte, 4096), want...)

	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	rep, got := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 1, Offset: 4096, Length: 4096,
	})
	if rep.Error != 0 {
		t.Fatalf("READ ending exactly at the export end returned error %d, want 0: the last byte is addressable", rep.Error)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("READ ending exactly at the export end returned the wrong bytes")
	}
}

func TestConnection_ReadPastEndIsEINVAL(t *testing.T) {
	st := newStoreFake()
	st.data[testPayloadID] = make([]byte, 8192)

	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	sendRequest(t, client, &Request{Type: CmdRead, Cookie: 1, Offset: 4096, Length: 4097})
	rep := readReplyHeader(t, client)
	if rep.Error != ErrEINVAL {
		t.Fatalf("READ past the export end reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	assertNoPayload(t, client, 100*time.Millisecond)
}

func TestConnection_ReadZeroLengthIsEINVAL(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	sendRequest(t, client, &Request{Type: CmdRead, Cookie: 1, Offset: 0, Length: 0})
	rep := readReplyHeader(t, client)
	if rep.Error != ErrEINVAL {
		t.Fatalf("zero-length READ reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	assertNoPayload(t, client, 100*time.Millisecond)
	if st.reads != 0 {
		t.Fatalf("the store saw %d reads for a zero-length request", st.reads)
	}
}

// Not EOVERFLOW: the spec reserves that for READ with NBD_CMD_FLAG_DF and for
// payload-less over-max requests, neither of which the adapter supports. Not
// EINVAL either — the request is not out of range, it cannot be interpreted at
// all. The check must not be `offset+length > size` with a wrapping add.
func TestConnection_OffsetPlusLengthOverflowIsEIO(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	sendRequest(t, client, &Request{Type: CmdRead, Cookie: 1, Offset: math.MaxUint64, Length: 4096})
	rep := readReplyHeader(t, client)
	if rep.Error != ErrEIO {
		t.Fatalf("overflowing READ reply error = %d, want ErrEIO (%d)", rep.Error, uint32(ErrEIO))
	}
	assertNoPayload(t, client, 100*time.Millisecond)
	if st.reads != 0 {
		t.Fatalf("the store saw %d reads for an overflowing request", st.reads)
	}
}

// FUA is valid on every command once SEND_FUA is advertised: the spec says
// "the server MUST accept all commands with this bit set (even by ignoring the
// bit)" and notes "existing clients are known to set this bit on other
// commands". A READ writes nothing, so the bit is ignored and the read
// succeeds. Refusing would break real clients for a guarantee nobody asked for.
func TestConnection_ReadWithFUAFlagSucceeds(t *testing.T) {
	want := make([]byte, 4096)
	for i := range want {
		want[i] = 0xa5
	}
	st := newStoreFake()
	st.data[testPayloadID] = want

	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	rep, got := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Flags: FlagFUA, Cookie: 1, Offset: 0, Length: 4096,
	})
	if rep.Error != 0 {
		t.Fatalf("READ with NBD_CMD_FLAG_FUA returned error %d, want 0", rep.Error)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("READ with NBD_CMD_FLAG_FUA returned the wrong bytes")
	}
}

// NO_HOLE is documented for WRITE_ZEROES only, so on WRITE it is a flag "not
// explicitly documented as applicable to the given request" → NBD_EINVAL.
func TestConnection_WriteWithNoHoleFlagIsEINVAL(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	sendRequest(t, client, &Request{Type: CmdWrite, Flags: FlagNoHole, Cookie: 1, Offset: 0, Length: 4096})
	// The payload goes out even though the request is refused: the spec requires
	// a client to follow the header with length bytes unconditionally, and the
	// server drains them before reading the next header. A test that declares
	// bytes it never sends is asserting against a client the spec forbids, and
	// it would leave the server draining a payload that never comes.
	if _, err := client.Write(make([]byte, 4096)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	rep := readReplyHeader(t, client)
	if rep.Error != ErrEINVAL {
		t.Fatalf("WRITE with NBD_CMD_FLAG_NO_HOLE reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
}

// DF, REQ_ONE and FAST_ZERO are each gated behind an advertisement the adapter
// does not make, so none of them may appear on a READ.
func TestConnection_UnadvertisedFlagIsEINVAL(t *testing.T) {
	for _, flag := range []CommandFlags{FlagDF, FlagReqOne, FlagFastZero} {
		t.Run(flagName(flag), func(t *testing.T) {
			st := newStoreFake()
			h := newTestHandler(st, testExport(8192), testTimeouts())
			server, client := pipe(t)
			serveConn(h, server)
			completeHandshake(t, client, testExportName)

			sendRequest(t, client, &Request{Type: CmdRead, Flags: flag, Cookie: 1, Offset: 0, Length: 4096})
			rep := readReplyHeader(t, client)
			if rep.Error != ErrEINVAL {
				t.Fatalf("READ with %s reply error = %d, want ErrEINVAL (%d)", flagName(flag), rep.Error, uint32(ErrEINVAL))
			}
			assertNoPayload(t, client, 100*time.Millisecond)
			if st.reads != 0 {
				t.Fatalf("the store saw %d reads for a request with %s set", st.reads, flagName(flag))
			}
		})
	}
}

// A command that arrives while the adapter is draining is answered with
// NBD_ESHUTDOWN rather than served: it is the protocol's "reconnect" signal,
// which is what the kernel's dead-connection timeout waits for. The connection
// is deliberately NOT closed on the first draining observation — closing
// immediately is what makes the client's kernel error the device instead of
// pausing it, and the base adapter's drain deadline is the backstop.
func TestConnection_DrainingAnswersESHUTDOWN(t *testing.T) {
	st := newStoreFake()
	st.data[testPayloadID] = make([]byte, 4096)

	var draining atomic.Bool
	h := NewHandler(exporterFor(testExport(8192)), resolverFor(st), nil,
		Identity{UID: 65534, GID: 65534}, testTimeouts(), draining.Load)

	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	draining.Store(true)

	sendRequest(t, client, &Request{Type: CmdRead, Cookie: 9, Offset: 0, Length: 4096})
	rep := readReplyHeader(t, client)
	if rep.Error != ErrESHUTDOWN {
		t.Fatalf("READ while draining reply error = %d, want ErrESHUTDOWN (%d)", rep.Error, uint32(ErrESHUTDOWN))
	}
	if rep.Cookie != 9 {
		t.Fatalf("draining reply cookie = %d, want 9", rep.Cookie)
	}
	// The request must not have reached the store: it is answered, not served.
	if st.reads != 0 {
		t.Fatalf("the store saw %d reads for a command answered while draining", st.reads)
	}
	// The connection stays open, so a second command is still answerable.
	sendRequest(t, client, &Request{Type: CmdRead, Cookie: 10, Offset: 0, Length: 4096})
	if rep := readReplyHeader(t, client); rep.Error != ErrESHUTDOWN {
		t.Fatalf("second draining READ reply error = %d, want ErrESHUTDOWN (%d)", rep.Error, uint32(ErrESHUTDOWN))
	}
}

// A WRITE that arrives while draining must have its payload consumed, or the
// next header is parsed out of payload bytes and the connection dies on a later
// command with NBD_EBADMAGIC.
//
// This is the case TestConnection_DrainingAnswersESHUTDOWN does not reach: it
// only sends READ, which carries no payload, so the drain is never exercised.
// A WRITE is the one command where answering without draining corrupts the
// stream — and it is the command a rolling restart of an fsync-heavy workload
// will actually hit.
func TestConnection_DrainingWriteDrainsPayload(t *testing.T) {
	st := newStoreFake()
	st.data[testPayloadID] = make([]byte, 4096)

	var draining atomic.Bool
	h := NewHandler(exporterFor(testExport(8192)), resolverFor(st), nil,
		Identity{UID: 65534, GID: 65534}, testTimeouts(), draining.Load)

	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	draining.Store(true)

	// A real WRITE: the header and its payload both go out, because the spec
	// requires a client to send the payload whatever the server answers.
	payload := make([]byte, 512)
	for i := range payload {
		payload[i] = 0xAB
	}
	sendRequest(t, client, &Request{Type: CmdWrite, Cookie: 11, Offset: 0, Length: uint32(len(payload))})
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("writing payload: %v", err)
	}

	rep := readReplyHeader(t, client)
	if rep.Error != ErrESHUTDOWN {
		t.Fatalf("WRITE while draining reply error = %d, want ErrESHUTDOWN (%d)", rep.Error, uint32(ErrESHUTDOWN))
	}
	if rep.Cookie != 11 {
		t.Fatalf("draining reply cookie = %d, want 11", rep.Cookie)
	}
	// Answered, not served: the payload must never reach the store.
	if st.writes != 0 {
		t.Fatalf("the store saw %d writes for a command answered while draining", st.writes)
	}

	// The assertion the bug fails. If the payload was not drained, the server
	// reads 0xabababab as the next request magic and closes the connection, so
	// this second command gets no reply at all. The deadline makes that a fast
	// failure with a message instead of a 60-second hang until the Go test
	// timeout.
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("arming the read deadline: %v", err)
	}
	sendRequest(t, client, &Request{Type: CmdRead, Cookie: 12, Offset: 0, Length: 4096})
	rep = readReplyHeader(t, client)
	if rep.Error != ErrESHUTDOWN {
		t.Fatalf("command after a draining WRITE reply error = %d, want ErrESHUTDOWN (%d): the WRITE payload was not drained", rep.Error, uint32(ErrESHUTDOWN))
	}
	if rep.Cookie != 12 {
		t.Fatalf("reply cookie after a draining WRITE = %d, want 12", rep.Cookie)
	}
}

func flagName(f CommandFlags) string {
	switch f {
	case FlagFUA:
		return "NBD_CMD_FLAG_FUA"
	case FlagNoHole:
		return "NBD_CMD_FLAG_NO_HOLE"
	case FlagDF:
		return "NBD_CMD_FLAG_DF"
	case FlagReqOne:
		return "NBD_CMD_FLAG_REQ_ONE"
	case FlagFastZero:
		return "NBD_CMD_FLAG_FAST_ZERO"
	default:
		return "flag"
	}
}

// A client that sends a partial header and stalls must not hold the connection
// open forever, and must not make the server allocate for a length it never
// sent.
//
// Idle is the timer that bounds this wait: the connection has been silent since
// the four bytes went out, so the arm that covers the wait for a header is the
// Idle one. It is set below Read so the test would still fail if the arm read
// Read — see TestConnection_IdleDeadlineClosesASilentConnection for the same
// split with nothing sent at all.
func TestConnection_PartialHeaderTimesOut(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192),
		Timeouts{Read: 5 * time.Second, Write: 5 * time.Second, Idle: 200 * time.Millisecond})
	server, client := pipe(t)
	done := serveConn(h, server)
	completeHandshake(t, client, testExportName)

	// Four bytes of the 28-byte header, then silence.
	if _, err := client.Write([]byte{0x25, 0x60, 0x95, 0x13}); err != nil {
		t.Fatalf("write partial header: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil after a partial header; a stalled client must end the connection")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return within 2s of a 200ms idle timeout: Timeouts.Idle is not armed")
	}
	assertNoPayload(t, client, 100*time.Millisecond)
}

func TestMapError_ClassifiesStoreAndContextErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want NBDError
	}{
		{"nil", nil, 0},
		{"store closed", engine.ErrStoreClosed, ErrEIO},
		{"not durable yet", common.ErrNotDurableYet, ErrEIO},
		{"no space", &merrs.StoreError{Code: merrs.ErrNoSpace, Message: "full"}, ErrENOSPC},
		{"permission denied", &merrs.StoreError{Code: merrs.ErrPermissionDenied, Message: "nope"}, ErrEPERM},
		// A RAW sentinel, not an already-wrapped StoreError: this case is what
		// makes normalization load-bearing. ClassifyBlockStoreError maps
		// journal.ErrLocalStoreFull to the no-space code, and the only thing
		// that turns that raw error into a *merrs.StoreError is
		// NormalizeBlockStoreError. Drop that call and this case collapses to
		// ErrEIO, which is the disk-full-reports-as-I/O bug the mapping exists
		// to prevent.
		{"raw local store full", journal.ErrLocalStoreFull, ErrENOSPC},
		{"wrapped raw local store full", fmt.Errorf("write: %w", journal.ErrLocalStoreFull), ErrENOSPC},
		{"canceled", context.Canceled, ErrESHUTDOWN},
		{"deadline exceeded", context.DeadlineExceeded, ErrESHUTDOWN},
		{"opaque", errors.New("opaque engine fault"), ErrEIO},
		{"wrapped store closed", fmt.Errorf("read: %w", engine.ErrStoreClosed), ErrEIO},
		{"wrapped no space", fmt.Errorf("write: %w", &merrs.StoreError{Code: merrs.ErrNoSpace}), ErrENOSPC},
		// A store failure wrapped in a cancellation must report the store
		// failure: the operator needs to know the store is gone, not that a
		// context expired.
		{"store closed under a canceled context", fmt.Errorf("op: %w (%w)", engine.ErrStoreClosed, context.Canceled), ErrEIO},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapError(tt.err); got != tt.want {
				t.Fatalf("mapError(%v) = %d, want %d", tt.err, got, uint32(tt.want))
			}
		})
	}
}

// The auth context is built per command from the configured synthetic identity
// and the export's read-only ceiling. It is never cached: it embeds the
// command's context, so a cached one would carry the first command's deadline
// and cancellation into every later command on the connection.
func TestHandler_AuthContextCarriesIdentityAndReadOnlyCeiling(t *testing.T) {
	h := NewHandler(exporterFor(testExport(8192)), resolverFor(newStoreFake()), nil,
		Identity{UID: 1234, GID: 5678}, testTimeouts(), func() bool { return false })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	authCtx := h.authContext(ctx, &Export{ReadOnly: true})
	if authCtx.Context != ctx {
		t.Fatal("auth context does not carry the command's context")
	}
	if authCtx.Identity == nil || authCtx.Identity.UID == nil || authCtx.Identity.GID == nil {
		t.Fatal("auth context has no identity")
	}
	if *authCtx.Identity.UID != 1234 || *authCtx.Identity.GID != 5678 {
		t.Fatalf("auth context identity = %d/%d, want 1234/5678", *authCtx.Identity.UID, *authCtx.Identity.GID)
	}
	if !authCtx.ShareReadOnly {
		t.Fatal("auth context does not apply the export's read-only ceiling")
	}

	// A second call must not share the first one's identity pointers.
	other := h.authContext(context.Background(), &Export{})
	if other.Identity.UID == authCtx.Identity.UID {
		t.Fatal("two auth contexts share one identity pointer")
	}
	if other.ShareReadOnly {
		t.Fatal("a writable export produced a read-only auth context")
	}
}

// serveHandler starts h over a pipe, completes the handshake, and returns the
// client end and the channel carrying serve's result.
func serveHandler(t *testing.T, h *Handler, exportName string) (net.Conn, chan error) {
	t.Helper()
	server, client := pipe(t)
	done := serveConn(h, server)
	completeHandshake(t, client, exportName)
	return client, done
}

// TestConnection_WritePersistsBytes is the only test that proves a WRITE
// round-trips, so it asserts the bytes rather than the error code.
func TestConnection_WritePersistsBytes(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	want := make([]byte, 4096)
	for i := range want {
		want[i] = byte(i%251) + 1
	}

	rep := writeThroughClient(t, client, 8192, want, 0)
	if rep.Error != 0 {
		t.Fatalf("WRITE reply error = %d, want 0", rep.Error)
	}
	if st.writes != 1 {
		t.Fatalf("the store saw %d writes, want 1", st.writes)
	}

	rep, got := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 8, Offset: 8192, Length: 4096,
	})
	if rep.Error != 0 {
		t.Fatalf("READ after WRITE returned error %d, want 0", rep.Error)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("READ after WRITE did not return the bytes that were written")
	}
}

// A write past the end is NBD_ENOSPC, not NBD_EINVAL: the spec reserves EINVAL
// for a read or trim past the end, because a write past the end means the
// client's view of the device is too large while a read past the end is a
// malformed request. The export size is fixed, so a write never grows it.
func TestConnection_WritePastEndIsENOSPC(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	// The last byte of the export is addressable, so one byte more is the
	// smallest request that is out of range.
	rep := writeThroughClient(t, client, 8192-4096, make([]byte, 4097), 0)
	if rep.Error != ErrENOSPC {
		t.Fatalf("WRITE past the export end reply error = %d, want ErrENOSPC (%d)", rep.Error, uint32(ErrENOSPC))
	}
	if st.writes != 0 {
		t.Fatalf("the store saw %d writes for a request past the export end", st.writes)
	}
}

// A zero-length write carries no payload, so the handler must not drain one:
// draining would consume the next request header and park the connection until
// the read deadline. The READ that follows on the same connection is what
// proves the zero-length path left the stream where it found it.
func TestConnection_WriteZeroLengthIsEINVAL(t *testing.T) {
	st := newStoreFake()
	st.data[testPayloadID] = make([]byte, 4096)
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	sendRequest(t, client, &Request{Type: CmdWrite, Cookie: 11, Length: 0})
	rep := readReplyHeader(t, client)
	if rep.Error != ErrEINVAL {
		t.Fatalf("zero-length WRITE reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	if st.writes != 0 {
		t.Fatalf("the store saw %d writes for a zero-length request", st.writes)
	}
	assertNoPayload(t, client, 100*time.Millisecond)

	rep, got := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 12, Offset: 0, Length: 4096,
	})
	if rep.Error != 0 {
		t.Fatalf("READ after a zero-length WRITE returned error %d; the zero-length path consumed the next header", rep.Error)
	}
	if len(got) != 4096 {
		t.Fatalf("READ after a zero-length WRITE returned %d bytes, want 4096", len(got))
	}
}

// A rejected WRITE still has its payload in flight — the spec requires the
// client to follow the request header with length bytes unconditionally — so
// the handler must consume them before reading the next header. Without the
// drain the bytes that follow are read as a request header, and the symptom
// appears on an unrelated command.
func TestConnection_RejectedWriteKeepsTheStreamSynchronized(t *testing.T) {
	want := make([]byte, 4096)
	for i := range want {
		want[i] = 0x5a
	}
	st := newStoreFake()
	st.data[testPayloadID] = want
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	// 4096 bytes at offset 8192 on an 8192-byte export: refused on the range
	// check, with the payload already sent.
	rep := writeThroughClient(t, client, 8192, make([]byte, 4096), 0)
	if rep.Error != ErrENOSPC {
		t.Fatalf("WRITE past the export end reply error = %d, want ErrENOSPC (%d)", rep.Error, uint32(ErrENOSPC))
	}

	rep, got := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 13, Offset: 0, Length: 4096,
	})
	if rep.Error != 0 {
		t.Fatalf("READ after a rejected WRITE returned error %d; the rejected payload was read as the next request header", rep.Error)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("READ after a rejected WRITE returned the wrong bytes")
	}
}

// A client that declares a gigabyte and sends nothing must be answered and then
// dropped, not allocated for and not held open. The reply precedes the drain,
// so the client gets the error it can act on instead of only a closed socket;
// Serve then ends on the drain's read deadline, which is the correct outcome.
func TestConnection_RejectedHugeWriteDoesNotAllocate(t *testing.T) {
	const declared = 1 << 30

	st := newStoreFake()
	h := newTestHandler(st, testExport(1<<20),
		Timeouts{Read: time.Second, Write: 5 * time.Second, Idle: 5 * time.Second})
	client, done := serveHandler(t, h, testExportName)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	sendRequest(t, client, &Request{Type: CmdWrite, Cookie: 14, Offset: 0, Length: declared})
	// A little of the declared payload, then silence.
	if _, err := client.Write(make([]byte, 4096)); err != nil {
		t.Fatalf("write partial payload: %v", err)
	}

	rep := readReplyHeader(t, client)
	if rep.Error != ErrENOSPC {
		t.Fatalf("huge WRITE reply error = %d, want ErrENOSPC (%d)", rep.Error, uint32(ErrENOSPC))
	}
	if st.writes != 0 {
		t.Fatalf("the store saw %d writes for a request past the export end", st.writes)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil after the drain timed out; the connection must close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the drain's read deadline")
	}

	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew >= declared {
		t.Fatalf("the server allocated %d bytes for a request that declared %d", grew, declared)
	}
}

// Distinct from the huge-write case: here the client stopped mid-payload on a
// request the server had accepted, so the stream position is unknown and the
// connection must close rather than report success for bytes it never saw.
func TestConnection_WriteTruncatedPayloadIsAnError(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, done := serveHandler(t, h, testExportName)

	sendRequest(t, client, &Request{Type: CmdWrite, Cookie: 15, Offset: 0, Length: 4096})
	if _, err := client.Write(make([]byte, 100)); err != nil {
		t.Fatalf("write partial payload: %v", err)
	}
	_ = client.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil after the client stopped mid-payload; the reply must not claim success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return after the client stopped mid-payload")
	}
	if st.writes != 0 {
		t.Fatalf("the store saw %d writes for a payload the server never fully received", st.writes)
	}
}

// The payload read must be bounded by Read, not by whatever budget the header
// read left behind on the connection.
//
// The fixture is the INVERSE of the header test: Read is short and Idle is long,
// so a payload read that inherited the header's Idle budget is still waiting at
// 1s. The client is deliberately NOT closed — closing it makes the read return on
// EOF, which is why the committed TestConnection_WriteTruncatedPayloadIsAnError
// (equal budgets, and it closes) passes with the arm deleted.
func TestConnection_PayloadReadIsBoundedByRead(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192),
		Timeouts{Read: 200 * time.Millisecond, Write: 5 * time.Second, Idle: 5 * time.Second})
	client, done := serveHandler(t, h, testExportName)

	// A header declaring 4096 bytes of payload, then 100 of them, then silence on
	// an open connection: only a deadline can end this read.
	sendRequest(t, client, &Request{Type: CmdWrite, Cookie: 16, Offset: 0, Length: 4096})
	if _, err := client.Write(make([]byte, 100)); err != nil {
		t.Fatalf("write partial payload: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil after a partial payload; the read must fail")
		}
	case <-time.After(time.Second):
		t.Fatal("serve did not return within 1s of a 200ms read timeout: the payload read is not bounded by Timeouts.Read")
	}
	if st.writes != 0 {
		t.Fatalf("the store saw %d writes for a payload the server never fully received", st.writes)
	}
	_ = client.Close()
}

// FUA is the assertion that makes the flag mean something: the spec says the
// server "MUST NOT send its reply to that command until all write operations
// ... have been completed and persisted to non-volatile storage", so the commit
// has to run before the reply is written.
//
// The check runs inside the store's Flush, on the server's goroutine, and hands
// the client read to the test goroutine over a channel — the reply is not in
// flight yet, so a read that succeeds means the reply was written first.
func TestConnection_WriteWithFUAIsDurableBeforeReply(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()

	atCommit := make(chan struct{})
	verdict := make(chan struct{})
	st.onFlush = func() {
		atCommit <- struct{}{}
		<-verdict
	}

	h := f.newHandler(t, st)
	client, done := serveHandler(t, h, f.exp.Name)

	sendRequest(t, client, &Request{
		Type: CmdWrite, Flags: FlagFUA, Cookie: 16, Offset: 0, Length: 4096,
	})
	if _, err := client.Write(make([]byte, 4096)); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	select {
	case <-atCommit:
	case <-time.After(2 * time.Second):
		t.Fatal("the FUA write never reached Flush")
	}

	// The server is inside Flush, so a reply read that succeeds means the reply
	// was written before the commit.
	if err := client.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var hdr [16]byte
	_, err := io.ReadFull(client, hdr[:])
	sawReply := err == nil
	_ = client.SetReadDeadline(time.Time{})
	verdict <- struct{}{}

	if sawReply {
		t.Fatal("the reply was written before the FUA commit: a client that has seen the reply has not been given the durability it asked for")
	}
	if st.flushes != 1 {
		t.Fatalf("the store saw %d flushes for a FUA write, want 1", st.flushes)
	}

	_ = client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return after the client closed")
	}
}

// CommitAck: journal semantics — the store is durable, Flush reports the remote
// mirror as not finalized, and the commit is acknowledged because the local
// tier already survives a restart.
//
// It needs the real-metadata fixture: commit is common.FlushStableWrite, whose
// second half calls meta.FlushPendingWriteForFile, and newTestHandler passes nil
// for meta. The block store is still the fake, so st.flushes is the observable.
func TestConnection_FlushIsAckedWhenDurable(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	rep := flushThroughClient(t, client)
	if rep.Error != 0 {
		t.Fatalf("FLUSH on a durable store reply error = %d, want 0", rep.Error)
	}
	if st.flushes != 1 {
		t.Fatalf("the store saw %d flushes, want 1", st.flushes)
	}
}

// FLUSH promised durability and could not deliver it, so there is no honest
// success to send: the commit rule's ErrNotDurableYet maps to NBD_EIO.
func TestConnection_FlushIsEIOWhenNotDurable(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	st.localDurable = false
	st.remoteDurable = false
	st.requireDurable = true

	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	rep := flushThroughClient(t, client)
	if rep.Error != ErrEIO {
		t.Fatalf("FLUSH on a non-durable store reply error = %d, want ErrEIO (%d)", rep.Error, uint32(ErrEIO))
	}
}

// The spec requires offset and length to be zero on FLUSH. There is no payload
// to drain — the command has none — so the connection stays usable.
func TestConnection_FlushWithNonZeroLengthIsEINVAL(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	sendRequest(t, client, &Request{Type: CmdFlush, Cookie: 17, Length: 4096})
	rep := readReplyHeader(t, client)
	if rep.Error != ErrEINVAL {
		t.Fatalf("FLUSH with a nonzero length reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	if st.flushes != 0 {
		t.Fatalf("the store saw %d flushes for a malformed FLUSH", st.flushes)
	}

	if rep := flushThroughClient(t, client); rep.Error != 0 {
		t.Fatalf("FLUSH after a malformed one reply error = %d, want 0", rep.Error)
	}
}

// A read-only export still answers FLUSH successfully. Refusing would make a
// client error a device that has nothing to flush, and the command carries no
// data to protect.
func TestConnection_FlushOnReadOnlyExportIsPermitted(t *testing.T) {
	f := newWriteVolumeFixture(t)
	exp := *f.exp
	exp.ReadOnly = true

	st := newStoreFake()
	h := NewHandler(exporterFor(&exp), resolverFor(st), f.rt.GetMetadataService(),
		integrationIdentity, testTimeouts(), func() bool { return false })
	client, _ := serveHandler(t, h, exp.Name)

	rep := flushThroughClient(t, client)
	if rep.Error != 0 {
		t.Fatalf("FLUSH on a read-only export reply error = %d, want 0", rep.Error)
	}
	if st.flushes != 1 {
		t.Fatalf("the store saw %d flushes, want 1", st.flushes)
	}
}

// trimThroughClient sends one TRIM and returns the reply. TRIM carries no
// payload, so there is nothing to send after the header.
func trimThroughClient(t *testing.T, client net.Conn, offset uint64, length uint32, flags CommandFlags) *Reply {
	t.Helper()
	sendRequest(t, client, &Request{
		Type: CmdTrim, Flags: flags, Cookie: 0x71, Offset: offset, Length: length,
	})
	return readReplyHeader(t, client)
}

// writeZeroesThroughClient sends one WRITE_ZEROES and returns the reply. Like
// TRIM it carries no payload.
func writeZeroesThroughClient(t *testing.T, client net.Conn, offset uint64, length uint32, flags CommandFlags) *Reply {
	t.Helper()
	sendRequest(t, client, &Request{
		Type: CmdWriteZeroes, Flags: flags, Cookie: 0x72, Offset: offset, Length: length,
	})
	return readReplyHeader(t, client)
}

// A read-only export refuses a TRIM with NBD_EPERM. The export still advertises
// SEND_TRIM while read-only — handshake.go sets it unconditionally and only adds
// READ_ONLY — so this is not a client violating the advertisement; it is the
// case the spec's error list names explicitly: "it SHOULD return NBD_EPERM if it
// receives a write or trim request on a read-only export".
func TestConnection_TrimIsEPERMOnReadOnlyExport(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, readOnlyExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	if rep := trimThroughClient(t, client, 0, 4096, 0); rep.Error != ErrEPERM {
		t.Fatalf("TRIM on a read-only export reply error = %d, want ErrEPERM (%d)", rep.Error, uint32(ErrEPERM))
	}
	if st.punches != 0 {
		t.Fatalf("the store saw %d punches for a refused TRIM", st.punches)
	}
}

// The trimmed range reads back as zeros and the range before it is untouched.
// It needs the real-metadata fixture: the handler calls h.meta.PunchHole, and
// newTestHandler passes nil for meta.
func TestConnection_TrimThenReadIsZeros(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	want := make([]byte, 8192)
	for i := range want {
		want[i] = byte(i%251) + 1
	}
	if rep := writeThroughClient(t, client, 0, want, 0); rep.Error != 0 {
		t.Fatalf("WRITE reply error = %d, want 0", rep.Error)
	}

	if rep := trimThroughClient(t, client, 4096, 4096, 0); rep.Error != 0 {
		t.Fatalf("TRIM reply error = %d, want 0", rep.Error)
	}
	if st.punches != 1 {
		t.Fatalf("the store saw %d punches, want 1", st.punches)
	}

	rep, kept := readReplyAndPayload(t, client, &Request{Type: CmdRead, Cookie: 0x73, Offset: 0, Length: 4096})
	if rep.Error != 0 {
		t.Fatalf("READ below the trimmed range returned error %d, want 0", rep.Error)
	}
	if !bytes.Equal(kept, want[:4096]) {
		t.Fatal("READ below the trimmed range did not return the bytes that were written")
	}

	rep, trimmed := readReplyAndPayload(t, client, &Request{Type: CmdRead, Cookie: 0x74, Offset: 4096, Length: 4096})
	if rep.Error != 0 {
		t.Fatalf("READ of the trimmed range returned error %d, want 0", rep.Error)
	}
	if !bytes.Equal(trimmed, make([]byte, 4096)) {
		t.Fatal("READ of the trimmed range did not return zeros")
	}
}

// A TRIM whose length runs past the export end is refused, not clamped. The
// spec: "it SHOULD return NBD_EINVAL if it receives a read or trim request
// including one or more sectors beyond the size of the device." READ already
// answers EINVAL past the end, and the same condition deserves the same answer.
func TestConnection_TrimPastEndIsEINVAL(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	// 4096 bytes starting at 4096 ends exactly at the export end and is
	// permitted; one byte more is the smallest out-of-range request.
	if rep := trimThroughClient(t, client, 4096, 4097, 0); rep.Error != ErrEINVAL {
		t.Fatalf("TRIM past the export end reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	if st.punches != 0 {
		t.Fatalf("the store saw %d punches for a request past the export end", st.punches)
	}
}

// An offset at or past the export end is refused on its own check. It is
// asserted separately because it is the case a subtraction-based clamp
// underflows on: end-offset with end == size and offset > size wraps to a value
// near 2^64, which the engine's overflow guard then reports as a confusing
// "punch range overflow" instead of the right answer.
func TestConnection_TrimOffsetPastEndIsEINVAL(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	if rep := trimThroughClient(t, client, 8192, 100, 0); rep.Error != ErrEINVAL {
		t.Fatalf("TRIM at the export end reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	if rep := trimThroughClient(t, client, 10000, 100, 0); rep.Error != ErrEINVAL {
		t.Fatalf("TRIM past the export end reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	if st.punches != 0 {
		t.Fatalf("the store saw %d punches for an out-of-range TRIM", st.punches)
	}
}

// WRITE_ZEROES past the end is NBD_ENOSPC, not EINVAL: the spec gives it its own
// rule — "The server SHOULD return NBD_ENOSPC if it receives a write zeroes
// request including one or more sectors beyond the size of the device."
func TestConnection_WriteZeroesPastEndIsENOSPC(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	if rep := writeZeroesThroughClient(t, client, 4096, 4097, 0); rep.Error != ErrENOSPC {
		t.Fatalf("WRITE_ZEROES past the export end reply error = %d, want ErrENOSPC (%d)", rep.Error, uint32(ErrENOSPC))
	}
	if st.punches != 0 {
		t.Fatalf("the store saw %d punches for a request past the export end", st.punches)
	}
}

// NO_HOLE must be accepted, not rejected: "The server MUST support the use of
// this flag if it advertises NBD_FLAG_SEND_WRITE_ZEROES", and the adapter does.
// It is the client asking that the range be genuinely allocated rather than left
// sparse, and PunchHole already zero-overwrites rather than leaving a hole, so
// honouring it takes no extra work. Refusing it would break a MUST on the
// strength of the rule that makes NO_HOLE invalid on WRITE.
func TestConnection_WriteZeroesWithNoHoleIsAccepted(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	if rep := writeZeroesThroughClient(t, client, 0, 4096, FlagNoHole); rep.Error != 0 {
		t.Fatalf("WRITE_ZEROES with NBD_CMD_FLAG_NO_HOLE reply error = %d, want 0", rep.Error)
	}
	if st.punches != 1 {
		t.Fatalf("the store saw %d punches, want 1", st.punches)
	}
}

// A TRIM whose block-store punch fails reaches the client as NBD_EIO, mapped by
// mapError from whatever the store returned. It is a real path rather than a
// hypothetical one: engine.Store.PunchHole is the call that fails when the local
// store is closed underneath a live connection, and the metadata punch that
// precedes it would have succeeded, so nothing else reports the failure.
func TestConnection_TrimPunchFailureIsEIO(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	st.punchErr = errors.New("boom")
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	if rep := trimThroughClient(t, client, 0, 4096, 0); rep.Error != ErrEIO {
		t.Fatalf("TRIM with a failing store punch reply error = %d, want ErrEIO (%d)", rep.Error, uint32(ErrEIO))
	}
	if st.punches != 1 {
		t.Fatalf("the store saw %d punches, want 1", st.punches)
	}
}

// A zero-length TRIM is NBD_EINVAL, the same answer READ, WRITE and
// WRITE_ZEROES give. TRIM carries no payload either way, so the connection must
// stay usable for the next command.
func TestConnection_TrimZeroLengthIsEINVAL(t *testing.T) {
	st := newStoreFake()
	st.data[testPayloadID] = make([]byte, 4096)
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	if rep := trimThroughClient(t, client, 0, 0, 0); rep.Error != ErrEINVAL {
		t.Fatalf("zero-length TRIM reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	if st.punches != 0 {
		t.Fatalf("the store saw %d punches for a zero-length request", st.punches)
	}

	// The READ proves the zero-length path left the stream where it found it: a
	// handler that drained a payload it never received would consume this
	// header.
	rep, got := readReplyAndPayload(t, client, &Request{Type: CmdRead, Cookie: 0x75, Offset: 0, Length: 4096})
	if rep.Error != 0 {
		t.Fatalf("READ after a zero-length TRIM returned error %d; the zero-length path consumed the next header", rep.Error)
	}
	if len(got) != 4096 {
		t.Fatalf("READ after a zero-length TRIM returned %d bytes, want 4096", len(got))
	}
}

// A zero-length WRITE_ZEROES is NBD_EINVAL, matching READ and WRITE. No payload
// either way, so the connection must stay usable.
func TestConnection_WriteZeroesZeroLengthIsEINVAL(t *testing.T) {
	st := newStoreFake()
	st.data[testPayloadID] = make([]byte, 4096)
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	if rep := writeZeroesThroughClient(t, client, 0, 0, 0); rep.Error != ErrEINVAL {
		t.Fatalf("zero-length WRITE_ZEROES reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	if st.punches != 0 {
		t.Fatalf("the store saw %d punches for a zero-length request", st.punches)
	}

	rep, got := readReplyAndPayload(t, client, &Request{Type: CmdRead, Cookie: 0x76, Offset: 0, Length: 4096})
	if rep.Error != 0 {
		t.Fatalf("READ after a zero-length WRITE_ZEROES returned error %d; the zero-length path consumed the next header", rep.Error)
	}
	if len(got) != 4096 {
		t.Fatalf("READ after a zero-length WRITE_ZEROES returned %d bytes, want 4096", len(got))
	}
}

// The assertion that keeps WRITE_ZEROES from becoming a zero-buffer WriteAt: on
// a content-addressed store that call MATERIALIZES real zero chunks, so a
// filesystem's discard path would allocate storage instead of freeing it. The
// punches/writes counters are the guard, and the read back is what proves the
// range is zeros either way.
func TestConnection_WriteZeroesThenReadIsZeros(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	// Pre-populated rather than written through the handler, so st.writes stays
	// zero for a reason the assertion can see: a WRITE_ZEROES that went through
	// WriteAt would increment it.
	nonzero := make([]byte, 4096)
	for i := range nonzero {
		nonzero[i] = byte(i%251) + 1
	}
	st.data[string(f.exp.PayloadID)] = nonzero

	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	if rep := writeZeroesThroughClient(t, client, 0, 4096, 0); rep.Error != 0 {
		t.Fatalf("WRITE_ZEROES reply error = %d, want 0", rep.Error)
	}
	if st.punches != 1 {
		t.Fatalf("the store saw %d punches, want 1", st.punches)
	}
	if st.writes != 0 {
		t.Fatalf("the store saw %d writes, want 0: WRITE_ZEROES must punch, not write a zero buffer", st.writes)
	}

	rep, got := readReplyAndPayload(t, client, &Request{Type: CmdRead, Cookie: 0x77, Offset: 0, Length: 4096})
	if rep.Error != 0 {
		t.Fatalf("READ of the zeroed range returned error %d, want 0", rep.Error)
	}
	if !bytes.Equal(got, make([]byte, 4096)) {
		t.Fatal("READ of the zeroed range did not return zeros")
	}
}

// WRITE_ZEROES is a write, so a read-only export refuses it.
func TestConnection_WriteZeroesOnReadOnlyExportIsEPERM(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, readOnlyExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	if rep := writeZeroesThroughClient(t, client, 0, 4096, 0); rep.Error != ErrEPERM {
		t.Fatalf("WRITE_ZEROES on a read-only export reply error = %d, want ErrEPERM (%d)", rep.Error, uint32(ErrEPERM))
	}
	if st.punches != 0 {
		t.Fatalf("the store saw %d punches for a refused WRITE_ZEROES", st.punches)
	}
}

// Read-only means writes are refused, not that the device is unusable: a client
// mounting a read-only export still reads it.
func TestConnection_ReadOnReadOnlyExportSucceeds(t *testing.T) {
	want := make([]byte, 4096)
	for i := range want {
		want[i] = byte(i%251) + 1
	}
	st := newStoreFake()
	st.data[testPayloadID] = want

	h := newTestHandler(st, readOnlyExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	rep, got := readReplyAndPayload(t, client, &Request{Type: CmdRead, Cookie: 0x78, Offset: 0, Length: 4096})
	if rep.Error != 0 {
		t.Fatalf("READ on a read-only export returned error %d, want 0", rep.Error)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("READ on a read-only export did not return the stored bytes")
	}
}

// A WRITE on a read-only export is NBD_EPERM, checked before the range and the
// store: a read-only export refuses the write wherever it lands. The payload
// still goes out and is still drained, so the connection stays synchronized.
func TestConnection_WriteOnReadOnlyExportIsEPERM(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, readOnlyExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	if rep := writeThroughClient(t, client, 0, make([]byte, 4096), 0); rep.Error != ErrEPERM {
		t.Fatalf("WRITE on a read-only export reply error = %d, want ErrEPERM (%d)", rep.Error, uint32(ErrEPERM))
	}
	if st.writes != 0 {
		t.Fatalf("the store saw %d writes for a refused request", st.writes)
	}

	rep, got := readReplyAndPayload(t, client, &Request{Type: CmdRead, Cookie: 0x79, Offset: 0, Length: 4096})
	if rep.Error != 0 {
		t.Fatalf("READ after a refused WRITE returned error %d; the refused payload was read as the next request header", rep.Error)
	}
	if len(got) != 4096 {
		t.Fatalf("READ after a refused WRITE returned %d bytes, want 4096", len(got))
	}
}

// Idle bounds the wait for a request header, so a connection that completes its
// handshake and then sends nothing is closed by the Idle timer.
//
// The values are the point: Idle is SHORTER than Read, so the test fails if the
// arm reads Read instead. With Idle == Read or Idle > Read it would pass whether
// or not the arm exists — which is why the committed
// TestConnection_PartialHeaderTimesOut (Idle 5s / Read 200ms) cannot catch the
// missing arm.
func TestConnection_IdleDeadlineClosesASilentConnection(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192),
		Timeouts{Read: 5 * time.Second, Write: 5 * time.Second, Idle: 200 * time.Millisecond})
	client, done := serveHandler(t, h, testExportName)

	// Silence from here: the handshake is complete and no request follows.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil after the idle timeout; a silent connection must end")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return within 2s of a 200ms idle timeout: Timeouts.Idle is not armed")
	}
	_ = client.Close()
}

// A client that connects and then never speaks must not hold a connection open
// forever. Before the handshake is bounded it can: ServeHandshake arms no
// deadline, the base adapter's accept loop sets none on the accepted socket, and
// the dispatch loop's first arm only runs after the handshake has returned. The
// result is one goroutine and one file descriptor per idle socket, held
// indefinitely and before the client has named an export.
//
// The budget is Timeouts.Read, so the test sets it low and leaves Idle high: an
// arm that read Idle instead would let this test pass on the 2s guard without
// the handshake ever being bounded by the field it claims to use.
func TestConnection_HandshakeIsBounded(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192),
		Timeouts{Read: 200 * time.Millisecond, Write: 5 * time.Second, Idle: 30 * time.Second})

	server, client := pipe(t)
	done := serveConn(h, server)
	defer func() { _ = client.Close() }()

	// Read the greeting so the server is definitely past its first write and
	// parked in readClientFlags, then say nothing at all.
	readGreeting(t, client)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil for a client that never sent client flags")
		}
		t.Logf("handshake ended as expected: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return within 2s of a 200ms Read timeout: the handshake is unbounded")
	}
}

// The same bound, but for a client that completes the flags exchange and then
// stalls between options. This is the loop that has to poll the context and
// honour the deadline: the flags read is one ReadFull, while the option loop is
// unbounded in principle and runs until the client asks for an export.
func TestConnection_HandshakeOptionLoopIsBounded(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192),
		Timeouts{Read: 200 * time.Millisecond, Write: 5 * time.Second, Idle: 30 * time.Second})

	server, client := pipe(t)
	done := serveConn(h, server)
	defer func() { _ = client.Close() }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1) // NBD_FLAG_C_FIXED_NEWSTYLE, then nothing

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil for a client that stalled in the option loop")
		}
		t.Logf("handshake ended as expected: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return within 2s of a 200ms Read timeout: the option loop is unbounded")
	}
}

// A shutdown that begins during a handshake must end it promptly rather than
// waiting out the handshake budget. The deadline bounds a silent client; the
// context poll bounds a server that is stopping, and only the second one keeps a
// drain inside its own deadline when a half-open connection is in handshake.
func TestConnection_HandshakeHonoursContextCancellation(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192),
		Timeouts{Read: 30 * time.Second, Write: 30 * time.Second, Idle: 30 * time.Second})

	server, client := pipe(t)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.serve(ctx, server) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1) // now parked in the option loop

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve returned nil after the context was cancelled")
		}
		t.Logf("handshake ended as expected: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return within 2s of cancellation: the option loop does not poll ctx")
	}
}
