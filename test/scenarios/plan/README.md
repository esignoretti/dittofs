# Planned scenarios (2026-10-01)

These are 20 new scenarios, one bash file each next to `setup.sh`, split ten and ten:

- **Lucian:** [lucian.md](lucian.md) covers remote failures, capacity, crash, encryption, config, case and share modes.
- **Radu:** [radu.md](radu.md) covers GC, dedup, truncate, unlink, snapshots, concurrency and permissions.

Each entry says what the scenario must validate, how to drive it, what must hold, and what it rests on. It also predicts whether it fails today.

## Sources

- **Storage RFCs:** upstream branch `docs/storage-rfcs`, read at `9be288de`. They are the source of truth for the expected behaviour.
  - RFC 0 §9 and §10 give the invariants and the failure model.
  - Each RFC's conformance section gives the checks.
  - Each RFC's "Appendix A: where the current code differs" says where today's code is expected to fail.
- **IDs:**
  - Findings (`A-02`, `F-05` and so on) are IDs from an external review of DittoFS. Where one has an upstream issue, the issue is cited with it.
  - Matrix rows (`RO-03`, `BS-14` and so on) are rows of [matrix.md](matrix.md).

## Conventions

These are the same as in [../README.md](../README.md):
- one command per line, no functions;
- print the outcomes, then check them;
- the first failing command fails the scenario.

The header comment names the issue, RFC section or finding, and says "Fails today: …" once that is known. Scenarios longer than 15 minutes say in the header which `SCENARIO_TIMEOUT` they need.

## Shared tools (done)

They are in `../lib/tools`, and each file says how to use it. `setup.sh` installs them in `/usr/local/bin` inside the container. Every tool waits until it is done, so a scenario never has to sleep for one.

| Tool | What it does |
|---|---|
| `s3 start`, `s3 stop` | Start SeaweedFS and wait until it answers, or kill it (SIGKILL) like an outage, so requests are refused. Its data survives a stop. |
| `s3 pause`, `s3 resume` | SIGSTOP or SIGCONT on SeaweedFS. Connections stay open and requests hang, which is a stall rather than an outage. |
| `dfs-server start [VAR=value ...]` | Start DittoFS with setup's flags plus the given environment (for example `DITTOFS_ENCRYPTION_PASSPHRASE=…`), and wait until `dfsctl` is logged in. Resume a paused S3 first: startup checks the bucket. |
| `dfs-server stop`, `dfs-server kill` | A clean shutdown (SIGTERM), or a crash (SIGKILL). Both wait until the server is gone. |
| `nfs-truncate URL SIZE`, `smb-truncate URL SIZE` | Set a file's size over NFSv4 (libnfs), or with SMB SET_INFO (libsmb2), as `smb://127.0.0.1/test/f`. Taken from Radu's truncate scenarios. |
| `nfs-hold URL` | Open a file over NFSv4 and keep it open. Each line on stdin reads the whole file to stdout; EOF closes it. |
| `smb-open URL ACCESS SHARE` | Open with a given desired access and share access (libsmb2), print the NT status, and keep the handle open until stdin closes. |

## Next batch (not today)

- **`smb-s3-puts-refused`:** SeaweedFS restarted with a read-only identity. Uploads fail and retry, cold reads still work, and the drain completes after write access returns (RFC 3 §2.8).
- **`smb-snapshot-delete-middle`:** three snapshots with overwrites between them. Delete the middle one, GC, then restore the outer two (RFC 12 S9).
- **Recycle-bin retention, size cap and exclude patterns** (TR-05).
- **Repeated content freed after the dedup hold** (BS-08, G-02). It takes about 75 minutes, so it is nightly only.
- **lz4 beside zstd** in `13-smb-compression-frame-magic-xs` (BS-09).
- **An overwrite never rewrites an existing block object** (BS-06).

## Index

| # | Scenario | Owner | Matrix rows | Status |
|---|---|---|---|---|
| L1 | `70-smb-s3-down-reads-fail-m` | Lucian | RO-03 | done, passes (`68263a82`) |
| L2 | `71-smb-s3-stalled-deadline-m` | Lucian | RO-03 | done, fails: cold reads end at 60 s (`a0a29ec0`) |
| L3 | `61-smb-journal-full-s3-down-m` | Lucian | BS-14, RO-03 | done, fails: IO_TIMEOUT, not DISK_FULL (`7562f2f6`) |
| L4 | `60-smb-nfs-journal-small-stream-m` | Lucian | BS-14 | done, passes (`323a9661`) |
| L5 | `62-smb-remote-block-damaged-xs` | Lucian | BS-03 | done, passes (`d274cb6f`) |
| L6 | `73-smb-nfs-crash-restart-m` | Lucian | ST-04 | done, passes (`c7fadc90`) |
| L7 | `12-smb-encryption-roundtrip-s` | Lucian | BS-10, CP-07 | done, passes (`8ae34042`) |
| L8 | `10-smb-store-options-xs` | Lucian | CP-07 | done, fails: flags dropped, unknown key ignored, bound edits accepted (`09263815`) |
| L9 | `20-smb-nfs-case-across-adapters-xs` | Lucian | new row | done, fails: NFS case-sensitive, SMB not (`48945b07`) |
| L10 | `22-smb-share-modes-s` | Lucian | SMB-06 | done, fails: share root lets an add-file open in (`7089f6b1`) |
| R1 | `41-smb-dedup-delete-original-gc-xs` | Radu | BS-07, BS-08 | todo |
| R2 | `50-smb-2shares-one-store-gc-xs` | Radu | MS-02 | todo |
| R3 | `11-smb-2stores-same-bucket-xs` | Radu | new row | todo |
| R4 | `24-smb-nfs-truncate-during-upload-xs` | Radu | FO-06 | todo |
| R5 | `43-smb-nfs-open-unlinked-gc-xs` | Radu | new row | todo |
| R6 | `44-smb-unlink-crash-gc-xs` | Radu | BS-05 | todo |
| R7 | `45-smb-snapshot-survives-gc-xs` | Radu | SN-03 | todo |
| R8 | `72-smb-snapshot-s3-down-s` | Radu | SN-03 | todo |
| R9 | `21-smb-nfs-concurrent-creates-xs` | Radu | RO-02 | todo |
| R10 | `30-smb-nfs-permission-revoke-xs` | Radu | PE-01 | todo |
