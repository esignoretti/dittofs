# NBD Block-Protocol Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give DittoFS an NBD adapter so a KVM guest can boot from a DittoFS share as its root disk.

**Architecture:** A hand-rolled, scoped NBD server in `internal/adapter/nbd/` (wire + dispatch) behind an adapter in `pkg/adapter/nbd/` (lifecycle), following the existing NFS/SMB split. A volume is one metadata file at `volumes/<export-name>` inside a share, so no new control-plane surface is needed. Durability is not reimplemented: `FLUSH` and `FUA` route through the same `common.CommitBlockStore` that NFS COMMIT and SMB Flush already call.

**Tech Stack:** Go 1.26, `pkg/adapter` (BaseAdapter lifecycle), `pkg/controlplane/runtime` (share/block-store resolution), `pkg/metadata` (file operations + `AuthContext`), `pkg/block/engine` (payload I/O), QEMU/KVM + `nbd-client` for the boot test, `qemu-nbd` as a differential oracle.

**Spec:** `docs/superpowers/specs/2026-10-08-dittofs-nbd-block-protocol-design.md`

## Global Constraints

- **Target tree:** `origin/develop` @ `4dae5562`. All line citations below are verified against it.
- **Go module:** `github.com/marmos91/dittofs`, `go 1.26.0`.
- **No new dependencies.** The codec is hand-rolled; the test client is `github.com/pojntfx/go-nbd`'s `pkg/client` only if it is already in `go.mod` — if it is not, integration tests drive the codec over a loopback TCP socket with the in-repo encoder (never `net.Pipe`, which is synchronous and unbuffered: a server `Write` blocks until the reader consumes every byte, so a test that reads a header and stops hangs instead of failing), and the differential oracle (Task 7) provides the independent check.
- **Commit messages:** never name AI tooling, never add `Co-Authored-By` for AI. Concise. Sign with `git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S` (plain `-S` fails: `gpg.format` is unset).
- **Adapter type string:** `"nbd"`. **Default port:** `10809`.
- **Transmission flags advertised:** exactly `HAS_FLAGS` (0), `READ_ONLY` (1, conditionally), `SEND_FLUSH` (2), `SEND_FUA` (3), `SEND_TRIM` (5), `SEND_WRITE_ZEROES` (6). Nothing else.
- **Protocol constants** (from `NetworkBlockDevice/nbd` `doc/proto.md`): `NBDMAGIC`/`INIT_PASSWD` `0x4e42444d41474943`; `IHAVEOPT` `0x49484156454f5054`; option reply magic `0x3e889045565a9`; request magic `0x25609513`; simple reply magic `0x67446698`. Header sizes: option request 16 B, option reply 20 B, simple request 28 B, simple reply 16 B.
- **Error values:** `NBD_EPERM` 1, `NBD_EIO` 5, `NBD_EINVAL` 22, `NBD_ENOSPC` 28, `NBD_ESHUTDOWN` 108. `NBD_EOVERFLOW` (75) and `NBD_ENOTSUP` (95) exist in the protocol but are **never returned**: the spec reserves `EOVERFLOW` for `READ`+`DF` and payload-less over-max requests, and `ENOTSUP` for `WRITE_ZEROES`+`FAST_ZERO` — neither of which this adapter advertises. Unknown commands and inapplicable flags are `NBD_EINVAL`.
- **No hole-reporting surface in v1.** Do not implement `NBD_CMD_BLOCK_STATUS`, do not negotiate `NBD_OPT_STRUCTURED_REPLY`, and do not advertise `BLOCK_STATUS_PAYLOAD`. This is the residency-truth gate (spec §7): `engine.Store.DataExtents` derives its answer from local bytes plus manifest rows, so a range in neither — which is what `journal.StateLost` describes — is absent from the map and would be reported as a **hole**, letting a client skip real data. Today no code path holds `StateLost` (`pkg/block/journal/state.go:40`), but that is a runtime invariant, not a structural one. With no hole-reporting surface there is no way to violate the rule. Phase 4 adds `BLOCK_STATUS` *together with* `journal.Store.Extents` (`pkg/block/journal/state.go:111`, currently called only by its own test), so a lost range reports as **data** and forces a loud READ failure.
- **Comments:** describe behaviour, never reference issue/PR numbers, phase IDs, or this plan. Use `decision:` markers per CLAUDE.md where a choice is deliberately narrower than it looks.
- **Verify a change the way CLAUDE.md asks:** put the regression test at the layer that *consumes* the data, and prove it by reverting the fix and watching it fail on the assertion.

## Spec Coverage

Every section of the design doc, and where the plan implements it. A section with no task is a section the plan silently dropped; a task with no section is scope the spec never approved.

| Spec | Implemented by |
|---|---|
| §1 Intent, §1.1 "working", §1.2 deployment context | Global Constraints (target tree, no new dependencies) and the Architecture line; §1.2's `CommitAck` choice is an operator setting, not a code path |
| §2.1 Codec hand-rolled (A3) | The whole plan: no NBD dependency is added, and the codec is Task 1 + Task 2 |
| §2.2 Volume model (B1) | Task 2 (`volumes/<export-name>`), Task 3 (`default_export`), Task 8's NFS-readability check |
| §3.1 Sibling adapter | Task 3 (`pkg/adapter/nbd`, `createNBDAdapter`) |
| §3.2 Layer split | File Structure; Task 4's `Conn`/`Handler` split and the `BlockStore` interface |
| §3.3 Control-plane registration | Task 3 Step 6 |
| §3.4 Bind-address gate | Task 3 Step 4 + `TestServe_RefusesNonLoopbackBindWithoutOptIn` |
| §4.1 Handshake | Task 2 (`ServeHandshake`) |
| §4.2 Transmission flags | Task 2 (`TransmissionFlags`), asserted in Task 2 and Task 7 |
| §4.3 Command set | Tasks 4–6 (one command per handler), Task 4's `mapError` |
| §4.4 Export size fixed | Task 2 (`Export.Size` from `metadata.File.Size`), Task 5 (a `WRITE` past the end is `ENOSPC`, never an extend) |
| §4.5 Serialization | Task 4 Step 3 (one command in flight, replies written before the next read) |
| §4.6 Error mapping | Task 4 Step 5 (`mapError`) and every handler's bounds checks |
| §4.7 Identity | Task 3 (`Config.UID`/`GID`), Task 4 (`Identity`, `authContext`) |
| §5.1 What the adapter promises | Task 5 (`h.commit` on FLUSH and FUA) |
| §5.2 Restart obligations | Task 3 (same port), Task 2 (same name and size), Task 4 (drain), Task 8 (the measured window) |
| §6.1 Dead-connection timeout | Task 8 (`crash-restart.sh` configures and prints it) |
| §6.2 Caveats as harness requirements | Task 8 (the hung-task-detector bound, the `-timeout`/`-persist` distinction) |
| §7 Residency-truth gate, §7.1–§7.4 | Global Constraints (no `BLOCK_STATUS`, no structured replies, no `BLOCK_STATUS_PAYLOAD`) and Task 2's refusal test; Phase 4 work is explicitly out of scope, so no task implements §7.3's fix — that is the point of the gate |
| §8 Healthcheck | Task 3 Step 5 (`Disabled`, delegation), Task 4 Step 6 (`Degraded`) |
| §9.1 Unit — codec | Task 1, Task 2, and each handler task's negative cases |
| §9.2 Integration — in-process | Task 4 Step 7, Task 5 Step 7, Task 6 Step 6 |
| §9.3 Crash tier | Task 8 (`crash-restart.sh`) |
| §9.4 Differential oracle | Task 7 |
| §9.5 KVM harness | Task 8 (`boot-qemu.sh`) |
| §10 Phasing | The task order is the phase order: 1–2 = phase 0, 3–4 = phase 1, 5–6 = phase 2, 7 = phase 3, 8 = the acceptance gate |
| §11 Out of scope | Global Constraints' residency gate, and the absence of any task for STARTTLS, resize, `CAN_MULTI_CONN`, multi-export, or pipelining |
| §12 Risks | Each risk is priced in the task that could incur it: §12's TRIM cost in Task 6, the serialization throughput risk in Task 4's `decision:` note |
| §13 Verification checklist | Verification Checklist (this plan's, which is a superset) |

---

## Review Focus

Inputs and conditions the spec implies but whose tests no single task obviously owns. Each is pinned to the task that owns the code, in that task's own step style.

1. **A wire-declared length that lies about the data that follows.** `NBD_OPT_GO`/`INFO` carry a 32-bit name length the spec bounds by the option data length, and the option header itself carries a 32-bit data length. A reasonable person expects a malformed or hostile length to produce `NBD_REP_ERR_INVALID` on the wire — not a panic, not a slice bounds error, and not an allocation sized by the client. Most likely to bite, because the naive read is `make([]byte, length)`.
2. **`offset + length == exportSize`, exactly.** The single most common off-by-one in a block server. A reasonable person expects the last byte of the device to be both readable and writable; only `offset + length > exportSize` is out of range.
3. **A zero-length request.** The spec says a client SHOULD NOT send `READ`, `WRITE`, `WRITE_ZEROES`, or `TRIM` with length 0 and that server behaviour is unspecified — but also that the server SHOULD NOT disconnect. A reasonable person expects a *defined* answer, and the plan gives one answer for all four: `NBD_EINVAL`. The argument for uniformity over per-command cleverness is that no correct client sends a zero-length block request (the kernel client does not), the spec leaves the choice open, and one rule is a rule nobody has to remember — a per-command table is a table somebody gets wrong. It also matches how the rest of the adapter answers every other malformed request. For `WRITE` and `WRITE_ZEROES` specifically there is a second requirement: length 0 means there is no payload to drain, so the connection must remain usable — a following command on the same connection must succeed.
4. **A known command carrying a flag it does not accept.** `NBD_CMD_FLAG_NO_HOLE` on `WRITE` (it is documented only for `WRITE_ZEROES`), or `DF`/`REQ_ONE`/`FAST_ZERO` on anything. A reasonable person expects a refusal, because silently ignoring a flag means reporting success for a guarantee that was never provided — the same failure shape as `SEND_FLUSH` unadvertised-but-implied. Note the two exceptions the spec forces: `FUA` is valid on *every* command once `SEND_FUA` is advertised (*"the server MUST accept all commands with this bit set (even by ignoring the bit)"*), and `NO_HOLE` **must** be supported on `WRITE_ZEROES` because we advertise `SEND_WRITE_ZEROES`.
5. **A connection that sends a partial header and stalls.** A reasonable person expects the connection to be closed once `Timeouts.Read` elapses, with no unbounded buffer growth in the meantime.
6. **A rejected `WRITE` whose payload is still in flight.** The client always sends `length` bytes after a `WRITE` header — the spec states it unconditionally, with no exemption for requests the server intends to refuse. A reasonable person expects a rejected write to leave the connection usable; the failure mode is that the unconsumed payload is parsed as the next request header, and the client sees a protocol error on some later, unrelated command. Highest-value item in the list because the symptom is remote from the cause. The drain must use `io.CopyN(io.Discard, ...)`, never `make([]byte, length)`, or a declared 4 GiB length becomes a 4 GiB allocation. The **reply is written before the drain**, not after: the two travel in opposite directions, so neither orders the other, and replying first is what lets a client that stalled mid-payload learn the reason instead of seeing only a closed socket.
7. **A panic inside a connection handler.** The base adapter's accept loop has no `recover` (`grep -rn "recover()" pkg/adapter/base.go` returns nothing) and it invokes the handler on a bare goroutine (`pkg/adapter/base.go:373`), so an unrecovered panic does not kill the connection — it kills the process, taking every NFS and SMB connection with it. A reasonable person expects a malformed request from one client to cost that client's connection and nothing else, and both sibling adapters recover for exactly this reason: NFS at `pkg/adapter/nfs/connection.go:420` (deferred at `:111`), SMB at `pkg/adapter/smb/connection.go:474`. Pinned by `TestConn_PanicInHandlerDoesNotKillTheProcess` in Task 4.
8. **A store resolution that fails after the handshake succeeded.** The export is resolved during the handshake, but the block store is resolved *after* it (Task 4 Step 3, items 1 and 2), so a share removed, disabled, or reloaded in between leaves a connection whose export is valid and whose store is gone. A reasonable person expects the connection to end rather than to serve reads from a nil store, and expects the operator to be able to see that it happened — which is what the `Degraded` counter is for. Pinned by `TestConnection_FailedStoreResolutionDegradesAndCounts` in Task 4.

---

## File Structure

**Created — wire and dispatch (`internal/adapter/nbd/`):**

| File | Responsibility |
|---|---|
| `protocol.go` | Constants (magics, flags, commands, errors, options), request/reply header encode+decode. No I/O, no store access. |
| `handshake.go` | Server greeting, option haggling (`GO`, `INFO`, `LIST`, `ABORT`, `EXPORT_NAME`), transmission-flag computation. |
| `reply.go` | Writing simple replies and error replies; the one place a reply header is serialized. |
| `connection.go` | Per-connection loop: read header → dispatch → reply. Owns serialization and shutdown answers. |
| `volume.go` | The only file that talks to the runtime: export name → metadata file → payload ID → export size. |
| `handlers_read.go` | `NBD_CMD_READ`. |
| `handlers_write.go` | `NBD_CMD_WRITE`, `NBD_CMD_WRITE_ZEROES`, `NBD_CMD_TRIM`. |
| `handlers_control.go` | `NBD_CMD_FLUSH`, `NBD_CMD_DISC`, and the FUA barrier path. |

**Created — adapter lifecycle (`pkg/adapter/nbd/`):**

| File | Responsibility |
|---|---|
| `config.go` | `Config`, `TimeoutsConfig`, `applyDefaults`, `validate`. |
| `adapter.go` | `Adapter`, `New`, `Serve`, `Stop`, `SetRuntime`, `NewConnection`. Embeds `adapter.BaseAdapter`. |
| `healthcheck.go` | `Healthcheck` override: `Disabled` → `Degraded` → delegate. |

**Modified:**

| File | Change |
|---|---|
| `pkg/controlplane/models/adapter.go` | `DefaultPort` gains `"nbd"` → `10809`; `Validate` error text names `nbd`. |
| `internal/adapter/common/write_payload.go` | `CommitBlockStore`'s store parameter widened from `*engine.Store` to a new `Committer` interface, so the wire layer can reuse the durability rule without the concrete type. Type-only; no behaviour change, no call-site change. |
| `cmd/dfs/commands/start.go` | `createAdapterFactory` gains `case "nbd"`; add `createNBDAdapter`. |
| `docs/guide/cli.md` | Regenerated. |

**Created — tests:**

| File | Covers |
|---|---|
| `internal/adapter/nbd/protocol_test.go` | Header round-trips, flag matrix. |
| `internal/adapter/nbd/handshake_test.go` | Option sequences, malformed input. |
| `internal/adapter/nbd/connection_test.go` | Dispatch, serialization, shutdown, timeouts. |
| `internal/adapter/nbd/volume_test.go` | Export resolution against a real runtime. |
| `internal/adapter/nbd/integration_test.go` | Real `engine.Store`, real bytes, end to end over a loopback TCP socket. |
| `pkg/adapter/nbd/adapter_test.go` | Lifecycle, config validation, bind gate. |
| `pkg/adapter/nbd/healthcheck_test.go` | Status mapping. |

**Created — harness (Task 8), not Go:**

| File | Covers |
|---|---|
| `test/nbd/run-oracle.sh` | Differential comparison against `qemu-nbd`; drives Tier A (`go test -run TestOracle_`) and Tier B when a server port is given. |
| `internal/adapter/nbd/oracle_test.go` | Tier A: the real `Handler` on a real TCP listener behind a fake `Exporter`, driven by the real `qemu-io`, diffed against `qemu-nbd`. Internal (`package nbd`), so it reuses `newWriteVolumeFixture`, `storeFake`, `pipe` and `sendRequest` rather than duplicating them — an external test package would have to redefine all of them, including a seven-method `BlockStore` fake. |
| `test/nbd/boot-qemu.sh` | KVM boot over NBD. |
| `test/nbd/crash-restart.sh` | SIGKILL mid-write, restart, resume. |
| `test/nbd/README.md` | Prerequisites and what each script proves. |

---

## Task 1: Wire codec

**Files:**
- Create: `internal/adapter/nbd/protocol.go`
- Test: `internal/adapter/nbd/protocol_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Command uint16`, `type CommandFlags uint16`, `type TransmissionFlags uint16`, `type Option uint32`, `type OptionReplyType uint32`, `type NBDError uint32`
  - Constants: `CmdRead Command = 0`, `CmdWrite = 1`, `CmdDisc = 2`, `CmdFlush = 3`, `CmdTrim = 4`, `CmdCache = 5`, `CmdWriteZeroes = 6`, `CmdBlockStatus = 7`
  - `FlagFUA CommandFlags = 1 << 0`, `FlagNoHole = 1 << 1`, `FlagDF = 1 << 2`, `FlagReqOne = 1 << 3`, `FlagFastZero = 1 << 4`
  - `FlagHASFlags TransmissionFlags = 1 << 0`, `FlagReadOnly = 1 << 1`, `FlagSendFlush = 1 << 2`, `FlagSendFUA = 1 << 3`, `FlagSendTrim = 1 << 5`, `FlagSendWriteZeroes = 1 << 6`
  - `ErrEPERM NBDError = 1`, `ErrEIO = 5`, `ErrEINVAL = 22`, `ErrENOSPC = 28`, `ErrESHUTDOWN = 108`. **`NBD_EOVERFLOW` (75) and `NBD_ENOTSUP` (95) are deliberately absent**: the spec reserves both (`"The server SHOULD NOT return NBD_EOVERFLOW except as documented in response to NBD_CMD_READ when NBD_CMD_FLAG_DF is supported, or when a command without payload requests a length larger than an advertised maximum payload length"`, and the same shape for `ENOTSUP` with `FAST_ZERO`), and we advertise neither `SEND_DF` nor `SEND_FAST_ZERO`. Define them as constants only if a test needs to assert their absence; the handlers never return them.
  - `OptExportName Option = 1`, `OptAbort = 2`, `OptList = 3`, `OptStartTLS = 5`, `OptInfo = 6`, `OptGO = 7`, `OptStructuredReply = 8`, `OptListMetaContext = 9`, `OptSetMetaContext = 10`
  - `RepAck OptionReplyType = 1`, `RepServer = 2`, `RepInfo = 3`, `RepMetaContext = 4`; `RepErrUnsup = 0x80000001`, `RepErrPolicy = 0x80000002`, `RepErrInvalid = 0x80000003`, `RepErrPlatform = 0x80000004`, `RepErrTLSReqd = 0x80000005`, `RepErrUnknown = 0x80000006`, `RepErrShutdown = 0x80000007`, `RepErrBlockSizeReqd = 0x80000008`, `RepErrTooBig = 0x80000009`, `RepErrExtHeaderReqd = 0x8000000A`
  - `type Request struct { Magic uint32; Flags CommandFlags; Type Command; Cookie uint64; Offset uint64; Length uint32 }`
  - `func (r *Request) Encode(w io.Writer) error`
  - `func ReadRequest(r io.Reader) (*Request, error)`
  - `type Reply struct { Magic uint32; Error NBDError; Cookie uint64 }`
  - `func (r *Reply) Encode(w io.Writer) error`
  - `func ReadReply(r io.Reader) (*Reply, error)`
  - `type OptionHeader struct { Magic uint64; Option Option; Length uint32 }`
  - `func ReadOptionHeader(r io.Reader) (*OptionHeader, error)`
  - `func (o *OptionHeader) Encode(w io.Writer) error`
  - `type OptionReplyHeader struct { Magic uint64; Option Option; Type OptionReplyType; Length uint32 }`
  - `func (o *OptionReplyHeader) Encode(w io.Writer) error`
  - `func ReadOptionReplyHeader(r io.Reader) (*OptionReplyHeader, error)`
  - `var ErrBadMagic = errors.New("nbd: bad magic")` — the only sentinel the codec needs. There is deliberately no `ErrBadCommand`: an unknown command *value* is a well-formed 28-byte header that the dispatcher answers with `ErrEINVAL`, not a decode failure. Validating it at decode time would put a protocol rule in the codec and make the dispatcher's `default` branch unreachable.

- [ ] **Step 1: Write the failing round-trip test**

Create `internal/adapter/nbd/protocol_test.go`. All values below are the spec's, not invented.

```go
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
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run 'TestRequest|TestReply|TestOption|TestMagic' -v`
Expected: FAIL — `undefined: requestMagic`, `undefined: Request`, and the rest.

- [ ] **Step 3: Implement `protocol.go`**

Write the constants and the four encode/decode pairs. Bodies are determined by the header layouts in the spec, so they are not transcribed here. Two constraints that are not visible from the layouts:

- **Decode from a `[]byte` read with `io.ReadFull`, not field-by-field `binary.Read` calls.** `io.ReadFull` on a fixed-size buffer gives one error path for a truncated header and guarantees no partial decode is acted on. Field-by-field reads produce a half-populated `Request` on error, which is the shape that makes `ErrBadMagic` unreachable in practice.
- **`ReadRequest` checks the magic before populating anything else**, returning `ErrBadMagic` wrapped with the offending value: `fmt.Errorf("%w: %#x", ErrBadMagic, magic)`.
- `ReadOptionReplyHeader` is needed by tests, not by the server. Say so in its doc comment rather than leaving a reader to wonder.

Use `encoding/binary.BigEndian` throughout — NBD is big-endian on the wire.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/adapter/nbd/ -run 'TestRequest|TestReply|TestOption|TestMagic' -v`
Expected: PASS, 7 tests.

- [ ] **Step 5: Verify the magic-value test actually bites**

Temporarily change `requestMagic` to `0x25609514`, run `go test ./internal/adapter/nbd/ -run TestMagicValues -v`, and confirm it fails naming the request magic. Revert.

- [ ] **Step 6: Commit**

```bash
git add internal/adapter/nbd/protocol.go internal/adapter/nbd/protocol_test.go
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "feat(nbd): wire codec for the NBD protocol"
```

---

## Task 2: Handshake and export resolution

**Files:**
- Create: `internal/adapter/nbd/handshake.go`, `internal/adapter/nbd/reply.go`, `internal/adapter/nbd/volume.go`
- Test: `internal/adapter/nbd/handshake_test.go`, `internal/adapter/nbd/volume_test.go`

**Interfaces:**
- Consumes: Task 1's constants, `OptionHeader`, `OptionReplyHeader`, `TransmissionFlags`.
- Produces:
  - `type Export struct { Name string; Size uint64; PayloadID metadata.PayloadID; Handle metadata.FileHandle; ReadOnly bool }` — `Handle` is what the write path needs for `PrepareWrite`/`CommitWrite`; it is resolved once at handshake and held for the connection's life.
  - `type Exporter interface { Resolve(ctx context.Context, name string) (*Export, error); List(ctx context.Context) ([]string, error) }`
  - `var ErrUnknownExport = errors.New("nbd: unknown export")`
  - `type Identity struct { UID, GID uint32 }` — the configured synthetic identity (spec §4.7). Declared here, not in Task 4, because `NewRuntimeExporter` below takes one: the handshake needs it to build the `AuthContext` that `metadata.Service.Lookup` requires, so this task's own commit cannot compile without it. An earlier draft declared it in Task 4 and left this task uncompilable until Task 4 landed.
  - `func ServeHandshake(ctx context.Context, conn net.Conn, ex Exporter) (*Export, error)` — writes the greeting, haggles options, returns the resolved export with the transmission phase ready to begin. Returns `ErrUnknownExport`-wrapped errors only through the wire; a client that names no valid export ends with the connection closed by this function.
  - `func (e *Export) TransmissionFlags() TransmissionFlags`
  - `type RuntimeExporter struct { rt *runtime.Runtime; id Identity; volumesDir string }`
  - `func NewRuntimeExporter(rt *runtime.Runtime, id Identity) *RuntimeExporter` — `id` is needed because `Lookup` takes an `*AuthContext` and the handshake has no client identity; see Step 5.
  - `func (r *RuntimeExporter) Resolve(ctx context.Context, name string) (*Export, error)`
  - `func (r *RuntimeExporter) List(ctx context.Context) ([]string, error)`
  - `func writeOptionReply(w io.Writer, opt Option, typ OptionReplyType, data []byte) error` (in `reply.go`)
  - `func writeSimpleReply(w io.Writer, cookie uint64, err NBDError) error` (in `reply.go`)

**Export-name syntax:** `<share>/<volume>`, e.g. `/export/boot`. `volumesDir` is the fixed string `"volumes"`. The file lives at `<share>/volumes/<volume>`, and the share part is resolved with `rt.GetRootHandle(share)` then `metadata.Service.Lookup` down the path.

- [ ] **Step 1: Write the failing handshake tests**

Create `internal/adapter/nbd/handshake_test.go`. Drive both sides over a
**loopback TCP socket**, via the `pipe(t)` helper the block below defines — **not**
`net.Pipe()`. `net.Pipe` is synchronous and unbuffered: a server `Write` blocks until the
reader has consumed every byte, so a test that reads a reply header and stops before the
payload leaves the server blocked and the test hangs until the Go test timeout instead of
failing. Several tests below do exactly that. A loopback socket buffers in the kernel, so
the server finishes its reply whether or not the test reads it.

```go
package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type fakeExporter struct {
	exports map[string]*Export
}

func (f *fakeExporter) Resolve(_ context.Context, name string) (*Export, error) {
	if e, ok := f.exports[name]; ok {
		return e, nil
	}
	return nil, ErrUnknownExport
}

func (f *fakeExporter) List(context.Context) ([]string, error) {
	names := make([]string, 0, len(f.exports))
	for n := range f.exports {
		names = append(names, n)
	}
	return names, nil
}

func newFakeExporter() *fakeExporter {
	return &fakeExporter{exports: map[string]*Export{
		"/export/boot": {Name: "/export/boot", Size: 1 << 30, PayloadID: "payload-1"},
	}}
}

// pipe returns a connected pair over a loopback TCP socket.
//
// net.Pipe would be simpler and is wrong here: it is synchronous and
// unbuffered, so a server Write blocks until the reader has consumed every
// byte. These tests deliberately read a reply header and assert on it without
// draining the payload that follows, which leaves the server blocked forever
// and turns a test that should fail loudly into one that hangs until the Go
// test timeout. A loopback socket buffers in the kernel, so a server can
// finish a reply the test has not read yet.
func pipe(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		ch <- accepted{c, err}
	}()

	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	a := <-ch
	if a.err != nil {
		t.Fatalf("accept: %v", a.err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = a.conn.Close()
	})
	return a.conn, client
}

