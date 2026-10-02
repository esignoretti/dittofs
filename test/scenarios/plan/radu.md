# Radu's ten

The shared tools are listed in [README.md](README.md). "Today" is a prediction from a finding or an RFC appendix row, not a result. Use random content wherever dedup is not the point, so the dedup hold (G-02) cannot delay a reclaim.

## R1 `41-smb-dedup-delete-original-gc-xs`

**Validates:** deleting a file whose chunks other files share never deletes those chunks, including after a restart.

**Steps:**
1. Write `a.bin` (32 MiB, random) and drain.
2. Write `b.bin`, a copy of `a.bin` (full overlap), and drain.
3. Write `c.bin`: the first 16 MiB of `a.bin` plus 16 MiB of new random data (partial overlap). Drain.
4. Print the bucket bytes.
5. Delete `a.bin`, run GC, evict, and cold-read `b.bin` and `c.bin`.
6. Repeat steps 1-5 as a second variant, with a restart (`dfs-server stop`, then `dfs-server start`) between the delete and the GC.

**Must hold:**
- The bucket holds about 48 MiB for the three files: dedup worked, as in `40-smb-dedup-same-content-xs`.
- After each GC, `b.bin` and `c.bin` read cold and match by sha256.

**Basis:**
- RFC 0 §8.3 (I3): "A block MUST NOT be deleted while any chunk it contains is referenced."
- RFC 0 §3.1.
- G-02, BS-07.

**Today:** unknown for the plain run. The restart variant is the risk: the code's adoption guard is in memory and lasts one hour.

## R2 `50-smb-2shares-one-store-gc-xs`

**Validates:** two shares on one block store and one metadata store. GC for one share frees only what no share references.

**Steps:**
1. Create shares `/a` and `/b`, both on block store `s3` and metadata store `md`.
2. Write a distinct random file in each, plus one identical file in both. Drain.
3. Delete `/a`'s distinct file, run `gc /a`, and evict.
4. Delete `/a`'s copy of the shared file, run `gc /a`, and evict.

**Must hold:**
- After each GC, all of `/b`'s files read cold and match.
- The first GC shrinks the bucket by about the size of `/a`'s distinct file.
- The second GC leaves the bucket unchanged, because `/b` still references the shared chunks.
- The GC numbers (objects swept, bytes freed) agree with the change in the bucket.

**Basis:**
- RFC 6 §2.6: counts are kept per namespace, and shares on one store share a namespace.
- RFC 9 §3.6.
- MS-02, the same-store counterpart of A-08.

**Today:** unknown.

## R3 `11-smb-2stores-same-bucket-xs`

**Validates:** two block stores on the same bucket and prefix are refused, or else they can never delete each other's objects.

**Steps:**
1. Add `s3-x` and `s3-y`, both on bucket `dittofs` with prefix `shared/`.
2. If both are accepted:
   1. create shares `/x` and `/y`, write a file to each, and drain;
   2. delete `/x`'s file, then run `gc /x` and `store block reclaim`;
   3. evict and cold-read `/y`'s file.

**Must hold:**
- The second add is refused.
- If it is not refused, `/y`'s objects are still in the bucket after GC and reclaim, and `/y`'s file reads cold and matches.

**Basis:**
- RFC 6 §2.6: "Two partitions that can name one key, each keeping its own count, are forbidden."
- RFC 9 §5.3: GC "MUST NOT delete an object found only by listing unless" the namespace is proven.
- RFC 6 Appendix A: "shares on one remote config share an unnamespaced key space"; unrecorded objects "are deleted by age".
- No matrix row yet; the matrix needs one.

**Today:** fails. The second add is expected to be accepted, and reclaim may delete the other store's objects.

## R4 `24-smb-nfs-truncate-during-upload-xs`

**Validates:** truncating a file while its upload is in flight never brings the truncated bytes back.

**Steps:**
1. Run `s3 pause`.
2. Write `f` (10 MiB, random) over SMB. Its upload is now stalled.
3. Run `nfs-truncate f 5242880`, then `s3 resume`, then drain.
4. Run `nfs-truncate f 10485760` (grow it back), then evict.
5. Read `f` over SMB and over NFS.

**Must hold:**
- Bytes 0 to 5 MiB match the original.
- Bytes 5 to 10 MiB are all zeros.
- The same holds after another evict and cold read.

**Basis:**
- RFC 8 §8.3: "a later truncate up reads zeros where the user was promised zeros".
- RFC 6 §6.2: "A transfer survives a removal under it": the commit drops refs the removal covers.
- FO-06.

**Today:** fails, per RFC 8's appendix: "no removal is checked at commit".

## R5 `43-smb-nfs-open-unlinked-gc-xs`

**Validates:** a file that is deleted while still open is not released until it is closed.

**Steps:**
1. Write `f` (32 MiB, random) over SMB, drain, and evict.
2. Start `nfs-hold` on `f`, so it holds `f` open over NFSv4.
3. Delete `f` over SMB, run GC, and evict.
4. Send a line to `nfs-hold`, so it reads `f`.
5. Close it (EOF), then run GC again.

