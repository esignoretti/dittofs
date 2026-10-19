package nbd

import (
	"bytes"
	"errors"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	want := &Request{
		Magic:  requestMagic,
		Flags:  FlagFUA,
		Type:   CmdWrite,
		Cookie: 0xdeadbeefcafef00d,
		Offset: 4096,
		Length: 8192,
	}
	var buf bytes.Buffer
	if err := want.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got := buf.Len(); got != 28 {
		t.Fatalf("encoded request is %d bytes, want 28", got)
	}
	got, err := ReadRequest(&buf)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if *got != *want {
		t.Fatalf("round trip: got %+v, want %+v", got, want)
	}
}

func TestRequestBadMagic(t *testing.T) {
	buf := bytes.NewReader([]byte{
		0xde, 0xad, 0xbe, 0xef, // not NBD_REQUEST_MAGIC
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0,
	})
	if _, err := ReadRequest(buf); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("got %v, want ErrBadMagic", err)
	}
}

func TestReplyRoundTrip(t *testing.T) {
	want := &Reply{Magic: simpleReplyMagic, Error: ErrEIO, Cookie: 0x1122334455667788}
	var buf bytes.Buffer
	if err := want.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got := buf.Len(); got != 16 {
		t.Fatalf("encoded reply is %d bytes, want 16", got)
	}
	got, err := ReadReply(&buf)
	if err != nil {
		t.Fatalf("ReadReply: %v", err)
	}
	if *got != *want {
		t.Fatalf("round trip: got %+v, want %+v", got, want)
	}
}

func TestOptionHeaderRoundTrip(t *testing.T) {
	want := &OptionHeader{Magic: ihaveoptMagic, Option: OptGO, Length: 42}
	var buf bytes.Buffer
	if err := want.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got := buf.Len(); got != 16 {
		t.Fatalf("encoded option header is %d bytes, want 16", got)
	}
	got, err := ReadOptionHeader(&buf)
	if err != nil {
		t.Fatalf("ReadOptionHeader: %v", err)
	}
	if *got != *want {
		t.Fatalf("round trip: got %+v, want %+v", got, want)
	}
}

func TestOptionReplyHeaderIs20Bytes(t *testing.T) {
	h := &OptionReplyHeader{Magic: optionReplyMagic, Option: OptGO, Type: RepAck, Length: 0}
	var buf bytes.Buffer
	if err := h.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got := buf.Len(); got != 20 {
		t.Fatalf("encoded option reply header is %d bytes, want 20", got)
	}
}

// The magic values are the contract with every client. A transposed byte here
// is invisible until a real client refuses to negotiate.
func TestMagicValues(t *testing.T) {
	cases := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"NBDMAGIC", nbdMagic, 0x4e42444d41474943},
		{"IHAVEOPT", ihaveoptMagic, 0x49484156454f5054},
		{"option reply", optionReplyMagic, 0x3e889045565a9},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
	if requestMagic != 0x25609513 {
		t.Errorf("request magic = %#x, want 0x25609513", requestMagic)
	}
	if simpleReplyMagic != 0x67446698 {
		t.Errorf("simple reply magic = %#x, want 0x67446698", simpleReplyMagic)
	}
}

// A truncated read must surface as an error, never as a zero-valued header that
// the dispatcher then acts on.
func TestReadRequestTruncated(t *testing.T) {
	buf := bytes.NewReader([]byte{0x25, 0x60, 0x95, 0x13, 0x00})
	if _, err := ReadRequest(buf); err == nil {
		t.Fatal("truncated header decoded without error")
	}
}
