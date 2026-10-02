# DittoFS e2e test matrix

Which DittoFS behaviours the upstream e2e suite (`test/e2e`) covers, which ones the scenarios in `test/scenarios` cover, where the two overlap, and what the e2e suite should add. First written 2026-10-01 with 10 scenarios; updated the same evening for all 38.

Sources:
- **e2e suite:** a catalog of `test/e2e` built from develop `d562e665`. It has 140 test functions: 137 run by default and 3 need the `stress` tag. The Linux container run had 551 subtests, with 130 passing, 3 failing (environment) and 4 skipped.
- **CI:** it runs the whole default suite (`go test -tags=e2e ./test/e2e/...`, `.github/workflows/e2e-tests.yml`).
- **Scenarios:** the 38 scripts in `test/scenarios` at `d2a021bc`, with about 200 explicit assertions between them. Each is named `<number>-<what it does>-<size>.sh` (see the README); the 9x scenarios take long, so the schedule leaves them out. Every other command is a check too, because the first failing command fails a scenario.

## Summary

The matrix has **101 capabilities** in 12 areas: behaviours a user or operator relies on. Each one is marked Full, Partial or none for the e2e suite and for the scenarios. "Weighted" counts Partial as half.

| | Capabilities covered | Weighted coverage |
|---|---|---|
| e2e suite | 70 of 101 (69%) | 65% |
| Isolated scenarios | 67 of 101 (66%) | 54% |
| Either | 100 of 101 (99%) | 96% |

How the two overlap:

| | Capabilities | Share |
|---|---|---|
| Covered by both (overlap) | 37 | 37% |
| e2e only | 33 | 33% |
| Scenarios only | 30 | 30% |
| Neither | 1 | 1% |

**What the numbers say:**

- **The e2e suite is broad.** It covers 69% of the capabilities. Its strongest areas are NFS protocol behaviour (100%), the control plane (85%), file operations (79%) and the cross-protocol basics.
- **Its weak spots are what happens to data after it is written:**
  - the remote after GC, and GC's own numbers;
  - shares that share a metadata store, a block store or a bucket;
  - compression and encryption;
  - local-tier accounting and its size limit;
  - the recycle bin with more than one user;
  - S3 failures and crashes;
  - snapshots of real data;
  - files that share chunks, and a file deleted while open.
- **Those weak spots are where the defects are.** The scenarios cover 30 capabilities the e2e suite does not, and 22 of the 38 scenarios fail today, each on a defect the e2e suite lets through. See "Scenario by scenario" below.
- **The scenarios are narrow by design.** They cover 66% (weighted 54%), mostly SMB with userspace NFSv4.0 clients against a single S3 server, and they never mount. NFS protocol depth (4%) stays with the e2e suite, and most of SMB's (44%). Where they overlap the e2e suite (37%), it is on purpose: the round trip and the single-share GC control are canaries for the same paths the e2e blocks tests check.
- **Only one capability is covered by neither suite:** Cubbit DS3 as the remote. A scenario for it would need Cubbit credentials inside the container, and the suite is built not to depend on Cubbit.
- **Some e2e coverage is weaker than the table suggests.** See "Where the e2e coverage is weaker than it looks" below: dedup, for instance, is listed but never runs.

## How this was built

1. **The capabilities.** One row per behaviour a user or operator relies on, grouped by area. They come from four places:
   - the e2e catalog (every test maps to at least one row);
   - what the scenarios check;
   - the findings of an external review of DittoFS;
   - the storage RFCs the scenario plan cites.

   The first version had 97 rows. Four were added for behaviours that newer scenarios check and no row described:
   - **FO-12:** a file deleted while open;
   - **MS-07:** two block stores on one bucket and prefix;
   - **BS-15:** files that share chunks;
   - **XP-06:** one case rule across SMB and NFS.

   136 of the 140 e2e tests map to a row. The other four test the suite itself (`TestSMBLockHolderHelper`, `TestCleanupBucket_PaginatesAndDeletesAll`, and the two `TestImageObtainable_*` tests) and are left out.
2. **The levels.** For each row and each suite: Full, Partial or none.
   - **Full:** the behaviour is asserted directly.
   - **Partial:** it is exercised but not asserted, or only one variant is checked. Examples:
     - the scenarios' single backend combination, against the e2e store matrix;
     - the e2e tests that drain, evict and GC without checking GC's numbers;
     - encryption with a local key and no KMIP.
3. **The percentages.** Each is a share of the 101 rows. "Covered" counts Full and Partial alike; "weighted" counts Partial as half. "Either, weighted" takes the better of the two suites per row.
4. **Overlap.** A row covered by both suites at any level.
5. **Pass or fail does not change coverage.** A failing scenario still checks its behaviour; it is how the defect was found.

Levels go by what a test asserts, not by its name. Where a test's own doc comment and its code disagree, the catalog's description from the code wins. Some calls are close:
- `41-smb-dedup-delete-original-gc-xs` is not counted for BS-08: it checks that shared chunks are kept, not that repeated content is freed after the hold.
- The e2e suite gets Partial on BS-15 for `TestNFSv42Clone`, which writes to one side of a clone; no e2e test deletes or truncates a file whose chunks another file shares.

## Coverage by area

| Area | Capabilities | e2e | Scenarios | Both | e2e only | Scenarios only | Neither | e2e weighted | Scenarios weighted | Either, weighted |
|---|---|---|---|---|---|---|---|---|---|---|
| Control plane and CLI | 17 | 15 | 11 | 9 | 6 | 2 | 0 | 85% | 44% | 100% |
| File operations | 12 | 10 | 8 | 6 | 4 | 2 | 0 | 79% | 58% | 100% |
| Stores and persistence | 5 | 4 | 3 | 3 | 1 | 0 | 1 | 70% | 40% | 70% |
| Multiple shares | 7 | 2 | 7 | 2 | 0 | 5 | 0 | 21% | 93% | 100% |
| Block store and data path | 15 | 8 | 14 | 7 | 1 | 7 | 0 | 43% | 80% | 87% |
| SMB protocol | 9 | 6 | 5 | 2 | 4 | 3 | 0 | 61% | 44% | 94% |
| NFS protocol | 12 | 12 | 1 | 1 | 11 | 0 | 0 | 100% | 4% | 100% |
| Cross-protocol | 6 | 4 | 4 | 2 | 2 | 2 | 0 | 67% | 58% | 100% |
| Permissions and identity | 5 | 3 | 3 | 1 | 2 | 2 | 0 | 60% | 50% | 100% |
| Recycle bin | 5 | 2 | 5 | 2 | 0 | 3 | 0 | 40% | 70% | 90% |
| Snapshots | 3 | 2 | 2 | 1 | 1 | 1 | 0 | 67% | 50% | 100% |
| Robustness and operations | 5 | 2 | 4 | 1 | 1 | 3 | 0 | 30% | 80% | 100% |
| **Total** | **101** | **70** | **67** | **37** | **33** | **30** | **1** | **65%** | **54%** | **96%** |

