# NBD adapter test rigs

Three rigs and one fixture. Each answers a different question; none substitutes for
another.

| Rig | Question it answers | Needs |
| --- | --- | --- |
| `run-oracle.sh` | Does the adapter's **wire codec and handshake** agree with a known-good NBD server? | QEMU. Tier B also a running server. |
| `boot-qemu.sh` | Does a **real kernel client boot** off an NBD export? | QEMU with `kvm` (Linux) or `hvf` (macOS), an aarch64 kernel + initramfs, a provisioned share. |
| `crash-restart.sh` | Does client I/O **pause and resume** across a server SIGKILL inside the dead-connection window? | **Linux + root**, a `master`-built `nbd-client`, init netns. |
| `fixture/` | What does a crash rig kill? | Nothing beyond Go — it is the server those rigs need. |

The oracle can be green while the boot path is broken, and the boot path can
work while the codec disagrees on an edge the kernel never hits. Run all three.

**Neither boot nor crash can run in CI on macOS.** `boot-qemu.sh` runs on macOS
via `hvf`, but `crash-restart.sh` needs the Linux block layer, root, netlink and a
`master`-built `nbd-client`; it refuses cleanly on Darwin rather than half-running.

## `fixture/` — the server a crash rig kills

`crash-restart.sh` needs a server that can be SIGKILLed, and the production `dfs`
server cannot be: it needs a provisioned volume, and creating one needs a share
mount, which needs root. `fixture/` builds the same runtime the integration tests
build — real control plane, real `engine.Store`, real `pkg/adapter/nbd` — from an
ordinary program, and then holds still.

```
GOOS=linux GOARCH=amd64 go build -o /tmp/nbd-fixture ./test/nbd/fixture
/tmp/nbd-fixture -port 10950 -size 2147483648 -journal /tmp/fx-j -meta /tmp/fx-m
# then point the crash rig at it
sudo ./test/nbd/crash-restart.sh --server 127.0.0.1 --port 10950 \
    --export export/boot --device /dev/nbd7 --expect-size 2147483648 \
    --cmd-timeout 5 --dead-timeout 40 --restart-cmd '<restart the fixture>'
```

`--expect-size` is required here: the fixture's metadata lives on disk but there is
no NFS share mount to stat, so the crash rig cannot derive the export size from a
file.

Two properties of the fixture are load-bearing, and both were learned by getting
them wrong:

- **The metadata store is on disk.** An in-memory store loses the file's size and
  chunk manifest when the process dies, so after a restart every read returns
  zeros — which a verifying client reports as `err=84` (EILSEQ), i.e. exactly as a
  corruption bug in the adapter. The first version of this test did that and the
  investigation started in the wrong place.
- **Provisioning is idempotent.** A crash test restarts the server by definition; a
  fixture that re-created its volume on every start would erase the data it is
  meant to have survived. Every step is create-or-look-up.

It also refuses to print `READY` until something actually accepts a connection on
its port. A fixture that announced readiness after `Serve` had already died on
"address already in use" let an older server on the same port answer the clients,
so a run silently measured the wrong process.

## Prerequisites

QEMU, for `qemu-nbd`, `qemu-io` and `qemu-img`:

```
macOS:   brew install qemu
Debian:  apt-get install qemu-utils
Fedora:  dnf install qemu-img
```

`python3` is used by the transmission-flag check. Without it that check skips —
the driver prints a warning and the rest of Tier A still runs.

### `nbd-client` — a `master` build is mandatory for `crash-restart.sh`

**No released `nbd-client` can reconnect.** Do not "fix" the crash rig by adding
`-p` back to your distribution's package: you will get a `BLK_STS_IOERR` after the
full window that looks like a DittoFS bug and is not one.

| Version | `-T`/`--dead-timeout` | persist over netlink |
| --- | --- | --- |
| ≤ 3.26.1 (Debian bookworm `1:3.26.1-6.1`, Ubuntu 24.04 `1:3.26.1-2`) | **absent entirely** | **no — `-p` silently ignored** |
| 3.27.0 / 3.27.1 (Debian trixie/sid `1:3.27.1-4`) | present | **no — same bug** |
| `master` (after `eac55e0b06fe`, 2026-08-30) | present | yes |

- **≤ 3.26.1** has no `-T` at all and never sends `NBD_ATTR_DEAD_CONN_TIMEOUT`.
  Its netlink path does `netlink_configure(...); return 0;`, so it never reaches
  the ioctl reconnect loop and `-p`/`--persist` is parsed, stored, and silently
  ignored whenever netlink is available (the default).
