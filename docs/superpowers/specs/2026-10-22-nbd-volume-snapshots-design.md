# NBD volume snapshots: crash-consistent backups you can boot

Status: design, awaiting approval
Date: 2026-10-22

## Intent

A VM runs with its disk on an NBD export served by DittoFS. Every few minutes a
crash-consistent image of that disk is captured. When the guest filesystem is
damaged, or the production volume is lost, an operator boots a VM from one of
those images — **without taking the production volume down first**.

The backup is crash-consistent, not application-consistent: the guest keeps
running and its filesystem is never quiesced. Its own journal replay on boot is
what makes the image usable, exactly as after a power cut. That is the right
trade at a few-minute cadence, where a guest-side freeze every interval would
cost more than it buys.

## The finding that shapes everything

**DittoFS already has snapshots.** `pkg/controlplane/runtime/snapshot.go` is a
complete implementation: create, list, show, remove, restore, a scheduler with
`Interval`/`KeepLast`/`TTL` (`pkg/controlplane/models/snapshot_policy.go`), a
durability verify gate, and a restore path with a pre-restore safety snapshot
and crash recovery via a durable restore marker. `dfsctl share snapshot` and
`dfsctl share snapshot-policy` expose all of it today.

So this is not "add snapshots". It is "give an existing snapshot an address on
the read path".

A snapshot on disk is two files under `<shareDataDir>/snapshots/<id>/`:

| File | Contents |
|---|---|
| `metadata.dump` | A Badger backup of every `f:` record, each carrying `FileAttr.Blocks` (the chunk references), plus the `fm:<uuid>` manifest rows |
| `manifest.hashes` | The set of content hashes the snapshot references |

Creating one copies no data. The chunks are immutable and content-addressed, and
`engine.Store.Truncate` drops *references* rather than overwriting bytes, so a
snapshot is a frozen set of references to bytes that already exist.

**The bytes are already protected.** `SnapshotHoldProvider.HeldHashes`
(`pkg/controlplane/runtime/snapshot_hold.go`) unions every live snapshot's
manifest into the GC mark set before the sweep, so a chunk a snapshot names
cannot be reclaimed. That is the Ceph "protect" step, already built.

**Consistency is already proven.** There is no global write freeze and none is
needed: the dump is taken inside a single Badger MVCC view, so all `f:` and
`fm:` rows come from one logical instant.
`TestCreateSnapshot_PointInTimeConsistency` drives a real `CreateSnapshot`
against a real store while a background writer hammers it, and asserts the
result is never half-captured. This is the crash-consistent guarantee, tested.

## What is missing

The only consumer of a snapshot today is `RestoreSnapshot`, and it is
destructive: it refuses unless the share is disabled, resets local state,
replays the dump over the live metadata, and leaves the share disabled. Booting
from a backup therefore means taking production down for the length of a
restore.

There is no clone and no fork. `metadata.CloneBlocks` is a slice copy helper.
`engine.Store.CopyPayload` and `common.CloneWholeFile` reflink a file within a
share, not a snapshot into a new addressable volume.

## Goals

- Capture a crash-consistent image of an NBD volume on a schedule, with no data
  copy and no guest quiesce. **Already satisfied by the existing snapshot
  scheduler.**
- Boot a VM from any ready snapshot while the production volume keeps serving
  reads and writes.
- Promote a snapshot to a writable export when the operator decides the
  production volume is gone, and keep running on it.
- Leave production completely unaffected: no lock it must wait on, no change to
  its cache, no risk of its writes landing in the snapshot.

## Non-goals

- **Automatic failover.** The operator decides when to promote. A promotion
  while production is alive creates two writers on a shared base — split brain —
  and detecting that needs a witness (lease, heartbeat, quorum) that does not
  exist. Not building the witness is the point of this decision, not an
  oversight.
- **Application-consistent snapshots.** No guest freeze, no VSS, no fsfreeze
  orchestration. The guest filesystem replays its own journal.
- **Single-file recovery from a snapshot.** Restore is whole-volume: connect and
  boot. Reading a file out of an image is what mounting it as NFS/SMB is for.
