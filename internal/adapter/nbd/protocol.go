package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Wire magics. NBD is big-endian on the wire; every header carries one of
// these in its first field.
const (
	// nbdMagic begins the server greeting. The spec also names it
	// NBDMAGIC; older documents call the same value initPasswd.
	nbdMagic uint64 = 0x4e42444d41474943
	// ihaveoptMagic begins every option header in both directions.
	ihaveoptMagic uint64 = 0x49484156454f5054
	// optionReplyMagic begins every option reply header.
	optionReplyMagic uint64 = 0x3e889045565a9

	// requestMagic begins every transmission-phase request.
	requestMagic uint32 = 0x25609513
	// simpleReplyMagic begins every transmission-phase reply.
	simpleReplyMagic uint32 = 0x67446698
)

// Command is a transmission-phase command type.
type Command uint16

// Transmission-phase command values.
const (
	CmdRead        Command = 0
	CmdWrite       Command = 1
	CmdDisc        Command = 2
	CmdFlush       Command = 3
	CmdTrim        Command = 4
	CmdCache       Command = 5
	CmdWriteZeroes Command = 6
	CmdBlockStatus Command = 7
)

// CommandFlags is the per-request flag word.
type CommandFlags uint16

// Command flag bits. Only the bits the server advertises may legitimately
// appear; the rest are named so a dispatcher can reject them explicitly.
const (
	FlagFUA      CommandFlags = 1 << 0
	FlagNoHole   CommandFlags = 1 << 1
	FlagDF       CommandFlags = 1 << 2
	FlagReqOne   CommandFlags = 1 << 3
	FlagFastZero CommandFlags = 1 << 4
)

// TransmissionFlags is the flag word the server hands the client at the end of
// the handshake. A client acts on these bits without verifying them.
type TransmissionFlags uint16

// Transmission flag bits. Bit 4 (rotational) and bits 7 and above are
// deliberately absent: the adapter does not advertise them.
const (
	FlagHASFlags        TransmissionFlags = 1 << 0
	FlagReadOnly        TransmissionFlags = 1 << 1
	FlagSendFlush       TransmissionFlags = 1 << 2
	FlagSendFUA         TransmissionFlags = 1 << 3
	FlagSendTrim        TransmissionFlags = 1 << 5
	FlagSendWriteZeroes TransmissionFlags = 1 << 6
)

// NBDError is a transmission-phase error value, carried in a simple reply.
//
// NBD_ENOTSUP (95) is deliberately absent. The spec reserves it for
// WRITE_ZEROES with NBD_CMD_FLAG_FAST_ZERO, and the adapter does not advertise
// SEND_FAST_ZERO, so that value is unreachable.
//
// NBD_EOVERFLOW (75) IS reachable, and an earlier version of this comment was
// wrong to say otherwise. The spec allows it "when a command without payload
// requests a length larger than an advertised maximum payload length" — and
// NBD_CMD_READ is exactly a command without payload, while the handshake
// advertises maxBlockSize (32 MiB) as the maximum. A READ of more than that is
// therefore both reachable and the one case the spec documents, so it is
// answered with EOVERFLOW rather than silently allocating whatever the client
// asked for. See checkPayloadLength.
type NBDError uint32

// Transmission-phase error values.
const (
	ErrEPERM     NBDError = 1
	ErrEIO       NBDError = 5
	ErrEINVAL    NBDError = 22
	ErrENOSPC    NBDError = 28
	ErrEOVERFLOW NBDError = 75
	ErrESHUTDOWN NBDError = 108
)

// Option is a handshake option type.
type Option uint32

// Handshake option values.
const (
	OptExportName      Option = 1
	OptAbort           Option = 2
	OptList            Option = 3
	OptStartTLS        Option = 5
	OptInfo            Option = 6
	OptGO              Option = 7
	OptStructuredReply Option = 8
	OptListMetaContext Option = 9
	OptSetMetaContext  Option = 10
)

// OptionReplyType is the type field of an option reply header. Values below
// 0x80000000 are successes; the rest are errors.
type OptionReplyType uint32

// Option reply types.
const (
	RepAck              OptionReplyType = 1
	RepServer           OptionReplyType = 2
	RepInfo             OptionReplyType = 3
	RepMetaContext      OptionReplyType = 4
	RepErrUnsup         OptionReplyType = 0x80000001
	RepErrPolicy        OptionReplyType = 0x80000002
	RepErrInvalid       OptionReplyType = 0x80000003
	RepErrPlatform      OptionReplyType = 0x80000004
	RepErrTLSReqd       OptionReplyType = 0x80000005
	RepErrUnknown       OptionReplyType = 0x80000006
	RepErrShutdown      OptionReplyType = 0x80000007
	RepErrBlockSizeReqd OptionReplyType = 0x80000008
	RepErrTooBig        OptionReplyType = 0x80000009
	RepErrExtHeaderReqd OptionReplyType = 0x8000000A
)

// ErrBadMagic reports a header whose leading magic value did not match the
// value required for that header.
//
// decision: there is no sentinel for an unknown command value. An unrecognised
// command is a well-formed header that the dispatcher answers with EINVAL;
// rejecting it during decode would put a dispatch rule in the codec and leave
// the dispatcher's default branch unreachable.
var ErrBadMagic = errors.New("nbd: bad magic")

