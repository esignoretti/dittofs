# NBD block-protocol adapter for DittoFS — design

**Status:** design, awaiting implementation plan
**Date:** 2026-10-08
**Target tree:** `origin/develop` @ `4dae5562` (line citations verified against it)
**Predecessor:** `dittofs-iscsi-frontend-assessment.md` (2026-10-04) — assessed iSCSI vs NBD
and concluded NBD. This document is the implementation design for that conclusion.

---

## 1. Intent

Give DittoFS an NBD adapter so a KVM guest can use a DittoFS share as a **root/boot
disk**: the guest runs `nbd-client` against DittoFS, gets `/dev/nbd0`, and `mkfs`/`mount`
a filesystem on it. DittoFS becomes the storage behind a running virtual machine's root
filesystem.

This is a **personal experiment**, not an upstream contribution. The acceptance bar is
the author's own; there is no maintainer roadmap to satisfy and no obligation to match
what upstream would merge. Collisions with upstream direction are worth noting but are
not constraints on this design.

### 1.1 What "working" means

**Correct-by-construction**, chosen over "demo" and "usable". Concretely: the adapter is
built so that the unsafe answers are not expressible, rather than built quickly and then
guarded. Two properties follow from that choice and drive most of this document:

1. **Durability is never claimed falsely.** A flush that did not reach durable storage
   must not be acknowledged. The engine already has the machinery for this (§6); the
   adapter's obligation is to not defeat it.
2. **Residency is never reported as absence.** A range that holds data must never be
   reported as a hole. §7 states this as a structural rule and explains why v1 satisfies
   it by construction rather than by care.

The correctness bar is the reason the hand-rolled codec was chosen over an existing
library (§3.1): a library that sends the reply as soon as the backend call returns puts
the durability decision out of reach.

### 1.2 Deployment context

- Backing store: **local filesystem + S3 remote**. DittoFS manages reliability; the
  server respawns with cache coming back online, protected by RAID. The adapter
  therefore does not impose a durability configuration on the share, and the
  "avoid an inline `PutObject` per guest fsync" concern is **not** a design driver —
  `CommitAck` remains a per-share operator choice (§6.1).
- Client: **QEMU/KVM**, with the guest's own kernel NBD client talking to `/dev/nbd0`.
  QEMU is not an alternative to the kernel client — it contains one, and the in-guest
  path is the real production path being tested.
- The volume is the **boot disk**, so the guest's root filesystem generates the write
  traffic and the kernel's `nbd` module is loaded from initramfs.

---

## 2. Decisions

Three decisions were settled before this design and are not reopened here.

### 2.1 Codec: hand-rolled, scoped (A3)

Not `pojntfx/go-nbd`, not `andrewchambers/gonbdserver`.

**`pojntfx/go-nbd` is disqualified on one fact:** its server command switch handles
`READ`, `WRITE` and `DISC` only — **no `FLUSH`** (`pkg/server/nbd.go:354`, `:371`,
`:405` in that repo). A block device without `FLUSH` cannot be a boot disk, and the
backend interface cannot express FUA.

**`andrewchambers/gonbdserver` is closer but wrong in two ways that matter:**

- `NBD_CMD_WRITE_ZEROES` is implemented by **writing zeros** through `WriteAt`
  (`connection.go:503-509`). On a content-addressed store that *materializes* real
  chunks, so a filesystem's discard path would allocate storage instead of freeing it.
  The correct call is `engine.Store.PunchHole` (`pkg/block/engine/readwrite.go:286`).
- The reply is sent when the backend call returns. "Verify durability before
  acknowledging" is therefore not expressible without forking the library — which is the
  "a guard someone remembered to add" pattern this design exists to avoid.

`gonbdserver` remains useful as a **reference implementation**, and `qemu-nbd` as a
**differential oracle** (§9.4). Neither is a dependency.

Scope of the hand-rolled codec is bounded by §5: fixed-newstyle handshake,
`READ`/`WRITE`/`FLUSH`/`TRIM`/`WRITE_ZEROES`, simple replies only in v1.

### 2.2 Volume model: one metadata file per volume (B1)

A volume **is** a metadata file in the share, at `volumes/<export-name>`. Its bytes are
the payload addressed by that file's payload ID.

`engine.Store` is payload-keyed, and a share is a filesystem namespace whose bytes are
the union of many payloads with no index. "The whole share is one device" therefore
collapses into this option anyway, with the share name as the volume name.

Consequences — all of them things this option *gives* rather than things it must build:

| Property | Why |
|---|---|
| Existence, permissions, identity | Come from the metadata store. No new control-plane table, API, or CLI. |
| Stable device geometry | Size is read from `metadata.File.Size`, fixed at creation (§4.4). |
| **Second way to inspect the bytes** | The same share can be mounted over NFS and the volume read as a plain file. When a block write goes wrong this is an independent check on the data. |
| Cheap hot-path resolution | One call to `rt.GetBlockStoreForShare(name)` (`pkg/controlplane/runtime/runtime.go:838`). No file handle held across a restart. |

The rejected alternative was a dedicated sparse payload plus a volume registry: it needs
new control-plane surface (table, API, CLI, lifecycle) and loses the NFS-readable
property, buying nothing the file-backed volume does not already provide.

---

## 3. Architecture

### 3.1 A sibling adapter

`pkg/adapter/nbd/` sits beside `pkg/adapter/nfs/` and `pkg/adapter/smb/`, embedding
`adapter.BaseAdapter` (`pkg/adapter/base.go`) for lifecycle, listener, and connection
management, and implementing `adapter.Adapter` (`pkg/adapter/adapter.go:30`).

The interface's contract is what the design leans on:

- `Serve(ctx)` blocks until cancellation or unrecoverable error. **If it returns before
  cancellation, `DittoServer` treats it as fatal and stops every other adapter** — so the
  connection loop must never return early on a per-connection error.
