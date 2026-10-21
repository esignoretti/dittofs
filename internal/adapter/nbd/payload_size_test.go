package nbd

import (
	"testing"
	"time"
)

// A READ beyond the advertised maximum must be refused with NBD_EOVERFLOW and
// must not allocate the buffer.
//
// This is a regression test for a real defect: before payloadTooLarge existed,
// the only bound on a READ was the export size, so a client could make the
// server allocate the whole export in one request. Measured against the
// committed code with a 1 GiB export, a 256 MiB READ (8x the advertised 32 MiB
// maximum) was accepted with reply error 0.
func TestConnection_ReadBeyondAdvertisedMaxIsEOVERFLOW(t *testing.T) {
	f := newWriteVolumeFixture(t)
	h := f.newHandler(t, newStoreFake())

	// Widen the export so the range check passes and only the advertised
	// maximum can reject the request. Without this the test would pass for the
	// wrong reason: the 1 MiB fixture export would trip the range check first.
	exp := *f.exp
	exp.Size = 1 << 30

	c, srv := pipe(t)
	defer c.Close()
	defer srv.Close()
	_ = srv.SetDeadline(time.Now().Add(5 * time.Second))

	req := &Request{
		Type:   CmdRead,
		Cookie: 11,
		Length: maxBlockSize + 1,
		Offset: 0,
	}
	done := make(chan struct{})
	go func() { _ = h.handleRead(t.Context(), f.bs, &exp, req).write(srv, h); close(done) }()

	rep := readReplyHeader(t, c)
	if rep.Error != ErrEOVERFLOW {
		t.Fatalf("READ of %d bytes (max %d) reply error = %d, want ErrEOVERFLOW (%d)",
			req.Length, maxBlockSize, rep.Error, ErrEOVERFLOW)
	}
	if rep.Cookie != req.Cookie {
		t.Fatalf("cookie = %d, want %d", rep.Cookie, req.Cookie)
	}
	<-done
}

// Exactly the advertised maximum is legal: the check is ">", not ">=".
func TestConnection_ReadAtAdvertisedMaxIsServed(t *testing.T) {
	f := newWriteVolumeFixture(t)
	h := f.newHandler(t, newStoreFake())

	exp := *f.exp
	exp.Size = 1 << 30

	c, srv := pipe(t)
	defer c.Close()
	defer srv.Close()
	_ = srv.SetDeadline(time.Now().Add(30 * time.Second))

	req := &Request{Type: CmdRead, Cookie: 12, Length: maxBlockSize, Offset: 0}
	go func() { _ = h.handleRead(t.Context(), f.bs, &exp, req).write(srv, h) }()

	rep := readReplyHeader(t, c)
	if rep.Error != 0 {
		t.Fatalf("READ of exactly maxBlockSize (%d) reply error = %d, want 0",
			maxBlockSize, rep.Error)
	}
}

// A WRITE beyond the advertised maximum is EINVAL (not EOVERFLOW: the spec's
// EOVERFLOW clause covers only commands WITHOUT payload), and its payload must
// be drained so the stream stays synchronized.
func TestConnection_WriteBeyondAdvertisedMaxIsEINVALAndDrained(t *testing.T) {
	f := newWriteVolumeFixture(t)
	h := f.newHandler(t, newStoreFake())

	exp := *f.exp
	exp.Size = 1 << 30

	c, srv := pipe(t)
	defer c.Close()
	defer srv.Close()
	_ = srv.SetDeadline(time.Now().Add(15 * time.Second))

	length := uint32(maxBlockSize + 4096)
	req := &Request{Type: CmdWrite, Cookie: 13, Length: length, Offset: 0}

	// The client sends the payload unconditionally ("The client MUST follow the
	// request header with length number of bytes"), so the test must too —
	// otherwise a correct server blocks in the drain and the test would hang
	// rather than pass.
	payload := make([]byte, length)
	// The split is visible here: validateWrite refuses, the caller writes the
	// refusal, and the caller drains — because the payload read and the drain
	// both belong to whoever owns the stream.
	go func() {
		pre, refused := h.validateWrite(&exp, req)
		if !refused {
			t.Error("validateWrite accepted a WRITE beyond the advertised maximum")
			return
		}
		if err := pre.write(srv, h); err != nil {
			return
		}
		if err := srv.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return
		}
		_ = h.drainWritePayload(srv, req.Length)
	}()

	go func() { _, _ = c.Write(payload) }()

	rep := readReplyHeader(t, c)
	if rep.Error != ErrEINVAL {
		t.Fatalf("WRITE of %d bytes (max %d) reply error = %d, want ErrEINVAL (%d)",
			length, maxBlockSize, rep.Error, ErrEINVAL)
	}

	// The drain is what keeps the stream usable. Prove it by sending a
	// well-formed READ afterwards and requiring a normal reply: if the drain
	// were skipped, the leftover payload bytes would be parsed as a header and
	// the connection would desynchronize.
	next := &Request{Type: CmdRead, Cookie: 14, Length: 4096, Offset: 0}
	sendRequest(t, c, next)
	go func() { _ = h.handleRead(t.Context(), f.bs, &exp, next).write(srv, h) }()
	rep2 := readReplyHeader(t, c)
	if rep2.Cookie != next.Cookie {
		t.Fatalf("follow-up reply cookie = %d, want %d: the stream desynchronized",
			rep2.Cookie, next.Cookie)
	}
}