- **Incremental or differential backup streams.** A snapshot is a reference set,
  not a transfer format. Shipping one off-box is a separate concern.
- **Snapshotting a local-only share.** A shadow store hydrates from a remote
  tier; a local-only share has none. Same gap `RestoreSnapshot` already has
  (upstream #1718).
- **NBD-level snapshot semantics.** No `NBD_CMD_BLOCK_STATUS`, no
  `NBD_FLAG_SEND_RESIZE`. The snapshot is a separate export, not a feature of
  the export it came from.

## Anti-goals

These would each mean the design failed even if a VM boots:

- A snapshot read returns a byte production wrote after the snapshot. The
  snapshot's meaning is destroyed silently.
- Booting a snapshot slows or fails production. The whole reason for this work
  is not taking production down.
- GC reclaims a chunk a snapshot needs. Silent corruption discovered at restore,
  which is the worst possible time.
- Promotion produces a volume that silently shares mutable state with
  production.
- A snapshot export is reachable without authentication. NBD has no
  authentication of its own; the existing bind gate is the only control.

## The design: a shadow store over the same blocks

Serve each snapshot from a **second block store instance** whose local tier is
empty and whose metadata is the snapshot's, pointing at the **same remote
blocks**, driven by a **private metadata service** that nothing else shares.

```
production (unchanged)                  snapshot export
┌──────────────────────┐               ┌──────────────────────┐
│ metadata.Service     │               │ metadata.Service     │
│  registry: /export   │               │  (fresh, one share)  │
├──────────────────────┤               ├──────────────────────┤
│ meta store (live)    │               │ meta store (replayed │
│  f:, fm: rows        │               │  from metadata.dump) │
├──────────────────────┤               ├──────────────────────┤
│ engine.Store         │               │ engine.Store         │
│  local journal ──────┼──► disk       │  local journal ──────┼──► disk
│  manifest: live      │               │  manifest: SNAPSHOT  │
└──────────┬───────────┘               └──────────┬───────────┘
           │                                      │
           └────────► remote (S3), shared, ◄──────┘
                      immutable chunks
```

**Why this works, mechanically.** `engine.Store.ReadAt` calls
`bs.local.ReadAt`, then reconciles when `st.Cold || (st.Hole && bs.HasRemoteStore())`
(`pkg/block/engine/read_internal.go:56`). The shadow's journal is empty, so
every range reports `Hole` — the state a range with no local copy is *supposed*
to be in — and the read reconciles through `ensureAndReadFromLocal` →
`syncer.EnsureAvailable` → hydrate from the manifest plus remote.

The manifest it resolves against is the shadow metadata store's, which is the
snapshot's. So the bytes are the snapshot's. **No seeding step is needed**: an
empty journal naturally reports holes, and a hole on a remote-backed share
hydrates from the manifest.

**Why production is untouched.** Separate journal directory, separate metadata
store, separate `engine.Store` instance, shared remote. Nothing the shadow does
takes a lock production holds, and nothing production does changes the shadow's
manifest. The only shared object is the remote store, which is already
reference-counted per config UUID (`acquireRemoteStore`/`releaseRemoteStore` in
`pkg/controlplane/runtime/shares/blockstore_config.go:649,796`) so two shares
pointing at one remote share a single instance safely.

**Why the payload IDs line up.** `PayloadID` is `shareName/uuid`
(`pkg/metadata/file_helpers.go:23`) and the dump preserves it verbatim. Replaying
the dump into a fresh store reproduces the same `fm:<payloadID>/<offset>` rows
the `f:` records' `FileAttr.Blocks` point at. Nothing is rewritten, which is
what makes this a replay rather than a migration.

**Why a fresh store is required, not optional.** `BadgerMetadataStore.RestoreSnapshot`
returns `ErrRestoreDestinationNotEmpty` unless the target holds no `s:` key
(`pkg/metadata/store/badger/snapshot_store.go:256-264`). A brand-new store is
the only thing that satisfies it, and it is also the only thing that keeps the
snapshot's rows from colliding with production's.

### Routing: a private metadata service per open snapshot

The NBD handler resolves an export name to an `*Export`, which carries a
`Handle` and a `PayloadID`. Reads then call
`h.meta.PrepareRead(authCtx, exp.Handle)`, and `metadata.Service.storeForHandle`
decodes the **share name out of the handle** to pick a store
(`pkg/metadata/service.go:252`).

The obvious routing trick — register the replayed store under a synthetic share
name and let `storeForHandle` (`pkg/metadata/service.go:252`) decode it out of
the handle — **does not work**, for two independent reasons found while
implementing.

**The name does not fit in a handle.** A handle is `"<shareName>:<uuid>"` and
`MaxFileHandleSize` is 64 (`pkg/metadata/types.go:48`), with a 36-byte UUID and
a one-byte separator, so `MaxShareNameLen` is 27. Snapshot IDs are
`uuid.NewString()` (`pkg/controlplane/runtime/snapshot.go:188`) — 36 characters:

```
/export@e3b0c442-98fc-11ef-9c4d-0242ac120002   shareName=44  handle=81  TOO LONG
```

Every operation on such an export would fail at handle mint. Shortening the
name to fit is possible but pointless, because of the second reason.

**A second registration would fork state that must not fork.** `metadata.Service`
is not just a store map. `shareRegistry` holds, per share, a lock manager, a
directory-change notifier, a unified view, a quota row and a writeback flag
(`pkg/metadata/service.go:1016, 1075, 1097, 1102`). Registering the snapshot
under a different name creates a *second* lock manager and a *second* quota row
for what is physically the same set of files — two lock namespaces over one
namespace, which is a correctness bug, not an inefficiency. And it does not
even fix the data: the dump carries `File.ShareName = "/export"` inside every
record (`pkg/metadata/store/badger/encoding.go:327,437`), so the registry key
and the records would disagree, and the paths that compare them
(`badger/shares.go:195`) would be comparing different names.

So a snapshot gets its **own `metadata.Service` instance**, built fresh with
`metadata.New()`, holding only the replayed store registered under the
**production share name**. Nothing else is in it, so there is nothing to
collide with, and `File.ShareName` already matches the key.

```
production                        snapshot (per open snapshot)
┌─────────────────────┐           ┌──────────────────────────┐
│ metadata.Service    │           │ metadata.Service (new)   │
│  registry: /export  │           │  registry: /export only  │
│   → live store      │           │   → replayed store       │
│   → lock manager    │           │   → its own lock manager │
└─────────┬───────────┘           └────────────┬─────────────┘
          │                                    │
          └──────────► remote (S3) ◄───────────┘
                   shared, immutable chunks
```

`Handler` already takes a `meta *metadata.Service` and every handler method
receives it as `h.meta` (`handlers_read.go:16`, `handlers_write.go:26`), so a
snapshot handler is the same `Handler` type over a different service. The
`*Export` it serves is built by the snapshot exporter with the replayed
store's handle.

The `Export.Handle` must therefore be minted **from the replayed store's own
records**, not copied from production: it is `<shareName>:<fileUUID>`, and while
the UUID survives the dump, encoding it against the replayed file is what makes
the handle belong to the shadow. Production's handle would work by accident
(same UUID, same share name) and would silently point at production's service if
the two were ever swapped, so the exporter encodes it explicitly.

Export names follow the shape the existing exporter already accepts, with the
snapshot named explicitly:

```
NBD export name:  export/snapshots/e3b0c442-98fc-11ef-9c4d-0242ac120002/boot
```

A leading `snapshots/` segment after the share is what distinguishes it. That
segment can never collide with a real volume, because `volumes/` is the only
directory the production exporter reads and a volume name cannot contain `/`.

**What this costs.** A `metadata.Service` per open snapshot. It is a struct with
a registry and a handful of trackers — small next to the Badger instance it
wraps — and the number of concurrently open snapshots is expected to be one or
two. It also means the snapshot path cannot reuse
`common.ResolveForWrite` (which asks the runtime's shares service for a block
store by handle); the snapshot's `StoreResolver` returns the shadow
`engine.Store` directly, which is why `StoreResolver` is a function type
(`internal/adapter/nbd/connection.go:59`) rather than an interface.

### Lifecycle of an open snapshot

1. Operator (or the boot rig) requests a snapshot export by name.
2. Resolve the snapshot row; refuse unless `State == StateReady`. A `creating`
   snapshot has no dump yet, and a `failed` one may have a partial manifest.
   Refuse a `degraded` one unless the caller explicitly opts in — it restores,
   but its manifest is short by `DegradedEntries` rows.
3. Take the **shared** side of the per-share snapshot lock, so a concurrent
   delete cannot remove the dump or manifest mid-boot (see below).
4. Create a fresh Badger metadata store at
   `<journalRoot>/<share>/snapshots/<snapID>/shadow-meta`.
5. Replay `metadata.dump` into it via `RestoreSnapshot`
   (`pkg/metadata/snapshotable.go:52`). The store must be empty; that is
   exactly what a new directory gives.
6. Build a fresh `metadata.New()` and `RegisterStoreForShare(shareName, store)`
   under the **production** share name.
7. Acquire the shared remote store (reference-counted).
8. Open an empty journal directory at
   `<journalRoot>/<share>/snapshots/<snapID>/shadow-journal`, and build an
   `engine.Store` with the replayed metadata store as its `FileChunkStore` and
   that journal as its local tier. **`RemoteSync` must be constructed with the
   replayed store too** — `NewRemoteSync` panics on a nil `fileChunkStore`
   (`pkg/block/engine/syncer.go:182`), and it is the object that resolves a cold
   read to a manifest, so pointing it at production's store would hydrate
   production's bytes into a snapshot export.
9. Build the snapshot `Handler` over the new `metadata.Service` and register the
   export in the open-snapshot registry.
10. Serve reads. All reads are cold and hydrate on demand.
11. On close: deregister, release the remote reference, close the engine, close
    and remove the shadow journal and metadata directories, release the shared
    lock.

**The snapshot must be held against deletion for the life of the open export.**
`SnapshotHoldProvider` already owns this mutual exclusion: `HeldHashes` takes
the **read** side of a per-share `sync.RWMutex`
(`pkg/controlplane/runtime/snapshot_hold.go:53,274`) and the delete path takes
the **write** side through `AcquireDeleteLock` (`:238`).

An open export needs the same read side, and **that accessor does not exist
yet** — `AcquireDeleteLock` is the exclusive side and using it would block GC's
mark phase for the whole life of the export. A shared-side accessor
(`AcquireHoldLock`, returning `RLock`/`RUnlock`) must be added alongside it,
borrowing the same `snapshotDeleteLock(shareName)` pointer so it excludes
deletion exactly as `HeldHashes` does. Without it, an operator removing the
snapshot mid-boot deletes the manifest out from under a running VM.


## Promotion to read-write

Today's snapshot export is read-only: the `Export.ReadOnly` flag is already
honoured by every write path — `validateWrite` refuses with `ErrEPERM` at
`internal/adapter/nbd/handlers_write.go:96` and `handleDiscard` at `:202` — and
`FlagReadOnly` is advertised by `Export.TransmissionFlags` at
`internal/adapter/nbd/handshake.go:152`.

Promotion is an operator action, and it is **not** a flag flip. Two facts in
the current code force the shape:

- **NBD transmission flags are one-shot.** `Export.TransmissionFlags`
  (`internal/adapter/nbd/handshake.go:149`) is serialized to the wire exactly
  once, in the handshake at `:370` and `:392`. A client that saw
  `NBD_FLAG_READ_ONLY` will not send a write whatever the server does
  afterwards. Promotion therefore requires the client to **reconnect** — the
  operator promotes, then the VM re-attaches. That is the operational story and
  it must be documented as such, not hidden behind "flip a switch".
- **`Export` is not a place to store mutable state.**
  `RuntimeExporter.Resolve` (`internal/adapter/nbd/volume.go:149-155`) builds a
  fresh `&Export{...}` on every call and nothing caches it; `serve`
  (`internal/adapter/nbd/connection.go:226`) keeps the one copy the handshake
  returned and `pipeline` holds that pointer (`internal/adapter/nbd/pipeline.go:44`).
  Mutating `exp.ReadOnly` would change one connection's view and nothing else.

The read-only decision must therefore live in a **snapshot registry that
`Resolve` consults**, not in the `Export` value. The adapter also has no
live-connection registry today — `pkg/adapter/nbd/adapter.go` exposes only
`Serve` and `Stop`, and connection tracking is `BaseAdapter`'s `connSemaphore` —
so nothing can force a disconnect; the reconnect is the operator's step.

So promotion is:

1. Refuse unless the operator explicitly confirms production is gone. This is
   the split-brain gate and it must be a deliberate, separate act — never
   implied by opening the export.
2. Mark the snapshot promoted **in the registry**. This is the durable record;
   it survives a server restart, which a struct field would not.
3. The client disconnects and re-attaches. `Resolve` now builds the export with
   `ReadOnly: false`, so the new handshake omits `NBD_FLAG_READ_ONLY` and the
   client is willing to write.
4. Writes proceed through the shadow store.

**Why writes are safe without a copy.** Chunks are immutable and
content-addressed: a write mints new chunks and never overwrites a chunk the
snapshot references. The shadow's metadata diverges from the snapshot's; the
blocks are shared. That is exactly Ceph's protected-snapshot clone and ZFS's
`clone`. The snapshot itself stays valid and stays bootable.

**What a promoted write actually needs.** `engine.Store.WriteAt`
(`pkg/block/engine/readwrite.go:89`) appends into the local journal and returns
early — it touches no coordinator and no refcount, so a shadow store built
without a `Coordinator` accepts writes as-is. The coordinator is only consulted
on the reap paths (`Truncate` at `:236` and `:323`, `Delete` at `:437`,
`CopyPayload` at `:596` and `:632`), each of which already nil-checks it. So
promotion needs no new engine plumbing; what it needs is durability (below).

**What promotion must not do:** it must not write into the snapshot's own
`metadata.dump` or `manifest.hashes`. Those files are the immutable record of
the backup. The shadow store's divergence lives in its own metadata store.

**Promotion must not release the GC hold.** The snapshot keeps protecting its
chunks after promotion. The promoted export's *new* chunks are referenced by the
shadow's metadata, which the mark phase enumerates through
`EnumerateFileChunks` (`pkg/block/gc/gc.go:543`); the snapshot's *original*
chunks stay reachable only through `HeldHashes` (`:555`). Dropping the hold at
promotion would let GC reclaim the base blocks the promoted export is still
reading from — the precise silent corruption this design exists to prevent.

**Durability on promotion.** A promoted export's writes need somewhere durable
to land. The shadow's journal is a hydration cache whose contents are already
recoverable from remote; once writes start arriving, its journal holds bytes
that exist nowhere else. The promotion step must therefore either flush to
remote on FUA/FLUSH as production does (the same `common.FlushStableWrite` path
the production handlers already use) or refuse to advertise `FlagSendFUA` until
its journal is durable-backed. Leaving this as-is would advertise a durability
guarantee the promoted export does not keep.

## Costs, stated honestly

- **Cold reads.** A boot off a snapshot hydrates every chunk it touches. On the
  S3-backed host a single cold 4 KiB read costs 13.75 ms (measured), so a boot
  is slow until the shadow's journal warms. This is inherent to reading from a
  remote tier and is the same cost a production cold read already pays.
- **Disk for the shadow journal.** It is a second hydration cache. It must be
  size-capped so opening a snapshot cannot fill the disk production writes to.
- **Memory for the replayed metadata store.** A Badger instance per open
  snapshot. Capped by the number of concurrently open snapshots, which is
  expected to be one or two, not by the number of snapshots retained.
- **Snapshot retention is the operator's lever, not space.** Because chunks are
  shared, N snapshots of a slowly-changing volume cost little more than one.
  Retention still matters: `KeepLast`/`TTL` on the policy is what bounds it.

## What is left to the builder

These are implementation calls that do not change what gets built:

- Whether the snapshot export shares the production NBD adapter's port (routed
  by export name) or gets its own. The export name already distinguishes them;
  a separate port is a security convenience, not a requirement.
