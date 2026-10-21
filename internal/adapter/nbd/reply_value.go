package nbd

import (
	"net"
	"time"
)

// reply is one command's answer, produced by a handler and written by whoever
// owns the socket.
//
// A handler no longer writes to the connection, and that is what makes several
// commands in flight possible: two handlers writing to one conn would interleave
// a header and its payload, and the client would read one reply's bytes as the
// other's. A handler that returns a value instead cannot do that even by
// accident.
//
// The payload is already sized by the handler, so writing it is a single Write
// after the header with no intermediate copy.
type reply struct {
	cookie  uint64
	nbdErr  NBDError
	payload []byte
}

// write sends the reply: header first, then payload when there is one.
//
// The write deadline is armed here, before every reply, so a client that stops
// reading cannot hold the connection past the configured budget. It is a
// per-write deadline rather than a per-connection one because a connection is
// long-lived: a connection-wide budget would expire a boot disk mid-boot.
func (r reply) write(conn net.Conn, h *Handler) error {
	if err := conn.SetWriteDeadline(time.Now().Add(h.timeouts.Write)); err != nil {
		return err
	}
	if err := writeSimpleReply(conn, r.cookie, r.nbdErr); err != nil {
		return err
	}
	if len(r.payload) == 0 {
		return nil
	}
	_, err := conn.Write(r.payload)
	return err
}

// errorReply is the shape most handlers return: a header and nothing else.
func errorReply(cookie uint64, nbdErr NBDError) reply {
	return reply{cookie: cookie, nbdErr: nbdErr}
}
