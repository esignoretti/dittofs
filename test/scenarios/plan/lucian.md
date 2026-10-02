# Lucian's ten

The shared tools are listed in [README.md](README.md). "Today" is a prediction from a finding or an RFC appendix row, not a result.

## L1 `70-smb-s3-down-reads-fail-m`

**Validates:** with the remote down, a read of evicted data fails with an error and never returns zeros. The outage is reported, and the share recovers without an operator.

**Steps:**
1. Write `a.bin` (64 MiB, random) over SMB, drain and evict.
2. Run `s3 stop`.
3. Read `a.bin` over SMB (`smbclient get`) and over NFS (`nfs-cp`).
4. Write `b.bin` (1 MiB).
5. Run `s3 start`.
6. Poll for up to 120 s until `a.bin` reads.

**Must hold:**
- Both reads in step 3 fail. Bytes returned before an error must match the start of `a.bin`. No full-size file of zeros may appear.
- `dfsctl store block stats` shows `remote_healthy: false` while S3 is down, and `true` again after it is back.
- The write of `b.bin` succeeds, because the journal accepts it.
- After `s3 start`, without any `dfsctl` intervention, `a.bin` reads and matches, the drain completes, and `b.bin` is in the bucket.

**Basis:**
- RFC 0 §10: "Reads of Remote extents fail; they MUST NOT return zeros."
- RFC 8 §7.5: an unreachable remote fails the read, distinguishably.
- RFC 0 §10.2: no state requires intervention to leave.
- RO-03.

**Today:** unknown. Record how long the failing read takes; that duration is L2's subject.

## L2 `71-smb-s3-stalled-deadline-m`

**Validates:** when S3 hangs (it accepts connections and never answers), every client call still ends at a deadline.

**Steps:**
1. Write `a.bin` and drain. Write `c.bin` and leave it un-drained. Evict.
2. Run `s3 pause`.
3. Run `timeout 300` around three operations, recording the elapsed seconds of each: an SMB read of `a.bin`, an NFS read of `a.bin`, and an SMB write of 2 GiB, which forces the upload path.
4. While S3 is paused, list and stat from a second client over SMB and NFS.
5. Run `s3 resume`, then read `a.bin` again.

**Must hold:**
- Each call in step 3 returns, with success or an error, within 45 s. RFC 17 §4.3's default deadline is 30 s; 15 s is margin.
- None of them hits the 300 s timeout.
- The step 4 metadata calls answer within 5 s.
- After `s3 resume`, `a.bin` reads and matches.

**Basis:**
- RFC 0 §10.3: "every wait on that operation's path MUST end at the deadline".
- RFC 17 §4.3: a 30 s default deadline.
- F-05: NFS has no per-request deadline. Hung requests can stop a connection's read loop.
- F-07: several S3 calls rely only on HTTP and retry limits (2 min × 10).
- RO-03.

**Today:** fails. Calls are expected to wait minutes.

## L3 `61-smb-journal-full-s3-down-m`

**Validates:** a full journal refuses writes as "no space", not as an I/O error and not with a hang. Space-freeing operations still work, and the backlog drains once S3 returns.

**Steps:**
1. Add block store `s3-small` (bucket `dittofs`, prefix `small/`). Create share `/small --journal-size 1GiB`, read-write for tester.
2. Run `s3 stop`.
3. Write 100 MiB files over SMB, up to 15 of them, until one is refused. Run each write under `timeout 300`.
4. Delete one file and truncate another (`smb-truncate`).
5. Run `s3 start`, then `drain-uploads`.
6. Evict, then cold-read every file that was accepted.

**Must hold:**
- The refusal is `NT_STATUS_DISK_FULL`, not `NT_STATUS_IO_DEVICE_ERROR` and not a timeout.
- Files accepted before the refusal read back intact while S3 is still down.
- The delete and the truncate succeed while the journal is full.
- The drain completes with no `dfsctl` intervention, and the cold reads match.

**Basis:**
- RFC 0 §10.3: a write refused for space reaches the client as "no space", not as an I/O error.
- RFC 0 §10.1: capacity is a bound, not a target.
- RFC 1 §7: records without bytes are never refused.
- F-06: backpressure resets its wait budget on any drain progress.
- BS-14, RO-03.

