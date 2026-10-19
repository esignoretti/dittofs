package nbd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Handshake flag bits the server advertises in its greeting.
const (
	// handshakeFlagFixedNewstyle marks the fixed-newstyle handshake, the only
	// one this adapter speaks.
	handshakeFlagFixedNewstyle uint16 = 1 << 0
	// handshakeFlagNoZeroes tells the client that the legacy
	// NBD_OPT_EXPORT_NAME reply omits its 124 trailing zero bytes when the
	// client asks for that with clientFlagNoZeroes.
	handshakeFlagNoZeroes uint16 = 1 << 1
)

// Client flag bits the client answers the greeting with.
const (
	// clientFlagNoZeroes asks the server to omit the trailing zero padding on
	// the legacy NBD_OPT_EXPORT_NAME reply.
	clientFlagNoZeroes uint32 = 1 << 1
)

// handshakeFlags is what the server advertises in its greeting. It is one
// value rather than two literals so that the check on the client's answer
// tests what was actually advertised, not what the code meant to advertise.
const handshakeFlags = handshakeFlagFixedNewstyle | handshakeFlagNoZeroes

// maxOptionData bounds the option payload the server will read into memory.
// An option header states its own length, so without this bound a client
// could make the server allocate whatever it asked for.
const maxOptionData = 64 << 10

// NBD_INFO_* information types carried by an NBD_REP_INFO reply.
const (
	infoExport    uint16 = 0
	infoBlockSize uint16 = 3
)

// Block-size advertisement, in bytes.
//
// Minimum 1 is the value the protocol allows for an export backed by a regular
// file, which is what a volume is; the 512/4096 values the protocol calls more
// typical belong to exports backed by a block device. Advertising 4096 would
// make the server refuse every request that is not a multiple of 4096, and a
// guest filesystem's own 512-byte metadata writes are unaligned by that
// standard. Preferred and maximum are the protocol's defaults.
const (
	minBlockSize       uint32 = 1
	preferredBlockSize uint32 = 4096
	maxBlockSize       uint32 = 32 << 20
)

// exportNameTrailingZeroes is the zero padding the legacy
// NBD_OPT_EXPORT_NAME reply ends with, for clients that predate
// NBD_FLAG_C_NO_ZEROES.
const exportNameTrailingZeroes = 124

// ErrUnknownExport reports an export name that names no volume.
//
// Every reason a name can fail to resolve — no such share, no volumes
// directory, no such volume — maps to this one value: the name is
// client-supplied, and the server must not tell a client which of its guesses
// was closer.
var ErrUnknownExport = errors.New("nbd: unknown export")

// errSessionAborted reports that the client ended the handshake with
// NBD_OPT_ABORT. It is not a protocol error: the caller must close the
// connection, but nothing has gone wrong.
var errSessionAborted = errors.New("nbd: session aborted by the client")

// Export is a resolved volume, ready to serve.
type Export struct {
	// Name is the export name as the client asked for it.
	Name string
	// Size is the volume size in bytes, fixed at volume creation.
	Size uint64
	// PayloadID addresses the volume's bytes in the block store.
	PayloadID metadata.PayloadID
	// Handle is the volume file's metadata handle. The write path needs it to
	// derive the payload ID it commits against, so it is resolved once here
	// and held for the life of the connection.
	Handle metadata.FileHandle
	// ReadOnly reports whether the share that holds the volume is read-only.
	ReadOnly bool
}

// TransmissionFlags returns the flag word the client is handed for this
// export. A client acts on these bits without verifying them, so the set is a
// contract: it names exactly the commands the adapter implements.
func (e *Export) TransmissionFlags() TransmissionFlags {
	flags := FlagHASFlags | FlagSendFlush | FlagSendFUA | FlagSendTrim | FlagSendWriteZeroes
	if e.ReadOnly {
		flags |= FlagReadOnly
	}
	return flags
}

// Exporter resolves export names to volumes.
type Exporter interface {
	// Resolve returns the volume an export name names, or ErrUnknownExport.
	Resolve(ctx context.Context, name string) (*Export, error)
	// List returns every export name the server offers.
	List(ctx context.Context) ([]string, error)
}

