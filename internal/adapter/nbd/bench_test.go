package nbd

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// These benchmarks exist to answer one question with a number instead of an
// argument: does the adapter's serialized dispatch cap throughput below what the
// block store can serve?
//
// The measurement only means anything with latency in the store. Against a store
// that answers in nanoseconds the adapter is CPU-bound and the depth of the
// request queue makes no difference; against one that takes 100 µs the
// difference between "one command in flight" and "many" is the whole point. So
// every benchmark here runs over latencyStore, which sleeps for a configured
// delay before answering.
//
// The comparison to read is depth 1 against depth N:
//
//	go test -run '^$' -bench 'BenchmarkConnection_ReadDepth' -benchtime 2000x ./internal/adapter/nbd/
//
// Today the numbers are flat across depth, because serve() reads one request,
// dispatches it on the connection goroutine and only then reads the next — so
// queueing requests at the client just fills a socket buffer. After the
// pipelining work (docs/superpowers/plans/2026-10-20-nbd-pipelining-and-release-cleanup.md,
// tasks 9-11) the depth-N figure should approach the store's own ceiling of
// 1/delay per worker. A flat result after that change is a regression, not noise.
//
// Writes are deliberately absent. A WRITE needs a real metadata service
// (PrepareWrite and CommitWrite both run before the reply), and the write
// fixture is built on a real engine.Store, so a latency-injecting wrapper cannot
// be dropped in without also faking the metadata layer — at which point the
// number stops describing the production path. The plateau this is chasing was
// measured on random 4 KiB reads (docs/BENCHMARKS.md), and that is the case
// reproduced here.

// benchDelay is the per-command store latency these benchmarks inject.
//
// 100 µs is the shape of a real backing store rather than a guess: the local
// journal answers in tens of microseconds, and docs/BENCHMARKS.md measured
// 71 µs of per-request cost end to end on the Linux fixture. At that latency one
// command in flight caps a connection at about 10k IOPS, which is the same order
// as the measured single-connection ceiling and comfortably below what a CPU can
// dispatch — so the store, not the adapter's own compute, is the thing being
// waited on.
const benchDelay = 100 * time.Microsecond

// latencyStore is a BlockStore that sleeps before answering, with its own
// byte storage so the benchmark measures dispatch and not a fake's map lookups.
//
// It is a separate type from storeFake on purpose. storeFake's counters are
// plain ints, which is fine for the connection tests (one goroutine, serialized
// dispatch) and a data race the moment dispatch becomes concurrent — and these
// benchmarks are exactly the thing that will be re-run after that change. Every
// counter here is atomic, so the file stays race-clean when the adapter starts
// serving commands in parallel.
type latencyStore struct {
	delay time.Duration

	mu   sync.RWMutex
	data map[string][]byte

	reads  atomic.Int64
	writes atomic.Int64
}

func newLatencyStore(delay time.Duration) *latencyStore {
	return &latencyStore{delay: delay, data: map[string][]byte{}}
}

func (s *latencyStore) ReadAt(_ context.Context, id string, buf []byte, off uint64) (int, error) {
	s.reads.Add(1)
	time.Sleep(s.delay)
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := s.data[id]
	for i := range buf {
		if o := off + uint64(i); o < uint64(len(b)) {
			buf[i] = b[o]
		} else {
			buf[i] = 0
		}
	}
	return len(buf), nil
}

func (s *latencyStore) WriteAt(_ context.Context, id string, currentBlocks []block.ChunkRef, data []byte, off uint64) ([]block.ChunkRef, error) {
	s.writes.Add(1)
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	end := off + uint64(len(data))
	if cur := s.data[id]; uint64(len(cur)) < end {
		grown := make([]byte, end)
		copy(grown, cur)
		s.data[id] = grown
	}
	copy(s.data[id][off:], data)
	return currentBlocks, nil
}

func (s *latencyStore) PunchHole(_ context.Context, id string, currentBlocks []block.ChunkRef, off, length uint64) ([]block.ChunkRef, error) {
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.data[id]
	for i := off; i < off+length && i < uint64(len(b)); i++ {
		b[i] = 0
	}
	return currentBlocks, nil
}

func (s *latencyStore) Flush(context.Context, string) (*block.FlushResult, error) {
	time.Sleep(s.delay)
	return &block.FlushResult{}, nil
}

func (s *latencyStore) LocalDurable() bool         { return true }
func (s *latencyStore) RemoteDurable() bool        { return true }
func (s *latencyStore) RequireDurableCommit() bool { return false }

// benchmarkHandler builds the handler the way the production adapter does, but
// over st. Metadata is nil: READ never touches it, and that is the only command
// these benchmarks drive.
func benchmarkHandler(st BlockStore) *Handler {
	return NewHandler(exporterFor(testExport(1<<20)), resolverFor(st), nil,
		Identity{UID: 65534, GID: 65534}, testTimeouts(), func() bool { return false })
}

