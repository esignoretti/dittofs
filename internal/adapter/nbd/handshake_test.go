package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type fakeExporter struct {
	exports map[string]*Export
}

func (f *fakeExporter) Resolve(_ context.Context, name string) (*Export, error) {
	if e, ok := f.exports[name]; ok {
		return e, nil
	}
	return nil, ErrUnknownExport
}

func (f *fakeExporter) List(context.Context) ([]string, error) {
	names := make([]string, 0, len(f.exports))
	for n := range f.exports {
		names = append(names, n)
	}
	return names, nil
}

func newFakeExporter() *fakeExporter {
	return &fakeExporter{exports: map[string]*Export{
		"/export/boot": {Name: "/export/boot", Size: 1 << 30, PayloadID: "payload-1"},
	}}
}

// pipe returns a connected pair over a loopback TCP socket.
//
// net.Pipe would be simpler and is wrong here: it is synchronous and
// unbuffered, so a server Write blocks until the reader has consumed every
// byte. These tests deliberately read a reply header and assert on it without
// draining the payload that follows, which leaves the server blocked forever
// and turns a test that should fail loudly into one that hangs until the Go
// test timeout. A loopback socket buffers in the kernel, so a server can
// finish a reply the test has not read yet.
func pipe(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		ch <- accepted{c, err}
	}()

	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	a := <-ch
	if a.err != nil {
		t.Fatalf("accept: %v", a.err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = a.conn.Close()
	})
	return a.conn, client
}

// readGreeting consumes NBDMAGIC, IHAVEOPT and the 16-bit handshake flags, and
// returns the flags. Every client does this before anything else.
func readGreeting(t *testing.T, r io.Reader) uint16 {
	t.Helper()
	var buf [18]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if got := binary.BigEndian.Uint64(buf[0:8]); got != nbdMagic {
		t.Fatalf("greeting magic = %#x, want %#x", got, uint64(nbdMagic))
	}
	if got := binary.BigEndian.Uint64(buf[8:16]); got != ihaveoptMagic {
		t.Fatalf("IHAVEOPT = %#x, want %#x", got, uint64(ihaveoptMagic))
	}
	return binary.BigEndian.Uint16(buf[16:18])
}

func sendClientFlags(t *testing.T, w io.Writer, flags uint32) {
	t.Helper()
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], flags)
	if _, err := w.Write(buf[:]); err != nil {
		t.Fatalf("write client flags: %v", err)
	}
}

func sendOption(t *testing.T, w io.Writer, opt Option, data []byte) {
	t.Helper()
	h := &OptionHeader{Magic: ihaveoptMagic, Option: opt, Length: uint32(len(data))}
	if err := h.Encode(w); err != nil {
		t.Fatalf("encode option header: %v", err)
	}
	if len(data) > 0 {
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write option data: %v", err)
		}
	}
}

// goPayload builds an NBD_OPT_GO / NBD_OPT_INFO payload: name length, name,
// 16-bit count of information requests, then the requests.
func goPayload(name string, infos ...uint16) []byte {
	var buf bytes.Buffer
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(name)))
	buf.Write(n[:])
	buf.WriteString(name)
	var c [2]byte
	binary.BigEndian.PutUint16(c[:], uint16(len(infos)))
	buf.Write(c[:])
	for _, i := range infos {
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], i)
		buf.Write(b[:])
	}
	return buf.Bytes()
}

func TestHandshake_GreetingAdvertisesFixedNewstyle(t *testing.T) {
	server, client := pipe(t)

	done := make(chan error, 1)
	go func() {
		_, err := ServeHandshake(context.Background(), server, newFakeExporter())
		done <- err
	}()

	flags := readGreeting(t, client)
	if flags&1 == 0 {
		t.Fatalf("handshake flags %#x do not set NBD_FLAG_FIXED_NEWSTYLE (bit 0)", flags)
	}
	// The server must not proceed before reading client flags.
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHandshake did not return after the client closed")
	}
}

