# Address the eight open items

**Status:** ready to execute. Each item's premise was re-measured on 2026-10-31
at HEAD `9ef8cd97`; three of the eight were found to be misdiagnosed in the
README, and those corrections are the most valuable part of this plan.

## How the eight were re-read

The README lists them as "open work" items 1–6 plus two that live only in the
prose (the 4 GiB worst case and the missing write fence). Re-checking each
against the code changed three of them materially:

| # | As recorded | As measured | Verdict |
|---|---|---|---|
| 1 | kernel-boot rig not in CI | true, and `boot-qemu.sh` **refuses TCG** — so CI needs `/dev/kvm`, which GitHub's `ubuntu-latest` runners do not reliably have | **needs a decision, not code** |
| 2 | one missing artifact ("the deploy script") | true — referenced twice in `test/nbd/deploy/config-server2.yaml:26,72` | **write it** |
| 3 | cold reads off a snapshot unmeasured | true; the rig already tracks `elapsed` at `test/nbd/boot-qemu.sh:756` but never prints it | **small, high value** |
| 4 | live server unusable for benchmarking | **misdiagnosed** — the README blames the block cache; the badger `hit-ratio: 0.40` message is the **metadata** cache, and the real cause is a per-READ metadata round trip (below) | **fix the bug, then re-measure** |
| 5 | buffer pooling unbuilt | true; the byte budget bounds concurrency, not allocation | **measure first** |
| 6 | three snapshot capabilities deliberately unbuilt | true, all three documented with reasons | **leave alone, one doc gap** |
| 7 | 4 GiB worst case (64 MiB × 64 conns) | true: `DefaultMaxConnections = 64` × `DefaultMaxInFlightBytes = 64 << 20` | **add a global ceiling** |
| 8 | nothing fences two writers | true: no export-level write lock exists anywhere in the adapter | **document, or build a lock** |

### The finding that reframes item 4

`handleRead` calls `PrepareRead` on **every** READ command
(`internal/adapter/nbd/handlers_read.go:83`). `PrepareRead`
(`pkg/metadata/io.go:602`) calls `s.GetFile`, which calls
`store.GetFile` → `derivePath`. `derivePath`
(`pkg/metadata/store/badger/transaction.go:359`) walks parent edges to the share
root, doing **one Badger `txn.Get` per path component** (plus a `childName`
reverse-edge read per level).

There is already a fast path for exactly this: `GetFileForRead`
(`pkg/metadata/service.go:381`) exists so that "handle-addressed hot paths (NFS
READ/WRITE/GETATTR) that never read `File.Path`" skip the walk. **NFS uses it**
(`internal/adapter/nfs/v4/handlers/read_plus.go:94`,
`internal/adapter/nfs/v4/handlers/commit.go:62`). **SMB uses it**
(`internal/adapter/smb/handlers/read.go:310`). **NBD does not** — it is the last
adapter still calling `PrepareRead`, which goes through the slow `GetFile`.

And NBD does not need `Path` at all: `ReadMetadata` carries only `Attr`
(`pkg/metadata/io.go:48-52`), and `handleRead` discards the result entirely
(`if _, err := ...; err != nil`).

So every NBD READ pays a parent-edge walk it throws away. On the S3-backed host,
where the badger block cache is already thrashing at `hit-ratio: 0.40`, each of
those extra Gets can become a disk read. That is a plausible mechanical
explanation for ~92 IOPS flat, and it is **not** a cache-configuration problem.

This must be measured before it is believed — see Task 4.

---

## Task 1: Make the boot rig's timing printable (item 3, and it unblocks item 1)

**Files:** `test/nbd/boot-qemu.sh`, `test/nbd/README.md`.

`elapsed` already counts 2-second ticks at `:756-768` and is used only for the
timeout branch. It is the boot duration and is thrown away.

**Steps:**

1. Print `boot-qemu.sh: boot took ${elapsed}s` on the PASS path, next to the
   verdict at `:806`.
2. Add `--json` writing `{verdict, boot_seconds, export, hydrated_bytes, tag}`
   to a file given by `--json-out`, so CI and the benchmarks doc can consume it
   without parsing prose. `hydrated_bytes` is read from the server's
   `dittofs_nbd_*` metrics if the metrics endpoint is reachable, else `null` —
   an absent value must be distinguishable from zero.
3. Document both in `test/nbd/README.md` under the existing
   "Booting a snapshot export" section.

**Acceptance:** a `--no-mkfs` boot prints a boot time, and two consecutive runs
against the same fixture print times within the same order of magnitude (a first
boot hydrates, a second does not — so the two numbers are the cold and warm
case, and the doc must say which is which).

**Commit:** `test(nbd): report how long a boot took`

