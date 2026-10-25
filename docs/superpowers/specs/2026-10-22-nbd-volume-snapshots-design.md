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

Two properties keep the design small:

- **A snapshot is always read-only.** It never accepts a write, so it needs no
  durability story, no refcount discipline, and no divergence handling.
- **Getting a writable copy is a separate, explicit operation** — rehydrate —
  which materialises an ordinary volume and leaves the snapshot untouched.

## The finding that shapes everything

**DittoFS already has snapshots.** `pkg/controlplane/runtime/snapshot.go` is a
complete implementation: create, list, show, remove, restore, a scheduler with
`Interval`/`KeepLast`/`TTL` (`pkg/controlplane/models/snapshot_policy.go`), a
durability verify gate, and a restore path with a pre-restore safety snapshot
and crash recovery via a durable restore marker. `dfsctl share snapshot` and
`dfsctl share snapshot-policy` expose all of it today.

So this is not "add snapshots". It is "give an existing snapshot an address on
the read path", plus one operation that turns a snapshot into a volume.

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

**What the guarantee is not.** `CreateSnapshot` drains uploads
(`snapshot.go:563`) so the manifest is complete, but it does not stop production
writes, and nothing between the drain and the dump holds them off. The image is
therefore "the state at the instant the dump was written", not "a clean point in
time". A guest that is shut down cleanly, or that has just issued a FLUSH, gives
a clean snapshot, because that FLUSH is drained by Step 0. That is the operator's
lever and it is worth documenting rather than engineering around.

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
- Turn a snapshot into a new, writable volume when the operator wants to move on
  from the original.
- Remove old snapshots and know what that frees.
- Leave production completely unaffected: no lock it must wait on, no change to
  its cache, no risk of its writes landing in the snapshot.

## Non-goals

- **In-place promotion.** A snapshot never becomes writable. Turning one into a
  writable volume is rehydration, which produces a *new* volume and leaves the
  snapshot immutable. This is the decision that removes most of the complexity
  in this document.
- **Automatic failover.** The operator decides. Rehydrating while production is
  alive creates two volumes with independent metadata over a shared base, which
  is a deliberate clone, not a split brain — but it is still an operator
  decision, not something the server infers.
- **Application-consistent snapshots.** No guest freeze, no VSS, no fsfreeze
  orchestration. The guest filesystem replays its own journal.
- **Single-file recovery from a snapshot.** Recovery is whole-volume: boot it, or
  rehydrate it. Reading one file out of an image is what mounting it as NFS/SMB
  is for.
- **Incremental or differential backup streams.** A snapshot is a reference set,
  not a transfer format. Shipping one off-box is a separate concern.
