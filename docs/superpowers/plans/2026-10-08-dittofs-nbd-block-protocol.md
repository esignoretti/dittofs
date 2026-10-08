# NBD Block-Protocol Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give DittoFS an NBD adapter so a KVM guest can boot from a DittoFS share as its root disk.

**Architecture:** A hand-rolled, scoped NBD server in `internal/adapter/nbd/` (wire + dispatch) behind an adapter in `pkg/adapter/nbd/` (lifecycle), following the existing NFS/SMB split. A volume is one metadata file at `volumes/<export-name>` inside a share, so no new control-plane surface is needed. Durability is not reimplemented: `FLUSH` and `FUA` route through the same `common.CommitBlockStore` that NFS COMMIT and SMB Flush already call.

**Tech Stack:** Go 1.26, `pkg/adapter` (BaseAdapter lifecycle), `pkg/controlplane/runtime` (share/block-store resolution), `pkg/metadata` (file operations + `AuthContext`), `pkg/block/engine` (payload I/O), QEMU/KVM + `nbd-client` for the boot test, `qemu-nbd` as a differential oracle.

**Spec:** `docs/superpowers/specs/2026-10-08-dittofs-nbd-block-protocol-design.md`

## Global Constraints

- **Target tree:** `origin/develop` @ `4dae5562`. All line citations below are verified against it.
- **Go module:** `github.com/marmos91/dittofs`, `go 1.26.0`.
- **No new dependencies.** The codec is hand-rolled; the test client is `github.com/pojntfx/go-nbd`'s `pkg/client` only if it is already in `go.mod` — if it is not, integration tests drive the codec through `net.Pipe` with the in-repo encoder, and the differential oracle (Task 7) provides the independent check.
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
6. **A rejected `WRITE` whose payload is still in flight.** The client always sends `length` bytes after a `WRITE` header — the spec states it unconditionally, with no exemption for requests the server intends to refuse. A reasonable person expects a rejected write to leave the connection usable; the failure mode is that the unconsumed payload is parsed as the next request header, and the client sees a protocol error on some later, unrelated command. Highest-value item in the list because the symptom is remote from the cause. The drain must use `io.CopyN(io.Discard, ...)`, never `make([]byte, length)`, or a declared 4 GiB length becomes a 4 GiB allocation.

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
| `internal/adapter/nbd/integration_test.go` | Real `engine.Store`, real bytes, end to end over `net.Pipe`. |
| `pkg/adapter/nbd/adapter_test.go` | Lifecycle, config validation, bind gate. |
| `pkg/adapter/nbd/healthcheck_test.go` | Status mapping. |

**Created — harness (Task 8), not Go:**

| File | Covers |
|---|---|
| `test/nbd/run-oracle.sh` | Differential comparison against `qemu-nbd`. |
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

Create `internal/adapter/nbd/handshake_test.go`. Drive both sides over `net.Pipe`.

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
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

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
	client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHandshake did not return after the client closed")
	}
}