## Task 2: Measure whether the per-READ metadata walk costs anything (item 4, part 1)

**Files:** new `internal/adapter/nbd/metadata_cost_test.go` or a `-bench` in
`bench_test.go`.

**This is a measurement task and must not change behaviour.** Its output decides
Task 3.

**Steps:**

1. Add a benchmark that drives READ through the pipeline with a **real**
   metadata service (the `writeVolumeFixture` harness — `newTestHandler` passes
   nil meta, so the existing benchmarks do not exercise `PrepareRead` at all;
   `benchmarkHandler` at `internal/adapter/nbd/bench_test.go:134` confirms this).
2. Add the same benchmark with the file at the share root (depth 1) and nested
   three levels deep (depth 4), so the parent-walk cost is a visible delta rather
   than a guess.
3. Record the numbers in `docs/nbd-benchmarks.md` with the fixture and Go version
   named, in the same "measured, with the command" register the rest of that file
   uses.

**Acceptance:** the two depths produce a measurable, repeatable difference — or
they do not, in which case Task 3 is dropped and item 4 returns to being a cache
question with a negative result recorded.

**Commit:** `test(nbd): measure what a READ's metadata lookup costs`

## Task 3: Stop the NBD read path deriving a path it discards (item 4, part 2)

**Conditional on Task 2.** If Task 2 shows no difference, do not do this.

**Files:** `internal/adapter/nbd/handlers_read.go`.

**A correction to the first draft of this plan, which matters.** It said to change
`pkg/metadata/io.go`'s `PrepareRead` and audit SMB for fallout. Both halves were
wrong, and checking them changed the shape of the task entirely:

- **NBD is the only production caller of `PrepareRead`.** `grep -rn '\.PrepareRead('
  --include=*.go .` returns exactly one non-test hit:
  `internal/adapter/nbd/handlers_read.go:83`.
- **SMB already uses the fast path.** `internal/adapter/smb/handlers/read.go:310`
  calls `metaSvc.GetFileForRead(...)` and then
  `metaSvc.CheckReadPermissionFile(...)` at `:335`.
- **So does NFS.** `internal/adapter/nfs/v4/handlers/read_plus.go:94-103` does the
  same pair, with the comment `GetFileForRead: handle-addressed, File.Path unused
  — skip derivePath.` The comment on `CheckReadPermissionFile`
  (`pkg/metadata/auth_permissions.go:367-371`) says it exists precisely to replace
  "PrepareRead's redundant inode re-fetch + permission-path re-fetch."

NBD is the last adapter still using the old, slower entry point. The fix is
therefore **local to the NBD handler**, with no shared-function risk at all.

**Steps:**

1. Replace the `PrepareRead` call at `handlers_read.go:83` with the pair the other
   two adapters use: `file, err := h.metaFor(exp).GetFileForRead(ctx, exp.Handle)`
   then `h.metaFor(exp).CheckReadPermissionFile(auth, exp.Handle, file)`.
2. Keep the existing ordering guarantees, which are load-bearing and documented in
   the current code: the permission check must stay **before** the buffer
   allocation at `:88`, so a denied 32 MiB read costs nothing, and the nil
   `h.metaFor(exp)` guard must stay because `newTestHandler` passes nil meta.
3. Keep the regular-file check that `PrepareRead` performed — `GetFileForRead`
   does not do it, and `PrepareRead` returns `ErrIsDirectory` for a directory. Use
   the same shape as NFS (`file.Type != metadata.FileTypeRegular` →
   `readTypeError`). Losing this would silently allow a READ on a directory
   handle, so add a test that asserts it is still refused.
4. Re-run Task 2's benchmark and record the delta.
5. Leave `PrepareRead` in place — it is exported API and its only remaining
   caller would be gone, so note in the commit message that it is now unused by
   production code and should be deleted in a separate change, not this one.

**Acceptance:** the Task 2 benchmark improves, a new test asserts a READ on a
directory handle is still refused, `go test ./internal/adapter/nbd/ ./pkg/metadata/`
passes, and `grep -rn '\.PrepareRead(' --include=*.go . | grep -v _test` returns
nothing.

**Commit:** `perf(nbd): read through the fast path the other adapters use`

## Task 4: A global byte ceiling across connections (item 7)

**Files:** `pkg/adapter/nbd/config.go`, `pkg/adapter/nbd/adapter.go`,
`internal/adapter/nbd/pipeline.go`, `internal/adapter/nbd/connection.go`.

Today `DefaultMaxConnections = 64` (`pkg/adapter/nbd/config.go:24`) and
`DefaultMaxInFlightBytes = 64 << 20` (`:40`) are independent, so the worst case
is 64 × 64 MiB = **4 GiB of payload buffers** on a host with no such guarantee.

**Steps:**

