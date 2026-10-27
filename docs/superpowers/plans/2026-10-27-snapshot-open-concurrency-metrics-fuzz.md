# Snapshot open concurrency, adapter metrics, and codec fuzzing

## Why

Three findings from reviewing the adapter after the snapshot feature landed. The
first is a proven bug; the other two are gaps that make the first class of bug
harder to catch next time.

**1. Two clients opening one snapshot destroy each other.** The shadow metadata
and journal directories are derived from the *snapshot*, not from the open:

```go
metaDir: filepath.Join(snap.SnapshotDir(localStoreDir), shadowMetaDirName),
jrnlDir: filepath.Join(snap.SnapshotDir(localStoreDir), shadowJournalDirName),
```

and `replaySnapshotMetadata` begins with `os.RemoveAll(dir)`
(`pkg/controlplane/runtime/snapshot_shadow.go:237`). So a second connection to
the same snapshot export deletes the first one's Badger directory while the
first is still reading it. Measured on the current tree:

```
open 0: err=... shadow-meta is locked by another process ... resource temporarily unavailable
open 1: err=<nil>
```

and, sequentially, with the second open *refused* by Badger's directory lock,
the first export is left retrying forever:

```
badger ERROR: error flushing memtable to disk: ... 000001.sst: no such file or directory
unable to open: .../shadow-meta/000001.sst, retrying
unable to open: .../shadow-meta/000002.sst, retrying
panic: test timed out after 2m40s
```

This breaks a documented use case. The README says a golden snapshot can be
"rehydrated any number of times", and the design's own motivation is booting
several VMs from one snapshot. Two concurrent boots deadlock one of them.

**2. The NBD adapter emits no metrics.** NFS and SMB both call
`Registry.Metrics().RecordRequest(...)`; NBD calls nothing. `PeakInFlightBytes`
exists and is referenced only by a test. A pipelined adapter with a byte budget
is precisely the thing whose in-flight depth and peak memory should be visible —
and the byte budget's whole purpose is to bound memory, which nobody can observe.

**3. No fuzz tests on the wire codec.** `protocol.go` has four decoders of
untrusted bytes and `handshake.go` parses arbitrary option payloads. The
repository has exactly one fuzz test, in NFS (`auth_fuzz_test.go`), so the
pattern and the CI wiring question are already answered by precedent.

## Scope

Three tasks, in this order. Task 1 must land first because Task 2 instruments the
code path Task 1 fixes, and a metric over a broken path measures the wrong thing.

Deferred by decision, not by omission: the connection-cap × byte-budget product
(4 GiB worst case), buffer pooling, and a nightly oracle run. Each is recorded in
the README's open work rather than silently dropped.

---

## Task 1: make each snapshot open own its shadow directories

**Files:** `pkg/controlplane/runtime/snapshot_shadow.go`,
`pkg/controlplane/runtime/snapshot_shadow_test.go`

**Interfaces:** `OpenSnapshotAsExport` keeps its signature. `OpenSnapshotExport`
gains an unexported `openID string`. `replaySnapshotMetadata` is unchanged — it
already takes a `dir`, which is the point.