**Today:** unknown. F-06 suggests the write may wait instead of being refused.

## L4 `60-smb-nfs-journal-small-stream-m`

**Validates:** with S3 up, a journal much smaller than the data still takes it all, without errors and within its bound.

**Steps:**
1. Use share `/small` with `--journal-size 1GiB`, as in L3.
2. Write a 5 GiB file over SMB, then a 5 GiB file over NFS (`nfs-cp`). Sample `local_disk_used` every 5 s.
3. Drain, evict, and cold-read both files.

**Must hold:**
- No write error.
- The peak `local_disk_used` stays within 1.25 × 1 GiB. The cap is a soft threshold, so the scenario records the peak.
- Both cold reads match by sha256.
- The client MB/s for each file is printed; it is the comparison point for #2910.

**Basis:**
- RFC 0 §10, when the journal is full and the remote is up: "The write is accepted once space is free, or refused at its deadline."
- RFC 0 §10.1.
- BS-14. Also the #2910 decay seen at 125 GB.

**Today:** unknown.

## L5 `62-smb-remote-block-damaged-xs`

**Validates:** a missing or altered remote block makes the read fail. The read never returns zeros or wrong bytes.

**Steps:**
1. Write `a.bin`, `b.bin` and `c.bin` (32 MiB each, random), draining after each. The difference in `rclone lsf` after each drain gives each file's objects.
2. Delete one of `a.bin`'s objects (`rclone deletefile`).
3. Overwrite one of `b.bin`'s objects with random bytes of the same size (`rclone rcat`).
4. Evict, then read all three over SMB and over NFS.

**Must hold:**
- The reads of `a.bin` and `b.bin` fail.
- No returned byte differs from the original at its offset.
- No range of `a.bin` reads as zeros.
- `c.bin` reads intact.

**Basis:**
- RFC 8 §7.1 and §7.7: "MUST NOT return zeros for any part but a hole or a zero ref."
- RFC 4 §3.4 and RFC 8 §7.2: "MUST return an error rather than unverified bytes".
- A-02 (tampered block fails closed, reproduced over NFS), BS-03.

**Today:** the tampered case passes, per A-02. The deleted case may read zeros: RFC 8's appendix row says "an uncovered extent reads as zeros".

## L6 `73-smb-nfs-crash-restart-m`

**Validates:** after a crash, everything acknowledged as committed survives, with the right bytes and sizes, and uploads resume by themselves.

**Steps:**
1. Run `s3 pause`, so nothing can upload.
2. Write `a.bin` over SMB, and `b.bin` over NFS (`nfs-cp`, which COMMITs).
3. Run `mkdir d` and rename `a.bin` to `d/a.bin`.
4. Run `dfs-server kill`, `s3 resume`, then `dfs-server start`.
5. Check sizes and contents over SMB and NFS. Drain, evict and cold-read.

**Must hold:**
- Both files exist with their written sizes and contents, `d/a.bin` is in place, and the old name is gone.
- The drain completes, and the cold reads match.

**Basis:**
- RFC 0 §10, on a crash: "the engine re-applies from the journal the existence of writes not yet committed, before serving".
- RFC 8 §4.1.
- RFC 7, Appendix A row for §2.5: size "grown from the journal at every share start".
- ST-04.

**Today:** unknown.

## L7 `12-smb-encryption-roundtrip-s`

**Validates:** with encryption on, no plaintext reaches the bucket, reads round-trip across restarts, and a store whose key cannot be unlocked refuses to serve instead of writing unencrypted.