- **Snapshotting a local-only share.** A shadow store hydrates from a remote
  tier; a local-only share has none, so its snapshot would read as zeros. Same
  gap `RestoreSnapshot` already has (upstream #1718). Refused explicitly, see
  below.
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
- A rehydrated volume silently shares *mutable* state with production.
- A snapshot export is reachable without authentication. NBD has no
  authentication of its own; the existing bind gate is the only control.
- Deleting a snapshot silently succeeds while something is reading it, and the
  reader then fails halfway through a boot.

---

## Part 1 — Booting from a snapshot

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

**A seeding step would change nothing, and this is worth stating so nobody adds
one.** `SeedColdBatch` / `SeedColdFromManifest`
(`pkg/controlplane/runtime/shares/lifecycle.go:767`, called from
`seedColdIfNeeded` for a share whose local tier has no record of a manifest
seed) mark a payload's manifest ranges remote-durable-but-not-local. That is a
state change for residency accounting only: `reconcile` is
`st.Cold || (st.Hole && HasRemoteStore())`, so a seeded range and a hole take
the *same* path on a read. `SeedColdRefs` says so in its own doc — "It changes
no read: a hole on a remote-backed share already reconciles against the manifest
and hydrates exactly as a cold range does" (`pkg/block/engine/flush.go:659-667`).
Seeding would only make the shadow's remote-only byte count and
`OfflineReadiness` describe ranges nobody asked about.

**Why production is untouched.** Separate journal directory, separate metadata
store, separate `engine.Store` instance, shared remote. Nothing the shadow does
takes a lock production holds, and nothing production does changes the shadow's
manifest. The only shared object is the remote store, which is already
reference-counted per config UUID (`acquireRemoteStore`/`releaseRemoteStore` in
`pkg/controlplane/runtime/shares/blockstore_config.go:649,796`), wrapped for the
engine in `nonClosingRemote` (`:311`) so `engine.Close` does not close it out
from under production.

**Why the payload IDs line up.** `PayloadID` is `shareName/uuid`
(`pkg/metadata/file_helpers.go:23`) and the dump preserves it verbatim. Replaying
the dump into a fresh store reproduces the same `fm:<payloadID>/<offset>` rows
the `f:` records' `FileAttr.Blocks` point at. Nothing is rewritten, which is
what makes this a replay rather than a migration.

**Why a fresh store is required, not optional.**
`BadgerMetadataStore.RestoreSnapshot` returns `ErrRestoreDestinationNotEmpty`
unless the target holds no `s:` key
(`pkg/metadata/store/badger/snapshot_store.go:256-264`). A brand-new store is
the only thing that satisfies it, and it is also the only thing that keeps the
snapshot's rows from colliding with production's.

**Why the shadow never needs to be visible to GC.** A read-only export mints no
chunks: every write path refuses before it reaches the block store. So the
shadow's metadata is never a source of *new* references, and the snapshot's own
hashes are already held by `HeldHashes` from the manifest on disk. The shadow
store therefore needs no entry in `sharesSvc.registry` and no seam in
`DistinctRemoteStores` — which is exactly why read-only is the cheap case.

**Why hydration does not write to the metadata store.** `journal.Store.Hydrate`
(`pkg/block/journal/store.go:541`) appends to the *journal index* with
`synced=true` (`:550`) and touches no manifest row. The replayed Badger store is
never written after the replay, so the shadow needs no write path to it, no
rollback, and no cleanup beyond closing it.

### Routing: a private metadata service per open snapshot

The NBD handler resolves an export name to an `*Export`, which carries a
`Handle` and a `PayloadID`. Reads then call
`h.meta.PrepareRead(authCtx, exp.Handle)`, and `metadata.Service.storeForHandle`
decodes the **share name out of the handle** to pick a store
(`pkg/metadata/service.go:252`).

The obvious routing trick — register the replayed store under a synthetic share
name — **does not work**, for two independent reasons.

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
snapshot handler is the same `Handler` type over a different service.

The `Export.Handle` must be minted **from the replayed store's own records**,
not copied from production: it is `<shareName>:<fileUUID>`, and while the UUID
survives the dump, encoding it against the replayed file is what makes the
handle belong to the shadow. Production's handle would work by accident (same
UUID, same share name) and would silently point at production's service if the
two were ever swapped, so the exporter encodes it explicitly.

**Export names.** The existing exporter splits on the *last* slash
(`sep := strings.LastIndexByte(name, '/')`, `internal/adapter/nbd/volume.go:79`),
so for `export/snapshots/<id>/boot` it would take `share =
/export/snapshots/<id>` and `volume = boot` — a share that does not exist, so it
returns `ErrUnknownExport` rather than mis-routing. The snapshot exporter
therefore does not need to *pre-empt* it: it must only be consulted at all. It
claims any name whose second segment is `snapshots`, and any name it does not
claim falls through unchanged, so production's resolver is untouched either way.

```
export/snapshots/e3b0c442-98fc-11ef-9c4d-0242ac120002/boot
└────┬─┘└───┬────┘└──────────────┬──────────────┘└─┬─┘
   share   marker              snapID          volume
```

`snapshots/` can never collide with a real volume, because `volumes/` is the
only directory the production exporter reads and a volume name cannot contain
`/`.

### Lifecycle of an open snapshot

1. Operator (or the boot rig) requests a snapshot export by name.
2. **Check the preconditions, all before creating anything.** Refuse unless
   `State == StateReady`; refuse `degraded` unless the caller opted in (its
   manifest is short by `DegradedEntries` rows); refuse a share whose block
   store has no remote, because there is nothing to hydrate from and every read
   would return zeros. Failing here costs nothing to unwind.

   The no-remote refusal is a guard, not a reachable production state: every
   share must reference a block store, and both store types this release can
   build (`fs` and `memory`) wire a remote — a share configured with no remote
   is refused at `AddShare` ("failed to create remote store"). It is kept
   because the shadow store's correctness depends on it: a share that somehow
   had no remote would produce an export that reads as a blank disk with no
   error, which is the one failure mode worth a cheap precondition even when
   nothing today can reach it. It is therefore tested by unit assertion on the
   guard rather than by an end-to-end fixture, which cannot construct the
   state.
