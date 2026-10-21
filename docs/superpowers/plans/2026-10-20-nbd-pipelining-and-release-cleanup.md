# NBD Pipelining and Release Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the one-command-at-a-time bottleneck in the DittoFS NBD adapter, and close every defect recorded against the current branch, so the branch is mergeable.

**Architecture:** Split `Handler.serve` into three roles that already exist implicitly — a reader that owns the socket's read side, a bounded worker pool that does store work, and a single ordered writer that owns the socket's write side. Handlers stop writing to the socket and return a `reply` value instead. A `barrier` replaces the serialization argument that currently makes FLUSH correct. Cleanup tasks come first so pipelining is built on code that is already correct.

**Tech Stack:** Go 1.26 (the repo's CI floor), `net`, `sync`, `sync/atomic`; buffered channels for the queues, sized by the in-flight limit so the reader blocks rather than the server buffering without bound. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-08-dittofs-nbd-block-protocol-design.md` — the design gate. `docs/superpowers/plans/2026-10-19-nbd-pipelining.md` — the earlier design rationale for pipelining, whose measurements and rejected alternatives this plan relies on. Read both; this plan argues from them and does not repeat their reasoning.

**Where this plan runs.** Execute it in the DittoFS source worktree — `dittofs-nbd` on branch `nbd-on-036` — not in the `NBD-experimental` extraction. The extraction holds only the 39 files the branch touches; it has no `go.mod`, no `pkg/metadata/io.go`, no `test/required-tests.json` and no CI workflows, so `go build ./...` and every path Tasks 3, 6 and 15 name do not exist there. This file is kept in both repositories for the record, and the extraction's copy carries this same note.

---

## Global Constraints

- **Go 1.26.x** — CI matrix floor (`.github/workflows/unit-tests.yml`), not the local toolchain.
- **No new module dependencies.** Everything needed is in the standard library.
- **Scope is `internal/adapter/nbd/` and `pkg/adapter/nbd/`.** No engine, metadata, control-plane, or `cmd/` changes except where a task names one.
- **`NBD_FLAG_CAN_MULTI_CONN` is now ON** (`c03a908b`), which changes one premise this plan was written against. Pipelining is still one connection with several commands in flight — it is not multi-connection cache coherence — but a client that sees the flag may now open several connections, so the depth plateau is a per-connection ceiling rather than a per-export one. Two consequences for the tasks below: `TestIntegration_OneBlockStorePerShare` and `TestIntegration_FlushOnOneConnectionCoversAWriteOnAnother` pin the coherence the flag promises and must keep passing, and the oracle's expected flag word is `0x016d` (it was `0x006d` when this plan was written) because `oracleStartQemuNBD` now passes `--shared=0`.
- **`BLOCK_STATUS` and structured replies stay unimplemented.** Spec §7's residency-truth gate has not moved. [`docs/nbd-block-status.md`](../../nbd-block-status.md) states the gate's cost and the checklist to lift it.
- **`maxBlockSize` is 32 MiB** (`internal/adapter/nbd/payload_size.go`) and is the per-request payload ceiling. It does not change.
- **Existing suite must pass unchanged.** That is the primary regression gate for the pipelining tasks; any test that must change is named in the task that changes it.
- **Every test that pins a fixed defect must be shown to fail before the fix.** Record the failing output in the commit message.
- **All comments and messages in the existing house style**: comments explain *why* and record decisions with a `decision:` prefix where a reasonable alternative was rejected. This codebase's comment density is deliberate — match it, do not thin it.
- **Commit messages**: `fix(nbd):` / `feat(nbd):` / `refactor(nbd):` / `test(nbd):` / `docs(nbd):`, imperative mood, wrapped at 72 columns, with a body that states the defect or the decision.

## Review Focus

Five conditions the spec implies but no existing test exercises, each of which a user would notice as a bug. Each gets a test in the task that owns the code.

1. **A READ is not authorized.** `handleRead` never builds a metadata auth context, so it never calls a permission check; `handleWrite` does (`PrepareWrite` → `checkReadPermission`/`checkWritePermission`). A file whose mode forbids reads is still served in full. Expected: `NBD_EPERM`, as the write path answers.
2. **A FLUSH after pipelining returns before earlier writes are durable.** Today this is free because dispatch is serialized — `handlers_control.go` says so in a comment. Expected: FLUSH's reply is not written until every earlier write's commit has returned.
3. **A malformed WRITE during shutdown desynchronizes the stream.** Fixed for the draining path; the *rejecting* path (`rejectWrite`) and the discard path are the same hazard class and must stay covered. Expected: the next header is read at the right offset.
4. **The export size is a snapshot.** A truncate over NFS/SMB is invisible to a live NBD connection, which then serves reads past EOF as zeroes and accepts writes that extend a file the operator just shrank. Expected: the size is re-read when it can have changed, or the staleness is documented as a hard limitation in the handshake reply.
5. **`MaxConnections: 0` means unlimited, and the semaphore is nil.** `pkg/adapter/base.go:159` only allocates when `> 0`. An unauthenticated protocol on a public interface with no connection cap is one `for` loop away from exhausting file descriptors. Expected: either a documented non-zero default or an explicit refusal to bind remotely with no cap.

---

### Task 1: Give `PunchHole` the request deadline every other block-store call has

**Files:**
- Modify: `internal/adapter/nbd/handlers_write.go:265`
- Test: `internal/adapter/nbd/handlers_write_deadline_test.go` (create)

**Interfaces:**
- Consumes: `common.WithRequestDeadline(ctx context.Context) (context.Context, context.CancelFunc)` — already used at `handlers_write.go:120` and `handlers_read.go:93`.
- Produces: nothing new.

- [x] **Step 1: Write the failing test**

The test must observe that the context handed to `bs.PunchHole` carries a deadline. A fake `BlockStore` records the context it receives.

```go
// internal/adapter/nbd/handlers_write_deadline_test.go
package nbd

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// deadlineRecordingStore records whether PunchHole's context carried a deadline.
// It embeds storeFake so only the one method under test is overridden — and so
// the embedded pointer's counters (punches, flushes) stay observable.
type deadlineRecordingStore struct {
	*storeFake
	punchHoleHadDeadline bool
}

func (s *deadlineRecordingStore) PunchHole(ctx context.Context, payloadID string, currentBlocks []block.ChunkRef, offset, length uint64) ([]block.ChunkRef, error) {
	_, s.punchHoleHadDeadline = ctx.Deadline()
	return s.storeFake.PunchHole(ctx, payloadID, currentBlocks, offset, length)
}

// TestHandleDiscard_PunchHoleCarriesRequestDeadline pins the one block-store
// call in this package that had no deadline. A discard that reaches a remote
// tier and stalls would otherwise hold the connection's goroutine for as long
// as the tier takes, because the command's own context carries no budget.
//
// It needs the real-metadata fixture: handleDiscard calls h.meta.PunchHole
// first, and newTestHandler passes nil for meta. The embedded storeFake keeps
// the real PunchHole's behaviour, so the command completes normally and the
// only thing under test is the context.
func TestHandleDiscard_PunchHoleCarriesRequestDeadline(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := &deadlineRecordingStore{storeFake: newStoreFake()}
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	if rep := writeZeroesThroughClient(t, client, 0, 4096, 0); rep.Error != 0 {
		t.Fatalf("WRITE_ZEROES reply error = %d, want 0", rep.Error)
	}
	if !st.punchHoleHadDeadline {
		t.Fatal("PunchHole was called without a request deadline on its context")
	}
	if st.punches != 1 {
		t.Fatalf("the store saw %d punches, want 1", st.punches)
	}
}
```

Two details that decide whether this test is honest:

- **`storeFake` is embedded by pointer, and the override delegates.** `newStoreFake()` returns `*storeFake` (`connection_test.go:59`). Embedding the value instead of the pointer would leave the recording store with its own zero-valued counters, so `st.punches` would read 0 regardless and the second assertion would be meaningless.
- **The offset/length are inside the export.** `newWriteVolumeFixture` creates a 1 MiB volume (`newIntegrationFixtureAs` → `newIntegrationFixtureAsSized(t, id, 1<<20)`), and `handleDiscard` bounds-checks against `exp.Size` before touching the store — a 4096-byte WRITE_ZEROES at offset 0 passes, while one at offset 0 with length 1 MiB+1 would be answered `NBD_ENOSPC` and the store would never be called.

- [x] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run TestHandleDiscard_PunchHoleCarriesRequestDeadline -v`
Expected: `FAIL` — `PunchHole was called without a request deadline on its context`.

- [x] **Step 3: Apply the deadline**

In `handleDiscard`, wrap the context before the engine call, mirroring `handlers_read.go:93`:

```go
	// The deadline wrapper is applied per command, not per connection, and it
	// is a no-op when the context already carries one. The metadata call above
	// is bounded by its own service timeout; the block-store call is not, so
	// this is the only thing standing between a stalled remote tier and a
	// connection goroutine held open indefinitely.
	punchCtx, cancel := common.WithRequestDeadline(ctx)
	defer cancel()
	if _, err := bs.PunchHole(punchCtx, string(res.PayloadID), res.PreOpBlocks, req.Offset, uint64(req.Length)); err != nil {
```

- [x] **Step 4: Run the test and the package**

Run: `go test ./internal/adapter/nbd/ -run TestHandleDiscard_PunchHoleCarriesRequestDeadline -v && go test -race -count=1 ./internal/adapter/nbd/ ./pkg/adapter/nbd/`
Expected: both `ok`.

- [x] **Step 5: Commit**

```bash
git add internal/adapter/nbd/handlers_write.go internal/adapter/nbd/handlers_write_deadline_test.go
git commit -m "fix(nbd): bound the block-store punch with a request deadline"
```

---

### Task 2: Validate FLUSH's flags, as every other command does

**Files:**
- Modify: `internal/adapter/nbd/handlers_control.go:22-24`
- Test: `internal/adapter/nbd/handlers_control_test.go` (create)

**Interfaces:**
- Consumes: `FlagFUA` (`protocol.go`), the `req.Flags&^allowed != 0` idiom from `handleDiscard` (`handlers_write.go:191`).
- Produces: nothing new.

- [x] **Step 1: Write the failing test**

```go
// internal/adapter/nbd/handlers_control_test.go
package nbd

// Both tests need the real-metadata fixture: handleFlush calls h.commit, which
// calls common.FlushStableWrite and dereferences h.meta. newTestHandler passes
// nil for meta.

// TestHandleFlush_RejectsUndocumentedFlags pins that FLUSH validates its flags
// like every other command. The spec documents FUA on every command once
// SEND_FUA is advertised, and no other flag on FLUSH; a request carrying one is
// malformed and must be answered NBD_EINVAL rather than silently served.
func TestHandleFlush_RejectsUndocumentedFlags(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	sendRequest(t, client, &Request{Type: CmdFlush, Flags: FlagDF, Cookie: 0x51})
	if rep := readReplyHeader(t, client); rep.Error != ErrEINVAL {
		t.Fatalf("FLUSH with FlagDF reply error = %d, want ErrEINVAL (%d)", rep.Error, uint32(ErrEINVAL))
	}
	if st.flushes != 0 {
		t.Fatalf("the store saw %d flushes for a rejected FLUSH, want 0", st.flushes)
	}
}

// TestHandleFlush_AcceptsFUA pins that the one documented flag is still allowed,
// so the new check cannot be "reject every flag".
func TestHandleFlush_AcceptsFUA(t *testing.T) {
	f := newWriteVolumeFixture(t)
	st := newStoreFake()
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	sendRequest(t, client, &Request{Type: CmdFlush, Flags: FlagFUA, Cookie: 0x52})
	if rep := readReplyHeader(t, client); rep.Error != 0 {
		t.Fatalf("FLUSH with FlagFUA reply error = %d, want 0", rep.Error)
	}
}
```

- [x] **Step 2: Run both and confirm the first fails**

Run: `go test ./internal/adapter/nbd/ -run 'TestHandleFlush_' -v`
Expected: `TestHandleFlush_RejectsUndocumentedFlags` FAILS (reply error is 0, not EINVAL); `TestHandleFlush_AcceptsFUA` passes.

- [x] **Step 3: Add the check**

Insert before the length/offset check in `handleFlush`:

```go
	// FUA is accepted because the spec requires the server to accept it on
	// every command once SEND_FUA is advertised; no other flag is documented
	// for FLUSH, so one is a malformed request. This is the same rule
	// handleDiscard applies, and it was missing here only because FLUSH is the
	// one command whose payload is nothing at all.
	if req.Flags&^FlagFUA != 0 {
		return h.writeReply(conn, req.Cookie, ErrEINVAL, nil)
	}
```

- [x] **Step 4: Run the tests**

Run: `go test ./internal/adapter/nbd/ -run 'TestHandleFlush_' -v`
Expected: both PASS.

- [x] **Step 5: Commit**

```bash
git add internal/adapter/nbd/handlers_control.go internal/adapter/nbd/handlers_control_test.go
git commit -m "fix(nbd): reject undocumented flags on FLUSH"
```

---

### Task 3: Authorize READ

**Files:**
- Modify: `internal/adapter/nbd/handlers_read.go` (around line 88-96)
- Modify: `internal/adapter/nbd/connection.go` — `Handler.meta` is already a field; no struct change
- Test: `internal/adapter/nbd/handlers_read_test.go` (create)

**Interfaces:**
- Consumes: `(*metadata.Service).PrepareRead(ctx *metadata.AuthContext, handle metadata.FileHandle) (*metadata.ReadMetadata, error)` at `pkg/metadata/io.go:602` — it validates the file is regular and calls `checkReadPermission` (`pkg/metadata/auth_permissions.go:601`). `(*Handler).authContext(ctx context.Context, exp *Export) *metadata.AuthContext` at `connection.go:118`.
- Produces: nothing new. `handleRead` gains a permission check and returns `ErrEPERM` on denial, matching the write path's mapping.

- [x] **Step 1: Write the failing tests**

Two tests. The first pins the authorization gap; the second pins the discarded return value in the same function, because Task 3 rewrites that call anyway and a short read reported as success is the one failure a filesystem cannot detect.

```go
// TestHandleRead_ChecksReadPermission pins the authorization gap: the write
// path funnels through PrepareWrite, which calls checkWritePermission, but the
// read path built no auth context at all — so a volume file whose mode forbids
// reads was served in full to every client.
//
// NBD carries no identity, so the single synthetic identity either may read or
// may not; the check is still required, because the identity's group and the
// file's mode are the operator's configuration and the adapter's job is to
// honour it.
//
// The assertion is on the wire: a denied READ answers NBD_EPERM with no
// payload. That is what a client acts on, and it is the same answer the write
// path already gives (mapError maps both merrs.ErrPermissionDenied and
// merrs.ErrAccessDenied to ErrEPERM).
//
// It needs the real-metadata fixture: newTestHandler passes nil for meta, so a
// handler built that way has no permission check to exercise.
//
// The chmod runs as integrationIdentity because SetFileAttributes gates a mode
// change on ownership (pkg/metadata/file_modify.go:421 isOwner, :564
// `case isOwner || isRoot:`). Any other identity fails the chmod with a
// permission error and this test would then fail for the wrong reason.
func TestHandleRead_ChecksReadPermission(t *testing.T) {
	f := newWriteVolumeFixture(t)
	metaSvc := f.rt.GetMetadataService()

	// There is no f.authCtx(): the fixture builds its AuthContext inline (see
	// newIntegrationFixtureAsSized, integration_test.go:131), so build one here
	// the same way rather than adding a helper for one test.
	authCtx := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: ptr(integrationIdentity.UID),
			GID: ptr(integrationIdentity.GID),
		},
	}
	// 0o200 leaves write and removes read, for the owner.
	mode := uint32(0o200)
	if _, err := metaSvc.SetFileAttributes(authCtx, f.exp.Handle, &metadata.SetAttrs{Mode: &mode}); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	st := newStoreFake()
	// Keyed by the fixture's real payload ID, not testPayloadID: this export
	// comes from a real volume file, so its ID is whatever the metadata layer
	// assigned.
	st.data[string(f.exp.PayloadID)] = make([]byte, 4096)
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	if rep, _ := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 3, Offset: 0, Length: 4096,
	}); rep.Error != ErrEPERM {
		t.Fatalf("READ on a file with no read bit reply error = %d, want ErrEPERM (%d)", rep.Error, uint32(ErrEPERM))
	}
}
```

The second test below does **not** need the metadata fixture: it is a store-fault test, and `newTestHandler`'s nil `meta` is fine because the permission check is guarded (step 3).

```go
// TestHandleRead_ShortReadIsEIO pins that a store which fills only part of the
// buffer is reported as a fault. The range was already checked against
// exp.Size, so the bytes exist; replying success with a partly-filled buffer
// hands the client zeros in place of its data, which is silent corruption.
//
// It embeds storeFake by pointer and delegates, so the fake's own data map and
// counters stay live — embedding the value would give this store its own
// zero-valued map and the ReadAt below would never be reached for real data.
func TestHandleRead_ShortReadIsEIO(t *testing.T) {
	st := &shortReadStore{storeFake: newStoreFake()}
	st.data[testPayloadID] = make([]byte, 4096)
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	if rep, _ := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 5, Offset: 0, Length: 4096,
	}); rep.Error != ErrEIO {
		t.Fatalf("short READ reply error = %d, want ErrEIO (%d)", rep.Error, uint32(ErrEIO))
	}
}