// readGreeting consumes NBDMAGIC, IHAVEOPT and the 16-bit handshake flags, and
// returns the flags. Every client does this before anything else.
func readGreeting(t *testing.T, r io.Reader) uint16 {
	t.Helper()
	var buf [18]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if got := binary.BigEndian.Uint64(buf[0:8]); got != nbdMagic {
		t.Fatalf("greeting magic = %#x, want %#x", got, uint64(nbdMagic))
	}
	if got := binary.BigEndian.Uint64(buf[8:16]); got != ihaveoptMagic {
		t.Fatalf("IHAVEOPT = %#x, want %#x", got, uint64(ihaveoptMagic))
	}
	return binary.BigEndian.Uint16(buf[16:18])
}

func sendClientFlags(t *testing.T, w io.Writer, flags uint32) {
	t.Helper()
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], flags)
	if _, err := w.Write(buf[:]); err != nil {
		t.Fatalf("write client flags: %v", err)
	}
}

func sendOption(t *testing.T, w io.Writer, opt Option, data []byte) {
	t.Helper()
	h := &OptionHeader{Magic: ihaveoptMagic, Option: opt, Length: uint32(len(data))}
	if err := h.Encode(w); err != nil {
		t.Fatalf("encode option header: %v", err)
	}
	if len(data) > 0 {
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write option data: %v", err)
		}
	}
}

// goPayload builds an NBD_OPT_GO / NBD_OPT_INFO payload: name length, name,
// 16-bit count of information requests, then the requests.
func goPayload(name string, infos ...uint16) []byte {
	var buf bytes.Buffer
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(name)))
	buf.Write(n[:])
	buf.WriteString(name)
	var c [2]byte
	binary.BigEndian.PutUint16(c[:], uint16(len(infos)))
	buf.Write(c[:])
	for _, i := range infos {
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], i)
		buf.Write(b[:])
	}
	return buf.Bytes()
}

func TestHandshake_GreetingAdvertisesFixedNewstyle(t *testing.T) {
	server, client := pipe(t)

	done := make(chan error, 1)
	go func() {
		_, err := ServeHandshake(context.Background(), server, newFakeExporter())
		done <- err
	}()

	flags := readGreeting(t, client)
	if flags&1 == 0 {
		t.Fatalf("handshake flags %#x do not set NBD_FLAG_FIXED_NEWSTYLE (bit 0)", flags)
	}
	// The server must not proceed before reading client flags.
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHandshake did not return after the client closed")
	}
}

func TestHandshake_GoReturnsExportInfoThenAck(t *testing.T) {
	server, client := pipe(t)

	got := make(chan *Export, 1)
	go func() {
		exp, err := ServeHandshake(context.Background(), server, newFakeExporter())
		if err != nil {
			got <- nil
			return
		}
		got <- exp
	}()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)                                 // NBD_FLAG_C_FIXED_NEWSTYLE
	sendOption(t, client, OptGO, goPayload("/export/boot", 0, 3)) // NBD_INFO_EXPORT, NBD_INFO_BLOCK_SIZE

	// NBD_INFO_EXPORT is mandatory before a successful ACK.
	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read first option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepInfo {
		t.Fatalf("first reply type = %#x, want NBD_REP_INFO (%#x)", typ, uint32(RepInfo))
	}
	data := make([]byte, binary.BigEndian.Uint32(hdr[16:20]))
	if _, err := io.ReadFull(client, data); err != nil {
		t.Fatalf("read INFO payload: %v", err)
	}
	if infoType := binary.BigEndian.Uint16(data[0:2]); infoType != 0 {
		t.Fatalf("info type = %d, want NBD_INFO_EXPORT (0)", infoType)
	}
	if size := binary.BigEndian.Uint64(data[2:10]); size != 1<<30 {
		t.Fatalf("advertised size = %d, want %d", size, 1<<30)
	}
	flags := TransmissionFlags(binary.BigEndian.Uint16(data[10:12]))
	if flags&FlagSendFlush == 0 {
		t.Error("NBD_FLAG_SEND_FLUSH not advertised: QEMU turns every guest fsync into a silent success without it")
	}
	if flags&FlagHASFlags == 0 {
		t.Error("NBD_FLAG_HAS_FLAGS not advertised")
	}
	if flags&(1<<8) != 0 {
		t.Error("NBD_FLAG_CAN_MULTI_CONN advertised: two connections on one volume is a corruption path")
	}

	exp := <-got
	if exp == nil || exp.Name != "/export/boot" {
		t.Fatalf("resolved export = %+v, want /export/boot", exp)
	}
}

func TestHandshake_UnknownExportIsRepErrUnknown(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	sendOption(t, client, OptGO, goPayload("/export/nope"))

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepErrUnknown {
		t.Fatalf("reply type = %#x, want NBD_REP_ERR_UNKNOWN (%#x)", typ, uint32(RepErrUnknown))
	}
}

func TestHandshake_StartTLSIsUnsupported(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	sendOption(t, client, OptStartTLS, nil)

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepErrUnsup {
		t.Fatalf("reply type = %#x, want NBD_REP_ERR_UNSUP", typ)
	}
	// The connection must not be left half-negotiated: a second option must
	// still be answerable.
	sendOption(t, client, OptAbort, nil)
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read ABORT reply after STARTTLS refusal: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepAck {
		t.Fatalf("ABORT after STARTTLS refusal: type = %#x, want ACK", typ)
	}
}

// Review Focus 1: a name length that claims more bytes than the option carried.
// The naive read is make([]byte, nameLen) followed by a slice of the payload.
func TestHandshake_GoNameLengthExceedsOptionData(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)

	// Claim a 4096-byte name in an option that carries 8 bytes.
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[0:4], 4096)
	copy(payload[4:], "/export/")
	sendOption(t, client, OptGO, payload)

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepErrInvalid {
		t.Fatalf("reply type = %#x, want NBD_REP_ERR_INVALID (%#x)", typ, uint32(RepErrInvalid))
	}
}

// Review Focus 1, second shape: the option header's own data length is absurd.
// The server must not allocate what the client asked for.
func TestHandshake_HugeOptionLengthRejected(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)

	h := &OptionHeader{Magic: ihaveoptMagic, Option: OptGO, Length: 0xffffffff}
	if err := h.Encode(client); err != nil {
		t.Fatalf("encode: %v", err)
	}

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepErrTooBig {
		t.Fatalf("reply type = %#x, want NBD_REP_ERR_TOO_BIG (%#x)", typ, uint32(RepErrTooBig))
	}
}

func TestHandshake_ExportNameLegacyPath(t *testing.T) {
	server, client := pipe(t)

	got := make(chan *Export, 1)
	go func() {
		exp, err := ServeHandshake(context.Background(), server, newFakeExporter())
		if err != nil {
			got <- nil
			return
		}
		got <- exp
	}()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	// Legacy path: the name is the entire option payload and there is no
	// option reply — the export size and flags come back in lieu of one.
	sendOption(t, client, OptExportName, []byte("/export/boot"))

	var buf [10]byte
	if _, err := io.ReadFull(client, buf[:]); err != nil {
		t.Fatalf("read export reply: %v", err)
	}
	if size := binary.BigEndian.Uint64(buf[0:8]); size != 1<<30 {
		t.Fatalf("export size = %d, want %d", size, 1<<30)
	}
	if flags := TransmissionFlags(binary.BigEndian.Uint16(buf[8:10])); flags&FlagSendFlush == 0 {
		t.Error("SEND_FLUSH not advertised on the legacy path")
	}
	if exp := <-got; exp == nil || exp.Name != "/export/boot" {
		t.Fatalf("resolved export = %+v", exp)
	}
}

func TestHandshake_ListEnumeratesVolumes(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	sendOption(t, client, OptList, nil)

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read LIST reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepServer {
		t.Fatalf("LIST reply type = %#x, want NBD_REP_SERVER", typ)
	}
	length := binary.BigEndian.Uint32(hdr[16:20])
	data := make([]byte, length)
	if _, err := io.ReadFull(client, data); err != nil {
		t.Fatalf("read LIST entry: %v", err)
	}
	nameLen := binary.BigEndian.Uint32(data[0:4])
	if got := string(data[4 : 4+nameLen]); got != "/export/boot" {
		t.Fatalf("listed name = %q, want %q", got, "/export/boot")
	}
}

func TestHandshake_AbortIsAckedAndConnectionClosed(t *testing.T) {
	server, client := pipe(t)

	done := make(chan error, 1)
	go func() {
		_, err := ServeHandshake(context.Background(), server, newFakeExporter())
		done <- err
	}()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	sendOption(t, client, OptAbort, nil)

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read ABORT reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepAck {
		t.Fatalf("ABORT reply type = %#x, want ACK", typ)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ABORT returned nil; the caller cannot tell the session ended deliberately")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHandshake did not return after ABORT")
	}
}

func TestHandshake_UnsupportedOptionIsRefused(t *testing.T) {
	server, client := pipe(t)

	go func() { _, _ = ServeHandshake(context.Background(), server, newFakeExporter()) }()

	readGreeting(t, client)
	sendClientFlags(t, client, 1)
	sendOption(t, client, OptStructuredReply, nil)

	var hdr [20]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatalf("read option reply: %v", err)
	}
	if typ := OptionReplyType(binary.BigEndian.Uint32(hdr[12:16])); typ != RepErrUnsup {
		t.Fatalf("NBD_OPT_STRUCTURED_REPLY reply = %#x, want NBD_REP_ERR_UNSUP", typ)
	}
}

func TestExport_TransmissionFlagsIncludeReadOnlyOnlyWhenSet(t *testing.T) {
	rw := &Export{Size: 1 << 20}
	if rw.TransmissionFlags()&FlagReadOnly != 0 {
		t.Error("writable export advertised NBD_FLAG_READ_ONLY")
	}
	ro := &Export{Size: 1 << 20, ReadOnly: true}
	if ro.TransmissionFlags()&FlagReadOnly == 0 {
		t.Error("read-only export did not advertise NBD_FLAG_READ_ONLY")
	}
	// The omissions are the load-bearing part.
	for _, f := range []TransmissionFlags{1 << 8, 1 << 7, 1 << 9, 1 << 10, 1 << 11, 1 << 12, 1 << 4} {
		if ro.TransmissionFlags()&f != 0 {
			t.Errorf("export advertised flag bit %d, which the design excludes", f)
		}
	}
}

func TestRuntimeExporter_UnknownShareIsUnknownExport(t *testing.T) {
	ex := NewRuntimeExporter(nil, Identity{UID: 65534, GID: 65534})
	if _, err := ex.Resolve(context.Background(), "/nope/boot"); !errors.Is(err, ErrUnknownExport) {
		t.Fatalf("got %v, want ErrUnknownExport", err)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run TestHandshake -v`
Expected: FAIL — `undefined: ServeHandshake`, `undefined: NewRuntimeExporter`, `undefined: Export`.

- [ ] **Step 3: Implement `reply.go`**

```go
func writeOptionReply(w io.Writer, opt Option, typ OptionReplyType, data []byte) error
func writeSimpleReply(w io.Writer, cookie uint64, nbdErr NBDError) error
```

`writeOptionReply` writes `OptionReplyHeader{Magic: optionReplyMagic, Option: opt, Type: typ, Length: uint32(len(data))}` then `data`. `writeSimpleReply` writes `Reply{Magic: simpleReplyMagic, Error: nbdErr, Cookie: cookie}`.

`decision:` the error-reply helpers (`writeOptionError(w, opt, typ)`) take the error type as a parameter rather than each having its own function — six near-identical functions is six places to transpose a magic.

- [ ] **Step 4: Implement `handshake.go`**

`ServeHandshake` in order:

1. Write the greeting: `nbdMagic`, `ihaveoptMagic`, then a 16-bit handshake flags field with `NBD_FLAG_FIXED_NEWSTYLE` (bit 0) and `NBD_FLAG_NO_ZEROES` (bit 1) set. Writing the client-flags read *after* the greeting is not optional — the spec has the client reply with 32-bit client flags before any option.
2. Read 32-bit client flags. Reject `NBD_FLAG_C_NO_ZEROES` when we did not offer it (we always do, so this is unreachable — assert it rather than silently accepting).
3. Loop: read `OptionHeader`. On `Magic != ihaveoptMagic` → return `ErrBadMagic`. Switch on `Option`.
4. `OptGO` / `OptInfo`: validate the payload before touching it — `Length < 6` → `RepErrInvalid`; name length `> Length-6` → `RepErrInvalid`; `Length > maxOptionData` (define `const maxOptionData = 64 << 10`) → `RepErrTooBig`. **Do the length check before allocating.** Then resolve; unknown → `RepErrUnknown`. Send `NBD_INFO_EXPORT` (type 0, 12 bytes: `uint16(0)`, `uint64(size)`, `uint16(flags)`) and, when requested, `NBD_INFO_BLOCK_SIZE` (type 3, 14 bytes: `uint16(3)`, min `uint32(1)`, preferred `uint32(4096)`, max `uint32(32<<20)`). Then `RepAck`. For `OptGO` return the export; for `OptInfo` continue the loop.

   **Both block-size values are the spec's defaults, not tuning knobs.** Minimum 1 because the engine is backed by a journal and a file, not a block device — the spec says a minimum of 1 is allowed *"for an export backed by a regular file"* and that `2^9`/`2^12` are *"more typical for an export backed by a block device"*. Advertising 4096 would make the server refuse every unaligned request, and a guest filesystem's 512-byte metadata writes are unaligned by that standard. Preferred 4096 and max `2^25` are the spec's stated defaults. **Consequence:** because we advertise the default minimum, the alignment check in the handlers is a no-op and must not be written as a 4096-multiple test.
5. `OptExportName`: the payload *is* the name. Reply with `uint64(size)`, `uint16(flags)`, then 124 zero bytes unless the client negotiated `NBD_FLAG_C_NO_ZEROES`. No option reply header on this path. Unknown export → close the connection (the spec: "it MUST terminate the session"), which is why the test asserts a non-nil error from ABORT rather than from this path.
6. `OptList`: for each name send `RepServer` with `uint32(len(name))` + name; then `RepAck`.
7. `OptAbort`: `RepAck`, then return a non-nil error (`errSessionAborted`) so the caller closes the connection.
8. `OptStartTLS`, `OptStructuredReply`, `OptListMetaContext`, `OptSetMetaContext`: `RepErrUnsup`, continue the loop.
9. Anything else: `RepErrUnsup`.

**`NBD_INFO_EXPORT` must precede the ACK.** The spec: *"The server MUST send an NBD_INFO_EXPORT information type at some point before sending an NBD_REP_ACK."* And no zero padding after the ACK.

- [ ] **Step 5: Implement `volume.go`**

`RuntimeExporter.Resolve` parses `<share>/<volume>` from the export name, resolves the share's root handle via `rt.GetRootHandle(share)` (`pkg/controlplane/runtime/runtime.go:748`), then `Lookup` down `volumes` then `<volume>` using `rt.GetMetadataService()` (`:1102`) → `metadata.Service.Lookup(ctx, dirHandle, name)` (`pkg/metadata/file_modify.go:24`). It reads `File.Size` for the export size and `File.PayloadID` for the payload ID, and `rt.GetShare(share)` (`:729`) for `ReadOnly`.

**`Lookup` needs an `*AuthContext`, and the handshake has no client identity.** Build one here with the same synthetic unprivileged identity the wire layer uses, in the local-variable style `internal/adapter/smb/handlers/auth_helper.go:50` uses (no generic helper needed in production code — `uid, gid := id.UID, id.GID` then `&uid`, `&gid`):

```go
uid, gid := r.id.UID, r.id.GID
authCtx := &metadata.AuthContext{
	Context:  ctx,
	Identity: &metadata.Identity{UID: &uid, GID: &gid},
}
```

**Do not** set `ShareReadOnly` here. This is an internal resolution against a share the operator already configured, not a client operation; the per-user ceiling that `ShareReadOnly` applies (`pkg/metadata/auth_identity.go:15`) is enforced later, on every command, from `exp.ReadOnly` via `Handler.authContext`. Setting it in both places would make the handshake refuse an export whose data the client is entitled to read. So `RuntimeExporter` needs the configured `Identity` too: `NewRuntimeExporter(rt *runtime.Runtime, id Identity) *RuntimeExporter`. That is the reason the field exists rather than the exporter reading it off the handler.

**`Handle` is what makes the write path possible, and it must be the volume FILE's handle** — not the share root, not the `volumes` directory. `PrepareWrite`/`CommitWrite` take the file handle and derive the payload ID from it; the share root's handle would resolve to a different payload. Resolve it once here and hold it for the connection's life; that is the whole point of `Export.Handle`.

A missing share, a missing `volumes` directory, or a missing file all map to `ErrUnknownExport` — the export name is client-supplied and the server must not distinguish "no such share" from "no such volume" on the wire.

**A disabled share is refused too.** `Resolve` returns `ErrUnknownExport` for a share whose `Enabled` flag is false, and `List` skips such a share rather than advertising a volume that `Resolve` would then refuse. `Share.Enabled` is the DB-row `shares.enabled` flag (`pkg/controlplane/runtime/shares/service.go:26-29`: *"Disabled shares reject new MOUNT / TREE_CONNECT and in-flight operations"*), and `DisableShare` flips it synchronously before a restore begins so that no adapter admits a new client into the restore window. Every other adapter enforces it at establishment — `internal/adapter/nfs/mount/handlers/mount.go:114` (`MountErrAccess`), `internal/adapter/smb/handlers/tree_connect.go:119` (`STATUS_NETWORK_NAME_DELETED`), `internal/adapter/nfs/v4/handlers/helpers.go:226` (`NFS4ERR_STALE`) — and `ServeHandshake`/`Resolve` is the only such point NBD has. The write path that follows does not re-consult the flag, so a client admitted here would keep writing through the restore window. The refusal is reported as an unknown export for the same reason the missing-share case is: the server must not tell a client whether a share exists but is quiesced.

**The fixture must set `Enabled: true`.** `AddShare` copies the flag verbatim from `ShareConfig` (`pkg/controlplane/runtime/shares/lifecycle.go:548`) and nothing defaults it, so a share built without it is disabled and the exporter refuses to resolve it.

`decision:` resolution happens at handshake only, never on the hot path. The size is fixed at creation, so a re-resolve would only re-read a value that cannot change; holding a file handle across a restart is exactly what §2.2 of the spec avoids.

- [ ] **Step 6: Implement `volume_test.go`'s runtime case**

`TestRuntimeExporter_UnknownShareIsUnknownExport` needs no runtime. Add one case that does, using the existing fixture:

```go
func TestRuntimeExporter_ResolvesVolumeFile(t *testing.T) {
	rt, bsID := testruntime.NewWithShareStore(t)
	store := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("nbd-meta", store); err != nil {
		t.Fatal(err)
	}
	const share = "/export"
	// Enabled: true because AddShare copies the flag verbatim and nothing
	// defaults it; a share built without it is disabled, which the exporter now
	// refuses to resolve.
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name: share, MetadataStore: "nbd-meta", BlockStoreID: bsID,
		Enabled:           true,
		DefaultPermission: string(models.PermissionReadWrite),
		RootAttr:          &metadata.FileAttr{UID: 1000, GID: 1000},
	}); err != nil {
		t.Fatal(err)
	}
	root, err := rt.GetRootHandle(share)
	if err != nil {
		t.Fatal(err)
	}
	metaSvc := rt.GetMetadataService()
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: ptr(uint32(1000)), GID: ptr(uint32(1000))},
	}
	vols, _, err := metaSvc.CreateDirectory(authCtx, root, "volumes", &metadata.FileAttr{Mode: 0o755})
	if err != nil {
		t.Fatal(err)
	}
	volsHandle, err := metadata.EncodeFileHandle(vols)
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := metaSvc.CreateFile(authCtx, volsHandle, "boot", &metadata.FileAttr{Mode: 0o644})
	if err != nil {
		t.Fatal(err)
	}
	createdHandle, err := metadata.EncodeFileHandle(created)
	if err != nil {
		t.Fatal(err)
	}
	// Creation zeroes the size (ApplyCreateDefaults), so the export size is set
	// explicitly afterwards — this is the "size is fixed at creation" step.
	if _, err := metaSvc.SetFileAttributes(authCtx, createdHandle, &metadata.SetAttrs{Size: ptr(uint64(1 << 30))}); err != nil {
		t.Fatal(err)
	}

	ex := NewRuntimeExporter(rt, Identity{UID: 65534, GID: 65534})
	got, err := ex.Resolve(context.Background(), "/export/boot")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Size != 1<<30 {
		t.Fatalf("size = %d, want %d", got.Size, 1<<30)
	}
	if got.PayloadID == "" {
		t.Fatal("resolved export has an empty payload ID")
	}
	if got.ReadOnly {
		t.Fatal("share is read-write but the export reports read-only")
	}
	if got.Handle == nil {
		t.Fatal("resolved export has no file handle; the write path cannot call PrepareWrite without one")
	}
	// The handle must be the volume file's own, not the share root's and not
	// the volumes directory's: PrepareWrite/CommitWrite derive the payload ID
	// from it, so either of the other two would address different bytes.
	if !bytes.Equal(got.Handle, createdHandle) {
		t.Fatalf("export handle = %q, want the volume file's handle %q", got.Handle, createdHandle)
	}
}