3. Register the snapshot as open (Part 3) so a concurrent delete is refused
   rather than racing the boot.
4. Replay. Create a fresh Badger metadata store at
   `<journalRoot>/<share>/snapshots/<snapID>/shadow-meta` and replay
   `metadata.dump` into it via `RestoreSnapshot` (`pkg/metadata/snapshotable.go:52`).
   This is the expensive step and the only one that can fail slowly.
5. Build a fresh `metadata.New()` and `RegisterStoreForShare(shareName, store)`
   under the **production** share name.
6. Build the shadow `engine.Store`: acquire the shared remote store
   (reference-counted) wrapped in `nonClosingRemote`, open an empty journal at
   `<journalRoot>/<share>/snapshots/<snapID>/shadow-journal`, and pass the
   replayed store as **both** `FileChunkStore` and `RemoteSync`'s
   `fileChunkStore`. This is not optional: `NewRemoteSync` panics on nil
   (`pkg/block/engine/syncer.go:180-182`), and pointing it at production's store
   would hydrate production's bytes into a snapshot export.
7. `Start`, then **cap the shadow journal and allow it to evict** (see below).
8. Build the snapshot `Handler` over the new `metadata.Service` and register the
   export. Only now is the export reachable.
9. Serve reads. All reads are cold and hydrate on demand.
10. On close: deregister, release the remote reference, close the engine, remove
    the shadow journal and metadata directories, release the open registration.

**Failure unwinds in reverse and is allowed to be slow.** Every step after 4
holds a resource, so the open path must be written as an ordered list of
acquired resources with one deferred teardown, not as a function that returns
early. A replay that fails at 90% leaves a partial Badger directory behind, so
the directory is created fresh per attempt (`shadow-meta` is removed on failure,
not reused) — `ErrRestoreDestinationNotEmpty` is exactly the guard that would
otherwise make the second attempt fail for a different reason than the first.

**The shadow journal must evict, and this needs one small engine change.**
`CanEvict()` is `carveActive && IsRemoteHealthy()`
(`pkg/block/engine/sync_health.go:218`), and `carveActive` is true only once the
carve substrate is wired — `SetRemoteBlockStore` plus a block committer derived
from the metadata store (`pkg/block/engine/syncer.go:155`). A read-only shadow
wires neither, so `engine.Start` computes `CanEvict() == false` and calls
`local.SetEvictionEnabled(false)` (`pkg/block/engine/engine.go:248`).

That is fatal to a capped shadow. With eviction held, `evict()` returns
`{Held: true}` immediately (`pkg/block/journal/evict.go:64-65`), and
`ensureSpace` (`pkg/block/journal/evict.go:504`) has nothing evictable, no
drain progress (`unsynced` never moves), so it stalls for `EvictMaxWait` and
then returns `ErrLocalStoreFull` — a boot that dies mid-flight. And the cap
cannot simply be left unset: then the shadow journal grows without bound next to
the volume production writes to.

Hydrated records are written `synced=true` (`Hydrate` →
`appendRecord(..., true, ...)`, `pkg/block/journal/store.go:550`), so every byte
in a shadow journal is recoverable from remote and is genuinely safe to evict.
The fix is one exported method on `engine.Store`:

```go
// SetEvictionAllowed forces the local tier's eviction gate, for a store whose
// every byte is recoverable from the remote — a read-only shadow whose journal
// is a pure hydration cache. A normal store must not use this: its journal
// holds bytes that exist nowhere else, which is why the gate is normally
// derived from carve health.
func (bs *Store) SetEvictionAllowed(allowed bool) { bs.local.SetEvictionEnabled(allowed) }
```

Called with `true` right after `Start`, together with a `MaxLocalBytes` cap. Two
caveats the builder must respect:

- The health callback installed in `Start` (`engine.go:215-231`) re-derives the
gate on every remote-health transition, so a transition after the override
silently turns eviction back off. For a shadow that is tolerable — the shadow
never *needs* to upload, so carving is not an option to wire — but the override
must be reapplied in the callback, or documented as "eviction may be suspended
while the remote is unhealthy", which is also correct.
- The cap comes from the share's `LocalStoreDefaults.MaxSize`, the same field
production's journal uses (`pkg/controlplane/runtime/shares/journal_open.go:51`).
It is per-store, so the shadow's cap is independent of production's — but it is
the same *number* unless the shadow overrides it, which the builder should do
with a dedicated fraction rather than inheriting a multi-gigabyte share size for
a boot cache.