type shortReadStore struct {
	*storeFake
}

func (s *shortReadStore) ReadAt(ctx context.Context, id string, buf []byte, off uint64) (int, error) {
	return len(buf) - 1, nil
}
```

- [x] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run 'TestHandleRead_' -v`
Expected: `TestHandleRead_ChecksReadPermission` FAILS — reply error is 0 and the payload is present, because no check runs. `TestHandleRead_ShortReadIsEIO` FAILS — reply error is 0 because `n` is discarded.

- [x] **Step 3: Add the check**

In `handleRead`, after the flags and payload-size checks and **before** `buf := make([]byte, req.Length)`, insert:

```go
	// The metadata read path is the only authorization funnel available to a
	// READ. The write path gets one from PrepareWrite; nothing equivalent was
	// called here, so a file whose mode forbids reads was still served in full.
	// NBD carries no client identity, so the identity is the adapter's single
	// synthetic one — but its group and the file's mode are the operator's
	// configuration, and honouring it is not optional.
	//
	// Placed before the allocation for the same reason the bounds checks are:
	// a denied 32 MiB read should cost nothing.
	//
	// The nil guard is not defensive noise: newTestHandler in connection_test.go
	// passes nil for meta, and every store-fault dispatch test would panic
	// without it.
	if h.meta != nil {
		if _, err := h.meta.PrepareRead(h.authContext(ctx, exp), exp.Handle); err != nil {
			return h.writeReply(conn, req.Cookie, mapError(err), nil)
		}
	}
```

Then, while the function is open, fix the short read. Today the return value is discarded (`if _, err := bs.ReadAt(...)`), so a store that fills only part of the buffer gets its reply padded with whatever `make` left there — zeros today, which looks like data. Replace the discard with a check:

```go
	n, err := bs.ReadAt(readCtx, string(exp.PayloadID), buf, req.Offset)
	if err != nil {
		return h.writeReply(conn, req.Cookie, mapError(err), nil)
	}
	// A short read is a store fault, not a short device: the range was already
	// checked against exp.Size, so the bytes exist. Replying with a partially
	// filled buffer would hand the client zeros in place of its data and report
	// success — the one failure mode a filesystem cannot detect.
	if n != len(buf) {
		return h.writeReply(conn, req.Cookie, ErrEIO, nil)
	}
	return h.writeReply(conn, req.Cookie, 0, buf)
```

A short read is a store fault, not a short device: the range was already checked against `exp.Size`, so the bytes exist. `TestHandleRead_ShortReadIsEIO` from step 1 pins it.

- [x] **Step 4: Confirm `mapError` maps the denial to EPERM**

Run: `grep -n "ErrAccessDenied" internal/adapter/nbd/connection.go`
Expected: `case merrs.ErrPermissionDenied, merrs.ErrAccessDenied:` at `connection.go:402`, returning `ErrEPERM`. Confirmed present — no change needed. `mapError` normalizes through `common.NormalizeBlockStoreError`, which walks the wrap chain with `errors.As`, so a metadata `*merrs.StoreError{Code: ErrAccessDenied}` from `PrepareRead` reaches the same arm as a block-store denial. That is what makes the test's expectation of `ErrEPERM` correct rather than hopeful.

- [x] **Step 5: Run the tests**

Run: `go test -race -count=1 ./internal/adapter/nbd/ ./pkg/adapter/nbd/`
Expected: both `ok`. If an existing dispatch test now fails, it is because that test drives a READ with `nil` meta — which the `h.meta != nil` guard covers. Any other failure is a real regression; investigate before continuing.

- [x] **Step 6: Commit**

```bash
git add internal/adapter/nbd/handlers_read.go internal/adapter/nbd/connection.go internal/adapter/nbd/handlers_read_test.go
git commit -m "fix(nbd): authorize READ and reject a short read as EIO"
```

---

### Task 4: Re-read the export size when it can have changed

**Files:**
- Modify: `internal/adapter/nbd/volume.go:146` and the `Resolve` doc comment above it
- Modify: `internal/adapter/nbd/handshake.go:82-95` — the `Export.Size` doc comment
- Modify: `internal/adapter/nbd/integration_test.go:151-153` — the fixture comment that states the old model
- Modify: `internal/adapter/nbd/connection.go` — `Handler.sizeRefresh`, `lastResolve`, the refresh site in `serve`
- Test: `internal/adapter/nbd/volume_test.go` (extend)

**Interfaces:**
- Consumes: `Exporter` (`handshake.go:109`), `RuntimeExporter.Resolve` (`volume.go`).
- Produces: `(*Export).Size` becomes documented as a snapshot, and `Handler` gains a way to refresh it. Two options; the task picks one and records why.

- [x] **Step 1: Establish what the current comment claims**

Run: `sed -n '120,150p' internal/adapter/nbd/volume.go`
Expected: a comment asserting the size "cannot change" because it is "fixed at volume creation". That is false — a volume is an ordinary regular file in a share, mutable over NFS/SMB. The exact claim to find and delete is the `Resolve` doc's "its size is fixed at volume creation, so a later resolve could only re-read a value that cannot change".

- [x] **Step 2: Write the failing test**

```go
// TestExport_SizeIsReReadAfterATruncate pins that a live connection sees a
// truncate. A volume is an ordinary file in a share: the operator can shrink it
// over NFS or SMB while an NBD client is connected, and the adapter held the
// handshake-time size for the life of the connection — so reads past the new
// EOF were served as zeroes and writes extended a file that had just been
// shrunk.
//
// It asserts on the wire, because the reply is where the staleness shows: a
// READ whose range starts past the shrunk size must be refused.
func TestExport_SizeIsReReadAfterATruncate(t *testing.T) {
	f := newWriteVolumeFixture(t)
	// The fixture creates the volume at 1 MiB (newIntegrationFixtureAs).
	if f.exp.Size != 1<<20 {
		t.Fatalf("fixture export size = %d, want %d", f.exp.Size, uint64(1<<20))
	}

	// Shrink the volume file behind the connection's back, as an operator would
	// over NFS or SMB. There is no f.authCtx(): the fixture builds its
	// AuthContext inline from integrationIdentity.
	smaller := uint64(4096)
	authCtx := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: ptr(integrationIdentity.UID),
			GID: ptr(integrationIdentity.GID),
		},
	}
	if _, err := f.rt.GetMetadataService().SetFileAttributes(
		authCtx, f.exp.Handle, &metadata.SetAttrs{Size: &smaller},
	); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	st := newStoreFake()
	// Keyed by the fixture's real payload ID, not testPayloadID: this export
	// comes from a real volume file, so its ID is whatever the metadata layer
	// assigned.
	st.data[string(f.exp.PayloadID)] = make([]byte, 1<<20)
	h := f.newHandler(t, st)
	// 0 means "refresh before every command", which is what makes this test
	// deterministic instead of a race against a one-second window.
	h.sizeRefresh = 0
	client, _ := serveHandler(t, h, f.exp.Name)

	// Past the new EOF. Refused, not served as zeroes.
	if rep, _ := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 4, Offset: 4096, Length: 4096,
	}); rep.Error == 0 {
		t.Fatal("READ past the truncated size was served, so the connection still holds the handshake-time size")
	}
}
```

`ptr` already exists in the package at `volume_test.go:18` (`func ptr[T any](v T) *T { return &v }`), and `integration_test.go:153` already uses it for a `SetFileAttributes` size change — the same call this test makes. `metadata` is already imported by both `volume_test.go` and `integration_test.go`.

Two things about the truncate that decide whether this test is honest:

- **The identity has to be the file's owner.** `SetFileAttributes` resolves ownership at `pkg/metadata/file_modify.go:421` (`isOwner := identity != nil && identity.UID != nil && *identity.UID == file.UID`) and only an owner or root clears the ownership gate at `:560-565`. The fixture creates the volume as `integrationIdentity` (`newWriteVolumeFixture` → `newIntegrationFixtureAs(t, integrationIdentity)`), so an auth context built from `integrationIdentity` is the owner. Any other identity would fail the chmod with a permission error and the test would fail for the wrong reason.
- **A size change is not a mode change.** `onlySettingSize` (`pkg/metadata/file_modify.go:465`) is a distinct branch, so this does not depend on the chmod path Task 3's test uses. Both are legitimate, they just take different routes through the same function.

- [x] **Step 3: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run TestExport_SizeIsReReadAfterATruncate -v`
Expected: `FAIL` — size is still 1 MiB, so the READ at offset 4096 succeeds.

- [x] **Step 4: Implement the refresh**

Re-resolving the export per command is not affordable — it takes a registry lock and decodes a handle on every 4 KiB read, which is the exact cost the handshake's one-time resolve exists to avoid. So: re-resolve on a bounded interval, and document the residual staleness as a limit rather than leaving it as a surprise.

Add to `Handler` in `connection.go`:

```go
	// sizeRefresh bounds how stale Export.Size can be. A volume is an ordinary
	// file in a share, so its size is mutable over NFS and SMB while an NBD
	// connection is live — the handshake's snapshot is not "fixed at volume
	// creation" as the Resolve doc comment claimed. Per-command re-resolution
	// would take a registry lock and decode a handle on every 4 KiB read; a
	// bounded interval is the compromise, and the residual staleness is stated
	// in Export.Size's own doc comment so it is a documented limit.
	sizeRefresh time.Duration