// benchmarkConn starts a handler over a real TCP socket and completes the
// handshake, returning the client end. A real socket rather than net.Pipe:
// net.Pipe is unbuffered, so a client that queues several requests before
// reading any reply would deadlock against a server that is busy replying — which
// is precisely the shape being measured.
func benchmarkConn(b *testing.B, h *Handler) net.Conn {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	server, ok := <-accepted
	if !ok {
		b.Fatal("accept failed")
	}
	go func() { _ = h.serve(context.Background(), server) }()

	b.Cleanup(func() { _ = client.Close() })

	// The handshake is driven directly rather than through completeHandshake,
	// which takes a *testing.T. The GO payload is built by goPayload, which is
	// the same helper the connection tests use: NBD_OPT_GO's data is not the
	// export name but a 4-byte name length, the name, then a 2-byte count of
	// NBD_INFO requests. Writing the bare name instead makes the server read
	// the first four bytes of the name as a length and wait for that many bytes
	// that will never arrive — a deadlock on both ends, not an error.
	if _, err := io.ReadFull(client, make([]byte, 8+8+2)); err != nil {
		b.Fatalf("read greeting: %v", err)
	}
	var flags [4]byte
	binary.BigEndian.PutUint32(flags[:], 1) // NBD_FLAG_C_FIXED_NEWSTYLE
	if _, err := client.Write(flags[:]); err != nil {
		b.Fatalf("send client flags: %v", err)
	}
	data := goPayload(testExportName)
	var opt [16]byte
	binary.BigEndian.PutUint64(opt[0:8], 0x49484156454f5054) // IHAVEOPT
	binary.BigEndian.PutUint32(opt[8:12], uint32(OptGO))
	binary.BigEndian.PutUint32(opt[12:16], uint32(len(data)))
	if _, err := client.Write(append(opt[:], data...)); err != nil {
		b.Fatalf("send NBD_OPT_GO: %v", err)
	}
	for {
		var hdr [20]byte
		if _, err := io.ReadFull(client, hdr[:]); err != nil {
			b.Fatalf("read option reply: %v", err)
		}
		if n := binary.BigEndian.Uint32(hdr[16:20]); n > 0 {
			if _, err := io.ReadFull(client, make([]byte, n)); err != nil {
				b.Fatalf("read option reply payload: %v", err)
			}
		}
		if OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])) == RepAck {
			return client
		}
	}
}

// benchRequest is a pre-encoded READ so the timed loop measures the protocol
// path and not request encoding.
type benchRequest struct {
	wire   []byte
	length int
}

func newBenchRequest(cookie uint64, offset uint64, length uint32) *benchRequest {
	var buf [28]byte
	binary.BigEndian.PutUint32(buf[0:4], uint32(requestMagic))
	binary.BigEndian.PutUint16(buf[4:6], 0) // no flags
	binary.BigEndian.PutUint16(buf[6:8], uint16(CmdRead))
	binary.BigEndian.PutUint64(buf[8:16], cookie)
	binary.BigEndian.PutUint64(buf[16:24], offset)
	binary.BigEndian.PutUint32(buf[24:28], length)
	return &benchRequest{wire: buf[:], length: int(length)}
}

// readReply consumes one reply header and its payload.
func (r *benchRequest) readReply(b *testing.B, c net.Conn) {
	b.Helper()
	var hdr [16]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		b.Fatalf("read reply: %v", err)
	}
	if magic := binary.BigEndian.Uint32(hdr[0:4]); magic != uint32(simpleReplyMagic) {
		b.Fatalf("reply magic = %#x", magic)
	}
	if errCode := binary.BigEndian.Uint32(hdr[4:8]); errCode != 0 {
		b.Fatalf("reply error = %d", errCode)
	}
	if r.length > 0 {
		if _, err := io.ReadFull(c, make([]byte, r.length)); err != nil {
			b.Fatalf("read payload: %v", err)
		}
	}
}

// BenchmarkConnection_ReadDepth1 is the single-command-in-flight baseline: one
// request on the wire, wait for the reply, repeat.
func BenchmarkConnection_ReadDepth1(b *testing.B) {
	benchmarkReadDepth(b, 1)
}

// BenchmarkConnection_ReadDepth8, _32 and _64 queue that many commands before
// reading any reply. They are the same code path as depth 1 with a different
// window, so the comparison is like for like.
//
// A flat result across these is the measurement that motivates pipelining; a
// rising one is the evidence that it landed.
func BenchmarkConnection_ReadDepth8(b *testing.B)  { benchmarkReadDepth(b, 8) }
func BenchmarkConnection_ReadDepth32(b *testing.B) { benchmarkReadDepth(b, 32) }
func BenchmarkConnection_ReadDepth64(b *testing.B) { benchmarkReadDepth(b, 64) }

func benchmarkReadDepth(b *testing.B, depth int) {
	st := newLatencyStore(benchDelay)
	client := benchmarkConn(b, benchmarkHandler(st))

	const blockSize = 4096
	// One pre-encoded request per in-flight slot. Distinct offsets so a future
	// store-side cache cannot turn the measurement into a cache test, and
	// distinct cookies so a reply is attributable to a slot even when replies
	// arrive out of order.
	reqs := make([]*benchRequest, depth)
	for i := range reqs {
		reqs[i] = newBenchRequest(uint64(i+1), uint64(i%64)*blockSize, blockSize)
	}

	b.SetBytes(int64(blockSize))
	b.ResetTimer()

	sent, received := 0, 0
	for received < b.N {
		for sent < b.N && sent-received < depth {
			if _, err := client.Write(reqs[sent%depth].wire); err != nil {
				b.Fatalf("send request %d: %v", sent, err)
			}
			sent++
		}
		reqs[received%depth].readReply(b, client)
		received++
	}
}

// BenchmarkConnection_ReadStoreCeiling is what the adapter is being compared
// against: the same latency store driven directly, with no protocol in the way.
// It bounds what any dispatch change can achieve, so a pipelined result can be
// read as a fraction of the ceiling rather than as a bare number.
func BenchmarkConnection_ReadStoreCeiling(b *testing.B) {
	st := newLatencyStore(benchDelay)
	const blockSize = 4096
	buf := make([]byte, blockSize)
	ctx := context.Background()

	b.SetBytes(int64(blockSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.ReadAt(ctx, testPayloadID, buf, uint64(i%64)*blockSize); err != nil {
			b.Fatalf("ReadAt: %v", err)
		}
	}
}