1. Add `Config.MaxTotalInFlightBytes int64` (`mapstructure:"max_total_in_flight_bytes"`,
   default 256 MiB, `0` = no global ceiling). Document it beside the two existing
   knobs and state the multiplication it exists to bound.
2. Add a **shared** `*byteBudget` to `Adapter`, created in `Serve` before the
   listener exists, and passed to each `NewConnection` so every pipeline takes
   from the same budget. `newByteBudget(0)` already means unbounded
   (`internal/adapter/nbd/pipeline.go:191`), so a zero config needs no special
   case.
3. The acquire order must be **global first, then per-connection**, and release
   in the reverse order — the other way round deadlocks two connections that each
   hold their local budget and wait on the global one.
4. Test: two connections, a global ceiling below two per-connection budgets, both
   issuing a large READ; assert the summed peak never exceeds the ceiling. The
   `byteBudget` already tracks `peak`, so the assertion is direct rather than
   inferred.
5. Update `docs/guide/configuration.md` (upstream path, so it belongs in the
   merge) and the adapter's config doc block.

**Acceptance:** the new test fails when the global budget is not shared, with a
message naming the observed peak; `go test -race ./internal/adapter/nbd/
./pkg/adapter/nbd/` passes.

**Commit:** `feat(nbd): bound the payload memory of all connections together`

## Task 5: Decide and document the write fence (item 8)

**Files:** `README.md` (extraction), `docs/superpowers/specs/` (a short note), and
— only if the decision is "build it" — `internal/adapter/nbd/`.

**The measured state:** no export-level write lock exists. `Adapter`
(`pkg/adapter/nbd/adapter.go:25`) holds a config, a runtime and an atomic handler
pointer; the only mutexes in the adapter package are the pipeline's own
`byteBudget` and reply-queue locks. The per-file flush locks in
`pkg/metadata/io.go` and the journal's `sh.flushMu` are in-process and keyed by
file, not by export. **Two clients on one export from two hosts is silent
corruption**, and the README does not say so.

**Steps:**

1. Write the decision down before writing code, because the two options have very
   different costs:
   - **Document it.** NBD's own model has no fencing primitive; the kernel client
     assumes one writer. Say so where an operator will read it, in the section
     that currently discusses `CAN_MULTI_CONN` — which is the flag a reader will
     reach for when they think multi-host is safe.
   - **Build a coarse export lock.** A per-export `sync.Mutex` taken by WRITE,
     TRIM and WRITE_ZEROES would serialize writes across connections and turn
     corruption into slowness. It would also halve the pipelining win, because
     the whole point of Task 11 was several commands in flight — and it would not
     fence a *crashed* client that still holds a TCP connection, which is the
     actual failure mode.
2. Recommended: **document it, and do not build the lock**, because a lock that
   does not survive a client crash provides false assurance. Say that explicitly
   rather than leaving it as an omission.
3. If documenting: add it to the README's `CAN_MULTI_CONN` paragraph and to the
   `docs/nbd-benchmarks.md` caveat about two-writer setups.

**Acceptance:** the README states, in one sentence an operator cannot misread,
that one export has at most one writing client, and what happens if that is
violated.

**Commit:** `docs: one export, one writer — what nothing in this adapter enforces`

## Task 6: Write the deploy script that is referenced but missing (item 2)

**Files:** new `test/nbd/deploy/deploy.sh`; `test/nbd/deploy/config-server2.yaml`
(remove the two "see the deploy script" references or point them at the new file).

`config-server2.yaml:26` says `ReadBufferSize 1024 MiB (set per share, see the
deploy script)` and `:72` says `which is why the deploy script rewrites them from
scratch`. Neither has ever existed.

**Steps:**

1. Read what the config expects the script to do: it must set the per-share
   `ReadBufferSize`, and it must rewrite the redacted secret and host from
   scratch. Both are stated in the config's own comments.
2. Write `deploy.sh` with an explicit `--dry-run` that prints every command it
   would run. This script touches a live host's state; a dry run is the only way
   to review it before it runs.
3. Refuse to run without `--yes` and without an explicit `--host`, and never read
   the host or secret from the config file — the file is redacted precisely so it
   cannot be used to deploy.
4. Document it in `test/nbd/README.md` beside `crash-restart.sh`, including what
   it destroys.

**Acceptance:** `./deploy.sh --dry-run --host example` prints a reviewable command
list and exits 0; without `--yes` it refuses.

**Commit:** `test(nbd): the deploy script the config has always referenced`

## Task 7: A CI gate for the boot rig, or a recorded decision not to have one (item 1)

**Files:** `.github/workflows/integration-tests.yml`, or `README.md` if the
decision is "no".