- **3.27.0 / 3.27.1** gate the persist loop on `cur_client->persist_mode`
  (`nbd-client.c:1548`) but the CLI sets `cur_client->persist`
  (`args.c:186-188`), and **nothing in the tree ever assigns `persist_mode =
  true`** (verified across `nbd-client.c`, `args.c`, `args.h`, `nbdclt.h`,
  `nbd.h`, `cliserv.h`). The loop never starts, the kernel never receives
  `NBD_CMD_RECONFIGURE`, the device pauses for `dead_conn_timeout` and then
  returns `BLK_STS_IOERR`. Upstream fixed it on `master`; the fix postdates
  3.27.1.

So the script checks the **capability**, not a version number — a floor of
"3.27.1 or newer" would pass on exactly the two broken releases. `--help` must
mention `--dead-timeout`, and a bare release version string is refused by name.

**Both `-T` and `-p` are required and do different jobs.** The kernel has no
reconnect of its own (`grep -c nbd_reconnect_socket drivers/block/nbd.c` → `0`):

| Setting | What it does | What it does *not* do |
| --- | --- | --- |
| `-T <seconds>` (`--dead-timeout`) | sets the kernel's patience before failing in-flight I/O (`wait_for_reconnect`, `nbd.c:1101-1114`) | reconnect anything |
| `-p` / `--persist` | runs the monitor: `NBD_CMD_LINK_DEAD` → `reconnect_with_backoff` → `netlink_reconfigure` → `NBD_CMD_RECONFIGURE` | set the window (it supplies a 30s default, `nbd-client.c:188`, which `-T` overrides) |

`-T` alone pauses the I/O for the full window and then returns `BLK_STS_IOERR`.
`-T` reaches the kernel **only over netlink**, as `NBD_ATTR_DEAD_CONN_TIMEOUT`
(`nbd-client.c:185-186`); on the ioctl path it is parsed, stored, and never read,
so `-nonetlink`/`-L` is refused.

Build it with `-T` support and netlink, then point the rig at the binary:

```
git clone https://github.com/NetworkBlockDevice/nbd
cd nbd && ./autogen.sh && ./configure && make -j
sudo test/nbd/crash-restart.sh --restart-cmd 'systemctl start dittofs'
```

**The client must be in the init network namespace.** `nbd_mcast_index`
multicasts `NBD_CMD_LINK_DEAD` with `genlmsg_multicast(&nbd_genl_family, ...)`
(`drivers/block/nbd.c:2673`), and `genlmsg_multicast` hardcodes `&init_net`
(`include/net/genetlink.h:530`). In a container netns a netlink listener receives
**zero** messages, so `persist_conn.link_dead` is never set, the loop never
reconnects, and the device behaves exactly as with no `-p` at all. Measured,
changing only the namespace: container netns → zero messages and a read that
failed at the window; `--network=host` → the message arrived, the client logged
`Received link dead notification for device 0` → `Successfully reconnected after
3 attempts`, and the blocked read completed with the pre-crash bytes. Run the
container with `--network=host`; there is no kernel-side workaround.

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

## `fetch-artifacts.sh` — the kernel and initramfs `boot-qemu.sh` needs

```
./test/nbd/fetch-artifacts.sh                  # -> test/nbd/artifacts/
./test/nbd/fetch-artifacts.sh --dest /tmp/nbd  # somewhere else
./test/nbd/fetch-artifacts.sh --force          # re-fetch
```

`boot-qemu.sh` boots a real Linux kernel whose root filesystem is the DittoFS
export. Nothing in this repository builds such a kernel, so before this script
existed the artifacts had to be assembled by hand and kept somewhere like `/tmp`
— where a cleanup silently removed the ability to run the rig at all. This fetches
them from Alpine's mirrors and assembles a tree `boot-qemu.sh` can boot directly
via `--initramfs-dir`.

**It takes the kernel and its modules from the same package** (`linux-virt`),
which is what guarantees their `vermagic` agrees. The obvious shortcut — Alpine's
netboot `initramfs-virt` — does not work: measured, the v3.20 netboot kernel is
6.6.134 while the `linux-virt` package is 6.6.142, so an `insmod` of the
mismatched `nbd.ko` fails on vermagic, and the netboot initramfs carries no
`nbd.ko` at all.

**Modules are extracted at their real nested paths**, not flattened. That matters:
the guest's module lookup is glob-based, and an earlier version of `boot-qemu.sh`
was missing the compressed one-level pattern, so `crc16`, `libcrc32c`,
`crc32c_generic` and `mbcache` were silently not found against a real
distribution tree. Every boot passed anyway, because the hand-built initramfs had
flattened the modules into `/lib/modules/*.ko`. The recipe's layout is what makes
that class of bug visible.

Versions resolve from `APKINDEX` at fetch time rather than being pinned, so a
point release does not turn the recipe into a 404. `--release`, `--mirror` and
`ALPINE_ARCH` override the defaults.