- `Stop(ctx)` must be idempotent, safe to call concurrently with `Serve`, and must clean
  up all goroutines.
- `SetRuntime(rt any)` is called exactly once before `Serve`; implementations type-assert
  to `*runtime.Runtime`.

### 3.2 Layer split

CLAUDE.md invariant 1 is explicit: *"Protocol handlers handle only protocol concerns.
XDR/SMB wire encoding, framing, dispatch, type conversion. Permission checks, file ops,
and directory traversal belong in `pkg/metadata/` and the store implementations. Don't
inline business logic into handlers."* The NBD code splits the same way NFS and SMB do:

```
internal/adapter/nbd/          wire + dispatch
  protocol.go                  constants, request/reply header encode+decode
  handshake.go                 NBDMAGIC/IHAVEOPT, option haggling (GO/INFO/LIST/ABORT)
  connection.go                per-connection loop, serialized dispatch, shutdown
  reply.go                     simple replies, error replies
  handlers_read.go             READ
  handlers_write.go            WRITE / WRITE_ZEROES / TRIM
  handlers_control.go          FLUSH / DISC / FUA path
  volume.go                    export name -> metadata file -> payload ID

pkg/adapter/nbd/               adapter lifecycle
  adapter.go                   Config, New, Serve, Stop, Healthcheck
  config.go
```

`volume.go` is the only file that talks to the runtime. This mirrors the existing
`internal/adapter/smb/handlers/` vs `pkg/adapter/smb/` split.

The wire layer's boundary is a small interface, not `*engine.Store`, for one reason:
the dispatch tests must be able to drive failure modes the engine cannot be made to
produce on demand (a read error, a flush that reports `Finalized: false`). The interface
carries the four I/O methods (`ReadAt`, `WriteAt`, `PunchHole`, `Flush`) plus the three
durability accessors (`LocalDurable`, `RemoteDurable`, `RequireDurableCommit`) that the
shared commit rule reads. `*engine.Store` satisfies all seven unchanged.

One consequence worth naming, because it is the seam where a shortcut would be tempting:
`common.WriteToBlockStore` and `common.CommitBlockStore` take `*engine.Store` concretely.
The commit rule is the one that **must** stay shared — it is the durability decision, and
a second copy is how the NBD path drifts from NFS COMMIT and SMB Flush — so
`CommitBlockStore`'s store parameter widens to an interface. The read and write helpers
are not widened: the wire handlers call `ReadAt`/`WriteAt` directly on a
deadline-wrapped context and route errors through one `mapError`, which reproduces the
two lines those helpers add without forcing a third interface.

### 3.3 Control-plane registration

Four touch points, each following an existing pattern:

| Site | Change |
|---|---|
| `pkg/controlplane/models/adapter.go:36` | `DefaultPort`'s type switch gains `"nbd"` → `10809` |
| `pkg/controlplane/models/adapter.go:50` | `Validate()`'s error text becomes "must be one of nfs, smb, nbd" |
| `cmd/dfs/commands/start.go:943` | Construct alongside `nfs.New(nfsCfg)` / `smb.New(smbCfg)` (`:977`) |
| `docs/guide/cli.md` | Regenerate (CLAUDE.md "Frequent commands") |

`DefaultPort` (`:34`) returning non-zero is what makes `Validate()` accept the type for
free, and 10809 is the conventional NBD port — already non-privileged, matching
DittoFS's 12049/12445 convention.

The config struct mirrors `pkg/adapter/smb/config.go:63`:

```go
type Config struct {
    Enabled        bool          `mapstructure:"enabled"`
    Port           int           `mapstructure:"port" validate:"min=0,max=65535"`
    BindAddress    string        `mapstructure:"bind_address"`
    MaxConnections int           `mapstructure:"max_connections" validate:"min=0"`
    MaxMessageSize int           `mapstructure:"max_message_size" validate:"min=0"`
    Timeouts       TimeoutsConfig `mapstructure:"timeouts"`
    // NBD-specific
    AllowRemoteBind bool   `mapstructure:"allow_remote_bind"`
    DefaultExport   string `mapstructure:"default_export"`
}
```

`TimeoutsConfig` follows `pkg/adapter/smb/config.go:19` — `Read`, `Write`, `Idle`,
`Shutdown`.

### 3.4 The bind-address gate

NBD has **no user identity** — no `AUTH_SYS`, no Kerberos, only optional STARTTLS. An
unauthenticated *block* device is a different risk class from an unauthenticated *file*
share: a single `READ` hands over every byte on the volume, with no path scoping and no
per-user check to fail.

The precedent is in the tree: `pkg/adapter/smb/adapter.go:225` refuses to start when
`Encryption.Mode == "disabled" && netutil.IsNonLoopbackHost(config.BindAddress)`.
NBD's version is **stricter**: refuse a non-loopback bind unless `allow_remote_bind` is
explicitly set. There is no NBD encryption mode that makes a remote bind safe on its
own, so the SMB shape ("insecure if not encrypted") does not transfer — the honest shape
is an explicit opt-in.

The failure must be at **startup**, not first connection: a misconfigured adapter that
binds and serves is indistinguishable from a working one until someone reads the volume.

---

## 4. Protocol surface

### 4.1 Handshake

Fixed-newstyle only. `NBDMAGIC` (`0x4e42444d41474943`) then `IHAVEOPT`
(`0x49484156454f5054`), then option haggling. Option reply magic is `0x3e889045565a9`.