- [ ] **Step 1: write the failing test.** Add to
      `pkg/controlplane/runtime/snapshot_shadow_test.go`:

      ```go
      // TestSnapshotShadow_TwoOpensOfOneSnapshotAreIndependent is the regression
      // guard for the bug this task fixes.
      //
      // The shadow metadata and journal directories used to be derived from the
      // snapshot ID alone, and replay starts with os.RemoveAll(dir) — so a second
      // client connecting to the same snapshot deleted the first client's Badger
      // directory out from under it. Badger then retried "no such file or
      // directory" forever and the first export never completed a read.
      //
      // Two VMs booting one golden snapshot is the documented use case, so this
      // is not a corner: it is the second user.
      func TestSnapshotShadow_TwoOpensOfOneSnapshotAreIndependent(t *testing.T) {
      	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
      	defer cancel()

      	fx := newShadowFixture(t, true)

      	const volumeSize = 256 << 10
      	pid := fx.makeVolume(ctx, "boot", volumeSize)
      	original := distinctBytes(volumeSize, 0x51)
      	fx.write(ctx, pid, original)

      	snapID := takeShadowSnapshot(t, ctx, fx)

      	first, err := fx.rt.OpenSnapshotAsExport(ctx, fx.shareName, snapID, "boot", &fx.identity)
      	if err != nil {
      		t.Fatalf("first open: %v", err)
      	}
      	defer first.Close()

      	// Read before, so a later failure is attributable to the second open
      	// rather than to a read that never worked.
      	before := readExport(t, ctx, first, 4096)

      	second, err := fx.rt.OpenSnapshotAsExport(ctx, fx.shareName, snapID, "boot", &fx.identity)
      	if err != nil {
      		t.Fatalf("second open of the same snapshot failed: %v\n"+
      			"two clients must be able to boot one snapshot; the shadow "+
      			"directories must be per-open, not per-snapshot", err)
      	}
      	defer second.Close()

      	// Both must still serve the snapshot's bytes, and the first must not have
      	// been disturbed by the second's replay.
      	for i, exp := range []*OpenSnapshotExport{first, second} {
      		got := readExport(t, ctx, exp, 4096)
      		if !bytes.Equal(got, before) {
      			t.Errorf("open %d reads different bytes after the second open: %s",
      				i, firstDiff(before, got))
      		}
      	}
      }
      ```

      Add the two helpers it uses, so the assertion is about behaviour and not
      about setup:

      ```go
      // takeShadowSnapshot creates a snapshot and waits for it to be ready.
      func takeShadowSnapshot(t *testing.T, ctx context.Context, fx *shadowFixture) string {
      	t.Helper()
      	snapID, err := fx.rt.CreateSnapshot(ctx, fx.shareName, CreateSnapshotOpts{})
      	if err != nil {
      		t.Fatalf("CreateSnapshot: %v", err)
      	}
      	snap, werr := fx.rt.WaitForSnapshot(ctx, fx.shareName, snapID)
      	if werr != nil {
      		t.Fatalf("WaitForSnapshot: %v", werr)
      	}
      	if snap.State != models.StateReady {
      		t.Fatalf("snapshot state = %q, want ready", snap.State)
      	}
      	return snapID
      }

      // readExport reads n bytes through an open snapshot export's shadow store.
      func readExport(t *testing.T, ctx context.Context, exp *OpenSnapshotExport, n int) []byte {
      	t.Helper()
      	buf := make([]byte, n)
      	if _, err := exp.BlockStore().ReadAt(ctx, string(exp.PayloadID), buf, 0); err != nil {
      		t.Fatalf("read through export: %v", err)
      	}
      	return buf
      }
      ```

- [ ] **Step 2: run it and confirm it fails for the stated reason.**

      ```sh
      go test -count=1 -timeout 120s -run 'TestSnapshotShadow_TwoOpens' ./pkg/controlplane/runtime/
      ```

      Expected: `second open of the same snapshot failed: ... shadow-meta is
      locked by another process`. If it fails with anything else, stop and read
      the error — a different failure means a different bug.

- [ ] **Step 3: give each open its own directory.** In
      `pkg/controlplane/runtime/snapshot_shadow.go`, replace the two directory
      constants' doc and add a per-open directory builder:

      ```go
      // shadowMetaDirName and shadowJournalDirName are the two directories an open
      // snapshot creates inside its snapshot directory. Both are removed on close.
      //
      // Each open gets its own pair, suffixed with the open's ID, and that is
      // load-bearing rather than tidy: replaySnapshotMetadata starts with
      // os.RemoveAll(dir), so a shared path means a second client connecting to
      // the same snapshot deletes the first client's Badger directory while it is
      // being read. Badger then retries "no such file or directory" forever and
      // the first export never completes a read. Two VMs booting one golden
      // snapshot is the documented use case, so a shared path is a bug and not a
      // saving.
      const (
      	shadowMetaDirName    = "shadow-meta"
      	shadowJournalDirName = "shadow-journal"
      )
      ```

      Add the ID to the struct, next to the dirs:

      ```go
      	// openID makes this open's shadow directories unique. Derived directories
      	// are per-open, not per-snapshot: see the constants' comment.
      	openID  string
      	metaDir string
      	jrnlDir string
      ```

      In `OpenSnapshotAsExport`, before the export literal:

      ```go
      	// A fresh ID per open, so two clients on one snapshot get two independent
      	// shadow stores instead of racing over one directory pair.
      	openID := uuid.NewString()
      ```

      and build the paths from it:

      ```go
      	exp := &OpenSnapshotExport{
      		ShareName:  shareName,
      		SnapshotID: snapID,
      		openID:     openID,
      		release:    release,
      		metaDir:    filepath.Join(snap.SnapshotDir(localStoreDir), shadowMetaDirName+"-"+openID),
      		jrnlDir:    filepath.Join(snap.SnapshotDir(localStoreDir), shadowJournalDirName+"-"+openID),
      	}
      ```

      `uuid` is already imported by `snapshot_rehydrate.go` in this package, so
      no new dependency — but check that the import is present in this file.

