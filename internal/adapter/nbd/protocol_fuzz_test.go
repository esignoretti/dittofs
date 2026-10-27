package nbd

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// FuzzReadRequest drives the transmission-phase header decoder, which is the
// first thing every client touches and the only parser on the request path that
// sees attacker-controlled bytes before any validation.
//
// The fuzz input is the header AFTER the magic, and the magic is prepended here.
// That is deliberate: ReadRequest rejects a wrong magic before looking at
// anything else, so fuzzing raw bytes would spend essentially every iteration in
// the one branch that cannot be wrong, and would never explore the flags, the
// command, or the length that actually reach a handler.
//
// The property is round-trip, not "rejects garbage". Most random input IS
// rejected, so asserting that would prove nothing. What matters is that a header
// the server accepts is a header it would itself produce: one it accepts but
// would not re-encode is one it can only have misinterpreted, and the payload
// read that follows would then consume the client's next command.
func FuzzReadRequest(f *testing.F) {
	seeds := []*Request{
		{Type: CmdRead, Cookie: 1, Offset: 0, Length: 4096},
		{Type: CmdWrite, Cookie: 2, Offset: 0, Length: maxBlockSize},
		{Type: CmdFlush, Cookie: 3},
		{Type: CmdDisc, Cookie: 4},
		{Type: CmdWriteZeroes, Cookie: 5, Length: 1},
		{Type: CmdTrim, Cookie: 6, Offset: 1 << 20, Length: 1 << 20},
		{
			Type: Command(0xffff), Cookie: 0xffffffffffffffff,
			Offset: 0xffffffffffffffff, Length: 0xffffffff,
		},
	}
	for _, s := range seeds {
		var buf bytes.Buffer
		if err := s.Encode(&buf); err != nil {
			f.Fatalf("encode seed: %v", err)
		}
		// Drop the leading magic, which the fuzz body puts back.
		f.Add(buf.Bytes()[4:])
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) != requestHeaderLen-4 {
			// The decoder reads a fixed-size header, so a short or long body only
			// exercises io.ReadFull. Let the seeds do the work.
			return
		}
		wire := make([]byte, requestHeaderLen)
		binary.BigEndian.PutUint32(wire[0:4], requestMagic)
		copy(wire[4:], body)

		req, err := ReadRequest(bytes.NewReader(wire))
		if err != nil {
			// Only a bad magic can fail here, and the magic was set. A failure is
			// therefore a decoder bug, not bad input.
			t.Fatalf("rejected a header with a valid magic: %v", err)
		}

		var out bytes.Buffer
		if err := req.Encode(&out); err != nil {
			t.Fatalf("accepted a request that will not re-encode: %v", err)
		}
		if !bytes.Equal(out.Bytes(), wire) {
			t.Fatalf("round-trip mismatch:\n in: %x\nout: %x", wire, out.Bytes())
		}
	})
}

// FuzzReadOptionHeader drives the handshake option header, which is parsed
// before any export exists and before any authentication-shaped decision is
// made.
//
// ReadOptionHeader does not validate its magic, so this one takes raw bytes.
func FuzzReadOptionHeader(f *testing.F) {
	seeds := []*OptionHeader{
		{Magic: ihaveoptMagic, Option: OptGO, Length: 0},
		{Magic: ihaveoptMagic, Option: OptList, Length: 0},
		{Magic: ihaveoptMagic, Option: OptAbort, Length: 0},
		{Magic: ihaveoptMagic, Option: OptInfo, Length: 4},
		{Magic: ihaveoptMagic, Option: Option(0xffff), Length: 0xffffffff},
		{Magic: 0, Option: 0, Length: 0},
	}
	for _, s := range seeds {
		var buf bytes.Buffer
		if err := s.Encode(&buf); err != nil {
			f.Fatalf("encode seed: %v", err)
		}
		f.Add(buf.Bytes())
	}

	f.Fuzz(func(t *testing.T, wire []byte) {
		if len(wire) != optionHeaderLen {
			return
		}
		hdr, err := ReadOptionHeader(bytes.NewReader(wire))
		if err != nil {
			// The decoder reads a fixed size and validates nothing, so any error
			// on a full-length input is a bug.
			t.Fatalf("failed to decode a full-length option header: %v", err)
		}

		var out bytes.Buffer
		if err := hdr.Encode(&out); err != nil {
			t.Fatalf("accepted an option header that will not re-encode: %v", err)
		}
		if !bytes.Equal(out.Bytes(), wire) {
			t.Fatalf("round-trip mismatch:\n in: %x\nout: %x", wire, out.Bytes())
		}
	})
}