// ServeHandshake writes the greeting, haggles options with the client, and
// returns the export the client selected. On return the connection is ready
// for the transmission phase, and the caller owns the export for the life of
// the connection.
//
// A client that names no valid export, or that aborts, ends with a non-nil
// error and a closed connection: the handshake has no way back to a usable
// session, and the caller must not serve one.
func ServeHandshake(ctx context.Context, conn net.Conn, ex Exporter) (*Export, error) {
	if err := writeGreeting(conn); err != nil {
		return nil, err
	}

	// The greeting is followed by the client's 32-bit flags before any option.
	clientFlags, err := readClientFlags(conn)
	if err != nil {
		return nil, err
	}
	// A client may only ask for the no-zeroes form when the server offered it.
	// The server always offers it, so this cannot fire; it is checked rather
	// than assumed so that the reply format is chosen from the flags the
	// greeting actually carried.
	if clientFlags&clientFlagNoZeroes != 0 && handshakeFlags&handshakeFlagNoZeroes == 0 {
		return nil, errors.New("nbd: client negotiated NBD_FLAG_C_NO_ZEROES, which the server did not offer")
	}
	noZeroes := clientFlags&clientFlagNoZeroes != 0

	for {
		h, err := ReadOptionHeader(conn)
		if err != nil {
			return nil, err
		}
		if h.Magic != ihaveoptMagic {
			return nil, fmt.Errorf("%w: option header magic %#x", ErrBadMagic, h.Magic)
		}

		// The announced length is checked before anything is allocated for it.
		//
		// decision: an over-long option ends the session rather than being
		// refused and skipped. Skipping means consuming the announced bytes to
		// leave the stream aligned, and a length the server refuses to believe
		// is exactly the length it cannot consume. The client is told why first.
		if h.Length > maxOptionData {
			if werr := writeOptionError(conn, h.Option, RepErrTooBig); werr != nil {
				return nil, werr
			}
			return nil, fmt.Errorf("nbd: option %d announces %d bytes of payload, over the %d-byte limit",
				h.Option, h.Length, maxOptionData)
		}

		// Every option's payload is consumed here, whether or not the option
		// uses it, so that the next option header starts where the client put
		// it.
		data := make([]byte, h.Length)
		if h.Length > 0 {
			if _, err := io.ReadFull(conn, data); err != nil {
				return nil, err
			}
		}

		switch h.Option {
		case OptGO, OptInfo:
			exp, done, err := serveGoOption(ctx, conn, ex, h.Option, data)
			if err != nil {
				return nil, err
			}
			if done {
				return exp, nil
			}

		case OptExportName:
			// Legacy path: the payload is the name, and the reply carries the
			// size and flags in place of an option reply.
			exp, err := ex.Resolve(ctx, string(data))
			if err != nil {
				return nil, err
			}
			if err := writeExportNameReply(conn, exp, noZeroes); err != nil {
				return nil, err
			}
			return exp, nil

		case OptList:
			if err := serveList(ctx, conn, ex); err != nil {
				return nil, err
			}

		case OptAbort:
			if err := writeOptionReply(conn, OptAbort, RepAck, nil); err != nil {
				return nil, err
			}
			return nil, errSessionAborted

		default:
			// NBD_OPT_STARTTLS, NBD_OPT_STRUCTURED_REPLY and the meta-context
			// options land here by design: the adapter implements none of
			// them, and a client that is told so can fall back. STARTTLS in
			// particular is refused rather than ignored — a client that
			// believes its session is encrypted and is not has no way to
			// notice.
			if err := writeOptionError(conn, h.Option, RepErrUnsup); err != nil {
				return nil, err
			}
		}
	}
}

// writeGreeting writes NBDMAGIC, IHAVEOPT and the 16-bit handshake flags.
func writeGreeting(w io.Writer) error {
	var buf [18]byte
	binary.BigEndian.PutUint64(buf[0:8], nbdMagic)
	binary.BigEndian.PutUint64(buf[8:16], ihaveoptMagic)
	binary.BigEndian.PutUint16(buf[16:18], handshakeFlags)
	_, err := w.Write(buf[:])
	return err
}

// readClientFlags reads the client's 32-bit flag word.
func readClientFlags(r io.Reader) (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf[:]), nil
}