## Scenario by scenario

What each scenario checks, which e2e tests check the same thing, and what only the scenario checks. Status is on develop `18d85aec` plus the scenarios (up to `d2a021bc`), run on ditto on 2026-10-01: 16 pass, 22 fail.

| Scenario | Status | Capabilities | Same check in e2e | Only in the scenario |
|---|---|---|---|---|
| 00-smb-nfs-roundtrip-gc-xs | passes; in cron every 15 min (paused on 2026-10-01) | FO-02, FO-05, FO-07, FO-11, BS-01, BS-02, BS-04, BS-12, CP-15, ST-02, RO-05 | TestBlocksFlipLifecycle_NFS/_SMB (upload, cold read, GC); TestSMBFileOperations; TestNFSv4BasicOperations | a userspace NFSv4 client (libnfs); SeaweedFS as S3; bucket count, bytes and key layout; per-command timing |
| 01-smb-rename-in-share-root-xs | passes (fixed by #2915); in cron every 4 h (paused) | FO-10, SMB-06 | none (a unit test in #2915 only) | everything: the Explorer pattern, a rename while the root is held |
| 42-smb-1share-small-large-gc-xs | passes | BS-02, BS-04, BS-05, CP-15, FO-07, MS-03 (control) | TestBlocksFlipLifecycle_* (GC frees the block, cold read) | GC's numbers per step (objects swept, bytes freed, hashes marked); the control for the two-share case |
| 52-smb-2shares-small-large-gc-xs | fails: A-08 (#2909, PR #2919) | MS-01, MS-03, BS-05 | TestMultiShareIsolation, but with separate metadata stores | one metadata store with two remotes; GC through the right bucket; the other bucket untouched |
| 51-smb-2shares-same-content-xs | fails: A-07 (#2909) | MS-04, BS-02 | none | the same content on two remotes; the second share's cold read |
| 53-smb-2shares-share-scoped-views-xs | fails: #2906 | MS-05, CP-16 | none | stats, offline check and warm scoped to a share |
| 54-smb-2shares-idle-disk-used-s | passes (#2908 not reproduced at this level) | MS-06, BS-11, FO-07 | TestFileSizeMatrix (large files only) | Local Disk Used against the files on disk; ranges seeded across shares |
| 32-smb-nfs-trash-two-users-xs | fails: E-06 (#2920), A-06 (#2921) | TR-01, TR-02, TR-03, TR-04, PE-02, PE-05, XP-01, XP-03 | TestNFSTrashRecycleAndRestore, TestSMBTrashRecycleAndRestore (one user each) | a second user; the bin's owner and mode; the admin-only restriction; the error each protocol reports |
| 31-smb-nfs-owner-gid-xs | fails: E-01 (#2922) | XP-05, XP-01, CP-03 | TestCrossProtocolInterop (data only, not ownership) | the group owner of an SMB-created file, seen over NFS |
| 13-smb-compression-frame-magic-xs | fails: #2897 | BS-09, BS-02, CP-07 | none | a compressed block store; a chunk that looks like a frame; found A-10 (#2923) on the way |
| 55-smb-extra-share-gc-xs | fails: A-08 (whenever GC visits the other remote first) | MS-03, BS-05, FO-07, MS-01 | none | one delete and GC per round, so a lucky GC order rarely passes every round |
| 56-smb-extra-share-gc-minimal-xs | fails: A-08 | MS-03 | none | the smallest reproduction: seven deletes, a GC after each |
| 50-smb-2shares-one-store-gc-xs | passes | MS-02, BS-15, BS-05 | TestMultiShareIsolation, subtest SameBlockStore (no GC) | GC on one of two shares on one store; GC's bytes freed equal the bucket's change |
| 11-smb-2stores-same-bucket-xs | fails: a second store on the same bucket and prefix is accepted (the other store's data survives GC and reclaim) | MS-07, CP-06 | none | everything |
| 40-smb-dedup-same-content-xs | passes | BS-07, BS-02 | TestDedupRace_NFSv4_ConcurrentIdenticalWrites, TestObjectIDPopulation_NFSWriteQuiesce (nightly, never run: C-01) | dedup measured in the bucket: a copy adds nothing, new data adds its size |
| 41-smb-dedup-delete-original-gc-xs | passes, also with a restart between the delete and the GC | BS-15, BS-07, BS-02, ST-04 | none | the original of deduplicated files deleted and collected; full and partial overlap; the copies read cold |
| 26-smb-truncate-same-content-s | fails: B-01 (#2924; STATUS_INSUFFICIENT_RESOURCES on every try) | BS-15, FO-06 | TestNFSv4AdvancedFileOps (truncate of a file that shares nothing) | SMB SET_INFO EndOfFile on two files with the same chunks |
| 25-smb-nfs-truncate-same-content-s | fails: B-01 (#2924; NFS4ERR_DELAY on every try) | BS-15, FO-06, FO-02 | TestNFSv4AdvancedFileOps (same) | the same over NFSv4 |
| 24-smb-nfs-truncate-during-upload-xs | passes (whether the upload was in flight at the truncate is not checked) | FO-06, RO-03, XP-01 | TestNFSv4AdvancedFileOps (truncate) | a truncate with S3 paused, then a grow: zeros after the cut, over SMB and NFS, read cold twice |
| 43-smb-nfs-open-unlinked-gc-xs | fails: #2927 (after an evict, the open handle reads zeros) | FO-12, BS-04 | none | everything |
| 44-smb-unlink-crash-gc-xs | passes | BS-04, BS-05, ST-04 | none | SIGKILL right after deletes; GC sweeps the orphans and the bucket returns to its baseline |
| 21-smb-nfs-concurrent-creates-xs | passes | RO-02, FO-08, XP-01 | TestMultiClientConcurrency; TestStressConcurrentFileCreation (stress tag, not run) | 64 clients on both protocols in one directory with a chmod loop: 1,280 entries, no I/O errors |
| 70-smb-s3-down-reads-fail-m | passes | RO-03 | none | S3 stopped: reads fail and never return zeros, the outage shows in the stats, and service recovers with no dfsctl step |
| 71-smb-s3-stalled-deadline-m | fails: F-14 (cold reads end at 60 s, past RFC 17's 30 s) | RO-03 | none | S3 frozen: every call must end at a deadline, without wrong bytes |
| 61-smb-journal-full-s3-down-m | fails: A-13 (IO_TIMEOUT after 60 s, not DISK_FULL) | BS-14, RO-03, FO-06 | none | a full journal with S3 down; delete and truncate while full; the backlog drains by itself |
| 60-smb-nfs-journal-small-stream-m | passes | BS-14, FO-07, BS-11 | none | 5 GiB over each protocol through a 1 GiB journal; local use stays at the cap |
| 91-smb-125gb-file-sync-xxl | fails: #2910 (uploads stall at about 105 of 125 GB; the drain aborts) | RO-04, FO-07, MS-01, BS-12 | TestFileSizeMatrix (to 100 MB) | 125 GB through one SMB put, then a cold read; an idle second store left unchanged. Needs `SCENARIO_TIMEOUT=10800` |
| 73-smb-nfs-crash-restart-m | passes (F-15 noted: the NFS grace period runs its full 90 s) | ST-04, NFS-04, CP-01 | TestServerRestartRecovery (graceful, metadata only) | SIGKILL with nothing uploaded; journal replay; names, sizes and bytes over both protocols |
| 62-smb-remote-block-damaged-xs | passes | BS-03, BS-01, XP-03 | TestBlocksFlipLifecycle_* (one tampered block) | a missing object as well as an altered one, read over SMB and NFS |
| 12-smb-encryption-roundtrip-s | passes (A-14 noted: a wrong passphrase leaves the share unserved) | BS-10, CP-07, ST-04, BS-02 | none | no plaintext in the bucket; cold reads before and after a restart; a wrong passphrase |
| 10-smb-store-options-xs | fails: #2923 (flags dropped), A-12 (a misspelt key ignored), A-11 (bucket and prefix edits accepted; the data is unreadable after a restart) | CP-07, CP-06, BS-09, ST-04 | none | every option kept or refused; settings bound to stored data |
| 20-smb-nfs-case-across-adapters-xs | fails: E-07 (NFS is case-sensitive, SMB is not; an SMB create overwrites) | XP-06, XP-01, FO-02 | none | one case rule per share, whichever protocol asks |
| 22-smb-share-modes-s | fails: E-08 (the share root lets an add-file open past a read-sharing lister) | SMB-06 | none (smbtorture, outside e2e) | MS-FSA share-mode cases, the share root against a subdirectory, with libsmb2 |
| 30-smb-nfs-permission-revoke-xs | passes | CP-09, PE-01, XP-03 | TestSharePermissions; TestPermissionEnforcement (ENF-04) | a revoke and a read grant take effect on SMB and NFS handles already open |
| 45-smb-snapshot-survives-gc-xs | fails: a share with a snapshot can be removed (GC keeps the blocks and the restore matches) | SN-03, SN-01, CP-08 | TestCLI_* (fake runtime) | a real snapshot kept through GC, restored and read cold |
| 72-smb-snapshot-s3-down-s | fails: #2928 (`--no-verify` waits too; the snapshot restores a later write) | SN-03, SN-01, RO-03 | TestSnapshotHTTP_CreateFailureModes (fake runtime) | a snapshot taken with S3 down, restored after later overwrites |
| 23-smb-notify-durable-s | fails: A-15 (a watch hears nothing made over NFS); the SMB events and both durable suites pass | SMB-08, SMB-07 | none (smbtorture, outside e2e) | change-notify events checked one by one, from both protocols; durable reconnects through smbtorture, installed in the container |
| 90-smb-hour-holds-xxl (about 62 min) | fails: A-17 (GC tracks one block of repeated content), A-16 (16 writers on a 1 GiB journal time out, S3 up); the recycle bin and the journal accounting pass | BS-08, TR-05, BS-13, BS-14 | none | the one-hour dedup hold; the recycle bin's exclude, size cap and retention; journal accounting under 16 parallel writers |

## The matrix

Levels: **Full** means the behaviour is checked directly. **Partial** means it is exercised but not asserted, or only one variant is checked. **—** means not covered. "setup.sh" is the environment every scenario runs in.

### Control plane and CLI

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| CP-01 | Server start, health, status, graceful shutdown | Full | `TestServerLifecycle` | Partial | `setup.sh`, the restart scenarios |  |
| CP-02 | Admin bootstrap and login; credentials kept per context | Full | `TestLoginAsAdmin_XDGIsolation`, `TestServerLifecycle` | Partial | `setup.sh` |  |
| CP-03 | Users: CRUD, password change and reset, forced change on first login | Full | `TestUserCRUD` | Partial | `setup.sh`, `31-smb-nfs-owner-gid-xs`, `32-smb-nfs-trash-two-users-xs` |  |
| CP-04 | Groups and membership | Full | `TestGroupManagement` | — | — |  |
| CP-05 | Metadata stores: CRUD | Full | `TestMetadataStoresCRUD` | Partial | `setup.sh` | #2914 removes the memory and SQL stores |
| CP-06 | Block stores: CRUD | Full | `TestBlockStoresCRUD` | Partial | `setup.sh`, two-share scenarios, `10-smb-store-options-xs`, `11-smb-2stores-same-bucket-xs` | A-11: a used store's bucket and prefix can be edited; a second store on the same bucket and prefix is accepted (MS-07) |
| CP-07 | Block-store options honoured (compression, encryption, parallel uploads; flags and --config) | — | — | Full | `10-smb-store-options-xs`, `12-smb-encryption-roundtrip-s`, `13-smb-compression-frame-magic-xs` | A-10 / #2923: --config drops the flags; A-12: unknown keys are ignored |
| CP-08 | Shares: CRUD and options | Full | `TestSharesCRUD` | Partial | `setup.sh`, `32-smb-nfs-trash-two-users-xs`, `45-smb-snapshot-survives-gc-xs` | a share with a snapshot can be removed (`45-smb-snapshot-survives-gc-xs`) |
| CP-09 | Share permissions: grant and revoke | Full | `TestSharePermissions` | Full | `30-smb-nfs-permission-revoke-xs`, `setup.sh` | the scenario checks that a revoke and a read grant reach open SMB and NFS handles |
| CP-10 | Adapters: enable, disable, port, hot reload | Full | `TestAdapterLifecycle` | Partial | `setup.sh` |  |
| CP-11 | Settings API: validation, PATCH vs PUT, hot reload, version tracking | Full | `TestControlPlaneV2_SettingsValidation`, `TestControlPlaneV2_PatchVsPut`, `TestControlPlaneV2_SettingsHotReload`, `TestControlPlaneV2_SettingsVersionTracking`, `TestNFSv4ControlPlaneSettingsHotReload` | — | — |  |
| CP-12 | Netgroups | Full | `TestControlPlaneV2_NetgroupCRUD`, `TestControlPlaneV2_NetgroupInUse`, `TestNFSv4ControlPlaneNetgroup` | — | — |  |
| CP-13 | Share security policy, blocked operations, delegation policy | Full | `TestControlPlaneV2_ShareSecurityPolicy`, `TestControlPlaneV2_BlockedOperations`, `TestControlPlaneV2_DelegationPolicy`, `TestNFSv4ControlPlaneBlockedOps`, `TestNFSv4ControlPlaneMultipleBlockedOps` | — | — |  |
| CP-14 | CLI contexts (several servers) | Full | `TestContextManagement` | — | — |  |
| CP-15 | Block-store operations: stats, drain, evict, GC with JSON results | Partial | `TestBlocksFlipLifecycle_NFS`, `TestBlocksFlipLifecycle_SMB`, `TestBlockStoreImmutableOverwrites` | Full | most S3 scenarios | e2e uses them as helpers and does not assert the JSON numbers |
| CP-16 | Share warm and offline-safe check | — | — | Full | `53-smb-2shares-share-scoped-views-xs` | #2906 |
| CP-17 | Whole control-plane lifecycle through the API | Full | `TestControlPlaneV2_FullLifecycle` | — | — |  |

### File operations

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| FO-01 | NFSv3 basic file and directory operations (kernel client) | Full | `TestNFSFileOperations`, `TestNFSv4BasicOperations`, `TestBackwardCompatNFSv3Full`, `TestStoreMatrixOperations` | — | — |  |
| FO-02 | NFSv4.0 basic file and directory operations | Full | `TestNFSv4BasicOperations`, `TestNFSv4GoldenPathSmoke` | Partial | `00-smb-nfs-roundtrip-gc-xs` and the other `smb-nfs-*` scenarios | scenarios use libnfs (userspace), not a kernel mount |
| FO-03 | NFSv4.1 file operations | Full | `TestStoreMatrixV4`, `TestNFSv41SessionEstablishment` | — | — |  |
| FO-04 | NFSv4.2: clone, sparse files, xattrs | Full | `TestNFSv42Clone`, `TestNFSv42Sparse`, `TestNFSv42XattrRoundTrip` | — | — |  |
| FO-05 | SMB3 file and directory operations | Full | `TestSMBFileOperations`, `TestSMB3_GoSMB2_BasicFileOps`, `TestSMB3_GoSMB2_DirectoryOps`, `TestSMB3_SmbClient_FileOps`, `TestSMB3_SmbClient_DirectoryOps`, `TestSMB3_SmbClient_Connect` | Full | every scenario |  |
| FO-06 | Symlinks, hard links, chmod, chown, truncate | Full | `TestNFSv4AdvancedFileOps` | Partial | `24-smb-nfs-truncate-during-upload-xs`, `26-smb-truncate-same-content-s`, `25-smb-nfs-truncate-same-content-s`, `61-smb-journal-full-s3-down-m` | scenarios: truncate only; B-01 / #2924: truncating files that share chunks fails |
| FO-07 | Large files with checksum integrity | Full | `TestFileSizeMatrix`, `TestSMB3_GoSMB2_LargeFile` | Full | `00-smb-nfs-roundtrip-gc-xs`, `54-smb-2shares-idle-disk-used-s`, `60-smb-nfs-journal-small-stream-m`, `91-smb-125gb-file-sync-xxl`, the small-large-gc and extra-share scenarios | scenarios go to 125 GB over SMB and 5 GiB over NFS; e2e to 100 MB over NFS, 1 MB over SMB |
| FO-08 | Directory listing at scale (pagination, many entries) | Full | `TestNFSv4READDIRPagination`, `TestSMB3_GoSMB2_MultipleFiles`, `TestStressLargeDirectory` | Full | `21-smb-nfs-concurrent-creates-xs` | the scenario lists 1,280 entries over NFS and SMB; the 500-entry stress test is not run by default |
| FO-09 | Open and create modes (exclusive create etc.) | Full | `TestNFSv4OpenCreateModes` | — | — |  |
| FO-10 | Rename in the share root while a client holds the root open (Explorer) | — | — | Full | `01-smb-rename-in-share-root-xs` | #2907, fixed by PR #2915 with a unit test; in cron |
| FO-11 | Userspace clients (smbclient, libnfs) without kernel mounts | Partial | `TestSMB3_SmbClient_Connect`, `TestSMB3_SmbClient_FileOps`, `TestSMB3_SmbClient_DialectNegotiation`, `TestSMB3_SmbClient_DirectoryOps` | Full | every scenario | no e2e test uses a userspace NFS client |
| FO-12 | A file deleted while open stays readable through its handle, and is released after the close | — | — | Full | `43-smb-nfs-open-unlinked-gc-xs` | #2927: after an evict, the open handle reads zeros |

### Stores and persistence

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| ST-01 | Metadata x block store matrix | Full | `TestStoreMatrixOperations`, `TestStoreMatrixV4` | Partial | `setup.sh` | scenarios: badger + S3 only; #2914 reduces the matrix to badger |
| ST-02 | S3-compatible servers | Partial | `TestS3CompatiblePresets`, `TestBlocksFlipLifecycle_NFS` | Partial | every S3 scenario | e2e: MinIO, LocalStack; scenarios: SeaweedFS |
| ST-03 | Cubbit DS3 as the remote | — | — | — | — | the retired live canary used it; no test does now |
| ST-04 | Persistence across a server restart | Full | `TestServerRestartRecovery`, `TestNFSv42XattrPersistAcrossRestart`, `TestNFSv41SessionRecoveryAfterRestart` | Full | `73-smb-nfs-crash-restart-m`, `44-smb-unlink-crash-gc-xs`; restarts in `12-smb-encryption-roundtrip-s`, `10-smb-store-options-xs`, `41-smb-dedup-delete-original-gc-xs` | e2e: a graceful restart, metadata only (memory payload); scenarios: SIGKILL, journal replay, cold reads |
| ST-05 | Stale handles after an ephemeral restart | Full | `TestNFSv4StaleHandle`, `TestStaleNFSHandle` | — | — |  |

### Multiple shares

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| MS-01 | Shares isolated, each with its own stores | Full | `TestMultiShareIsolation`, `TestMultiShareConcurrent` | Partial | two-share scenarios, `55-smb-extra-share-gc-xs`, `91-smb-125gb-file-sync-xxl` | e2e: separate memory metadata stores per share; scenarios: one metadata store |
| MS-02 | One block store shared by several shares | Partial | `TestMultiShareIsolation` | Full | `50-smb-2shares-one-store-gc-xs` | e2e: subtest SameBlockStore, no GC; the scenario runs GC on one share |
| MS-03 | One metadata store, two remotes: GC deletes through the owning remote | — | — | Full | `52-smb-2shares-small-large-gc-xs`, `55-smb-extra-share-gc-xs`, `56-smb-extra-share-gc-minimal-xs`; `42-smb-1share-small-large-gc-xs` as the control | A-08 / #2909, PR #2919 |
| MS-04 | One metadata store, two remotes: same content on both shares, cold read | — | — | Full | `51-smb-2shares-same-content-xs` | A-07 / #2909 |
| MS-05 | Share-scoped stats, offline check and warm | — | — | Full | `53-smb-2shares-share-scoped-views-xs` | #2906 |
| MS-06 | Per-share local disk accounting with seeded ranges | — | — | Full | `54-smb-2shares-idle-disk-used-s` | #2908 at the consumer |
| MS-07 | Two block stores on one bucket and prefix: refused, or never deleting each other's objects | — | — | Full | `11-smb-2stores-same-bucket-xs` | accepted today; the other store's data survived GC and reclaim |

### Block store and data path

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| BS-01 | Upload to S3 in the blocks/ layout | Full | `TestBlocksFlipLifecycle_NFS`, `TestBlocksFlipLifecycle_SMB` | Full | `00-smb-nfs-roundtrip-gc-xs`, `62-smb-remote-block-damaged-xs` |  |
| BS-02 | Cold read from S3 after evict (NFS and SMB) | Full | `TestBlocksFlipLifecycle_NFS`, `TestBlocksFlipLifecycle_SMB` | Full | `00-smb-nfs-roundtrip-gc-xs`, small-large-gc scenarios, `13-smb-compression-frame-magic-xs`, `12-smb-encryption-roundtrip-s`, dedup scenarios | C-05: the NFS variant does not drop the page cache |
| BS-03 | Tampered remote block fails closed (BLAKE3) | Full | `TestBlocksFlipLifecycle_NFS`, `TestBlocksFlipLifecycle_SMB` | Full | `62-smb-remote-block-damaged-xs` | the scenario covers a missing object as well as an altered one, over SMB and NFS |
| BS-04 | Delete + GC frees the remote objects (one share) | Full | `TestBlocksFlipLifecycle_NFS`, `TestBlocksFlipLifecycle_SMB` | Full | `00-smb-nfs-roundtrip-gc-xs`, `42-smb-1share-small-large-gc-xs`, `44-smb-unlink-crash-gc-xs` |  |
| BS-05 | GC run accounting (objects swept, bytes freed, hashes marked) | — | — | Full | small-large-gc scenarios, `55-smb-extra-share-gc-xs`, `50-smb-2shares-one-store-gc-xs` | C-11, C-12 |
| BS-06 | Overwrites never rewrite an existing block object | Full | `TestBlockStoreImmutableOverwrites` | — | — |  |
| BS-07 | Dedup: ObjectID, concurrent identical writes, VM-fleet ratio | Partial | `TestObjectIDPopulation_NFSWriteQuiesce`, `TestDedupRace_NFSv4_ConcurrentIdenticalWrites`, `TestDEDUP03_VMFleet40Pct` | Partial | `40-smb-dedup-same-content-xs`, `41-smb-dedup-delete-original-gc-xs` | e2e: nightly tier, never run in CI (C-01), broken at setup (C-02, C-03); scenarios: sequential copies and partial overlap, no concurrent writes |
| BS-08 | Repeated content: GC frees it after the dedup hold | — | — | Full | `90-smb-hour-holds-xxl` | G-02; A-17: of the 3 or 4 blocks the content goes up as, GC only ever tracks one, so the rest stay after the hour |
| BS-09 | Block compression, incl. a chunk that looks like a frame | — | — | Partial | `13-smb-compression-frame-magic-xs`, `10-smb-store-options-xs` | #2897; zstd only |
| BS-10 | Block encryption (AEAD, KMIP) | — | — | Partial | `12-smb-encryption-roundtrip-s` | AES-256-GCM with a local key file; no KMIP (C-04). A-14: a wrong passphrase leaves the share unserved |
| BS-11 | Local Disk Used matches the files on disk | — | — | Full | `54-smb-2shares-idle-disk-used-s`, `60-smb-nfs-journal-small-stream-m` | #2908 |
| BS-12 | Upload drain completes and leaves nothing pending | Partial | `TestBlockStoreImmutableOverwrites`, `TestBlocksFlipLifecycle_NFS` | Full | every S3 scenario |  |
| BS-13 | Journal GC racing eviction | — | — | Partial | `90-smb-hour-holds-xxl` | #2908 cause; unit test in PR #2918. The scenario exercises the race under 16 parallel writers but cannot force it; the accounting matched in every round |
| BS-14 | Local tier size limit and eviction under pressure | — | — | Full | `60-smb-nfs-journal-small-stream-m`, `61-smb-journal-full-s3-down-m`, `90-smb-hour-holds-xxl` | A-13: a full journal answers IO_TIMEOUT after 60 s, not DISK_FULL; A-16: with 16 parallel writers on a 1 GiB journal every write times out, S3 up |
| BS-15 | Files that share chunks: deleting or truncating one leaves the others intact | Partial | `TestNFSv42Clone` (CLONE-02: a write to one side of a clone) | Full | `41-smb-dedup-delete-original-gc-xs`, `50-smb-2shares-one-store-gc-xs`, `26-smb-truncate-same-content-s`, `25-smb-nfs-truncate-same-content-s` | B-01 / #2924; e2e: only a write to one side of a clone |

### SMB protocol

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| SMB-01 | NTLM session setup, valid and invalid credentials | Full | `TestSMB3_GoSMB2_SessionSetup` | Partial | every scenario |  |
| SMB-02 | Dialect negotiation | Full | `TestSMB3_SmbClient_DialectNegotiation` | Partial | every scenario |  |
| SMB-03 | Signing and transport encryption | Full | `TestSMB3_GoSMB2_Signing`, `TestSMB3_GoSMB2_Encryption` | — | — |  |
| SMB-04 | Byte-range locks | Full | `TestSMBByteRangeLocking` | — | — |  |
| SMB-05 | Leases and oplocks, breaks | Full | `TestCrossProtocol_LeaseBreaks`, `TestGracePeriodWithSMBLeases` | — | — |  |
| SMB-06 | Share modes (sharing violations) | — | — | Full | `22-smb-share-modes-s`, `01-smb-rename-in-share-root-xs` | E-08: the share root lets an add-file open past a read-sharing lister |
| SMB-07 | Durable handles and reconnect | — | — | Full | `23-smb-notify-durable-s` | smbtorture's durable-open and durable-v2-open suites, installed in the container; all pass but delete_on_close2, which Samba's own run skips |
| SMB-08 | Change notify | — | — | Full | `23-smb-notify-durable-s` | A-15: a watch hears SMB changes but none made over NFS |
| SMB-09 | Security descriptors and ACLs | Partial | `TestNFSv4ACLCrossProtocol` | — | — |  |

### NFS protocol

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| NFS-01 | NLM byte-range locks (NFSv3) and rpcbind | Full | `TestNLMAxisInterop`, `TestNLMSystemRpcbindRegistration`, `TestNFSv4Locking` | — | — | TestNLMAxisInterop fails in a container (veth) |
| NFS-02 | Embedded portmapper | Full | `TestPortmapper` | — | — |  |
| NFS-03 | NFSv4 byte-range and blocking locks | Full | `TestNFSv4Locking`, `TestNFSv4BlockingLock` | — | — |  |
| NFS-04 | Grace period and reclaim, incl. across protocols | Full | `TestGracePeriodRecovery`, `TestGracePeriodUnclaimedLocks`, `TestCrossProtocolReclaim`, `TestGracePeriodNewLockBlocked`, `TestGracePeriodTiming`, `TestGracePeriodEarlyExit`, `TestGracePeriodWithSMBLeases` | Partial | `73-smb-nfs-crash-restart-m` | the scenario waits out the grace period after a crash, with no reclaim; F-15: it runs the full 90 s |
| NFS-05 | Delegations: grant, recall, revoke, backchannel | Full | `TestNFSv4DelegationBasicLifecycle`, `TestNFSv4DelegationRecall`, `TestNFSv4DelegationRevocation`, `TestNFSv4NoDelegationConflict`, `TestNFSv41BackchannelDelegationRecall`, `TestStressConcurrentDelegations` | — | — | F-12: namespace-change recalls missing |
| NFS-06 | Directory delegations (CB_NOTIFY) | Full | `TestNFSv41DirDelegationEntryAdded`, `TestNFSv41DirDelegationEntryRemoved`, `TestNFSv41DirDelegationEntryRenamed`, `TestNFSv41DirDelegationAttrChanged`, `TestNFSv41DirDelegationCleanup` | — | — |  |
| NFS-07 | NFSv4.1 sessions, EOS replay, disconnects | Full | `TestNFSv41SessionEstablishment`, `TestNFSv41MultipleSessions`, `TestNFSv41EOSReplayOnReconnect`, `TestNFSv41EOSConnectionDisruption`, `TestNFSv41DisconnectDuringLargeWrite`, `TestNFSv41DisconnectDuringReadDir`, `TestNFSv41DisconnectDuringSessionSetup` | — | — |  |
| NFS-08 | NFSv3, v4.0 and v4.1 on the same share | Full | `TestNFSv41v40Coexistence`, `TestNFSv41v3Coexistence` | — | — |  |
| NFS-09 | Pseudo-filesystem browsing | Full | `TestNFSv4PseudoFSBrowsing` | — | — |  |
| NFS-10 | NFSv4 ACLs: lifecycle, inheritance, enforcement | Full | `TestNFSv4ACLLifecycle`, `TestNFSv4ACLInheritance`, `TestNFSv4ACLAccessEnforcement`, `TestNFSv4ACLCrossProtocol` | — | — |  |
| NFS-11 | Squash and root squash | Full | `TestSquashBehavior`, `TestNFSRootSquash` | — | — |  |
| NFS-12 | Adapter restart and client reconnection | Full | `TestClientReconnection` | — | — |  |

### Cross-protocol

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| XP-01 | Create on one protocol, read, list or delete on the other | Full | `TestCrossProtocolInterop` | Full | `20-smb-nfs-case-across-adapters-xs`, `21-smb-nfs-concurrent-creates-xs`, `31-smb-nfs-owner-gid-xs`, `32-smb-nfs-trash-two-users-xs`, `24-smb-nfs-truncate-during-upload-xs` |  |
| XP-02 | Locks across protocols | Full | `TestCrossProtocolLocking`, `TestCrossProtocolLockingByteRange` | — | — |  |
| XP-03 | Same error on both protocols | Full | `TestCrossProtocol_ErrorConformance` | Partial | `32-smb-nfs-trash-two-users-xs`, `62-smb-remote-block-damaged-xs`, `30-smb-nfs-permission-revoke-xs` | the scenario shows a wrong error (I/O instead of access denied) on both |
| XP-04 | xattr parity | Full | `TestCrossProtocolXattrParity` | — | — |  |
| XP-05 | Owner and group of a file created over SMB, seen over NFS | — | — | Full | `31-smb-nfs-owner-gid-xs` | E-01 / #2922 |
| XP-06 | One case rule per share, whichever protocol asks | — | — | Full | `20-smb-nfs-case-across-adapters-xs` | E-07: NFS is case-sensitive, SMB is not, and an SMB create overwrites |

### Permissions and identity

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| PE-01 | Permission enforcement: read-only, no access, group removal | Full | `TestPermissionEnforcement`, `TestNFSFileOperations` | Partial | `30-smb-nfs-permission-revoke-xs` | C-06: ENF-02 takes any mount failure as denied; the scenario checks no access and read-only per call, not group removal |
| PE-02 | Several users writing and deleting on one share | — | — | Full | `32-smb-nfs-trash-two-users-xs` | C-14 |
| PE-03 | NFS Kerberos (krb5, krb5i, krb5p) | Full | `TestKerberos`, `TestNFSv4KerberosExtended`, `TestCrossProtocolKerberosIdentity` | — | — | fails in a container without rpcsec_gss_krb5 |
| PE-04 | SMB Kerberos | Full | `TestSMBKerberos`, `TestSMB3_KerberosFeatureMatrix` | — | — |  |
| PE-05 | Admin-only settings enforced on the mount | — | — | Full | `32-smb-nfs-trash-two-users-xs` | A-06 / #2921 |

### Recycle bin

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| TR-01 | Recycle and restore over NFS | Full | `TestNFSTrashRecycleAndRestore` | Partial | `32-smb-nfs-trash-two-users-xs` | the scenario lists the bin over NFS; no restore |
| TR-02 | Recycle and restore over SMB (delete-on-close) | Full | `TestSMBTrashRecycleAndRestore` | Partial | `32-smb-nfs-trash-two-users-xs` | no restore |
| TR-03 | Several users deleting on a recycle-bin share | — | — | Full | `32-smb-nfs-trash-two-users-xs` | E-06 / #2920 |
| TR-04 | Restrict emptying to admins | — | — | Full | `32-smb-nfs-trash-two-users-xs` | A-06 / #2921 |
| TR-05 | Retention, size cap and exclude patterns | — | — | Partial | `90-smb-hour-holds-xxl` | exclude, size cap, and retention not purging early; not the purge once the retention days pass |

### Snapshots

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| SN-01 | Snapshot CLI: create, list, show, delete, restore | Full | `TestCLI_CreateNoWait`, `TestCLI_CreateBlock`, `TestCLI_List_TableMode`, `TestCLI_List_JSON`, `TestCLI_List_YAML`, `TestCLI_Show`, `TestCLI_Delete_RefusesWithoutYes`, `TestCLI_Delete_YesFlag`, `TestCLI_Restore_RefusesEnabled`, `TestCLI_Restore_Disabled_YesFlag_Success`, `TestCLI_Restore_NotDurable_NoForce`, `TestCLI_Restore_NotDurable_Force` | Partial | `45-smb-snapshot-survives-gc-xs`, `72-smb-snapshot-s3-down-s` | e2e: against a fake runtime; scenarios: create, list and restore on a real server |
| SN-02 | Snapshot HTTP API and failure modes | Full | `TestSnapshotHTTP_HappyPath`, `TestSnapshotHTTP_RestoreFailureModes`, `TestSnapshotHTTP_CreateFailureModes` | — | — | against a fake runtime |
| SN-03 | Snapshot restore on a real server, checked over a mount | — | — | Full | `45-smb-snapshot-survives-gc-xs`, `72-smb-snapshot-s3-down-s` | #2928: a snapshot taken with S3 down restores later writes; a share with a snapshot can be removed |

### Robustness and operations

| ID | Capability | e2e | e2e tests | Scenarios | Scenario coverage | Notes |
|---|---|---|---|---|---|---|
| RO-01 | Client disconnects mid-operation | Full | `TestNFSv41DisconnectDuringLargeWrite`, `TestNFSv41DisconnectDuringReadDir`, `TestNFSv41DisconnectDuringSessionSetup` | — | — |  |
| RO-02 | Concurrent clients and many files | Partial | `TestMultiClientConcurrency`, `TestStressConcurrentFileCreation` | Full | `21-smb-nfs-concurrent-creates-xs` | the e2e stress tests are not run by default |
| RO-03 | S3 failures: errors, timeouts, hangs | — | — | Full | `70-smb-s3-down-reads-fail-m`, `71-smb-s3-stalled-deadline-m`, `61-smb-journal-full-s3-down-m`, `24-smb-nfs-truncate-during-upload-xs`, `72-smb-snapshot-s3-down-s` | F-14: cold reads end at 60 s, past RFC 17's 30 s |
| RO-04 | Long, large syncs | — | — | Full | `91-smb-125gb-file-sync-xxl` | #2910 reproduced at 125 GB: uploads stall at about 105 GB |
| RO-05 | Timing regressions caught | — | — | Full | every scenario | slow alerts against each scenario's own record of times |

## Where the e2e coverage is weaker than it looks

Some rows count as covered in the matrix, but the tests behind them do not run, or check less than their names suggest. The `C-` IDs are findings of the same review.

| Area | What the matrix counts | What actually happens |
|---|---|---|
| Dedup (BS-07) | Partial: 3 tests | The nightly tier never runs in CI: `DITTOFS_E2E_NIGHTLY` is never set (C-01). Run by hand, it fails at setup on DittoFS's S3 endpoint guard (C-02), and past that it asserts the old `cas/` layout (C-03). Its real coverage is zero. |
| Shared chunks (BS-15) | Partial: 1 test | `TestNFSv42Clone` writes to one side of a clone. No test deletes or truncates a file whose chunks another file shares, which is how #2924 got through. |
| Block encryption (BS-10) | none | The KMIP interop tests exist at another tier but never run in CI (C-04), and no e2e test turns encryption on. |
| Cold reads over NFS (BS-02) | Full | `TestBlocksFlipLifecycle_NFS` does not drop the client's page cache before its cold reads, so it can pass from cache (C-05). That is the intermittent macOS failure. |
| Restart (ST-04) | Full | `TestServerRestartRecovery` stops the server gracefully and checks metadata only; its payload store is memory. No e2e test crashes the server. |
| Permission enforcement (PE-01) | Full | ENF-02 treats any mount failure as "access denied" (C-06), so it can pass for the wrong reason. |
| The CI gate itself | — | CI's wrapper grades on `ok` lines, so a `-run` filter that matches no test still reports a pass (C-07). |
| GC (BS-04, BS-05) | Full / none | GC is tested end to end only on one share, one remote and one metadata store. `audit-refcounts` reports clean while blocks leak (C-11), and GC reports nothing about the hashes it holds back (C-12). |
| Multiple shares (MS-01) | Full | `TestMultiShareIsolation` gives each share its own memory metadata store and block store. The layout behind #2906 to #2909, one metadata store serving several remotes, is never built. |
| Snapshots (SN-01, SN-02) | Full: 15 tests | All 15 run against an in-process HTTP server with a fake runtime. They cover the CLI and API surface, but no snapshot of real data is ever taken or restored. |
| Store matrix (ST-01) | Full | memory, badger and postgres metadata × memory and S3 block stores, with S3 meaning LocalStack or MinIO. #2914 removes the memory and SQL metadata stores, so the matrix shrinks to badger. No test uses Cubbit DS3. |
| Recycle bin (TR-01 to TR-04) | Full for one user | Both tests use one user (C-14), which is how E-06 (#2920) and A-06 (#2921) got through. |
| Kerberos and NLM (PE-03, NFS-01) | Full | These need a real kernel. They fail in the Linux dev container (no `rpcsec_gss_krb5`; NLM's veth setup), so they are checked only on CI's GitHub runners. |

## What the e2e suite should add

The list is ordered by risk to data and to Cubbit's deployment. Where a scenario already reproduces the case, it can be ported almost line for line into a Go test with the e2e helpers.

**Priority 1: data integrity and Cubbit's layout**

1. **One metadata store with two S3 remotes, then GC and cold reads per remote** (MS-03, MS-04).
   - **What it checks:** after each delete and GC, objects leave the right bucket and the other bucket is untouched; the same content on both shares can be read back cold from each.
   - **How:** the pass order is random, so repeat the GC or force the order.
   - **Basis:** `52-smb-2shares-small-large-gc-xs`, `51-smb-2shares-same-content-xs`, `55-smb-extra-share-gc-xs` (one round per file), and `42-smb-1share-small-large-gc-xs` as the control. Covers A-07 and A-08 (#2909) and guards PR #2919 and the full fix that will replace it.
2. **Views scoped to a share on a shared metadata store** (MS-05, CP-16). Stats, offline check and warm, from `53-smb-2shares-share-scoped-views-xs` (#2906).
3. **A file deleted while open** (FO-12). The open handle must read the file's data after the delete, a GC and an evict, and the blocks must go only after the close. From `43-smb-nfs-open-unlinked-gc-xs` (#2927).
4. **Files that share chunks** (BS-15). Delete the original of deduplicated copies and GC; truncate two files with the same content to the same size; read the others back cold. From `41-smb-dedup-delete-original-gc-xs`, `50-smb-2shares-one-store-gc-xs` and the two truncate scenarios (#2924).
5. **Two block stores on one bucket and prefix** (MS-07). The second must be refused, or the two must never delete each other's objects. From `11-smb-2stores-same-bucket-xs`.
6. **Snapshot restore on a real server, checked over a mount** (SN-03). A snapshot keeps its blocks through GC, restores them, and blocks removing its share; a snapshot taken with S3 down restores the version it saw. From `45-smb-snapshot-survives-gc-xs` and `72-smb-snapshot-s3-down-s` (#2928). This was priority 3 before both failed.
7. **Block compression and encryption, with eviction and a cold read** (BS-09, BS-10).
   - **Compression:** include a chunk that looks like a compression frame (#2897, `13-smb-compression-frame-magic-xs`), and run zstd and lz4 alike.
   - **Encryption:** AEAD with a local key, from `12-smb-encryption-roundtrip-s`: no plaintext in the bucket, a restart, a wrong passphrase (A-14). Add KMIP if CI can host PyKMIP (C-04).
8. **Local-tier accounting** (BS-11, MS-06). After deletes, evict and GC, Local Disk Used must equal the bytes of the segment files, from `54-smb-2shares-idle-disk-used-s`. The journal-level race behind #2908 already has a unit test (PR #2918).
9. **S3 failures** (RO-03).
   - **The cases:** the remote refuses, is slow, or hangs during a write, a cold read and a GC.
   - **The bar:** errors are bounded, reads never return wrong bytes, nothing hangs, and service recovers once the remote is back.
   - **Basis:** `70-smb-s3-down-reads-fail-m`, `71-smb-s3-stalled-deadline-m` (F-14) and `61-smb-journal-full-s3-down-m` (A-13). They stop or freeze the S3 process; the e2e helpers would need the same control over their S3 container, or a fault-injecting proxy.
10. **Cubbit DS3 as the remote** (ST-03). A nightly job against a real DS3 bucket under a test prefix, with the credentials as a CI secret. The retired live canary showed that behaviour on the S3 side matters: the evening slowdown came from S3, not DittoFS.

**Priority 2: permissions, identity, the recycle bin, GC reporting, options**

11. **The recycle bin with two users, and the admin-only restriction** (TR-03, TR-04, PE-02, PE-05). Extend the two existing trash tests with a second user (C-14). Covers #2920 and #2921, from `32-smb-nfs-trash-two-users-xs`.
12. **Ownership across protocols** (XP-05). The owner and group of a file created over SMB, seen over NFS, for a user with and without groups. Covers #2922, from `31-smb-nfs-owner-gid-xs`.
13. **One case rule across protocols** (XP-06). Names created over NFS that differ only in case, then a create over SMB. Covers E-07, from `20-smb-nfs-case-across-adapters-xs`.
14. **Rename in the share root while a client holds it open** (FO-10). An e2e regression for #2907: PR #2915 added a unit test, but nothing at e2e level. From `01-smb-rename-in-share-root-xs`.
15. **The share-mode rules beyond the root** (SMB-06), including the follow-ups Marco asked for on #2915. `22-smb-share-modes-s` drives them with libsmb2 through the `smb-open` tool; the e2e helpers' `go-smb2` cannot set share modes and access masks directly. Covers E-08.
16. **GC's numbers, checked against the bucket** (BS-05). Objects swept and bytes freed must match what actually left S3, and the hashes GC holds back must be reported (C-11, C-12). From the `small-large-gc` scenarios and `50-smb-2shares-one-store-gc-xs`.
17. **Block-store options set through the CLI** (CP-07). With and without `--config`, the store must carry the options it was given or refuse them (#2923, A-12); a used store's bucket and prefix must not change (A-11). From `10-smb-store-options-xs`.

**Priority 3: breadth**

18. **A local-tier size limit, a full journal, and many writers on a capped journal** (BS-14, BS-13). From `60-smb-nfs-journal-small-stream-m`, `61-smb-journal-full-s3-down-m` (A-13) and `90-smb-hour-holds-xxl` (A-16).
19. **Durable-handle reconnect and change notify over SMB** (SMB-07, SMB-08), with changes made over NFS too. From `23-smb-notify-durable-s` (A-15).
20. **Recycle-bin retention, size cap and exclude patterns** (TR-05). From `90-smb-hour-holds-xxl`; the size cap needs the reaper's hourly pass.
21. **Repeated content after the dedup hold** (BS-08, G-02). It takes over an hour, so it belongs in a nightly run. From `90-smb-hour-holds-xxl` (A-17).
22. **A long, large sync** (RO-04), as a nightly or weekly soak. `91-smb-125gb-file-sync-xxl` reproduces #2910 at 125 GB.
23. **Scheduled runs of what exists but never runs:** the `stress` tests (RO-02), and the dedup tier once C-02 and C-03 are fixed (BS-07).

Items 1 to 17 (priorities 1 and 2) cover 24 rows. With them, the e2e suite's weighted coverage goes from 65% to 88%, and the capabilities it covers from 70 to 93 of 101 (92%). That closes every gap where a scenario has found a defect, except the journal (A-13, A-16, item 18), change notify (A-15, item 19), repeated content (A-17, item 21) and the large sync (#2910, item 22).

## Limits of this matrix

- **What it counts:** behaviours, not code. One row can hide very different depths: NFS-07 sits on seven tests, XP-05 on one scenario. The rows are judgment calls, made from the tests' doc comments and their code.
- **When it was measured:**
  - e2e results are from the 2026-09-29 runs at `d562e665`;
  - scenario results are from ditto on 2026-10-01 at develop `18d85aec` plus the scenarios (up to `d2a021bc`);
  - #2914 (removing the SQL and memory stores) will change the store rows when it lands.
- **A pass is a pass at this scale.** `24-smb-nfs-truncate-during-upload-xs` does not check that the upload is in flight when it truncates, and `21-smb-nfs-concurrent-creates-xs` runs 64 clients where RFC 7's appendix predicts conflicts under more load.
- **What the scenarios trade away:** they never mount, by design. They use `smbclient`, libsmb2 and libnfs (NFSv4.0) inside a throwaway container, so kernel-client behaviour, NFSv3, NFSv4.1 and 4.2, locking and delegations stay with the e2e suite.
- **What else exists:** conformance suites (smbtorture, WPTS, pynfs, pjdfstest) cover protocol detail beyond both suites and are not counted here.