**Steps:**
1. Run `dfs-server stop`, then `dfs-server start DITTOFS_ENCRYPTION_PASSPHRASE=…`.
2. Add block store `s3-enc` (bucket `enc`), with AES-256-GCM and a local key file set in the `--config` JSON. The `--encryption-*` flags are dropped when `--config` is used (#2923). Create share `/enc`.
3. Write a file containing a known marker string many times over, plus random data. Drain.
4. Read every object in bucket `enc` (`rclone cat`) and count the marker.
5. Evict and cold-read. Restart with the same passphrase and cold-read again.
6. Restart with a wrong passphrase, then try a read and a write on `/enc`.

**Must hold:**
- `dfsctl store block list -o json` shows the encryption settings.
- The marker count in the bucket is 0.
- Both cold reads match.
- With the wrong passphrase the store or share refuses to open, and neither the read nor the write succeeds. No new object appears in `enc`, so nothing is written unencrypted.

**Basis:**
- RFC 5: T2 round trip; §2.7, a chain that fails to build "MUST NOT fall back to writing without it"; §5.1.
- BS-10, CP-07, A-10.

**Today:** unknown.

## L8 `10-smb-store-options-xs`

**Validates:** block-store options are kept or refused, and never silently dropped, clamped or replaced by a default. A setting bound to stored content cannot change under it.

**Steps:**
1. Run `store block add --config <json>` with `--compression zstd`, with `--parallel-uploads 4`, and with an `--encryption-*` set. Check `store block list -o json` after each.
2. Try invalid values: `--parallel-uploads -1`, compression `bogus`, an unknown JSON key (`"bukcet"`), and an empty compression value.
3. Write data through a share on store X. Run `store block edit X --bucket other`, then change its prefix through `--config`.

**Must hold:**
- In step 1, each option shows up in the listing, or the add fails and names the conflicting flag.
- In step 2, each add is refused and names the field.
- In step 3, both edits are refused while the store holds content, and the share's files still read cold.

**Basis:**
- A-10 (#2923).
- RFC 13 §6: "MUST NOT clamp it, round it, or fall back to a default"; "Unknown fields are refused."
- RFC 13 §5.1: refuse a change to a bound setting while the store holds content.
- CP-07.

**Today:** fails. Step 1 hits #2923, and RFC 13's appendix rows C3 and C4 list fallbacks.

## L9 `20-smb-nfs-case-across-adapters-xs`

**Validates:** both adapters see one case rule for one share.

**Steps:**
1. Over NFS, create `README` and then `readme`, with different contents.
2. Over SMB: `ls`, `get README`, `get readme`, then `put` a new `Readme`.
3. List over NFS again.

**Must hold:**
- SMB and NFS list the same entries.
- Each name returns the same content over both protocols.
- The SMB create of `Readme` either conflicts or replaces the entry NFS shows for that name. It never adds an entry NFS cannot see, and never adds a third name.
- The header records which rule DittoFS applies. There is no per-share case setting today; `share create` has no flag for it.

**Basis:**
- RFC 7 §3.3: case sensitivity "MUST NOT vary between adapters reaching the same share".
- RFC 7 Appendix A lists "the unique key is byte-exact on a case-insensitive share".
- No matrix row yet; the matrix needs one.

**Today:** fails, per RFC 7 Appendix A.

## L10 `22-smb-share-modes-s`

**Validates:** the SMB sharing-violation rules beyond the share root, and that the share root and a subdirectory behave alike. These are the #2915 follow-ups: comparing root and non-root `OpenFile` fields, and auditing the `ShareAccess` readers.

**Steps:** run each pair of opens (`smb-open`) twice, once on a file in the share root and once on a file in a subdirectory:
- a holder with `READ|WRITE` and share `READ`, then a second open asking `WRITE`, then one asking `READ`;
- a holder asking only `FILE_READ_ATTRIBUTES|SYNCHRONIZE` with share `NONE`, then a `READ|WRITE` open;
- a holder asking `MAXIMUM_ALLOWED` with share `NONE`, then a `READ` open;
- a holder with `DELETE` access and a share without `FILE_SHARE_DELETE`, then a rename or delete through another open;
- a holder on a named stream `f:s` with share `NONE`, then an open of `f` and an open of `f:t`.

**Must hold:**
- Each second open gets the status MS-FSA 2.1.5.1.2 gives for its pair (sharing violation or success). The expected status is written into the scenario per pair.
- The root and the subdirectory give identical statuses for every pair.

**Basis:**
- #2907, PR #2915, and Marco's two follow-ups.
- MS-SMB2 3.3.5.9, MS-FSA 2.1.5.1.2.
- SMB-06 (the matrix's recommendation 10).

**Today:** unknown. This scenario is the reproduction the two held #2915 follow-up issues need.