| Option | Value | v1 behaviour |
|---|---|---|
| `NBD_OPT_GO` | 7 | Primary path. Resolve export, send `NBD_REP_INFO` + `NBD_INFO_EXPORT`, then `NBD_REP_ACK`; transmission phase begins |
| `NBD_OPT_INFO` | 6 | Same replies, no transmission phase |
| `NBD_OPT_LIST` | 3 | Enumerate volumes (readdir of `volumes/`) |
| `NBD_OPT_ABORT` | 2 | `NBD_REP_ACK`, close |
| `NBD_OPT_EXPORT_NAME` | 1 | Legacy; still used by `nbd-client` and `qemu-img`. Reply carries flags in lieu of the option reply |
| `NBD_OPT_STRUCTURED_REPLY` | 8 | `NBD_REP_ERR_UNSUP` in v1; added in phase 4 |
| `NBD_OPT_LIST_META_CONTEXT` / `SET_META_CONTEXT` | 9 / 10 | `NBD_REP_ERR_UNSUP` in v1; added in phase 4 |
| `NBD_OPT_STARTTLS` | 5 | `NBD_REP_ERR_UNSUP` — **fail closed, never pretend** |

Option-reply error values used: `NBD_REP_ERR_UNSUP` (2³¹+1), `NBD_REP_ERR_INVALID`
(2³¹+3), `NBD_REP_ERR_UNKNOWN` (2³¹+6), `NBD_REP_ERR_SHUTDOWN` (2³¹+7).

`NBD_INFO_EXPORT` (0, length 12) is **mandatory before a successful `NBD_REP_ACK`** — the
spec requires the server to send at least one `NBD_REP_INFO` carrying it. We also send
`NBD_INFO_BLOCK_SIZE` (3, length 14): minimum 1, preferred 4096, maximum 32 MiB.

The preferred block size is what makes a client align its I/O; 4096 matches what the
journal's chunking already wants. No zero-padding after the ACK.

Unknown export name → `NBD_REP_ERR_UNKNOWN`.

### 4.2 Transmission flags — the contract

**A client acts on these bits without verifying them.** They are a contract, not a hint.

**Advertised:**

| Flag | Bit | Why |
|---|---|---|
| `NBD_FLAG_HAS_FLAGS` | 0 | Mandatory, always set |
| `NBD_FLAG_READ_ONLY` | 1 | From `share.ReadOnly` (`pkg/controlplane/models/share.go:40`) |
| `NBD_FLAG_SEND_FLUSH` | 2 | We can honor it, and honoring it is the point of the design |
| `NBD_FLAG_SEND_FUA` | 3 | Mapped to the same durability path as FLUSH |
| `NBD_FLAG_SEND_TRIM` | 5 | Mapped to `PunchHole` (§4.3) |
| `NBD_FLAG_SEND_WRITE_ZEROES` | 6 | Mapped to `PunchHole` (§4.3) |

**Not advertised:** `ROTATIONAL` (4), `SEND_DF` (7), `CAN_MULTI_CONN` (8),
`SEND_RESIZE` (9), `SEND_CACHE` (10), `SEND_FAST_ZERO` (11),
`BLOCK_STATUS_PAYLOAD` (12).

Two omissions are load-bearing:

- **`SEND_FLUSH` is the one flag whose absence fails open.** QEMU's `nbd_client_co_flush`
  returns **success** when the flag is clear. A server that does not advertise it turns
  every guest `fsync` into a no-op that reports durability it never had — silently, and
  in the direction that loses data. We advertise it *because we honor it*.
- **`CAN_MULTI_CONN` is the one flag whose presence creates silent corruption.** It tells
  the client it may open several connections to one device and rely on server-side
  coherence. The engine is per-share with a local journal (CLAUDE.md invariant 4); two
  concurrent connections on one volume means two writers into one payload ID with
  ordering unresolved. Leaving it clear is what makes the single-connection
  serialization of §4.5 a **guarantee** rather than a hope.

### 4.3 Command set

The command flags below are honored only where advertised; see §4.2.

| Command | Value | Maps to |
|---|---|---|
| `NBD_CMD_READ` | 0 | `engine.Store.ReadAt(ctx, payloadID, buf, offset)` |
| `NBD_CMD_WRITE` | 1 | `PrepareWrite` → `engine.Store.WriteAt` → `CommitWrite` |
| `NBD_CMD_DISC` | 2 | Drain in-flight, close. **No reply** |
| `NBD_CMD_FLUSH` | 3 | `common.CommitBlockStore(ctx, bs, payloadID)` (parameter widened to an interface; see §3.2) |
| `NBD_CMD_TRIM` | 4 | `engine.Store.PunchHole` |
| `NBD_CMD_CACHE` | 5 | Not advertised → `NBD_EINVAL` (22); see the note below |
| `NBD_CMD_WRITE_ZEROES` | 6 | `engine.Store.PunchHole` |
| `NBD_CMD_BLOCK_STATUS` | 7 | Phase 4 only |

Command flags: `NBD_CMD_FLAG_FUA` (bit 0) is valid on **all** commands once
`NBD_FLAG_SEND_FUA` is advertised — the spec: *"the server MUST accept all commands with
this bit set (even by ignoring the bit)"*, and *"existing clients are known to set this
bit on other commands"*. So a `READ` carrying FUA is accepted and the bit is ignored (a
read writes nothing, so there is nothing to persist); it is **not** an error.

`NO_HOLE` (1) is the opposite case: the spec says *"The server MUST support the use of
this flag if it advertises `NBD_FLAG_SEND_WRITE_ZEROES`"*, and we do advertise that. So
`NO_HOLE` on `WRITE_ZEROES` is accepted and honored — it is the client asking that the
range be genuinely allocated rather than left sparse. `DF` (2), `REQ_ONE` (3), and
`FAST_ZERO` (4) are not advertised and cannot appear: a request carrying one is a client
bug, and because `DF`/`REQ_ONE` are documented as applicable only to commands we do not
advertise, `NBD_EINVAL` is the answer (`NBD_ENOTSUP` is reserved by the spec for
`FAST_ZERO`).

#### READ

`engine.Store.ReadAt` already implements NBD's READ contract, including its hard part.
It zero-fills genuine holes — correct for NBD — and refuses rather than serving zeros it
cannot stand behind:

- `pkg/block/engine/read_internal.go:56` — `reconcile := st.Cold || (st.Hole && bs.HasRemoteStore())`.
- `pkg/block/engine/read_internal.go:104` — *"accepting a cold re-read would hand the
  caller zeros for real data with no error anywhere. Fail closed instead."* A window
  still cold after hydration returns `ErrChunkNotFound`.
- `pkg/block/engine/read_internal.go:78` — `healCorruptWarmRead`: a BLAKE3-verified
  failure demotes the range and re-fetches, or fails closed with
  `ErrIntegrityCheckFailed` when there is no remote to heal from.

**A read that cannot be served honestly returns an error, never silent zeros.** That
property exists today; the adapter's job is to not destroy it. Both failure modes map to
`NBD_EIO` (5).

#### WRITE

Reuses the three-step sequence the NFS and SMB handlers already use —
`internal/adapter/nfs/v4/handlers/write.go:63` documents it as *"Delegates to
MetadataService.PrepareWrite+CommitWrite and BlockStore.WriteAt."*

Bounds-check **before touching anything**: `offset + length > exportSize` → `NBD_ENOSPC`
(28), **not** `NBD_EINVAL`. The spec: *"The server SHOULD return `NBD_ENOSPC` if it
receives a write request including one or more sectors beyond the size of the device.
... It SHOULD return `NBD_EINVAL` if it receives a read or trim request including one or
more sectors beyond the size of the device."* The distinction is deliberate — a write
past the end means the client's view of the device is too large, which is a space
problem; a read or trim past the end is a malformed request.

Note this makes the check an *error*, not a growth: the export size is fixed at creation
(§4.4), so a `WRITE` past the end is refused rather than extending the file. The client's
own constraint is the stronger one: *"the client MUST NOT use a length which, when added
to offset, would exceed the export size."*

#### FLUSH

Maps 1:1 onto code that already exists (`internal/adapter/common/write_payload.go:110`):

```
Flush → hard error? → classify via ClassifyBlockStoreError
      → requireDurable? no       → return nil          (CommitAck: journal)
      → localDurable?   yes      → return nil          (journal fsync landed)
      → finalized && remoteDurable? yes → return nil
      → else ErrNotDurableYet → NBD_EIO
```

The spec's requirement is stronger than "flush this volume": *"the server MUST NOT send
a successful reply header for this request before all write requests for which a reply
has already been sent to the client have reached permanent storage."* That is a barrier
over writes whose replies the client already saw — which is why §4.5's serialization is
part of the durability design, not merely a performance choice.

`ErrNotDurableYet` maps to `NBD_EIO`: FLUSH promised durability and we could not deliver
it, so there is no honest success to send.

#### FUA

Spec: *"If the server receives a command with `NBD_CMD_FLAG_FUA` set it MUST NOT send its
reply to that command until all write operations (if any) associated with that command
have been completed and persisted to non-volatile storage."* Implementation: perform the
write, then `CommitBlockStore`, then reply.

#### TRIM and WRITE_ZEROES

Both map to `engine.Store.PunchHole` (`pkg/block/engine/readwrite.go:286`), and the
reasoning differs for each.

**TRIM has a semantics trap.** The spec says a client *"MUST NOT make any assumptions
about the contents of the export affected by this command"* and a server *"MAY discard
length bytes starting at offset, but is not required to."* So TRIM does **not** require
zeros on read. But `PunchHole` is deliberately stronger —
`pkg/block/engine/readwrite.go:281`: *"To guarantee zeros on the read path regardless of
rollup state, the range is overwritten with zeros via the local append log."* Mapping
TRIM → `PunchHole` therefore over-delivers.

The alternative — a cheaper discard leaving contents undefined — would make reads
non-deterministic, a far worse property than paying for zero chunks that dedup to a
single CAS object anyway. **TRIM maps to `PunchHole`.**

TRIM is also covered by the flush barrier for free, since `PunchHole` writes through the
same journal.

**WRITE_ZEROES** → `PunchHole`, and this is exactly where the reference library gets it
wrong: `gonbdserver` calls `WriteAt` with a zero buffer, which on a CAS store
*materializes* real chunks, so a filesystem's discard path would allocate storage instead
of freeing it. `PunchHole` is the correct call.

### 4.4 Export size is fixed

No `SEND_RESIZE` means the client assumes the size will not change. A boot-disk client
partitioned the device at negotiation time; a size change invalidates its partition table
underneath a mounted filesystem.

So: size is set at volume creation from `metadata.File.Size` and never changes. `WRITE`
past the end is **refused** rather than growing the file, and `TRIM`/`WRITE_ZEROES` are
clamped to the export end.

### 4.5 Serialization

**v1 processes one command at a time per connection: read request → handle → reply.**