func TestHandshake_GoReturnsExportInfoThenAck(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

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
	sendClientFlags(t, client, 1) // NBD_FLAG_C_FIXED_NEWSTYLE
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
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

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
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

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
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

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
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

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
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

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
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

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
	server, client := net.Pipe()
	defer client.Close()

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
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

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
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name: share, MetadataStore: "nbd-meta", BlockStoreID: bsID,
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
}
```

Add one test helper in this file: `func ptr[T any](v T) *T { return &v }`.

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
- Consumes: Task 2's `ServeHandshake`, `RuntimeExporter`, `Exporter`; Task 4's `Handler`, `StoreResolver`, `Identity`, `Timeouts` (so Task 3's `Serve` is written against interfaces Task 4 provides — the two tasks touch different files and the signature is fixed here).
- Produces:
  - `type TimeoutsConfig struct { Read, Write, Idle, Shutdown time.Duration }`
  - `type Config struct { Enabled bool; Port int; BindAddress string; MaxConnections int; MaxMessageSize int; Timeouts TimeoutsConfig; AllowRemoteBind bool; DefaultExport string; UID uint32; GID uint32 }`
  - `func (c *Config) timeouts() nbd.Timeouts` — adapts `TimeoutsConfig` (four fields) to `nbd.Timeouts` (three), dropping `Shutdown` which the base adapter owns. It is unexported because only the adapter's own `Serve` calls it, and it is the *only* place the two timeout vocabularies meet.
  - `const DefaultPort = 10809`
  - `func (a *Adapter) Handler() *nbd.Handler` — exported only because the tests are external (`package nbd_test`); it is how `TestServe_ClientDisconnectDoesNotStopTheAdapter` and Task 4's integration tests reach the wired handler. Document it as test-facing.
  - `func (c *Config) applyDefaults()`, `func (c *Config) validate() error`
  - `type Adapter struct { *adapter.BaseAdapter; config Config; ... }`
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

// Serve needs a runtime once it gets past the gate, so the listener tests use
// the real fixture. This also exercises SetRuntime, which nothing else does.
func serveWithRuntime(t *testing.T, cfg nbd.Config) (*nbd.Adapter, chan error) {
	t.Helper()
	rt, _ := testruntime.NewWithShareStore(t)
	a := nbd.New(cfg)
	a.SetRuntime(rt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after context cancellation")
		}
	})
	return a, done
}

func TestServe_AllowsLoopbackBind(t *testing.T) {
	for _, bind := range []string{"127.0.0.1", "::1", "localhost"} {
		t.Run(bind, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.BindAddress = bind
			cfg.Port = 0 // ephemeral: the test must not need a fixed free port
			a, done := serveWithRuntime(t, cfg)
			select {
			case <-a.ListenerReady():
			case err := <-done:
				t.Fatalf("Serve returned before the listener was ready: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("listener was never ready")
			}
			if a.Port() == 0 {
				t.Fatal("Port() is 0 after ListenerReady; the adapter did not publish the bound port")
			}
		})
	}
}

func TestServe_AllowsNonLoopbackWithOptIn(t *testing.T) {
	cfg := minimalConfig()
	cfg.BindAddress = "0.0.0.0"
	cfg.AllowRemoteBind = true
	cfg.Port = 0
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
func TestServe_ClientDisconnectDoesNotStopTheAdapter(t *testing.T) {
	cfg := minimalConfig()
	cfg.Port = 0
	a := nbd.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	select {
	case <-a.ListenerReady():
	case <-time.After(5 * time.Second):
		t.Fatal("listener was never ready")
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(a.Port()))
	for range 3 {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		// Read the greeting, then hang up mid-handshake.
		buf := make([]byte, 18)
		_, _ = io.ReadFull(conn, buf)
		conn.Close()
	}

	select {
	case err := <-done:
		t.Fatalf("Serve returned after client disconnects: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	<-done
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

Add the missing imports (`io`, `strconv`) at the top.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./pkg/adapter/nbd/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement `config.go`**

Mirror `pkg/adapter/smb/config.go:63` and its `applyDefaults` at `:311`. Defaults: `Port` 10809; `Timeouts.Read` 5m, `Write` 30s, `Idle` 5m, `Shutdown` 30s; `MaxMessageSize` 32 MiB (`32 << 20` — the maximum block size we advertise); `UID`/`GID` `65534` (nobody/nogroup, the unprivileged default the SMB synthetic-identity path uses, `internal/adapter/smb/handlers/auth_helper.go:74-95`).

`validate()` returns an error when `Port < 0 || Port > 65535`. `New` panics on a `validate()` error, matching `pkg/adapter/smb/adapter.go:176`.

Also add the accessor Step 5b uses:

```go
// timeouts narrows the adapter's four configured durations to the three the
// wire layer needs. Shutdown is deliberately not here: the base adapter owns
// the drain deadline, and the handler's job during drain is to answer
// ErrESHUTDOWN, not to run its own timer.
func (c *Config) timeouts() nbd.Timeouts {
	return nbd.Timeouts{Read: c.Timeouts.Read, Write: c.Timeouts.Write, Idle: c.Timeouts.Idle}
}
```

- [ ] **Step 4: Implement `adapter.go`**

`New` calls `applyDefaults()`, `validate()` (panic on error), then `adapter.NewBaseAdapter(adapter.BaseConfig{BindAddress, Port, MaxConnections, ShutdownTimeout: Timeouts.Shutdown}, "nbd")`.

`SetRuntime(rt any)` type-asserts to `*runtime.Runtime` and builds the `RuntimeExporter`.

`Serve(ctx)`:
1. **The bind gate, before anything binds.** `if netutil.IsNonLoopbackHost(a.config.BindAddress) && !a.config.AllowRemoteBind { return fmt.Errorf(...) }`. The error text must contain `allow_remote_bind`. `netutil.IsNonLoopbackHost` (`pkg/netutil/netutil.go:15`) returns false for `127.0.0.1`, `::1`, `[::1]`, `localhost`, and **true for an empty string** (it falls through the switch, `net.ParseIP("")` is nil, and the non-IP branch assumes off-host). That is the behaviour the gate wants — an empty `BindAddress` binds all interfaces, so it must be refused without the opt-in — and it needs no special case. Say so in a comment, because the next reader will not check the fall-through.
2. `return a.ServeWithFactory(ctx, a, nil, nil)`.

`NewConnection(conn net.Conn) adapter.ConnectionHandler` returns `nbd.NewConn(a.handler, conn)` — the wrapper from Task 4, which exists because `adapter.ConnectionHandler.Serve` takes only a context. In this task that type does not exist yet, so the stub returns a minimal type implementing `Serve(ctx context.Context)` that reads the greeting and closes, letting the lifecycle tests pass; **Task 4 replaces it** with the real wrapper. (Task 4 lists `pkg/adapter/nbd/adapter.go` under Modify for exactly this reason — the wiring step is written here because the adapter is the only place that can do it, and it lands with the handler it wires.)

`Stop` delegates to `a.BaseAdapter.Stop(ctx)`.

- [ ] **Step 5: Implement `healthcheck.go`**

```go
func (a *Adapter) Healthcheck(ctx context.Context) health.Report
```

`!a.config.Enabled` → `StatusDisabled` with message `"NBD adapter is disabled in config"`. Otherwise delegate to `a.BaseAdapter.Healthcheck(ctx)`. `StatusDegraded` is added in Task 4, when there is a resolvable-export failure to report — until then there is nothing that can degrade.

Note `BaseAdapter.Healthcheck` already reports `StatusUnhealthy` once shutdown is initiated but `Serve` has not returned (`pkg/adapter/healthcheck.go:39`), so the drain path needs no help here.

- [ ] **Step 5b: Wire the adapter to the wire layer (replaces Step 4's `Serve` body)**

The adapter is the only place that knows about both halves — the runtime and the wire layer — so this is where the two seams meet. It must import `internal/adapter/nbd`, which is legal (both are in-repo and `internal/adapter/nbd` does not import `pkg/adapter/nbd`, so there is no cycle).

```go
type Adapter struct {
	*adapter.BaseAdapter
	config  Config
	runtime *runtime.Runtime
	handler *nbd.Handler      // built lazily in Serve, once the runtime is set
}

func (a *Adapter) SetRuntime(rt any) {
	r, ok := rt.(*runtime.Runtime)
	if !ok {
		panic(fmt.Sprintf("nbd: SetRuntime wants *runtime.Runtime, got %T", rt))
	}
	a.runtime = r
}