---

## Part 2 — Rehydrating a snapshot into a new volume

A snapshot is never promoted. Turning one into a writable volume is
**rehydration**: a job that creates a *new* volume whose chunks are shared with
the snapshot and whose metadata is its own. The snapshot stays read-only and
stays bootable.

**Rehydration reads no bytes.** It needs the snapshot's *manifest*, not its
contents, because `CopyPayload` reflinks by hash. So the rehydrate path never
builds a shadow `engine.Store` — no journal, no remote, no hydration. It replays
the dump, reads `FileAttr.Blocks`, and hands it to production's store.

### Why the result is independent

Chunks are immutable and content-addressed. `CopyPayload`
(`pkg/block/engine/readwrite.go:536`) creates the destination's per-file
`FileChunk` rows keyed by the **same** offset, hash and size, pointing at the
same CAS objects — O(blocks) metadata puts and zero data movement. A write to
the new volume mints new chunks and never touches one the snapshot references.
That is exactly a reflink, and exactly Ceph's protected-snapshot clone.

**It must run against production's store, not a shadow.** `CopyPayload` returns
`ErrMetadataCoordinatorNotWired` when the source has blocks and the store has no
coordinator (`readwrite.go:596-598`), and it persists the destination's rows
through that coordinator. Production's store has one; a shadow does not. This is
also the right direction: the new volume lives in production.

### The job

1. Validate: the share exists, is not read-only, and has a remote store (chunks
   are addressed by hash inside one remote; without one the copied references
   would resolve to nothing). As built, the target is always the snapshot's own
   share — a snapshot is only readable through its own share's metadata dump, so
   cross-share rehydration would need the dump resolved under the other share's
   name, which is a different feature. The remote check is still written
   explicitly rather than assumed, because it is the invariant that keeps a
   copied reference from resolving to nothing.
2. Register the snapshot as open (Part 3) for the duration, so a delete cannot
   remove the dump mid-job.
3. Replay the dump into a temporary Badger store and read the snapshot volume's
   `FileAttr.Blocks`. This is the only use of the replayed store, and it is
   removed when the job finishes.
4. Create the destination volume file in the target share — an ordinary regular
   file, created the same way the NBD volume path creates one. **The
   destination's payload ID is the new file's, never the snapshot's**:
   `PayloadID` is `shareName/uuid`, so reusing the source's would address the
   source's chunks and leave the new file with no manifest of its own.
5. `productionBS.CopyPayload(ctx, srcPayloadID, dstPayloadID, srcBlocks)` →
   `[]block.ChunkRef`. Note the argument order — **source first, destination
   second** — which reads the other way round from the name.
6. Persist the returned refs as the new volume's `FileAttr.Blocks` in the same
   metadata transaction, then `productionBS.SeedColdRefs(ctx, dstPayloadID, refs)`
   after the commit, so the local index describes the ranges the manifest
   places. Binding the txn in `ctx` (`metadata.TxFromContext`) is what
   `CopyPayload` expects from its clone callers: the per-file rows and the
   `Blocks` write then commit or roll back together
   (`pkg/block/engine/readwrite.go:660-672`).
7. Report the new volume's name. The volume is immediately writable.

**Order matters: replay before create.** Step 3 must succeed before step 4
leaves a file behind, otherwise a dump that fails to replay leaves an empty
volume the operator has to clean up.

**Failure is a rollback, not a repair.** If any step fails, delete the
destination volume and its rows. There is no partial state worth keeping: the
snapshot is untouched throughout, so the operator simply retries.