```

Set it to `time.Second` in `NewHandler`. In `serve`'s dispatch loop, before the command switch, declare `lastResolve := time.Now()` alongside the existing `exp` and then:

```go
		// See sizeRefresh. Cheap enough to keep the size honest within a second,
		// rare enough that the 4 KiB read path never pays for it.
		if time.Since(lastResolve) >= h.sizeRefresh {
			if refreshed, err := h.exporter.Resolve(ctx, exp.Name); err == nil {
				exp = refreshed
			}
			lastResolve = time.Now()
		}
```

A failed refresh keeps the previous export rather than failing the command: the export was valid a second ago, and a transient registry error should not error the client's device. Say so in the comment.

**Two behaviours to state in `Export.Size`'s doc, because both are load-bearing for Task 4's test:**

- `sizeRefresh` is a `time.Duration` compared with `>=`, so `0` means "refresh before every command" — that falls out of the comparison with no extra branch, and it is what makes Task 4's test deterministic instead of racing a one-second window.
- A refused refresh keeps the previous export. Document that the bound is "up to one second stale by default", not "always current".

Rewrite the false comment at `volume.go:146` and the `Export.Size` doc at `handshake.go:85-87` to state the snapshot-and-refresh behaviour exactly, including the one-second default bound. Note that the fixture's own comment at `integration_test.go:151-153` ("it is fixed at creation and the handler bounds-checks against it") becomes false once this task lands — update it too, or the next reader will believe the old model.

**Note for Task 9:** the refresh must move into the reader, not stay in the dispatch switch, or the pipeline will re-resolve on the worker path. Leave a comment at the refresh site naming `pipeline` so the Task 9 implementer moves it rather than duplicating it.

**And note for Task 9 the harder half of the same point:** the refresh *reassigns* `exp`, so `exp` cannot become a plain `pipeline` field that workers read — that is a data race the moment the worker count exceeds one, and `go test -race` in Task 11 would catch it as a flake rather than a design error. The reader goroutine owns the current export and stamps each `command` with it (`command.exp`), so a worker never reads shared mutable state. Task 9's `command` type must therefore carry `exp *Export`, not rely on `p.exp`.

- [x] **Step 5: Run the tests**

Run: `go test -race -count=1 ./internal/adapter/nbd/ ./pkg/adapter/nbd/`
Expected: both `ok`.

- [x] **Step 6: Commit**

```bash
git add internal/adapter/nbd/volume.go internal/adapter/nbd/handshake.go internal/adapter/nbd/connection.go internal/adapter/nbd/volume_test.go
git commit -m "fix(nbd): stop treating the export size as immutable"
```

---

### Task 5: Refuse a remote bind with no connection cap

**Files:**
- Modify: `pkg/adapter/nbd/config.go` — `applyDefaults`, `validate`, the `Config.MaxConnections` doc comment, the `Config` type's defaults block (lines 53-63, which lists `MaxMessageSize: 32 MiB` and omits `MaxConnections` entirely)
- Test: `pkg/adapter/nbd/config_test.go` (create)

**Interfaces:**
- Consumes: `Config.AllowRemoteBind`, `Config.MaxConnections`.
- Produces: `const DefaultMaxConnections = 64` in `pkg/adapter/nbd/config.go`; `applyDefaults` maps 0 → `DefaultMaxConnections`; `validate()` returns an error for `AllowRemoteBind && MaxConnections < 0` and for `MaxConnections < -1`.

**The test file is `package nbd_test`, not `package nbd`.** Both existing test files in `pkg/adapter/nbd/` are external (`adapter_test.go:1`, `healthcheck_test.go:1`). That means `applyDefaults` and `validate` — both unexported — are **not reachable** from it. Do not export them just for a test, and do not switch the package. Assert through `New` instead, which is what actually runs them (`adapter.go:55-60`) and which is already how the package's other validation test works (`TestNew_PanicsOnOutOfRangePort`, `adapter_test.go:30`).

- [x] **Step 1: Write the failing test**

```go
// TestNew_RefusesAnUncappedRemoteBind pins the combination the adapter must not
// accept. NBD has no authentication and no per-path scoping: one connection
// returns every byte of the volume. `MaxConnections: 0` used to mean unlimited,
// and pkg/adapter/base.go:159 only allocates the semaphore when the value is
// positive — so an unlimited, unauthenticated, remotely reachable adapter was
// one accept loop away from exhausting file descriptors.
//
// It asserts through New rather than through validate because validate is
// unexported and this file is in package nbd_test. New is also the real caller.
func TestNew_RefusesAnUncappedRemoteBind(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New accepted a remote bind with an explicitly unlimited connection count")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "max_connections") {
			t.Fatalf("panic %q does not name max_connections, so an operator cannot find the setting", msg)
		}
	}()
	nbd.New(nbd.Config{Enabled: true, BindAddress: "127.0.0.1", AllowRemoteBind: true, MaxConnections: -1})
}

// TestNew_ZeroMaxConnectionsDefaultsToACap pins that 0 is no longer "unlimited".
// An operator who wants unlimited must say so with -1, which is explicit rather
// than the accidental meaning of an omitted key.
func TestNew_ZeroMaxConnectionsDefaultsToACap(t *testing.T) {
	cfg := minimalConfig() // MaxConnections left at 0
	cfg.AllowRemoteBind = true
	// Reaching Serve would need a runtime and a listener; the panic check above
	// already proves New validates. This one proves the default is not zero,
	// which is the half that cannot be observed through a panic.
	if DefaultMaxConnections <= 0 {
		t.Fatalf("DefaultMaxConnections = %d, want a positive cap", DefaultMaxConnections)
	}
	// The adapter does not expose its resolved config, so the observable
	// property is that a remote bind with an omitted count is accepted — it is
	// only refused when the count is explicitly unlimited.
	a := nbd.New(cfg)
	if a == nil {
		t.Fatal("New returned nil for a remote bind with the default connection cap")
	}
}
```

The second test is deliberately weak, and it is honest about why: `Adapter.config` is unexported and there is no accessor for `MaxConnections` (the only exported accessors are `Port`, `Protocol`, and the base adapter's). A stronger assertion would need a new accessor, which is a production API added for a test. The property that matters — that 0 no longer means unlimited — is enforced by `applyDefaults` and observed by the fact that a remote bind with 0 is *not* refused while one with -1 is. That pair is the test.

- [x] **Step 2: Run both and confirm they fail**

Run: `go test ./pkg/adapter/nbd/ -run 'TestNew_' -v`
Expected: `TestNew_RefusesAnUncappedRemoteBind` FAILS — `New accepted a remote bind with an explicitly unlimited connection count`. `TestNew_ZeroMaxConnectionsDefaultsToACap` passes already (`DefaultMaxConnections` does not exist yet, so this step does not compile until step 3 adds the constant — add the constant first if you want the failing run to be about behaviour rather than about a missing symbol).

- [x] **Step 3: Implement**

In `applyDefaults`, before the UID/GID block:

```go
	// 0 is no longer "unlimited". The zero value of an omitted key used to mean
	// an uncapped, unauthenticated adapter, and pkg/adapter/base.go only
	// allocates its semaphore when the value is positive — so the default
	// configuration had no limit at all. -1 is the explicit spelling for
	// unlimited, which makes it a decision an operator writes down.
	if c.MaxConnections == 0 {
		c.MaxConnections = DefaultMaxConnections
	}
```

In `validate`, after the port check:

```go
	// A remote bind with no cap is the one combination worth refusing rather
	// than defaulting, because the operator asked for both halves explicitly:
	// -1 is the deliberate spelling of "unlimited", and pairing it with a
	// reachable interface is a decision the adapter should not make silently.
	//
	// This reaches the operator as a panic from New, which is the same path
	// every other invalid config takes (see models.AdapterConfig.Validate for
	// the earlier check that usually catches it).
	if c.AllowRemoteBind && c.MaxConnections < 0 {
		return fmt.Errorf("allow_remote_bind requires a connection limit: set max_connections to a positive value")
	}
	// Any other negative value is a typo, not a policy.
	if c.MaxConnections < -1 {
		return fmt.Errorf("invalid max_connections %d: use 0 for the default, -1 for unlimited, or a positive limit", c.MaxConnections)
	}
```

Also update the `Config.MaxConnections` doc comment at `config.go:77`, which currently reads "0 means unlimited" — that is no longer true and it is the sentence an operator reads.

Two things to know about where this lands:

- **`New` panics on a validation failure.** `pkg/adapter/nbd/adapter.go:55-60` runs `applyDefaults()` then `validate()` and panics with `"invalid NBD config: %v"`. That is the established contract (the values come from the operator's file and the control-plane row), so this task does not change it — but it does mean the new error is a startup panic, not a log line. Say so in the doc comment. Note that `models.AdapterConfig.Validate` (`pkg/controlplane/models/adapter.go:55-77`) checks only the type and the port; it does **not** check `max_connections`, so the earlier gate an operator hits is this one, not the control-plane one. An earlier draft of this task claimed otherwise.
- **`DefaultMaxConnections` does not collide.** `pkg/adapter/nfs/adapter.go:46` already defines `DefaultMaxConnections = 1024`, in a different package. NBD's 64 is lower on purpose: an NBD connection holds a block-store handle and up to 32 MiB of read buffer, where an NFS connection is cheaper. Note the divergence so the next reader does not "fix" it to match. The NFS precedent at `pkg/adapter/nfs/adapter.go:472-477` is worth copying for the comment's reasoning: *"A sane default cap so the accept loop's connSemaphore is always built. Leaving this at 0 (unlimited) lets unauthenticated clients exhaust memory/goroutines."*

- [x] **Step 4: Run the tests**

Run: `go test -race -count=1 ./pkg/adapter/nbd/`
Expected: `ok`. Also confirm nothing else builds a `Config` with a negative count: `grep -rn 'MaxConnections' --include=*.go pkg/adapter/nbd/ cmd/dfs/commands/start.go` — the only writer should be `createNBDAdapter`'s `parsedConfig["max_connections"]` read.

- [x] **Step 5: Commit**

```bash
git add pkg/adapter/nbd/config.go pkg/adapter/nbd/config_test.go
git commit -m "fix(nbd): cap connections by default and refuse an uncapped remote bind"
```

---

### Task 6: Make UID/GID 0 expressible, and delete the dead config surface

**Files:**
- Modify: `pkg/adapter/nbd/config.go` — `applyDefaults`, `Config` field docs, the `Config` type's defaults block (line 62 lists `MaxMessageSize: 32 MiB`), delete `MaxMessageSize`/`DefaultMaxMessageSize`/`DefaultExport`
- Modify: `cmd/dfs/commands/start.go:1052-1080` — `createNBDAdapter`
- Create: `pkg/adapter/nbd/config_test.go` — the package has no config test today (`pkg/adapter/nbd/` holds `adapter.go`, `adapter_test.go`, `config.go`, `healthcheck.go`, `healthcheck_test.go`), so this task creates the file and Task 11 adds to it. **Task 5 also creates this file** and runs first, so in practice Task 6 extends what Task 5 wrote.
- Test: `pkg/adapter/nbd/config_test.go`

**Interfaces:**
- Consumes: `Config.UID`, `Config.GID`, `DefaultUID`, `DefaultGID`.
- Produces: `Config.UIDSet *uint32` / `Config.GIDSet *uint32`; `MaxMessageSize`, `DefaultMaxMessageSize` and `DefaultExport` are deleted.

**The file is `package nbd_test`** (`adapter_test.go:1`, `healthcheck_test.go:1`), so `applyDefaults` is unreachable from it — same constraint as Task 5.

**Asserting through `New` is not enough here.** A UID's *resolved value* cannot be observed through a panic: Task 5's `max_connections` fix is visible because a bad value makes `New` panic, but `uid: 0` is a perfectly valid config that quietly resolves to the wrong identity. `Adapter.config` is unexported and `Serve` builds the identity at `adapter.go:118` (`id := nbd.Identity{UID: a.config.UID, GID: a.config.GID}`) without exposing it. So this task adds a test-only accessor rather than widening the production API: `pkg/adapter/nbd/export_test.go` in `package nbd` with

```go
package nbd

// ConfigForTest exposes the resolved config to the external test package.
// A resolved UID cannot be observed through New (nothing panics for uid: 0),
// and exporting the field would widen the production API for a test.
func (a *Adapter) ConfigForTest() Config { return a.config }
```

Go allows a `package nbd` file and `package nbd_test` files in one directory as long as only the former is `_test.go`-suffixed... which it is, and that is the point: `nbd_test` sees `ConfigForTest`, and `ConfigForTest` sees `a.config`. State this reasoning in the accessor's comment, because the next reader will otherwise ask why one directory has two packages.

- [x] **Step 1: Write the failing test for UID 0**

```go
// TestConfig_UIDZeroIsExpressible pins that "run as root" can be configured.
// 0 doubled as "unset", so an operator who set uid: 0 silently got 65534 —
// the one value the field exists to allow or forbid was the one it could not
// express.
//
// The fix is a set-pointer rather than a sentinel value: every uint32 is a
// legal UID, so no in-band value can mean "unset".
func TestConfig_UIDZeroIsExpressible(t *testing.T) {
	zero := uint32(0)
	a := nbd.New(nbd.Config{Enabled: true, Port: 11089, BindAddress: "127.0.0.1", UID: 0, UIDSet: &zero})
	if got := a.ConfigForTest().UID; got != 0 {
		t.Fatalf("UID = %d, want 0", got)
	}
}

