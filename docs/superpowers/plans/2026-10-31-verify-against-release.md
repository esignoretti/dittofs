# Verify the NBD branch against the latest DittoFS *release*

**Status:** ready to execute. Every premise below was measured on 2026-10-31 at
HEAD `9ef8cd97`, not inferred.

## Why this plan exists

The branch is verified against `develop`'s tip (`176be82e`). That is the right
base for a merge, but it is not what an operator runs. An operator runs a
release. The published README says the branch **cannot** build on the latest
release, and that claim has never been tested by the person most likely to
repeat it.

The headline finding of this plan's own research is that the claim is true but
far narrower than the README implies: **exactly one symbol** is missing at
v0.36.0, and with a one-line backport the entire branch builds and passes on the
release. That changes the advice in the README from "you cannot pin this to a
tag" to "you can, with one documented patch" — which is a much better answer for
anyone evaluating the work.

## What was measured, and how

Environment: macOS arm64, Go 1.27.2, `~/Documents/OpenCode/Ditto/dittofs-nbd`.

**The latest release is `v0.36.0`** (`51fc8443`, 2026-10-19). `gh release list`
confirms it is marked Latest; there is no `v0.37.0`. `develop`'s tip is
`176be82e`, five commits later.

Those five commits are:

| Commit | Subject | Touches one of our 91 files? |
|---|---|---|
| `176be82e` | docs(rfc): offers carry each extent's version and stamp | no |
| `4b74f895` | fix(shares): removing a share deletes its metadata, journal and local blocks (#3200) | **yes** — `runtime.go`, `shares/journal_open.go`, `handlers/shares.go`, badger stores |
| `99ccc9c3` | refactor(harness): lean pass over dt | no |
| `b8b06baf` | feat(harness): SMB in the fixture | no |
| `126a619a` | docs(smb): the ACL fidelity matrix | no |

Of the 91 files the branch touches, **63 are new** (absent at v0.36.0) and
**28 are modified**. Upstream's five commits touch 68 files, of which exactly
**two** overlap ours:

- `pkg/controlplane/runtime/runtime.go`
- `docs/guide/cli.md` (documentation only — no build impact)

### The measurement

```bash
git clone --no-checkout ~/Documents/OpenCode/Ditto/dittofs-nbd /tmp/relcheck
cd /tmp/relcheck && git checkout v0.36.0          # 51fc84433 chore(release): v0.36.0
git diff --name-only 176be82e..nbd-on-036 > /tmp/nbd91.txt   # 91 paths
git checkout nbd-on-036 -- $(cat /tmp/nbd91.txt)
go build ./...
```

Result:

```
# github.com/marmos91/dittofs/pkg/controlplane/runtime
pkg/controlplane/runtime/runtime.go:736:19: undefined: shares.RemoveShareJournal
```

**One error. One symbol.** `go vet` on all five touched packages reports the
same single line and nothing else.

### The minimal backport, and why it is safe

`RemoveShareJournal` is upstream's, added by `4b74f895` *after* v0.36.0. It is
called from `Runtime.PurgeShare` — which is also upstream's, and is also not
present at v0.36.0. The call site at `runtime.go:736` arrived in the same commit
as the function, so it is not something our branch added.

The backport replaces the journal-removal step in `PurgeShare` with a no-op:

```go
// v0.36.0 has no RemoveShareJournal (upstream 4b74f895 adds it). On this base a
// removed share leaves its journal directory behind; the journal is a local
// cache whose records are already remote-durable, and GC reclaims the blocks.
_ = defaults
```

Semantics lost: `dfsctl share remove` no longer deletes the share's journal
directory, so re-creating a share under the same name attaches the old journal
rather than an empty one. That is a *storage-hygiene* regression on a code path
the NBD work does not touch, and it is exactly the behaviour v0.36.0 shipped
with. It is not a correctness or durability regression.

With that one edit:

| Check | Result |
|---|---|
| `go build ./...` | **0** |
| `go vet ./...` | **0** |
| `go test ./internal/adapter/nbd/ ./pkg/adapter/nbd/` | `ok 9.715s`, `ok 1.790s` |
| `go test ./pkg/controlplane/runtime/` | `ok 78.744s` |
| `go test ./internal/controlplane/api/handlers/` | `ok 2.553s` |
| `go test ./cmd/dfsctl/commands/share/snapshot/` | `ok 1.909s` |
| `go test ./pkg/controlplane/models/` | `ok 1.552s` |
| `go test ./...` | **151 packages ok**, 1 failing: `test/harness/known` (3 shell tests, macOS bash 3.2 `declare -A`) |
| The 4 pinned NBD gates | all PASS |

The `test/harness/known` failure is the known macOS-only one; it fails
identically on an untouched v0.36.0 checkout.

### The conclusion the README must be corrected to

The branch **does** build and pass on the latest release, with a one-line,
semantically-documented backport. The current README says:

> one of the 91 files calls a function that arrived upstream after the release,
> so the extraction can no longer be pinned to a tag

The first half is right. The second half — "can no longer be pinned" — is
overstated, and it is the sentence a reviewer will act on. The honest statement
is: *it can be pinned, and here is the single patch, and here is the one
behaviour that patch gives up.*

---

## Task 1: Record the release-verification result as a repeatable script

The measurement above was done by hand in `/tmp`. It has to be one command, or
nobody will re-run it when v0.37.0 lands.

**Files:** new `test/nbd/verify-against-release.sh`; new
`test/nbd/deploy/backport-v0.36.0.patch`.

**Interfaces:**

```bash
verify-against-release.sh [--release <tag>] [--branch <ref>] [--work-dir <dir>]
```

Defaults: `--release` = the newest tag matching `v*` by version sort, `--branch`
= `nbd-on-036`, `--work-dir` = a fresh `mktemp -d`.

**Steps:**

1. Resolve the release: `git tag --list 'v*' --sort=-v:refname | head -1`, and
   refuse if the tag is not an ancestor of nothing — i.e. confirm it is a real
   commit. Print the resolved tag, its short SHA and its subject so the output
   says which release it verified.
2. `git clone --no-checkout --quiet <repo> "$work_dir/repo"`, `git checkout
   <release>`, then `git diff --name-only <base>..<branch>` to get the file list.
   **Refuse if the list is empty** — an empty overlay would "pass" while
   verifying nothing.
3. `git checkout <branch> -- $(cat filelist)`.
4. Apply `deploy/backport-v0.36.0.patch` with `git apply --check` first, so a
   patch that no longer applies fails loudly instead of silently doing nothing.
   If the release already has `RemoveShareJournal` (i.e. the patch is
   unnecessary), skip it and say so — that is the expected state from v0.37.0
   onwards.
5. `go build ./...`, `go vet ./...`, then `go test` on the five touched packages
   and the four pinned gates.
6. Print a verdict with the same shape as `boot-qemu.sh`:
   `VERDICT: PASS — <tag> builds and passes with N files overlaid` /
   `VERDICT: FAIL — <reason>`.

**Acceptance:** running it with no arguments prints a PASS naming `v0.36.0`;
running it with `--release v0.35.0` fails with a specific missing symbol rather
than a generic build error.

**Commit:** `test(nbd): verify the branch against a release, not just develop`

## Task 2: Correct the README's merge section

**Files:** `README.md` in `NBD-experimental` (the `## Merging upstream` section
and the base-discussion paragraph above it).

**Steps:**

1. Replace the "can no longer be pinned to a tag" claim with the measured
   result: v0.36.0 builds and passes with one documented patch, and give the
   patch inline.
2. Keep the develop-tip recipe as the *preferred* path, because that is what a
   merge actually applies to, and say why: a merge lands on `develop`, and
   `develop` already has `RemoveShareJournal`, so the patch is not needed there.
   State that the release path exists for *evaluation*, not for merging.
3. Add the behaviour the patch gives up, in the same "here is what it costs"
   register the rest of the README uses: a removed share leaves its journal
   directory on v0.36.0.
4. Add the verification command to the reproducibility section:
   `test/nbd/verify-against-release.sh`.

**Acceptance:** every claim in the rewritten section is one of the measurements
in this plan; no sentence asserts a build result that was not produced by Task
1's script.

**Commit:** `docs: the branch does pin to v0.36.0, with one patch`

## Task 3: Re-verify at whatever the release is when this is read

**Files:** none. This is a gate, not an edit.

**Steps:**

1. Run `test/nbd/verify-against-release.sh` with no arguments.
2. If it PASSES: record the tag and date in `docs/sdd/progress.md` under a new
   "Release verification" heading, and stop.
3. If it FAILS: the failure is the deliverable. Capture the exact compiler error,
   identify whether the missing symbol is upstream's or ours (the test is whether
   it appears in `git diff <base>..<branch> -- <file>`), and decide between
   extending the patch file and re-basing. Do not paper over it by pinning to the
   older tag silently.