**What makes the new volume a real GC reference.** `EnumerateFileChunks`
(`pkg/metadata/store/badger/objects.go:876-890`) runs **two** passes over the
share — the `fb:` FileChunk rows, then the `f:` `File.Blocks` manifests — so a
chunk the copy names is marked twice over. `CopyPayload` writes the rows, which
is the pass the read path also resolves through
("The cold-read path resolves bytes via `ListFileChunks(dstPayloadID)`, not
`FileAttr.Blocks`", `pkg/block/engine/readwrite.go:655-657`). The `Blocks` write
in step 6 is therefore belt-and-braces, not the load-bearing part — but it is
still required, because a file whose `Blocks` disagree with its rows is a
correctness bug the next `Truncate` or copy would act on.

**It must be a normal file in a normal share.** Nothing about it is
special-cased for having been rehydrated, so every existing invariant — the
quota, the lock manager, the GC enumeration, the read path — applies to it
unchanged. That is the whole reason rehydration materialises a volume instead
of inventing a second kind of thing.

**Why a job and not a synchronous call.** Step 3 replays a metadata dump and
step 5 writes one row per block. For a large volume that is seconds to minutes,
and the caller wants progress and a status to poll. The existing block-GC job
machinery (`pkg/controlplane/runtime/blockgc_job.go`) is the shape to copy:
submit, poll, terminal state, one durable record.

### What rehydration is not

- It is **not** a restore. `RestoreSnapshot` overwrites the production volume in
  place and needs the share disabled. Rehydration never touches the source.
- It is **not** a move. The snapshot keeps its chunks and its GC hold; the new
  volume's chunks are additionally referenced by the new volume's own metadata,
  which GC sees because it is a real file in a real share.

---

## Part 3 — Removing snapshots and knowing what that frees

Deletion already exists at three levels, so this part adds almost no mechanism —
it closes one race and adds the visibility that is missing.

### What exists

| Layer | Mechanism |
|---|---|
| Manual | `dfsctl share snapshot remove <share> <id>` (`cmd/dfsctl/commands/share/snapshot/remove.go:13`) |
| Retention | `snapshotsched.Service.prune` (`pkg/controlplane/runtime/snapshotsched/service.go:282`) runs after every scheduled snapshot and deletes scheduler-created ready snapshots outside `KeepLast` (count) or `TTL` (age). Both zero disables pruning. Manual snapshots are never pruned. |
| Space | `DeleteSnapshot` (`pkg/controlplane/runtime/snapshot.go:1953`) removes the snapshot dir (`metadata.dump` + `manifest.hashes`) and the DB row, then `recomputePinVersion`. The *chunks* are reclaimed by the next GC run, once the grace period (default 1h) expires and no remaining manifest holds them. Auto-GC runs every 15 minutes (`StartScheduledGC`, `pkg/controlplane/runtime/blockgc_scheduler.go:22`). |

So "remove old snapshots to free space" is already: delete the row, then let GC
reclaim. Nothing new is needed to make that work.

### The race to close: delete while open

`DeleteSnapshot` takes the per-share `snapshotDeleteLock` write side
(`snapshot.go:1974`) and wipes the directory. Nothing stops it running while a
snapshot export is being read — the reader would fail partway through a boot
with a missing manifest.

The wrong fix is to have the open export hold the shared side of that lock for
its lifetime. `DeleteSnapshot` would then **block** for the duration of a boot —
not refuse, block, with no context and no timeout — and because Go's `RWMutex`
blocks new readers once a writer is pending, a second snapshot open on the same
share would hang too. That is worse than the race.

The right fix is a **per-snapshot open registry with refusal**, which is ~30
lines and no new lock type. It is implemented in
`pkg/controlplane/runtime/snapshot_open.go`:

```go
// one map guarded by one mutex
refs map[snapshotKey]int   // key: {share, snapID} → refcount
```

- `OpenSnapshot(ctx, share, snapID) (release func(), err error)` — increments.
  Called by the boot path and the rehydrate job.
- `DeleteSnapshot` checks the count for `{share, snapID}` and returns a new
  sentinel `models.ErrSnapshotOpen` when non-zero. The REST layer maps it to
  `409 Conflict` with the holders' count, so the operator sees *why*.
- Refusal, not blocking, and **scoped to the one snapshot**. Deleting snapshot B
  while snapshot A is open proceeds normally — which is the whole point, since
  retention pruning is exactly the case of deleting a snapshot while another is
  in use.

The `snapshotDeleteLock` keeps its current job — excluding the GC mark phase
from a delete — and gains no new caller.

### The gap: no way to see what a snapshot costs

`dfsctl store block gc-status` shows GC state; `dfsctl share snapshot show`
shows one snapshot's metadata. There is no "how much of the retained set does
this one snapshot actually pin" and no "these 40 snapshots hold 4.8M chunks,
0.3M of them unique to one snapshot."

That falls out of a single pass over the manifests, which `HeldHashes` already
performs — it unions every snapshot's manifest, deduplicating across snapshots
because hourly snapshots of a slowly-changing tree re-reference the same hashes
(`snapshot_hold.go:99-127`). The same walk, counting per-snapshot membership
instead of unioning, answers both questions in one pass.

| Report | Computation |
|---|---|
| Per snapshot | `unique` = hashes in this manifest and no other; `shared` = the rest |
| Per share | `total` = union size; `reclaimable_if_deleted(id)` = that snapshot's `unique`; `reclaimable_now` = hashes no live manifest and no live file references |

**Counts, not bytes, and that is a deliberate limit.** A manifest line is a
hash and nothing else — `snapshot.ReadManifest` parses `block.ContentHash` per
line (`pkg/snapshot/manifest.go:83-96`). Byte sizes live in the remote objects,
not in the manifest, which is why GC's own `BytesFreed` is documented as coming
from the reclaimer rather than the index (`pkg/block/gc/sweep_index.go:37-39`).
Reporting chunk counts is exact and needs no remote calls; converting to bytes
would mean a remote HEAD per distinct hash. So the report gives counts, and the
operator who wants bytes reads `bytes_freed` from the next `gc-status` after a
real delete. Adding a byte estimate later is a `BlockReclaimer` dry-run, not a
new index.

Exposed as `dfsctl share snapshot usage <share> [--id <id>]`, backed by
`GET /api/v1/shares/{name}/snapshots/usage` and
`Runtime.SnapshotUsage` (`pkg/controlplane/runtime/snapshot_usage.go`).
Read-only, no new storage, one manifest walk, no remote traffic.

Three details that came out of building it, each of which changes an answer if
it is got wrong:

- **Every manifest is read even when the report covers one id.** "Unique" means
  "no *other* snapshot references it", so a filtered walk marks every chunk of
  the reported snapshot unique and turns the report into a lie about what a
  delete frees. The filter applies only to what is reported, never to what is
  measured.
- **A snapshot with no readable manifest is named, not counted as zero.** "Holds
  nothing" and "not readable" are opposite answers to "is deleting this free",
  and reporting the second as the first encourages a delete that frees nothing.
- **The report carries the open/holders state from the fence above**, so an
  operator sees why a delete would be refused instead of discovering it by
  trying.

The walk takes the same per-share `snapshotDeleteLock` read side the GC mark
phase does, so a delete in flight cannot remove a manifest mid-walk.

---

## Costs, stated honestly

- **Cold reads.** A boot off a snapshot hydrates every chunk it touches. On the
  S3-backed host a single cold 4 KiB read costs 13.75 ms (measured), so a boot
  is slow until the shadow's journal warms. This is inherent to reading from a
  remote tier and is the same cost a production cold read already pays. It has
  not been measured end to end for a full boot, and should be before promising a
  number.
- **Disk for the shadow journal.** It is a second hydration cache on the same
  volume production writes to. It must be capped *and* allowed to evict — the
  cap alone would turn a large boot into `ErrLocalStoreFull`.
- **Replay cost per open.** `metadata.dump` is replayed row by row into a fresh
  Badger instance on **every** open and every rehydrate. For a large volume that
  is a real delay before the first byte is served. Not cached between opens,
  deliberately: a cache would need invalidation on delete and would pin memory
  for snapshots nobody is booting.
- **Memory for the replayed metadata store.** A Badger instance per open
  snapshot, bounded by concurrently open snapshots (expected: one or two), not
  by snapshots retained.
- **Retention is the operator's lever, not space.** Because chunks are shared, N
  snapshots of a slowly-changing volume cost little more than one. `KeepLast`
  and `TTL` are what bound it; the usage report is what makes the trade visible.

## What is left to the builder

**What is built.** All three parts are implemented on `nbd-on-036`:

| Part | Where |
|---|---|
| Boot a snapshot as a read-only export | `internal/adapter/nbd/snapshot.go`, `Export.Meta`/`store`/`closeFn`, `Handler.metaFor`/`storeFor` |
| Open it as a shadow store | `pkg/controlplane/runtime/snapshot_shadow.go` (`OpenSnapshotAsExport`) |
| Evictable shadow journal | `engine.Store.SetEvictionAllowed` (`pkg/block/engine/engine.go`) |
| Refuse a delete while open | `pkg/controlplane/runtime/snapshot_open.go`, `models.ErrSnapshotOpen` → 409 |
| Rehydrate into a new volume | `pkg/controlplane/runtime/snapshot_rehydrate.go`, `POST .../snapshots/{id}/rehydrate` |
| Usage report | `pkg/controlplane/runtime/snapshot_usage.go`, `GET .../snapshots/usage`, `dfsctl share snapshot usage` |

**What is deliberately not built**, and why each is a decision rather than an
omission:

- **Cross-share rehydration.** A snapshot is readable only through its own
  share's metadata dump, so copying into a different share needs the dump
  resolved under that share's name — a second routing decision, not a parameter.
  Same-share covers the stated goal (restore a VM into a fresh volume).
- **Byte accounting in the usage report.** A manifest line is a hash with no
  size; bytes would need one remote HEAD per distinct chunk. Counts are exact and
  local; `bytes_freed` from `gc-status` after a real delete is the byte answer.
- **A `snapshot open` CLI verb.** Serving a snapshot is not something an operator
  does — it happens because a client connects to
  `<share>/snapshots/<id>/<volume>`. A verb that only minted a name would be a
  no-op with a flag.
- **Caching the replay between opens.** Deliberately uncached: a cache needs
  invalidation on delete and pins memory for snapshots nobody is booting.

Calls that do not change what gets built:

- Whether the snapshot export shares the production NBD adapter's port (routed
  by export name) or gets its own. The export name already distinguishes them; a
  separate port is a security convenience, not a requirement.