func TestHandshake_GoReturnsExportInfoThenAck(t *testing.T) {
	server, client := pipe(t)

	got := make(chan *Export, 1)
	go func() {
		exp, err := ServeHandshake(context.Background(), server, newFakeExporter())
		if err != nil {
			got <- nil
			return
		}
		got <- exp
	}()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)                                 // NBD_FLAG_C_FIXED_NEWSTYLE
	sendOption(t, client, OptGO, goPayload("/export/boot", 0, 3)) // NBD_INFO_EXPORT, NBD_INFO_BLOCK_SIZE

	// NBD_INFO_EXPORT is mandatory before a successful ACK.
	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read first option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepInfo {
		t.Fatalf("first reply type = %#x, want NBD_REP_INFO (%#x)", typ, uint32(RepInfo))
	}
	data := make([]byte, binary.BigEndian.Uint32(hdr[16:20]))
	if _, err := io.ReadFull(client, data); err != nil {
		t.Fatalf("read INFO payload: %v", err)
	}
	if infoType := binary.BigEndian.Uint16(data[0:2]); infoType != 0 {
		t.Fatalf("info type = %d, want NBD_INFO_EXPORT (0)", infoType)
	}
	if size := binary.BigEndian.Uint64(data[2:10]); size != 1<<30 {
		t.Fatalf("advertised size = %d, want %d", size, 1<<30)
	}
	flags := TransmissionFlags(binary.BigEndian.Uint16(data[10:12]))
	if flags&FlagSendFlush == 0 {
		t.Error("NBD_FLAG_SEND_FLUSH not advertised: QEMU turns every guest fsync into a silent success without it")
	}
	if flags&FlagHASFlags == 0 {
		t.Error("NBD_FLAG_HAS_FLAGS not advertised")
	}
	if flags&(1<<8) == 0 {
		t.Error("NBD_FLAG_CAN_MULTI_CONN not advertised: a client that does not see it keeps to one connection, which is the single-connection throughput ceiling")
	}

	exp := <-got
	if exp == nil || exp.Name != "/export/boot" {
		t.Fatalf("resolved export = %+v, want /export/boot", exp)
	}
}

func TestHandshake_UnknownExportIsRepErrUnknown(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	sendOption(t, client, OptGO, goPayload("/export/nope"))

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepErrUnknown {
		t.Fatalf("reply type = %#x, want NBD_REP_ERR_UNKNOWN (%#x)", typ, uint32(RepErrUnknown))
	}
}

func TestHandshake_StartTLSIsUnsupported(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	sendOption(t, client, OptStartTLS, nil)

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepErrUnsup {
		t.Fatalf("reply type = %#x, want NBD_REP_ERR_UNSUP", typ)
	}
	// The connection must not be left half-negotiated: a second option must
	// still be answerable.
	sendOption(t, client, OptAbort, nil)
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read ABORT reply after STARTTLS refusal: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepAck {
		t.Fatalf("ABORT after STARTTLS refusal: type = %#x, want ACK", typ)
	}
}

// Review Focus 1: a name length that claims more bytes than the option carried.
// The naive read is make([]byte, nameLen) followed by a slice of the payload.
func TestHandshake_GoNameLengthExceedsOptionData(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)

	// Claim a 4096-byte name in an option that carries 8 bytes.
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[0:4], 4096)
	copy(payload[4:], "/export/")
	sendOption(t, client, OptGO, payload)

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepErrInvalid {
		t.Fatalf("reply type = %#x, want NBD_REP_ERR_INVALID (%#x)", typ, uint32(RepErrInvalid))
	}
}

// Review Focus 1, second shape: the option header's own data length is absurd.
// The server must not allocate what the client asked for.
func TestHandshake_HugeOptionLengthRejected(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)

	h := &OptionHeader{Magic: ihaveoptMagic, Option: OptGO, Length: 0xffffffff}
	if err := h.Encode(client); err != nil {
		t.Fatalf("encode: %v", err)
	}

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepErrTooBig {
		t.Fatalf("reply type = %#x, want NBD_REP_ERR_TOO_BIG (%#x)", typ, uint32(RepErrTooBig))
	}
}

func TestHandshake_ExportNameLegacyPath(t *testing.T) {
	server, client := pipe(t)

	got := make(chan *Export, 1)
	go func() {
		exp, err := ServeHandshake(context.Background(), server, newFakeExporter())
		if err != nil {
			got <- nil
			return
		}
		got <- exp
	}()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	// Legacy path: the name is the entire option payload and there is no
	// option reply — the export size and flags come back in lieu of one.
	sendOption(t, client, OptExportName, []byte("/export/boot"))

	var buf [10]byte
	if _, err := io.ReadFull(client, buf[:]); err != nil {
		t.Fatalf("read export reply: %v", err)
	}
	if size := binary.BigEndian.Uint64(buf[0:8]); size != 1<<30 {
		t.Fatalf("export size = %d, want %d", size, 1<<30)
	}
	if flags := TransmissionFlags(binary.BigEndian.Uint16(buf[8:10])); flags&FlagSendFlush == 0 {
		t.Error("SEND_FLUSH not advertised on the legacy path")
	}
	if exp := <-got; exp == nil || exp.Name != "/export/boot" {
		t.Fatalf("resolved export = %+v", exp)
	}
}