// TestConfig_UIDUnsetStillDefaults pins that the new pointer did not break the
// ordinary case: no uid key at all must still give the unprivileged default.
func TestConfig_UIDUnsetStillDefaults(t *testing.T) {
	a := nbd.New(nbd.Config{Enabled: true, Port: 11089, BindAddress: "127.0.0.1"})
	if got := a.ConfigForTest().UID; got != nbd.DefaultUID {
		t.Fatalf("UID = %d, want DefaultUID (%d)", got, nbd.DefaultUID)
	}
}
```

`ConfigForTest` is the test-only accessor described above, in `pkg/adapter/nbd/export_test.go` (`package nbd`, `func (a *Adapter) ConfigForTest() Config { return a.config }`).

**While adding it, tighten Task 5's deliberately weak test.** `TestNew_ZeroMaxConnectionsDefaultsToACap` (Task 5 step 1) could only assert that `DefaultMaxConnections > 0` and that `New` does not panic, because the resolved value was unobservable. Now it is observable — replace the weak half with the real assertion, and delete the paragraph in that test's doc comment that explains why it is weak:

```go
	a := nbd.New(minimalConfig()) // MaxConnections left at 0
	if got := a.ConfigForTest().MaxConnections; got != nbd.DefaultMaxConnections {
		t.Fatalf("MaxConnections = %d, want DefaultMaxConnections (%d)", got, nbd.DefaultMaxConnections)
	}
```

A test that documents its own weakness is better than one that pretends, but a test that can be made strong should be.

- [x] **Step 2: Run it and confirm it fails**

Run: `go test ./pkg/adapter/nbd/ -run 'TestConfig_UID' -v`
Expected: FAIL — `UID = 65534, want 0` (`UIDSet` does not exist yet, so add the field first if you want the failure to be about behaviour rather than about a missing symbol).

- [x] **Step 3: Implement the sentinel, then wire or delete the dead keys**

Add `UIDSet *uint32` / `GIDSet *uint32` alongside `UID`/`GID` (set by `createNBDAdapter` when the key is present), and change `applyDefaults` to test the pointer rather than the value. Document on `UID`/`GID` that the bare fields are the *resolved* values and the `*Set` pointers are what the config file said, or the next reader will set `UID: 0` and wonder why it became 65534.

`createNBDAdapter` reads the JSON blob with type assertions (`parsedConfig["bind_address"].(string)` and friends), so a numeric key arrives as `float64` — the existing `max_connections` read shows both branches (`cmd/dfs/commands/start.go:1076-1081`). Copy that shape:

```go
	if v, ok := parsedConfig["uid"].(float64); ok {
		u := uint32(v)
		nbdCfg.UID, nbdCfg.UIDSet = u, &u
	}
```

A negative or absurd `float64` becomes a wrapped `uint32` here. That is not a new hazard — the existing `max_connections` read has the same conversion — but `uid` is a security-relevant value, so range-check it and return an error from `createNBDAdapter` rather than silently serving as a different principal than the operator configured.

Then decide each dead key and record the decision in a comment:

- `max_message_size` — **delete it, and do not wire it.** The advertised maximum and the enforced maximum must be the same number: `writeExportInfo` puts `maxBlockSize` on the wire at `handshake.go:332` and `payloadTooLarge` compares against the same constant at `payload_size.go:38`. Making the enforcement side configurable without the advertisement desynchronises them, and a server that advertises 32 MiB and refuses at 8 MiB is a conformance bug that a client discovers by failing. Wiring it properly means threading a value into `ServeHandshake(ctx, conn, ex)` — one non-test call site (`connection.go:200`) and ten test call sites — to expose a knob whose purpose (bounding memory) Task 11's in-flight byte semaphore serves better. Delete `Config.MaxMessageSize` and `DefaultMaxMessageSize`, and state in the config docs that the payload cap is the protocol constant the handshake advertises, not an operator setting.
- `default_export` — **delete it.** `RuntimeExporter.Resolve` has no fallback path and `ServeHandshake` always requires a name: `OptGO` carries one in its payload, `OptExportName`'s payload *is* the name, and a client that asks for none gets `RepErrUnsup`. A config key with no consumer is a promise the adapter does not keep.
- `uid`/`gid`/`timeouts.*` — **wire them** in `createNBDAdapter`, which currently reads only four keys. `a.config.UID`/`GID` are read at `adapter.go:118` and `timeouts()` at `config.go:150`, so the values are consumed once something sets them; `start.go` just never does.

- [x] **Step 4: Verify the wiring by construction**

There is no unit test for `createNBDAdapter` (it needs a control-plane `models.AdapterConfig`). Prove the wiring instead:

Run: `grep -n 'parsedConfig\[' cmd/dfs/commands/start.go`
Then compare against the mapstructure keys on `Config` (`grep -n 'mapstructure:' pkg/adapter/nbd/config.go`) and record both lists in the commit message. A key on `Config` with no `parsedConfig[...]` read is the defect being fixed; a `parsedConfig[...]` read with no `Config` field is a key that does nothing, which is the other half of the same defect. After this task the only `Config` keys without a `parsedConfig` read should be `enabled` (set by `createNBDAdapter` itself, `nbdCfg := nbd.Config{Enabled: true, Port: port}`) and `port` (taken from the row, not the blob).

- [x] **Step 5: Run the tests**

Run: `go build ./... && go test -race -count=1 ./pkg/adapter/nbd/ ./cmd/...`
Expected: build OK, tests `ok`.

- [x] **Step 6: Commit**

```bash
git add pkg/adapter/nbd/config.go cmd/dfs/commands/start.go pkg/adapter/nbd/config_test.go pkg/adapter/nbd/export_test.go
git commit -m "fix(nbd): make uid/gid 0 expressible, wire the dead keys, delete the rest"
```

---

### Task 7: Delete the dead protocol surface

**Files:**
- Modify: `internal/adapter/nbd/protocol.go`
- Test: `internal/adapter/nbd/protocol_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: nothing. Pure deletion.

- [x] **Step 1: Confirm each symbol is unused in production**

The audit found two distinct classes. Verify both with one script rather than a grep, because the naive `grep -v protocol.go` counts the declaration in no file but does count test references — which is exactly the distinction that matters here.

```bash
python3 - <<'PY'
import re, pathlib
proto = pathlib.Path('internal/adapter/nbd/protocol.go').read_text()
syms = re.findall(r'^\t([A-Z][A-Za-z0-9]*)\s+(?:Command|CommandFlags|TransmissionFlags|NBDError|Option|OptionReplyType|InfoType)\s*=', proto, re.M)
files = list(pathlib.Path('internal/adapter/nbd').glob('*.go')) + list(pathlib.Path('pkg/adapter/nbd').glob('*.go'))
src = {f: f.read_text() for f in files}
for s in syms:
    prod = test = 0
    for f, t in src.items():
        n = len(re.findall(r'\b'+s+r'\b', t))
        if n == 0: continue
        if f.name == 'protocol.go': n -= 1  # the declaration
        if f.name.endswith('_test.go'): test += n
        else: prod += n
    if prod <= 0:
        print(f'{"DEAD" if test <= 0 else "TEST-ONLY"}\t{s}\t(test refs: {test})')
PY
```

Expected — exactly these, no more:

| Class | Symbols |
|---|---|
| **DEAD** (0 refs anywhere) | `CmdBlockStatus`, `OptListMetaContext`, `OptSetMetaContext`, `RepMetaContext`, `RepErrPolicy`, `RepErrPlatform`, `RepErrTLSReqd`, `RepErrShutdown`, `RepErrBlockSizeReqd`, `RepErrExtHeaderReqd` |
| **TEST-ONLY** (0 prod refs) | `CmdCache` (1), `FlagDF` (2), `FlagReqOne` (2), `FlagFastZero` (2), `OptStartTLS` (1), `OptStructuredReply` (1) |

Any other symbol in that output is a symbol the audit missed; stop and report it rather than deleting it.

Verified against HEAD: 48 typed constants are declared in `protocol.go`; the script above classifies exactly the ten DEAD and six TEST-ONLY names in the table, and the other 32 are live. Run it before editing — if the split differs, the file has changed since this plan was written.

- [x] **Step 2: Delete the dead ten**

Remove the ten `DEAD` constants from `protocol.go`. **Keep the six test-only ones**: they pin wire values in `protocol_test.go`, and a test that asserts `FlagDF == 1<<2` is the only thing keeping the constants file honest against the spec. Deleting them would delete their tests too, for no gain.

- [x] **Step 3: Delete the one fully dead codec function**

The audit distinguished three classes here, and only one is removable without also deleting a test:

| Function | Class |
|---|---|
| `ReadOptionReplyHeader` (`protocol.go:278`) | **dead** — its only occurrence in the tree is its own doc comment. Delete it. |
| `ReadReply` (`protocol.go:215`) | test-only — 3 refs, all in `connection_test.go:232` and `protocol_test.go:54-56` |
| `Request.Encode` (`protocol.go:162`) | test-only — `connection_test.go:223`, `protocol_test.go:19,48,66` |
| `OptionHeader.Encode` (`protocol.go:236`) | test-only — `handshake_test.go:110,294`, `protocol_test.go:84` |
| `Reply.Encode` (`protocol.go:206`) | test-only — `protocol_test.go` round-trip |

Delete only `ReadOptionReplyHeader`. The test-only ones are the codec's own round-trip fixtures and the test harness's own writers (`sendRequest` uses `req.Encode`, `sendOption` uses `OptionHeader.Encode`) — removing them means rewriting `protocol_test.go` and the harness to test encode and decode through the live paths instead, which is a different task with no defect behind it. Leave them and say so in the commit message.

Note: `writeSimpleReply` (`reply.go:37`) is **live** — `writeReply` calls it at `connection.go:350`. An earlier ledger entry called it dead code; that entry is wrong.

- [x] **Step 4: Run the full package**

Run: `go build ./... && go test -race -count=1 ./internal/adapter/nbd/`
Expected: `ok`. A build failure means a symbol was live after all — restore it.

- [x] **Step 5: Commit**

```bash
git add internal/adapter/nbd/protocol.go internal/adapter/nbd/protocol_test.go
git commit -m "refactor(nbd): remove protocol constants and codecs nothing uses"
```

---

### Task 8: Extract the `reply` type; handlers stop owning the socket

**Files:**
- Create: `internal/adapter/nbd/reply_value.go`
- Modify: `internal/adapter/nbd/handlers_read.go`, `handlers_write.go`, `handlers_control.go`, `connection.go`
- Test: all existing tests must pass unchanged

**Interfaces:**
- Consumes: `NBDError`, `Request`, `Export`, `BlockStore`, `Handler`.
- Produces:
  ```go
  // reply is one command's answer, produced by a handler and written by the
  // connection's single writer. The handler no longer touches the socket, which
  // is what makes several commands in flight possible: two handlers writing to
  // one conn would interleave a header and its payload.
  type reply struct {
      cookie  uint64
      nbdErr  NBDError
      payload []byte
  }
  ```
  and every handler becomes
  ```go
  func (h *Handler) handleRead(ctx context.Context, bs BlockStore, exp *Export, req *Request) reply
  func (h *Handler) handleWrite(ctx context.Context, bs BlockStore, exp *Export, req *Request, data []byte) reply
  func (h *Handler) handleFlush(ctx context.Context, bs BlockStore, exp *Export, req *Request) reply
  func (h *Handler) handleTrim(ctx context.Context, bs BlockStore, exp *Export, req *Request) reply
  func (h *Handler) handleWriteZeroes(ctx context.Context, bs BlockStore, exp *Export, req *Request) reply
  func (h *Handler) handleDiscard(ctx context.Context, bs BlockStore, exp *Export, req *Request, allowed CommandFlags, outOfRange NBDError) reply
  ```

**The split is not "handlers stop touching the socket". It is "handlers stop *writing* to the socket."** That distinction is load-bearing and an earlier draft of this task got it wrong in a way that would not compile against Task 9:

- `handleWrite` currently **reads** its payload from `conn` at `handlers_write.go:87-104` (`SetReadDeadline`, then `io.ReadFull(io.LimitReader(conn, …))`). That read belongs to the **reader**, which is the only goroutine allowed to move the stream forward. So `handleWrite` takes `data []byte` — already read, already sized — and the reader does the read.
- Every handler **writes** through `h.writeReply(conn, …)`. That is what moves to `reply`.
- `rejectWrite` (`handlers_write.go:297-316`) both writes a reply and drains a payload. It splits in two: the reply becomes a `reply` value, and the drain becomes `func (h *Handler) drainWritePayload(conn net.Conn, length uint32) error`, which already exists and already does exactly that. `drainWritePayload` stays with the reader, which owns the socket.