- [ ] **Step 4: run the new test, then the whole package under `-race`.**

      ```sh
      go test -count=1 -race -timeout 600s -run 'TestSnapshotShadow' ./pkg/controlplane/runtime/
      go test -count=1 -race -timeout 900s ./pkg/controlplane/runtime/
      ```

- [ ] **Step 5: prove the test bites.** Revert just the path change (keep the
      test), confirm `TestSnapshotShadow_TwoOpensOfOneSnapshotAreIndependent`
      fails with the lock message, then restore.

- [ ] **Step 6: check that a stale directory from a crashed process cannot
      accumulate.** A process killed between `MkdirAll` and `Close` leaves
      `shadow-meta-<uuid>` behind, and nothing will ever remove it because the
      ID is gone. Verify the containment story before claiming one:

      ```sh
      grep -rn "shadow-meta\|shadow-journal" --include=*.go pkg/controlplane/runtime/ | grep -v _test
      grep -rn "RemoveAll" pkg/controlplane/runtime/snapshot*.go
      ```

      If nothing reclaims them, add the sweep to `DeleteSnapshot`'s existing
      wipe — the snapshot directory is already being removed wholesale there, so
      the leak only matters for a snapshot that is never deleted. Record the
      answer in the commit message either way; do not leave it implied.

- [ ] **Step 7: commit.**

      ```
      fix(runtime): give each snapshot open its own shadow directories

      Two clients connecting to one snapshot export corrupted each other. The
      shadow metadata and journal directories were derived from the snapshot ID
      alone, and replaySnapshotMetadata begins with os.RemoveAll(dir), so the
      second open deleted the first open's Badger directory while it was being
      read. Measured before the fix:

        open 0: err=... shadow-meta is locked by another process
        open 1: err=<nil>

      and with the second open refused, the first export retried
      "no such file or directory" on every memtable flush until the test timed
      out after 2m40s.

      Each open now suffixes both directories with its own UUID, so the two are
      independent. Two VMs booting one golden snapshot is the documented use
      case — the README states a snapshot can be rehydrated any number of times —
      so this was the second user, not a corner.
      ```

---

## Task 2: emit request and pipeline metrics from the NBD adapter

**Files:** `internal/adapter/nbd/pipeline.go`, `internal/adapter/nbd/connection.go`,
`pkg/adapter/nbd/adapter.go`, `internal/adapter/nbd/pipeline_test.go`

**Interfaces:** new exported type in the nbd package:

```go
// Metrics is the subset of the metrics surface the NBD adapter reports.
//
// An interface rather than *metrics.Metrics so the wire layer keeps its
// existing property of importing no pkg/metrics: every other adapter reaches
// metrics through the registry, and the nbd package reaches the runtime only
// through interfaces already (Exporter, StoreResolver, BlockStore). It also
// makes the recorder nil-safe to test without a Prometheus registry.
type Metrics interface {
	RecordRequest(protocol, op, status string, d time.Duration)
}
```

and `func (h *Handler) SetMetrics(m Metrics)`.

