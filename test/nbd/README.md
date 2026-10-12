# NBD adapter test rigs

Two rigs, two different questions. Neither substitutes for the other.

| Rig | Question it answers | Needs |
| --- | --- | --- |
| `run-oracle.sh` | Does the adapter's **wire codec and handshake** agree with a known-good NBD server? | QEMU. Tier B also a running server. |
| KVM harness (Task 8, `test/nbd/`) | Does a **real kernel client boot** off an NBD export? | Root, Linux, KVM, a provisioned share. |

The oracle can be green while the boot path is broken, and the boot path can
work while the codec disagrees on an edge the kernel never hits. Run both.

## Prerequisites

QEMU, for `qemu-nbd`, `qemu-io` and `qemu-img`:

```
macOS:   brew install qemu
Debian:  apt-get install qemu-utils
Fedora:  dnf install qemu-img
```

`python3` is used by the transmission-flag check. Without it that check skips —
the driver prints a warning and the rest of Tier A still runs.

## `run-oracle.sh`

```
./test/nbd/run-oracle.sh                    # Tier A
DITTOFS_NBD_PORT=12050 ./test/nbd/run-oracle.sh   # Tier A + Tier B
```

### Tier A — the differential oracle (QEMU only)

`internal/adapter/nbd/oracle_test.go`. No root, no VM, no share provisioning.

The same `qemu-io` command sequence is run against two servers — this adapter's
NBD handler over a real loopback TCP listener, and `qemu-nbd` serving a raw
scratch file of exactly the export's size — and the two transcripts must match
line for line. Timings and the URI (which embeds a different port on each side)
are scrubbed before the comparison; each command's **exit status** is part of the
transcript, because `qemu-io` prints `read 1048576/1048576 bytes` even when the
pattern check failed and it exits 1.

The sequence covers WRITE, READ, DISCARD, WRITE_ZEROES (via `write -z`), a read
of a never-written range, and FLUSH — so it exercises command framing, reply
framing, offset and length handling, and the export-name fold.

A second test asserts that every transmission flag this adapter advertises is
also advertised by `qemu-nbd`. The assertion is one-directional: `qemu-nbd`
advertises `SEND_CACHE` and `SEND_FAST_ZERO`, which this adapter deliberately
does not, so equality would be wrong. What must hold is *ours ⊆ theirs*, because
the flags we do advertise are the ones a client acts on without verifying.

**What Tier A does not cover.** The block layer is an in-package fake, not a real
`engine.Store`. So this compares the codec and the handshake and nothing else: it
does not cover `PunchHole`, the journal, the CAS, or durability. Do not read a
green Tier A as evidence about the storage path.

Two `qemu-nbd` flags are load-bearing and easy to omit:

- `-x export/boot` — measured, `qemu-nbd` exposes an **empty** export name by
  default, so the URI that works against DittoFS is refused on every command
  without it.
- `--discard=unmap` — without it a TRIM is dropped and `qemu-nbd` answers
  success while the data survives. DittoFS's `PunchHole` genuinely zeroes, so the
  discard step would then diverge on the **oracle's** configuration and be
  reported as a codec bug. That is the most expensive kind of false positive,
  because the natural fix is to make DittoFS stop zeroing.

### Tier B — live server (skipped without `DITTOFS_NBD_PORT`)

Needs a running `dfs` with an NBD adapter and a provisioned volume. Unset, the
driver says so and exits 0 having run Tier A only — the tier that ran is always
printed, so a partial run is never mistaken for a full one.

Tier B is currently a handshake smoke test: it confirms the port accepts a
connection and returns a fixed-newstyle greeting. It does not yet replay the
Tier A transcript against a live server.

### A skip is a failure

`go test` skips the oracle when QEMU is missing. A green run that quietly skipped
the only differential check is worse than no run, so the driver greps its own
output for `--- SKIP` and exits non-zero. The absence of a prerequisite is
reported as a failure here and as a skip in `go test ./...`, which keeps the unit
suite green on a machine without QEMU without letting this rig pass vacuously.

## Interpreting a divergence

A divergence is a finding, not a failure to paper over. Check it against the NBD
spec before changing the adapter to match — and check the oracle's own
configuration first (`-x`, `--discard=unmap`, and that the scratch file is
exactly the export's size). A size mismatch makes the two servers bounds-check
differently — DittoFS answers `NBD_ENOSPC` to a WRITE past the end and
`NBD_EINVAL` to a READ or TRIM past it — and surfaces as a transcript divergence
that names neither cause.