func (a *Adapter) Serve(ctx context.Context) error {
	if a.runtime == nil {
		return errors.New("nbd: SetRuntime was not called before Serve")
	}
	exporter := nbd.NewRuntimeExporter(a.runtime, nbd.Identity{UID: a.config.UID, GID: a.config.GID})
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
	a.handler = nbd.NewHandler(exporter, resolver, a.runtime.GetMetadataService(),
		nbd.Identity{UID: a.config.UID, GID: a.config.GID}, a.config.timeouts(), draining)
	return a.ServeWithFactory(ctx, a, nil, nil)
}

func (a *Adapter) NewConnection(conn net.Conn) adapter.ConnectionHandler {
	return a.handler
}
```

Three things this shape gets right and one it makes explicit:

- **`NewConnection` returns a small per-connection wrapper, not the shared handler.** `adapter.ConnectionHandler.Serve` takes only a context (`pkg/adapter/base.go:38`), so the socket must be bound at construction — `return nbd.NewConn(a.handler, conn)`, the same shape as `pkg/adapter/smb/adapter.go:717`. The handler itself is shared and stateless; `Conn` carries nothing but the socket and the handler pointer, so there is no per-connection protocol state to leak.
- **The resolver returns `*engine.Store` as `nbd.BlockStore` implicitly** — `common.ResolveForWrite` returns the concrete type, which satisfies the interface. No adapter.
- **`Serve` builds the handler once, before the listener exists**, so a `nil` runtime or a bad config fails as a startup error rather than as a failed first connection.
- **`SetRuntime` panics on the wrong type**, matching the documented contract of `pkg/adapter/base.go:185` (*"must be a `*runtime.Runtime` or it panics"*) and the constructor-panics-on-invalid-config precedent at `pkg/adapter/smb/adapter.go:176`.
- **`preAccept` stays `nil`** — see Step 4's note; there is nothing checkable at the TCP layer for a protocol that names its export inside the handshake.

`Config` therefore needs `UID`/`GID` fields (the synthetic identity, spec §4.7) and a `timeouts() nbd.Timeouts` accessor. Add both in Step 3.

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

- [ ] **Step 8: Regenerate the CLI docs**

Run: `go run ./cmd/dfs docs --output docs/guide/cli.md` (check the exact invocation in CLAUDE.md's "Frequent commands" before running).
Expected: `docs/guide/cli.md` updated; `git diff --stat` shows it.

- [ ] **Step 9: Run the full unit suite and build**

Run: `go build ./... && make test-unit`
Expected: both clean. `make test-unit` is `go test -race ./...` — the lifecycle tests must be race-clean.

- [ ] **Step 10: Commit**

```bash
git add pkg/adapter/nbd/ pkg/controlplane/models/adapter.go cmd/dfs/commands/start.go docs/guide/cli.md
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "feat(nbd): adapter lifecycle, control-plane registration, bind gate"
```

---

## Task 4: Connection loop and READ

**Files:**
- Create: `internal/adapter/nbd/connection.go`, `internal/adapter/nbd/handlers_read.go`
- Modify: `internal/adapter/common/write_payload.go` (`CommitBlockStore` parameter widened to the new `Committer` interface — a type-only change, no behaviour change; every existing call site passes `*engine.Store` and keeps compiling), `internal/adapter/common/normalize.go` (`normalizeBlockStoreError` exported as `NormalizeBlockStoreError`), `internal/adapter/common/{doc.go,read_payload.go,write_payload.go}`, `internal/adapter/nfs/v4/handlers/content_errmap_wiring_test.go` (the rename's seven call sites — mechanical, but the build breaks until all of them land together), `pkg/adapter/nbd/adapter.go` (the `NewConnection` stub from Task 3's Step 4 is replaced here with `return nbd.NewConn(a.handler, conn)`), `pkg/adapter/nbd/healthcheck.go` (add `Degraded`)

**Task 3 must land first**, because both tasks modify `pkg/adapter/nbd/adapter.go`. They touch different functions — Task 3 writes the stub, this task replaces its body — so the second edit is a one-line substitution rather than a conflict.
- Test: `internal/adapter/nbd/connection_test.go`, `internal/adapter/nbd/integration_test.go`, `internal/adapter/common/write_payload_test.go` (only if one does not already cover `CommitBlockStore`)

**Interfaces:**
- Consumes: Task 2's `Export`/`Exporter`, Task 3's `Config` (timeouts).
- Produces:
  - `type Timeouts struct { Read, Write, Idle time.Duration }` — the subset of the adapter's config the wire layer needs, so `internal/adapter/nbd` does not import `pkg/adapter/nbd`. `Idle` bounds a connection that has completed a handshake but sends nothing; `Read` bounds a single request header or payload, so it must be the *shorter* of the two or a stalled client is held open by the wrong timer.
  - `type Identity struct { UID, GID uint32 }` — the configured synthetic identity (spec §4.7).
  - `type StoreResolver func(ctx context.Context, exp *Export) (BlockStore, error)` — a function type rather than a method on `Handler`, so the dispatch tests can inject a fake store. The production implementation wraps `common.ResolveForWrite` (below), so `internal/adapter/nbd` never imports `pkg/controlplane/runtime`.
  - `type BlockStore interface { ReadAt(...); WriteAt(...); PunchHole(...); Flush(...); LocalDurable() bool; RemoteDurable() bool; RequireDurableCommit() bool }` — the four I/O methods plus the three durability accessors, so `commit` can call `common.CommitBlockStore` without the concrete `*engine.Store`. `*engine.Store` satisfies all seven (`pkg/block/engine/readwrite.go:21`, `:87`, `:286`; `pkg/block/engine/flush.go:534`; `pkg/block/engine/engine.go:388`, `:397`, `:406`).
  - `type Handler struct { exporter Exporter; stores StoreResolver; meta *metadata.Service; id Identity; timeouts Timeouts; draining func() bool; degraded atomic.Int64 }`
  - `func NewHandler(exporter Exporter, stores StoreResolver, meta *metadata.Service, id Identity, timeouts Timeouts, draining func() bool) *Handler` — `draining` is a closure over `a.BaseAdapter.Shutdown` from the adapter in Task 3; passing it as a function rather than the base adapter keeps `internal/adapter/nbd` free of any `pkg/adapter` import.
  - `func (h *Handler) serve(ctx context.Context, conn net.Conn) error` — unexported, because it is the test entry point, not the framework's. `type Conn struct { handler *Handler; conn net.Conn }` plus `func NewConn(h *Handler, conn net.Conn) *Conn` and `func (c *Conn) Serve(ctx context.Context)` satisfy `adapter.ConnectionHandler` (`pkg/adapter/base.go:38`, `Serve(ctx)` with no `net.Conn`); the pattern matches `pkg/adapter/smb/connection.go:64`.
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

func (c *Conn) Serve(ctx context.Context) {
	if err := c.handler.serve(ctx, c.conn); err != nil && ctx.Err() == nil {
		logger.Debug("NBD connection ended", "remote", c.conn.RemoteAddr(), "error", err)
	}
}
```

