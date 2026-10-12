// Differential oracle: the real handler against a real independent NBD client.
//
// Package nbd, NOT nbd_test, and that is a deliberate choice with a verified
// basis rather than an inheritance.
//
// The reason to be in-package is that this test needs pieces Tasks 4-6 already
// built: storeFake (a seven-method BlockStore fake whose PunchHole zeroes the
// range and whose ReadAt zero-fills past the written extent), resolverFor, and
// integrationIdentity. An external test package cannot see any of them and
// would have to redefine every one — including a second BlockStore fake that
// would drift from the one the other tests use.
//
// It does NOT need pipe, sendRequest, readReplyHeader, completeHandshake or
// exporterFor: the client here is qemu-io, not a test goroutine, so the
// handshake and the framing are the client's business.
//
// An in-package test CAN import internal/adapter/testruntime — integration_test.go
// is package nbd and imports it, and there is no cycle either way
// (`go list -deps ./internal/adapter/testruntime` contains no entry for
// dittofs/internal/adapter/nbd). So the plan's original `package nbd_test`
// constraint was imaginary, and in-package is strictly better here.
//
// WHAT THIS COVERS, and what it does not: the block layer is the in-package
// storeFake, not a real engine.Store, so this compares the CODEC and the
// HANDSHAKE against a known-good server — command framing, reply framing,
// offset and length handling, the transmission flag word, the export-name fold.
// It does NOT cover engine.Store.PunchHole, the journal, the CAS, or durability:
// storeFake.PunchHole zeroes a byte range in a map, which is what makes the
// discard step comparable at all, but a reader who believes this test covers
// PunchHole will trust it for something it never tested. The KVM harness (Task 8)
// is what validates the boot path.
//
// The metadata service, by contrast, IS real: handleTrim and handleWriteZeroes
// call h.meta.PunchHole, which panics on a nil service.
package nbd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// oracleExportName is the wire export name both servers are told to answer to.
//
// qemu-io turns the URI path `nbd://host:port/export/boot` into the wire name
// `export/boot` by stripping exactly one leading `/`. DittoFS accepts that
// spelling because RuntimeExporter.Resolve folds the share half through
// metadata.NormalizeShareName, which re-prepends the slash; qemu-nbd accepts it
// because it is launched with `-x export/boot`. One URI string therefore works
// against both, which is the whole point of pinning the name.
const oracleExportName = "export/boot"

// oracleExportSize is the export's size, and the scratch file's size.
//
// The two must agree. Every command below is bounds-checked against the export:
// DittoFS refuses a WRITE past the end with NBD_ENOSPC and a READ or TRIM past
// the end with NBD_EINVAL, while qemu-nbd against a larger file would succeed —
// so a size mismatch surfaces as a transcript divergence that names neither
// cause. The sequence reaches 9 MiB, so 16 MiB leaves room without being large.
const oracleExportSize = 16 << 20

// oracleCommands is the identical qemu-io sequence run against both servers.
//
// `read -P 0 12M 1M` is the never-written range: storeFake.ReadAt zero-fills
// past both the written extent and past len(data[id]), so it needs no setup and
// still compares equal.
//
// `flush` carries no offset, length or payload, so it exercises the control
// path with no data at all.
var oracleCommands = []string{
	"write -P 0x11 0 1M",
	"write -P 0x22 4M 64k",
	"read -P 0x11 0 1M",
	"discard 1M 1M",
	"read -P 0 1M 1M",
	"write -z 8M 1M",
	"read -P 0 8M 1M",
	"read -P 0 12M 1M",
	"flush",
}