Requires `curl` (or `wget`), `tar` and `gzip`. `cpio` is not needed — the recipe
writes a packed `initramfs.cpio.gz` if it can, but `--initramfs-dir` is the path
that carries a current `/init`, which `boot-qemu.sh` generates itself from the
same source that documents it.

The fetched tree is **not** committed (`.gitignore` covers `test/nbd/artifacts/`);
it is roughly 30MB of kernel and modules.

## `boot-qemu.sh` — a real kernel client boots off the export

```
# One-time: fetch a kernel and initramfs (Alpine linux-virt + busybox).
./test/nbd/fetch-artifacts.sh --dest test/nbd/artifacts

# First boot: format the export and write the readback file.
./test/nbd/boot-qemu.sh --kernel test/nbd/artifacts/Image \
    --initramfs-dir test/nbd/artifacts/initramfs \
    --volume-path /mnt/dittofs/export --export export/boot --mkfs

# Second boot: no mkfs. This is the one that proves durability.
./test/nbd/boot-qemu.sh --kernel test/nbd/artifacts/Image \
    --initramfs-dir test/nbd/artifacts/initramfs \
    --volume-path /mnt/dittofs/export --export export/boot --no-mkfs
```

Exit status is the verdict: **0 PASS, 1 FAIL, 2 INCONCLUSIVE**, derived from the
guest transcript. See "Reading the verdict" below.

Topology, exactly as spec §9.5 — there is no data disk and the root filesystem
arrives over NBD:

```
host:       -kernel Image -initrd initramfs.cpio.gz   <- no disk, no NBD
initramfs:  modprobe nbd
            nbd-client -N export/boot 10.0.2.2 10809 /dev/nbd0 &
            mkfs.ext4 /dev/nbd0                       <- FIRST BOOT ONLY
            mount /dev/nbd0 /root; switch_root
NBD:        the root filesystem
```

### Prerequisites

- `qemu-system-aarch64` with **`kvm` on Linux or `hvf` on macOS**. The script
  *selects* the accelerator and refuses to fall back to TCG: a TCG run boots and
  passes while every timing in it is meaningless, so a green TCG run is worse
  than no run. `-cpu host` requires an explicit `-accel` (without one QEMU
  defaults to TCG and `host` is meaningless → `Invalid CPU model: host`);
  `-cpu max` is the portable substitute via `NBD_QEMU_CPU=max`.
- An aarch64 kernel `Image` and an initramfs containing `nbd-client` **and**
  `nbd.ko`. **`fetch-artifacts.sh` builds both** — see below. To supply your own,
  build the initramfs from a directory with `--initramfs-dir DIR` (needs `cpio` +
  `gzip`), or point at a prebuilt one with `--initramfs`. Either way the script
  verifies both members are present and refuses otherwise, because a boot without
  them never reaches the handshake.
- A provisioned share, a running DittoFS with the NBD adapter enabled, and a
  volume reachable through the share's own NFS/SMB mount.

### Two spellings that are load-bearing

- **`-N <export>` is mandatory.** Omitting it does not mean "no export name" —
  `nbd-client` sends an **empty** one (`nbd-client.c:1504-1505`) and DittoFS
  refuses it (`RuntimeExporter.Resolve` requires a `/` with non-empty share and
  volume) as `nbd: unknown export`. The client reports a handshake failure that
  never mentions the missing flag. The name is `<share>/<volume>`, no leading
  slash.
- **Mount `/dev/nbd0`, not `/dev/nbd0p1`.** The export is a single file served
  whole and nothing writes a partition table; `mkfs.ext4` goes directly on the
  device. A `p1` suffix fails with `no such device` *after* the handshake and
  `mkfs` have both already succeeded.

Also deliberate: **no `-drive`** (the root filesystem arrives over NBD; a data
disk would test the wrong path), **no `hostfwd`** (it forwards host→guest, the
wrong direction, and would try to bind host port 10809 where DittoFS is already
listening — the guest initiates outward to `10.0.2.2`, QEMU's user-net gateway,
which crosses SLIRP with no rule), and **no UEFI-boot-from-NBD** (EDK2 has no NBD
driver and `-M virt` on arm64 has no BIOS to run it with). `-display none
-serial stdio` is required, or the transcript — the only evidence the harness
produces — never reaches the host.

### The volume must exist before the guest boots, owned by the adapter

`RuntimeExporter.Resolve` looks up the share root, then `volumes/`, then the
volume name, and **no command creates a volume** (`grep -rn "volume"
cmd/dfsctl/` is empty). Provisioning is by hand, through the share's mount, and
it is the operator's job in production too. The script checks
`<volume-path>/volumes/<volume>` exists and is non-zero before booting, and
refuses naming the missing volume — because the guest's failure otherwise reads
`nbd: unknown export`, the *same* message an omitted `-N` produces.

