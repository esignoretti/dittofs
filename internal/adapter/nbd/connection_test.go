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
	data     map[string][]byte
	readErr  error
	writeErr error
	flushErr error
	reads    int
	writes   int
	flushes  int

	localDurable   bool
	remoteDurable  bool
	requireDurable bool

	// panicOnRead makes ReadAt panic instead of returning, so a test can drive
	// a panic from below the dispatcher — exactly where a real one would (a nil
	// map, a slice bounds error, a bad type assertion on an engine-returned
	// store).
	panicOnRead bool
}

func newStoreFake() *storeFake {
	return &storeFake{
		data:          map[string][]byte{},
		localDurable:  true,
		remoteDurable: true,
	}
}

func (s *storeFake) ReadAt(_ context.Context, id string, buf []byte, off uint64) (int, error) {
	s.reads++
	if s.panicOnRead {
		panic("storeFake: ReadAt panicked on purpose")
	}
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
	s.writes++
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
	b := s.data[id]
	for i := off; i < off+length && i < uint64(len(b)); i++ {
		b[i] = 0
	}
	return currentBlocks, nil
}

func (s *storeFake) Flush(_ context.Context, _ string) (*block.FlushResult, error) {
	s.flushes++
	if s.flushErr != nil {
		return nil, s.flushErr
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
// data lives at testPayloadID.
func testExport(size uint64) *Export {
	return &Export{Name: testExportName, Size: size, PayloadID: testPayloadID}
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

// TestConnection_WriteIsEINVALBeforeTask5 pins the dispatcher's default branch
// for a command the adapter advertises but does not yet serve. Task 5 replaces
// this with the real WRITE handler.
func TestConnection_WriteIsEINVALBeforeTask5(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192), testTimeouts())
	server, client := pipe(t)
	serveConn(h, server)
	completeHandshake(t, client, testExportName)

	sendRequest(t, client, &Request{Type: CmdWrite, Cookie: 5, Offset: 0, Length: 4096})
	rep := readReplyHeader(t, client)
	if rep.Error != ErrEINVAL {
		t.Fatalf("WRITE reply error = %d, want ErrEINVAL (%d) while the write path is unimplemented", rep.Error, uint32(ErrEINVAL))
	}
	if st.writes != 0 {
		t.Fatalf("the store saw %d writes for an unimplemented command", st.writes)
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
// the handler on a bare goroutine), so a panic that escapes here does not kill
// the connection — it kills the process, and with it every NFS and SMB
// connection. Both sibling adapters recover for this reason: NFS at
// pkg/adapter/nfs/connection.go:420, SMB at pkg/adapter/smb/connection.go:474.
//
// The assertion is "it came back", and that is sufficient: if the recover is
// missing the panic unwinds the goroutine and the test binary dies, so the
// failure mode is a crashed package rather than a failed assertion. The test
// deliberately does not wrap the call in its own recover — that would make it
// pass whether or not Conn.Serve has one.
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

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Conn.Serve did not return after the handler panicked")
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
func TestConnection_PartialHeaderTimesOut(t *testing.T) {
	st := newStoreFake()
	h := newTestHandler(st, testExport(8192),
		Timeouts{Read: 200 * time.Millisecond, Write: 5 * time.Second, Idle: 5 * time.Second})
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
		t.Fatal("serve did not return within 2s of a 200ms read timeout")
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