- [ ] **Step 1: write the failing test.** In
      `internal/adapter/nbd/pipeline_test.go`:

      ```go
      // TestPipeline_RecordsEachCommand is the regression guard for the adapter
      // emitting no metrics at all: NFS and SMB both record per-request RED
      // metrics, and NBD recorded nothing, so an operator could see a connection
      // accepted and then no idea what it did.
      func TestPipeline_RecordsEachCommand(t *testing.T) {
      	rec := &recordingMetrics{}
      	st := newStoreFake()
      	h := newTestHandler(st, testExport(1<<20), testTimeouts())
      	h.SetMetrics(rec)

      	client, server := serveHandler(t, h, testExportName)
      	completeHandshake(t, client, testExportName)

      	// One of each command the adapter serves, plus one it does not.
      	writeThroughClient(t, client, 0, []byte("hello"), 0)
      	flushThroughClient(t, client)
      	trimThroughClient(t, client, 0, 4096)
      	writeZeroesThroughClient(t, client, 0, 4096, 0)
      	sendRequest(t, client, &Request{Type: Command(0x7f), Cookie: 9})
      	readReplyHeader(t, client)

      	_ = server
      	client.Close()

      	want := map[string]bool{"write": true, "flush": true, "trim": true,
      		"write_zeroes": true, "unknown": true}
      	got := rec.ops()
      	for op := range want {
      		if !got[op] {
      			t.Errorf("no metric recorded for %q; recorded: %v", op, rec.opList())
      		}
      	}
      	if len(rec.errors()) == 0 {
      		t.Error("the unknown command was refused but no error status was recorded")
      	}
      }
      ```

      Plus the recorder:

      ```go
      // recordingMetrics captures what the adapter reports, so a test can assert
      // on labels rather than on a Prometheus registry.
      type recordingMetrics struct {
      	mu     sync.Mutex
      	calls  []metricCall
      }

      type metricCall struct {
      	protocol, op, status string
      	d                    time.Duration
      }

      func (m *recordingMetrics) RecordRequest(protocol, op, status string, d time.Duration) {
      	m.mu.Lock()
      	defer m.mu.Unlock()
      	m.calls = append(m.calls, metricCall{protocol, op, status, d})
      }

      func (m *recordingMetrics) ops() map[string]bool {
      	m.mu.Lock()
      	defer m.mu.Unlock()
      	out := map[string]bool{}
      	for _, c := range m.calls {
      		out[c.op] = true
      	}
      	return out
      }

      func (m *recordingMetrics) opList() []string {
      	m.mu.Lock()
      	defer m.mu.Unlock()
      	var out []string
      	for _, c := range m.calls {
      		out = append(out, c.op+"/"+c.status)
      	}
      	return out
      }

      func (m *recordingMetrics) errors() []metricCall {
      	m.mu.Lock()
      	defer m.mu.Unlock()
      	var out []metricCall
      	for _, c := range m.calls {
      		if c.status == "error" {
      			out = append(out, c)
      		}
      	}
      	return out
      }
      ```

- [ ] **Step 2: run it and confirm nothing is recorded.**

      ```sh
      go test -count=1 -run 'TestPipeline_RecordsEachCommand' ./internal/adapter/nbd/
      ```

      Expected: `no metric recorded for "write"; recorded: []`.

- [ ] **Step 3: add the label helper and the recorder field.** In
      `pipeline.go`, next to `isWriteLike`:

      ```go
      // commandMetricName maps a command to its metric label.
      //
      // Bounded by construction: the switch enumerates every command the adapter
      // serves plus one label for everything else. Labelling by raw command value
      // would let a client mint unlimited series just by sending new command
      // numbers, which is the same cardinality trap NFS avoids by recording
      // ok|error rather than a status code.
      func commandMetricName(cmd Command) string {
      	switch cmd {
      	case CmdRead:
      		return "read"
      	case CmdWrite:
      		return "write"
      	case CmdFlush:
      		return "flush"
      	case CmdTrim:
      		return "trim"
      	case CmdWriteZeroes:
      		return "write_zeroes"
      	default:
      		return "unknown"
      	}
      }
      ```

      Add the field to `Handler` (`connection.go`), next to `degraded`:

      ```go
      	// metrics records per-request RED metrics. Nil until SetMetrics, and every
      	// call site checks, so a test handler and a handler built without a
      	// runtime both work without a no-op recorder.
      	metrics Metrics
      ```

      and the setter, next to `SetPipelining`:

      ```go
      // SetMetrics installs the metrics recorder. Must be called before Serve: the
      // field is read by every command's dispatch on multiple goroutines.
      func (h *Handler) SetMetrics(m Metrics) { h.metrics = m }
      ```