func TestHandshake_ListEnumeratesVolumes(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	sendOption(t, client, OptList, nil)

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read LIST reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepServer {
		t.Fatalf("LIST reply type = %#x, want NBD_REP_SERVER", typ)
	}
	length := binary.BigEndian.Uint32(hdr[16:20])
	data := make([]byte, length)
	if _, err := io.ReadFull(client, data); err != nil {
		t.Fatalf("read LIST entry: %v", err)
	}
	nameLen := binary.BigEndian.Uint32(data[0:4])
	if got := string(data[4 : 4+nameLen]); got != "/export/boot" {
		t.Fatalf("listed name = %q, want %q", got, "/export/boot")
	}
}

func TestHandshake_AbortIsAckedAndConnectionClosed(t *testing.T) {
	server, client := pipe(t)

	done := make(chan error, 1)
	go func() {
		_, err := ServeHandshake(context.Background(), server, newFakeExporter())
		done <- err
	}()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	sendOption(t, client, OptAbort, nil)

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read ABORT reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepAck {
		t.Fatalf("ABORT reply type = %#x, want ACK", typ)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ABORT returned nil; the caller cannot tell the session ended deliberately")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHandshake did not return after ABORT")
	}
}

func TestHandshake_UnsupportedOptionIsRefused(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	sendOption(t, client, OptStructuredReply, nil)

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepErrUnsup {
		t.Fatalf("NBD_OPT_STRUCTURED_REPLY reply = %#x, want NBD_REP_ERR_UNSUP", typ)
	}
}

func TestExport_TransmissionFlagsIncludeReadOnlyOnlyWhenSet(t *testing.T) {
	rw := &Export{Size: 1 << 20}
	if rw.TransmissionFlags()&FlagReadOnly != 0 {
		t.Error("writable export advertised NBD_FLAG_READ_ONLY")
	}
	ro := &Export{Size: 1 << 20, ReadOnly: true}
	if ro.TransmissionFlags()&FlagReadOnly == 0 {
		t.Error("read-only export did not advertise NBD_FLAG_READ_ONLY")
	}
	// The omissions are the load-bearing part. Bit 8 is deliberately absent
	// from this list: it is advertised, and TestExport_TransmissionFlagsAdvertiseCanMultiConn
	// asserts that it is.
	for _, f := range []TransmissionFlags{1 << 7, 1 << 9, 1 << 10, 1 << 11, 1 << 12, 1 << 4} {
		if ro.TransmissionFlags()&f != 0 {
			t.Errorf("export advertised flag bit %d, which the design excludes", f)
		}
	}
}

// TestExport_TransmissionFlagsAdvertiseCanMultiConn pins the flag that decides
// how many connections a client opens, and pins it for the read-only case too:
// the guarantee is about cache coherence and flush visibility, which a read-only
// export needs just as much (two readers must not disagree, and a FLUSH on one
// must be a barrier over a write another connection made before the export went
// read-only).
func TestExport_TransmissionFlagsAdvertiseCanMultiConn(t *testing.T) {
	for _, tc := range []struct {
		name string
		exp  *Export
	}{{"writable", &Export{Size: 1 << 20}}, {"read-only", &Export{Size: 1 << 20, ReadOnly: true}}} {
		if tc.exp.TransmissionFlags()&FlagCanMultiConn == 0 {
			t.Errorf("%s export did not advertise NBD_FLAG_CAN_MULTI_CONN", tc.name)
		}
	}
}

func TestRuntimeExporter_UnknownShareIsUnknownExport(t *testing.T) {
	ex := NewRuntimeExporter(nil, Identity{UID: 65534, GID: 65534})
	if _, err := ex.Resolve(context.Background(), "/nope/boot"); !errors.Is(err, ErrUnknownExport) {
		t.Fatalf("got %v, want ErrUnknownExport", err)
	}
}