`boot-qemu.sh` refuses to run under TCG
(`test/nbd/boot-qemu.sh:221-222`, with the reasoning at `:209-211`: a TCG run
"boots and passes while every timing in it is meaningless"). GitHub's
`ubuntu-latest` runners have no `/dev/kvm` reliably. So a per-PR boot job would
either fail or silently degrade to the thing the script exists to refuse.

**Steps:**

1. Measure whether `/dev/kvm` exists on the runner before deciding: add a
   throwaway job that runs `test -e /dev/kvm && nproc` and read the log once.
   Do not guess.
2. If `/dev/kvm` is present: add a job that installs `qemu-utils`, builds the
   fixture, and runs `boot-qemu.sh --no-mkfs` against a pre-made export, gated on
   `test/nbd/**` in the path filter. Budget the 45-minute `timeout-minutes`
   already used by that workflow.
3. If it is absent: record the decision in the README with the measurement, and
   instead add the boot rig to a **nightly** workflow on a self-hosted runner if
   one exists — or leave it manual and say so. The current README already says
   the decision "has not been made"; making it is the deliverable, either way.

**Acceptance:** the README either says "CI runs the boot rig, here" or "CI cannot
run the boot rig because the runner has no KVM, measured on <date>".

**Commit:** `ci: what the boot rig can and cannot run on` (or a README-only commit)

## Task 8: Measure before pooling buffers (item 5)

**Files:** `internal/adapter/nbd/bench_test.go` (a new allocation benchmark),
`docs/nbd-benchmarks.md`.

**Steps:**

1. Add `BenchmarkConnection_ReadAllocs` reporting `-benchmem`
   (`b.ReportAllocs()`), at depths 1 and 8.
2. Record bytes/op and allocs/op. The question a pool must answer is whether
   allocation is a measurable share of the per-command cost; the byte budget
   already bounds the *concurrent* memory, so a pool would only buy GC pressure.
3. Write the result into `docs/nbd-benchmarks.md` under a "Where the time goes"
   heading, with an explicit statement of whether pooling is worth building.
4. Do **not** build the pool in this task. Its ownership discipline is a design
   problem (the README already says so) and is not justified by a number nobody
   has measured.

**Acceptance:** a bytes/op figure exists, and the README's "buffer pooling is
unbuilt" line either stays (with the number beside it) or is replaced by a
justified "not worth building".

**Commit:** `test(nbd): what a READ allocates`

## Task 9: Close the one documentation gap in the snapshot decisions (item 6)

**Files:** `README.md` (extraction).

All three unbuilt snapshot capabilities are documented. The gap is elsewhere:
nothing states that **snapshots are useless for disaster recovery when the block
store is `memory`**, because `RestoreSnapshot` refuses a snapshot whose chunks
are not remote-durable (`pkg/controlplane/runtime/snapshot.go:1427`,
`ErrSnapshotNotDurable`), and the deploy config uses the `memory` store. An
operator reading the snapshot section could reasonably conclude otherwise.

**Steps:**

1. Add one sentence to the snapshot section: a snapshot is only a recovery
   artifact if the share's block store has a remote, and name the check.
2. Cross-reference the durability gate where the deploy config is described.

**Acceptance:** the sentence names `RemoteDurable` and the `memory`-store caveat.

**Commit:** `docs: a snapshot is not a backup unless its chunks left the host`

---

## Order, and why

**1 → 2 → 3** (item 3, then the item-4 measurement, then the fix): Task 1's
timing output is what makes Task 2's numbers interpretable, and Task 3 is
conditional on Task 2 and must not be built on a guess.

**4 and 7 are independent** of the above and of each other.

**5, 6, 9 are documentation-shaped** and can be done in any order; they are the
cheapest and should not be deferred behind the code work.

**8 last**, because its output is a decision about whether to do more work.

## What this plan deliberately does not do

- It does not build buffer pooling (Task 8 measures only).
- It does not build a write fence (Task 5 documents, with the reasoning for
  refusing a lock that a crashed client defeats).
- It does not add CI boot coverage unless `/dev/kvm` is measured to exist.
- It does not revisit the three snapshot capabilities — each has a recorded
  reason and none of them blocks the stated goal.

## The one risk worth naming

Task 3 is much safer than this plan first assumed — it touches one file in one
adapter, not a shared metadata entry point — but it is not free. The `PrepareRead`
call it replaces does three things the fast path does not do by itself: the
regular-file check, the permission check, and the "no allocation on a denied
read" ordering. Dropping any of the three is a behaviour change. The permission
check already has a test (`TestHandleRead_ChecksReadPermission`, one of the four
pinned gates) and would fail loudly. The allocation ordering has no test at all —
it is a comment and a diff away from being lost — and the directory-handle check
has none either. Task 3's steps 2 and 3 exist because of that.