// TestOracle_DifferentialAgainstQemuNBD runs the same qemu-io sequence against
// the real handler and against qemu-nbd serving a scratch file of the same size,
// and requires the normalized transcripts to match.
func TestOracle_DifferentialAgainstQemuNBD(t *testing.T) {
	qemuNBD := oracleRequireBinary(t, "qemu-nbd")
	qemuIO := oracleRequireBinary(t, "qemu-io")
	qemuImg := oracleRequireBinary(t, "qemu-img")

	rt, name, _ := newIntegrationFixtureAsSized(t, integrationIdentity, oracleExportSize)

	// No Resolve here: the handler is built over the REAL exporter and resolves
	// the export itself, per connection, from the name the client sends. That is
	// the path under test — resolving in the test and handing the result to
	// exporterFor(exp) would replace it with a map lookup keyed on
	// "/export/boot", which does not match the `export/boot` spelling qemu-io
	// puts on the wire.
	_ = name

	// The block layer is the in-package fake. It needs no sizing: ReadAt
	// zero-fills both past the written extent and past len(data[id]), so the
	// never-written-range step compares equal without any setup.
	st := newStoreFake()

	// The server must be on a REAL TCP PORT, because the client is qemu-io — a
	// separate process. pipe(t) and go h.serve(ctx, server) are the right tools
	// for every other test in this package and the WRONG ones here: pipe's pair
	// is an in-process loopback socket, so an external process can never reach
	// it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port // from the listener, not from config:
	// BaseAdapter.Port() is the CONFIGURED port (pkg/adapter/base.go:598).

	// The exporter must be the REAL one, not exporterFor(exp). fakeExporter's
	// Resolve is an exact map lookup keyed on "/export/boot", so it returns
	// `nbd: unknown export` for the `export/boot` spelling the client actually
	// sends — and a Tier A built on it would fail the handshake on the first
	// command with an error that has nothing to do with the codec.
	h := NewHandler(NewRuntimeExporter(rt, integrationIdentity), resolverFor(st),
		rt.GetMetadataService(), integrationIdentity,
		Timeouts{Read: 30 * time.Second, Write: 30 * time.Second, Idle: 5 * time.Minute},
		func() bool { return false })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = ln.Close() // unblocks Accept; the loop then returns
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed by cleanup
			}
			go NewConn(h, c).Serve(ctx)
		}
	}()

	scratch := oracleScratchFile(t)
	oracleStartQemuNBD(t, qemuNBD, scratch)

	dittofsURI := fmt.Sprintf("nbd://127.0.0.1:%d/%s", port, oracleExportName)
	qemuNBDURI := fmt.Sprintf("nbd://127.0.0.1:%d/%s", oracleQemuNBDPort, oracleExportName)

	got := normalizeOracleTranscript(oracleTranscript(t, qemuIO, dittofsURI))
	want := normalizeOracleTranscript(oracleTranscript(t, qemuIO, qemuNBDURI))
	if got != want {
		t.Fatalf("the codec's transcript diverges from qemu-nbd's.\n\n--- DittoFS ---\n%s\n--- qemu-nbd ---\n%s\n--- diff ---\n%s",
			got, want, oracleTranscriptDiff(want, got))
	}

	// qemu-img info is deterministic — no timing line — so this one is a plain
	// comparison rather than a normalized one.
	gotInfo := oracleImgInfo(t, qemuImg, dittofsURI)
	wantInfo := oracleImgInfo(t, qemuImg, qemuNBDURI)
	if gotInfo != wantInfo {
		t.Fatalf("qemu-img info diverges.\n--- DittoFS ---\n%s\n--- qemu-nbd ---\n%s", gotInfo, wantInfo)
	}
}