`Adapter.NewConnection` (`pkg/adapter/nbd/adapter.go`) becomes `return nbd.NewConn(a.handler, conn)`. The unexported `func (h *Handler) serve(ctx context.Context, conn net.Conn) error` is what the dispatch tests call directly, so they keep the `error` return and never need the wrapper.

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

`storeFake` must also implement the three durability accessors — returning `true, true, false` for the happy path, so `commit` takes the `localDurable` fast path. The tests that need `ErrNotDurableYet` set them to `false, false, true`. `Handler` takes a `StoreResolver`, and the production resolver is built over **`common.ResolveForWrite`** — do not write a new resolution path.

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

Tests:

```go
func TestConnection_ReadServesBytes(t *testing.T)
func TestConnection_ReadServesZerosForUnwrittenRange(t *testing.T)
func TestConnection_UnknownCommandIsEINVAL(t *testing.T)
func TestConnection_UnimplementedCommandIsEINVAL(t *testing.T)
func TestConnection_WriteIsEINVALBeforeTask5(t *testing.T) // replaced in Task 5
func TestConnection_ErrorsCarryTheCookie(t *testing.T)
func TestConnection_DiscClosesWithoutReply(t *testing.T)
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

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run TestConnection -v`
Expected: FAIL — `undefined: NewHandler`, `undefined: BlockStore`.

- [ ] **Step 3: Implement `connection.go`**

`serve(ctx, conn)`:
1. `exp, err := ServeHandshake(ctx, conn, h.exporter)`. On error, return it — the handshake already told the client why where the protocol has a way to.
2. Resolve the block store once: `bs, err := h.stores(ctx, exp)`. A failure here is `Degraded` territory (`h.degraded.Add(1)`), not a wire error — the connection ends, because every subsequent command would fail the same way.
3. **Loop, one command at a time.** Set `conn.SetReadDeadline(time.Now().Add(h.timeouts.Read))` before each `ReadRequest`; a deadline error returns a timeout error from `Serve`, closing the connection. On `ErrBadMagic` or any decode error, return it.
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

**The handler's own read deadline fights the base adapter's shutdown, and the handler has to lose that fight deliberately.** `initiateShutdown` calls `interruptBlockingReads` (`pkg/adapter/base.go:447`), which sets a **100 ms** read deadline on every active connection to unblock pending reads. But the handler sets its own deadline before each read (`SetReadDeadline(time.Now().Add(h.timeouts.Read))` — 5 minutes by default), and a `SetReadDeadline` **replaces** the previous one. So a handler that finishes a command after shutdown began will overwrite the 100 ms interruption with a fresh 5-minute deadline, and `waitForDrain` will sit until `ShutdownTimeout` (30 s) expires and force-close.

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
- `context.Canceled`, `context.DeadlineExceeded` → `ErrESHUTDOWN`
- default → `ErrEIO`

**Order matters where the causes overlap.** `engine.ErrStoreClosed` and the `merrs` pair are tested before the `context` pair, because a store failure wrapped in a cancellation should report the store failure — the operator needs to know the store is gone, not that a context expired. `errors.Is` walks the whole wrap chain, so a single `err` can match several cases; the first match wins.

`mapError` never returns `ErrEOVERFLOW` or `ErrENOTSUP`; both are unreachable by design (see the Global Constraints entry). If a future handler wants one, it must also add the advertisement that makes it legal to send.

Use `errors.Is` throughout, and put the original error in a log line at Debug for expected classes and Error otherwise, per CLAUDE.md invariant 6.

- [ ] **Step 6: Add `Degraded` to the healthcheck**

Add a counter the connection path increments on a failed export resolution or block-store resolution, and have `Healthcheck` report `StatusDegraded` when it is nonzero since the last probe. Message: `"NBD adapter is serving but N export resolutions have failed"`. Keep the probe cheap — read the counter and reset it, no I/O.

The counter lives on `Handler` (`h.degraded`), but `Healthcheck` is on `Adapter` in the other package, so the adapter needs a read-and-reset accessor: `func (h *Handler) TakeDegraded() int64` using `h.degraded.Swap(0)`. `Swap` rather than `Load` then `Store` — two steps let a concurrent increment vanish between them. `Adapter.Healthcheck` calls it only when `a.handler != nil` (before `Serve` there is nothing to ask):