// Header sizes on the wire.
const (
	requestHeaderLen     = 28
	replyHeaderLen       = 16
	optionHeaderLen      = 16
	optionReplyHeaderLen = 20
)

// Request is a transmission-phase request header.
type Request struct {
	Magic  uint32
	Flags  CommandFlags
	Type   Command
	Cookie uint64
	Offset uint64
	Length uint32
}

// Encode writes the request header to w.
func (r *Request) Encode(w io.Writer) error {
	var buf [requestHeaderLen]byte
	binary.BigEndian.PutUint32(buf[0:4], r.Magic)
	binary.BigEndian.PutUint16(buf[4:6], uint16(r.Flags))
	binary.BigEndian.PutUint16(buf[6:8], uint16(r.Type))
	binary.BigEndian.PutUint64(buf[8:16], r.Cookie)
	binary.BigEndian.PutUint64(buf[16:24], r.Offset)
	binary.BigEndian.PutUint32(buf[24:28], r.Length)
	_, err := w.Write(buf[:])
	return err
}

// ReadRequest reads and decodes a request header.
//
// The whole header is read before any field is populated, so a truncated read
// surfaces as an error rather than a half-filled Request, and the magic is
// checked before the remaining fields are trusted.
func ReadRequest(r io.Reader) (*Request, error) {
	var buf [requestHeaderLen]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	magic := binary.BigEndian.Uint32(buf[0:4])
	if magic != requestMagic {
		return nil, fmt.Errorf("%w: %#x", ErrBadMagic, magic)
	}
	return &Request{
		Magic:  magic,
		Flags:  CommandFlags(binary.BigEndian.Uint16(buf[4:6])),
		Type:   Command(binary.BigEndian.Uint16(buf[6:8])),
		Cookie: binary.BigEndian.Uint64(buf[8:16]),
		Offset: binary.BigEndian.Uint64(buf[16:24]),
		Length: binary.BigEndian.Uint32(buf[24:28]),
	}, nil
}

// Reply is a transmission-phase simple reply header.
type Reply struct {
	Magic  uint32
	Error  NBDError
	Cookie uint64
}

// Encode writes the reply header to w.
func (r *Reply) Encode(w io.Writer) error {
	var buf [replyHeaderLen]byte
	binary.BigEndian.PutUint32(buf[0:4], r.Magic)
	binary.BigEndian.PutUint32(buf[4:8], uint32(r.Error))
	binary.BigEndian.PutUint64(buf[8:16], r.Cookie)
	_, err := w.Write(buf[:])
	return err
}

// ReadReply reads and decodes a simple reply header.
func ReadReply(r io.Reader) (*Reply, error) {
	var buf [replyHeaderLen]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	return &Reply{
		Magic:  binary.BigEndian.Uint32(buf[0:4]),
		Error:  NBDError(binary.BigEndian.Uint32(buf[4:8])),
		Cookie: binary.BigEndian.Uint64(buf[8:16]),
	}, nil
}

// OptionHeader is a handshake option header, sent client to server.
type OptionHeader struct {
	Magic  uint64
	Option Option
	Length uint32
}

// Encode writes the option header to w.
func (o *OptionHeader) Encode(w io.Writer) error {
	var buf [optionHeaderLen]byte
	binary.BigEndian.PutUint64(buf[0:8], o.Magic)
	binary.BigEndian.PutUint32(buf[8:12], uint32(o.Option))
	binary.BigEndian.PutUint32(buf[12:16], o.Length)
	_, err := w.Write(buf[:])
	return err
}

// ReadOptionHeader reads and decodes an option header.
func ReadOptionHeader(r io.Reader) (*OptionHeader, error) {
	var buf [optionHeaderLen]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	return &OptionHeader{
		Magic:  binary.BigEndian.Uint64(buf[0:8]),
		Option: Option(binary.BigEndian.Uint32(buf[8:12])),
		Length: binary.BigEndian.Uint32(buf[12:16]),
	}, nil
}

// OptionReplyHeader is a handshake option reply header, sent server to client.
// It is wider than OptionHeader because it carries the reply type.
type OptionReplyHeader struct {
	Magic  uint64
	Option Option
	Type   OptionReplyType
	Length uint32
}

// Encode writes the option reply header to w.
func (o *OptionReplyHeader) Encode(w io.Writer) error {
	var buf [optionReplyHeaderLen]byte
	binary.BigEndian.PutUint64(buf[0:8], o.Magic)
	binary.BigEndian.PutUint32(buf[8:12], uint32(o.Option))
	binary.BigEndian.PutUint32(buf[12:16], uint32(o.Type))
	binary.BigEndian.PutUint32(buf[16:20], o.Length)
	_, err := w.Write(buf[:])
	return err
}

// ReadOptionReplyHeader reads and decodes an option reply header.
//
// The server never reads one: it only writes them. This decoder exists so tests
// can assert on what the server sent without hand-parsing bytes.
func ReadOptionReplyHeader(r io.Reader) (*OptionReplyHeader, error) {
	var buf [optionReplyHeaderLen]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	return &OptionReplyHeader{
		Magic:  binary.BigEndian.Uint64(buf[0:8]),
		Option: Option(binary.BigEndian.Uint32(buf[8:12])),
		Type:   OptionReplyType(binary.BigEndian.Uint32(buf[12:16])),
		Length: binary.BigEndian.Uint32(buf[16:20]),
	}, nil
}
