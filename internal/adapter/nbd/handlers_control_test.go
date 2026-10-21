package nbd

import "testing"

// Both tests need the real-metadata fixture: handleFlush calls h.commit, which
// calls common.FlushStableWrite and dereferences h.meta. newTestHandler passes
// nil for meta, so a handler built that way panics instead of answering.

// TestHandleFlush_RejectsUndocumentedFlags pins that FLUSH validates its flags
// like every other command. The spec documents FUA on every command once
// SEND_FUA is advertised, and nothing else on FLUSH; a request carrying another
// flag is malformed and must be answered NBD_EINVAL rather than silently served.
//
// FlagDF is the flag used here because it is the one a client is most likely to
// set by accident: it means "don't fragment" and is legal only on WRITE. It is
// also the value this package already declares and its own protocol_test.go
// pins, so the test does not need a magic number.
//
// The second assertion is the one that makes this more than a status-code check:
// a rejected FLUSH must not have flushed. Answering EINVAL after doing the work
// would be a lie in the opposite direction, and a client that retried would be
// paying for two barriers.
func TestHandleFlush_RejectsUndocumentedFlags(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	sendRequest(t, client, &Request{Type: CmdFlush, Flags: FlagDF, Cookie: 0x51})
	if rep := readReplyHeader(t, client); rep.Error != ErrEINVAL {
		t.Fatalf("FLUSH with FlagDF reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	if st.flushes != 0 {
		t.Fatalf("the store saw %d flushes for a rejected FLUSH, want 0", st.flushes)
	}
}

// TestHandleFlush_AcceptsFUA pins that the one documented flag is still allowed,
// so the new check cannot be "reject every flag". FUA on FLUSH is redundant —
// FLUSH is already a barrier — but the spec says a server that advertises
// SEND_FUA must accept the flag on any command that supports it, and refusing a
// legal request is the worse failure.
func TestHandleFlush_AcceptsFUA(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	sendRequest(t, client, &Request{Type: CmdFlush, Flags: FlagFUA, Cookie: 0x52})
	if rep := readReplyHeader(t, client); rep.Error != 0 {
		t.Fatalf("FLUSH with FlagFUA reply error = %d, want 0", rep.Error)
	}
	if st.flushes != 1 {
		t.Fatalf("the store saw %d flushes, want 1", st.flushes)
	}
}