```go
func (a *Adapter) Healthcheck(ctx context.Context) health.Report {
	if !a.config.Enabled {
		return health.Report{Status: health.StatusDisabled, Message: "NBD adapter is disabled in config", CheckedAt: time.Now().UTC()}
	}
	if a.handler != nil {
		if n := a.handler.TakeDegraded(); n > 0 {
			return health.Report{Status: health.StatusDegraded, Message: fmt.Sprintf("NBD adapter is serving but %d export resolutions have failed", n), CheckedAt: time.Now().UTC()}
		}
	}
	return a.BaseAdapter.Healthcheck(ctx)
}
```

The `Disabled` branch and the final delegation are the same shape as `pkg/adapter/nfs/healthcheck.go`; only the `Degraded` branch is new, and it must come **before** the delegation because `BaseAdapter.Healthcheck` has no way to know about a counter it does not own. `TakeDegraded` is called on every probe, so the count is "since the last probe" — say so in the field's doc, because a reader will otherwise assume it is cumulative.

- [ ] **Step 7: Write the integration test**

Create `internal/adapter/nbd/integration_test.go`. This is the first test over a **real** `engine.Store`, and it is where the durability contract gets pinned.

```go
func newIntegrationFixture(t *testing.T) (*runtime.Runtime, string, *engine.Store)
```

Use `testruntime.NewWithShareStore(t)` (`internal/adapter/testruntime/runtime.go:53`), register a metadata store, `AddShare` with `BlockStoreID`, create `volumes/boot`, set its size, and return the runtime, the export name, and `rt.GetBlockStoreForShare(share)`. The journal root comes from `testruntime.New`, so the local tier is an fs store — which is what makes `LocalDurable()` true and the FLUSH assertions below meaningful.

```go
func TestIntegration_ReadAfterWriteOverRealStore(t *testing.T) {
	// 64 KiB of deterministic non-zero bytes written through the handler's
	// WRITE path (or directly via bs.WriteAt while Task 5 is pending), then
	// read back through the handler's READ path; compare byte-exact.
}

func TestIntegration_ReadEvictedButManifestCoveredRange(t *testing.T) {
	// Write, Flush, evict the local range, then READ. Expect real bytes, not
	// zeros and not an error: this is the hydrate path
	// (pkg/block/engine/read_internal.go:56).
}

func TestIntegration_ReadNeverWrittenRangeIsZeros(t *testing.T) {
	// A range inside the export with no write ever. Expect 4096 zeros and
	// Error == 0.
}
```

- [ ] **Step 8: Run the tests and confirm they pass**

Run: `go test ./internal/adapter/nbd/ -race -v`
Expected: PASS.

- [ ] **Step 9: Verify the bounds checks actually bite**

Comment out the overflow check in `handleRead`, run `go test ./internal/adapter/nbd/ -run TestConnection_OffsetPlusLengthOverflowIsEIO -v`, and confirm it fails on the assertion. Restore. Repeat for the exact-end check: change `>` to `>=` and confirm `TestConnection_ReadAtExactEndIsServed` fails.

- [ ] **Step 10: Commit**

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
    return mapError(err)
}
```

**`mapError` is the single place the wire layer converts an error, and it calls `common.NormalizeBlockStoreError` internally** — so the handler passes the raw error and needs no explicit normalize call. Normalizing first matters: the engine returns raw sentinels as well as `*merrs.StoreError`, and normalization is what turns a raw one into a `*merrs.StoreError` with the right code while leaving an already-wrapped one alone (`if goerrors.As(err, &storeErr) { return err }`). The failure mode of getting this wrong is an error code that silently degrades to `ErrEIO` because `errors.Is` no longer matches through an extra wrap — so a disk-full becomes an I/O error and the client has no way to know its device is full.

`normalizeBlockStoreError` (`internal/adapter/common/normalize.go:25`) is currently unexported. **Export it in this task** (`NormalizeBlockStoreError`, a one-word change) rather than writing a second classifier: one implementation of the classification, or the NBD path drifts from NFS and SMB. Do not hand-roll a classifier that skips normalization.

- [ ] **Step 1: Write the failing tests**

Replace `TestConnection_WriteIsEINVALBeforeTask5` with the real cases:

```go
func TestConnection_WritePersistsBytes(t *testing.T) {
	// WRITE 4096 bytes at offset 8192; expect Error == 0, and a following READ
	// of [8192, 12288) returns them byte-exact.
}

func TestConnection_WritePastEndIsENOSPC(t *testing.T) {
	// Offset: size-4096, Length: 4097 → ErrENOSPC, NOT EINVAL. The spec:
	// "The server SHOULD return NBD_ENOSPC if it receives a write request
	// including one or more sectors beyond the size of the device." A read or
	// trim past the end is EINVAL; a write is ENOSPC. The export size is fixed,
	// so a WRITE past the end is refused, never grown.
}

func TestConnection_WriteZeroLengthIsEINVAL(t *testing.T) {
	// Length: 0 → ErrEINVAL. There is no payload to drain, so the connection
	// stays usable: a following READ on the same connection must succeed.
}

// The desync guard. A rejected WRITE still has its payload in flight; if the
// handler replies without consuming it, the next "request header" is the
// payload's first bytes and the connection is garbage from then on.
func TestConnection_RejectedWriteKeepsTheStreamSynchronized(t *testing.T) {
	// Send a WRITE that is rejected on the range check (Length: 4096, offset
	// past the end) WITH its 4096-byte payload, then send a valid READ and
	// expect the correct reply. Without the drain, the READ's reply is a
	// parse of payload bytes.
}

func TestConnection_RejectedHugeWriteDoesNotAllocate(t *testing.T) {
	// Declare Length: 1<<30 on a 1 MiB export, send a little of the payload,
	// then stall. Expect ErrENOSPC (the range check precedes the read) and no
	// 1 GiB allocation. Uses Timeouts.Read to bound the drain.
}

func TestConnection_WriteTruncatedPayloadIsAnError(t *testing.T) {
	// Declare Length: 4096, send 100 bytes, then close. Serve must return an
	// error and must NOT report success.
}