// serveGoOption answers NBD_OPT_GO and NBD_OPT_INFO.
//
// done reports whether the handshake is over with exp as its result: NBD_OPT_GO
// begins the transmission phase, NBD_OPT_INFO only asks about the export and
// leaves the client free to name another one.
func serveGoOption(ctx context.Context, conn net.Conn, ex Exporter, opt Option, data []byte) (exp *Export, done bool, err error) {
	name, requested, ok := parseGoPayload(data)
	if !ok {
		return nil, false, writeOptionError(conn, opt, RepErrInvalid)
	}

	exp, err = ex.Resolve(ctx, name)
	if err != nil {
		if !errors.Is(err, ErrUnknownExport) {
			return nil, false, err
		}
		return nil, false, writeOptionError(conn, opt, RepErrUnknown)
	}

	// NBD_INFO_EXPORT is mandatory before a successful NBD_REP_ACK, whether or
	// not the client asked for it.
	if err := writeExportInfo(conn, opt, exp, requested); err != nil {
		return nil, false, err
	}
	if err := writeOptionReply(conn, opt, RepAck, nil); err != nil {
		return nil, false, err
	}
	return exp, opt == OptGO, nil
}

// parseGoPayload decodes an NBD_OPT_GO / NBD_OPT_INFO payload: a 32-bit name
// length, the name, a 16-bit count of information requests, then that many
// 16-bit request types.
//
// ok is false for any payload whose own lengths do not fit inside it. Both
// lengths are client-supplied, so neither may be used as a slice bound before
// it has been checked against the bytes actually received.
func parseGoPayload(data []byte) (name string, requested []uint16, ok bool) {
	if len(data) < 6 {
		return "", nil, false
	}
	nameLen := binary.BigEndian.Uint32(data[0:4])
	if uint64(nameLen) > uint64(len(data)-6) {
		return "", nil, false
	}
	name = string(data[4 : 4+nameLen])

	rest := data[6+nameLen:]
	count := binary.BigEndian.Uint16(data[4+nameLen : 6+nameLen])
	if int(count) > len(rest)/2 {
		return "", nil, false
	}
	requested = make([]uint16, count)
	for i := range requested {
		requested[i] = binary.BigEndian.Uint16(rest[2*i : 2*i+2])
	}
	return name, requested, true
}

// writeExportInfo writes the NBD_INFO_EXPORT reply every successful
// NBD_OPT_GO / NBD_OPT_INFO ends with, followed by NBD_INFO_BLOCK_SIZE when
// the client asked for it.
func writeExportInfo(w io.Writer, opt Option, exp *Export, requested []uint16) error {
	var info [12]byte
	binary.BigEndian.PutUint16(info[0:2], infoExport)
	binary.BigEndian.PutUint64(info[2:10], exp.Size)
	binary.BigEndian.PutUint16(info[10:12], uint16(exp.TransmissionFlags()))
	if err := writeOptionReply(w, opt, RepInfo, info[:]); err != nil {
		return err
	}

	if !slices.Contains(requested, infoBlockSize) {
		return nil
	}
	var block [14]byte
	binary.BigEndian.PutUint16(block[0:2], infoBlockSize)
	binary.BigEndian.PutUint32(block[2:6], minBlockSize)
	binary.BigEndian.PutUint32(block[6:10], preferredBlockSize)
	binary.BigEndian.PutUint32(block[10:14], maxBlockSize)
	return writeOptionReply(w, opt, RepInfo, block[:])
}

// writeExportNameReply writes the legacy NBD_OPT_EXPORT_NAME reply: the export
// size, the transmission flags, then 124 zero bytes unless the client
// negotiated their absence.
func writeExportNameReply(w io.Writer, exp *Export, noZeroes bool) error {
	buf := make([]byte, 10+exportNameTrailingZeroes)
	binary.BigEndian.PutUint64(buf[0:8], exp.Size)
	binary.BigEndian.PutUint16(buf[8:10], uint16(exp.TransmissionFlags()))
	if noZeroes {
		buf = buf[:10]
	}
	_, err := w.Write(buf)
	return err
}

// serveList writes one NBD_REP_SERVER per export name, then NBD_REP_ACK.
func serveList(ctx context.Context, conn net.Conn, ex Exporter) error {
	names, err := ex.List(ctx)
	if err != nil {
		// A listing that failed ends the handshake rather than answering with
		// an empty list: a client reads an empty list as "this server has no
		// exports", which is a different and wrong statement.
		return err
	}
	for _, name := range names {
		payload := make([]byte, 4+len(name))
		binary.BigEndian.PutUint32(payload[0:4], uint32(len(name)))
		copy(payload[4:], name)
		if err := writeOptionReply(conn, OptList, RepServer, payload); err != nil {
			return err
		}
	}
	return writeOptionReply(conn, OptList, RepAck, nil)
}