- **Create `volumes/` and the volume as the adapter's identity, UID/GID 65534**
  (`pkg/adapter/nbd/config.go:26-27`; `applyDefaults` rewrites a configured
  `UID == 0` back to 65534 at `:125-129`, and there is no `--uid` flag). A volume
  created as any other identity is owned by it at mode `600644`; the write then
  runs as 65534, is not owner and not root, so `calculatePermissions`
  (`pkg/metadata/auth_permissions.go:183`) takes the "other" bits (`r--`) and
  refuses with `AccessDenied: write permission denied`. The `volumes/`
  directory carries the same requirement and fails *earlier* with
  `Permission denied` from the mount.
- **`chmod` through the SMB mount does NOT change the mode.** Measured twice,
  before and after `chmod 666`: `mode_oct=600644` both times, write still
  refused. Do not "fix" this with a `chmod`.
- `dfsctl user create --username <n> --password <p> --uid 65534 --gid 65534`
  names that identity; then `dfsctl share permission grant /export --user <n>
  --level read-write` (the flag is `--level`, not `--permission`). A share with
  default permission `none` refuses the **resolution** itself, also reported as
  `nbd: unknown export`.
- **Set the size AFTER creating the file.** `ApplyCreateDefaults`
  (`pkg/metadata/validation.go:492`) sets `Size = 0` for every non-symlink, so
  `CreateFile` with a size does not produce a file of that size.
- **Two unrelated causes, one symptom.** The same "write never lands" also
  appears as `manifest reconcile for <payload> at offset 0 failed: CAS object
  missing ... chunk not found` when the volume was written by a server whose
  in-memory remote tier has since restarted. The script therefore prints the
export size and adapter identity before starting, and never treats one message
  as diagnostic on its own.

### Reading the verdict

The script prints the accelerator, the export name and size, the share's
`CommitAck` (`DITTOFS_COMMIT_ACK`), and the guest's page-cache state and
`nbd0/queue/write_cache` — so the run is self-describing. In this topology the
guest's kernel NBD client speaks TCP directly to DittoFS, so there is no QEMU
block layer and a `-drive`'s `cache=` mode is irrelevant.

**The exit status is derived from the transcript**, and there are three outcomes:

| Exit | Verdict | Meaning |
| --- | --- | --- |
| 0 | `PASS` | the guest printed `INIT: OK`, and on a `--no-mkfs` boot the readback matched |
| 1 | `FAIL` | the guest printed `INIT-FAIL`, or a `--no-mkfs` boot could not read the file back, or the watchdog fired before any terminal line |
| 2 | `INCONCLUSIVE` | the boot reached neither — it proves nothing either way |

QEMU runs in the background and is stopped as soon as a terminal line appears, so
a success finishes in seconds and a failure is bounded by `NBD_BOOT_TIMEOUT`
(default 180s) rather than hanging. That bound is necessary, not a convenience:
every failure path ends in `exec sh`, and with no init that shell simply waits —
a failing run used to sit there until an operator killed it, and its exit status
said nothing. The full transcript is echoed at the end and its path printed.

**Where the boot ends, and why it no longer panics.** A volume holding a bare
filesystem has no `/sbin/init`, so `switch_root` cannot succeed and the kernel
panics with `Attempted to kill init! exitcode=0x00000100`. That used to be the
end of every run — a legitimate outcome, but indistinguishable in a transcript
from a broken rig, and it made the exit status useless. The generated init now
checks: if `/sbin/init` exists on the volume it switches to it, and otherwise it
prints `INIT: OK …`, syncs, and powers off. So a normal run ends with a clean
`VERDICT: PASS` and no panic. A panic in the transcript now means something is
actually wrong.