- Whether the shadow metadata and journal directories are kept after close (fast
  re-open) or removed. Removal is the simpler default and costs only the replay.
- Where the open-snapshot refcount map lives: runtime or shares service. Built in
  the runtime, because the delete path it fences is a `Runtime` method.

Five things that are **not** left to the builder:

1. **The shadow gets its own `metadata.Service`.** Registering the replayed store
   in the production service under a synthetic name forks the lock manager and
   the quota row for one namespace, and the name does not fit a file handle
   anyway.
2. **`RemoteSync` is built with the replayed store as its `fileChunkStore`.**
   Pointing it at production's store would hydrate production's bytes into a
   snapshot export, and `NewRemoteSync` panics on nil so it cannot be skipped by
   accident.
3. **The shadow journal evicts.** `SetEvictionAllowed(true)` plus a cap. Without
   the former the latter turns a boot into `ErrLocalStoreFull`.
4. **Delete is refused, never blocked, while a snapshot is open** — and the
   refusal is per snapshot, not per share.
5. **A snapshot is never writable.** No code path sets `ReadOnly: false` on a
   snapshot export, and `TransmissionFlags` therefore always advertises
   `FlagReadOnly` for one. Note the one asymmetry that is intentional: `handleFlush`
   (`internal/adapter/nbd/handlers_control.go:20`) does not refuse a read-only
   export, because FLUSH carries no data — a snapshot export answers FLUSH
   successfully as a no-op. Only WRITE, WRITE_ZEROES, TRIM and DISCARD are
   refused, all through `validateWrite`'s `exp.ReadOnly` check.

   Two honest notes on how that guarantee is enforced, both verified while
   building it. First, the read-only decision is made in three independent
   places — `validateWrite` and `handleWriteZeroes`/`handleDiscard` check
   `exp.ReadOnly` directly, and `authContext` passes the same field down as
   `AuthContext.ShareReadOnly`, which the metadata layer enforces for the NFS and
   SMB paths as well. That redundancy is wanted, but it means an over-the-wire
   test of "a write is refused" passes with any one layer removed; the test in
   `internal/adapter/nbd/snapshot_test.go` therefore carries a positive control
   (the same identity writing successfully to the production export), because
   without it a refusal caused by ordinary ownership permissions looks identical
   to one caused by read-only. Second, the shadow `engine.Store` is not itself
   write-refusing: a direct `WriteAt` on it succeeds. Nothing reaches it that way
   today — the only caller is the handler, and every write command is refused
   before the store is touched — but the guarantee lives in the handler, not in
   the store, and that is the layer to re-check if a future caller is added.