- [ ] **Step 4: record in `dispatch`.** It is the single place every command
      passes through exactly once, including the `pre` refusals, so no command
      can be counted twice or missed:

      ```go
      func (p *pipeline) dispatch(ctx context.Context, cmd command) reply {
      	start := time.Now()
      	rep := p.dispatchCommand(ctx, cmd)
      	if p.h.metrics != nil {
      		status := "ok"
      		if rep.nbdErr != 0 {
      			status = "error"
      		}
      		p.h.metrics.RecordRequest("nbd", commandMetricName(cmd.req.Type), status,
      			time.Since(start))
      	}
      	return rep
      }
      ```

      Rename the existing body to `dispatchCommand` and keep its doc comment with
      it. Do not time in `serve`: `serve` returns before the reply is written, so
      a duration measured there would exclude the socket write and understate
      what a client waits for. The duration here is "handler time", which is what
      the other adapters record.

- [ ] **Step 5: wire it from the adapter.** In `pkg/adapter/nbd/adapter.go`,
      after `handler.SetPipelining(...)`:

      ```go
      	// The runtime's metrics handle is nil-safe, and SetMetrics' argument is an
      	// interface — so a nil *metrics.Metrics would be a non-nil interface
      	// holding a nil pointer, and RecordRequest's own nil check would not fire.
      	// Guard on the handle, not on the interface.
      	if m := a.runtime.Metrics(); m != nil {
      		handler.SetMetrics(m)
      	}
      ```

      This guard is the one real trap in the task: `*metrics.Metrics` has nil-safe
      methods, but storing it in an interface makes it non-nil, so the adapter
      would call through a typed nil and panic.

- [ ] **Step 6: verify the labels are what an operator will see.**

      ```sh
      go test -count=1 -race -run 'TestPipeline' ./internal/adapter/nbd/
      go build ./... && go vet ./internal/adapter/nbd/ ./pkg/adapter/nbd/
      ```

      Then confirm against the metric definitions that `nbd` is a legal
      `protocol` value and that no new series name was introduced:

      ```sh
      grep -n "requests\s*=\|requests:" pkg/metrics/instruments.go | head
      grep -rn '"nfs"\|"smb"' pkg/metrics/ | head
      ```

- [ ] **Step 7: commit.**

      ```
      feat(nbd): record per-request metrics

      NFS and SMB both record RED metrics per request; NBD recorded nothing, so
      an operator could see a connection accepted and then no evidence of what it
      did. A pipelined adapter with a byte budget is exactly the thing whose
      in-flight depth and peak memory need to be observable.

      The label set is bounded by construction: the command switch enumerates
      what the adapter serves plus one "unknown" label, so a client cannot mint
      unlimited series by sending new command numbers.

      Duration is handler time, measured in dispatch, not in serve — serve
      returns before the reply is written, so timing there would exclude the
      socket write and understate what the client waits for.
      ```

---

## Task 3: fuzz the wire codec

**Files:** new `internal/adapter/nbd/protocol_fuzz_test.go`,
new `internal/adapter/nbd/handshake_fuzz_test.go`

**Interfaces:** none — fuzz targets are ordinary `FuzzXxx` functions. Go runs
them as unit tests with the seed corpus when `-fuzz` is absent, so they cost
nothing in the normal suite.