- The exact CLI/API surface. `dfsctl share snapshot` already exists; a `boot` or
  `open` subcommand is the natural shape, and a REST endpoint under
  `/api/v1/shares/{share}/snapshots/{id}/` mirrors it.
- Whether promotion is a separate endpoint or a flag on the open call. Separate
  is safer given the split-brain gate.
- Whether the shadow metadata store is kept after close (so a re-open is fast)
  or removed with the shadow journal. Keeping it costs disk per snapshot and
  buys nothing on a first boot, so removal is the simpler default.

Three things that are **not** left to the builder:

- **The shadow gets its own `metadata.Service`.** Registering the replayed store
  in the production service under a synthetic name forks the lock manager and
  the quota row for one namespace, and the name does not fit a file handle
  anyway. See the routing section.
- **`RemoteSync` is built with the replayed store as its `fileChunkStore`.**
  Pointing it at production's store would hydrate production's bytes into a
  snapshot export. `NewRemoteSync` panics on nil, so this cannot be silently
  skipped (`pkg/block/engine/syncer.go:182`).
- **The registry that `Resolve` consults is the source of truth for read-only,
  and its promoted flag is durable.** Putting the flag on the `Export` value, or
  in a per-connection struct, cannot work — see the promotion section. Where the
  registry *lives* is still open; that it exists, is consulted at resolve time,
  and survives a restart is not.