**Must hold:**
- The read through the open handle matches the original.
- After the close and the second GC, the bucket returns to its baseline.

**Basis:**
- RFC 7 §4.2: "A file with Nlink zero that is still open MUST NOT be released."
- RFC 7 Appendix A, row for §4.4: today a hold list read by GC protects such files.
- No matrix row yet; the matrix needs one.

**Today:** unknown.

## R6 `44-smb-unlink-crash-gc-xs`

**Validates:** a crash right after deletes leaks nothing.

**Steps:**
1. Print the bucket's object count and bytes as the baseline.
2. Write three files (random), drain, and delete them over SMB.
3. Run `dfs-server kill` at once, then `dfs-server start`.
4. Run GC, then `store block audit-refcounts /test`.

**Must hold:**
- The bucket's object count and bytes return to the baseline.
- `audit-refcounts` agrees with the bucket: no leaked and no missing blocks.

**Basis:**
- RFC 7 §4.3: "A crash anywhere between the unlink and that transaction leaves the record".
- RFC 7 Appendix A: "no pending-release record".
- C-11: `audit-refcounts` reports clean while blocks leak.
- BS-05.

**Today:** fails, per RFC 7's appendix.

## R7 `45-smb-snapshot-survives-gc-xs`

**Validates:** a snapshot keeps every block it names through deletes and GC, and restores them.

**Steps:**
1. Write `a.bin` and `b.bin` (random) and drain.
2. Run `share snapshot create /test --name s1`, and wait until it is ready.
3. Delete both files and run GC.
4. Run `share snapshot restore /test <id> --yes`, evict, and cold-read.
5. Last step: `share remove /test` while the snapshot exists.

**Must hold:**
- After the GC, the bucket still holds at least the bytes of `a.bin` plus `b.bin`.
- After the restore, both files read cold and match.
- `share remove` is refused while the snapshot exists. It runs last, because if it succeeds the share is gone.

**Basis:**
- RFC 12 §8.3 (S1): "Snapshot a share, delete every file, run sweep and collection to completion. Every block the snapshot names survives".
- RFC 12 §2.8: deleting a share with snapshots must be refused.
- SN-03.

**Today:** unknown.

## R8 `72-smb-snapshot-s3-down-s`

**Validates:** a snapshot taken while the remote is down still restores the version it saw.

**Steps:**
1. Write `f` as v1 and drain.
2. Run `s3 stop`. Overwrite `f` with v2, which lives only in the journal.
3. Run `share snapshot create /test`, recording what it does: wait, fail, or succeed. Also try `--no-verify` as a variant.
4. Overwrite `f` with v3.
5. Run `s3 start`, drain, GC, and restore the snapshot. Then evict and cold-read.

**Must hold:**
- After the restore, `f` is v2.

**Basis:**
- RFC 12 §2.4: "the journal keeps every version at or below the cut's hold mark until an offload has committed it".
- RFC 12 scenarios `S-snap-remote-down` and `S-snap-hold-overwrite`.
- SN-03.

**Today:** unknown. By default, `snapshot create` verifies remote durability, so with S3 down it should wait or fail. RFC 12's appendix row D3 says the dirty content at the cut is not held, and that a snapshot "may be created without the durability check" (`--no-verify`). With that flag, the restore is expected to bring back the wrong version.

## R9 `21-smb-nfs-concurrent-creates-xs`

**Validates:** concurrent creates and attribute changes in one directory never surface as I/O errors and never lose or duplicate entries.

**Steps:**
1. Start 32 `smbclient` and 32 `nfs-cp` processes in parallel, each creating 20 files in `/test/d`.
2. At the same time, run a chmod loop over NFS through a small libnfs helper.
3. Collect every exit status and error string.

**Must hold:**
- No call fails. No `NT_STATUS_IO_DEVICE_ERROR` or `NFS4ERR_IO` appears.
- The final listing has exactly 1,280 entries, each once, and every content matches.

**Basis:**
- RFC 0 §9.2 (I8): conflicts "MUST NOT reach the caller as an I/O error".
- RFC 7 Appendix A: "retried under a fixed budget, after which a conflict reaches the client as an I/O error".
- RO-02.

**Today:** fails, per RFC 7's appendix.

## R10 `30-smb-nfs-permission-revoke-xs`

**Validates:** permission is checked on every call, not only at connect time.

**Steps:**
1. Hold an SMB session open: `smbclient` reading its commands from a FIFO.
2. Hold an NFS connection open: `nfs-hold`, or a small libnfs helper.
3. Run `share permission revoke /test --user tester`.
4. Issue one read and one write on each open connection, then try new connections.
5. Grant `read`, then retry the read and the write on both protocols.

**Must hold:**
- After the revoke, every call is refused (`NT_STATUS_ACCESS_DENIED`, `NFS4ERR_ACCESS`), on the open connections and on new ones.
- With the `read` grant, reads succeed and writes are refused on both protocols.

**Basis:**
- RFC 7 §7.4: "Authorize MUST evaluate it on every call, not only at mount or tree connect".
- PE-01.
- C-06: the e2e test ENF-02 can pass for the wrong reason.

**Today:** unknown.
