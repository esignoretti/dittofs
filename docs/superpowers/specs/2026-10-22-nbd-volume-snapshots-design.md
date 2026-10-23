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
blocks**.

```
production (unchanged)                    snapshot export
┌──────────────────────┐                 ┌──────────────────────┐
│ meta store (live)    │                 │ meta store (replayed │
│  f:, fm: rows        │                 │  from metadata.dump) │
├──────────────────────┤                 ├──────────────────────┤
│ engine.Store         │                 │ engine.Store         │
│  local journal ──────┼──► disk         │  local journal ──────┼──► disk
│  manifest: live      │                 │  manifest: SNAPSHOT  │
└──────────┬───────────┘                 └──────────┬───────────┘
           │                                        │
           └──────────► remote (S3), shared, ◄──────┘
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

### Routing: how a client names a snapshot

The NBD handler resolves an export name to an `*Export`, which carries a
`Handle` and a `PayloadID`. Reads then call
`h.meta.PrepareRead(authCtx, exp.Handle)`, and `metadata.Service.storeForHandle`
decodes the **share name out of the handle** to pick a store
(`pkg/metadata/service.go:252`).

That is the seam. Register each snapshot's shadow metadata store under a
distinct synthetic share name:

```
production:  /export              → live metadata store
snapshot:    /export@snap-abc123  → replayed snapshot store
```

A handle whose share name is `/export@snap-abc123` routes to the snapshot's
store with no change to `metadata.Service`. The **payload ID inside stays
`export/<uuid>`**, because it is not derived from the handle — so the shadow
store's `fm:` rows match the dumped `f:` rows without a rewrite.

Export names follow the same shape the existing exporter already accepts
`<share>/<volume>`, so a snapshot of volume `boot` on share `/export` is:

```
NBD export name:  export@snap-abc123/boot
```

**This routing choice is mine, not the user's.** The alternative — one NBD
adapter instance per open snapshot, each on its own port with its own handler —
is simpler to reason about but burns a port per snapshot and duplicates the
adapter lifecycle. The synthetic-share-name approach reuses one adapter and one
`metadata.Service`; its cost is that `common.ResolveForWrite` would decode
`/export@snap-abc123` and ask the shares service for a block store that has no
share row, so the snapshot path needs its own `StoreResolver` that checks the
snapshot registry first and falls through to production otherwise.
`StoreResolver` is already a function type for exactly this kind of injection
(`internal/adapter/nbd/connection.go:59`). If this proves awkward in
implementation, switching to an adapter instance per snapshot is a contained
change that touches no other part of the design.

### Lifecycle of an open snapshot

1. Operator (or the boot rig) requests a snapshot export by name.
2. Resolve the snapshot row; refuse unless `State == StateReady`. A `creating`
   snapshot has no dump yet, and a `failed` one may have a partial manifest.
   Refuse a `degraded` one unless the caller explicitly opts in — it restores,
   but its manifest is short by `DegradedEntries` rows.
3. Create a fresh metadata store (Badger, on disk under a per-snapshot directory
   so a restart can find it again, or in memory if the design prefers
   simplicity — see "what's left to the builder").
4. Replay `metadata.dump` into it via `RestoreSnapshot`.
5. Register it under `/export@<snapID>` in the metadata service.
6. Acquire the shared remote store (reference-counted).
7. Build an `engine.Store` with that metadata store as its `FileChunkStore`, an
   empty journal directory, and the shared remote.
8. Serve reads. All reads are cold and hydrate on demand.
9. On close: deregister, release the remote reference, close the shadow store,
   remove the shadow journal directory.

**The snapshot must be held against deletion for the life of the open export.**
`SnapshotHoldProvider` already owns the mutual exclusion for this: `HeldHashes`
takes the **read** side of a per-share `sync.RWMutex`
(`pkg/controlplane/runtime/snapshot_hold.go:53,274`) and the delete path takes
the **write** side through `AcquireDeleteLock` (`:238`).

An open snapshot export needs the same read side, and **that accessor does not
exist yet** — `AcquireDeleteLock` is the exclusive side and using it would block
GC's mark phase for the whole life of the export. A shared-side accessor
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

Promotion is an operator action on an open snapshot export:

1. Refuse unless the operator explicitly confirms production is gone. This is
   the split-brain gate and it must be a deliberate, separate act — never
   implied by opening the export.
2. Flip `Export.ReadOnly` to false and clear `FlagReadOnly` from the advertised
   flags.
3. Writes now proceed through the shadow store.

**Why writes are safe without a copy.** Chunks are immutable and
content-addressed: a write mints new chunks and never overwrites a chunk the
snapshot references. The shadow's metadata diverges from the snapshot's; the
blocks are shared. That is exactly Ceph's protected-snapshot clone and ZFS's
`clone`. The snapshot itself stays valid and stays bootable.

**What a promoted write actually needs.** `engine.Store.WriteAt`
(`pkg/block/engine/readwrite.go:89`) appends into the local journal and returns
early — it touches no coordinator and no refcount, so a shadow store built
without a `Coordinator` accepts writes as-is. The coordinator is only consulted
on the reap paths (`Truncate`, `Delete`, `CopyPayload`), each of which already
nil-checks it. So promotion needs no new engine plumbing; what it needs is
durability (next paragraph).

**What promotion must not do:** it must not write into the snapshot's own
`metadata.dump` or `manifest.hashes`. Those files are the immutable record of
the backup. The shadow store's divergence lives in its own metadata store.

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

- Whether the shadow metadata store lives on disk (restartable) or in memory
  (simpler, lost on restart). A boot that outlives a server restart argues for
  disk; the per-snapshot directory already exists as a natural home.
- How the open-snapshot registry is keyed and where it lives (runtime, shares
  service, or the NBD adapter). The routing section states the constraint, not
  the location.
- Whether the snapshot export shares the production NBD adapter's port (routed
  by export name) or gets its own. The export name already distinguishes them;
  a separate port is a security convenience, not a requirement.
- The exact CLI/API surface. `dfsctl share snapshot` already exists; a `boot` or
  `open` subcommand is the natural shape, and a REST endpoint under
  `/api/v1/shares/{share}/snapshots/{id}/` mirrors it.
- Whether promotion is a separate endpoint or a flag on the open call. Separate
  is safer given the split-brain gate.

## Verification

A design is only worth building if its failure modes are testable. Each
anti-goal maps to a check:

| Claim | How to prove it |
|---|---|
| A snapshot read never sees a post-snapshot write | Write a pattern, snapshot, overwrite with a different pattern, read through the snapshot export, assert the first pattern. Then read through production and assert the second. |
| Booting a snapshot does not affect production | Run the production write path under load while a snapshot export is read; assert production throughput and correctness are unchanged. |
| GC cannot reclaim a snapshot's chunks | Create a snapshot, overwrite the volume, run GC, boot from the snapshot. The `SnapshotHoldProvider` test suite already covers the hold; this covers the integration. |
| A promoted snapshot shares no mutable state with production | Promote, write to the promoted export, assert the snapshot's own manifest and production's manifest both still describe their original bytes. |
| A snapshot export cannot be reached without the bind gate | The existing `TestServe_RefusesNonLoopbackBindWithoutOptIn` covers the adapter; assert the snapshot export is not exempt. |
| The snapshot's bytes equal production's at capture time | Boot a real kernel from a snapshot export taken from a booted VM, and confirm the guest's filesystem replays and mounts. This is the end-to-end proof and it needs the existing `test/nbd/boot-qemu.sh` rig, extended to point at a snapshot export. |

The last one is the one that matters. Everything else is a component test; that
one is the claim the user actually cares about — a VM boots from the backup.