- [ ] **Step 1: fuzz the two request/reply decoders.** In
      `protocol_fuzz_test.go`:

      ```go
      package nbd

      import (
      	"bytes"
      	"testing"
      )

      // FuzzReadRequest drives the transmission-phase header decoder, which is the
      // first thing every client touches and the only parser on the request path
      // that sees attacker-controlled bytes before any validation.
      //
      // The property asserted is not "rejects bad input" — most random bytes are
      // rejected — but "never panics and never accepts a header it would then
      // mis-handle". A decoder that panics is a remote crash; one that accepts a
      // short header is worse, because the payload read that follows would consume
      // the client's next command.
      func FuzzReadRequest(f *testing.F) {
      	seeds := []*Request{
      		{Type: CmdRead, Cookie: 1, Offset: 0, Length: 4096},
      		{Type: CmdWrite, Cookie: 2, Offset: 0, Length: maxBlockSize},
      		{Type: CmdFlush, Cookie: 3},
      		{Type: CmdDisc, Cookie: 4},
      		{Type: CmdWriteZeroes, Cookie: 5, Length: 1},
      		{Type: Command(0xffff), Cookie: 0xffffffffffffffff, Offset: 0xffffffffffffffff, Length: 0xffffffff},
      	}
      	for _, s := range seeds {
      		var buf bytes.Buffer
      		if err := s.Encode(&buf); err != nil {
      			f.Fatalf("encode seed: %v", err)
      		}
      		f.Add(buf.Bytes())
      	}

      	f.Fuzz(func(t *testing.T, data []byte) {
      		req, err := ReadRequest(bytes.NewReader(data))
      		if err != nil {
      			return
      		}
      		// Accepted. Re-encoding must round-trip exactly: a header the server
      		// accepts but would not produce is a header it can only have
      		// misinterpreted.
      		var buf bytes.Buffer
      		if err := req.Encode(&buf); err != nil {
      			t.Fatalf("accepted a request that will not re-encode: %v", err)
      		}
      		if !bytes.Equal(buf.Bytes(), data[:requestHeaderLen]) {
      			t.Fatalf("round-trip mismatch:\n in: %x\nout: %x", data[:requestHeaderLen], buf.Bytes())
      		}
      	})
      }
      ```

      Note the `data[:requestHeaderLen]` slice is safe *because* `ReadRequest`
      succeeded, which means it consumed exactly that many bytes — assert that
      invariant explicitly if the round-trip ever looks surprising.

- [ ] **Step 2: fuzz the option-header decoders.** In the same file:

      ```go
      // FuzzReadOptionHeader drives the handshake option header, which is parsed
      // before any export exists and before any authentication-shaped decision.
      func FuzzReadOptionHeader(f *testing.F) {
      	seeds := []*OptionHeader{
      		{Option: OptGo, Length: 0},
      		{Option: OptList, Length: 0},
      		{Option: OptAbort, Length: 0},
      		{Option: OptInfo, Length: 4},
      		{Option: Opt(0xffff), Length: 0xffffffff},
      	}
      	for _, s := range seeds {
      		var buf bytes.Buffer
      		if err := s.Encode(&buf); err != nil {
      			f.Fatalf("encode seed: %v", err)
      		}
      		f.Add(buf.Bytes())
      	}

      	f.Fuzz(func(t *testing.T, data []byte) {
      		hdr, err := ReadOptionHeader(bytes.NewReader(data))
      		if err != nil {
      			return
      		}
      		var buf bytes.Buffer
      		if err := hdr.Encode(&buf); err != nil {
      			t.Fatalf("accepted an option header that will not re-encode: %v", err)
      		}
      		if !bytes.Equal(buf.Bytes(), data[:optionHeaderLen]) {
      			t.Fatalf("round-trip mismatch:\n in: %x\nout: %x", data[:optionHeaderLen], buf.Bytes())
      		}
      	})
      }
      ```

      Check the constant names first — `optionHeaderLen` may not exist under that
      name; use whatever `protocol.go` declares and do not invent one.

- [ ] **Step 3: fuzz the option *payload* parser, which is the real risk.** The
      header is 16 bytes of fixed fields; the payload is where a length field
      from the client meets an allocation. In `handshake_fuzz_test.go`, target the
      exported name parser and the GO payload reader:

      ```go
      // FuzzParseSnapshotExportName drives the snapshot export name grammar. It is
      // a string splitter, but it runs on a client-supplied name before any
      // validation, and a panic there is a remote crash in the handshake.
      //
      // The property is stronger than "does not panic": a name that parses must
      // round-trip to the same string, because the parsed share and ID are used to
      // look up a snapshot and a mismatch would open the wrong one.
      func FuzzParseSnapshotExportName(f *testing.F) {
      	for _, s := range []string{
      		"export/snapshots/abc/boot",
      		"export/snapshots//boot",
      		"/snapshots/abc/boot",
      		"snapshots/abc/boot",
      		"a/b/snapshots/abc/boot",
      		"export/snapshots/abc/",
      		"",
      		"/",
      		"export/snapshots",
      	} {
      		f.Add(s)
      	}

      	f.Fuzz(func(t *testing.T, name string) {
      		share, snapID, volume, ok := parseSnapshotExportName(name)
      		if !ok {
      			return
      		}
      		// A parse that claims success must have produced three non-empty parts
      		// and a name that contains the marker — otherwise it matched something
      		// that is not the snapshot shape.
      		if share == "" || snapID == "" || volume == "" {
      			t.Fatalf("parsed %q into empty parts: share=%q snap=%q volume=%q",
      				name, share, snapID, volume)
      		}
      		rebuilt := share + "/snapshots/" + snapID + "/" + volume
      		if rebuilt != name {
      			t.Fatalf("parse does not round-trip: %q -> %q", name, rebuilt)
      		}
      	})
      }
      ```

      If that round-trip property does not hold for a legitimate input (a nested
      share name, say), the fuzzer will find it immediately and the *test* is
      wrong, not the code — fix the property to state what the parser actually
      guarantees, and write down what that is.