## Verification

A design is only worth building if its failure modes are testable. Each
anti-goal maps to a check:

| Claim | How to prove it |
|---|---|
| A snapshot read never sees a post-snapshot write | Write a pattern, snapshot, overwrite with a different pattern, read through the snapshot export, assert the first pattern. Then read through production and assert the second. |
| Booting a snapshot does not affect production | Run the production write path under load while a snapshot export is read; assert production throughput and correctness are unchanged. |
| GC cannot reclaim a snapshot's chunks | Create a snapshot, overwrite the volume, run GC, boot from the snapshot. The `SnapshotHoldProvider` test suite already covers the hold; this covers the integration. |
| A promoted snapshot shares no mutable state with production | Promote, write to the promoted export, assert the snapshot's own manifest and production's manifest both still describe their original bytes. |
| Promotion needs a reconnect, and the promoted state survives a restart | Promote, assert the still-connected client's writes are refused `ErrEPERM`; reconnect; assert writes now succeed. Then restart the server and assert the reconnected export is still writable. |
| Promotion does not release the GC hold | Promote, overwrite every block the snapshot named, run GC, then read the promoted export's untouched ranges. If the hold were dropped, those reads fail. |
| A snapshot export cannot be reached without the bind gate | The existing `TestServe_RefusesNonLoopbackBindWithoutOptIn` covers the adapter; assert the snapshot export is not exempt. |
| A snapshot cannot be deleted while it is open | Open a snapshot export, attempt `DeleteSnapshot`, assert it blocks or is refused; close the export, assert the delete then succeeds. This is the shared-lock test. |
| A snapshot export reads the snapshot's manifest, not production's | Open a snapshot, write a distinct pattern to production, read the same range through the snapshot, assert the pre-write bytes. Then assert the shadow's `FileChunkStore` is the replayed store and not production's — the wrong wiring is silent, so a behaviour test is required, not a wiring assertion. |
| A snapshot of a non-ready snapshot is refused | Open against a `creating` and a `failed` row; assert both are refused and a `degraded` one is refused without the opt-in. |
| The snapshot's bytes equal production's at capture time | Boot a real kernel from a snapshot export taken from a booted VM, and confirm the guest's filesystem replays and mounts. This is the end-to-end proof and it needs the existing `test/nbd/boot-qemu.sh` rig, extended to point at a snapshot export. |

The last one is the one that matters. Everything else is a component test; that
one is the claim the user actually cares about — a VM boots from the backup.