// TestOracle_TransmissionFlagsMatchQemuNBD asserts that every transmission flag
// the adapter advertises is also advertised by qemu-nbd.
//
// The assertion is one-directional on purpose. qemu-nbd advertises extra bits
// (SEND_CACHE, SEND_FAST_ZERO) that this adapter deliberately does not, so
// equality would be wrong; what must hold is ours ⊆ theirs, because the flags we
// DO advertise are the ones a client acts on without verifying.
func TestOracle_TransmissionFlagsMatchQemuNBD(t *testing.T) {
	oracleRequireBinary(t, "qemu-nbd")

	rt, name, _ := newIntegrationFixtureAsSized(t, integrationIdentity, oracleExportSize)
	exp, err := NewRuntimeExporter(rt, integrationIdentity).Resolve(context.Background(), name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	ours := uint16(exp.TransmissionFlags())
	// The two bits the oracle's data path depends on, named so a regression
	// that dropped one fails with the flag rather than with a transcript.
	for _, f := range []struct {
		name string
		flag TransmissionFlags
	}{{"SEND_FLUSH", FlagSendFlush}, {"SEND_FUA", FlagSendFUA}} {
		if ours&uint16(f.flag) == 0 {
			t.Fatalf("the adapter does not advertise %s (flags = 0x%04x)", f.name, ours)
		}
	}

	scratch := oracleScratchFile(t)
	oracleStartQemuNBD(t, oracleRequireBinary(t, "qemu-nbd"), scratch)
	theirs, err := oracleQemuNBDFlags(t, oracleQemuNBDPort, oracleExportName)
	if err != nil {
		t.Fatalf("read qemu-nbd's flags: %v", err)
	}
	t.Logf("measured flag words: DittoFS = 0x%04x, qemu-nbd = 0x%04x, ours-only = 0x%04x, theirs-only = 0x%04x",
		ours, theirs, ours&^theirs, theirs&^ours)
	if ours&^theirs != 0 {
		t.Fatalf("the adapter advertises flags qemu-nbd does not: ours = 0x%04x, qemu-nbd = 0x%04x, extra = 0x%04x",
			ours, theirs, ours&^theirs)
	}
}

// oracleRequireBinary resolves a QEMU tool, skipping the test when it is absent.
//
// A skip rather than a failure: `go test ./...` has to stay green on a machine
// without QEMU. test/nbd/run-oracle.sh checks the prerequisites itself and
// treats a skip as a failure, which is where the absence is supposed to be loud.
func oracleRequireBinary(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not installed; the differential oracle needs qemu (brew install qemu, or the distro's qemu-utils)", name)
	}
	return path
}

// osCreateSized creates a file of exactly size bytes.
//
// The truncate is what sizes it. It extends sparsely, which is fine here because
// nothing reads the scratch file's contents directly — qemu-nbd serves it and
// qemu-io compares patterns against it, and a hole reads as zero exactly like an
// unwritten range does.
func osCreateSized(path string, size int64) (*os.File, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// oracleFlagsScript is the NBD_OPT_GO handshake, run against the oracle's server
// to read the flag word it advertises.
//
// It is a script rather than Go code because parsing the reply is three field
// layouts deep and each one has a trap that a from-scratch implementation gets
// wrong the same way:
//
//   - the greeting is 18 bytes — NBDMAGIC(8) IHAVEOPT(8) gflags(u16). There is
//     NO version field in the fixed-newstyle handshake; reading 16 bytes, or
//     reading a version after the 18, desynchronizes the stream and the next
//     read hangs until the socket times out.
//   - gflags is a capability BITMAP, not a version number, so it is tested with
//     & and never compared.
//   - the NBD_INFO_EXPORT body is u16 info_type, u64 size, u16 flags.
//
// python3 is not a project dependency; a machine without it skips the flag
// test, which is why the flags are also asserted directly against the Export
// before the comparison runs.
const oracleFlagsScript = `#!/usr/bin/env python3
"""Negotiate one export and print its transmission flags.

Usage: nbd-flags.py <port> <export-name>
Exits non-zero if the server does not offer fixed-newstyle or refuses the export.
"""
import socket, struct, sys

NBDMAGIC = 0x4e42444d41474943
IHAVEOPT = 0x49484156454f5054
NBD_FLAG_FIXED_NEWSTYLE = 1 << 0
NBD_FLAG_NO_ZEROES      = 1 << 1
NBD_OPT_GO   = 7
NBD_REP_ACK  = 1
NBD_REP_INFO = 3
NBD_INFO_EXPORT = 0

NAMES = {0: "HAS_FLAGS", 1: "READ_ONLY", 2: "SEND_FLUSH", 3: "SEND_FUA",
         4: "ROTATIONAL", 5: "SEND_TRIM", 6: "SEND_WRITE_ZEROES", 7: "SEND_DF",
         8: "CAN_MULTI_CONN", 9: "SEND_RESIZE", 10: "SEND_CACHE",
         11: "SEND_FAST_ZERO", 12: "BLOCK_STATUS_PAYLOAD", 13: "SEND_BLOCK_STATUS",
         14: "CAN_META_CONTEXT"}

def recvn(s, n):
    b = b""
    while len(b) < n:
        c = s.recv(n - len(b))
        if not c:
            raise EOFError("short read")
        b += c
    return b

def negotiate(port, export):
    s = socket.create_connection(("127.0.0.1", port), timeout=5)
    # TRAP 1: the greeting is 18 bytes -- NBDMAGIC(8) IHAVEOPT(8) gflags(u16).
    # There is NO version field in the fixed-newstyle handshake: the server sends
    # these 18 bytes and then waits for the client flags. Reading 16 bytes (or
    # reading a version after the 18) desynchronizes the stream, and the next
    # read hangs until the socket times out.
    magic, ihav, gflags = struct.unpack(">QQH", recvn(s, 18))
    if magic != NBDMAGIC:
        raise ValueError("bad NBDMAGIC")
    if ihav != IHAVEOPT:
        raise ValueError("bad IHAVEOPT")
    # TRAP 2: gflags is a capability bitmap -- test the bit, do not compare it
    # to a version number. (The old-style handshake's version field is the thing
    # people reach for here, and it is not on the wire.)
    if not (gflags & NBD_FLAG_FIXED_NEWSTYLE):
        raise ValueError(f"server lacks fixed newstyle (gflags=0x{gflags:04x})")
    s.sendall(struct.pack(">I", NBD_FLAG_FIXED_NEWSTYLE | NBD_FLAG_NO_ZEROES))
    name = export.encode()
    payload = struct.pack(">I", len(name)) + name + struct.pack(">H", 0)
    s.sendall(struct.pack(">QII", IHAVEOPT, NBD_OPT_GO, len(payload)) + payload)
    flags = None
    while True:
        _, _, rtype, rlen = struct.unpack(">QIII", recvn(s, 20))
        body = recvn(s, rlen)
        if rtype == NBD_REP_INFO and rlen >= 12:
            info_type = struct.unpack(">H", body[:2])[0]
            if info_type == NBD_INFO_EXPORT:
                # TRAP 3: u16 info_type, u64 size, u16 flags.
                size, flags = struct.unpack(">QH", body[2:12])
        elif rtype == NBD_REP_ACK:
            break
        elif rtype & 0x80000000:
            raise ValueError(f"server refused the export (reply type {rtype})")
    s.close()
    if flags is None:
        raise ValueError("no NBD_INFO_EXPORT in the reply")
    return flags

if __name__ == "__main__":
    f = negotiate(int(sys.argv[1]), sys.argv[2])
    print(f"flags = 0x{f:04x}")
    for bit in range(16):
        if f & (1 << bit):
            print(f"  {bit:2d} {NAMES.get(bit, '?')}")
`

// oracleQemuNBDFlags negotiates export on port and returns the flag word
// qemu-nbd advertises for it.
func oracleQemuNBDFlags(t *testing.T, port int, export string) (uint16, error) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 is not installed; the flag-parity check needs it to parse the NBD_OPT_GO reply")
	}
	scriptPath := t.TempDir() + "/nbd-flags.py"
	if err := os.WriteFile(scriptPath, []byte(oracleFlagsScript), 0o600); err != nil {
		t.Fatalf("write the flags script: %v", err)
	}
	out, err := exec.Command("python3", scriptPath, fmt.Sprint(port), export).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("nbd-flags.py: %w\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		var flags uint16
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "flags = 0x%x", &flags); err == nil {
			return flags, nil
		}
	}
	return 0, fmt.Errorf("no flag line in the script's output:\n%s", out)
}