- [x] **Step 1: Add the type**

Create `reply_value.go` with the type above and a `func (r reply) write(conn net.Conn, h *Handler) error` that arms the write deadline and writes header then payload — the body currently in `Handler.writeReply` (`connection.go:341-356`).

- [x] **Step 2: Convert one handler and keep the loop serial**

Convert `handleRead` only — it neither reads nor drains, so it is the clean case. In `serve`, replace its call site with:

```go
		case CmdRead:
			if err := h.handleRead(ctx, bs, exp, req).write(conn, h); err != nil {
				return err
			}
```

Run: `go test -race -count=1 ./internal/adapter/nbd/`
Expected: `ok`, unchanged. This is the proof that the extraction is behaviour-preserving for one handler.

- [x] **Step 3: Move the WRITE payload read into the reader**

This is the step the pipelining depends on, and the one an earlier draft omitted. `handleWrite`'s payload read has to leave the handler **before** Task 9 exists, or the reader and the handler both read the same socket.

`serve` currently dispatches `CmdWrite` straight to `handleWrite`, which reads the payload itself. Split it in place — no new helper is needed:

```go
// validateWrite runs every check that decides whether the payload should be
// read at all: flags, read-only, zero length, the advertised maximum, overflow,
// and the range. It returns a reply and true when the request is refused, in
// which case the payload is still in flight and must be drained; false means
// the request is served and the caller owns the payload read.
func (h *Handler) validateWrite(ctx context.Context, bs BlockStore, exp *Export, req *Request) (reply, bool)
```

`serve`'s `CmdWrite` arm then becomes the only place in the adapter that reads a payload:

```go
		case CmdWrite:
			// The payload read moves here, to the loop, because this is the only
			// goroutine allowed to advance the stream. handleWrite keeps the store
			// calls and gains the already-read bytes.
			//
			// A rejected WRITE still has its payload in flight — the spec requires
			// the client to send length bytes whatever the server answers — so the
			// drain stays here too.
			if pre, refused := h.validateWrite(ctx, bs, exp, req); refused {
				if err := pre.write(conn, h); err != nil {
					return err
				}
				if err := h.drainWritePayload(conn, req.Length); err != nil {
					return err
				}
				continue
			}
			data, err := h.readWritePayload(conn, req)
			if err != nil {
				return err
			}
			if err := h.handleWrite(ctx, bs, exp, req, data).write(conn, h); err != nil {
				return err
			}
```

with

```go
// readWritePayload reads exactly req.Length bytes of WRITE payload. It arms the
// read deadline first — the client may send the header and then stall — which is
// why this cannot be a bare io.ReadFull at the call site.
func (h *Handler) readWritePayload(conn net.Conn, req *Request) ([]byte, error)
```

and `drainWritePayload` keeps its current signature and its current refusal to arm a deadline (`handlers_write.go:307-329`); the `Read` budget for a drain is armed by whoever calls it, which here is `validateWrite`'s caller — so move the `conn.SetReadDeadline` that `rejectWrite` currently arms (`handlers_write.go:299`) into this arm, before the drain.

**The invariant to hold is one sentence: `handleWrite` never calls `ReadFull`.** Assert it mechanically at the end of this step:

```bash
grep -n 'ReadFull\|LimitReader\|SetReadDeadline' internal/adapter/nbd/handlers_*.go
```
Expected: no output. Every stream read is in `connection.go` (or, after Task 9, `pipeline.go`).

Run: `go test -race -count=1 ./internal/adapter/nbd/ ./pkg/adapter/nbd/`
Expected: both `ok`, no test changed. This step is the riskiest in the task — `TestConnection_RejectedHugeWriteDoesNotAllocate`, `TestConnection_RejectedWriteKeepsTheStreamSynchronized`, `TestConnection_DrainingWriteDrainsPayload` and `payload_size_test.go` all exercise the rejected-write drain, so a mistake here fails loudly rather than silently.

**Note for Task 9:** the `pre.write` + `drain` pair above is exactly what becomes `command.pre` — the reader refuses, drains, and hands the pipeline a command carrying the ready reply instead of writing it inline. Do not design a second mechanism there; move this one.

- [x] **Step 4: Convert the rest**

Do `handleFlush`, `handleTrim`, `handleWriteZeroes`, `handleDiscard` the same way. Every `return h.writeReply(conn, req.Cookie, X, nil)` becomes `return reply{cookie: req.Cookie, nbdErr: X}`; every `return h.writeReply(conn, req.Cookie, 0, buf)` becomes `return reply{cookie: req.Cookie, payload: buf}`.

The places that keep the socket are `drainWritePayload` (a read) and the `CmdDisc`/draining branches in `serve`. Keep them and say why in a comment.

- [x] **Step 5: Run the full package and the oracle**

Run: `go test -race -count=1 ./internal/adapter/nbd/ ./pkg/adapter/nbd/ && go test -run Oracle ./internal/adapter/nbd/ -v`
Expected: both `ok`; the oracle's transmission-flag words unchanged (`DittoFS = 0x016d`, `qemu-nbd = 0x0d6d`, `ours-only = 0x0000`).

- [x] **Step 6: Commit**

```bash
git add internal/adapter/nbd/reply_value.go internal/adapter/nbd/handlers_*.go internal/adapter/nbd/connection.go
git commit -m "refactor(nbd): handlers return a reply instead of writing to the socket"
```

---

### Task 9: Add the pipeline with a single worker, behaviour identical

**Files:**
- Create: `internal/adapter/nbd/pipeline.go`
- Modify: `internal/adapter/nbd/connection.go` — `serve` delegates to the pipeline
- Create: `internal/adapter/nbd/pipeline_test.go`

**Interfaces:**
- Consumes: `reply` and the converted handlers from Task 8.
- Produces:
  ```go
  // pipeline owns one connection's three roles. maxInFlight == 1 reproduces the
  // serial loop exactly, which is what makes it the regression gate for this
  // change: the existing suite runs against it unchanged.
  type pipeline struct {
      h           *Handler
      conn        net.Conn
      bs          BlockStore
      exp         *Export
      ctx         context.Context
      maxInFlight int
      barrier     *barrier
  }

  func newPipeline(h *Handler, conn net.Conn, bs BlockStore, exp *Export, ctx context.Context, maxInFlight int) *pipeline

  // run reads commands, dispatches them, and writes replies until the client
  // disconnects, the context is cancelled, or the stream fails. It returns the
  // error that ended the connection, or nil for a clean NBD_CMD_DISC.
  func (p *pipeline) run() error

  // command is one request plus the payload the reader already consumed for it.
  // data is nil for every command that carries no payload, and for a WRITE the
  // reader refused before reading one (the refusal reply rides along in pre).
  //
  // exp is stamped per command by the reader rather than read from p.exp: the
  // size refresh (Task 4) reassigns the export on the reader goroutine, so a
  // worker reading p.exp would race it. Each command carries the export that was
  // current when its header was read, which is also the right semantics — a
  // command is bounds-checked against the size the client could have known.
  type command struct {
      req  *Request
      exp  *Export
      data []byte
      pre  *reply // non-nil when the request was refused before dispatch
  }
  ```
  and, in `pipeline.go`:
  ```go
  // barrier is the FLUSH ordering guarantee. Serialized dispatch gave it for
  // free: a command was handled and replied before the next was read, so every
  // already-replied write was committed. A pool breaks that argument, so the
  // guarantee becomes explicit — FLUSH waits until the counter of submitted but
  // uncommitted writes reaches zero.
  type barrier struct {
      mu      sync.Mutex
      cond    *sync.Cond
      pending uint64
  }
  func newBarrier() *barrier
  func (b *barrier) submitted()
  func (b *barrier) committed()
  func (b *barrier) wait()
  ```

- [x] **Step 1: Write the failing test**

```go
// TestPipeline_WritePayloadIsConsumedInOrder pins the property the whole design
// rests on. A WRITE's payload is inline in the stream, so a reader that parses
// the next header before consuming it reads a header out of payload bytes. The
// symptom appears on a later, unrelated command — which is why this is tested by
// asserting the third command's reply, not the second's.
func TestPipeline_WritePayloadIsConsumedInOrder(t *testing.T) {
	// WRITE 4 KiB of 0xAA at 0; WRITE 4 KiB of 0xBB at 4096; READ 4 KiB at 4096.
	// Assert three replies, cookies in order, and the READ's payload is 0xBB.
}
```