The spec permits out-of-order replies (*"Replies need not be sent in the same order as
requests... and structured reply chunks from one request may be interleaved with reply
messages from other requests"*), but the FLUSH and FUA barriers are defined over *replies
already sent*, which is a real ordering problem the moment two commands are in flight.
Serializing makes both barriers trivially correct and gives in-order replies for free.

This composes with the `CAN_MULTI_CONN` omission (§4.2): one connection, one command in
flight, means the engine sees writes in exactly the order the client intended, once.

**Upgrade path** (not v1): pipelining with explicit barrier tracking of the
highest-replied write. Not to be taken until a benchmark shows serial dispatch is the
bottleneck. For a boot disk backed by a local journal, it very likely will not be.

### 4.6 Error mapping

| Condition | NBD error |
|---|---|
| Misaligned, unknown command, unknown or inapplicable flag | `NBD_EINVAL` (22) |
| Zero-length `READ`, `WRITE`, `TRIM`, or `WRITE_ZEROES` | `NBD_EINVAL` (22) |
| `READ` or `TRIM` beyond the export size | `NBD_EINVAL` (22) |
| `WRITE` beyond the export size | `NBD_ENOSPC` (28) |
| Read unservable honestly (cold after hydrate, integrity failure) | `NBD_EIO` (5) |
| `ErrNotDurableYet` from a FLUSH or FUA | `NBD_EIO` (5) |
| `offset + length` wraps | `NBD_EIO` (5) |
| Quota exhausted | `NBD_ENOSPC` (28) |
| Write to a read-only share | `NBD_EPERM` (1) |
| Shutdown in progress | `NBD_ESHUTDOWN` (108) |
| `engine.ErrStoreClosed` otherwise | `NBD_EIO` (5) |

Three entries here are narrower than they first look, and each is deliberate:

- **`NBD_EOVERFLOW` is not in the table.** The spec: *"The server SHOULD NOT return
  `NBD_EOVERFLOW` except as documented in response to `NBD_CMD_READ` when
  `NBD_CMD_FLAG_DF` is supported, or when a command without payload requests a length
  larger than an advertised maximum payload length."* We support neither case, so an
  overflowing `offset + length` — reachable, since both fields are client-supplied — is
  `NBD_EIO`. It is not a range error the client can act on; it is a request that cannot
  be interpreted at all.
- **`NBD_ENOTSUP` is not in the table either.** The spec: *"The server SHOULD NOT return
  `NBD_ENOTSUP` except as documented in response to `NBD_CMD_WRITE_ZEROES` when
  `NBD_CMD_FLAG_FAST_ZERO` is supported."* We do not advertise `SEND_FAST_ZERO`, so a
  command we do not implement is `NBD_EINVAL` — the value the spec names twice
  (*"The server SHOULD return `NBD_EINVAL` if it receives an unknown command"*, and the
  same for an unknown flag).
- **`NBD_ESHUTDOWN` is used deliberately.** It is the protocol's way of saying
  "reconnect", which is exactly the signal the kernel's dead-connection timeout is
  waiting for (§6.2). A graceful DittoFS restart should answer new commands with
  `NBD_ESHUTDOWN` during drain, so the client pauses instead of erroring the device.

`ClassifyBlockStoreError` (`internal/adapter/common/errclassify.go:31`) already maps
`engine.ErrStoreClosed` → `merrs.ErrStaleHandle` and everything else → `merrs.ErrIOError`;
the NBD mapping layers protocol values on top of that classification rather than
duplicating it.

The zero-length row is worth stating because the spec declines to: for all four commands
it says only *"the client SHOULD NOT request a [read/write/trim/write zeroes] length of 0;
the behavior of a server on such a request is unspecified although the server SHOULD NOT
disconnect."* One uniform answer — `NBD_EINVAL` — is chosen over a per-command table
because no correct client sends a zero-length block request, the spec leaves the choice
open, and a rule nobody has to remember is a rule nobody gets wrong. The connection
stays usable, which is the one part the spec does require.

### 4.7 Identity

NBD carries no user identity. The adapter supplies a **configured synthetic identity** for
the metadata permission checks (`CheckWritePermissionFile` and friends), and creates
`volumes/<name>` with that identity as owner.

This is the honest shape rather than a shortcut: a block device is all-or-nothing by
construction, there is no per-user scoping to enforce, and the only meaningful access
control is *who can reach the socket at all*. That is why the bind gate (§3.4) is not
optional.

Per CLAUDE.md invariant 2, every operation carries a `*metadata.AuthContext`; the
synthetic identity populates it, and it threads through handler → `pkg/metadata` →
store like any other.

---

## 5. Durability, restart, and recovery

### 5.1 What the adapter promises

| NBD op | Promise | Kept by |
|---|---|---|
| `READ` | bytes, or an error — never zeros it cannot stand behind | `read_internal.go:104` fail-closed; `:78` heal-or-fail |
| `WRITE` (no FUA) | accepted into the journal, ordering preserved | `engine.WriteAt` → `local.WriteAt` |
| `WRITE` + FUA | persisted before reply | write, then `CommitBlockStore` |
| `FLUSH` | every already-replied write is persistent | `common.CommitBlockStore` |
| `TRIM` / `WRITE_ZEROES` | range reads as zeros afterward | `engine.PunchHole` zero-overwrite |

**The adapter adds no durability machinery of its own.** It calls the same
`CommitBlockStore` that NFS COMMIT and SMB Flush call. `CommitAck: journal`
(`pkg/controlplane/models/share.go:24`) is the honest default for a block volume —
*"durable in the share's journal. It survives process and host crash, not device loss"* —
and with an fs local store `LocalDurable()` is true, so
`pkg/block/engine/flush.go:554` (`if bs.LocalDurable() && !bs.RequireDurableCommit()`)
returns right after a real fsync and the promise is exactly as strong as documented.

`CommitAck: block-store` (`pkg/controlplane/models/share.go:29`) is available and makes
FLUSH wait for the remote. **This is a per-share operator choice, not an adapter
decision** — the deployment context (§1.2) has DittoFS managing reliability, so the
adapter does not force either value.

### 5.2 What carries state across a restart

**Nothing in the protocol.** NBD has no session, no resume, no handshake replay. A
reconnect is a brand-new client that must see the same export name, the same size, and
the same bytes. So the answer to "what happens when DittoFS respawns" is entirely: what
does the journal reconstruct, and is the export identical?

The journal is built for this. `pkg/block/journal/cold.go:262` — *"a restart replays
exactly what fsynced."* The ordering that makes it safe is already enforced at the one
chokepoint, `pkg/block/journal/state.go:149` (`demote`): `appendCold` fsyncs **before**
the flip runs, so a crash between the two leaves the bytes local and the marker absent —
the conservative direction. A failed append returns `errors.Join(ErrStateLost, err)`
(`pkg/block/journal/state.go:67`) and runs nothing.

The adapter's obligations across a restart are therefore narrow and checkable:

1. **Re-bind the same port.** `DefaultPort` → 10809; the port is part of the client's
   connect command line.
2. **Serve the same export name and the same size.** Size is fixed at creation (§4.4).
3. **Answer `NBD_ESHUTDOWN` (108) during drain**, so the kernel treats it as "reconnect"
   rather than an I/O error.
4. **Come back inside the client's dead-connection window** (§6.2).

**Why the kernel's replay after reconnect needs no dedup layer from us:** every NBD
command is idempotent. `READ`, `WRITE` at a fixed offset, `FLUSH`, `TRIM` and
`WRITE_ZEROES` all re-execute to the same end state. Combined with one connection and one
command in flight (§4.5), the engine sees writes in the client's intended order, once.
The absence of `CAN_MULTI_CONN` is what makes that a guarantee.

---

## 6. Client-side reconnection

This is the mechanism that makes "DittoFS respawns and cache comes back online" actually
work, and it is a kernel feature rather than a DittoFS one — which is why it constrains
the harness.

### 6.1 The dead-connection timeout

Commit `560bc4b399` ("nbd: handle dead connections") added `dead_conn_timeout`, settable
via the `NBD_ATTR_DEAD_CONN_TIMEOUT` netlink attribute. Its message:

> Sometimes we like to upgrade our server without making all of our clients freak out and
> reconnect. This patch provides a way to specify a dead connection timeout to allow us to
> pause all requests and wait for new connections to be opened. With this in place I can
> take down the nbd server for less than the dead connection timeout time and bring it back
> up and everything resumes gracefully.

In-flight requests are **paused**, not failed: the guest's mounted filesystem never sees
the outage if the server returns inside the window.

### 6.2 Caveats that become harness requirements

1. **The window is bounded in practice by the hung-task detector.** `wait_for_reconnect()`
   blocks the submitting thread, and `BLK_MQ_F_BLOCKING` leaves it in an uninterruptible
   wait — a `dead_conn_timeout` over ~120 s trips the detector. **Plan for 30–60 s, not
   600 s.**
2. **`-timeout` and `-persist` are not this mechanism.** `-timeout` sets
   `NBD_SET_TIMEOUT` (per-request; kernel 2.6.24+, `NBD_SET_TIMEOUT` ioctl). `-persist`
   is `nbd-client`'s own reconnect loop. Conflating them is a real failure mode: a
   reported case with `-persist -timeout 600 -connections 4` still went read-only on
   server restart.
3. **Restart must re-bind the same port and serve the same export name, size, and bytes.**

---

## 7. The residency-truth gate

This is the C choice's dependency, stated as a structural requirement.

### 7.1 The rule

> **No code path in the NBD adapter may derive "this range is a hole" from the absence of
> a record.**

Every hole answer must come from `engine.Store.DataExtents`, which already widens on
uncertainty, and never from a direct `local.DataExtents` call or a manifest walk.

### 7.2 Why `DataExtents` is already the right primitive

`pkg/block/engine/dataextents.go:24` states the polarity: *"The map may over-report data
but must never under-report it."*

For `base:allocation` that is exactly right: over-reporting makes the client copy bytes it
could have skipped (**safe**); under-reporting makes it skip real data (**silent loss**).
Same rule, same direction.

The implementation already honors it — a local-tier error, an unenumerable manifest, or an
unparseable row each widen to `{{0, fileSize}}` rather than narrowing
(`pkg/block/engine/dataextents.go:41` and the two cases below it).

Its counterpart surface is `local.LocalStore.ReadAt`'s `ReadState.Hole`, which obeys the
same rule through the *opposite token* (an unclassifiable range reports `Hole` true, never
false). The doc warns explicitly: *"do not reconcile the two by making them agree on a
token — that edit is the bug in whichever direction it is made."*

### 7.3 The gap, and why v1 does not expose it

`DataExtents` derives its answer from local-tier bytes plus manifest rows. A range in
**neither** — which is what `StateLost` describes — is absent from the map and therefore
reported as a **hole**.

Today that cannot happen. `pkg/block/journal/state.go:40`:

> `StateLost` is written with no copy anywhere: reads MUST fail, never zero. No code path
> holds it — a loss that cannot be made durable is refused — but it is the state every
> residency-reducing path must be measured against, and `Extents` is what would make it
> observable if one ever did.

But that is a **runtime invariant, not a structural one**. The primitive that would make
it observable already exists and has **zero production callers**:
`journal.Store.Extents(ctx, id) ([]Extent, error)` at `pkg/block/journal/state.go:111`,
returning `[]Extent{Off, Len int64; State State; Durable bool}`
(`pkg/block/journal/state.go:73`). Its only caller is `state_test.go`. The transition
table it is measured against lives in the same package's 14 tests
(`TestTransition_*`, `TestModel_RandomOperationsAgainstNaiveOracle`).

**Decision: v1 does not advertise structured replies or `BLOCK_STATUS`.** Not because the
mapping is hard, but because advertising a residency query whose under-report case is
guarded only by a comment is the same class of mistake as advertising `SEND_FLUSH` you do
not honor. With structured replies off, the v1 adapter has **no hole-reporting surface at
all**, so the rule in §7.1 is satisfied by construction rather than by care — which is
what "correct-by-construction" means concretely here.

**Phase 4** adds `BLOCK_STATUS` *together with* the state-aware query: `Extents()`
consulted so a lost range reports as **data** (forcing a READ that fails loudly) rather
than as a **hole** (letting the client skip it). That is the bounded C task — not
"rewrite the state model", but "wire the existing state primitive into the one surface
where its absence is a silent-loss path."

### 7.4 Related, deliberately out of scope

The engine's own read path still reconstructs residency from two booleans —
`reconcile := st.Cold || (st.Hole && bs.HasRemoteStore())`
(`pkg/block/engine/read_internal.go:56`) — rather than consulting `Extents()`. That is
worth a follow-up but is **not** part of this project: the NBD adapter's obligation is to
not add a *third* place that guesses.

Similarly, `docs/internals/rfc-block-dataflow.md:3` describes the block-dataflow RFC as
"partially landed" (lanes H and C merged; the Flush+Run+AfterFile seam and the full
layout+tests pass still open). This design depends only on what is already merged.

---

## 8. Healthcheck

`BaseAdapter.Healthcheck` (`pkg/adapter/healthcheck.go:39`) is deliberately cheap —
atomic flags and channel state, never network or disk — and its doc instructs concrete
adapters to override and add `StatusDegraded`/`StatusDisabled`.

NBD has a real **`Degraded`** to report: *the adapter is serving, but a volume's export
could not be resolved or its block store is unhealthy.* That is distinct from `Unhealthy`
(adapter down) and from `Disabled` (`config.Enabled == false`), and it is the signal an
operator needs during exactly the window when a restart is in progress and clients are
paused.

Mapping: `StatusDisabled` when configured off → `StatusDegraded` on export/block-store
failure → delegate to `BaseAdapter.Healthcheck`. The probe must stay cheap per the
interface contract, so it reads already-tracked state rather than probing storage.

---

## 9. Testing

### 9.1 Unit — protocol codec

Table-driven byte fixtures: handshake option sequences, header encode/decode, error
replies, and the flag-negotiation matrix.

The valuable cases are the negative ones:
- `WRITE_ZEROES` sent when it was not advertised → `NBD_EINVAL`
- `TRIM` on a read-only export → `NBD_EPERM`
- `offset + length` overflowing → `NBD_EIO`
- `GO` for an unknown export → `NBD_REP_ERR_UNKNOWN`
- an unknown command, or a flag not documented as applicable to that command → `NBD_EINVAL`
- a zero-length `READ`, `WRITE`, `TRIM`, or `WRITE_ZEROES` → `NBD_EINVAL`, with the
  connection still usable afterwards
- `NBD_CMD_FLAG_FUA` on a `READ` → accepted, bit ignored (*"the server MUST accept all
  commands with this bit set (even by ignoring the bit)"*), where refusing it would break
  real clients
- `NBD_CMD_FLAG_NO_HOLE` on a `WRITE_ZEROES` → accepted, because *"The server MUST support
  the use of this flag if it advertises `NBD_FLAG_SEND_WRITE_ZEROES`"*
- a rejected `WRITE` whose payload is still in flight → the payload is drained and a
  following command on the same connection succeeds (the desync case)
- `STARTTLS` → `NBD_REP_ERR_UNSUP` (and the connection is not left in a half-negotiated
  state)

Per CLAUDE.md "Verifying a change": **the test belongs at the layer that consumes the
data.** For a fix carrying a field from handshake to command handler, the test goes at
the handler.

### 9.2 Integration — in-process

A real `engine.Store` over an fs local store and a memory remote, driven by a real NBD
client library over `net.Pipe`. Assertions that matter:

- FLUSH after writes with `CommitAck: journal` returns success.
- The same with `requireDurable` set and an unhealthy remote returns `NBD_EIO`.
- A `WRITE`+FUA is durable before its reply.
- A `TRIM`'d range reads back as zeros.
- A READ of an evicted-but-manifest-covered range hydrates and returns real bytes.
- A READ of a genuinely never-written range returns zeros (not an error).

### 9.3 Crash — the tier that tests the reliability model

Following the existing `test/crash/` convention (`device-loss.sh`,
`invalidate-cold-loss.sh`):

```
guest: write + fsync, keep writing
host:  SIGKILL the dfs process mid-flight
host:  restart dfs, same port, same export
guest: I/O resumes without a filesystem error; fsck clean
```

Two harness requirements treated as non-negotiable:

1. **A clean `umount` is not evidence.** It never exercises the barrier. Only a killed
   server does.
2. **Record which cache is in the path.** In the boot-disk topology there is no QEMU
   block layer between the guest and DittoFS — the guest's kernel NBD client speaks TCP
   directly — so the caches are the **guest page cache** and **DittoFS's journal**. A
   QEMU `-drive`'s `cache=` mode is irrelevant here and recording it proves nothing. It
   *does* matter in the QEMU-userspace-client tier (`file=nbd://...` as a `-drive`),
   where `cache=writeback` elides flushes QEMU deems unnecessary and `cache=none`
   (`O_DIRECT`) makes QEMU's role transparent. Record the cache mode **that is actually
   in the path for the tier being run** — and say which tier it was.

And one that follows from the deployment context: **the recovery path must not depend on
the volume being up.** Since the guest's root is on NBD, an `fsck` run from inside the
guest cannot judge a crash it is also recovering from. The check belongs in the harness on
the **host**, against the volume read as a file over NFS — which is the B1 property
(§2.2) earning its place — or from an initramfs shell before `switch_root`.

### 9.4 Differential oracle

`qemu-nbd` is a known-good NBD server. Point `qemu-img` / `qemu-io` at it to capture a
known-good byte sequence, then diff DittoFS's responses against it. Free, and independent
of the hand-rolled codec.

### 9.5 KVM harness

`test/e2e/framework/` already has `containers.go`, `netns.go`, `mount.go`,
`kerberos.go`, `lock_helpers.go`, `qcow2_fixture.go`. Note `qcow2_fixture.go` synthesizes
qcow2 *files* for a dedup test — it never boots a VM — so the VM launch is **new code**,
not an extension.

Boot path being tested (EDK2 has no NBD driver, so UEFI-boot-from-NBD is not a path):

```
local (small, static):  kernel + initramfs        <- firmware boots this; no NBD needed
initramfs:              modprobe nbd
                        nbd-client <host> <port> /dev/nbd0
                        mount /dev/nbd0p1 /root; switch_root
NBD (DittoFS):          the root filesystem
```

The upside of root-on-NBD is that the part being tested is exactly the part doing all the
writing (root fs fsyncs), not the read-only kernel load.

Reference launch. Note the three disks, and that only `boot.img` is local:

```
qemu-system-aarch64 -M virt -cpu host -m 2G \
  -drive if=virtio,file=boot.img,format=raw \
  -netdev user,id=n0,hostfwd=tcp::10809-:10809 -device virtio-net,netdev=n0
```

- `boot.img` — kernel + initramfs, small and static. Boots without NBD. This is the
  disk whose `cache=` mode does not matter, because nothing writes to it.
- **No data disk.** The root filesystem arrives over NBD from DittoFS, which is the
  thing under test.
- `hostfwd` maps the guest's loopback to the host's NBD port. The guest runs
  `nbd-client 10.0.2.2 10809 /dev/nbd0` — `10.0.2.2` being QEMU's user-net gateway to
  the host.
- The `cache=` mode that matters is the one on the **NBD client's** device
  (`/dev/nbd0`), not on a QEMU `-drive`: there is no QEMU block layer between the guest
  and DittoFS. §9.3's "record the cache mode" therefore applies to the guest's mount
  and `nbd-client` options, not to a `-drive` argument.

---

## 10. Phasing

| Phase | Deliverable | Exit criterion |
|---|---|---|
| 0 | Codec + handshake, no storage | `qemu-img info` negotiates and reports the right size; `nbdinfo` sees the advertised flags |
| 1 | READ + WRITE + FLUSH over a real share | `mkfs.ext4` on `/dev/nbd0` in a guest, mount, write files, `sync`, unmount, remount |
| 2 | TRIM, WRITE_ZEROES, FUA, read-only shares, bind gate | `fstrim` returns freed space; read-only export rejects writes with `NBD_EPERM`; non-loopback bind without opt-in refuses to start (including an empty `BindAddress`) |
| 3 | Crash harness | SIGKILL mid-write → restart → guest I/O resumes inside the dead-conn window; host-side fsck clean |
| 4 | `BLOCK_STATUS` + `base:allocation` + structured replies | `qemu-img map` matches `DataExtents`; a lost range reports as data, not hole |

**Phase 1 is the first moment the design is falsifiable end-to-end**, and it is the right
place to stop and reassess if the boot path turns out to need something the protocol
surface does not provide.

---

## 11. Out of scope

Explicit, so this document does not read as a promise it cannot keep:

- `NBD_OPT_STARTTLS` — answered `NBD_REP_ERR_UNSUP`, fail closed.
- `CAN_MULTI_CONN` — never advertised (§4.2).
- Resize — `SEND_RESIZE` unadvertised; size fixed at creation (§4.4).
- `SEND_CACHE`, `SEND_FAST_ZERO`, `SEND_DF`, `SEND_ROTATIONAL`.
- Multi-export per connection.
- Pipelined command processing (§4.5 gives the upgrade path).
- `NBD_OPT_EXTENDED_HEADERS` and `BLOCK_STATUS_PAYLOAD`.
- Upstreaming. This is a personal experiment; the branch is `nbd-block-protocol` off
  `develop`.
- The engine read path's own migration to `Extents()` (§7.4).

---

## 12. Risks

| Risk | Assessment | Mitigation |
|---|---|---|
| Serial dispatch is too slow for a boot disk | Low for a local-journal backend; the guest's fsyncs are the dominant cost, not the protocol | Phase 1 measures it; §4.5 gives the pipelining path |
| The kernel's dead-conn window is shorter than a DittoFS restart | Real — a cold start with journal replay could exceed 30–60 s | Phase 3 measures actual restart time; keep the window configurable and documented |
| TRIM cost on an aggressive-discard boot disk | Moderate — every discard writes zeros through the journal | Zeros dedup to one CAS object; revisit only if measurement justifies it |
| Hand-rolled codec has bugs a library would not | Accepted deliberately (§2.1) | `qemu-nbd` differential oracle (§9.4); negative-case unit tests (§9.1) |
| The `Extents()` gap is closed later and forgotten | The window is documented here and gated by phase 4 | §7.3 states the requirement; `BLOCK_STATUS` is not advertised until it holds |

---

## 13. Verification checklist

For the implementation, in the order the checks become meaningful:

- [ ] `go build ./...` and `make test-unit` clean
- [ ] `qemu-img info nbd://host:10809/vol` reports the created size
- [ ] `nbdinfo --has structured-reply` false in v1; advertised flags match §4.2 exactly
- [ ] A `WRITE_ZEROES` sent without advertisement is refused with `NBD_EINVAL`
- [ ] FLUSH with `requireDurable` and an unhealthy remote returns `NBD_EIO`
- [ ] FLUSH with `CommitAck: journal` and an fs local store returns success
- [ ] `mkfs.ext4` + mount + write + `sync` + unmount + remount in a KVM guest
- [ ] SIGKILL mid-write → restart → guest I/O resumes; host-side fsck clean
- [ ] Non-loopback bind without `allow_remote_bind` refuses to start, including an empty `BindAddress`
- [ ] A `READ` carrying `NBD_CMD_FLAG_FUA` is served, not refused
- [ ] A rejected `WRITE` with its payload still in flight leaves the connection synchronized
- [ ] A zero-length `READ`, `WRITE`, `TRIM`, or `WRITE_ZEROES` returns `NBD_EINVAL` and the connection stays usable
- [ ] A `WRITE` past the export end returns `NBD_ENOSPC`; `READ`/`TRIM` past the end return `NBD_EINVAL`
- [ ] `nbdinfo --map` is unavailable in v1 by design (no structured replies, no `BLOCK_STATUS`)
- [ ] The volume is readable as a plain file over NFS (the B1 property)
- [ ] `docs/guide/cli.md` regenerated; `test/required-tests.sh` unaffected