**Acceptance:** the output of step 1 is pasted into the ledger verbatim,
including the resolved tag name, so a later reader can tell which release the
claim refers to.

---

## Risks, and what each would invalidate

| Risk | Test that catches it | What it invalidates |
|---|---|---|
| A future release adds a *second* missing symbol | Task 1's script fails on `go build` | The "one patch" claim; the README needs a second patch entry |
| `4b74f895`'s other files (badger stores, `handlers/shares.go`) become load-bearing for us | `go test ./pkg/controlplane/runtime/ ./internal/controlplane/api/handlers/` | The claim that the patch is confined to one call site |
| The overlay hides a semantic change rather than a build break | the 4 pinned gates plus the full `go test ./...` | The "semantically safe" claim — this is the weakest link, because an overlay cannot detect a behaviour change in a file upstream also edited |
| The release moves past v0.36.0 mid-work | Task 3 | Any tag named in the docs |

The third risk is the real one and is worth stating plainly: **an overlay
verifies compilation and tests, not intent.** Upstream's `4b74f895` changed what
removing a share *does*. Our branch does not touch that behaviour, and the
runtime tests pass, but no test in either repository asserts "removing a share
leaves no journal behind" on the v0.36.0 base — because on that base the
behaviour does not exist. That is why the backport is documented as a
behavioural regression rather than presented as a free win.