// oracleScratchFile creates a raw file of exactly the export's size.
func oracleScratchFile(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/scratch.img"
	f, err := osCreateSized(path, oracleExportSize)
	if err != nil {
		t.Fatalf("create scratch file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close scratch file: %v", err)
	}
	return path
}

// oracleStartQemuNBD serves the scratch file with qemu-nbd on a free loopback
// port and registers its shutdown.
//
// -x export/boot is mandatory: measured, qemu-nbd exposes an EMPTY export name
// by default, so the URI that works against DittoFS is refused with
// `server reported: export 'export/boot' not present` on every command. The
// oracle needs the flag even though the code under test does not — the
// asymmetry is what makes it easy to omit.
//
// --discard=unmap is mandatory too, and leaving it out makes the oracle lie:
// without it BDRV_O_UNMAP is clear, a TRIM reaches the block layer and is
// dropped, and qemu-nbd replies success while the data survives. DittoFS's
// PunchHole genuinely zeroes, so the discard step would then diverge on the
// ORACLE's configuration and be reported as a codec bug — the most expensive
// kind of false positive, because the natural fix is to make DittoFS stop
// zeroing.
//
// -b 127.0.0.1 is explicit rather than relied on: this is a throwaway server
// holding a scratch file, and it has no business listening on anything but
// loopback. --persistent keeps it alive across the several qemu-io invocations.
func oracleStartQemuNBD(t *testing.T, qemuNBD, scratch string) {
	t.Helper()
	port := oracleFreePort(t)
	cmd := exec.Command(qemuNBD,
		"-f", "raw",
		"-p", fmt.Sprintf("%d", port),
		"-b", "127.0.0.1",
		"--persistent",
		"--discard=unmap",
		"-x", oracleExportName,
		scratch,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start qemu-nbd: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	oracleWaitForPort(t, port, &stderr, cmd)
	oracleQemuNBDPort = port
}

// oracleWaitForPort polls until the server accepts a connection, and fails with
// qemu-nbd's own stderr if it dies first. A bare sleep would turn a qemu-nbd
// that refused to start into a transcript divergence.
func oracleWaitForPort(t *testing.T, port int, stderr *strings.Builder, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("qemu-nbd never accepted a connection on 127.0.0.1:%d; stderr: %s", port, stderr.String())
}

// oracleQemuNBDPort is the port oracleStartQemuNBD chose. A package variable
// rather than a return value because the flag test needs it after the helper has
// registered its cleanup, and the tests in this file never run in parallel.
var oracleQemuNBDPort int

// oracleFreePort returns a port nothing is listening on.
//
// The listener is closed before the port is handed out, so this is a probe
// rather than a reservation — qemu-nbd takes no inherited socket. The window is
// small and a collision fails loudly at oracleWaitForPort with qemu-nbd's own
// "Failed to find an available port", which names the cause.
func oracleFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe for a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// oracleTranscript runs the command sequence against uri and returns the
// transcript with each command's exit status recorded.
//
// The exit status is the load-bearing half. qemu-io reports a pattern mismatch
// on stdout AND exits 1, but it still prints `read 1048576/1048576 bytes at
// offset 0`, so a transcript that ignores the status reads as success — and the
// discard step's entire signal is its exit code, because `read -P 0` after a
// discarded range exits 0 with --discard=unmap and 1 without it.
//
// The loop continues after a failing command rather than stopping, which is what
// the brief's `set +e` buys: one divergence must not truncate the comparison and
// hide the rest of the sequence.
func oracleTranscript(t *testing.T, qemuIO, uri string) string {
	t.Helper()
	var b strings.Builder
	for _, command := range oracleCommands {
		fmt.Fprintf(&b, "$ qemu-io -f raw -c %q %s\n", command, oracleURIForTranscript)
		out, err := exec.Command(qemuIO, "-f", "raw", "-c", command, uri).CombinedOutput()
		// qemu-io echoes the URI it connected to, and the two servers are on
		// different ports, so the raw output differs on every line for a reason
		// that is not the codec. The real URI is rewritten to the placeholder
		// before the comparison so the transcript compares the protocol, not the
		// port assignment.
		b.WriteString(strings.ReplaceAll(string(out), uri, oracleURIForTranscript))
		code := 0
		if err != nil {
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("qemu-io %q against %s: %v", command, uri, err)
			}
			code = exitErr.ExitCode()
		}
		fmt.Fprintf(&b, "exit=%d\n", code)
	}
	return b.String()
}