## Verification

Each anti-goal maps to a check. The last one is the claim that matters.

| Claim | How to prove it |
|---|---|
| A snapshot read never sees a post-snapshot write | Write a pattern, snapshot, overwrite with a different pattern, read through the snapshot export, assert the first pattern. Then read through production and assert the second. |
| Booting a snapshot does not affect production | Run the production write path under load while a snapshot export is read; assert production throughput and correctness are unchanged. |
| GC cannot reclaim a snapshot's chunks | Create a snapshot, overwrite the volume, run GC, boot from the snapshot. `SnapshotHoldProvider`'s suite covers the hold; this covers the integration. |
| A snapshot export reads the snapshot's manifest, not production's | Open a snapshot, write a distinct pattern to production, read the same range through the snapshot, assert the pre-write bytes. A behaviour test, not a wiring assertion — the wrong wiring is silent. |
| A snapshot export is never writable | Attempt WRITE, WRITE_ZEROES, TRIM and DISCARD through a snapshot export; assert `ErrEPERM` on each. Assert the handshake advertises `FlagReadOnly`. FLUSH is deliberately *not* in this list: it is not a data write, and `validateWrite` lets it through because the shadow has nothing to flush — a FLUSH on a snapshot export succeeds as a no-op, which is correct. |
| A snapshot of a share with no remote is refused, not served as zeros | Assert on the guard directly: no share can be constructed without a block store, and both store types wire a remote, so the refusal is verified by unit assertion rather than by an end-to-end fixture. The invariant it protects — a shadow with no remote reads as a blank disk with no error — is what the guard is for. |
| A snapshot cannot be deleted while it is open | Open a snapshot export, attempt `DeleteSnapshot`, assert `ErrSnapshotOpen` (409) and that the export still reads correctly; close, assert the delete then succeeds. |
| Deleting one snapshot does not affect another that is open | Open A, delete B, assert B is deleted and A still reads correctly. This is the per-snapshot scoping test. |
| A rehydrated volume is independent of its snapshot | Rehydrate, write a distinct pattern to the new volume, then read the same range through the snapshot and assert the original bytes. Assert the snapshot is still openable and still read-only. |
| A rehydrated volume survives the snapshot being deleted | **Done** — `TestRehydratedVolumeSurvivesTheSnapshotBeingDeleted`: rehydrate, delete the snapshot, run GC with grace zero, read the volume back. It carries a **negative control**, because "GC swept nothing" is also what a broken GC reports: deleting the rehydrated volume too must make the same chunks sweepable (measured: 0 swept with the volume present, 2 swept and 2,097,260 bytes freed once it was gone). Without that control the survival assertion passes for two reasons and proves neither. |
| Rehydration refuses a share with no remote | **Guard only, not covered by a test.** Verified while building: a `memory` block store reports `RemoteStore() != nil` even with no remote configured, so the refusal cannot be reached through any fixture this suite can build, and `RehydrateSnapshot` checks the snapshot's existence first anyway. The check stays because the invariant it protects — a copied reference resolving to nothing and reading as zeros — is silent if it is ever broken. |
| Rehydration rollback leaves nothing behind | **Partly.** `TestRehydrateSnapshot_RollsBackOnFailure` covers the caller-context case (a cancelled request must not abort a detached job). The failure-after-create case is implemented in the job but not asserted. |

Two things the tests found that are now fixed in the code rather than only
described:

- **The job reported `done` before releasing its open registration**, so a caller
  that polled to `done` and immediately deleted the snapshot was refused with
  "held open by 1". `runRehydrate` now returns the terminal state instead of
  publishing it, and the goroutine releases the hold *then* publishes — the
  window was microseconds wide and would have read as "you cannot delete what you
  just rehydrated".
- **The usage report filtered snapshots before reading their manifests**, which
  made every chunk of the reported snapshot look unique — a report that says a
  delete frees everything when it frees nothing. Manifests are now always all
  read; the filter applies only to what is reported.
| The usage report matches reality | **Done** — `TestSnapshotUsage_CountsSharingNotSize` builds three snapshots with known overlap and asserts every number against values derived by hand from the fixture, not from a previous run. |
| The snapshot's bytes equal production's at capture time | **Not yet.** The unit-level proof exists (a snapshot export reads the snapshot's manifest, not production's). Booting a kernel from a snapshot export needs `test/nbd/boot-qemu.sh` pointed at a snapshot export name; that is the one end-to-end claim this design makes that has not been run. |