func TestConnection_WriteWithFUAIsDurableBeforeReply(t *testing.T) {
	// A fake whose Flush records the order: assert Flush happened before the
	// reply was written. This is the assertion that makes FUA mean something.
}

func TestConnection_FlushIsAckedWhenDurable(t *testing.T) {
	// CommitAck: journal semantics — local fs store, Flush returns
	// &block.FlushResult{Finalized: false}, nil. Expect Error == 0.
}

func TestConnection_FlushIsEIOWhenNotDurable(t *testing.T) {
	// requireDurable set, Flush returns {Finalized: false}, nil, and the store
	// reports localDurable false / remoteDurable false. Expect ErrEIO: FLUSH
	// promised durability and could not deliver it, so there is no honest
	// success to send.
}

func TestConnection_FlushWithNonZeroLengthIsEINVAL(t *testing.T) {
	// The spec requires length and offset to be zero.
}

func TestConnection_FlushOnReadOnlyExportIsPermitted(t *testing.T) {
	// A read-only export still answers FLUSH successfully. Refusing would make
	// a client error a device that has nothing to flush.
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/adapter/nbd/ -run 'TestConnection_Write|TestConnection_Flush' -v`
Expected: FAIL.

Add `TestConnection_RejectedWriteKeepsTheStreamSynchronized` to **Review Focus** item 6, which it pins — it is the highest-value test in this task, because the failure it catches is silent and remote (a client sees a protocol error minutes later on an unrelated command).

- [ ] **Step 3: Implement `handleWrite`**

Order of checks, and the order is the point:

1. `req.Flags &^ FlagFUA != 0` → `ErrEINVAL` (`NO_HOLE`, `DF`, `REQ_ONE`, `FAST_ZERO` are not applicable to `WRITE`; the spec names `EINVAL` for *"a request with a flag not explicitly documented as applicable to the given request"*).
2. `exp.ReadOnly` → `ErrEPERM`.
3. `req.Length == 0` → `ErrEINVAL`.
4. `req.Offset > math.MaxUint64 - uint64(req.Length)` → `ErrEIO`.
5. `req.Offset + uint64(req.Length) > exp.Size` → `ErrENOSPC` (**not** `EINVAL`). The spec: *"The server SHOULD return `NBD_ENOSPC` if it receives a write request including one or more sectors beyond the size of the device. ... It SHOULD return `NBD_EINVAL` if it receives a read or trim request including one or more sectors beyond the size of the device."* A write past the end means the client's view of the device is too large; a read or trim past the end is a malformed request. Different causes, different codes.
6. **Drain the payload on every error path from here on.** The client has already sent `req.Length` bytes — the spec says so unconditionally (*"The client MUST follow the request header with *length* number of bytes"*). If the handler writes an error reply and returns without consuming them, those bytes are read as the *next request header* and the connection desynchronizes into garbage. So every rejection after the header is parsed must discard the payload first:

   ```go
   // The payload is already in flight; skipping it would desync the stream.
   if _, err := io.CopyN(io.Discard, conn, int64(req.Length)); err != nil {
       return err // the connection is unusable either way
   }
   ```

   Use `io.CopyN(io.Discard, ...)` rather than `make([]byte, req.Length)`: a client that declares a 4 GiB length must cost the server a bounded buffer, not an allocation. This is the same class as Review Focus 1, one layer down.
7. **Then** allocate `data := make([]byte, req.Length)` and `io.ReadFull(conn, data)`. A truncated payload returns the read error — the connection is unusable at that point, so returning is correct and the client gets a TCP close rather than a success reply for bytes the server never saw.
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
func TestConnection_TrimIsEPERMOnReadOnlyExport(t *testing.T) {
	// A read-only export does not advertise SEND_TRIM, so a TRIM arriving on
	// one is a client bug. ErrEPERM, not EINVAL: the spec says "it SHOULD
	// return NBD_EPERM if it receives a write or trim request on a read-only
	// export" — a deliberate, client-actionable refusal rather than a
	// malformed-request verdict.
}

func TestConnection_TrimThenReadIsZeros(t *testing.T) {
	// Write 8192 non-zero bytes, TRIM [4096, 8192), read back: the trimmed
	// range is zeros and [0, 4096) is untouched.
}

func TestConnection_TrimPastEndIsClamped(t *testing.T) {
	// TRIM offset near the end with a length running past it: the operation
	// succeeds over the in-range part. Clamped, not refused — a discard that
	// covers the end of the device is a normal filesystem action.
}

func TestConnection_WriteZeroesWithNoHoleIsAccepted(t *testing.T) {
	// The spec MUSTs this: "The server MUST support the use of this flag if it
	// advertises NBD_FLAG_SEND_WRITE_ZEROES." Expect Error == 0, and PunchHole
	// still called (which zero-overwrites rather than creating a hole).
}

func TestConnection_TrimZeroLengthIsEINVAL(t *testing.T) {
	// Length: 0 → ErrEINVAL, the same answer READ, WRITE and WRITE_ZEROES give.
	// One rule for all four: the spec leaves zero-length behaviour unspecified
	// and no correct client sends one.
}

func TestConnection_WriteZeroesZeroLengthIsEINVAL(t *testing.T) {
	// Length: 0 → ErrEINVAL, matching READ and WRITE. No payload either way, so
	// the connection must stay usable for the next command.
}

func TestConnection_WriteZeroesThenReadIsZeros(t *testing.T) {
	// The important one: WRITE_ZEROES must not materialize real chunks.
	// Assert on the fake that PunchHole was called and WriteAt was NOT.
}

func TestConnection_WriteZeroesOnReadOnlyExportIsEPERM(t *testing.T) {
}

func TestConnection_ReadOnReadOnlyExportSucceeds(t *testing.T) {
	// The flag means writes are refused, not that the device is unusable.
}

func TestConnection_WriteOnReadOnlyExportIsEPERM(t *testing.T) {
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/adapter/nbd/ -run 'TestConnection_Trim|TestConnection_WriteZeroes|TestConnection_ReadOnReadOnly|TestConnection_WriteOnReadOnly' -v`
Expected: FAIL.

- [ ] **Step 3: Implement both handlers**

Both call `bs.PunchHole(ctx, string(exp.PayloadID), nil, req.Offset, length)`, where `length` is clamped:

```go
end := req.Offset + uint64(req.Length)
if end > exp.Size {
    end = exp.Size
}
length := end - req.Offset
```

**Zero-length is `ErrEINVAL` on both commands**, the same answer `READ` and `WRITE` give. The spec leaves zero-length behaviour unspecified for all four (*"the behavior of a server on such a request is unspecified although the server SHOULD NOT disconnect"*) and no correct client sends one, so one uniform rule beats a per-command table somebody has to remember. It also matches the answer every other malformed request gets. The zero-length check runs **before** the clamp below, so a request that is both zero-length and past the end reports `EINVAL` rather than silently becoming a no-op. `length == 0` also means there is no payload to drain on either command, so the connection must stay usable for the next one.

This is the same clamp the NFS DEALLOCATE handler uses (`internal/adapter/nfs/v4/handlers/deallocate.go:129`). Reuse the shape; do not import across `internal/adapter/nfs` and `internal/adapter/nbd` for a four-line function.

**TRIM over-delivers, deliberately.** The spec says a client *"MUST NOT make any assumptions about the contents of the export affected by this command"* and a server *"MAY discard length bytes starting at offset, but is not required to."* `PunchHole` zeroes the range anyway (`pkg/block/engine/readwrite.go:281`). That is the honest mapping: the alternative is a cheaper discard leaving reads non-deterministic. Put this in a `decision:` comment at the call site with the spec quote, because the next reader will otherwise "optimize" it.

**WRITE_ZEROES must not become a zero-buffer `WriteAt`.** That is exactly the reference library's bug: on a CAS store it *materializes* zero chunks, so a filesystem's discard path allocates storage instead of freeing it. The test asserts `PunchHole` was called and `WriteAt` was not, and that assertion is the guard.

Flag validation: `Flags &^ FlagFUA != 0` → `ErrEINVAL` for both, and for the same reason as `WRITE` — the flag is not documented as applicable to that request. FUA on TRIM/WRITE_ZEROES is honored through the same `h.commit` path as WRITE.

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

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/adapter/nbd/ -race -v`
Expected: PASS.

- [ ] **Step 6: Add the integration assertions**

```go
func TestIntegration_TrimFreesAndReadsZero(t *testing.T) {
	// Write 1 MiB, Flush, PunchHole a 256 KiB range through the handler, then
	// read it back: zeros. Then assert via bs.DataExtents that the trimmed
	// range is no longer reported as data.
}

func TestIntegration_WriteZeroesDoesNotMaterializeChunks(t *testing.T) {
	// Write 1 MiB, then WRITE_ZEROES over it. Assert the export still reads
	// zeros AND that DataExtents no longer reports the range — the second half
	// is what proves storage was freed rather than filled with zeros.
}
```

- [ ] **Step 7: Verify the WRITE_ZEROES test actually bites**

Change `handleWriteZeroes` to call `bs.WriteAt(ctx, string(exp.PayloadID), nil, make([]byte, req.Length), req.Offset)` instead of `PunchHole`. Run `go test ./internal/adapter/nbd/ -run TestConnection_WriteZeroesThenReadIsZeros -v` and confirm it fails on the "PunchHole was called" assertion. Restore.

- [ ] **Step 8: Run the full suite**

Run: `go build ./... && make test-unit`
Expected: clean.

- [ ] **Step 9: Commit**

```bash
git add internal/adapter/nbd/
git -c gpg.format=ssh -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "feat(nbd): TRIM and WRITE_ZEROES via PunchHole"
```

---

## Task 7: Differential oracle against `qemu-nbd`

**Files:**
- Create: `test/nbd/run-oracle.sh`, `test/nbd/README.md`
- Modify: `test/required-tests.sh` only if the script is added to the required list — check first; a script needing QEMU should not be a required test.

**Interfaces:**
- Consumes: the running DittoFS server with an NBD adapter.
- Produces: `test/nbd/run-oracle.sh` — exits nonzero on any divergence.

This is the independent check on a hand-rolled codec. `qemu-nbd` is a known-good NBD server; the same client commands run against both, and the byte sequences and negotiated flags are diffed.

- [ ] **Step 1: Write the script**

`test/nbd/run-oracle.sh`, following the existing `test/crash/*.sh` convention: a `#!/usr/bin/env bash` header with a comment block explaining what it proves, `set -euo pipefail`, a usage line, and a prerequisites check.

What it does:

1. Refuse to run without `qemu-nbd`, `qemu-img`, and `qemu-io`; name the missing binary in the error.
2. Create a scratch file of a known size (e.g. 64 MiB) and serve it with `qemu-nbd -f raw -p <oracle-port> --persistent` in the background.
3. Against `qemu-nbd` and against DittoFS's NBD port, run the same `qemu-io` command sequence and capture both transcripts:
   - `qemu-io -f raw -c 'write -P 0x11 0 1M'`
   - `qemu-io -f raw -c 'write -P 0x22 4M 64k'`
   - `qemu-io -f raw -c 'read -P 0x11 0 1M'`
   - `qemu-io -f raw -c 'discard 1M 1M'` then `read -P 0 1M 1M`
   - `qemu-io -f raw -c 'write -z 8M 1M'` then `read -P 0 8M 1M`
   - a read of a never-written range
   - `qemu-io -f raw -c 'flush'`
4. `qemu-img info` against both, and diff the reported size and format.
5. Diff the two transcripts. Any line that differs is a failure, with both transcripts printed.

- [ ] **Step 2: Add the flag-parity check**

The oracle's most valuable assertion is not the data path — it is the flag set:

```bash
# qemu-nbd advertises its own flag set. Ours is deliberately narrower (no
# CAN_MULTI_CONN, no SEND_CACHE, no SEND_DF). What must match is the flags we
# DO advertise, because those are the ones a client acts on without verifying.
```

Capture the negotiated transmission flags for both servers and assert that every flag DittoFS advertises is also advertised by `qemu-nbd`, and specifically that `SEND_FLUSH` and `SEND_FUA` are present on both. `libnbd`'s `nbdinfo --list` does this in one command but **is not in Homebrew core** (`brew search --formula libnbd` returns only fuzzy matches; `nbdkit` likewise), so it cannot be the only path. The portable path is a 20-line Python handshake in the script — read the greeting, send `NBD_FLAG_C_FIXED_NEWSTYLE`, send `NBD_OPT_GO` for the export, and decode the `NBD_INFO_EXPORT` reply's 16-bit flags — which needs nothing but a Python 3 interpreter. **Use the Python path as the primary**, because a script that silently degrades when a tool is missing is a script whose check nobody notices has stopped running. If `nbdinfo` is present, run it too and print its output for the record; do not make the assertion depend on it.

- [ ] **Step 3: Write `test/nbd/README.md`**

Prerequisites (`brew install qemu` on macOS; the Linux packages on a Linux host), what each of the three scripts proves, and — explicitly — that the oracle validates the *codec and flags* while the KVM harness (Task 8) validates the *boot path*. State that the oracle needs no root and no VM, so it is the cheap regression check.

- [ ] **Step 4: Run the oracle**

Run: `test/nbd/run-oracle.sh`
Expected: exit 0, both transcripts identical.

If it diverges, the divergence is the finding: a hand-rolled codec's first real client is where a transposed field shows up. Diagnose against the spec before changing anything.

- [ ] **Step 5: Commit**

```bash
git add test/nbd/
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
- `-netdev user,id=n0,hostfwd=tcp::10809-:10809` — `10.0.2.2` is QEMU's user-net gateway to the host.
- **No UEFI-boot-from-NBD.** EDK2 has no NBD driver; the boot path is kernel+initramfs from a local disk, then root-on-NBD via `switch_root`.
- The script must build the initramfs with the `nbd` module and `nbd-client`, or refuse with a message naming what is missing.
- **The recovery environment must not be on NBD.** The script must not run `fsck` from inside the guest: the guest's root is the volume under test, so an `fsck` there is judging a crash it is also recovering from. The check belongs on the host, against the volume read as a file over NFS — which is the B1 property (spec §2.2) earning its place.
- Record which cache is in the path, in the script's own output: in this topology the guest's kernel NBD client speaks TCP directly to DittoFS, so there is no QEMU block layer, and a `-drive`'s `cache=` mode is irrelevant. Print the guest page-cache state and the share's `CommitAck` value so the run is self-describing.

Reference launch (from the spec):

```
qemu-system-aarch64 -M virt -cpu host -m 2G \
  -drive if=virtio,file=boot.img,format=raw \
  -netdev user,id=n0,hostfwd=tcp::10809-:10809 -device virtio-net,netdev=n0
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
- [ ] Task 5 — `TestConnection_WriteZeroLengthIsEINVAL` passes and the connection is still usable afterwards.
- [ ] Task 5 — `TestConnection_RejectedWriteKeepsTheStreamSynchronized` passes, and fails when the payload drain is removed.
- [ ] Task 5 — `TestConnection_WriteWithFUAIsDurableBeforeReply` passes, and fails when `commit` is moved after the reply.
- [ ] Task 5 — `TestIntegration_FlushRequireDurableWithUnhealthyRemoteIsEIO` passes.
- [ ] Task 6 — `TestConnection_WriteZeroesThenReadIsZeros` asserts `PunchHole` was called and `WriteAt` was not, and fails when `WriteZeroes` is changed to a zero-buffer write.
- [ ] Task 6 — `TestConnection_WriteZeroesWithNoHoleIsAccepted` passes (`NO_HOLE` is a MUST we advertise) and `TestConnection_WriteWithNoHoleFlagIsEINVAL` passes (it is not a `WRITE` flag).
- [ ] Task 6 — `TestConnection_TrimZeroLengthIsEINVAL` and `TestConnection_WriteZeroesZeroLengthIsEINVAL` pin the uniform zero-length answer (`EINVAL`), and each is followed by a valid command on the same connection to prove the stream is still usable.
- [ ] Task 6 — `go build ./... && make test-unit` clean.
- [ ] Task 7 — `test/nbd/run-oracle.sh` exits 0 with identical transcripts against `qemu-nbd`.
- [ ] Task 7 — the flag-parity check confirms every flag DittoFS advertises is one `qemu-nbd` also advertises.
- [ ] Task 8 — `bash -n` clean on both scripts; each refuses cleanly without prerequisites.
- [ ] Task 8 — `mkfs.ext4` + mount + write + `sync` + unmount + remount in a KVM guest.
- [ ] Task 8 — SIGKILL mid-write → restart → guest I/O resumes inside the dead-connection window; host-side `fsck` clean.
- [ ] The volume is readable as a plain file over NFS (the B1 property, spec §2.2).
- [ ] `docs/guide/cli.md` regenerated; `test/required-tests.sh` unaffected.
- [ ] No commit message names AI tooling or carries a `Co-Authored-By` trailer for it (`test/no-ai-attribution.sh`).