- [x] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run TestPipeline_WritePayloadIsConsumedInOrder -v`
Expected: FAIL — `pipeline` is undefined.

- [x] **Step 3: Implement `barrier` and `pipeline` with `maxInFlight: 1`**

Reader goroutine: loop reading a header, run the `CmdDisc` and draining branches **verbatim from `serve`**, then — for a WRITE that passed validation — read the payload (Task 8 step 3's `readWritePayload`, now living here), and hand a `command` to the pool. Worker: `switch req.Type` calling the converted handlers **with `cmd.exp`**, then hand the `reply` to the writer channel. Writer: single goroutine, `reply.write(conn, h)`.

The export the pipeline starts with is `p.exp` (the handshake's resolve). The reader replaces its own copy when the size refresh fires, which is why `command.exp` exists — see the `command` doc above. Workers never touch `p.exp`.

**The `CmdDisc` and draining branches move into `run` as part of this step, not later.** They have to: once `serve` delegates, a branch left behind in `serve` is unreachable, and a connection whose drain answers stopped being sent would silently regress `TestConnection_DrainingAnswersESHUTDOWN` and `TestConnection_DrainingWriteDrainsPayload`. Copy them unchanged — at `maxInFlight: 1` they behave identically, which is what makes this a refactor. Task 13 then adds the one thing the copy does not yet do: let already-queued work finish before `run` returns.

**Two pieces of `serve` are not branches, and they are the ones most likely to be dropped in a mechanical move.** Neither appears in the `switch`, so neither shows up when you diff the dispatch:

- **The read-deadline arming** (`connection.go:222-245`): `if !h.draining() { budget := h.timeouts.Read; if h.timeouts.Idle > 0 { budget = h.timeouts.Idle }; conn.SetReadDeadline(time.Now().Add(budget)) }`. This is the only thing bounding an idle connection. It moves into `run`'s reader loop, immediately before `ReadRequest`, guarded by `!h.draining()` for the same reason it is guarded today — `initiateShutdown` has already put a 100 ms deadline on the connection and re-arming the full budget would hold the drain open (`pkg/adapter/base.go:452`). Copy the existing comment with it; it is the one place that explains the `Idle` vs `Read` split, and the `Idle == 0` means "no idle timeout" case is a real behaviour, not a nicety.
- **`h.degraded.Add(1)`** on a store-resolution failure (`connection.go:215`). That is above the loop, so it stays in `serve` — but note that `TakeDegraded` is what the healthcheck reads, so if a later refactor moves resolution into `run`, the counter has to move with it or the adapter reports healthy while every connection fails.

**Verification that nothing was dropped:** after step 4, `grep -n 'SetReadDeadline\|CmdDisc\|draining()' internal/adapter/nbd/connection.go` should show only the handshake's `SetDeadline`; and `go test -run 'TestConnection_(Draining|Idle)' -v ./internal/adapter/nbd/` should pass, which is what catches the deadline arming going missing (an idle connection with no deadline never returns, and `TestConnection_IdleDeadlineClosesASilentConnection` (`connection_test.go:1593`) is the direct gate).

Use buffered channels sized `maxInFlight` so the reader blocks when the pool is full — that is the backpressure, and it is also the memory bound at `maxInFlight: 1`.

Errors: a reader error closes the work channel and is returned from `run` after the writer drains; a writer error cancels a `context.WithCancel` derived from `p.ctx` so workers stop.

- [x] **Step 4: Point `serve` at the pipeline with `maxInFlight: 1`**

Replace everything from the `for {` loop onward with `return newPipeline(h, conn, bs, exp, ctx, 1).run()`, keeping the handshake and the store resolution above it. The connection's `SetDeadline`/`SetReadDeadline` arming moves into the pipeline's reader, which is the only thing that reads.

Run: `go test -race -count=1 ./internal/adapter/nbd/ ./pkg/adapter/nbd/`
Expected: **both `ok` with no test changed.** This is the gate; a failure here means the refactor changed behaviour.

Also confirm nothing was left orphaned in `serve`:

```bash
grep -n 'CmdDisc\|draining()\|SetReadDeadline' internal/adapter/nbd/connection.go
```
Expected: only the handshake's `SetDeadline` and the `draining` closure's definition. A `CmdDisc` or `draining()` hit inside `serve` means a branch was copied rather than moved.

- [x] **Step 5: Commit**

```bash
git add internal/adapter/nbd/pipeline.go internal/adapter/nbd/connection.go internal/adapter/nbd/pipeline_test.go
git commit -m "refactor(nbd): dispatch through a pipeline with one worker"
```

---

### Task 10: Make FLUSH a real barrier

**Files:**
- Modify: `internal/adapter/nbd/pipeline.go`, `handlers_control.go`
- Modify: `internal/adapter/nbd/handlers_control.go` — delete the "correct because dispatch is serialized" comment block
- Test: `internal/adapter/nbd/pipeline_test.go`

**Interfaces:**
- Consumes: `barrier` from Task 9.
- Produces: `pipeline.run` calls `p.barrier.wait()` on the reader goroutine before queueing a `CmdFlush`; for the three mutating commands — `CmdWrite`, `CmdTrim`, `CmdWriteZeroes` — it calls `submitted()` before dispatch and `committed()` after the store call returns. `handleFlush` itself is unchanged apart from deleting its "serialized" comment — it still just commits.

- [x] **Step 1: Write the failing test**

```go
// TestPipeline_FlushIsABarrier is the load-bearing test of this whole plan. It
// is the one property serialized dispatch gave for free and a plausible-looking
// pool gets wrong: FLUSH must not reply before every write submitted earlier has
// reached the store.
//
// The fake store blocks a WRITE's commit on a channel the test controls, so the
// assertion is causal rather than a timing guess.
func TestPipeline_FlushIsABarrier(t *testing.T) {
	// Store fake: WriteAt blocks until released.
	// Send WRITE, then FLUSH.
	// Assert no FLUSH reply arrives while the WRITE is blocked.
	// Release the WRITE. Assert the FLUSH reply then arrives.
}
```

- [x] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run TestPipeline_FlushIsABarrier -v`
Expected: FAIL — the FLUSH reply arrives while the write is still blocked.

- [x] **Step 3: Wire the barrier**

In `pipeline.run`'s dispatch, for `CmdWrite`, `CmdTrim` and `CmdWriteZeroes`: `p.barrier.submitted()` before handing the command to the pool, `p.barrier.committed()` after the handler returns and **before** the reply is queued.

Note the list is three commands, not four: there is no `CmdDiscard`. `protocol.go:33-42` defines `CmdRead` 0, `CmdWrite` 1, `CmdDisc` 2, `CmdFlush` 3, `CmdTrim` 4, `CmdCache` 5, `CmdWriteZeroes` 6, `CmdBlockStatus` 7. `handleDiscard` is a shared **helper** that `handleTrim` and `handleWriteZeroes` both call (`handlers_write.go:186`, reached from `:155` and `:174`), not a command — so it must not appear in a dispatch switch at all. An earlier draft of this task listed it as one, which would not compile.

For `CmdFlush`, the wait belongs in the **dispatch**, not in `handleFlush` — the handler has no `pipeline` and must not grow one, or it becomes untestable without a socket:

```go
		case CmdFlush:
			// Wait for every write submitted before this command to reach the
			// store. Coarse on purpose: it waits for all in-flight writes, not
			// only earlier ones, which with a bounded pool is at most
			// maxInFlight writes and is obviously correct. The precise version
			// is a later optimisation if this measures badly.
			//
			// It runs on the reader goroutine, before the FLUSH is queued. That
			// is what makes it a barrier: queued-behind writes cannot start
			// while it waits, so it cannot be outrun by a command submitted
			// later in the stream.
			p.barrier.wait()
```

An earlier draft of this plan put `p.barrier.wait()` inside `handleFlush`; that does not compile, because the converted handler signature (Task 8) is `(ctx, bs, exp, req) reply` with no pipeline in scope.

Delete the "correct without tracking which writes are outstanding because dispatch is serialized" paragraph in `handlers_control.go` and replace it with a pointer to `barrier`.

- [x] **Step 4: Run the tests**

Run: `go test -race -count=1 ./internal/adapter/nbd/`
Expected: `ok`, and `TestPipeline_FlushIsABarrier` passes.

- [x] **Step 5: Commit**

```bash
git add internal/adapter/nbd/pipeline.go internal/adapter/nbd/handlers_control.go internal/adapter/nbd/pipeline_test.go
git commit -m "fix(nbd): make FLUSH a real barrier instead of relying on serialization"
```

---

### Task 11: Raise the worker count and bound in-flight bytes

**Files:**
- Modify: `internal/adapter/nbd/pipeline.go`, `connection.go`, `pkg/adapter/nbd/config.go`, `cmd/dfs/commands/start.go`
- Test: `internal/adapter/nbd/pipeline_test.go`, `pkg/adapter/nbd/config_test.go`

**Interfaces:**
- Consumes: `pipeline` from Task 9.
- Produces: `Config.MaxInFlight int` (`mapstructure:"max_in_flight"`, default 4, max 64) and `Config.MaxInFlightBytes int64` (`mapstructure:"max_in_flight_bytes"`, default 64 MiB). `pipeline` gains a byte budget — a `sync.Mutex` + `sync.Cond` counter in the same style as `barrier`, **not** a `chan struct{}`: a channel of empty structs cannot express "N bytes held", and a channel of `int64` would have to be re-filled on every release.

- [x] **Step 1: Write the failing tests**

Two tests, because the worker count introduces two distinct hazards: memory (below) and reply-to-cookie correspondence.

```go
// TestPipeline_BackpressureBoundsMemory pins the property that makes a pool
// safe on a small host. Each in-flight WRITE holds req.Length bytes and each
// in-flight READ allocates req.Length bytes, and the advertised maximum is
// 32 MiB — so an unbounded pool turns a fast client into an OOM. Commands must
// block when the budget is exhausted, not be refused: a refused write is a
// data-loss bug, a delayed one is just slow.
func TestPipeline_BackpressureBoundsMemory(t *testing.T) {
	// maxInFlightBytes = 64 KiB. Send 8 x 32 KiB writes, then 8 x 32 KiB reads.
	// Assert peak buffered bytes never exceeds 64 KiB and all 16 complete.
}

// TestPipeline_ConcurrentReadsAreConsistent pins that out-of-order completion
// does not mispair a reply with a cookie. NBD simple replies are matched by
// cookie, not position, so a worker pool legitimately returns them out of
// order — which means a single swapped cookie corrupts a filesystem silently,
// with no protocol error to notice it by. Each READ here targets a range whose
// contents are derived from the cookie, so a swap is detectable.
func TestPipeline_ConcurrentReadsAreConsistent(t *testing.T) {
	// maxInFlight = 16. For cookie i in 0..31, READ 512 bytes at offset i*512
	// from a store whose bytes are 0x00+i.
	// Assert every reply's cookie i carries payload byte 0x00+i.
}
```

- [x] **Step 2: Run both and confirm they fail**

Run: `go test ./internal/adapter/nbd/ -run 'TestPipeline_(Backpressure|ConcurrentReads)' -v`
Expected: `BackpressureBoundsMemory` FAILS — all 8 are buffered at once. `ConcurrentReadsAreConsistent` may pass by luck before the pool is widened (with one worker, replies are inherently in order); confirm it still passes after step 4, where it becomes meaningful.

- [x] **Step 3: Implement the semaphore and the config**

A weighted semaphore over a channel of `int64` is awkward; use a small `sync.Mutex` + `sync.Cond` counter in the same style as `barrier`, since the pattern is already established in this file. Release the budget in the writer, after the reply is written — that is when the payload buffer's ownership ends.

Two hazards, both of which hang the connection rather than failing a test cleanly:

- **Acquire before the payload read, not after.** The point of the budget is to bound the bytes the reader has already committed to buffering. If the reader reads 32 MiB and *then* asks for permission, the memory is spent before the gate is consulted. So the reader acquires `req.Length` bytes first, reads the payload, and the writer releases the same amount once the reply is written.
- **READ allocates too, so it takes the budget as well.** `handleRead` does `buf := make([]byte, req.Length)` at `handlers_read.go:88` — after its own bounds checks, but on the **worker**, unbudgeted. A byte semaphore that only covers WRITE would leave `maxInFlight` concurrent READs free to allocate `maxInFlight × 32 MiB` with the budget untouched, which is the same OOM the semaphore exists to prevent. So the reader acquires for any command whose payload is `req.Length` bytes in either direction: WRITE (buffered by the reader) and READ (buffered by the worker). FLUSH, TRIM, WRITE_ZEROES and DISC carry no payload and acquire nothing. Release stays in one place — the writer, after the reply is written — so both directions unwind through the same path.
- **Every acquire must be matched by a release on every path.** A command that never reaches the writer — reader error, worker panic (Task 12), drain branch — leaks its budget, and enough leaks wedge the connection at zero bytes forever. Route the release through the same place the reply is disposed of, and make `barrier`-style `Cond.Broadcast` on release so a blocked reader wakes.

`Config.applyDefaults`: `MaxInFlight` 0 → 4, clamp to 64; `MaxInFlightBytes` 0 → 64 MiB. `validate`: reject negative values. Wire both in `createNBDAdapter`.

- [x] **Step 4: Raise the default**

Change `serve`'s `newPipeline(..., 1)` to `newPipeline(..., h.maxInFlight)`.

Run: `go test -race -count=1 ./internal/adapter/nbd/ ./pkg/adapter/nbd/`
Expected: both `ok`.

- [x] **Step 5: Commit**

```bash
git add internal/adapter/nbd/pipeline.go internal/adapter/nbd/connection.go pkg/adapter/nbd/config.go cmd/dfs/commands/start.go internal/adapter/nbd/pipeline_test.go pkg/adapter/nbd/config_test.go
git commit -m "feat(nbd): allow several commands in flight, bounded in bytes"
```

---

### Task 12: Recover a worker panic into that command's reply

**Files:**
- Modify: `internal/adapter/nbd/pipeline.go`
- Test: `internal/adapter/nbd/pipeline_test.go`

**Interfaces:**
- Consumes: `pipeline`.
- Produces: nothing new on the wire.

- [x] **Step 1: Write the failing test**

```go
// TestPipeline_WorkerPanicRepliesEIO pins that a panic in one command does not
// kill the process or the connection. Today the recover covers the whole
// dispatch loop, so a panic costs the connection; with a pool it has to move
// into the worker, or one bad command takes every in-flight command with it.
func TestPipeline_WorkerPanicRepliesEIO(t *testing.T) {
	// Store fake panics on WriteAt.
	// Send WRITE (panics) then READ.
	// Assert the WRITE's cookie gets NBD_EIO and the READ still succeeds.
}
```

- [x] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run TestPipeline_WorkerPanicRepliesEIO -v`
Expected: FAIL — the process panics out of the test.

- [x] **Step 3: Implement**

Wrap the handler call in the worker:

```go
	// A panic is one command's failure, not the connection's. The recovery that
	// used to cover the whole dispatch loop lives here now: the command is
	// answered NBD_EIO, which is the honest answer for "the server failed to
	// serve this", and the connection stays usable for the commands queued
	// behind it.
	defer func() {
		if r := recover(); r != nil {
			// log with the cookie, the command type, and the panic value
			select {
			case replies <- reply{cookie: cmd.req.Cookie, nbdErr: ErrEIO}:
			case <-p.ctx.Done():
				// The writer is gone; nothing can consume this reply.
			}
		}
	}()
```

The `select` is not decoration. A bare `replies <-` in a deferred recover blocks forever if the writer has already returned — which is exactly the state a panic tends to arrive in — and a goroutine parked in a deferred send never reaches its `WaitGroup.Done()`, so shutdown hangs instead of failing. Send with an escape hatch.

Keep `Conn.Serve`'s existing recover as the outer backstop; do not delete it.

- [x] **Step 4: Run the tests**

Run: `go test -race -count=1 ./internal/adapter/nbd/`
Expected: `ok`.

- [x] **Step 5: Commit**

```bash
git add internal/adapter/nbd/pipeline.go internal/adapter/nbd/pipeline_test.go
git commit -m "fix(nbd): contain a worker panic to its own command"
```

---

### Task 13: Drain in-flight work on shutdown

**Files:**
- Modify: `internal/adapter/nbd/pipeline.go`
- Test: `internal/adapter/nbd/pipeline_test.go`

**Interfaces:**
- Consumes: `pipeline`, `Handler.draining`, the base adapter's 100 ms read deadline (`pkg/adapter/base.go:452`).
- Produces: nothing new.

- [x] **Step 1: Write the failing test**

```go
// TestPipeline_ShutdownDrainsInFlight pins that a command already being served
// when shutdown begins still gets its reply. A client that has sent a write and
// is waiting for the answer must not see a closed socket: that is the difference
// between a paused device and an errored one, which is the entire point of
// answering NBD_ESHUTDOWN rather than hanging up.
func TestPipeline_ShutdownDrainsInFlight(t *testing.T) {
	// Slow store write: the store blocks in WriteAt on a channel the test closes.
	// Start the write through the pipeline. Once the worker is demonstrably
	// inside WriteAt, cancel the context (NOT close the client conn — closing it
	// makes the write fail, which is a different assertion).
	// Then release WriteAt and assert the WRITE's reply still arrives, with no
	// error, on the still-open connection.
}
```

The trigger has to be context cancellation with the connection alive. Cancelling *and* closing the socket would pass against today's code, because today's loop returns as soon as the read fails and the client only notices the hangup — the test would then be asserting the wrong thing.

- [x] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/adapter/nbd/ -run TestPipeline_ShutdownDrainsInFlight -v`
Expected: FAIL — the reply never arrives, because `run` returns on `ctx.Done()` without waiting for the worker.

- [x] **Step 3: Implement the shutdown ordering**

On cancellation, `run` must go through three phases and only then return:

1. **Stop admitting.** Cancel a derived `workCtx` the reader selects on, and close the work channel once the reader has exited. No new command is queued after this point.
2. **Let queued work finish.** `wg.Wait()` on the workers. Every command already handed to a worker runs to completion and posts its reply. This is the phase whose absence the test catches.
3. **Let the writer drain.** Close the reply channel, `wg.Wait()` the writer, then return.

Two ordering traps to write down in the code:

- **Do not `return` early on `ctx.Done()`.** The current loop returns on cancellation, which is precisely the behaviour being replaced. The reader's exit condition becomes "ctx done or read error", and the exit *unwinds through* phases 2 and 3 rather than returning.
- **Close the work channel exactly once, and only after the reader has stopped sending.** Closing it while the reader may still be in a send is a `send on closed channel` panic. The natural shape: reader exits → `close(work)` → workers see the closed channel and return → `close(replies)` → writer returns.

The `NBD_ESHUTDOWN` answers that the serial loop gave to commands arriving during shutdown are **unchanged and stay in the reader's copy of the drain branch** (Task 9 step 3 moved it there). What this task adds is only that a command already *in* the pipeline is finished rather than abandoned. Note the interaction with `pkg/adapter/base.go:452`: `interruptBlockingReads` arms a 100 ms read deadline on every active connection at shutdown, so the reader's blocking read fails within ~100 ms regardless. Do not arm a longer deadline here to "give the drain more time" — that would hold the connection open past the base adapter's budget, and it is why the drain branch must not overwrite the shutdown deadline (the same reasoning as the P0 fix at `connection.go:276-296`).

- [x] **Step 4: Run the tests**

Run: `go test -race -count=1 ./internal/adapter/nbd/ ./pkg/adapter/nbd/`
Expected: both `ok`.

- [x] **Step 5: Commit**

```bash
git add internal/adapter/nbd/pipeline.go internal/adapter/nbd/pipeline_test.go
git commit -m "fix(nbd): finish in-flight commands before closing a draining connection"
```

---

### Task 14: Write the missing `bench-fio.sh` and measure

**Files:**
- Create: `test/nbd/bench-fio.sh`
- Modify: `test/nbd/README.md` — document it
- Modify: `docs/superpowers/plans/2026-10-19-nbd-pipelining.md` — append measured results

**Interfaces:**
- Consumes: `fio` on the client host; the same `nbd-client`/`qemu-nbd` rig the other scripts use.
- Produces: `test/nbd/bench-fio.sh` — the script the pipelining plan has cited since it was written and which does not exist.

- [x] **Step 1: Write the script**

It must reproduce the baseline table in the pipelining plan exactly: random 4 KiB read at depth 1/8/16/32/64, sequential read 64 MiB, sequential write 64 MiB, fsync-per-4 KiB. Follow `run-oracle.sh`'s structure for argument handling, `set -euo pipefail`, and its refusal to run without the binaries it needs.

Signature it must present:

```
Usage: bench-fio.sh <host> <port> <export> [--depth N] [--runtime SECONDS]
```

Defaults: every depth in `1 8 16 32 64`, 20 seconds per run. It prints one `depth<TAB>iops` line per depth plus the three sequential numbers, so two runs can be diffed with `paste`. Refuse to run if `fio` is absent, matching `oracleRequireBinary`'s message style.

- [x] **Step 2: Establish the baseline**

Run it against the **pre-pipelining** adapter — check out the commit before Task 11's worker-count change, or run against the source worktree at `nbd-on-036` — on the same host the plan's numbers came from:

```bash
bash test/nbd/bench-fio.sh <host> 10809 <share>/<volume> | tee /tmp/bench-before.txt
```

Expected: within noise of 5,328 / 8,880 / 8,955 / 9,245 / 8,918. If it is not, the environment differs (different host, different backing tier, different chunk size) and the comparison in step 3 is invalid — say so in the results instead of comparing across environments.

- [x] **Step 3: Measure the pipelined adapter**

```bash
bash test/nbd/bench-fio.sh <host> 10809 <share>/<volume> | tee /tmp/bench-after.txt
paste /tmp/bench-before.txt /tmp/bench-after.txt
```

The falsifiable prediction: **depth 32 must exceed depth 8 by a wide margin**, and **depth 1 must barely move** — a serialized connection at depth 1 has one command in flight either way, so a material depth-1 gain means the change is doing something other than what it claims. If depth 32 does not move, the 1.18 ms is CPU-bound inside the store rather than latency-bound, the plan's premise was wrong, and that is the finding to report.

- [x] **Step 4: Record the result in both documents**

Append a results section to the pipelining plan with both tables and the verdict. Update `test/nbd/README.md` with the command and what it measures.

- [x] **Step 5: Commit**

```bash
git add test/nbd/bench-fio.sh test/nbd/README.md docs/superpowers/plans/2026-10-19-nbd-pipelining.md
git commit -m "test(nbd): add the fio benchmark the pipelining plan cites, and measure"
```

---

### Task 15: Make the NBD tests a CI gate

**Files:**
- Modify: `test/required-tests.json`
- Modify: `.github/workflows/integration-tests.yml` — a `qemu-utils` install step and the `go` path filter (option 2 below; drop if option 1 or 3 is chosen)
- Modify: `test/nbd/README.md` — record the oracle's CI status
- **Not** `test/suites.yaml`: see step 3. An earlier draft of this task listed it, which was wrong — `dt` is not the mechanism for a Go test with a system dependency.

**Interfaces:**
- Consumes: `test/required-tests.sh`, which reads `test/required-tests.json` and is run in CI as part of the `lint-go` suite (`test/suites.yaml:31`, `job: Go Checks`, `tier: pr`) via `.github/workflows/lint.yml:84` (`"$RUNNER_TEMP/dt" run lint-go --tier "$TIER" --ci`).
- Produces: the NBD packages' load-bearing tests listed in `required-tests.json`; a decision, recorded in `test/nbd/README.md`, on whether the oracle runs in CI.

- [x] **Step 1: Establish what CI already runs**

Two separate facts, and the earlier draft of this task got both wrong:

```bash
sed -n '91,96p' .github/workflows/unit-tests.yml   # what the unit suite compiles and runs
grep -n 'required-tests' test/suites.yaml           # whether the pin runner is wired
```

Expected: `go test -v -race ... ./pkg/... ./internal/... ./cmd/...` — so the NBD packages **are** compiled and run on every pull request (they are under `./internal/...` and `./pkg/...`). And `test/suites.yaml:31` lists `test/required-tests.sh` in the `lint-go` suite, which `.github/workflows/lint.yml:84` runs via `dt run lint-go --tier "$TIER" --ci`.

Two things worth checking rather than assuming:

- **`-short` does not skip the NBD tests.** `unit-tests.yml:93` passes `-short` on pull requests (`${{ github.event_name == 'pull_request' && '-short' || '' }}`), so a suite that consulted `testing.Short()` would not run there. Verify with `grep -rn 'testing.Short' internal/adapter/nbd/ pkg/adapter/nbd/` — no hits, so all 97 tests run on every pull request regardless of event. The same grep confirms there is no `//go:build` tag in either package, so nothing is excluded from the `-tags=...` vet step in `lint-go` either.
- **`dt` runs only the lint suites.** `integration-tests.yml` does not use `dt` at all; it runs `go test` steps directly. So "add a `dt` cell" and "add a workflow step" are different mechanisms with different costs — see step 3.

So the gap is narrower than "no CI coverage" and narrower than "the pin runner is not in CI": the runner runs, but **no NBD test is listed in it** (`test/required-tests.json` pins exactly three packages: `./internal/adapter/nfs/auth`, `./internal/adapter/smb`, `./internal/adapter/smb/handlers`), so the NBD gates can be hollowed out while the suite stays green. That is the defect this task fixes.

- [x] **Step 2: Add the pinned names**

Add to `test/required-tests.json` under `packages`, following the file's own guidance — it says the list is for gates "whose removal is silent and expensive — authorization, encryption and squashing decisions":

```json
"./internal/adapter/nbd": [
  "TestHandleRead_ChecksReadPermission",
  "TestPipeline_FlushIsABarrier",
  "TestConnection_DrainingWriteDrainsPayload",
  "TestConnection_HandshakeIsBounded",
  "TestHandleDiscard_PunchHoleCarriesRequestDeadline"
]
```

Each must already exist and pass — the file's own rule ("name a test that already exists and passes on develop, or the check goes red on the pull request that adds it"). Do not list a test this plan creates before it exists.

`test/required-tests.sh` matches with `^(A|B|C)$` and requires a literal `^--- PASS: <name> (` line, so these must be top-level functions that actually run — a name behind a build tag or skipped at runtime fails the check. All five are plain top-level tests with no build tag; verify with:

```bash
for n in TestHandleRead_ChecksReadPermission TestPipeline_FlushIsABarrier \
         TestConnection_DrainingWriteDrainsPayload TestConnection_HandshakeIsBounded \
         TestHandleDiscard_PunchHoleCarriesRequestDeadline; do
  printf '%-52s %s\n' "$n" "$(grep -rl "^func $n(" internal/adapter/nbd/*_test.go || echo MISSING)"
done
```

Note that `TestHandleRead_ChecksReadPermission`, `TestPipeline_FlushIsABarrier` and `TestHandleDiscard_PunchHoleCarriesRequestDeadline` are created by Tasks 1, 3 and 10 — so this task must run **after** them. That is why it is second-to-last.

- [x] **Step 3: Decide on the oracle**

`TestOracle_DifferentialAgainstQemuNBD` calls `oracleRequireBinary(t, "qemu-nbd")` (`oracle_test.go:104`), which `t.Skipf`s when the binary is absent. No workflow installs `qemu-nbd`: the only `qemu` hits in `.github/workflows/` are `docker/setup-qemu-action` in `release.yml`, which is QEMU user-mode emulation for cross-building container images and has nothing to do with the NBD server. So in CI the oracle skips silently and proves nothing.

Three options; the task picks one and records why in `test/nbd/README.md`:

1. **Install `qemu-utils` in the unit-test job.** Cheapest to write, but `unit-tests.yml` is the whole-repo suite and adding a system package there for one package's test is a poor trade — and it would pay the `apt-get` cost on every pull request that touches any Go file.
2. **Add a step to `.github/workflows/integration-tests.yml`**, which already installs a system package for a Go test and is the precedent to copy verbatim (lines 199–212):
   ```yaml
   - name: Install and start rpcbind
     env:
       # apt and needrestart both prompt on a tty-less runner if left to
       # their defaults, and a prompt here blocks with no output until the
       # job's wall clock runs out.
       DEBIAN_FRONTEND: noninteractive
       NEEDRESTART_MODE: a
     run: |
       sudo apt-get update
       sudo apt-get install -y rpcbind
   ```
   `qemu-nbd` is in `qemu-utils` (no daemon to start, unlike `rpcbind`), so the step is the `apt-get` half only. Add `test/nbd/**` and `.github/workflows/integration-tests.yml` to that workflow's `go` path filter (lines 54–64) so a change to the rig triggers it. **Preferred.**
3. **Record it as local-only** in `test/nbd/README.md` with the reason.

Do **not** reach for a `dt` suite cell here, which an earlier draft of this task recommended. `dt` reads its tool requirements from `test/harness/needs/needs.go` — `tools` and `debian` (lines 35–48) and `inDtc` (line 50) — so a new suite would need a `qemu-nbd` entry in three maps and a hint string before the suite could declare `needs: [qemu-nbd]`. That is real surface for one test, and `dt` currently runs only the lint suites anyway (`test/suites.yaml`'s `lint-go` and `lint-repo` cells); `integration-tests.yml` is where Go tests with system dependencies actually live, and it does not use `dt` at all.

Whichever is chosen, `test/nbd/README.md` must say which, because a reader who assumes the oracle runs in CI will trust a green build more than it deserves.

- [x] **Step 4: Verify locally what CI will do**

Run: `go test -count=1 -v -run '^(TestHandleRead_ChecksReadPermission|TestPipeline_FlushIsABarrier|TestConnection_DrainingWriteDrainsPayload|TestConnection_HandshakeIsBounded|TestHandleDiscard_PunchHoleCarriesRequestDeadline)$' ./internal/adapter/nbd/`
Expected: five `--- PASS:` lines at column 0. This is the exact invocation `required-tests.sh` performs (minus its `-v`-independent grep), so a name that passes here cannot fail the pin.

Then the whole runner, which is what CI runs:

```bash
bash test/required-tests.sh 2>&1 | tail -20
```
Expected: `every required test ran and passed`, and four `== <package>` headers rather than three.

Finally, the two packages under the same flags the PR unit suite uses:

```bash
go test -short -race -count=1 ./internal/adapter/nbd/ ./pkg/adapter/nbd/
```
Expected: both `ok`. `-short` is in the command because it is in CI's; it changes nothing here, which is the point of the check above.

- [x] **Step 5: Commit**

```bash
git add test/required-tests.json test/nbd/README.md .github/workflows/integration-tests.yml
git commit -m "test(nbd): pin the NBD gates in required-tests and run the oracle in CI"
```

Drop `.github/workflows/integration-tests.yml` from the `git add` if option 1 or 3 was chosen, and adjust the subject to match.

---

### Task 16: Correct the documentation the audit found wrong

**Runs in `NBD-experimental`, not `dittofs-nbd`.** This is the one task that crosses repositories. The `README.md` it corrects is the extraction's entry point and `docs/sdd/progress.md` is the ledger that only exists there — `dittofs-nbd` has no `docs/sdd/` directory. Everything else in this plan runs in the source worktree. Run this task after Task 13, not after Task 11: pipelining is not true of the code until the in-flight drain lands, and the README's status line is exactly the claim that would then be premature again.

**Files:**
- Modify: `README.md` at the extraction's root (not `test/nbd/README.md`)
- Modify: `docs/sdd/progress.md` — add the resolved-minors ledger entry

**Interfaces:**
- Consumes: the results of Tasks 1–15.
- Produces: nothing.

- [x] **Step 1: Fix the three factual errors**

1. **"29 deferred minors"** (`README.md:100`) — the ledger enumerates 14 (`grep -c 'minor (deferred)' docs/sdd/progress.md` → `14`). Write the real number, or say "the deferred minors" without a count. A count that is wrong is worse than no count: it tells a reader the list was checked.
2. **"No CI coverage"** — appears **twice**, in the status line at `README.md:5` ("there is no CI coverage") and in open work item 4 at `README.md:96` ("`nbd` appears in no GitHub workflow and is not in `test/required-tests.json`"). Both are wrong: `unit-tests.yml:93-96` runs `./internal/...` and `./pkg/...`, so both NBD packages compile and all 97 tests run on every pull request. Replace with what Task 15 established: the packages run, but no NBD test was pinned in `test/required-tests.json` until now, and the differential oracle skips in CI because nothing installs `qemu-nbd`. That is the honest version, and it is still a real gap.
3. **"feature-complete"** — already softened to "a working prototype"; confirm it still reads that way after this work, and update the status line to say pipelining is implemented once Task 13 lands (the status line at `README.md:5` currently says "pipelining is planned and unstarted", which stops being true at Task 9 and is fully true of the code only at Task 13).

- [x] **Step 2: Move every fixed issue out of the known-issues table**

The README's table lists ten issues. Map each to its fate, then delete the row:

| Issue | Fate |
|---|---|
| `PunchHole` has no request deadline | Task 1 |
| `handleFlush` does not validate flags | Task 2 |
| `handleRead` ignores the returned `n` | Task 3 |
| No read-permission check on READ | Task 3 |
| Export size is a handshake-time snapshot | Task 4 |
| No buffer pooling on READ/WRITE | **not fixed** — leave the row. Task 11's byte semaphore bounds memory without pooling, which is the safety property; pooling is a separate allocation-rate optimisation with no defect behind it. |
| `max_message_size` / `default_export` have no consumer | Task 6 |
| UID/GID 0 unexpressible | Task 6 |
| `validate()` only checks the port | Task 5 |
| 10 dead protocol constants + `ReadOptionReplyHeader` | Task 7 — and the row's wording is itself wrong. It says "11 dead protocol constants and 4 test-only codec functions"; the verified split is **10 dead constants** (`CmdBlockStatus`, `OptListMetaContext`, `OptSetMetaContext`, `RepMetaContext`, `RepErrPolicy`, `RepErrPlatform`, `RepErrTLSReqd`, `RepErrShutdown`, `RepErrBlockSizeReqd`, `RepErrExtHeaderReqd`), **6 test-only constants** that Task 7 keeps on purpose, and **1 dead codec function** (`ReadOptionReplyHeader`) alongside the 4 test-only ones it names correctly. Write the split, not a total. |

Record each fixed one in `docs/sdd/progress.md` with the commit that fixed it. A row left in the table that the code has since fixed is the same failure as a row missing that it has not.

- [x] **Step 3: Verify no claim survives that the code contradicts**

Run:
```bash
grep -n "deferred\|CI coverage\|feature-complete\|known minor" README.md
```
For each hit, confirm against the code. A README that overstates is the failure mode this task exists to prevent — the previous audit found three.

- [x] **Step 4: Commit**

```bash
git add README.md docs/sdd/progress.md
git commit -m "docs: correct the claims the release audit found wrong"
```

---

## Self-Review

**1. Spec coverage.** The spec's §7 residency gate (no `BLOCK_STATUS`) is honoured — no task adds it. §11's transmission flags are unchanged except that the oracle re-verifies them in Task 8. The pipelining design doc's six tests all appear: `WritePayloadIsConsumedInOrder` → Task 9, `FlushIsABarrier` → Task 10, `BackpressureBoundsMemory` and `ConcurrentReadsAreConsistent` → Task 11, `WorkerPanicRepliesEIO` → Task 12, `ShutdownDrainsInFlight` → Task 13. The doc's "Order of work" 1–6 maps to Tasks 8–12 and 14; its `MaxInFlight`/`MaxInFlightBytes` appear in Task 11; its rejected alternative B (serialize all mutating commands) is honoured — Task 10 takes option A. The review's P0 and P1 are already fixed and committed (`89f2e4a`, `69e80b8`), so no task re-does them; Task 9's `WritePayloadIsConsumedInOrder` and Task 15's pinning keep them from regressing.

**2. Step scan.** Every step names a file and a command. Task 4 step 4 and Task 6 step 3 are the two that decide rather than transcribe, and each carries its decision and reason inline so the implementer writes one thing. Test steps carry the assertion, not a description of it: Tasks 1–5, 9, 11–13 all show the failing condition in code or in a named expected failure. Task 4's `sizeRefresh == 0` sentinel was added specifically so its test does not race a wall-clock window — a test that fails one run in fifty is worse than no test.

**3. Type consistency.** `reply` is defined once in Task 8 and used unchanged in Tasks 9–13. `barrier` is defined in Task 9 and used in Task 10. `pipeline` is defined in Task 9 and extended (never redefined) in Tasks 10–13. `command` is defined in Task 9 and carries `exp` so the Task 4 refresh does not become a data race once Task 11 raises the worker count. `Config.MaxInFlight`/`MaxInFlightBytes` are introduced and consumed in Task 11. `DefaultMaxConnections` is introduced in Task 5. `Handler.sizeRefresh` is introduced in Task 4 and moved into the reader by Task 9. Handler signatures change exactly once, in Task 8 — including `handleWrite` gaining `data []byte`, because the WRITE payload read moves to the reader in the same task rather than later.

Every test fixture the tasks name was verified to exist in `internal/adapter/nbd/*_test.go` before being written into the plan: `newStoreFake` and `storeFake` (with `data`, `readErr`, `writeErr`, `flushErr`, `punchErr`, `onFlush`, `panicOnRead`), `testExport(size)`, `newTestHandler(st, exp, timeouts)`, `serveHandler`, `readReplyAndPayload`, `testTimeouts`, `testExportName = "/export/boot"` (`connection_test.go:135`), `testPayloadID = "p1"` (`connection_test.go:138`), and `newWriteVolumeFixture` / `(*writeVolumeFixture).newHandler` (`integration_test.go:57`, `:70`). `Handler.authContext(ctx, exp)` already exists at `connection.go:118`, so Task 3's authorization call needs no new plumbing — an earlier draft of this plan wrongly listed it as something to add. `ErrEIO` and `mapError` already map to it.

One fixture the plan adds rather than reuses: `shortReadStore` in Task 3, because `storeFake` has no hook that returns a short count with a nil error. It embeds `storeFake` so only `ReadAt` is overridden.

**4. Review Focus.** Each of the five lines has an owning task with a test: (1) READ authorization → Task 3; (2) FLUSH barrier → Task 10; (3) drain desync → already fixed for the draining branch (`TestConnection_DrainingWriteDrainsPayload`), and Task 9's `WritePayloadIsConsumedInOrder` covers the pipelined reader; (4) export-size staleness → Task 4; (5) uncapped remote bind → Task 5.

Line 3's `rejectWrite` path is **already tested**: `TestConnection_RejectedWriteKeepsTheStreamSynchronized` (`connection_test.go:980`) sends a WRITE past the export end with its payload, then requires a following READ to return the right bytes. An earlier draft of this self-review claimed that gap was open and proposed a new test; it was wrong — the test exists and asserts exactly the alignment property. No task adds it.

**5. Proportion.** The spec is 1,061 lines and the earlier design doc 374; this plan is 1,676. It is larger than either because it carries two jobs — pipelining and the release cleanup — and the cleanup half is sixteen short tasks rather than one long one. Bodies appear only where a body encodes a decision the signature does not: the barrier, the panic recovery, the size refresh, the backpressure semaphore, the short-read check, and the payload-read move. Everywhere else it is a signature, a test name, and an assertion.

**5a. What the later revisions added, and why.** Nine corrections were made after the first complete draft, each because reading the code contradicted it:

- Task 8's split was rewritten from "handlers stop touching the socket" to "handlers stop *writing* to the socket", and the WRITE payload read moved to the reader in the same task. The original would not have compiled against Task 9 — the reader and the handler would both have read the same stream — and the fix had to land before the pipeline existed, not after. Task 8 step 3 also stopped hand-waving at a `readWritePayload(ctx, conn, bs, exp, req)` helper that would have had to re-run validation, and instead splits `handleWrite` into `validateWrite(...) (reply, bool)` plus a payload read, which is the only shape that keeps the "zero-length WRITE is refused without a drain" rule in one place.
- `command.exp` was added to Task 9 because Task 4's size refresh reassigns `exp` on the reader. A plain `p.exp` field read by workers is a data race the moment Task 11 raises the worker count, and `-race` would have reported it as a flake rather than as the design error it is.
- Task 11 gained "acquire before the payload read", "every acquire needs a release on every path", and **READ takes the budget too**. The third was the same mistake in a different place: `handleRead` allocates `req.Length` bytes on the worker (`handlers_read.go:88`), so a semaphore covering only WRITE would bound nothing on a read-heavy workload — `maxInFlight` concurrent 32 MiB READs would allocate with the budget untouched. Both hazards hang or OOM the connection rather than failing a test cleanly, so they had to be written down rather than discovered.
- Task 10 listed `CmdDiscard` among the commands that bump the barrier counter. There is no such command: `protocol.go:33-42` has `CmdDisc` (disconnect) and `CmdTrim`, and `handleDiscard` (`handlers_write.go:186`) is the shared *helper* `handleTrim` (`:155`) and `handleWriteZeroes` (`:174`) call. A dispatch case for it would not compile.
- Task 15's oracle recommendation moved from "add a `dt` suite cell" to "add a step to `integration-tests.yml`", because `dt` resolves tool requirements from `test/harness/needs/needs.go` and only runs the lint suites today.
- Tasks 5 and 6 were rewritten twice against the package layout. Their first drafts called `applyDefaults()` and `validate()` directly from `pkg/adapter/nbd/config_test.go` — which cannot compile, because both existing test files in that directory are `package nbd_test` and both functions are unexported. Task 5 now asserts through `New` (the real caller, and the shape `TestNew_PanicsOnOutOfRangePort` already uses); Task 6 adds a `pkg/adapter/nbd/export_test.go` in `package nbd` with a `ConfigForTest` accessor, because a UID's resolved value cannot be observed through a panic and exporting the field for a test would widen the production API. The second draft also claimed `models.AdapterConfig.Validate` rejects a bad `max_connections`; it does not — it checks only the type and the port — so the new error reaches the operator as a `New` panic and the task says so.
- Task 3 and Task 4 gained the ownership constraint: `SetFileAttributes` gates a chmod/truncate on `isOwner` (`pkg/metadata/file_modify.go:421`), so their fixtures must authenticate as `integrationIdentity`, which owns the volume the fixture creates. Without that note both tests fail with a permission error and look like a bug in the code under test.
- Task 16's README corrections were re-verified against the published file: the deferred-minor count is 14, not 29; "no CI coverage" appears twice (status line and open-work item 4), not once; and the known-issues row about dead protocol surface has its own arithmetic wrong (it says 11 dead constants and 4 test-only codec functions; the split is 10 dead + 6 test-only constants + 1 dead + 4 test-only codec functions). A task that quotes a wrong count into a corrected README would leave the defect it was written to fix.
- Task 4's two `sizeRefresh` paragraphs were merged into one "Two behaviours to state in `Export.Size`'s doc" block, because the first was an assertion with no mechanism behind it and the second contradicted it.

**6. Where each task runs.** Tasks 1–15 run in `dittofs-nbd` on `nbd-on-036`; Task 16 runs in `NBD-experimental`. That split is deliberate and stated in both the header note and Task 16, because the extraction cannot compile: it has no `go.mod`, no `pkg/metadata/`, no `test/required-tests.json`, no `.github/workflows/`, and no root `README.md` of the kind Task 16 corrects — its `README.md` is the extraction's own entry point, which does not exist in `dittofs-nbd` (whose root README is the upstream project README). A reader who tries to run Task 3 or Task 15 in the extraction will find none of the paths. `pkg/adapter/nbd/config.go`, `cmd/dfs/commands/start.go`, and `internal/adapter/common/*.go` are present in both, so a task that touches only those would run either place — no task does.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-10-20-nbd-pipelining-and-release-cleanup.md`.
