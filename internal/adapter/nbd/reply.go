package nbd

import "io"

// writeOptionReply writes an option reply header followed by data. Every
// handshake reply the server sends goes through here, so the reply magic and
// the option it answers are written in one place.
func writeOptionReply(w io.Writer, opt Option, typ OptionReplyType, data []byte) error {
	h := &OptionReplyHeader{
		Magic:  optionReplyMagic,
		Option: opt,
		Type:   typ,
		Length: uint32(len(data)),
	}
	if err := h.Encode(w); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	_, err := w.Write(data)
	return err
}

// writeOptionError writes an option reply that carries no payload. Every
// option-reply error type is empty on the wire, so the error value itself is
// the whole reply.
//
// decision: the error type is a parameter rather than one function per error
// value. Six near-identical functions is six places to transpose a magic, and
// the reply the client acts on is the type field, not the name of the Go
// function that wrote it.
func writeOptionError(w io.Writer, opt Option, typ OptionReplyType) error {
	return writeOptionReply(w, opt, typ, nil)
}

// writeSimpleReply writes a transmission-phase reply header with no payload,
// which is how every command except READ and BLOCK_STATUS answers.
//
// The transmission phase is where the first caller lives; this is declared here
// because the reply helpers belong together, not because the handshake sends
// one.
//
//nolint:unused // first caller is the transmission-phase dispatcher
func writeSimpleReply(w io.Writer, cookie uint64, nbdErr NBDError) error {
	r := &Reply{Magic: simpleReplyMagic, Error: nbdErr, Cookie: cookie}
	return r.Encode(w)
}