// TestRuntimeExporter_DisabledShareIsRefusedAndUnlisted pins both halves of the
// disabled-share gate: the handshake is the only point at which NBD admits a
// client, so a share being quiesced must be neither resolvable nor advertised.
func TestRuntimeExporter_DisabledShareIsRefusedAndUnlisted(t *testing.T) {
	rt, bsID := testruntime.NewWithShareStore(t)
	store := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("nbd-meta", store); err != nil {
		t.Fatal(err)
	}
	const share = "/export"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name: share, MetadataStore: "nbd-meta", BlockStoreID: bsID,
		Enabled:           true,
		DefaultPermission: string(models.PermissionReadWrite),
		RootAttr:          &metadata.FileAttr{UID: 1000, GID: 1000},
	}); err != nil {
		t.Fatal(err)
	}
	root, err := rt.GetRootHandle(share)
	if err != nil {
		t.Fatal(err)
	}
	metaSvc := rt.GetMetadataService()
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: ptr(uint32(1000)), GID: ptr(uint32(1000))},
	}
	vols, _, err := metaSvc.CreateDirectory(authCtx, root, "volumes", &metadata.FileAttr{Mode: 0o755})
	if err != nil {
		t.Fatal(err)
	}
	volsHandle, err := metadata.EncodeFileHandle(vols)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := metaSvc.CreateFile(authCtx, volsHandle, "boot", &metadata.FileAttr{Mode: 0o644}); err != nil {
		t.Fatal(err)
	}

	ex := NewRuntimeExporter(rt, Identity{UID: 65534, GID: 65534})
	if _, err := ex.Resolve(context.Background(), "/export/boot"); err != nil {
		t.Fatalf("Resolve on an enabled share: %v", err)
	}
	if names, err := ex.List(context.Background()); err != nil {
		t.Fatalf("List on an enabled share: %v", err)
	} else if len(names) != 1 || names[0] != "/export/boot" {
		t.Fatalf("List on an enabled share = %v, want [/export/boot]", names)
	}

	if err := rt.SetEnabledForTesting(share, false); err != nil {
		t.Fatal(err)
	}

	if _, err := ex.Resolve(context.Background(), "/export/boot"); !errors.Is(err, ErrUnknownExport) {
		t.Fatalf("Resolve on a disabled share: got %v, want ErrUnknownExport", err)
	}
	if names, err := ex.List(context.Background()); err != nil {
		t.Fatalf("List on a disabled share: %v", err)
	} else if len(names) != 0 {
		t.Fatalf("List on a disabled share = %v, want no exports", names)
	}
}
```

Add one test helper in this file: `func ptr[T any](v T) *T { return &v }`. This file's imports must also include `"bytes"` (the `Handle` comparison) and `"errors"` (the `errors.Is` check on the disabled-share refusal).

Convert a `*metadata.File` to a handle with **`metadata.EncodeFileHandle(file)`** (`pkg/metadata/types.go:53`) — the same call `internal/adapter/nfs/v4/handlers/create.go:344` and `lookup.go:105` make, and the reason those adapters never need a handle-minting helper.

**Do not use `store.GenerateHandle` for this.** On Badger it ignores its `path` argument entirely: `pkg/metadata/store/badger/shares.go:21` → `basestore.GenerateHandle(ctx, shareName)` (`pkg/metadata/store/basestore/handle.go:15`) → `metadata.GenerateNewHandle` (`pkg/metadata/types.go:100`), which mints a fresh random UUID. The handle it returns would not name the file you just created, so `SetFileAttributes` would fail with an invalid-handle error and the test would be measuring the wrong thing. `EncodeFileHandle` encodes the file's own ID, which is what `GetFile` resolves against.

Do not hand-build a handle either (`[]byte(share + ":" + someUUID)`): the runtime routes by decoding it, so a fabricated handle would make the test pass and production fail.

Note: `metadata.Service.SetFileAttributes` is at `pkg/metadata/file_modify.go:339` and its size branch requires a regular file and write permission (`:720`, `:740`).

- [ ] **Step 7: Run the tests and confirm they pass**

Run: `go test ./internal/adapter/nbd/ -v`
Expected: PASS, all of Task 1 and Task 2.

- [ ] **Step 8: Verify the malformed-length tests actually bite**

In `handshake.go`, temporarily replace the `Length < 6` / name-length checks with a direct `make([]byte, nameLen)`. Run `go test ./internal/adapter/nbd/ -run TestHandshake_GoNameLengthExceedsOptionData -v` and confirm it fails — panic or `RepErrInvalid` mismatch. Restore.

- [ ] **Step 9: Commit**

```bash
git add internal/adapter/nbd/
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "feat(nbd): handshake, option haggling, and export resolution"
```

---

## Task 3: Adapter lifecycle, registration, and the bind gate

**Files:**
- Create: `pkg/adapter/nbd/config.go`, `pkg/adapter/nbd/adapter.go`, `pkg/adapter/nbd/healthcheck.go`
- Modify: `pkg/controlplane/models/adapter.go` (`DefaultPort` at `:34`, `Validate` at `:50`), `cmd/dfs/commands/start.go` (`createAdapterFactory` at `:900`)
- Test: `pkg/adapter/nbd/adapter_test.go`, `pkg/adapter/nbd/healthcheck_test.go`

**Interfaces:**
- Consumes: Task 2's `ServeHandshake`, `RuntimeExporter`, `Exporter`, `Identity`. **Nothing from Task 4.** An earlier draft had this task's `Serve` build the `nbd.Handler`, which made this task's commit uncompilable until Task 4 landed; the wiring now lives in Task 4's Step 6, where every symbol it names already exists. This task binds and delegates.
- Produces:
  - `type TimeoutsConfig struct { Read, Write, Idle, Shutdown time.Duration }` — the operator-facing four-timer config. The wire layer's three-timer `nbd.Timeouts` and the `timeouts()` narrowing accessor belong to Task 4, which is where they are first needed; this task only stores the config.
  - `type Config struct { Enabled bool; Port int; BindAddress string; MaxConnections int; MaxMessageSize int; Timeouts TimeoutsConfig; AllowRemoteBind bool; DefaultExport string; UID uint32; GID uint32 }`
  - `const DefaultPort = 10809`
  - `func (c *Config) applyDefaults()`, `func (c *Config) validate() error`
  - `type Adapter struct { *adapter.BaseAdapter; config Config; runtime *runtime.Runtime }` — no `handler` field in this task; Task 4's Step 6 adds it together with the wiring. Declaring it here is what made an earlier draft uncompilable on its own.
  - `func New(config Config) *Adapter`
  - `func (a *Adapter) Serve(ctx context.Context) error`
  - `func (a *Adapter) Stop(ctx context.Context) error`
  - `func (a *Adapter) SetRuntime(rt any)`
  - `func (a *Adapter) NewConnection(conn net.Conn) adapter.ConnectionHandler`
  - `func (a *Adapter) Healthcheck(ctx context.Context) health.Report`

- [ ] **Step 1: Write the failing config and lifecycle tests**

Create `pkg/adapter/nbd/adapter_test.go`. **It is `package nbd_test`** (external), matching `pkg/adapter/nfs/healthcheck_test.go` and `pkg/adapter/smb/healthcheck_test.go`, both of which are `_test` packages. That choice is load-bearing here: the tests need `internal/adapter/testruntime`, whose package doc records that *"Every consumer is an in-package test, so importing the package under test would close an import cycle"* — an external test package breaks that constraint. Verified for this package: `go list -deps ./internal/adapter/testruntime` contains no `dittofs/pkg/adapter` entry, so `pkg/adapter/nbd` (imported by `package nbd_test`) creates no cycle. Do not "simplify" this to `package nbd` to reach an unexported symbol; add an exported accessor instead.

```go
package nbd_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/testruntime"
	"github.com/marmos91/dittofs/pkg/adapter/nbd"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func minimalConfig() nbd.Config {
	return nbd.Config{Enabled: true, Port: 11089, BindAddress: "127.0.0.1"}
}

func TestNew_AppliesDefaults(t *testing.T) {
	a := nbd.New(nbd.Config{})
	if a.Port() != nbd.DefaultPort {
		t.Fatalf("Port() = %d, want %d", a.Port(), nbd.DefaultPort)
	}
	if a.Protocol() != "nbd" {
		t.Fatalf("Protocol() = %q, want %q", a.Protocol(), "nbd")
	}
}

func TestNew_PanicsOnOutOfRangePort(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with port 70000 did not panic; the adapter constructor contract is to panic on programmer error")
		}
	}()
	nbd.New(nbd.Config{Enabled: true, Port: 70000})
}

// The bind gate. An NBD export is unauthenticated by construction: there is no
// AUTH_SYS, no Kerberos, and a single READ hands over every byte with no path
// scoping. A non-loopback bind must therefore be an explicit opt-in.
//
// The empty string is in the list deliberately: IsNonLoopbackHost("") is true
// (net.ParseIP("") is nil and the final branch assumes a hostname resolves
// off-host), which is the behaviour the gate wants — an empty BindAddress binds
// every interface. Do not add a special case for it.
func TestServe_RefusesNonLoopbackBindWithoutOptIn(t *testing.T) {
	for _, bind := range []string{"", "0.0.0.0", "::", "192.168.1.10"} {
		t.Run(bind, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.BindAddress = bind
			a := nbd.New(cfg)
			err := a.Serve(context.Background())
			if err == nil {
				t.Fatal("Serve on a non-loopback address without AllowRemoteBind returned nil")
			}
			if !strings.Contains(err.Error(), "allow_remote_bind") {
				t.Fatalf("error %q does not name allow_remote_bind, so an operator cannot find the setting", err)
			}
		})
	}
}

// The gate runs before the listener is created and before the runtime is
// consulted, so this test needs neither a free port nor a fixture.
func TestServe_RefusesRemoteBindEvenWithNoRuntime(t *testing.T) {
	cfg := minimalConfig()
	cfg.BindAddress = "0.0.0.0"
	a := nbd.New(cfg)
	if err := a.Serve(context.Background()); err == nil {
		t.Fatal("the bind gate must refuse before Serve touches the runtime")
	} else if strings.Contains(err.Error(), "SetRuntime") {
		t.Fatalf("the runtime check ran before the bind gate: %v", err)
	}
}

// freePort asks the kernel for an unused port and releases it, so a listener
// test binds somewhere real instead of colliding on DefaultPort. Port 0 cannot
// be used for this: applyDefaults rewrites it to DefaultPort before the
// listener is created (config.go:102).
//
// No test in this package runs in parallel, so nothing in this package competes
// for the port. The window is real but narrow: the deferred Close releases it
// before this returns, and a foreign listener would have to be handed the same
// ephemeral port in between.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// Serve needs a runtime once it gets past the gate, so the listener tests use
// the real fixture. This also exercises SetRuntime, which nothing else does.
//
// The returned done channel carries Serve's result for the test body to read.
// Cleanup waits on a separate served channel rather than on done, so a test
// that already consumed the result does not leave cleanup blocking for its
// full timeout and then reporting a second, bogus error for the same cause.
func serveWithRuntime(t *testing.T, cfg nbd.Config) (*nbd.Adapter, chan error) {
	t.Helper()
	rt, _ := testruntime.NewWithShareStore(t)
	a := nbd.New(cfg)
	a.SetRuntime(rt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	served := make(chan struct{})
	go func() {
		done <- a.Serve(ctx)
		close(served)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after context cancellation")
		}
	})
	return a, done
}

func TestServe_AllowsLoopbackBind(t *testing.T) {
	// "::1" is deliberately absent and "[::1]" stands in for it. BaseAdapter
	// builds its listen address as fmt.Sprintf("%s:%d", BindAddress, Port)
	// (pkg/adapter/base.go:216), so a BARE IPv6 literal yields "::1:0" and
	// net.Listen rejects it with "too many colons in address"; the bracketed
	// form is what binds. That is a property of the shared base adapter, not
	// something this task changes, so the test does not assert a capability the
	// code lacks. Both spellings pass netutil.IsNonLoopbackHost as loopback, so
	// the gate admits either — only the listen address differs.
	for _, bind := range []string{"127.0.0.1", "[::1]", "localhost"} {
		t.Run(bind, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.BindAddress = bind
			cfg.Port = freePort(t)
			a, done := serveWithRuntime(t, cfg)
			select {
			case <-a.ListenerReady():
			case err := <-done:
				t.Fatalf("Serve returned before the listener was ready: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("listener was never ready")
			}
			// GetListenerAddr, not Port. Port() returns the CONFIGURED port
			// (pkg/adapter/base.go:598 is "return b.Config.Port"), and the
			// configured port is what the test asked for, so it says nothing about
			// whether a socket came up. GetListenerAddr reports the socket's real
			// address, and it blocks on listenerReady too.
			if a.GetListenerAddr() == "" {
				t.Fatal("GetListenerAddr is empty after ListenerReady; the adapter did not bind a listener")
			}
		})
	}
}