- [ ] **Step 4: run each target for a bounded time.**

      ```sh
      go test -run '^$' -fuzz FuzzReadRequest -fuzztime 60s ./internal/adapter/nbd/
      go test -run '^$' -fuzz FuzzReadOptionHeader -fuzztime 60s ./internal/adapter/nbd/
      go test -run '^$' -fuzz FuzzParseSnapshotExportName -fuzztime 60s ./internal/adapter/nbd/
      ```

      Each of these replaces the `nbd` package's test binary; run them one at a
      time. Any crash writes a reproducer to `testdata/fuzz/<Target>/`, which
      must be committed — it is the regression.

- [ ] **Step 5: make the seeds part of the ordinary suite.** Confirm the fuzz
      targets run as unit tests without `-fuzz`:

      ```sh
      go test -count=1 -run 'Fuzz' ./internal/adapter/nbd/
      ```

      Expected: PASS, with the seed corpus executed. This is what keeps them
      cheap — no CI change is needed for the corpus, only for actual fuzzing,
      which is deferred with the nightly-run item.

- [ ] **Step 6: commit.**

      ```
      test(nbd): fuzz the wire codec and the export-name parser

      protocol.go has four decoders of untrusted bytes and the handshake parses
      arbitrary option payloads, and neither had a fuzz target. The repository's
      one precedent is internal/adapter/nfs/rpc/auth_fuzz_test.go.

      The asserted property is round-trip rather than "rejects garbage": a header
      the server accepts but would not itself produce is a header it can only
      have misinterpreted, and the payload read that follows would then consume
      the client's next command. A panic is a remote crash.

      Seeds run as ordinary unit tests, so the corpus costs nothing in the normal
      suite; actual fuzzing is left to the nightly-run item in the README.
      ```

---

## Verification

- [ ] `go build ./...` — clean.
- [ ] `go vet ./internal/adapter/nbd/ ./pkg/adapter/nbd/ ./pkg/controlplane/runtime/` — clean.
- [ ] `gofmt -l` on every touched file — empty.
- [ ] `go test -count=1 -race ./internal/adapter/nbd/ ./pkg/adapter/nbd/ ./pkg/controlplane/runtime/` — pass.
- [ ] `go test -count=1 -run 'Fuzz' ./internal/adapter/nbd/` — seeds pass.
- [ ] Each new test fails when its fix is reverted, and the failure message names the bug.

## What this plan deliberately does not do

- **The 4 GiB cap product.** `DefaultMaxInFlightBytes` (64 MiB) ×
  `DefaultMaxConnections` (64) is 4 GiB of payload buffers worst case, before
  shadow journals (`MaxSize/16` each) and before any number of simultaneously
  open snapshots. Each default is individually reasonable; only the product is
  alarming. Recorded in the README's open work.
- **Buffer pooling.** Every READ/WRITE still allocates. The byte budget bounds
  concurrency, not churn. It needs an ownership discipline the pipeline does not
  have — the reply carries the release amount today, so a pool would have to
  carry the buffer too.
- **A nightly oracle run.** The oracle runs on the `go` path filter, so a shell
  or CI-config change can break it without the job firing.
- **Fuzzing in CI.** The seeds run as unit tests; `-fuzz` is not wired to a
  workflow, because a fuzzer that runs for 60 seconds per PR is a flake source
  and one that runs for an hour does not belong on a PR path.