**What each boot proves.** The `--mkfs` boot proves the handshake, the
negotiated size, READ, WRITE, FLUSH and the mount are all correct enough to
format and mount a filesystem. The `--no-mkfs` boot is the load-bearing one: a
different VM process, a different kernel instance, no page cache shared with the
first, reading back a filesystem the first boot created. It proves the bytes are
**durable** and that the export size reaches the kernel correctly (ext4 sized
itself from `nbd0`'s sector count). The rig writes `hello.txt` on the first boot
and fails the second boot if it cannot read it back — `--expect-file` and
`--expect-content` override both.

**Do not run `fsck` from inside the guest.** The guest's root *is* the volume
under test, so an `fsck` there judges a crash it is also recovering from. The
check belongs on the host, against the volume read as a file over NFS — the B1
property (spec §2.2) earning its place.

## `crash-restart.sh` — SIGKILL the server and watch I/O pause then resume

**Linux only, root only.** macOS has no `nbd-client` and no Linux block layer to
pause; the script refuses on Darwin naming exactly that.

```
sudo ./test/nbd/crash-restart.sh --restart-cmd 'systemctl start dittofs' \
    --cmd-timeout 5 --dead-timeout 60 --export export/boot
```

### The ordering is the whole test

```
1. connect WITH ALL THREE of -persist -t 5 -T 60, then write a pattern and FLUSH it
2. SIGKILL the server FIRST, before issuing the read
3. THEN issue the read — it must PAUSE, not fail
4. restart the server inside the window
5. the read completes with the pre-crash bytes
```

The flush matters: without it the pattern is only in the guest's page cache, and a
read after the crash returns it from cache, proving nothing. Killing first matters
too: a read issued before the kill is served locally in `elapsed=0s` and the kill
lands on an idle device. **A pass without a nonzero pause AND a completed read
after the restart has tested the page cache, not the adapter** — the script fails
in that case rather than reporting a pass.

### Three timeouts, and why `-t` is not optional

This is the bug that made the first version of this script unable to pass, and it
was found by running it, not by reading it.

| flag | kernel attribute | what it does |
|---|---|---|
| `-t SECS` | `NBD_ATTR_TIMEOUT` | the **block-layer request timeout** (`nbd_set_cmd_timeout`, `nbd.c:1464`) — makes the kernel notice an in-flight request is stuck |
| `-T SECS` | `NBD_ATTR_DEAD_CONN_TIMEOUT` | how long the kernel then waits for a new socket before failing the I/O |
| `-p` | (client-side) | the monitor that actually performs the reconnect |

Without `-t`, `nbd_xmit_timeout` (`nbd.c:413`) takes the wrong branch when there
is a single connection:

```c
if (config->num_connections > 1 ||
    (config->num_connections == 1 && nbd->tag_set.timeout))   // nbd.c:435
        ... nbd_requeue_cmd(cmd); return BLK_EH_DONE;         // :462

if (!nbd->tag_set.timeout)                                   // :469
        "Possible stuck request ..." return BLK_EH_RESET_TIMER; // :477,:491
```

With one connection and no `-t` the second branch wins: the timer is reset
forever, the request is **never requeued**, and it never reaches
`wait_for_reconnect` at all. The read then hangs past the entire window instead of
failing at it — which is exactly what the first measured run did: `-T 20`, no
`-t`, dmesg printed `Possible stuck request ... Runtime 30 seconds`, and the read
was still blocked at t=41s.

So the **worst-case pause is `-t` + `-T`**, and the script computes its windows
from that sum. The default `-t` is 5s, well under `-T`; the script refuses a
`--cmd-timeout` of 0 (which disables the timeout and reintroduces the hang) or one
at or above `--dead-timeout`.

### The `dead_conn_timeout` value

Default **60s**, set explicitly with `-T 60` and printed. It is kept under ~120s
because over that the hung-task detector trips. `-p` alone supplies a 30s default
(`nbd-client.c:188`) — already inside the window, but a default that happens to be
safe is not a configured one, and the script must not depend on `-p` being what
supplied the window. `dead_conn_timeout` has **no sysfs read-back**
(`drivers/block/nbd.c` creates only `pid_attr` and `backend_attr`), so the window
is asserted **behaviourally and in both directions**: the I/O must *block* (that
is `-t` and `-T`) and must *resume* (that is `-p`). Blocking alone passes with a
broken `-p`. `--keep-dead` runs the negative control: no restart, and the read
must *fail* at the window.

### Reading the verdict

Measured on a Linux host against a DittoFS fixture serving a 2 GiB export
(`--cmd-timeout 5 --dead-timeout 40`, killed at t=0, restarted at t=5s):

```
=== issue the read — it must PAUSE, not fail
  read still blocked at t=2s
  read still blocked at t=4s
=== restart the server inside the window (at t=4s, window 40s)
  restart command returned at t=5s

VERDICT: read paused (blocked for >=2s), server restarted at t=5s inside the 45s
window (-t 5s + -T 40s), read completed status=0 elapsed=7s with
PRE-CRASH-PATTERN-AAAA.
Client logged the link-dead notification, so the kernel multicast reached it and
-p reconnected.
```

The matching negative control (`--keep-dead`, `--cmd-timeout 5 --dead-timeout 20`)
failed at t=27s with no server running — that is `-t` making the kernel notice the
stuck request and `-T` bounding the wait.

Independently of the paused client, the bytes were then read back through a
**fresh** client after the restart (`qemu-io -c "read -v 0 22"`) and returned
`PRE-CRASH-PATTERN-AAAA`, so the resumed read had genuinely reached the restarted
server rather than a cache.

The script fails if the pause is absent, if the read does not resume, if the
restart happened outside the window, or if the bytes do not match.

### Verifying more than one byte range, without fooling yourself

`crash-restart.sh` proves one 22-byte pattern survives. To check a whole region after a
crash, use `fio --verify=crc32c`, and avoid three traps that each produce a green run
that means nothing:

- **Verify sequentially, not with random reads.** `--rw=randread --number_ios=600` over
  16 MiB reads 600 of 4 096 blocks, so a corrupted block is very likely never visited.
  This was caught by zeroing a known block and watching the "verify" still return
  `err=0`. `--rw=read` is deterministic.
- **Prove the verifier bites before trusting it.** Seed with `--do_verify=0`, verify
  clean (`err=0`), corrupt one block, verify again (`err=84`, EILSEQ). Without that
  sequence `err=0` is unfalsifiable.
- **Do not count kernel log lines.** `Connection timed out, retrying` is emitted with
  `dev_err_ratelimited`, so identical runs logged 8, 3 and 1 lines for 8 in-flight
  requests. Use `/sys/block/nbdN/inflight` for the queue depth at the kill.

A pipelined crash run measured this way: 8 commands outstanding at the kill, `err=0`,
and a sequential verify of all 4 096 blocks afterwards crc32c-clean.

### The size cross-check (a bug this rig produced)

Pre-flight describes the **file** at `<volume-path>/volumes/<volume>`; it cannot
describe the **server** on the port. Measuring a fixture on port 10950 while
`--volume-path` pointed at a production share printed the production size and then
killed a different server — and both runs looked identical until the numbers were
compared.

So after connecting, the script reads `/sys/block/<dev>/size`, multiplies by 512,
and **refuses** if it does not equal the expected export size. For a server with
no on-disk share mount (an in-memory metadata store) there is no file to stat, so
`--expect-size BYTES` states it explicitly; that flag wins over the file when both
are present.

### Harness details worth knowing

- **`-p` does not daemonize**, so it is backgrounded — both here and in the
  guest initramfs (the persist branch returns before `daemon(0,0)`,
  `nbd-client.c:1571` vs `:1580`; a foreground `-p` never returns and
  `switch_root` is never reached). Backgrounding survives `switch_root`:
  busybox's `delete_contents` unlinks initramfs files and does not kill
  processes.
- **A clean `umount` is not evidence.** The script SIGKILLs the server and
  refuses to pass if the server was not actually killed or was never restarted.
- **A second process cannot substitute** for `-p`: `nbd_genl_connect` returns
  `-EBUSY` (`"nbd%d already in use"`) while the device is configured.
- **The host-side `fsck` is the durability verdict**, and the script prints the
  command to run — against the volume read as a file over NFS, never from inside
  the guest.

## `bench-fio.sh` — measure the export under load

The pipelining work cites this script, so it now exists.

```bash
./bench-fio.sh 127.0.0.1 10809 export/boot                    # fio's nbd engine
./bench-fio.sh --device /dev/nbd0 export/boot                 # an attached device
./bench-fio.sh 127.0.0.1 10809 export/boot --depth "1 2 4 8 16 32 64" --runtime 30
```

It prints one `key<TAB>value` line per measurement — `depth1`, `depth8`, …,
`seqread_kib_s`, `seqwrite_kib_s`, `fsync4k_iops`, `fsync4k_clat_mean_ns` — so
two runs can be compared with `paste`:

```bash
paste /tmp/bench-before.txt /tmp/bench-after.txt
```

### What the depth sweep is for

A server that serves one command at a time reports the **same** IOPS at depth 8,
16, 32 and 64, because the client's queue is only a queue of waits. A server that
can have several commands in flight keeps climbing with depth until it reaches the
block store's own ceiling. That shape — not any single number — is what separates
a serialized dispatcher from a pipelined one.

### Three ways to reach the export, and why the script supports two

`fio` can speak NBD only through its `nbd` engine, which is compiled only when
`libnbd` was present at build time. Most packaged builds omit it — the Homebrew
build on this machine has no `nbd` engine at all — so the script does not assume
it. It accepts an already-attached block device (`--device`, needs `nbd-client`
run by you, because attaching needs root and a device name), or falls back with an
explanation rather than a confusing `engine 'nbd' not loadable`.

The third route, and the one the recorded numbers use, is `qemu-img bench`, which
speaks the NBD protocol directly and needs neither root nor `libnbd`. It reports
elapsed time rather than IOPS:

```bash
qemu-img bench -f raw -c 10000 -s 4096 -S 4096 -t none -d 8 \
  nbd://127.0.0.1:10809/export/boot
```

`-S 4096` matters: `qemu-img bench`'s default step equals the block size, so it
re-reads the same offsets in order and measures the cache instead of the export.

## Results recorded on this host

Honest accounting of what the two new rigs have and have not proven. The
development machine is macOS arm64 (Apple silicon).

| Half | Status on this host | Why |
| --- | --- | --- |
| `boot-qemu.sh` syntax + refusal paths | **verified** | `bash -n` clean; refuses cleanly without kernel/initramfs/volume |
| `boot-qemu.sh` live boot off NBD | **PROVEN on this host, by the script itself** | a real aarch64 kernel booted via `hvf` and the guest's ext4 root was the DittoFS export |
| `boot-qemu.sh` durability verdict | **PROVEN on this host, 2026-10-14** | `--mkfs` writes `hello.txt`, `--no-mkfs` reads it back in a fresh VM: both exit 0 `PASS` in ~3s. The negative control — `--expect-file not-there.txt` — exits 1 `FAIL` in ~6s |
| `fetch-artifacts.sh` | **PROVEN on this host, 2026-10-14** | fetched Alpine `linux-virt` 6.6.142 + busybox from a clean directory, and the resulting tree booted the export end to end with nothing from a hand-built `/tmp` |
| `crash-restart.sh` syntax + refusal paths | **verified** | `bash -n` clean; refuses on Darwin naming the missing Linux block layer, and refuses without a `master` `nbd-client`, root, netlink, init netns, or a provisioned volume |
| `run-oracle.sh` in CI | **wired, 2026-10-21** | `.github/workflows/integration-tests.yml` now installs `qemu-utils` and runs the oracle, because `oracleRequireBinary` skips the only differential check in the repo when `qemu-nbd` is absent. `test/nbd/**` is also in that workflow's `go` path filter, so an edit to this rig triggers it |
| `bench-fio.sh` syntax + refusal paths | **verified** | `bash -n` clean; refuses cleanly with no arguments, with a non-block `--device`, and with `fio` built without the `nbd` engine, naming the three alternatives |
| `bench-fio.sh` JSON parsing | **verified on real fio output** | both extraction bugs it had are recorded in the script: a `note:` line before the JSON, and selecting the read section for a write job (which read as `0.000000`). Confirmed against `fio-3.43` on a local file: `read iops 11778.110945`, `read bw 47112`, `read clat 84496.605567` |
| `bench-fio.sh` against a live NBD export | **not run here** | needs `libnbd` (absent from the Homebrew `fio` build) or a root-attached device. The pipelining numbers below were measured with `qemu-img bench`, which reaches the same server over the same protocol |
| `crash-restart.sh` crash/reconnect verdict | **the mechanism was PROVEN manually, not by this script** | the recipe was run by hand inside a Linux container (`--network=host`, Docker Desktop's kernel has `CONFIG_BLK_DEV_NBD=y`) against a live DittoFS: with `-persist -T 60` a read stayed blocked at t=4s/8s/12s, the server returned at t=16s, and the read completed `exit=0 elapsed=14s` returning the pre-crash bytes with `Successfully reconnected after 4 attempts` in the client log. The **script** has not been run end to end; it needs a Linux host with root and a `master`-built `nbd-client` |
| Host-side `fsck` after the crash | **not run here** | follows the crash run; needs the Linux host |

The crash half's **premise** is therefore established (the kernel pauses and the
client reconnects), but its **scripted form** is unverified here: the
`dead_conn_timeout` value the script prints, its restart-latency assertion and
its `fsck` step all still need a Linux host. Do not read the manual result as a
pass for `crash-restart.sh`.

## A recorded end-to-end boot (real kernel client, real DittoFS)

This is the strongest evidence in the repo for the whole design: it is the only
run where a **real Linux kernel's block layer** drives DittoFS through NBD, and
it is stronger than both the differential oracle (which swaps the block layer for
an in-package fake) and the `qemu-io` transcript (which is userspace).

**Setup.** Host: macOS arm64, QEMU 11.1.2, `-accel hvf`. Guest kernel: Alpine
`linux-virt` 6.6.142 (`CONFIG_BLK_DEV_NBD=m`, `CONFIG_EXT4_FS=m`), taken from the
`linux-virt` apk so the modules match its `vermagic` exactly. Initramfs: static
busybox + `nbd.ko` + `ext4.ko` and its dependency chain
(`crc16`, `libcrc32c`, `crc32c_generic`, `mbcache`, `jbd2`), plus `virtio_net.ko`
with `failover`/`net_failover` and `virtio_mmio.ko` for the NIC. Server: the real
`dfs` process with the NBD adapter on 10809 and a 16384-sector (8 MiB) volume.

**The dependency chain is worth recording**, because every one of these failed
before the next step could run, each with a message that names the *symptom*
rather than the missing module:

| Failure | Cause |
| --- | --- |
| `nbd-client: can't open '/dev/nbd0': No such device or address` | `nbd.ko` not loaded — `modprobe` is useless in this initramfs (no `modules.dep`); `insmod` each module by path |
| `mount: /root: No such file or directory` | `virtio_net.ko` not loaded, so there is no NIC; it needs `net_failover` → `failover` first |
| `EXT4-fs (nbd0): Cannot load crc32c driver` | `crc32c_generic` (+ `libcrc32c`) missing; ext4 mounts read-only or not at all without it |
| `nbd: unknown export` | the volume does not exist, is owned by the wrong identity, or the share grant is `none` — see the four causes above |

**First boot** (`--mkfs`): `mke2fs -t ext4 /dev/nbd0` succeeded **through NBD**,
the filesystem mounted r/w, `hello.txt` was written and `sync`ed, and the guest
unmounted cleanly. The kernel reported:

```
EXT4-fs (nbd0): mounted filesystem a251cff1-ae05-43d3-8558-10b6791ae3bb r/w with ordered data mode.
df: /dev/nbd0  3.5M  152.0K  2.8M  5% /root
```

**Second boot** (fresh VM, **no `mkfs`**): the same filesystem UUID came back and
the file was still there:

```
EXT4-fs (nbd0): mounted filesystem a251cff1-ae05-43d3-8558-10b6791ae3bb r/w with ordered data mode.
ls /root: hello.txt lost+found
READBACK hello.txt: [written-by-the-kernel-client]
```

**Why the second boot is the load-bearing one.** The first boot proves the
handshake, READ, WRITE, FLUSH and the block-size negotiation are all correct
enough for `mkfs` and `mount`. The second proves the bytes are **durable** — a
different VM process, a different kernel instance, no page cache shared with the
first, reading back a filesystem that the first boot created. It also proves the
8 MiB export size is reported correctly to the kernel, since ext4 sized itself
from `nbd0`'s sector count and would have failed on a wrong one.

**This was then re-run through `boot-qemu.sh` itself** (not a hand-rolled QEMU
line), with the artifacts above, against the same live server — the script's own
gate chain passed, its launch line was identical in substance, and the guest
reported the same filesystem UUID and the same `hello.txt` contents. So the rig
is not merely syntactically valid: it works.

**The readback is now produced by the rig, not quoted from that hand-run.** The
generated init originally mounted the export and `switch_root`ed without ever
reading it, so the durability result above could only be quoted from a manual
run — a weaker claim than the same script producing it. The init now writes
`hello.txt` on a `--mkfs` boot, reads it back on a `--no-mkfs` one, and the script
derives a PASS/FAIL verdict from the transcript. Re-run on 2026-10-14 with
artifacts from `fetch-artifacts.sh` alone, both boots pass with the same UUID
`853580a8-d9e6-4789-ace8-94180888d0e2`.

**A defect this run exposed in the script's own gate.** The initramfs check used
`gzip -dc -- "$initramfs" | grep -aq 'nbd-client'` under `set -o pipefail`. That
is a **false negative**: `grep -q` exits at the first match, `gzip` dies of
SIGPIPE, and the pipeline's status becomes **141** even though the string was
found — so the script refused *every* initramfs, valid or not, with a message
telling the operator to rebuild the file they had just built correctly.
Measured on a valid archive:

```
gzip -dc x.cpio.gz | grep -aq nbd-client >/dev/null   -> status 141  (refused)
gzip -dc x.cpio.gz | grep -a  nbd-client >/dev/null   -> status 0    (correct)
gzip -dc x.cpio.gz | grep -a  <absent>   >/dev/null   -> status 1    (correct)
```

Dropping `-q` makes `grep` read to EOF, so no SIGPIPE arises. Note the tempting
alternative — capturing the stream into a variable first — does **not** work: a
cpio archive is full of NUL bytes, and a 3.6 MB listing came back as an empty
string, making both checks report MISSING.

Corroborated server-side without going through NBD at all: reading offset 1024
over a **fresh** `qemu-io` connection shows the ext4 superblock with magic
`53 ef` at offset 1080 (`9f 38 cd 6a` UUID, matching the `a251cff1` prefix the
kernel printed).

**What this run does NOT cover.** The crash/reconnect half (`crash-restart.sh`)
is Linux-only and was not run here; its recipe is recorded above and must be
re-run on a Linux host with a `master`-built `nbd-client`. The boot above uses
the *release* kernel's NBD driver, not `dead_conn_timeout`.

## Interpreting a divergence

A divergence is a finding, not a failure to paper over. Check it against the NBD
spec before changing the adapter to match — and check the oracle's own
configuration first (`-x`, `--discard=unmap`, and that the scratch file is
exactly the export's size). A size mismatch makes the two servers bounds-check
differently — DittoFS answers `NBD_ENOSPC` to a WRITE past the end and
`NBD_EINVAL` to a READ or TRIM past it — and surfaces as a transcript divergence
that names neither cause.