func TestServe_AllowsNonLoopbackWithOptIn(t *testing.T) {
	cfg := minimalConfig()
	cfg.BindAddress = "0.0.0.0"
	cfg.AllowRemoteBind = true
	cfg.Port = freePort(t)
	a, done := serveWithRuntime(t, cfg)
	select {
	case <-a.ListenerReady():
	case err := <-done:
		t.Fatalf("Serve with AllowRemoteBind returned early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("listener was never ready")
	}
}

func TestStop_IsIdempotent(t *testing.T) {
	a := nbd.New(minimalConfig())
	ctx := context.Background()
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// The adapter must not treat a per-connection failure as fatal: Serve returning
// before cancellation makes DittoServer stop every other adapter.
//
// The runtime comes from serveWithRuntime rather than being wired inline:
// Serve refuses to bind without one, so a listener test cannot skip it. Using
// the shared helper keeps every listener test on one construction path.
func TestServe_ClientDisconnectDoesNotStopTheAdapter(t *testing.T) {
	cfg := minimalConfig()
	cfg.Port = freePort(t)
	a, done := serveWithRuntime(t, cfg)
	select {
	case <-a.ListenerReady():
	case <-time.After(5 * time.Second):
		t.Fatal("listener was never ready")
	}

	// Dial the socket's real address rather than the configured port: the
	// configured port is a hint the test handed to the adapter, and the address
	// the socket actually got is what a client has to dial.
	addr := a.GetListenerAddr()
	for range 3 {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		// Read the greeting, then hang up mid-handshake.
		buf := make([]byte, 18)
		_, _ = io.ReadFull(conn, buf)
		_ = conn.Close()
	}

	select {
	case err := <-done:
		t.Fatalf("Serve returned after client disconnects: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestControlPlane_DefaultPortKnowsNBD(t *testing.T) {
	if got := models.DefaultPort("nbd"); got != nbd.DefaultPort {
		t.Fatalf("models.DefaultPort(\"nbd\") = %d, want %d", got, nbd.DefaultPort)
	}
	if err := (&models.AdapterConfig{Type: "nbd", Port: 10809}).Validate(); err != nil {
		t.Fatalf("Validate rejected an nbd adapter: %v", err)
	}
}
```

Add the missing import (`io`) at the top.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./pkg/adapter/nbd/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement `config.go`**

Mirror `pkg/adapter/smb/config.go:63` and its `applyDefaults` at `:311`. Defaults: `Port` 10809; `Timeouts.Read` 5m, `Write` 30s, `Idle` 5m, `Shutdown` 30s; `MaxMessageSize` 32 MiB (`32 << 20` — the maximum block size we advertise); `UID`/`GID` `65534` (nobody/nogroup, the unprivileged default the SMB synthetic-identity path uses, `internal/adapter/smb/handlers/auth_helper.go:74-95`).

`validate()` returns an error when `Port > 65535`. `New` panics on a `validate()` error, matching `pkg/adapter/smb/adapter.go:176`.

- [ ] **Step 4: Implement `adapter.go`**

`New` calls `applyDefaults()`, `validate()` (panic on error), then `adapter.NewBaseAdapter(adapter.BaseConfig{BindAddress, Port, MaxConnections, ShutdownTimeout: Timeouts.Shutdown}, "nbd")`.

`SetRuntime(rt any)` type-asserts to `*runtime.Runtime` and stores it. It must **not** build the exporter or the handler: `nbd.NewHandler` and `nbd.Identity` are declared by Task 4, so referencing them here would leave this task's own commit uncompilable. Task 4's Step 6 adds the wiring. This task's job is the lifecycle, the bind gate, and the control-plane registration.

`Serve(ctx)`:
1. **The bind gate, before anything binds.** `if netutil.IsNonLoopbackHost(a.config.BindAddress) && !a.config.AllowRemoteBind { return fmt.Errorf(...) }`. The error text must contain `allow_remote_bind`. `netutil.IsNonLoopbackHost` (`pkg/netutil/netutil.go:15`) returns false for `127.0.0.1`, `::1`, `[::1]`, `localhost`, and **true for an empty string** (it falls through the switch, `net.ParseIP("")` is nil, and the non-IP branch assumes off-host). That is the behaviour the gate wants — an empty `BindAddress` binds all interfaces, so it must be refused without the opt-in — and it needs no special case. Say so in a comment, because the next reader will not check the fall-through. The gate runs **before** the runtime is consulted, which is what `TestServe_RefusesRemoteBindEvenWithNoRuntime` pins; Task 4's Step 6 preserves that order when it fills in the body.
2. `if a.runtime == nil { return errors.New("nbd: SetRuntime was not called before Serve") }`.
3. `return a.ServeWithFactory(ctx, a, nil, nil)`.

`NewConnection(conn net.Conn) adapter.ConnectionHandler` returns a minimal per-connection type implementing `Serve(ctx context.Context)` that reads the greeting and closes, so the lifecycle tests pass. It exists because `adapter.ConnectionHandler.Serve` takes only a context (`pkg/adapter/base.go:38`), so the socket has to be bound at construction. **Task 4's Step 6 replaces this stub** with `return nbd.NewConn(a.handler.Load(), conn)`, the same shape as `pkg/adapter/smb/adapter.go:717`. (Task 4 lists `pkg/adapter/nbd/adapter.go` under Modify for exactly this reason.)

`Stop` delegates to `a.BaseAdapter.Stop(ctx)`.

- [ ] **Step 5: Implement `healthcheck.go`**

```go
func (a *Adapter) Healthcheck(ctx context.Context) health.Report
```

`!a.config.Enabled` → `StatusDisabled` with message `"NBD adapter is disabled in config"`. Otherwise delegate to `a.BaseAdapter.Healthcheck(ctx)`. `StatusDegraded` is added in Task 4, when there is a resolvable-export failure to report — until then there is nothing that can degrade.

Note `BaseAdapter.Healthcheck` already reports `StatusUnhealthy` once shutdown is initiated but `Serve` has not returned (`pkg/adapter/healthcheck.go:39`), so the drain path needs no help here.

- [ ] **Step 6: Wire the control plane**

In `pkg/controlplane/models/adapter.go`:
- Add `DefaultNBDPort = 10809` beside `DefaultNFSPort`/`DefaultSMBPort` at `:28`.
- Add `case "nbd": return DefaultNBDPort` to `DefaultPort` at `:34`.
- Change the `Validate` error text at `:55` to `"unsupported adapter type %q: must be one of nfs, smb, nbd"`.
- Update the `Type` field comment at `:9` from `// nfs, smb` to `// nfs, smb, nbd`.

In `cmd/dfs/commands/start.go`, add to `createAdapterFactory`'s switch at `:902`:

```go
case "nbd":
    return createNBDAdapter(cfg)
```

and add `createNBDAdapter`, modelled on `createSMBAdapter` at `:950`: resolve `port := cfg.Port; if port == 0 { port = models.DefaultNBDPort }`, build `nbdCfg := nbd.Config{Enabled: true, Port: port}`, call `cfg.GetConfig()` (error `"failed to parse adapter config: %w"`), then read `bind_address`, `allow_remote_bind`, `default_export`, `max_connections` out of the `map[string]any`.

- [ ] **Step 7: Run the tests and confirm they pass**

Run: `go test ./pkg/adapter/nbd/ ./pkg/controlplane/models/ ./cmd/... -v`
Expected: PASS.

- [ ] **Step 8: Check whether the CLI docs need regenerating**

The generator is `go run ./cmd/gendocs` (`CLAUDE.md:96`: *"`docs/guide/cli.md` is generated from the Cobra command trees by `cmd/gendocs`. Never hand-edit it"*). An earlier draft of this step named a `docs` subcommand on the `dfs` binary; the CLI has none.

**This task changes no CLI surface, so expect no diff.** Its changes to `cmd/dfs/commands/start.go` add `createNBDAdapter` and a `case "nbd":` arm — adapter construction, not a command or flag. Confirm that before concluding anything:

```bash
git diff cmd/dfs/commands/start.go | grep -E '^\+.*(cobra|Flags\(\)|Command\{|Use:)'
```

No output means no command definition or flag changed, so `docs/guide/cli.md` is legitimately unchanged and **you must not stage it or manufacture a diff**. If that grep *does* print something, run `go run ./cmd/gendocs` and stage the result.

- [ ] **Step 9: Run the full unit suite and build**

Run: `go build ./... && make test-unit`

`make test-unit` is `go test -race ./...` and takes ~16 minutes on this machine. Expect five `panic: test timed out after 10m0s` failures — `internal/adapter/nfs/v3/handlers`, `internal/adapter/smb/handlers`, `pkg/block/chunker`, `pkg/block/engine`, `pkg/block/middleware/encryption` — which are **environmental, not regressions**: concurrent per-package SQLite `AutoMigrate` (`pkg/controlplane/store/gorm.go:433`) and `blake3.CompressEigentree` under `-race` starve each other past the deadline. Verified by running the same packages on the base commit with `-p 1`: `pkg/block/engine` still times out unmodified, and `pkg/block/middleware/encryption` passes serially in 198s. None of the five imports anything this task changed.

The honest evidence for this task is the scoped run: `go test -race ./pkg/adapter/nbd/... ./pkg/controlplane/models/... ./cmd/...`. Do not run the full `./...` twice to re-confirm an environmental result. Note there is no `timeout(1)` on this machine — use `go test -timeout 240s` for a bound.

`go build ./...` must be clean, and the lifecycle tests must be race-clean in the scoped run.

- [ ] **Step 10: Commit**

```bash
git add pkg/adapter/nbd/ pkg/controlplane/models/adapter.go cmd/dfs/commands/start.go
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "feat(nbd): adapter lifecycle, control-plane registration, bind gate"
```

---

## Task 4: Connection loop and READ

**Files:**
- Create: `internal/adapter/nbd/connection.go`, `internal/adapter/nbd/handlers_read.go`
- Modify: `internal/adapter/common/write_payload.go` (`CommitBlockStore` parameter widened to the new `Committer` interface — a type-only change, no behaviour change; every existing call site passes `*engine.Store` and keeps compiling), `internal/adapter/common/normalize.go` (`normalizeBlockStoreError` exported as `NormalizeBlockStoreError`), `internal/adapter/common/{doc.go,read_payload.go,write_payload.go}` (the rename's four call sites: `read_payload.go:60`, `write_payload.go:68`, `:122`, `:171`), `pkg/adapter/nbd/adapter.go` (Step 6 replaces Task 3's `Serve` body and `NewConnection` stub with the real wiring), `pkg/adapter/nbd/config.go` (Step 6 adds `Config.timeouts()`), `pkg/adapter/nbd/healthcheck.go` (Step 7 adds `Degraded`)

**Task 3 must land first**, because both tasks modify `pkg/adapter/nbd/adapter.go`. Task 3 writes the lifecycle, the bind gate and the stubs; this task's Step 6 replaces the two stub bodies. They touch different functions, so the second edit is a substitution rather than a conflict.
- Test: `internal/adapter/nbd/connection_test.go`, `internal/adapter/nbd/integration_test.go`, `internal/adapter/common/write_payload_test.go` (only if one does not already cover `CommitBlockStore`)

**Interfaces:**
- Consumes: Task 2's `Export`/`Exporter`/`Identity`, Task 3's `Config` (timeouts), `Adapter`.
- Produces:
  - `type Timeouts struct { Read, Write, Idle time.Duration }` — declared here, in `connection.go`. **`Idle` bounds the wait for the next request header; `Read` bounds finishing a request already started** (the payload read Step 7 of Task 5 adds). The split is what the siblings already do — NFS arms `Idle` on the connection loop (`pkg/adapter/nfs/connection.go:312`) and `Read` on the fragment read (`:338`), SMB likewise (`pkg/adapter/smb/connection.go:230`, `:258`) — and it makes both doc comments true. Note `Idle == 0` means *no idle timeout* (`config.go:42`), so it is a sentinel and **cannot** be folded into a `min(Read, Idle)`; it is an independent arm guarded by `> 0`, exactly as the siblings guard it. Task 3 stores the operator-facing `TimeoutsConfig` (four fields); Step 6 below adds `Config.timeouts()` to narrow four to these three, dropping `Shutdown` because the base adapter owns the drain deadline.
  - `type StoreResolver func(ctx context.Context, exp *Export) (BlockStore, error)` — a function type rather than a method on `Handler`, so the dispatch tests can inject a fake store. The production implementation wraps `common.ResolveForWrite` (below), so `internal/adapter/nbd` never imports `pkg/controlplane/runtime`.
  - `type BlockStore interface { ReadAt(...); WriteAt(...); PunchHole(...); Flush(...); LocalDurable() bool; RemoteDurable() bool; RequireDurableCommit() bool }` — the four I/O methods plus the three durability accessors, so `commit` can call `common.CommitBlockStore` without the concrete `*engine.Store`. `*engine.Store` satisfies all seven (`pkg/block/engine/readwrite.go:21`, `:87`, `:286`; `pkg/block/engine/flush.go:534`; `pkg/block/engine/engine.go:388`, `:397`, `:406`).
  - `type Handler struct { exporter Exporter; stores StoreResolver; meta *metadata.Service; id Identity; timeouts Timeouts; draining func() bool; degraded atomic.Int64 }`
  - `func NewHandler(exporter Exporter, stores StoreResolver, meta *metadata.Service, id Identity, timeouts Timeouts, draining func() bool) *Handler` — `draining` is a closure over `a.BaseAdapter.Shutdown` from the adapter in Task 3; passing it as a function rather than the base adapter keeps `internal/adapter/nbd` free of any `pkg/adapter` import.
  - `func (h *Handler) serve(ctx context.Context, conn net.Conn) error` — unexported, because it is the test entry point, not the framework's. `type Conn struct { handler *Handler; conn net.Conn }` plus `func NewConn(h *Handler, conn net.Conn) *Conn` and `func (c *Conn) Serve(ctx context.Context)` satisfy `adapter.ConnectionHandler` (`pkg/adapter/base.go:38`, `Serve(ctx)` with no `net.Conn`); the pattern matches `pkg/adapter/smb/connection.go:64`. `Conn.Serve` **must recover panics** — the base adapter's accept loop does not, so an unrecovered panic kills the process rather than the connection (see the code block below).
  - `func (h *Handler) authContext(ctx context.Context, exp *Export) *metadata.AuthContext` — builds the synthetic-identity `AuthContext` from `h.id`, and sets `ShareReadOnly: exp.ReadOnly`. The body is fully determined by those two facts plus the SMB precedent at `internal/adapter/smb/handlers/auth_helper.go:50`:

```go
func (h *Handler) authContext(ctx context.Context, exp *Export) *metadata.AuthContext {
	uid, gid := h.id.UID, h.id.GID
	return &metadata.AuthContext{
		Context:       ctx,
		Identity:      &metadata.Identity{UID: &uid, GID: &gid},
		ShareReadOnly: exp.ReadOnly,
	}
}
```

Built **per command**, never cached: it embeds the command's `Context`, so a cached one would carry the first command's deadline and cancellation into every later command on the connection. `ShareReadOnly` is set **here and not in `RuntimeExporter`** — this is the client-facing ceiling, and the handshake's internal resolution deliberately does not apply it.
  - `type Committer interface { Flush(ctx context.Context, payloadID string) (*block.FlushResult, error); LocalDurable() bool; RemoteDurable() bool; RequireDurableCommit() bool }` in `internal/adapter/common/write_payload.go`, and `CommitBlockStore(ctx context.Context, c Committer, payloadID metadata.PayloadID) error` — the existing function with its parameter widened. `*engine.Store` satisfies it, so the NFS and SMB call sites are untouched.
  - `func mapError(err error) NBDError` (in `connection.go`)

`Handler` deliberately holds no `*runtime.Runtime`: `RuntimeExporter` (Task 2) owns that, and keeping the runtime out of the wire layer is what makes the dispatch tests possible without a control plane.

`Handler` also holds no `net.Conn`, and that is forced by the framework rather than chosen: `adapter.ConnectionHandler` is `interface{ Serve(ctx context.Context) }` (`pkg/adapter/base.go:38`) — no connection parameter — so the `net.Conn` has to arrive some other way. The precedent is `pkg/adapter/smb/connection.go:64 func NewConnection(server *Adapter, conn net.Conn) *Connection`, with `Adapter.NewConnection` at `pkg/adapter/smb/adapter.go:717` returning it. NBD does the same, with a name that does not collide with the framework method:

```go
// Conn is one accepted TCP connection. It exists because
// adapter.ConnectionHandler.Serve takes no net.Conn (pkg/adapter/base.go:38),
// so the socket has to be bound at construction. It holds no protocol state of
// its own: the handshake result and the resolved store live on the stack of
// serve, which is the only thing that can outlive them safely.
type Conn struct {
	handler *Handler
	conn    net.Conn
}

func NewConn(h *Handler, conn net.Conn) *Conn { return &Conn{handler: h, conn: conn} }

// Serve runs one connection and recovers panics.
//
// The recover is not defensive habit: the base adapter's accept loop has none
// (grep recover pkg/adapter/base.go returns nothing) and it invokes this on a
// bare goroutine (pkg/adapter/base.go:373), so an unrecovered panic here takes
// down the whole DittoFS process — every NFS and SMB connection with it. Both
// sibling adapters recover for the same reason and in the same place: NFS at
// pkg/adapter/nfs/connection.go:420 (handleConnectionClose, deferred at :111)
// and SMB at pkg/adapter/smb/connection.go:474. Log at Error, not Debug: a
// panic is unexpected by definition (CLAUDE.md invariant 6), and it is the
// only trace of why the connection died.
func (c *Conn) Serve(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Panic in NBD connection handler",
				"remote", c.conn.RemoteAddr(), "error", r, "stack", string(debug.Stack()))
		}
	}()
	if err := c.handler.serve(ctx, c.conn); err != nil && ctx.Err() == nil {
		logger.Debug("NBD connection ended", "remote", c.conn.RemoteAddr(), "error", err)
	}
}
```

`Adapter.NewConnection` (`pkg/adapter/nbd/adapter.go`) becomes `return nbd.NewConn(a.handler.Load(), conn)`. The unexported `func (h *Handler) serve(ctx context.Context, conn net.Conn) error` is what the dispatch tests call directly, so they keep the `error` return and never need the wrapper.

**The `handler` field is an `atomic.Pointer[nbd.Handler]`, so `pkg/adapter/nbd/adapter.go` needs `sync/atomic` in its imports.** The pointer is not a stylistic choice: `Serve` stores it on the accept-loop goroutine (`pkg/controlplane/runtime/adapters/service.go:611` starts `adp.Serve` inside `go func()`) while `Healthcheck` loads it from the API handler's goroutine (`internal/controlplane/api/handlers/adapters.go:240` → `Runtime.AdapterChecker` → `adp.Healthcheck`). A plain field is a data race the detector reports on any probe that lands during startup.

**`internal/adapter/nbd/connection.go` needs two imports for that recover block, and neither is obvious:** `github.com/marmos91/dittofs/internal/logger` (a *package*, called as `logger.Error(...)` — the same import `internal/adapter/nfs/v4/handlers/write.go:16` and `internal/adapter/smb/handlers/auth_helper.go:9` use; there is no package-level `logger` variable in `internal/adapter/nbd`) and `runtime/debug` for `debug.Stack()`, as `pkg/adapter/nfs/connection.go:10` does. Without the second, the panic log says what happened but not where, which is the half that matters for a bug that only fires on a client's malformed request.

- [ ] **Step 1: Write the failing connection tests**

Create `internal/adapter/nbd/connection_test.go`. Build a handler over a fake store, then drive it with the Task 1 encoder.

```go
// storeFake is the minimum surface a handler needs. It is deliberately not an
// engine.Store: these tests are about dispatch and error mapping, and a real
// engine would make every failure mode a store failure mode.
type storeFake struct {
	data     map[string][]byte
	readErr  error
	writeErr error
	flushErr error
	reads    int
	writes   int
	flushes  int
}

func (s *storeFake) ReadAt(_ context.Context, id string, buf []byte, off uint64) (int, error) {
	s.reads++
	if s.readErr != nil {
		return 0, s.readErr
	}
	b := s.data[id]
	if off >= uint64(len(b)) {
		for i := range buf {
			buf[i] = 0
		}
		return len(buf), nil
	}
	n := copy(buf, b[off:])
	for i := n; i < len(buf); i++ {
		buf[i] = 0
	}
	return len(buf), nil
}
```

The handler must take an interface, not `*engine.Store`, so this fake is possible — and it must receive the resolver rather than look the store up itself. Define in `connection.go`:

```go
// BlockStore is the engine surface the wire handlers use. An interface rather
// than *engine.Store so the dispatch tests can drive failure modes the engine
// cannot be made to produce on demand. The durability accessors are here
// because the commit rule (common.CommitBlockStore) reads them, and that rule
// must be the same one NFS COMMIT and SMB Flush use.
type BlockStore interface {
	ReadAt(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error)
	WriteAt(ctx context.Context, payloadID string, currentBlocks []block.ChunkRef, data []byte, offset uint64) ([]block.ChunkRef, error)
	PunchHole(ctx context.Context, payloadID string, currentBlocks []block.ChunkRef, offset, length uint64) ([]block.ChunkRef, error)
	Flush(ctx context.Context, payloadID string) (*block.FlushResult, error)
	LocalDurable() bool
	RemoteDurable() bool
	RequireDurableCommit() bool
}
```

`*engine.Store` satisfies all seven. The last three exist only so `commit` can pass the store to `common.CommitBlockStore`, whose parameter this task widens to `common.Committer`. **Do that widening first**, then confirm nothing else broke:

```bash
go build ./... && go test ./internal/adapter/common/ ./internal/adapter/nfs/... ./internal/adapter/smb/...
```

Expected: clean. A type-only widening that breaks a call site means the call site was passing something that is not `*engine.Store`, which is worth knowing before writing new code against it.

`storeFake` must also implement the three durability accessors — returning `true, true, false` for the happy path, so `commit` takes the `localDurable` fast path. The tests that need `ErrNotDurableYet` set them to `false, false, true`. It also needs a `panicOnRead bool` field, so `TestConn_PanicInHandlerDoesNotKillTheProcess` can drive a panic from below the dispatcher; when set, `ReadAt` panics instead of returning. `Handler` takes a `StoreResolver`, and the production resolver is built over **`common.ResolveForWrite`** — do not write a new resolution path.

`internal/adapter/common/resolve.go:15` already declares the seam:

```go
// BlockStoreRegistry is the runtime surface the resolvers need. An interface
// rather than *runtime.Runtime so this package does not import the control
// plane (see the package doc's cycle note).
type BlockStoreRegistry interface {
	GetBlockStoreForHandle(ctx context.Context, handle metadata.FileHandle) (*engine.Store, error)
}

func ResolveForWrite(ctx context.Context, reg BlockStoreRegistry, handle metadata.FileHandle) (*engine.Store, error)
```

So the adapter's resolver is `common.ResolveForWrite(ctx, rt, exp.Handle)` wrapped to return the `BlockStore` interface. `*runtime.Runtime` satisfies `BlockStoreRegistry` (`pkg/controlplane/runtime/runtime.go:830`), which is why the adapter can hold the runtime while the wire layer holds only the function. This is the same call the NFS DEALLOCATE handler makes (`internal/adapter/nfs/v4/handlers/deallocate.go:84`).

**`common.WriteToBlockStore` and `common.CommitBlockStore` take `*engine.Store` concretely, not an interface.** So the wire handlers must NOT call them with a `BlockStore`. The split that keeps both properties — a testable interface for dispatch, and the shared durability rule for production — is:

- The handlers call `bs.WriteAt` / `bs.ReadAt` / `bs.PunchHole` directly through `BlockStore` (they are the same three calls `common` makes internally).
- **`commit` is where the shared rule must be reused, and it is the one place it can be.** `common.CommitBlockStore` is the durability decision (`requireDurable → localDurable || (Finalized && remoteDurable)`, `internal/adapter/common/write_payload.go:110`); reimplementing it in the adapter is exactly the duplication that makes the NBD path drift from NFS COMMIT and SMB Flush. So `BlockStore` needs a way to reach it that does not require the concrete type.

Two acceptable resolutions, in order of preference:

1. **Add a narrow interface for the commit seam in `internal/adapter/common`** — e.g. `type Committer interface { Flush(ctx context.Context, payloadID string) (*block.FlushResult, error); LocalDurable() bool; RemoteDurable() bool; RequireDurableCommit() bool }` and a `CommitBlockStore(ctx, c Committer, payloadID)` overload (rename the existing one's parameter type; `*engine.Store` satisfies it, so every existing call site compiles unchanged). This keeps one implementation of the rule and makes the fake able to drive `ErrNotDurableYet`.
2. If (1) is rejected as too wide a change, have `commit` call `common.CommitBlockStore(ctx, store, exp.PayloadID)` where `store` is the concrete `*engine.Store` the resolver returns — i.e. `StoreResolver` returns both the interface and the concrete store. This works but makes the fake unable to exercise the durability rule, which is the rule most worth exercising.

**Option 1 is what the plan assumes.** It is a two-line change to `internal/adapter/common/write_payload.go` (widen the parameter type), and it is the difference between "the adapter calls the same durability rule as every other protocol" and "the adapter has its own copy of the durability rule."

**Resolve once per connection, not per command.** The handle is fixed for the connection's life, so a per-command resolution would decode a handle and take a lock on every 4 KiB read.

**Use the `pipe(t)` helper for these tests, and do not redefine it.** `connection_test.go` is `package nbd` (internal), the same package as `handshake_test.go`, so the helper declared there (`func pipe(t *testing.T) (server, client net.Conn)`, plan line 411) is already in scope — a second declaration is a redeclaration error. It exists because `net.Pipe` is synchronous and unbuffered: several of these tests read a reply header and assert on it without draining the payload that follows, which with `net.Pipe` leaves the server blocked forever and turns a test that should fail loudly into one that hangs until the Go test timeout (the full reasoning is at plan lines 403-410, where the helper is defined). If you find yourself wanting `net.Pipe` here, read that comment first.

Tests:

```go
func TestConnection_ReadServesBytes(t *testing.T)
func TestConnection_ReadServesZerosForUnwrittenRange(t *testing.T)
func TestConnection_UnknownCommandIsEINVAL(t *testing.T)
func TestConnection_UnimplementedCommandIsEINVAL(t *testing.T)
func TestConnection_WriteIsEINVALBeforeTask5(t *testing.T) // replaced in Task 5
func TestConnection_ErrorsCarryTheCookie(t *testing.T)
func TestConnection_DiscClosesWithoutReply(t *testing.T)
func TestConn_PanicInHandlerDoesNotKillTheProcess(t *testing.T)
func TestConnection_FailedStoreResolutionDegradesAndCounts(t *testing.T)
```

Assertions that matter, spelled out:

- `TestConnection_ReadServesBytes` — write 4096 known bytes into the fake at payload `"p1"`, send `Request{Type: CmdRead, Cookie: 7, Offset: 0, Length: 4096}`, expect a 16-byte reply with `Error == 0` and `Cookie == 7`, followed by exactly 4096 bytes matching the fixture.
- `TestConnection_ReadServesZerosForUnwrittenRange` — read past the fixture's end; expect `Error == 0` and 4096 zero bytes. A genuine hole reads as zeros, not as an error: that is the engine's contract and the adapter must not turn it into `EIO`.
- `TestConnection_UnknownCommandIsEINVAL` — send `Type: 99`; expect `Error == ErrEINVAL` and **no payload**. A reply header for a command whose reply shape is unknown must not be followed by data.
- `TestConnection_UnimplementedCommandIsEINVAL` — send `Type: CmdCache` (5); expect `Error == ErrEINVAL`, not `ErrENOTSUP`. `NBD_ENOTSUP` is reserved by the spec for `WRITE_ZEROES`+`FAST_ZERO`, which we do not advertise; a command we do not implement is an invalid command.
- `TestConnection_ErrorsCarryTheCookie` — with `readErr = errors.New("boom")`, expect `Error == ErrEIO` and the cookie echoed. A dropped cookie makes a client hang forever.
- `TestConnection_DiscClosesWithoutReply` — send `Type: CmdDisc`; expect the server to close the connection with nothing written after the request.

**Review Focus 2 and 3** live here:

```go
// The last byte of the device is addressable. Only offset+length > size is out
// of range.
func TestConnection_ReadAtExactEndIsServed(t *testing.T) {
	// size 8192; request Offset: 4096, Length: 4096 → expect Error == 0 and
	// 4096 bytes, not EINVAL.
}

func TestConnection_ReadPastEndIsEINVAL(t *testing.T) {
	// size 8192; request Offset: 4096, Length: 4097 → expect ErrEINVAL.
}

func TestConnection_ReadZeroLengthIsEINVAL(t *testing.T) {
	// Length: 0 → expect ErrEINVAL and no payload, not a silent empty success
	// and not a dropped connection.
}

func TestConnection_OffsetPlusLengthOverflowIsEIO(t *testing.T) {
	// Offset: math.MaxUint64, Length: 4096 → expect ErrEIO.
	// Not EOVERFLOW: the spec reserves that for READ+DF and payload-less
	// over-max requests, neither of which we support. Not EINVAL either — the
	// request is not out of range, it cannot be interpreted at all.
	// The check must not be `offset+length > size` with a wrapping add.
}

// FUA is valid on every command once SEND_FUA is advertised: the spec says
// "the server MUST accept all commands with this bit set (even by ignoring the
// bit)" and notes "existing clients are known to set this bit on other
// commands". A READ writes nothing, so the bit is ignored and the read succeeds.
// Refusing would break real clients for a guarantee nobody asked for.
func TestConnection_ReadWithFUAFlagSucceeds(t *testing.T) {
	// Flags: FlagFUA, Type: CmdRead → expect Error == 0 and the data.
}

// NO_HOLE is documented for WRITE_ZEROES only, so on WRITE it is a flag "not
// explicitly documented as applicable to the given request" → NBD_EINVAL.
func TestConnection_WriteWithNoHoleFlagIsEINVAL(t *testing.T) {
	// Flags: FlagNoHole, Type: CmdWrite → expect ErrEINVAL.
}

func TestConnection_UnadvertisedFlagIsEINVAL(t *testing.T) {
	// Flags: FlagDF | FlagReqOne | FlagFastZero on a READ → ErrEINVAL each.
}
```

**Review Focus 5:**

```go
// A client that sends a partial header and stalls must not hold the connection
// open forever, and must not make the server allocate for a length it never
// sent.
func TestConnection_PartialHeaderTimesOut(t *testing.T) {
	// Timeouts.Read = 200ms. Write 4 bytes, then block. Expect Serve to return
	// within ~1s with a timeout error, and no reply on the wire.
}
```

**Review Focus 7 and 8:**

```go
// The base adapter's accept loop has no recover (pkg/adapter/base.go:373 runs
// the handler on a bare goroutine), so a panic that escapes here does not kill
// the connection — it kills the process, and with it every NFS and SMB
// connection. Both sibling adapters recover for this reason: NFS at
// pkg/adapter/nfs/connection.go:420, SMB at pkg/adapter/smb/connection.go:474.
//
// A fake store whose ReadAt panics is the cheapest way to reach the recover:
// the panic originates below the dispatcher, exactly where a real one would
// (a nil map, a slice bounds error in a handler, a bad type assertion on a
// store the engine returned).
func TestConn_PanicInHandlerDoesNotKillTheProcess(t *testing.T) {
	// storeFake with panicOnRead set. Drive a full handshake and one READ
	// through Conn.Serve (not handler.serve — the recover lives on Conn).
	// Run Serve on a goroutine and assert it RETURNS within ~2s.
	//
	// The assertion is "it came back", and that is sufficient: if the recover
	// is missing the panic unwinds the goroutine and the test binary dies, so
	// the failure mode is a crashed package rather than a failed assertion.
	// Do NOT wrap the call in a recover() in the test itself — that would make
	// the test pass whether or not Conn.Serve has one.
}

// The store is resolved after the handshake, so a resolver that fails is the
// only way to reach this path deterministically: the window between the
// handshake ACK and the resolution is a few microseconds on the server's own
// goroutine, so no external test can land a share-removal inside it.
func TestConnection_FailedStoreResolutionDegradesAndCounts(t *testing.T) {
	// NewHandler with a StoreResolver returning a sentinel error and a fake
	// exporter whose Resolve succeeds. Drive one full handshake, then assert in
	// this order:
	//   1. serve returned a non-nil error (the connection ends)
	//   2. h.TakeDegraded() == 1
	//   3. a second h.TakeDegraded() == 0
	// The order matters: asserting the consumption first would pass on a
	// counter that was never incremented.
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run TestConnection -v`
Expected: FAIL — `undefined: NewHandler`, `undefined: BlockStore`.

- [ ] **Step 3: Implement `connection.go`**

`serve(ctx, conn)`:
1. `exp, err := ServeHandshake(ctx, conn, h.exporter)`. On error, return it — the handshake already told the client why where the protocol has a way to.
2. Resolve the block store once: `bs, err := h.stores(ctx, exp)`. A failure here is `Degraded` territory (`h.degraded.Add(1)`), not a wire error — the connection ends, because every subsequent command would fail the same way.
3. **Loop, one command at a time.** Before each `ReadRequest`, arm the wait for the next header from `Idle` when it is set, else from `Read`; a deadline error returns a timeout error from `Serve`, closing the connection. On `ErrBadMagic` or any decode error, return it.

```go
// Idle bounds the wait for a header; Read bounds finishing a request that has
// already started (the payload read in Task 5). At the defaults both are 5m, so
// the distinction only shows for an operator who separates them — which is
// exactly when the field's own doc has to be true rather than nearly true.
//
// Idle == 0 means no idle timeout, so it cannot be folded into a min(): the
// guard is an independent arm, the same shape NFS uses at
// pkg/adapter/nfs/connection.go:312.
budget := h.timeouts.Read
if h.timeouts.Idle > 0 {
	budget = h.timeouts.Idle
}
if err := conn.SetReadDeadline(time.Now().Add(budget)); err != nil {
	return err
}
```
4. Switch on `req.Type`. **Every branch must validate flags first**, then bounds, then act — in that order, so a malformed request never reaches the store:

```go
// The order matters: a request that is both malformed and out of range must
// fail on the malformation, because the range check reads the request's own
// fields.
```

5. On `CmdDisc`: return nil without writing a reply. The spec: *"There is no reply to an NBD_CMD_DISC."* Values of length and offset MUST be zero — reject nonzero with `ErrEINVAL` rather than closing.

Set `conn.SetWriteDeadline(time.Now().Add(h.timeouts.Write))` before every reply write.

**Shutdown:** when draining, answer new commands with `ErrESHUTDOWN` rather than serving them. The spec's §4.6 rationale: it is the protocol's "reconnect" signal, which is what the kernel's dead-connection timeout waits for. The draining state is **read from the base adapter, not owned by the handler**: `BaseAdapter` exposes `Shutdown chan struct{}` (`pkg/adapter/base.go:105`), closed by `initiateShutdown` (`:413`), so `Adapter.Serve` passes a closure and there is no second flag to keep in sync:

```go
draining := func() bool {
	select {
	case <-a.BaseAdapter.Shutdown:
		return true
	default:
		return false
	}
}
```

**The handler's own read deadline fights the base adapter's shutdown, and the handler has to lose that fight deliberately.** `initiateShutdown` calls `interruptBlockingReads` (`pkg/adapter/base.go:447`), which sets a **100 ms** read deadline on every active connection to unblock pending reads. But the handler arms its own deadline before each read (5 minutes by default), and a `SetReadDeadline` **replaces** the previous one. So a handler that finishes a command after shutdown began will overwrite the 100 ms interruption with a fresh 5-minute deadline, and `waitForDrain` will sit until `ShutdownTimeout` (30 s) expires and force-close.

The loop must therefore check `h.draining()` **before** arming its own deadline, and when draining:

- **Do not** arm the long deadline. Arm a short one (or none, leaving the base adapter's 100 ms in place), so the read unblocks promptly and the connection closes inside the drain window.
- If a command *does* arrive in that window, answer it `ErrESHUTDOWN` — that is the polite path the spec wants, and it is reachable only because the client had already written the request.
- **Do not** close the connection on the first draining observation. Closing immediately is what makes the client's kernel error the device instead of pausing it; the base adapter's `ShutdownTimeout` is the backstop, and `waitForDrain` force-closes when it expires.

`ErrESHUTDOWN` on drain is therefore **best-effort by construction**, not a guarantee: an idle connection is closed by the 100 ms deadline long before any command arrives. That is acceptable because a TCP close is exactly what the kernel's `dead_conn_timeout` is watching for (§6.1) — `ESHUTDOWN` is the fast, polite version of the same signal, not a different mechanism.

- [ ] **Step 4: Implement `handlers_read.go`**

```go
func (h *Handler) handleRead(ctx context.Context, bs BlockStore, exp *Export, req *Request, conn net.Conn) error
```

Checks in order: overflow → `ErrEIO`; `req.Length == 0` → `ErrEINVAL`; `req.Offset+uint64(req.Length) > exp.Size` → `ErrEINVAL`; then any flag other than `FlagFUA` → `ErrEINVAL`.

**Flag handling on READ:** `FlagFUA` is *accepted and ignored* — the spec requires the server to accept it on all commands once `SEND_FUA` is advertised (*"the server MUST accept all commands with this bit set (even by ignoring the bit)"*), and a read writes nothing, so there is nothing to persist. Every other flag (`NO_HOLE`, `DF`, `REQ_ONE`, `FAST_ZERO`) is `ErrEINVAL`.

**Overflow is `ErrEIO`, not `ErrEOVERFLOW`.** The spec: *"The server SHOULD NOT return `NBD_EOVERFLOW` except as documented in response to `NBD_CMD_READ` when `NBD_CMD_FLAG_DF` is supported, or when a command without payload requests a length larger than an advertised maximum payload length."* We advertise neither, so the value is out of bounds for us to send.

Then `buf := make([]byte, req.Length)`, `bs.ReadAt(ctx, string(exp.PayloadID), buf, req.Offset)`, and on error `writeSimpleReply(conn, req.Cookie, mapError(err))` with no payload.

**Wrap the context with `common.WithRequestDeadline(ctx)` (`internal/adapter/common/deadline.go:32`) before `ReadAt`.** It returns `(ctx, cancel)`, so `defer cancel()`. Two properties matter: it is a **no-op when the context already carries a deadline** (`if _, ok := ctx.Deadline(); ok { return ctx, func() {} }`), so it is free to call per command; and its own doc says a handler looping over choke points *"takes one deadline up front with this function and passes it down, so the loop shares one budget instead of starting a new one per call."* — for NBD the unit is **one command**, not one connection, because a connection is long-lived and a connection-wide budget would expire a boot disk mid-boot. So call it per command and let `cancel` run at the end of that command.

Every store call in this adapter (read, write, punch, flush) goes through it; that is the same wrapper `ReadFromBlockStore` and `WriteToBlockStore` apply. Skipping it means a hung remote fetch blocks the connection loop past every configured timeout.

**Do not** use `common.ReadFromBlockStore` (`internal/adapter/common/read_payload.go:42`): it takes `*engine.Store` concretely and returns a *pooled* buffer whose `Release()` the caller must call. A pooled buffer that outlives the reply write is a data race waiting to happen, and the handler has no pool. Allocate per-command and let the GC have it.

On success: write the reply header with `Error: 0` and `Cookie: req.Cookie`, then `buf`.

**Do not** allocate `buf` before the bounds checks. A client that sends `Length: 0xffffffff` with a 1 GiB export must get `ErrEINVAL` without a 4 GiB allocation.

- [ ] **Step 5: Implement `mapError`**

```go
func mapError(err error) NBDError
```

**`mapError` must call `common.NormalizeBlockStoreError` (`internal/adapter/common/normalize.go:25`, exported by Task 4) before classifying**, or the `merrs` cases below will not match: the engine returns raw sentinels and `merrs.StoreError` values, and normalization is what turns the raw ones into a `*merrs.StoreError` with the right code while leaving an already-wrapped one alone (`if goerrors.As(err, &storeErr) { return err }`). **Do not** hand-roll a classifier that skips normalization: it will silently report `ErrEIO` for a disk-full condition, which is exactly the class of bug this design exists to prevent.

- `nil` → 0
- `engine.ErrStoreClosed` → `ErrEIO` (the store is gone; the client should error the device, and §4.6 maps this to EIO rather than ESHUTDOWN because ESHUTDOWN means "reconnect" and a closed store is not a restart)
- `common.ErrNotDurableYet` → `ErrEIO`. `internal/adapter/nbd` importing `internal/adapter/common` is safe and verified: `go list -deps ./internal/adapter/common` includes nothing from `internal/adapter/nbd`, so there is no cycle. (This is the same direction the NFS and SMB handlers import in.)
- `merrs.ErrNoSpace` → `ErrENOSPC`
- `merrs.ErrPermissionDenied` → `ErrEPERM`
- **`merrs.ErrAccessDenied` → `ErrEPERM` as well, and this arm is not optional.** These are two different codes and only one of them was in the first draft of this list. `ErrPermissionDenied` is POSIX `EPERM` — *"the operation requires ownership or root privileges"* (`pkg/metadata/errors/errors.go:29-32`) — while `ErrAccessDenied` is POSIX `EACCES`, *"permission bit violations"*, *"used when the caller lacks the required read/write/execute permission bits"* (`:22-25`). **The metadata permission funnel returns `ErrAccessDenied`**, at `pkg/metadata/auth_permissions.go:510` and `:539` (`&StoreError{Code: ErrAccessDenied, Message: "write permission denied"}`). So a write refused by POSIX mode bits — the single most likely permission refusal this adapter will ever see — falls through to the default arm and is reported to the client as `NBD_EIO`. The client's filesystem then sees an I/O error on a write it was not permitted to make, which is a hardware-failure report for an authorization decision. Map both codes to `ErrEPERM`; NBD has one permission error and both mean "you may not do this".
- `context.Canceled`, `context.DeadlineExceeded` → `ErrESHUTDOWN`
- default → `ErrEIO`

The `ErrAccessDenied` arm needs a test, and it cannot be written against a fake store — the error is produced by the metadata service, not the block store. Drive it through a real metadata service: create the volume file as one identity, then have the handler write as a different one whose mode bits forbid it, and assert `ErrEPERM`. A fixture that creates the volume with the same identity the handler writes as will pass vacuously, because `ApplyOwnerDefaults` (`pkg/metadata/validation.go:465`) sets the file's owner to the creating identity and the owner always has the mode bits it wrote.

**Order matters where the causes overlap.** `engine.ErrStoreClosed` and the `merrs` pair are tested before the `context` pair, because a store failure wrapped in a cancellation should report the store failure — the operator needs to know the store is gone, not that a context expired. `errors.Is` walks the whole wrap chain, so a single `err` can match several cases; the first match wins.

`mapError` never returns `ErrEOVERFLOW` or `ErrENOTSUP`; both are unreachable by design (see the Global Constraints entry). If a future handler wants one, it must also add the advertisement that makes it legal to send.

Use `errors.Is` throughout, and put the original error in a log line at Debug for expected classes and Error otherwise, per CLAUDE.md invariant 6.

- [ ] **Step 6: Wire the adapter to the wire layer**

This step exists here, not in Task 3, because it is the first point at which every symbol it names exists: `nbd.Handler`, `nbd.NewHandler`, `nbd.BlockStore`, `nbd.StoreResolver`, and `nbd.Timeouts` are all declared by this task. Task 3 deliberately left `Serve`'s body and `NewConnection` as stubs so that its own commit compiles; this step replaces both.

The adapter is the only place that knows about both halves — the runtime and the wire layer — so this is where the two seams meet. It must import `internal/adapter/nbd`, which is legal (both are in-repo and `internal/adapter/nbd` does not import `pkg/adapter/nbd`, so there is no cycle).

```go
type Adapter struct {
	*adapter.BaseAdapter
	config  Config
	runtime *runtime.Runtime
	// handler is built once in Serve, before the listener exists, and read by
	// NewConnection and Healthcheck — both of which run on other goroutines.
	// An atomic pointer, not a plain field: Serve runs in its own goroutine
	// (pkg/controlplane/runtime/adapters/service.go:611) while Healthcheck is
	// reached from an HTTP handler (internal/controlplane/api/handlers/adapters.go:240)
	// via Runtime.AdapterChecker, so a plain field is a data race the race
	// detector reports on any probe that lands during startup.
	handler atomic.Pointer[nbd.Handler]
}

func (a *Adapter) SetRuntime(rt any) {
	r, ok := rt.(*runtime.Runtime)
	if !ok {
		panic(fmt.Sprintf("nbd: SetRuntime wants *runtime.Runtime, got %T", rt))
	}
	// The base adapter keeps its own reference for the shared accept loop,
	// which reaches it for connection metrics (pkg/adapter/base.go:359, :387).
	a.BaseAdapter.SetRuntime(rt)
	a.runtime = r
}

func (a *Adapter) Serve(ctx context.Context) error {
	if netutil.IsNonLoopbackHost(a.config.BindAddress) && !a.config.AllowRemoteBind {
		return fmt.Errorf("nbd: refusing to bind %q without allow_remote_bind; NBD has no authentication beyond optional TLS, so a remotely reachable export hands every byte of the volume to anyone who can reach the port", a.config.BindAddress)
	}
	if a.runtime == nil {
		return errors.New("nbd: SetRuntime was not called before Serve")
	}
	id := nbd.Identity{UID: a.config.UID, GID: a.config.GID}
	exporter := nbd.NewRuntimeExporter(a.runtime, id)
	resolver := func(ctx context.Context, exp *nbd.Export) (nbd.BlockStore, error) {
		return common.ResolveForWrite(ctx, a.runtime, exp.Handle)
	}
	draining := func() bool {
		select {
		case <-a.BaseAdapter.Shutdown:
			return true
		default:
			return false
		}
	}
	a.handler.Store(nbd.NewHandler(exporter, resolver, a.runtime.GetMetadataService(),
		id, a.config.timeouts(), draining))
	return a.ServeWithFactory(ctx, a, nil, nil)
}

func (a *Adapter) NewConnection(conn net.Conn) adapter.ConnectionHandler {
	return nbd.NewConn(a.handler.Load(), conn)
}
```

Also add to `pkg/adapter/nbd/config.go` the accessor this step calls:

```go
// timeouts narrows the adapter's four configured durations to the three the
// wire layer needs. Shutdown is deliberately not here: the base adapter owns
// the drain deadline, and the handler's job during drain is to answer
// ErrESHUTDOWN, not to run its own timer.
func (c *Config) timeouts() nbd.Timeouts {
	return nbd.Timeouts{Read: c.Timeouts.Read, Write: c.Timeouts.Write, Idle: c.Timeouts.Idle}
}
```

Five things this shape gets right, and one it makes explicit:

- **`NewConnection` returns a small per-connection wrapper, not the shared handler.** `adapter.ConnectionHandler.Serve` takes only a context (`pkg/adapter/base.go:38`), so the socket must be bound at construction — `return nbd.NewConn(a.handler.Load(), conn)`, the same shape as `pkg/adapter/smb/adapter.go:717`. The handler itself is shared and stateless; `Conn` carries nothing but the socket and the handler pointer, so there is no per-connection protocol state to leak.
- **The resolver returns `*engine.Store` as `nbd.BlockStore` implicitly** — `common.ResolveForWrite` returns the concrete type, which satisfies the interface. No adapter.
- **`Serve` builds the handler once, before the listener exists**, so a `nil` runtime or a bad config fails as a startup error rather than as a failed first connection.
- **The bind gate runs first, before the runtime is consulted**, which is what `TestServe_RefusesRemoteBindEvenWithNoRuntime` in Task 3 pins. Keep that ordering when you move the body.
- **`SetRuntime` panics on the wrong type**, matching the documented contract of `pkg/adapter/base.go:185` (*"must be a `*runtime.Runtime` or it panics"*) and the constructor-panics-on-invalid-config precedent at `pkg/adapter/smb/adapter.go:176`.
- **`preAccept` stays `nil`** — there is nothing checkable at the TCP layer for a protocol that names its export inside the handshake.

- [ ] **Step 7: Add `Degraded` to the healthcheck**

Add a counter the connection path increments on a failed export resolution or block-store resolution, and have `Healthcheck` report `StatusDegraded` when it is nonzero since the last probe. Message: `"NBD adapter is serving but N export resolutions have failed"`. Keep the probe cheap — read the counter and reset it, no I/O.

The counter lives on `Handler` (`h.degraded`), but `Healthcheck` is on `Adapter` in the other package, so the adapter needs a read-and-reset accessor: `func (h *Handler) TakeDegraded() int64` using `h.degraded.Swap(0)`. `Swap` rather than `Load` then `Store` — two steps let a concurrent increment vanish between them. `Adapter.Healthcheck` calls it only when the handler exists (before `Serve` there is nothing to ask):

```go
func (a *Adapter) Healthcheck(ctx context.Context) health.Report {
	if !a.config.Enabled {
		return health.Report{Status: health.StatusDisabled, Message: "NBD adapter is disabled in config", CheckedAt: time.Now().UTC()}
	}
	if h := a.handler.Load(); h != nil {
		if n := h.TakeDegraded(); n > 0 {
			return health.Report{Status: health.StatusDegraded, Message: fmt.Sprintf("NBD adapter is serving but %d export resolutions have failed", n), CheckedAt: time.Now().UTC()}
		}
	}
	return a.BaseAdapter.Healthcheck(ctx)
}
```

The `Disabled` branch and the final delegation are the same shape as `pkg/adapter/nfs/healthcheck.go`; only the `Degraded` branch is new, and it must come **before** the delegation because `BaseAdapter.Healthcheck` has no way to know about a counter it does not own. `TakeDegraded` is called on every probe, so the count is "since the last probe" — say so in the field's doc, because a reader will otherwise assume it is cumulative.

**The `handler` field must be an `atomic.Pointer[nbd.Handler]`** (Step 6), and this is the step that proves it: `Serve` stores it on the accept-loop goroutine while `Healthcheck` loads it from the API handler's goroutine. A plain field races — verified with a probe reproducing this exact shape under `-race`, which reported `WARNING: DATA RACE` between the `Serve` write and the `Healthcheck` read. `NewConnection` also loads it, and that runs on the accept goroutine, so the atomic is what makes the read legal there too.

**Add a test for the `Degraded` branch, and put it where it can actually be written.** The counter has two halves — an increment on a failed store resolution (`connection.go`) and a read-and-reset on a probe (`pkg/adapter/nbd/healthcheck.go`) — and neither is exercised by the two existing healthcheck tests, which only cover the `Disabled` and pre-`Serve` paths.

The increment half is tested in **`internal/adapter/nbd/connection_test.go`** (`package nbd`), not in the external healthcheck test, and the reason is structural rather than stylistic: the increment happens in `serve` **between** `ServeHandshake` and the store resolution (`Step 3` items 1 and 2), and that window is not reachable from outside — a client sees the handshake ACK, and the resolution runs microseconds later on the server goroutine, so no external test can land a share-removal inside it. The internal test does not need to: it injects a `StoreResolver` that fails directly.

```go
// The store resolution happens after the handshake, so a resolver that fails
// is the only way to reach this path without racing the server's own goroutine.
func TestConnection_FailedStoreResolutionDegradesAndCounts(t *testing.T) {
	// NewHandler with a StoreResolver returning a sentinel error, and a fake
	// exporter whose Resolve succeeds. Drive one full handshake, then assert:
	//   - serve returned an error (the connection ends)
	//   - h.TakeDegraded() == 1
	//   - a SECOND call to h.TakeDegraded() == 0 (the probe consumes the count)
	// Assert in that order, so the test cannot pass by the counter never having
	// been incremented at all.
}
```

The probe half is tested in **`pkg/adapter/nbd/healthcheck_test.go`** (`package nbd_test`), which reaches the counter only through the exported surface — `Adapter.Healthcheck`. Do **not** add a `Handler()` accessor to `Adapter` to reach in: it was considered and rejected as dead surface, since nothing in Tasks 4–8 needs it, and the increment half is already covered internally. Drive it through the real path instead:

```go
// The Degraded branch needs the handler to exist, so this test starts Serve.
// It does not need a failed resolution: removing the share after the handshake
// is not reachable from here (see connection_test.go's note), so the count is
// produced by the only external lever there is — resolving an export that does
// not exist, which is a handshake failure rather than a store failure and does
// NOT increment. That is the point: this test pins the OBSERVABLE contract,
// that a probe with a zero count reports healthy, and leaves the increment to
// the internal test.
func TestNBDAdapter_Healthcheck_NotDegradedWithoutFailures(t *testing.T) {
	// New(config), SetRuntime, start Serve on a cancellable ctx and wait for
	// ListenerReady. Probe twice; expect neither probe to be StatusDegraded.
	// A probe must not invent degradation, and TakeDegraded on a fresh handler
	// must not block or panic.
}
```

- [ ] **Step 8: Write the integration test**

Create `internal/adapter/nbd/integration_test.go`. This is the first test over a **real** `engine.Store`, and it is where the durability contract gets pinned.

```go
func newIntegrationFixture(t *testing.T) (*runtime.Runtime, string, *engine.Store)
```

Use `testruntime.NewWithShareStore(t)` (`internal/adapter/testruntime/runtime.go:53`), register a metadata store, `AddShare` with `BlockStoreID`, create `volumes/boot`, set its size, and return the runtime, the export name, and `rt.GetBlockStoreForShare(share)`. The journal root comes from `testruntime.New`, so the local tier is an fs store — which is what makes `LocalDurable()` true and the FLUSH assertions below meaningful.

**The fixture must pass `Enabled: true` in its `ShareConfig`.** `AddShare` copies the flag verbatim (`pkg/controlplane/runtime/shares/lifecycle.go:548`) and nothing defaults it, so a share built without it is disabled — and the exporter now refuses to resolve a disabled share (`internal/adapter/nbd/volume.go`). Every handler test in this task resolves its export through that gate, so omitting the field fails the whole task with a mystery `ErrUnknownExport` that names nothing about the real cause. Production is unaffected: the DB column defaults true (`pkg/controlplane/models/share.go:41`, `gorm:"default:true"`) and `init.go:314` propagates it, so this is a fixture-only trap.

```go
func TestIntegration_ReadAfterWriteOverRealStore(t *testing.T) {
	// 64 KiB of deterministic non-zero bytes written through the handler's
	// WRITE path (or directly via bs.WriteAt while Task 5 is pending), then
	// read back through the handler's READ path; compare byte-exact.
}

func TestIntegration_ReadEvictedButManifestCoveredRange(t *testing.T) {
	// Write, then make the bytes evictable, then evict, then READ. Expect real
	// bytes, not zeros and not an error: this is the hydrate path
	// (pkg/block/engine/read_internal.go:56).
	//
	// The eviction step is not "write then Flush". Verified with a probe against
	// this exact fixture: after WriteAt + Flush, DrainLocalSynced evicts NOTHING
	// (SegmentsEvicted=0), because the share's journal is durable
	// (pkg/block/journal/store.go:365 sets durable true by default) and
	// RequireDurableCommit is false, so Flush returns early at
	// pkg/block/engine/flush.go:554 with {Finalized: false} and never drains the
	// remote. The bytes stay dirty, and journal.Evict skips any segment holding
	// a dirty record (pkg/block/journal/evict.go:36) — so the range is still
	// local, the read is a warm local read, and the test passes without ever
	// exercising the hydrate path it exists to prove.
	//
	// DrainAllUploads is what makes the bytes evictable: it force-flushes every
	// dirty range to the remote and waits for the uploads to settle. Probe
	// result with it in place: SegmentsEvicted=1, BytesFreed=65676.
	//
	// So the sequence is: WriteAt, DrainAllUploads, DrainLocalSynced, and ASSERT
	// the eviction actually happened before reading:
	//
	//	ev, err := bs.DrainLocalSynced(ctx)
	//	if err != nil { t.Fatal(err) }
	//	if ev.SegmentsEvicted == 0 {
	//		t.Fatalf("nothing was evicted, so this test cannot exercise the hydrate path: %+v", ev)
	//	}
	//
	// That assertion is the whole test's reason for existing. Without it a
	// fixture change that keeps the bytes local turns this into a test that
	// passes while proving nothing — which is worse than no test, because it
	// reads as coverage.
	//
	// The remote is the fixture's `memory` block store (remotememory), whose
	// durability type-default is FALSE (pkg/block/remote/memory/store.go:51-53),
	// so RemoteDurable() reports false and a require-durable commit could not be
	// satisfied here. That is fine for this test: DrainAllUploads does not read
	// the durability flag, it drains the uploads.
}

func TestIntegration_ReadNeverWrittenRangeIsZeros(t *testing.T) {
	// A range inside the export with no write ever. Expect 4096 zeros and
	// Error == 0.
}
```

**Do not expect the eviction test to be satisfied by `Flush` alone, and do not "fix" it by setting `RequireDurableCommit` to true.** The early return at `pkg/block/engine/flush.go:554` is deliberate — its comment records that the synchronous path *"turned every FILE_SYNC/DATA_SYNC WRITE into an inline S3 PutObject the reply waited on: multi-second per-op stalls at ~3% CPU"* — and flipping the flag to make a test's setup convenient would both change the durability contract under test and slow every write. `DrainAllUploads` is the supported way to make bytes evictable on demand.

- [ ] **Step 9: Run the tests and confirm they pass**

Run: `go test ./internal/adapter/nbd/ -race -v`
Expected: PASS.

- [ ] **Step 10: Verify the bounds checks actually bite**

Comment out the overflow check in `handleRead`, run `go test ./internal/adapter/nbd/ -run TestConnection_OffsetPlusLengthOverflowIsEIO -v`, and confirm it fails on the assertion. Restore. Repeat for the exact-end check: change `>` to `>=` and confirm `TestConnection_ReadAtExactEndIsServed` fails.

- [ ] **Step 11: Commit**

```bash
git add internal/adapter/nbd/ pkg/adapter/nbd/
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "feat(nbd): connection loop and READ handler"
```

---

## Task 5: WRITE, FUA, and FLUSH

**Files:**
- Create: `internal/adapter/nbd/handlers_write.go`, `internal/adapter/nbd/handlers_control.go`
- Modify: `internal/adapter/nbd/connection.go` (dispatch), `internal/adapter/nbd/handlers_read.go` (none)
- Test: `internal/adapter/nbd/connection_test.go`, `internal/adapter/nbd/integration_test.go`

**Interfaces:**
- Consumes: Task 4's `BlockStore` interface, `Handler`, `mapError`; `internal/adapter/common.WriteToBlockStore` (`write_payload.go:54`), `internal/adapter/common.CommitBlockStore` (`:110`) with the `Committer` parameter Task 4 introduced.
- Produces:
  - `func (h *Handler) handleWrite(ctx context.Context, bs BlockStore, exp *Export, req *Request, conn net.Conn) error`
  - `func (h *Handler) handleFlush(ctx context.Context, bs BlockStore, exp *Export, req *Request, conn net.Conn) error`
  - `func (h *Handler) commit(ctx context.Context, bs BlockStore, exp *Export) error` — the shared FUA/FLUSH barrier

**The write path is four steps, and they are the same sequence the NFS handler uses** (`internal/adapter/nfs/v4/handlers/write.go:173`, `:209`, `:218`) — the only difference is that the NFS handler's `common.WriteToBlockStore` wrapper is inlined here, for the reason below:

```
authCtx := h.authContext(ctx, exp)   // per command: it embeds the command's Context
intent, err := h.meta.PrepareWrite(authCtx, exp.Handle, newSize)
bs.WriteAt(ctx, string(intent.PayloadID), nil, data, offset)
_, err = h.meta.CommitWrite(authCtx, intent)
```

**Step 3 is a direct `bs.WriteAt`, not `common.WriteToBlockStore`.** `WriteToBlockStore` takes `*engine.Store` concretely, and the handler holds a `BlockStore`. What `WriteToBlockStore` adds over the raw call is deadline wrapping (`WithRequestDeadline`) and error normalization (`normalizeBlockStoreError`) — so the handler must do both itself, and that is not optional:

```go
ctx, cancel := common.WithRequestDeadline(ctx)
defer cancel()
if _, err := bs.WriteAt(ctx, string(intent.PayloadID), nil, data, req.Offset); err != nil {
    return h.writeReply(conn, req.Cookie, mapError(err), nil)
}
```

Note the error shape: `mapError` returns `NBDError`, a `uint32` with no `Error()` method, so it is an *argument to the reply*, never a returned error. A handler that returned it would compile-fail, and a variant that swallowed it would leave the client waiting forever.

**`mapError` is the single place the wire layer converts an error, and it calls `common.NormalizeBlockStoreError` internally** — so the handler passes the raw error and needs no explicit normalize call. Normalizing first matters: the engine returns raw sentinels as well as `*merrs.StoreError`, and normalization is what turns a raw one into a `*merrs.StoreError` with the right code while leaving an already-wrapped one alone (`if goerrors.As(err, &storeErr) { return err }`). The failure mode of getting this wrong is an error code that silently degrades to `ErrEIO` because `errors.Is` no longer matches through an extra wrap — so a disk-full becomes an I/O error and the client has no way to know its device is full.

**Task 4 already exported it.** `NormalizeBlockStoreError` is at `internal/adapter/common/normalize.go:27` and `mapError` calls it at `internal/adapter/nbd/connection.go:291`. Nothing to do here — recorded so nobody goes looking for a rename that has already happened.

- [ ] **Step 1: Write the failing tests**

The names below are the contract; the bodies are yours to write. They are **not** transcribed because they cannot be: `handleWrite` calls `h.meta.PrepareWrite` and `h.meta.CommitWrite`, and the connection tests' `newTestHandler` passes **`nil`** for `meta` (`internal/adapter/nbd/connection_test.go:151`) — correct for Task 4, where nothing on the read path touched metadata. So every WRITE test that expects `Error == 0` needs a handler with a real metadata service.

**Add a second fixture helper** beside `newTestHandler`, and use it for the write tests. What landed in Task 5 is a small struct rather than a bare function, because a write test needs the resolved export and the runtime's metadata service together:

```go
// writeVolumeFixture holds a runtime, the volume file's export, and the
// share's real block store, built once per test.
type writeVolumeFixture struct {
	rt  *runtime.Runtime
	exp *Export
	bs  *engine.Store
}

func newWriteVolumeFixture(t *testing.T) *writeVolumeFixture
func (f *writeVolumeFixture) newHandler(t *testing.T, st BlockStore) *Handler
```

Both live in `internal/adapter/nbd/integration_test.go` (`:48`, `:69`) and are in `package nbd`, so `connection_test.go` reaches them without an import. `newHandler` passes `integrationIdentity` to `NewHandler`, and `newWriteVolumeFixture` creates the volume *as* `integrationIdentity` through `newIntegrationFixtureAs`.

Build it the way `integrationHandler` does (`internal/adapter/nbd/integration_test.go:96`): a `runtime.Runtime` from `testruntime.NewWithShareStore(t)` with one share and one metadata store, `rt.GetMetadataService()` for the service, and `exp.Handle` set to a real volume file's handle — a volume file created under the share's root with `CreateFile` and then sized with `SetFileAttributes`, because `ApplyCreateDefaults` zeroes the size (`pkg/metadata/validation.go:492`). Reuse the fixture that `integration_test.go` already builds if it can be factored into a helper both files call; do not build a second one that drifts.

**The fixture must create the volume as the same identity the handler writes as, and getting this wrong costs an afternoon.** `ApplyOwnerDefaults` (`pkg/metadata/validation.go:465`) assigns the new file the *creating* identity's UID/GID, and `createEntry` calls it at `pkg/metadata/file_create.go:389`; the mode bits you pass are then evaluated against whoever writes. A fixture that creates the volume as one identity and builds the handler with another produces `AccessDenied: write permission denied` from `PrepareWrite` on every happy-path test, and the error names the mode bits rather than the identity mismatch — so it reads as a permissions bug in the adapter. Create with the handler's identity; `RootAttr` on the share is a third, unrelated value and does not decide file ownership. Verified against a real runtime: create as UID 0 + write as 65534 on a 0644 file → `AccessDenied: write permission denied`; create as 65534 + write as 65534 → `PrepareWrite OK`.

Keep one test with the mismatch, deliberately: it is the only coverage for the `ErrAccessDenied` → `ErrEPERM` arm of `mapError`.

Tests whose assertions are *rejections* need no metadata (the handler returns before touching it), so they may keep `newTestHandler`. Tests that must reach `Error == 0` on a WRITE cannot.

```go
func TestConnection_WritePersistsBytes(t *testing.T)
func TestConnection_WritePastEndIsENOSPC(t *testing.T)
func TestConnection_WriteZeroLengthIsEINVAL(t *testing.T)
func TestConnection_RejectedWriteKeepsTheStreamSynchronized(t *testing.T)
func TestConnection_RejectedHugeWriteDoesNotAllocate(t *testing.T)
func TestConnection_WriteTruncatedPayloadIsAnError(t *testing.T)
func TestConnection_WriteWithFUAIsDurableBeforeReply(t *testing.T)
func TestConnection_FlushIsAckedWhenDurable(t *testing.T)
func TestConnection_FlushIsEIOWhenNotDurable(t *testing.T)
func TestConnection_FlushWithNonZeroLengthIsEINVAL(t *testing.T)
func TestConnection_FlushOnReadOnlyExportIsPermitted(t *testing.T)
```

What each must assert, in the terms the test file already uses (`sendRequest`, `readReplyHeader`, `readPayload`, `assertNoPayload`, `completeHandshake`, `pipe`):

- **`WritePersistsBytes`** — WRITE 4096 bytes at offset 8192 **with its payload**; `rep.Error == 0`; then a READ of `[8192, 12288)` on the same connection returns those bytes byte-exact. This is the only test that proves a WRITE round-trips, so it must assert the bytes rather than the error code.
- **`WritePastEndIsENOSPC`** — `Offset: exp.Size-4096, Length: 4097`, **payload sent**, expect `ErrENOSPC`. Not `EINVAL`: the spec reserves `ENOSPC` for a write past the end and `EINVAL` for a read or trim. The export size is fixed, so a WRITE past the end is refused, never grown.
- **`WriteZeroLengthIsEINVAL`** — `Length: 0` → `ErrEINVAL`, and **no drain** (there is no payload to consume). Then a valid READ on the same connection must succeed, which is what proves the zero-length path did not consume the next header.
- **`RejectedWriteKeepsTheStreamSynchronized`** — the desync guard, and the highest-value test in the task. Send a WRITE rejected on the range check (`Length: 4096`, offset past the end) **with its 4096-byte payload**, then a valid READ, and expect the READ's reply. Without the drain, the "request header" the server reads next is payload bytes.
- **`RejectedHugeWriteDoesNotAllocate`** — `Length: 1<<30` on a 1 MiB export, **send a little of the payload, then stall**. `ErrENOSPC` must arrive (the range check precedes both the reply and the drain) within `Timeouts.Read`, the process must not grow by 1 GiB, and `st.writes == 0`. Because the reply precedes the drain, the client gets the error it can act on; `Serve` then ends on the drain's read deadline, which is the correct outcome and the test should assert it rather than treat it as a failure. Under the rejected alternative — drain first — this assertion would be unreachable, since a stalled client never completes the drain and so would never see a reply at all.
- **`WriteTruncatedPayloadIsAnError`** — declare `Length: 4096`, send 100 bytes, then close. `Serve` returns an error and never reports success. Distinct from the huge-write case: here the client stopped mid-payload on a request the server had accepted.
- **`WriteWithFUAIsDurableBeforeReply`** — a fake whose `Flush` records whether the reply had already been written. This is the assertion that makes FUA mean something; it is the one Step 8 proves by moving the commit call.
- **`FlushIsAckedWhenDurable`** — `CommitAck: journal` semantics: an fs local store, `Flush` returning `&block.FlushResult{Finalized: false}, nil`. `rep.Error == 0`.
- **`FlushIsEIOWhenNotDurable`** — `requireDurable` set, `Flush` returning `{Finalized: false}, nil`, and the store reporting `localDurable`/`remoteDurable` both false. Expect `ErrEIO`: FLUSH promised durability and could not deliver it, so there is no honest success to send.
- **`FlushWithNonZeroLengthIsEINVAL`** — the spec requires `length` and `offset` to be zero on FLUSH.
- **`FlushOnReadOnlyExportIsPermitted`** — a read-only export still answers FLUSH successfully. Refusing would make a client error a device that has nothing to flush.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/adapter/nbd/ -run 'TestConnection_Write|TestConnection_Flush' -v`
Expected: FAIL.

Add `TestConnection_RejectedWriteKeepsTheStreamSynchronized` to **Review Focus** item 6 if it is not already there — it is the highest-value test in this task, because the failure it catches is silent and remote (a client sees a protocol error minutes later on an unrelated command).

**Delete `TestConnection_WriteIsEINVALBeforeTask5`** (`internal/adapter/nbd/connection_test.go:338`). It pins the dispatcher's `default` branch for a command the adapter advertises but does not yet serve, and after this task WRITE is served — so it asserts the opposite of the intended behaviour and will fail. `TestConnection_UnknownCommandIsEINVAL` and `TestConnection_UnimplementedCommandIsEINVAL` still cover the `default` branch for values the spec does not define. **Update `TestConnection_WriteWithNoHoleFlagIsEINVAL` rather than deleting it** (see Step 3 check 6): it pins a real rule — `NO_HOLE` is not a `WRITE` flag — but it must now send the payload its header declares.

- [ ] **Step 3: Implement `handleWrite`**

Order of checks, and the order is the point:

1. `req.Flags &^ FlagFUA != 0` → `ErrEINVAL` (`NO_HOLE`, `DF`, `REQ_ONE`, `FAST_ZERO` are not applicable to `WRITE`; the spec names `EINVAL` for *"a request with a flag not explicitly documented as applicable to the given request"*).
2. `exp.ReadOnly` → `ErrEPERM`.
3. `req.Length == 0` → `ErrEINVAL`.
4. `req.Offset > math.MaxUint64 - uint64(req.Length)` → `ErrEIO`.
5. `req.Offset + uint64(req.Length) > exp.Size` → `ErrENOSPC` (**not** `EINVAL`). The spec: *"The server SHOULD return `NBD_ENOSPC` if it receives a write request including one or more sectors beyond the size of the device. ... It SHOULD return `NBD_EINVAL` if it receives a read or trim request including one or more sectors beyond the size of the device."* A write past the end means the client's view of the device is too large; a read or trim past the end is a malformed request. Different causes, different codes.
6. **Reply first, then drain the payload — on every error path from here on, except a zero-length one.** The client has already sent `req.Length` bytes — the spec says so unconditionally (*"The client MUST follow the request header with *length* number of bytes"*). If the handler returns without consuming them, those bytes are read as the *next request header* and the connection desynchronizes into garbage. Check 3 already returned for `Length == 0`, so **no drain happens there**; every rejection that reaches this step has a non-zero payload and must discard it.

   **The reply goes out BEFORE the drain, and the two are independent.** The reply travels server→client and the payload travels client→server, so there is no stream-ordering constraint between them — the drain's obligation is to consume exactly `req.Length` bytes so the next header is read from the right offset, and that obligation is unchanged by when the reply is written. Replying first is the better order: a client that declared a huge length and stalled otherwise gets no reply at all, just a TCP close, which its kernel reports as a transport failure rather than the specific error it can act on. A client that has seen the error may also abandon the connection, and the server must not be blocked waiting on bytes that will never come.

   **This changes what the existing flag-rejection test must send.** `TestConnection_WriteWithNoHoleFlagIsEINVAL` (`internal/adapter/nbd/connection_test.go:578`) sent a bare `WRITE` header with `Length: 4096` and no payload, because before this task the dispatcher's `default` branch answered without ever reading one. Once `handleWrite` exists, the flag check returns `ErrEINVAL`, the reply is written, and then the drain blocks waiting for 4096 bytes that never arrive — so the test reads its reply fine but `Serve` is left draining a payload that will never come. **Update it to send its payload** so the drain completes and the connection stays usable. A test that declares bytes it does not send is asserting against a client the spec forbids.

   The drain's deadline is the request budget, re-armed. `SetReadDeadline` was set once before `ReadRequest` and `ReadRequest` read exactly 28 bytes, so what remains is already partly spent — and the drain is where a hostile or stalled client parks. Re-arming from `Timeouts.Read` is what bounds `RejectedHugeWriteDoesNotAllocate`: a client that declares 1 GiB and sends nothing is dropped after one timeout rather than held indefinitely:

   ```go
   // Reject path: the reply goes first, then the payload is consumed. The two
   // travel in opposite directions, so there is no ordering constraint between
   // them — and replying first is what lets a client that stalled mid-payload
   // still learn why, instead of seeing only a closed socket.
   if err := h.writeReply(conn, req.Cookie, nbdErr, nil); err != nil {
       return err
   }
   // Skipping the drain would leave the next header to be read out of payload
   // bytes. Bounded by a deadline so a client that declares a huge length and
   // sends nothing cannot hold the connection open.
   if err := conn.SetReadDeadline(time.Now().Add(h.timeouts.Read)); err != nil {
       return err
   }
   if _, err := io.CopyN(io.Discard, conn, int64(req.Length)); err != nil {
       return err // the stream position is unknown now; the connection must close
   }
   return nil // drained: the connection stays usable for the next command
   ```

   A failed drain returns the error, ending `Serve` and closing the connection — the byte position is unknown, so continuing would read garbage as the next header. A successful drain returns nil and the loop continues, which is what keeps the connection usable for `RejectedWriteKeepsTheStreamSynchronized` and the updated NO_HOLE test.

   `io.CopyN(io.Discard, ...)` rather than `make([]byte, req.Length)`: a client that declares a 4 GiB length must cost the server a bounded buffer, not an allocation. This is the same class as Review Focus 1, one layer down.
7. **Then** read the payload into a buffer sized to the request, using the same bounded shape so a truncated payload costs no allocation either:

   ```go
   data := make([]byte, req.Length)
   if _, err := io.ReadFull(io.LimitReader(conn, int64(req.Length)), data); err != nil {
       return err // the client stopped mid-payload; the connection is unusable
   }
   ```

   A truncated payload returns the read error — the connection is unusable at that point, so returning is correct and the client gets a TCP close rather than a success reply for bytes the server never saw.
8. `authCtx := h.authContext(ctx, exp)`, then `metaSvc.PrepareWrite(authCtx, exp.Handle, req.Offset+uint64(req.Length))` — `metaSvc` is `h.meta`, and `exp.Handle` was resolved once at handshake and has been on the handler ever since. (The auth context is built per command from the command's context, not cached: it embeds `Context`, and a cached one would carry the first command's deadline and cancellation into every later command on the connection.)
9. `bs.WriteAt(ctx, string(intent.PayloadID), nil, data, req.Offset)` on a deadline-wrapped ctx (see above), with the returned `[]block.ChunkRef` discarded — the canonical projection happens at Flush time, not here (`pkg/block/engine/readwrite.go` doc).
10. `h.meta.CommitWrite(authCtx, intent)`.
11. If `req.Flags&FlagFUA != 0`, `h.commit(ctx, bs, exp)` — **before** writing the reply. Spec: *"it MUST NOT send its reply to that command until all write operations... have been completed and persisted to non-volatile storage."*
12. Write the reply with no payload.

Errors from steps 8–11 map through `mapError`. A failure at step 9 or 10 must not write the reply header as success — and note that by then the payload has already been consumed, so the stream stays synchronized.

**`decision:`** the adapter writes through `bs.WriteAt` on a deadline-wrapped context and routes every error through one `mapError`, rather than calling `common.WriteToBlockStore`. That helper takes `*engine.Store` concretely, and widening it to an interface would force a second one for the read path; the deadline and normalization behaviour it provides is two lines and is reproduced here. The commit seam is the one that must be shared, and it is (`common.CommitBlockStore`).

- [ ] **Step 4: Implement `commit` and `handleFlush`**

`commit` calls `common.CommitBlockStore(ctx, bs, exp.PayloadID)` — the same function NFS COMMIT and SMB Flush call, with the same `requireDurable → localDurable || (Finalized && remoteDurable)` rule. This is the reason `BlockStore` carries the three durability accessors. `handleFlush` checks `req.Length == 0 && req.Offset == 0` → `ErrEINVAL` otherwise, then calls `commit`, then replies.

The durability decision is **not reimplemented**: `CommitBlockStore` already encodes `requireDurable → localDurable || (Finalized && remoteDurable)` and returns `ErrNotDurableYet` otherwise (`internal/adapter/common/write_payload.go:110`–`:171`). `mapError` turns that into `ErrEIO`.

**`decision:`** the FLUSH barrier is correct because dispatch is serialized (Task 4). Every write whose reply the client has already seen was handled and replied before this command was read, so "all already-replied writes are durable" follows from the ordering rather than from tracking. If pipelining is ever added, this stops being true and the barrier needs explicit tracking — say that in the comment.

- [ ] **Step 5: Wire the dispatch**

Add `CmdWrite` → `handleWrite` and `CmdFlush` → `handleFlush` to the switch in `connection.go`.

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./internal/adapter/nbd/ -race -v`
Expected: PASS.

- [ ] **Step 7: Add the integration assertions over the real store**

```go
func TestIntegration_FlushWithCommitAckJournalSucceeds(t *testing.T) {
	// fs local store (from testruntime.New's JournalRoot), requireDurable false.
	// Expect CommitBlockStore to return nil: LocalDurable() is true and
	// pkg/block/engine/flush.go:554 returns after a real fsync.
}

func TestIntegration_FlushRequireDurableWithUnhealthyRemoteIsEIO(t *testing.T) {
	// bs.SetRequireDurableCommit(true) (pkg/block/engine/engine.go:415) with a
	// memory remote. Expect ErrEIO through the handler.
}

func TestIntegration_WriteThenFlushThenReadBackByteExact(t *testing.T) {
	// 128 KiB of pseudorandom data through WRITE+FLUSH, then READ. Byte-exact.
}
```

- [ ] **Step 8: Verify the FUA ordering test actually bites**

In `handleWrite`, move the `h.commit` call to *after* `writeSimpleReply`. Run `go test ./internal/adapter/nbd/ -run TestConnection_WriteWithFUAIsDurableBeforeReply -v` and confirm it fails. Restore.

- [ ] **Step 9: Commit**

```bash
git add internal/adapter/nbd/
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "feat(nbd): WRITE, FUA, and FLUSH over the existing durability seam"
```

---

## Task 6: TRIM, WRITE_ZEROES, and read-only exports

**Files:**
- Modify: `internal/adapter/nbd/handlers_write.go`, `internal/adapter/nbd/connection.go`
- Test: `internal/adapter/nbd/connection_test.go`, `internal/adapter/nbd/integration_test.go`

**Interfaces:**
- Consumes: Task 4's `BlockStore`, Task 5's `handleWrite` structure.
- Produces:
  - `func (h *Handler) handleTrim(ctx context.Context, bs BlockStore, exp *Export, req *Request, conn net.Conn) error`
  - `func (h *Handler) handleWriteZeroes(ctx context.Context, bs BlockStore, exp *Export, req *Request, conn net.Conn) error`

- [ ] **Step 1: Write the failing tests**

```go
// storeFake needs a punches counter and a punchErr field before these tests can
// assert anything: the fake's PunchHole (connection_test.go:99) zero-fills and
// returns without recording that it ran, and Step 7's bite proof depends on
// that record existing. Add both in this task.
//
// Tests that expect Error == 0 on TRIM or WRITE_ZEROES need Task 5's
// writeVolumeFixture (integration_test.go:48) and its newHandler method
// (integration_test.go:69), which wire a real metadata service: the read-path
// helper passes nil for meta (connection_test.go:151), and the handler now
// calls h.meta.PunchHole. newWriteVolumeFixture(t).newHandler(t, st) is the
// call. Rejection tests may keep the nil-meta helper.

func TestConnection_TrimIsEPERMOnReadOnlyExport(t *testing.T) {
	// ErrEPERM. Note the export DOES advertise SEND_TRIM even when read-only:
	// handshake.go:101 sets it unconditionally and only adds READ_ONLY. So this
	// is not a client violating "MUST NOT send a trim request unless
	// NBD_FLAG_SEND_TRIM was set" — it is the case the spec's error list names
	// explicitly: "it SHOULD return NBD_EPERM if it receives a write or trim
	// request on a read-only export".
}

func TestConnection_TrimThenReadIsZeros(t *testing.T) {
	// Write 8192 non-zero bytes, TRIM [4096, 8192), read back: zeros in the
	// trimmed range, [0, 4096) untouched. Needs the real-meta fixture.
}

func TestConnection_TrimPastEndIsEINVAL(t *testing.T) {
	// Length runs past the export end → ErrEINVAL, refused not clamped. The
	// spec: "it SHOULD return NBD_EINVAL if it receives a read or trim request
	// including one or more sectors beyond the size of the device." READ
	// already answers EINVAL past the end (handlers_read.go:42), and the same
	// condition deserves the same answer.
}

func TestConnection_TrimOffsetPastEndIsEINVAL(t *testing.T) {
	// offset >= size with a small length. Asserted separately because it is the
	// case a subtraction-based clamp underflows on (see Step 3).
}

func TestConnection_WriteZeroesPastEndIsENOSPC(t *testing.T) {
	// ErrENOSPC, not EINVAL: the spec gives write zeroes its own rule — "The
	// server SHOULD return NBD_ENOSPC if it receives a write zeroes request
	// including one or more sectors beyond the size of the device."
}

func TestConnection_WriteZeroesWithNoHoleIsAccepted(t *testing.T) {
	// The spec MUSTs this: "The server MUST support the use of this flag if it
	// advertises NBD_FLAG_SEND_WRITE_ZEROES." Expect Error == 0 and
	// st.punches == 1 (PunchHole zero-overwrites rather than creating a hole).
}

func TestConnection_TrimZeroLengthIsEINVAL(t *testing.T) {
	// Length: 0 → ErrEINVAL, the same answer READ, WRITE and WRITE_ZEROES give.
}

func TestConnection_WriteZeroesZeroLengthIsEINVAL(t *testing.T) {
	// Length: 0 → ErrEINVAL, matching READ and WRITE. No payload either way, so
	// the connection must stay usable for the next command.
}

func TestConnection_WriteZeroesThenReadIsZeros(t *testing.T) {
	// The important one: WRITE_ZEROES must not materialize real chunks.
	// Assert st.punches == 1 and st.writes == 0 on the fake.
}

func TestConnection_WriteZeroesOnReadOnlyExportIsEPERM(t *testing.T) {
}

func TestConnection_ReadOnReadOnlyExportSucceeds(t *testing.T) {
	// The flag means writes are refused, not that the device is unusable.
}

func TestConnection_WriteOnReadOnlyExportIsEPERM(t *testing.T) {
	// Pins Task 5's handleWrite check 2. It lives here because the read-only
	// fixture is added in this task; the code under test is Task 5's.
}
```

These tests need a read-only export variant of the fixture: `testExport(size)`
(`connection_test.go:127`) returns `ReadOnly: false`. Add the flag as a parameter
or a second helper rather than mutating the returned `Export`, which other tests
share.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/adapter/nbd/ -run 'TestConnection_Trim|TestConnection_WriteZeroes|TestConnection_ReadOnReadOnly|TestConnection_WriteOnReadOnly' -v`
Expected: FAIL.

- [ ] **Step 3: Implement both handlers**

Both call the same two-step sequence the NFS DEALLOCATE handler uses (`internal/adapter/nfs/v4/handlers/deallocate.go:67`, `:91`):

```go
authCtx := h.authContext(ctx, exp)
res, err := h.meta.PunchHole(authCtx, exp.Handle, req.Offset, uint64(req.Length))
if err != nil {
    return h.writeReply(conn, req.Cookie, mapError(err), nil)
}
if _, err := bs.PunchHole(ctx, string(res.PayloadID), res.PreOpBlocks, req.Offset, uint64(req.Length)); err != nil {
    return h.writeReply(conn, req.Cookie, mapError(err), nil)
}
```

**`mapError` returns `NBDError`, which is not an `error`** — it is a `uint32` with no `Error()` method (`internal/adapter/nbd/protocol.go:76`), so `return mapError(err)` from a handler does not compile and, if it did, would send the client nothing and hang it. Every error on this path goes through `h.writeReply(conn, req.Cookie, mapError(err), nil)`, which is what the shipped `handleWrite` and `handleRead` do (`handlers_write.go:88`, `handlers_read.go:84`). Use that shape; there is no helper that turns a store error into a returned error, and adding one would put the reply in two places.

**The metadata punch is not optional, for two independent reasons.**

First, **without it the storage is never freed.** `engine.Store.PunchHole` does the reap — `DecrementRefCountAndReapMany` plus `ReprojectBlocks` — only inside `if len(currentBlocks) > 0` (`pkg/block/engine/readwrite.go:308`). Called with `nil`, it skips the reap entirely and only zero-overwrites the range, so reads return zeros while every CAS object stays referenced and the manifest keeps listing the chunks. The command would look like it worked and free nothing, which is the one thing a discard exists to do. `metadata.Service.PunchHole` (`pkg/metadata/sparse.go:48`) prunes `file.Blocks`, persists the new manifest, and returns `PreOpBlocks` for the block store to reap against.

Second, **without it the command bypasses authorization.** `metadata.Service.PunchHole` calls `checkWritePermission` (`pkg/metadata/sparse.go:80`); `engine.Store.PunchHole` checks nothing. WRITE goes through `PrepareWrite`, which checks. A TRIM that went straight to the engine would be the one mutating command in the adapter that no permission funnel sees — and it destroys data. This is CLAUDE.md invariant 2: every operation carries a `*metadata.AuthContext`.

Order of checks, and the order is the point:

1. `req.Flags &^ allowedFlags != 0` → `ErrEINVAL` (see the flag table below).
2. `exp.ReadOnly` → `ErrEPERM`.
3. `req.Length == 0` → `ErrEINVAL`.
4. `req.Offset > math.MaxUint64 - uint64(req.Length)` → `ErrEIO`, the same wrap guard `handleRead` uses and for the same reason.
5. `req.Offset >= exp.Size` → `ErrEINVAL` for TRIM, `ErrENOSPC` for WRITE_ZEROES.
6. `req.Offset + uint64(req.Length) > exp.Size` → the same two codes.
7. The store calls above.
8. If `req.Flags&FlagFUA != 0`, `h.commit(ctx, bs, exp)` **before** writing the reply, exactly as WRITE does.
9. Write the reply with no payload.

**Out of range is refused, not clamped — and a clamp is where the bug was.** An earlier draft of this task clamped with `end := min(offset+length, size); length := end - offset`. That **underflows** when `req.Offset > exp.Size`: `end` becomes `exp.Size`, and `exp.Size - req.Offset` wraps to a number near 2⁶⁴. Measured: `offset=10000, length=100, size=8192` → `18446744073709549808`. The engine's own overflow guard then turns it into a confusing `punch range overflow` error instead of the right answer. Checks 5 and 6 make the subtraction unnecessary, so there is no clamp to get wrong.

This also resolves an internal contradiction in the design spec. §4.4 said `TRIM`/`WRITE_ZEROES` are *"clamped to the export end"* while §4.6 and §13 both said a `TRIM` past the end is `EINVAL`. The spec's per-command error guidance is the more specific rule and wins: *"it SHOULD return `NBD_EINVAL` if it receives a read or trim request including one or more sectors beyond the size of the device"*, and for write zeroes *"The server SHOULD return `NBD_ENOSPC` if it receives a write zeroes request including one or more sectors beyond the size of the device."* Both spec sections are corrected in the same commit as this plan edit.

**TRIM over-delivers, deliberately.** The spec says a client *"MUST NOT make any assumptions about the contents of the export affected by this command"* and a server *"MAY discard length bytes starting at offset, but is not required to."* `PunchHole` zeroes the range anyway (`pkg/block/engine/readwrite.go:281`). That is the honest mapping: the alternative is a cheaper discard leaving reads non-deterministic. Put this in a `decision:` comment at the call site with the spec quote, because the next reader will otherwise "optimize" it.

**WRITE_ZEROES must not become a zero-buffer `WriteAt`.** That is exactly the reference library's bug: on a CAS store it *materializes* zero chunks, so a filesystem's discard path allocates storage instead of freeing it. The test asserts `st.punches == 1` and `st.writes == 0`, and that assertion is the guard.

Flag validation differs by command: `TRIM` accepts `FUA` only; `WRITE_ZEROES` accepts `FUA` and `NO_HOLE`. Any other bit → `ErrEINVAL`, for the same reason as `WRITE` — the flag is not documented as applicable to that request. FUA on either is honored through the same `h.commit` path as WRITE.

**`NO_HOLE` on `WRITE_ZEROES` must be accepted, not rejected.** The spec: *"The server MUST support the use of this flag if it advertises `NBD_FLAG_SEND_WRITE_ZEROES`"*, and we do. It is the client asking that the range be genuinely allocated rather than left sparse. `PunchHole` already zero-overwrites the range rather than creating a hole (`pkg/block/engine/readwrite.go:281`), so honoring it requires no extra work — but the handler must not refuse the flag, and a test must assert that. Rejecting it would violate a MUST on the strength of the same rule that makes `NO_HOLE` invalid on `WRITE`.

So the flag mask differs by command:

| Command | Accepted flags |
|---|---|
| `READ` | `FUA` (ignored) |
| `WRITE` | `FUA` |
| `FLUSH` | `FUA` |
| `TRIM` | `FUA` |
| `WRITE_ZEROES` | `FUA`, `NO_HOLE` |

- [ ] **Step 4: Wire the dispatch**

Add `CmdTrim` → `handleTrim`, `CmdWriteZeroes` → `handleWriteZeroes`. `CmdCache` (5) and `CmdBlockStatus` (7) stay unimplemented and answer `ErrEINVAL` — the spec's value for an unknown command.

- [ ] **Step 5: Land the `Timeouts.Idle` arm Task 4's committed loop is missing**

A fix to already-committed code, not new surface. It belongs here because this task already edits `connection.go` and already runs the package tests.

The loop at `internal/adapter/nbd/connection.go:203` arms the wait for the next request header from `h.timeouts.Read` unconditionally. The plan has mandated since commit `056b547f` that `Idle` bound that wait and `Read` bound finishing a request already started — the split NFS uses (`pkg/adapter/nfs/connection.go:312` / `:338`) and SMB uses (`pkg/adapter/smb/connection.go:230` / `:258`). That plan-side fix landed after Task 4 was committed, so the code never got it and `Idle` is dead: an operator who sets `Idle` below `Read` sees it silently ignored.

Measured with `Timeouts{Read: 5s, Idle: 200ms}` and a silent post-handshake connection: `serve still running after 1.5s`. At the defaults both are 5m, which is why nothing caught it.

Replace the arm with the block the plan specifies in Task 4 Step 3 (plan:1709-1715):

```go
// Idle bounds the wait for a header; Read bounds finishing a request that has
// already started (the payload read in handleWrite). At the defaults both are
// 5m, so the distinction only shows for an operator who separates them -- which
// is exactly when the field's own doc has to be true rather than nearly true.
//
// Idle == 0 means no idle timeout, so it cannot be folded into a min(): the
// guard is an independent arm, the same shape NFS uses at
// pkg/adapter/nfs/connection.go:312.
budget := h.timeouts.Read
if h.timeouts.Idle > 0 {
	budget = h.timeouts.Idle
}
if err := conn.SetReadDeadline(time.Now().Add(budget)); err != nil {
	return err
}
```

**And arm `Read` before the WRITE payload read**, the other half of the split, also missing: `handleWrite` reads `req.Length` bytes through `io.ReadFull` (`internal/adapter/nbd/handlers_write.go:69`) under whatever deadline the header read left behind, so without this `Read` bounds nothing and the payload read inherits the `Idle` budget.

The test that pins it must use `Idle` **shorter** than `Read`, or it passes either way:

```go
func TestConnection_IdleDeadlineClosesASilentConnection(t *testing.T) {
	// Timeouts{Read: 5 * time.Second, Idle: 200 * time.Millisecond}: complete
	// the handshake, then send nothing. serve must return within ~1s. With
	// Idle == Read or Idle > Read this passes whether or not the arm exists --
	// which is why the committed TestConnection_PartialHeaderTimesOut
	// (connection_test.go:696, Idle 5s / Read 200ms) cannot catch it.
}
```

**The second arm needs its own test, and the block above does not cover it.**
`TestConnection_IdleDeadlineClosesASilentConnection` pins the *header* wait; the
payload wait is a different arm in a different function, and nothing pins it.
Measured: deleting `conn.SetReadDeadline(time.Now().Add(h.timeouts.Read))` from
`handleWrite` (the arm before `io.ReadFull`) leaves **the whole package green** —
`go test ./internal/adapter/nbd/ -count=1` → `ok ... 4.725s`. The committed
`TestConnection_WriteTruncatedPayloadIsAnError` does not catch it because it uses
`testTimeouts()` (`Read 5s / Idle 5s`, so the two budgets are equal) **and closes
the client**, so the read returns on EOF and no deadline is ever consulted. Add:

```go
func TestConnection_PayloadReadIsBoundedByRead(t *testing.T) {
	// Timeouts{Read: 200 * time.Millisecond, Idle: 5 * time.Second} -- the
	// inverse of the header test, because this one must be bounded by Read.
	// Complete the handshake, send a WRITE header declaring 4096 bytes, send
	// 100 bytes of payload, then go silent WITHOUT closing: only the deadline
	// can end this, and it must be Read's. Assert serve returns non-nil within
	// ~1s and that st.writes == 0.
	//
	// With the arm removed the payload read inherits the header's Idle budget
	// (5s), so serve is still running at 1s and the test fails.
}
```

Leave the arm inside `if !h.draining()`, which is already correct: `initiateShutdown` sets a 100 ms deadline on every active connection (`pkg/adapter/base.go:447`) and a fresh `SetReadDeadline` replaces it, so arming the full budget while draining would hold the drain open for it.

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./internal/adapter/nbd/ -race -v`
Expected: PASS.

- [ ] **Step 7: Add the integration assertions**

```go
func TestIntegration_TrimFreesAndReadsZero(t *testing.T) {
	// Write 1 MiB, Flush, DrainAllUploads, then PunchHole through the handler
	// (h.meta.PunchHole then bs.PunchHole). Assert two things, in this order:
	//
	//   1. the punched range reads back as zeros;
	//   2. the manifest shrank — file.Blocks, read before and after through
	//      rt.GetMetadataService().GetFile(ctx, exp.Handle), has fewer entries
	//      and references fewer bytes.
}

func TestIntegration_WriteZeroesDoesNotMaterializeChunks(t *testing.T) {
	// Write 1 MiB, then WRITE_ZEROES over it. Same two assertions: reads
	// zeros, and the manifest shrank rather than growing.
}
```

**Do NOT assert that `bs.DataExtents` stops reporting the punched range. That assertion is unsatisfiable — it was measured, not reasoned.** Two structural reasons:

1. **The punch writes a record, so the local tier keeps answering "data here".** `engine.Store.PunchHole` zero-overwrites the range through the local append log (`pkg/block/engine/readwrite.go:281`) — that is the mechanism that guarantees reads return zeros regardless of rollup state. The side effect is that the local tier now holds an interval covering the punched range.
2. **`DataExtents` over-reports on purpose.** Its doc says *"The map may over-report data but must never under-report it"* (`pkg/block/engine/dataextents.go:41`), because NFSv4.2 SEEK and READ_PLUS callers must never skip real data. Reporting a hole where a punch happened is exactly the over-report the polarity permits — a client that copies zeros loses nothing.

Measured against the real fixture: 1 MiB written and 256 KiB punched → `DataExtents` still `[[0 1048576]]`; a punch aligned to a real chunk extent over 4 MiB of random data → still `[[0 4194304]]`; while the same punches took `file.Blocks` from **4 to 3** and the referenced bytes from **4194304 to 3095584**, and the punched range read back as zeros.

**So the manifest is the observable.** `metadata.Service.PunchHole` prunes `file.Blocks` and persists the new manifest; that pruning is the reclaim, and `GetFile` is how a test sees it. The engine's own reclaim test asserts the same thing the same way (`pkg/block/engine/api_blockref_test.go:270`, which checks the returned `kept` list and the coordinator's reap calls, never the extent map).

**One more trap in the same step:** `Flush` alone is not enough to populate the manifest. With a durable local store it returns before the remote drain (`pkg/block/engine/flush.go:554`), so `file.Blocks` is still empty and the punch has nothing to prune — a test written that way passes for the wrong reason. Call `bs.DrainAllUploads(ctx)` after `Flush` and assert `len(file.Blocks) > 0` before punching, exactly as Task 4's eviction test does.

- [ ] **Step 8: Verify the WRITE_ZEROES test actually bites**

Change `handleWriteZeroes` to call `bs.WriteAt(ctx, string(exp.PayloadID), nil, make([]byte, req.Length), req.Offset)` instead of the punch pair. Run `go test ./internal/adapter/nbd/ -run 'TestConnection_WriteZeroesThenReadIsZeros|TestIntegration_WriteZeroesDoesNotMaterializeChunks' -v` and confirm BOTH fail: the unit test on `st.punches == 1` / `st.writes == 0`, and the integration test on the manifest growing instead of shrinking. Restore both.

- [ ] **Step 9: Run the affected suites**

Run: `go build ./... && go test -race ./internal/adapter/nbd/... ./pkg/adapter/nbd/... ./internal/adapter/common/...`
Expected: clean.

**Do not run `make test-unit`** (`go test -race ./...`) and read a timeout as a failure: five packages — `internal/adapter/nfs/v3/handlers`, `internal/adapter/smb/handlers`, `pkg/block/chunker`, `pkg/block/engine`, `pkg/block/middleware/encryption` — hit the 10m deadline on this machine from SQLite contention under `-race`, reproducible on a clean base and unrelated to this work. They were verified environmental during Task 3.

- [ ] **Step 10: Commit**

```bash
git add internal/adapter/nbd/
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "feat(nbd): TRIM and WRITE_ZEROES via PunchHole"
```

---

## Task 7: Differential oracle against `qemu-nbd`

**Files:**
- Create: `test/nbd/run-oracle.sh`, `test/nbd/README.md`,
  `internal/adapter/nbd/oracle_test.go`
- Modify: `internal/adapter/nbd/handlers_control.go` (+ its tests) for the FLUSH
  metadata gap below. This is the one functional change in Task 7; everything
  else is test scaffolding. It is here rather than in a new task because the
  oracle is what makes the durability path observable, and a gap found while
  building the instrument should be closed with it.
- Modify: nothing else. `test/required-tests.json` is deliberately **not** touched:
  `test/required-tests.sh` runs `go test` for every name in the manifest, and
  the Tier A oracle needs a QEMU install, so listing it there would make the
  required set fail on every machine without one. A QEMU-dependent test is a
  manual regression check, not a required one — the manifest's own purpose is
  the opposite: names whose removal must be loud. (If Tier A is ever split so
  the QEMU-free half can run alone, that half can be listed.)

### FLUSH commits the block store but not the metadata — found while auditing this task

`handleFlush` (`internal/adapter/nbd/handlers_control.go`) calls only
`h.commit` → `common.CommitBlockStore`, the **block store** half of the barrier.
The metadata half is missing, and the sibling protocols all do both:
`common.FlushStableWrite` (`internal/adapter/common/write_payload.go:201`) is
exactly `CommitBlockStore` **then** `metaSvc.FlushPendingWriteForFile`, and it is
what NFSv3 WRITE (`internal/adapter/nfs/v3/handlers/write.go:299`), NFSv4 WRITE
(`internal/adapter/nfs/v4/handlers/write.go:238`), NFSv3 COMMIT
(`internal/adapter/nfs/v3/handlers/commit.go:257-277`), NFSv4 COMMIT
(`internal/adapter/nfs/v4/handlers/commit.go:105-119`) and SMB CLOSE/FLUSH
(`internal/adapter/smb/handlers/close.go:242-272`) call. Neither the plan nor the
spec mentioned `FlushStableWrite` or `FlushPendingWriteForFile` anywhere —
`grep` returns nothing in both documents — so this is an omission in the design,
not an implementer error.

**Measured, not inferred.** With deferred commits on (the default:
`pkg/metadata/service.go:131` `s.deferredCommit.Store(true)`), after
`PrepareWrite` + `CommitWrite` + `h.commit` the raw metadata store's mtime is
**unchanged**; calling `FlushPendingWriteForFile(ctx, handle, false)` afterwards
returns `flushed=true` and **does** change it. So the pending size/mtime state
survives an NBD FLUSH that the client was told succeeded.

**Why this is Minor rather than Important, stated so the severity is auditable
rather than asserted:**
- **The size can never be wrong.** NBD's `handleWrite` calls
  `PrepareWrite(authCtx, exp.Handle, req.Offset+uint64(req.Length))`
  (`internal/adapter/nbd/handlers_write.go:95`) and refuses anything past
  `exp.Size` first (`:61`), so `MaxSize` ≤ the size already stored; nothing in the
  NBD adapter calls `SetFileAttributes` or any other size setter. The flush is
  also max-only (`pkg/metadata/io.go:474` `if size > file.Size`), so it cannot
  shrink the export either.
- **The mtime is not observable over NBD.** The dispatch has five arms —
  `CmdRead`, `CmdWrite`, `CmdFlush`, `CmdTrim`, `CmdWriteZeroes`
  (`internal/adapter/nbd/connection.go:261-281`) — and no metadata command, so a
  client cannot read the mtime back. It is also not consumed by the block layer
  or the GC.
- **The state is bounded and not lost.** `RecordWrite` keeps one state per
  handle (`pkg/metadata/pending_writes.go:83-92`), so the pending set is bounded
  by the number of volume files rather than by write count, and the shutdown path
  flushes it.

**RULING: add the metadata flush.** `commit` becomes `FlushStableWrite`, so
NBD's barrier is the same barrier the other adapters use. The alternative —
keeping `CommitBlockStore` alone and documenting why NBD is different — was
rejected because NBD is not different in any way that matters here: an NBD
device is a boot path (Task 8 boots a kernel from it, and a kernel issues
`FLUSH`), the command's own spec text is an unconditional durability barrier,
and the cost of the fix is one type widening plus one body change. The
omit-and-document option would have to argue that a client told "durable" need
not actually be durable, which is exactly the class of bug the residency-truth
gate (spec §7) exists to prevent.

What was *not* acceptable is the state before this ruling: the omission was
invisible. The code comment on `commit` says the durability rule *"has exactly
one implementation across the adapters instead of one per protocol"*, which was
true of the block-store half and silent about the other half.

**The fix, and why the type widening is provably safe.**
`FlushStableWrite`'s body already passes `blockStore` to `CommitBlockStore`,
whose parameter is `Committer` (`internal/adapter/common/write_payload.go:125`),
so the concrete `*engine.Store` must already satisfy the interface — widening the
declared parameter changes no call site and no behaviour. NFS keeps passing a
concrete `*engine.Store`.

**Interfaces:**
- Consumes: a DittoFS NBD server listening on a real TCP port. The script does not start one; it takes a port.
- Produces: `test/nbd/run-oracle.sh` — exits nonzero on any divergence.

This is the independent check on a hand-rolled codec. `qemu-nbd` is a known-good NBD server; the same client commands run against both, and the byte sequences and negotiated flags are diffed.

**The oracle needs a server on a real port, and nothing in this repo starts one.**
DittoFS serves adapters from the control plane, not from a config file:
`pkg/config/config.go:37` says *"Dynamic configuration (users, groups, shares,
stores, adapters) is managed through the REST API and stored in the control
plane database"*, and `pkg/adapter/smb/config.go`'s `Enabled` is set `true`
unconditionally by the adapter factory (`cmd/dfs/commands/start.go:967`), so
there is no "off" a config file could express. A live run therefore needs
`dfs start` plus `dfsctl store metadata add`, `dfsctl store block add`,
`dfsctl share create`, and `dfsctl adapter enable nbd` — the sequence
`test/scenarios/setup.sh:73-81` already performs for smb and nfs, needing only
an `nbd` line. (It does need one non-obvious config value: the NBD bind gate
refuses a wildcard bind without `allow_remote_bind`, and the adapter factory
never sets `BindAddress`, so the adapter must be created with
`dfsctl adapter edit nbd --config '{"bind_address":"127.0.0.1"}'`.)

**But a live server cannot serve this oracle, for a reason that is not
incidental.** `RuntimeExporter.Resolve` (`internal/adapter/nbd/volume.go`)
resolves a volume by `Lookup`ing the share root, then `volumes/`, then the
volume name — and **no command in the repo creates a volume file**:
`grep -rn "volume" cmd/dfsctl/` returns nothing, and
`internal/adapter/nbd/volume.go:21` records that *"the operator, not the
protocol, decides who owns the volumes"*, so the operator creates
`volumes/<name>` through the share's own NFS/SMB mount. And the volume is a
**file inside a share**, so the byte-level oracle also needs an fs-backed local
tier, which is a second piece of production configuration rather than a test
fixture. A script that stood all of that up would be testing share
provisioning, not the codec, and would be unusable in CI.

**So the oracle is split, and the split is the point.**

- **Tier A — the codec, in-process, no QEMU and no provisioning.** A Go test
  that serves the real `Handler` on a real TCP listener and runs the real
  `qemu-io` against it, comparing against `qemu-nbd`. This is where the value
  is: `qemu-io` is a real independent NBD client, which is exactly what a
  hand-rolled codec needs to be checked against, and the whole codec is
  reachable without the control plane because `internal/adapter/nbd`'s
  `Handler` is built from an `Exporter`, a `StoreResolver`, a
  `*metadata.Service`, an `Identity` and `Timeouts` — all of which a test can
  supply. `pkg/adapter/nbd/adapter_test.go:88` already serves a real listener
  and `internal/adapter/nbd/handshake_test.go:50` already does a real TCP
  handshake, so neither the listener nor the wire path is new machinery.
  `test/nbd/run-oracle.sh` remains the driver: it checks QEMU is present, runs
  `go test` for Tier A, and reports.
- **Tier B — the deployed path, when someone has a server.** The same script
  accepts an already-running DittoFS NBD port via an environment variable or a
  flag, and skips Tier B with a printed reason when it is absent. It must not
  *discover* a server or silently fall back to Tier A: the script's output has
  to say which tier ran, or a green run means nothing in particular.

The flag-parity check (Step 2) belongs to **both** tiers, because it needs only
a handshake and no data path.

- [ ] **Step 0: Close the FLUSH metadata gap (its own commit)**

Do this first and commit it alone, so the functional change is reviewable
independently of the test scaffolding that follows.

1. Widen `FlushStableWrite`'s `blockStore` parameter from `*engine.Store` to
   `common.Committer` (`internal/adapter/common/write_payload.go:79-84`).
   Type-only; the body uses `CommitBlockStore`, which already takes `Committer`.
2. In `internal/adapter/nbd/handlers_control.go`, change **`commit`** — not
   `handleFlush` alone — to call
   `common.FlushStableWrite(h.authContext(ctx, exp), h.meta, bs, exp.Handle, exp.PayloadID, true)`
   in place of `common.CommitBlockStore(ctx, bs, exp.PayloadID)`. `commit` is
   already the single barrier both call sites share, so changing it there fixes
   FLUSH *and* FUA with one edit, and keeps the "one implementation" property
   the code comment claims. Building the `AuthContext` inside `commit` from the
   `ctx` it already receives is correct for both callers: `handleWrite` passes
   the same command context it used for `PrepareWrite`, and
   `h.authContext(ctx, exp)` is pure.

   Then correct `commit`'s doc comment, which currently says the durability rule
   *"has exactly one implementation across the adapters instead of one per
   protocol"* while implementing only the block-store half — the sentence
   becomes true once `FlushStableWrite` is the body, and the
   `decision:` paragraph about serialization (which is what makes a FLUSH a
   barrier over already-acked writes) still holds and should stay.
3. Add a test that reads the raw store before and after a FLUSH and requires the
   mtime to move. Build the fixture with `newIntegrationFixtureAs(t, integrationIdentity)`
   — with the wrong identity `PrepareWrite` fails `AccessDenied` and the test
   never reaches the barrier.

**This breaks the four `connection_test.go` FLUSH tests, and the failure is a
panic rather than an assertion.** `newTestHandler` (`connection_test.go:177`)
passes `nil` for the metadata service, and `FlushPendingWriteForFile` dereferences
it: measured, a nil `*metadata.Service` panics with `runtime error: invalid
memory address or nil pointer dereference`. The four affected tests are
`TestConnection_FlushIsAckedWhenDurable` (`:1119`),
`TestConnection_FlushIsEIOWhenNotDurable` (`:1139`),
`TestConnection_FlushWithNonZeroLengthIsEINVAL` (`:1161`) and
`TestConnection_FlushOnReadOnlyExportIsPermitted` (`:1177`). Move them onto the
real fixture (`newWriteVolumeFixture`, whose `newHandler` passes
`f.rt.GetMetadataService()`), as Tasks 5 and 6 already did for the WRITE and
punch paths. The two that assert on `st.flushes` still work, because `storeFake`
is passed through `resolverFor`; the EINVAL case must additionally keep asserting
`st.flushes == 0`, which is what proves a malformed FLUSH never reaches the
barrier at all.

Expected: the new test fails before step 2 and passes after. If it passes before,
it is not testing the gap — check that `deferredCommit` is on (it is by default)
and that the read is from `rt.GetMetadataStoreForShare(integrationShare)`, not
from the service's cached path.

```bash
git add internal/adapter/common/write_payload.go internal/adapter/nbd/
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S \
  -m "fix(nbd): commit the metadata half of the FLUSH barrier"
```

- [ ] **Step 1: Write the script**

`test/nbd/run-oracle.sh`, following the existing `test/crash/*.sh` convention: a `#!/usr/bin/env bash` header with a comment block explaining what it proves, `set -euo pipefail`, a usage line, and a prerequisites check.

**Tier A is a Go test, so the script's job is narrow.**
`internal/adapter/nbd/oracle_test.go` (package `nbd`, internal — see the note below on why) does this:

1. Skip, not fail, when `qemu-nbd` or `qemu-io` is absent: `t.Skip` with the
   missing binary named, so `go test ./...` stays green on a machine without
   QEMU while the script — which checks first — reports the skip as a failure.
2. Build the same fixture Tasks 4–6 use: `testruntime.NewWithShareStore`, a
   metadata store, a share with `Enabled: true`, and the volume file. The store
   is an in-memory fake of the seven-method `BlockStore`, so the oracle
   compares **the codec and the handshake**, not the block layer — state that
   in the test's doc comment, because a reader who believes it covers
   `PunchHole` will trust it for something it never tested.
3. `net.Listen("tcp", "127.0.0.1:0")` and serve the real `Handler` on it with a
   real accept loop — `go NewConn(h, c).Serve(ctx)` per accepted connection —
   exporting the fixture's export name and taking the port from the listener,
   not from config (`BaseAdapter.Port()` returns the *configured* port,
   `pkg/adapter/base.go:598`). **Not `pipe(t)` and not `go h.serve(...)`:** the
   client is `qemu-io`, a separate process, so an in-process socket pair cannot
   be reached. `pipe`/`serveConn` remain the right tools for every other test in
   the package, which is why this is easy to get wrong by habit.
4. Start `qemu-nbd` as a subprocess against a `t.TempDir()` scratch file **of exactly the export's size**, with `-f raw -p <free port> -b 127.0.0.1 --persistent
   --discard=unmap`, and stop it in `t.Cleanup`.

   **The scratch file must be the export's size, and the export must be large enough for the sequence below.** The two sizes have to agree, because every command in Step 5 is bounds-checked against the export: DittoFS refuses a WRITE past the end with `NBD_ENOSPC` and a READ or TRIM past the end with `NBD_EINVAL`, while `qemu-nbd` against a larger file would succeed. A mismatch therefore shows up as a transcript divergence that names neither cause. The committed `newWriteVolumeFixture` builds a **1 MiB** volume (`internal/adapter/nbd/integration_test.go:142` sets `Size: 1 << 20`) and the sequence below reaches **9 MiB**, so Tier A cannot use that fixture's volume as-is: it needs its own fixture sized to the sequence, e.g. 16 MiB, built by the same helper with the size parameterised (`newIntegrationFixtureAs` already takes the identity; add the size beside it rather than writing a second provisioning block).
5. Run the identical `qemu-io -f raw -c '<cmd>'` sequence against both, capture
   stdout+stderr, and require the transcripts to match line for line. Addresses
   are `nbd://127.0.0.1:<port>/<export>` — see the export-name note below.

`test/nbd/run-oracle.sh` then only has to: refuse without `qemu-nbd`,
`qemu-img` and `qemu-io` (naming the missing binary); run
`go test ./internal/adapter/nbd/ -run TestOracle_ -v -count=1` and fail if it
skips; run the Python flag-parity handshake against both servers; and, when
`DITTOFS_NBD_PORT` is set, additionally drive Tier B. It must print which tier
ran — a green run that does not say what it checked means nothing.

**Exactly what Tier A constructs, because the pieces are not all reusable.**

```go
// Package nbd, NOT nbd_test, and that is a deliberate choice with a verified
// basis rather than an inheritance.
//
// The reason to be in-package is that this test needs pieces Tasks 4-6 already
// built: storeFake (a seven-method BlockStore fake whose PunchHole zeroes the
// range and whose ReadAt zero-fills past the written extent), resolverFor, and
// integrationIdentity. An external test package cannot see any of them and
// would have to redefine every one -- including a second BlockStore fake that
// would drift from the one the other tests use.
//
// It does NOT need pipe, sendRequest, readReplyHeader, completeHandshake or
// exporterFor: the client here is qemu-io, not a test goroutine, so the
// handshake and the framing are the client's business.
//
// The plan originally specified package nbd_test on the grounds that an
// in-package test cannot import internal/adapter/testruntime. That is FALSE:
// integration_test.go is package nbd and imports it at line 10, and
// `go test ./internal/adapter/nbd/ -run TestIntegration_FlushRequireDurableWithUnhealthyRemoteIsEIO`
// compiles and passes. There is no cycle either way -- `go list -deps
// ./internal/adapter/testruntime` contains no entry for dittofs/internal/adapter/nbd.
// So the constraint was imaginary, and in-package is strictly better here.
//
// What this test does NOT reuse is the fixture's VOLUME SIZE: the sequence
// below reaches 9 MiB and the committed fixture builds a 1 MiB volume, so Tier
// A provisions its own volume at the size the sequence needs. Add the sized
// helper BESIDE the existing one rather than changing its signature, which four
// other tests call:
//
//   func newIntegrationFixtureAs(t *testing.T, id Identity) (*runtime.Runtime, string, *engine.Store) {
//       return newIntegrationFixtureAsSized(t, id, 1<<20)   // unchanged behaviour
//   }
//   func newIntegrationFixtureAsSized(t *testing.T, id Identity, size uint64) (*runtime.Runtime, string, *engine.Store) {
//       // ... the existing body, with `Size: ptr(size)` in SetFileAttributes
//   }
rt, name, _ := newIntegrationFixtureAsSized(t, integrationIdentity, 16<<20)
exp, err := NewRuntimeExporter(rt, integrationIdentity).Resolve(context.Background(), name)

// The block layer is the in-package fake, pre-sized to the volume so its
// zero-fill read path covers the whole export: ReadAt zero-fills past the
// written extent, and a read beyond `len(data[id])` also zero-fills, so the
// never-written-range step below compares equal either way.
st := newStoreFake()

// The server must be on a REAL TCP PORT, because the client is `qemu-io` -- a
// separate process. `pipe(t)` and `go h.serve(ctx, server)` are the right tools
// for every other test in this package and the WRONG ones here: pipe's pair is
// an in-process loopback socket, so an external process can never reach it.
ln, err := net.Listen("tcp", "127.0.0.1:0")
port := ln.Addr().(*net.TCPAddr).Port   // from the listener, not from config:
                                        // BaseAdapter.Port() is the CONFIGURED
                                        // port (pkg/adapter/base.go:598).

// The exporter must be the REAL one, for a reason that is not stylistic --
// see the export-name paragraph below.
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

// qemu-io addresses it as nbd://127.0.0.1:<port>/export/boot -- the ONE-slash
// form, which the client turns into the wire name `export/boot`. That is why
// the real exporter is required above; see the paragraph after this fence.
```

**The exporter must be the real `NewRuntimeExporter`, NOT `exporterFor(exp)`,
and this is a correctness requirement rather than a preference.** `qemu-io`
sends the export name with one leading `/` stripped — `nbd://127.0.0.1:<port>/export/boot`
becomes the wire name `export/boot` (see the export-name paragraph below).
`exporterFor` builds a `fakeExporter`, whose `Resolve` is an **exact map
lookup** keyed on `testExportName` = `"/export/boot"` (`handshake_test.go:18-23`,
`connection_test.go:135`). Measured: `fakeExporter.Resolve(ctx, "export/boot")`
returns `nbd: unknown export` while `Resolve(ctx, "/export/boot")` succeeds. So
a Tier A built on `exporterFor` would fail the handshake on the very first
`qemu-io` command with an unknown-export error that has nothing to do with the
codec — and it would look like a codec bug. The real exporter folds the share
half through `metadata.NormalizeShareName` (`pkg/metadata/validation.go:176`),
which re-prepends the slash, so it accepts the spelling the client actually
sends. This is exactly the fold the export-name paragraph pins, and it is worth
having the oracle exercise it rather than fake around it.

The metadata service is real and unavoidable: `handleTrim` and
`handleWriteZeroes` call `h.meta.PunchHole`, which panics on a nil service —
the same reason Tasks 5 and 6 needed `writeVolumeFixture`. The block store stays
the `storeFake` (`resolverFor(st)`), so the oracle compares the codec and the
handshake against a fake block layer; `storeFake.PunchHole` does zero the range
(`connection_test.go:112-116`) and `ReadAt` zero-fills past the written extent,
which is what makes the `discard` and `write -z` steps comparable. Say this in
the test's doc comment, because a reader who believes it covers `PunchHole` will
trust it for something it never tested.

What it does:

1. Refuse to run without `qemu-nbd`, `qemu-img`, and `qemu-io`; name the missing binary in the error.
2. Create a scratch file of a known size (e.g. 64 MiB) and serve it with
   `qemu-nbd -f raw -p <oracle-port> -b 127.0.0.1 --persistent --discard=unmap`
   in the background.

   **`--discard=unmap` is not optional, and leaving it out makes the oracle lie.**
   `qemu-nbd` starts with `int flags = BDRV_O_RDWR` (`qemu-nbd.c:589`) and
   `BDRV_O_UNMAP` is a separate bit (`include/block/block-common.h:246`,
   *"execute guest UNMAP/TRIM operations"*) that only `--discard=unmap` sets —
   `bdrv_parse_discard_flags` clears it and re-sets it only for `on`/`unmap`
   (`block.c:1154`). Without it a `NBD_CMD_TRIM` reaches the block layer and is
   dropped: qemu-nbd replies **success** and the data is still there. Since
   DittoFS's `PunchHole` genuinely zeroes the range, the `discard` step below
   would then diverge on the *oracle's* configuration and be reported as a
   codec bug — the most expensive kind of false positive, because the natural
   fix is to make DittoFS stop zeroing. A server that drops a discard is legal
   (*"A server MAY discard length bytes starting at offset, but is not required
   to"*), so this is not a qemu-nbd defect; it is a configuration the comparison
   has to pin.

   `-b 127.0.0.1` is written explicitly rather than relied on: the oracle is a
   throwaway server holding a scratch file, and it has no business listening on
   anything but loopback. `--persistent` (`-t`, *"don't exit on the last
   connection"*) keeps it alive across the several `qemu-io` invocations.
3. Against `qemu-nbd` and against DittoFS, run the same `qemu-io` command sequence and capture both transcripts:
   - `qemu-io -f raw -c 'write -P 0x11 0 1M'`
   - `qemu-io -f raw -c 'write -P 0x22 4M 64k'`
   - `qemu-io -f raw -c 'read -P 0x11 0 1M'`
   - `qemu-io -f raw -c 'discard 1M 1M'` then `read -P 0 1M 1M`
   - `qemu-io -f raw -c 'write -z 8M 1M'` then `read -P 0 8M 1M`
   - a read of a never-written range
   - `qemu-io -f raw -c 'flush'`
4. `qemu-img info` against both, and diff the reported size and format.
5. Diff the two transcripts. Any line that differs is a failure, with both transcripts printed.

**Export-name spelling, and why it is worth pinning.** QEMU turns a URI path into
the export name by stripping exactly one leading `/` (`nbd_parse_uri`,
`qemu-nbd.c:1563-1569`: `p = g_uri_get_path(uri); if (p[0] == '/') { p++; }`). So
`nbd://127.0.0.1:<port>/export/boot` sends the export name `export/boot`. That
resolves against DittoFS because `RuntimeExporter.Resolve` folds the share half
with `metadata.NormalizeShareName` (`pkg/metadata/validation.go:176`), which
re-prepends the slash — so both `export/boot` and `/export/boot` reach the same
volume. Write the single-slash form in the script and say in a comment that the
fold is what makes it work, because a reader who sees the volume listed as
`/export/boot` will otherwise "fix" the URI to `//export/boot` and get the same
result for a different reason.

- [ ] **Step 2: Add the flag-parity check**

The oracle's most valuable assertion is not the data path — it is the flag set:

```bash
# qemu-nbd advertises its own flag set. Ours is deliberately narrower (no
# CAN_MULTI_CONN, no SEND_CACHE, no SEND_DF). What must match is the flags we
# DO advertise, because those are the ones a client acts on without verifying.
```

Capture the negotiated transmission flags for both servers and assert that every flag DittoFS advertises is also advertised by `qemu-nbd`, and specifically that `SEND_FLUSH` and `SEND_FUA` are present on both. `libnbd`'s `nbdinfo --list` does this in one command but **is not in Homebrew core** (`brew search --formula libnbd` returns only fuzzy matches; `nbdkit` likewise), so it cannot be the only path. The portable path is a 20-line Python handshake in the script — read the greeting, send `NBD_FLAG_C_FIXED_NEWSTYLE`, send `NBD_OPT_GO` for the export, and decode the `NBD_INFO_EXPORT` reply's 16-bit flags — which needs nothing but a Python 3 interpreter. **Use the Python path as the primary**, because a script that silently degrades when a tool is missing is a script whose check nobody notices has stopped running. If `nbdinfo` is present, run it too and print its output for the record; do not make the assertion depend on it.

- [ ] **Step 3: Write `test/nbd/README.md`**

Prerequisites (`brew install qemu` on macOS; the Linux packages on a Linux host), what each of the three scripts proves, and — explicitly — that the oracle validates the *codec and flags* while the KVM harness (Task 8) validates the *boot path*. State that the oracle needs no root, no VM and no share provisioning, so it is the cheap regression check, and state which tier needs what: Tier A needs QEMU only; Tier B needs a running server with an NBD adapter and a provisioned volume, and is skipped without `DITTOFS_NBD_PORT`.

- [ ] **Step 4: Run the oracle**

Run: `test/nbd/run-oracle.sh`
Expected: exit 0, Tier A transcripts identical, flag parity asserted for both servers.

**QEMU is not installed on this machine.** `command -v qemu-nbd qemu-img qemu-io` returns nothing, and `qemu-system-aarch64` is absent too, so Task 7 and Task 8 both need `brew install qemu` before they can run. If it is not installed when you get here, do not skip the step and do not write a script you never ran: report the missing prerequisite and say exactly which steps are unverified. A script whose failure path was never executed is a script that has never run.

If it diverges, the divergence is the finding: a hand-rolled codec's first real client is where a transposed field shows up. Diagnose against the spec before changing anything.

- [ ] **Step 5: Commit**

```bash
git add test/nbd/ internal/adapter/nbd/oracle_test.go
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "test(nbd): differential oracle against qemu-nbd"
```

---

## Task 8: KVM boot harness and crash rig

**Files:**
- Create: `test/nbd/boot-qemu.sh`, `test/nbd/crash-restart.sh`
- Modify: `test/nbd/README.md`

**Interfaces:**
- Consumes: a DittoFS server with the NBD adapter, `qemu-system-*`, a kernel and initramfs, root.
- Produces: two scripts whose exit status is the verdict.

**This task cannot run on the development machine** (macOS arm64; it needs a Linux host with KVM, root, and `nbd-client` in the guest initramfs). The deliverable is the harness plus a recorded run on a Linux host. The scripts must fail loudly and specifically when their prerequisites are absent rather than silently skipping.

- [ ] **Step 1: Write `boot-qemu.sh`**

Topology, exactly as the spec's §9.5:

```
local (small, static):  kernel + initramfs        <- firmware boots this; no NBD needed
initramfs:              modprobe nbd
                        nbd-client 10.0.2.2 10809 /dev/nbd0
                        mount /dev/nbd0p1 /root; switch_root
NBD (DittoFS):          the root filesystem
```

- **No data disk.** The root filesystem arrives over NBD. A `-drive` holding the volume would test the wrong path.
- **No `hostfwd` rule.** `hostfwd` forwards a *host* port *into the guest*, which is the opposite of what this topology needs and would also try to bind host port 10809 where DittoFS is already listening. The guest initiates the connection outward to `10.0.2.2`, QEMU's user-net gateway to the host, and a guest-initiated outbound connection crosses SLIRP with no rule — the same path the guest uses to reach the outside world.
- **No UEFI-boot-from-NBD.** EDK2 has no NBD driver; the boot path is kernel+initramfs from a local disk, then root-on-NBD via `switch_root`.
- The script must build the initramfs with the `nbd` module and `nbd-client`, or refuse with a message naming what is missing.
- **The recovery environment must not be on NBD.** The script must not run `fsck` from inside the guest: the guest's root is the volume under test, so an `fsck` there is judging a crash it is also recovering from. The check belongs on the host, against the volume read as a file over NFS — which is the B1 property (spec §2.2) earning its place.
- Record which cache is in the path, in the script's own output: in this topology the guest's kernel NBD client speaks TCP directly to DittoFS, so there is no QEMU block layer, and a `-drive`'s `cache=` mode is irrelevant. Print the guest page-cache state and the share's `CommitAck` value so the run is self-describing.

Reference launch (from the spec):

```
qemu-system-aarch64 -M virt -cpu host -m 2G \
  -drive if=virtio,file=boot.img,format=raw \
  -netdev user,id=n0 -device virtio-net,netdev=n0
```

- [ ] **Step 2: Write `crash-restart.sh`**

```
guest: write + fsync, keep writing
host:  SIGKILL the dfs process mid-flight
host:  restart dfs, same port, same export
guest: I/O resumes without a filesystem error; fsck clean (on the host)
```

Two harness requirements the spec calls non-negotiable, both of which the script must enforce rather than document:

1. **A clean `umount` is not evidence.** The script must SIGKILL the server; it must refuse to pass if the guest unmounted cleanly, because that never exercises the barrier.
2. **Restart inside the dead-connection window.** The kernel's `dead_conn_timeout` (commit `560bc4b399`, `NBD_ATTR_DEAD_CONN_TIMEOUT`) pauses in-flight requests rather than failing them — but a window over ~120 s trips the hung-task detector, so the script must measure and print the actual restart time and fail if it exceeds the configured window.

`-timeout` and `-persist` are **not** this mechanism — `-timeout` sets `NBD_SET_TIMEOUT` (per-request) and `-persist` is `nbd-client`'s own reconnect loop. The script must configure `dead_conn_timeout` explicitly and say so, because conflating the three is a documented real-world failure.

**How to configure it, exactly, and the trap in the obvious spelling.**

- The flag is `-T <seconds>` / `--dead-timeout <seconds>` (`args.c:76`, handled at `:199`). It is **not documented in `nbd-client`'s man page** — `grep -i dead man/nbd-client.8.sgml.in` finds only "deadlock" — so a reader will not discover it from `man nbd-client`. Name the flag in the script's comment and in the README so the harness is self-explanatory.
- It reaches the kernel **only through netlink**, as `NBD_ATTR_DEAD_CONN_TIMEOUT` (`nbd-client.c:185-186`). On the ioctl path the option is parsed, stored, and then **never read**: the only other references to `dead_conn_timeout` in `nbd-client.c` are inside the netlink send. With `-nonetlink`/`-L`, or on a kernel built without netlink support, the script would print a configured window that the kernel never received — the same class of silent failure the whole design exists to eliminate, one layer down.
- **The script must therefore assert the mechanism took effect, not just that it passed the flag.** `-nonetlink`/`-L` is not to be used, and the script must fail if netlink is unavailable rather than falling back. This is checkable before the crash test even starts: `nbd-client` refuses at `nbd-client.c:394 check_conn` when the netlink family will not resolve.
- **`-persist` alone already gives 30 seconds** (`nbd-client.c:188`: `else if (cur_client->persist) NLA_PUT_U64(msg, NBD_ATTR_DEAD_CONN_TIMEOUT, 30); /* Default 30 seconds for persist mode */`). So the default is already inside the hung-task window. The script should still set `-T` explicitly and print the value, because a default that happens to be safe is not a configured one — but it must not *depend* on `-persist` being what supplied the window.
- The kernel side landed in 2017 (`560bc4b39952ed77cdb0000992e9415b0ee89edb`, "nbd: handle dead connections"), so any kernel in support today has it; the requirement to check is `nbd-client`'s netlink support, not the kernel's.

- [ ] **Step 3: Extend `test/nbd/README.md`**

Add the boot and crash sections: prerequisites, the exact `dead_conn_timeout` value used, how to read the verdict, and the honest statement that these require a Linux host with root and cannot run in CI on macOS.

- [ ] **Step 4: Syntax-check both scripts**

Run: `bash -n test/nbd/boot-qemu.sh && bash -n test/nbd/crash-restart.sh`
Expected: no output.

Run: `shellcheck test/nbd/*.sh` if available; fix what it reports.

- [ ] **Step 5: Verify each script refuses cleanly without prerequisites**

Run both on a host lacking QEMU/root and confirm each exits nonzero naming the missing prerequisite — not with a partial run, and not with exit 0.

- [ ] **Step 6: Record a run on a Linux host**

This is the step that produces the actual evidence, and it is the point at which the design is falsified or confirmed. Record the transcript in the README's results section: the `dead_conn_timeout` value, the measured restart time, the guest's I/O behaviour, and the host-side `fsck` verdict.

Per the spec's §10: **Phase 1 is the first moment the design is falsifiable end-to-end, and it is the right place to stop and reassess if the boot path turns out to need something the protocol surface does not provide.** If it does, stop and report rather than extending the surface ad hoc.

- [ ] **Step 7: Commit**

```bash
git add test/nbd/
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "test(nbd): KVM boot harness and crash-restart rig"
```

---

## Verification Checklist

Run in order; each becomes meaningful at the task named.

- [ ] Task 1 — `go test ./internal/adapter/nbd/ -run TestMagicValues` passes, and fails when a magic is transposed.
- [ ] Task 2 — `TestHandshake_GoNameLengthExceedsOptionData` and `TestHandshake_HugeOptionLengthRejected` pass, and fail when the length checks are removed.
- [ ] Task 2 — `TestHandshake_GoReturnsExportInfoThenAck` asserts `SEND_FLUSH` present and `CAN_MULTI_CONN` absent.
- [ ] Task 2 — `TestHandshake_UnsupportedOptionIsRefused` asserts `NBD_OPT_STRUCTURED_REPLY` is refused with `NBD_REP_ERR_UNSUP`, and no `NBD_FLAG_BLOCK_STATUS_PAYLOAD` is advertised anywhere (the residency-truth gate, spec §7).
- [ ] Task 2 — a restart serves the same export name and size: `TestRuntimeExporter_ResolvesVolumeFile` run twice against the same share returns identical `Size` and `PayloadID` (spec §5.2 obligations 1–2).
- [ ] Task 3 — `TestServe_RefusesNonLoopbackBindWithoutOptIn` passes for `""`, `0.0.0.0`, `::`, and a routable address.
- [ ] Task 3 — `models.DefaultPort("nbd") == 10809` and `AdapterConfig{Type: "nbd"}.Validate()` is nil.
- [ ] Task 3 — `go build ./... && make test-unit` clean.
- [ ] Task 4 — `TestConnection_ReadAtExactEndIsServed` passes, and fails when the bounds check uses `>=`.
- [ ] Task 4 — `TestConnection_OffsetPlusLengthOverflowIsEIO` passes, and fails when the overflow check is removed.
- [ ] Task 4 — `TestConnection_ReadWithFUAFlagSucceeds` passes, and fails when `READ` rejects any non-zero flag word.
- [ ] Task 4 — `TestConnection_ReadZeroLengthIsEINVAL` and `TestConnection_UnimplementedCommandIsEINVAL` pass (the latter answers `EINVAL`, not `ENOTSUP`).
- [ ] Task 4 — `TestConnection_PartialHeaderTimesOut` passes.
- [ ] Task 4 — `TestConn_PanicInHandlerDoesNotKillTheProcess` passes, and fails (by crashing the test binary, not by an assertion) when the `recover` is removed from `Conn.Serve`.
- [ ] Task 4 — `TestConnection_FailedStoreResolutionDegradesAndCounts` passes, and `h.TakeDegraded()` returns 0 on the second call.
- [ ] Task 4 — the `handler` field is an `atomic.Pointer`, and `go test -race` reports no race between `Serve` and `Healthcheck` (drive a probe while a connection is being accepted).
- [ ] Task 5 — `TestConnection_WriteZeroLengthIsEINVAL` passes and the connection is still usable afterwards.
- [ ] Task 5 — `TestConnection_RejectedWriteKeepsTheStreamSynchronized` passes, and fails when the payload drain is removed.
- [ ] Task 5 — `TestConnection_WriteWithFUAIsDurableBeforeReply` passes, and fails when `commit` is moved after the reply.
- [ ] Task 5 — a WRITE refused by POSIX mode bits reaches the client as `ErrEPERM`, not `ErrEIO`. The metadata funnel returns `merrs.ErrAccessDenied` (`pkg/metadata/auth_permissions.go:510`, `:539`), a different code from `ErrPermissionDenied`; the test needs a real metadata service with the volume created as one identity and written as another, or it passes vacuously.
- [ ] Task 5 — `TestIntegration_FlushRequireDurableWithUnhealthyRemoteIsEIO` passes.
- [ ] Task 6 — `TestConnection_WriteZeroesThenReadIsZeros` asserts `st.punches == 1` and `st.writes == 0`, and fails when `WriteZeroes` is changed to a zero-buffer write.
- [ ] Task 6 — `TestConnection_IdleDeadlineClosesASilentConnection` fails when the `Idle` arm is removed, and only when `Idle` is shorter than `Read` (`Idle: 200ms` vs `Read: 5s`). Measured without the arm: `serve still running after 1.5s`.
- [ ] Task 6 — `TestConnection_PayloadReadIsBoundedByRead` fails when the payload arm is removed from `handleWrite` (`Read: 200ms` vs `Idle: 5s`, the inverse fixture). Measured after adding it: exactly one failure, at the test's 1s bound. The reason the test had to be written is the *other* measurement — **before it existed, deleting the same arm left the whole package green**, because the payload read then inherits the 5s header budget and `TestConnection_WriteTruncatedPayloadIsAnError` cannot catch it: equal budgets, and it closes the client, so EOF ends the read before any deadline is consulted.
- [ ] Task 6 — `TestIntegration_TrimFreesAndReadsZero` and `TestIntegration_WriteZeroesDoesNotMaterializeChunks` assert the manifest shrank (`file.Blocks` before/after, read through `GetFile`), not that `DataExtents` shrank. The extent-map assertion is unsatisfiable: `engine.PunchHole` zero-overwrites through the local append log, so the local tier keeps reporting the range, and `DataExtents` over-reports by documented contract.
- [ ] Task 6 — `TestConnection_WriteZeroesWithNoHoleIsAccepted` passes (`NO_HOLE` is a MUST we advertise) and `TestConnection_WriteWithNoHoleFlagIsEINVAL` passes (it is not a `WRITE` flag).
- [ ] Task 6 — `TestConnection_TrimZeroLengthIsEINVAL` and `TestConnection_WriteZeroesZeroLengthIsEINVAL` pin the uniform zero-length answer (`EINVAL`), and each is followed by a valid command on the same connection to prove the stream is still usable.
- [ ] Task 6 — `TestConnection_TrimPastEndIsEINVAL`, `TestConnection_TrimOffsetPastEndIsEINVAL`, and `TestConnection_WriteZeroesPastEndIsENOSPC` pin out-of-range refusal (not clamping), and `TestConnection_TrimIsEPERMOnReadOnlyExport` passes with `SEND_TRIM` still advertised.
- [ ] Task 6 — `go build ./... && go test -race ./internal/adapter/nbd/... ./pkg/adapter/nbd/... ./internal/adapter/common/...` clean (NOT `make test-unit`, which times out on five unrelated packages on this machine).
- [ ] Task 7 — `test/nbd/run-oracle.sh` exits 0 with identical transcripts against `qemu-nbd`, and the script passes `--discard=unmap` (without it the discard step diverges on the oracle's configuration, not on the codec).
- [ ] Task 7 (Step 0) — an NBD `FLUSH` persists the file's deferred metadata, not just the block store: reading `rt.GetMetadataStoreForShare(integrationShare)` before and after shows the mtime move. Measured before the fix: `h.commit` left the raw store's mtime **unchanged**, and `FlushPendingWriteForFile(ctx, handle, false)` then returned `flushed=true` and changed it. The four `connection_test.go` FLUSH tests move off `newTestHandler` (nil meta panics: `runtime error: invalid memory address or nil pointer dereference`) onto `newWriteVolumeFixture`, and the EINVAL case still asserts `st.flushes == 0`.
- [ ] Task 7 — the flag-parity check confirms every flag DittoFS advertises is one `qemu-nbd` also advertises.
- [ ] Task 7 — `internal/adapter/nbd/oracle_test.go` skips (not fails) without QEMU, and `run-oracle.sh` treats that skip as a failure; the script prints which tier ran, so a green run says what it checked.
- [ ] Task 8 — the launch line carries **no `hostfwd`**: it forwards host→guest, which is the wrong direction and would try to bind host port 10809 where DittoFS is listening. The guest reaches the host at `10.0.2.2` with no rule.
- [ ] Task 8 — `bash -n` clean on both scripts; each refuses cleanly without prerequisites.
- [ ] Task 8 — the crash rig sets `dead_conn_timeout` with `-T`/`--dead-timeout` and fails when netlink is unavailable, rather than silently running the ioctl path where the option is parsed and ignored.
- [ ] Task 8 — `mkfs.ext4` + mount + write + `sync` + unmount + remount in a KVM guest.
- [ ] Task 8 — SIGKILL mid-write → restart → guest I/O resumes inside the dead-connection window; host-side `fsck` clean.
- [ ] The volume is readable as a plain file over NFS (the B1 property, spec §2.2).
- [ ] `docs/guide/cli.md` regenerated; `test/required-tests.sh` unaffected.
- [ ] No commit message names AI tooling or carries a `Co-Authored-By` trailer for it (`test/no-ai-attribution.sh`).