// oracleURIForTranscript is what both servers' URIs are rewritten to, so a
// transcript comparison is not a comparison of port numbers.
const oracleURIForTranscript = "nbd://HOST:PORT/export/boot"

// normalizeOracleTranscript drops qemu-io's timing line.
//
// qemu-io prints a line like `1 MiB, 1 ops; 00.00 sec (1.533 GiB/sec and
// 1569.8587 ops/sec)` after every command, and its numbers change on every run —
// measured, two identical runs against the same server differ on that line
// alone, so a literal line-for-line comparison can never pass. Dropping every
// line containing `ops;` was measured to make two identical runs byte-identical.
func normalizeOracleTranscript(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "ops;") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// oracleImgInfo runs qemu-img info, whose output is deterministic — no timing —
// so it is compared verbatim.
func oracleImgInfo(t *testing.T, qemuImg, uri string) string {
	t.Helper()
	out, err := exec.Command(qemuImg, "info", uri).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info %s: %v\n%s", uri, err, out)
	}
	// The URI is scrubbed for the same reason the transcript is: qemu-img echoes
	// it in its `image:` line and the two servers are on different ports.
	return strings.ReplaceAll(string(out), uri, oracleURIForTranscript)
}

// oracleTranscriptDiff renders a line-by-line diff of two transcripts so a
// failure names the diverging command instead of printing two walls of text.
func oracleTranscriptDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	var b strings.Builder
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w == g {
			continue
		}
		fmt.Fprintf(&b, "line %d:\n  qemu-nbd: %s\n  DittoFS : %s\n", i+1, w, g)
	}
	if b.Len() == 0 {
		return "(no line differs; trailing whitespace or length only)"
	}
	return b.String()
}
