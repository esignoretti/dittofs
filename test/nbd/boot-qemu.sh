#!/usr/bin/env bash
#
# boot-qemu.sh — boot a real kernel client off a DittoFS NBD export.
#
# Topology (spec §9.5), exactly:
#
#   host:       -kernel Image -initrd initramfs.cpio.gz    <- no disk, no NBD
#   initramfs:  modprobe nbd
#               nbd-client -N export/boot 10.0.2.2 10809 /dev/nbd0 &
#               mkfs.ext4 /dev/nbd0                        <- FIRST BOOT ONLY
#               mount /dev/nbd0 /root; switch_root
#   NBD (DittoFS): the root filesystem
#
# The `&` is load-bearing: with -p the client never returns (the persist branch
# returns before daemon(0,0), nbd-client.c:1571 vs :1580), so a foreground
# nbd-client would hang before switch_root and the boot would never reach the
# volume.
#
# Two spellings in that line are load-bearing and both fail without naming the
# cause:
#
#   -N <export>   mandatory. Omitting it does not mean "no export name":
#                 nbd-client sends an EMPTY one (nbd-client.c:1504-1505) and
#                 DittoFS refuses it (RuntimeExporter.Resolve requires a '/'
#                 with non-empty share and volume) as "nbd: unknown export".
#                 The name is <share>/<volume>, no leading slash.
#   /dev/nbd0     not /dev/nbd0p1. The export is a single file served whole and
#                 nothing here writes a partition table; mkfs.ext4 goes directly
#                 on the device, so a p1 suffix fails with "no such device"
#                 after the handshake and mkfs have both already succeeded.
#
# No -drive: the root filesystem arrives over NBD; a data disk would test the
# wrong path. No hostfwd: it forwards host->guest, the wrong direction, and
# would try to bind host port 10809 where DittoFS is already listening. The
# guest initiates the connection outward to 10.0.2.2, QEMU's user-net gateway
# to the host, and a guest-initiated outbound connection crosses SLIRP with no
# rule. No UEFI-boot-from-NBD: EDK2 has no NBD driver and -M virt on arm64 has
# no BIOS to run EDK2 with.
#
# The recovery environment must NOT be on NBD: this script never runs fsck from
# inside the guest, because the guest's root is the volume under test. The fsck
# belongs on the host, against the volume read as a file over NFS.

set -euo pipefail

usage() {
	cat <<'EOF'
boot-qemu.sh — boot a real kernel client off a DittoFS NBD export.

Usage:
  test/nbd/boot-qemu.sh [options]

Options:
  --kernel PATH        Kernel image handed to -kernel (default $NBD_KERNEL,
                       else test/nbd/artifacts/Image)
  --initramfs PATH     Initramfs handed to -initrd (default $NBD_INITRAMFS,
                       else test/nbd/artifacts/initramfs.cpio.gz)
  --initramfs-dir DIR  Build the initramfs from DIR instead of using a prebuilt
                       one (default $NBD_INITRAMFS_DIR). DIR must contain
                       bin/nbd-client and a nbd.ko under lib/modules/.
  --mkfs               mkfs.ext4 /dev/nbd0 in the guest before mounting (the
                       first boot of a fresh volume). Default: on, unless
                       --no-mkfs.
  --no-mkfs            Do not mkfs; the export already holds a filesystem.
  --export NAME        Export name, <share>/<volume> (default $DITTOFS_NBD_EXPORT,
                       else export/boot). -N is mandatory; see the header.
  --port PORT          DittoFS NBD port (default $DITTOFS_NBD_PORT, else 10809).
  --host ADDR          Guest-visible server address (default 10.0.2.2, QEMU's
                       user-net gateway to the host).
  --volume-path DIR    Host path where the share is mounted over NFS/SMB
                       (default $DITTOFS_VOLUME_PATH, else /mnt/dittofs/export).
                       The script checks DIR/volumes/<volume> exists and prints
                       its size before booting.
  --memory SIZE        Guest RAM (default $NBD_QEMU_MEM, else 2G).
  --dry-run            Print the QEMU command and exit without booting.
  -h, --help           This message.

Environment:
  NBD_QEMU             qemu-system-* binary (default qemu-system-aarch64).
  NBD_QEMU_CPU         -cpu value (default "host"; "max" is the portable
                       substitute). -cpu host REQUIRES an explicit -accel:
                       without one QEMU defaults to TCG and host is meaningless.
  DITTOFS_COMMIT_ACK   Value to print for the share's CommitAck (default
                       "unknown"). Printed so the run is self-describing.

Exit status is the verdict. The script refuses loudly when a prerequisite is
absent rather than falling back: a TCG run boots and passes while every timing
in it is meaningless, so TCG is never used.
EOF
}

die() {
	printf 'boot-qemu.sh: REFUSING: %s\n' "$*" >&2
	exit 1
}

# Resolve the script's own directory so the script runs from any cwd.
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)

kernel=${NBD_KERNEL:-$script_dir/artifacts/Image}
initramfs=${NBD_INITRAMFS:-$script_dir/artifacts/initramfs.cpio.gz}
initramfs_dir=${NBD_INITRAMFS_DIR:-}
export_name=${DITTOFS_NBD_EXPORT:-export/boot}
port=${DITTOFS_NBD_PORT:-10809}
server_host=${NBD_GUEST_SERVER:-10.0.2.2}
volume_path=${DITTOFS_VOLUME_PATH:-/mnt/dittofs/export}
memory=${NBD_QEMU_MEM:-2G}
qemu=${NBD_QEMU:-qemu-system-aarch64}
qemu_cpu=${NBD_QEMU_CPU:-host}
commit_ack=${DITTOFS_COMMIT_ACK:-unknown}
do_mkfs=1
dry_run=0

while [ $# -gt 0 ]; do
	case $1 in
	--kernel)
		[ $# -ge 2 ] || die "--kernel needs a path"
		kernel=$2
		shift 2
		;;
	--initramfs)
		[ $# -ge 2 ] || die "--initramfs needs a path"
		initramfs=$2
		shift 2
		;;
	--initramfs-dir)
		[ $# -ge 2 ] || die "--initramfs-dir needs a directory"
		initramfs_dir=$2
		shift 2
		;;
	--mkfs)
		do_mkfs=1
		shift
		;;
	--no-mkfs)
		do_mkfs=0
		shift
		;;
	--export)
		[ $# -ge 2 ] || die "--export needs a name"
		export_name=$2
		shift 2
		;;
	--port)
		[ $# -ge 2 ] || die "--port needs a value"
		port=$2
		shift 2
		;;
	--host)
		[ $# -ge 2 ] || die "--host needs an address"
		server_host=$2
		shift 2
		;;
	--volume-path)
		[ $# -ge 2 ] || die "--volume-path needs a directory"
		volume_path=$2
		shift 2
		;;
	--memory)
		[ $# -ge 2 ] || die "--memory needs a size"
		memory=$2
		shift 2
		;;
	--dry-run)
		dry_run=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		die "unknown argument: $1 (try --help)"
		;;
	esac
done

# ---------------------------------------------------------------- prerequisites

# The accelerator must be explicit and must exist. A silent TCG fallback is much
# worse than a refusal: TCG is slow enough to make the dead-connection window
# measurements meaningless while still passing. Measured: -accel kvm,fallback=allow
# on a machine without KVM prints "invalid accelerator kvm" — it does not
# silently degrade — so passing an explicit -accel is safe.
case $(uname -s) in
Linux) accel=kvm ;;
Darwin) accel=hvf ;;
*) die "unsupported host platform '$(uname -s)': need Linux (kvm) or macOS (hvf)" ;;
esac

command -v "$qemu" >/dev/null 2>&1 ||
	die "QEMU binary '$qemu' not found on PATH. Install QEMU (macOS: brew install qemu; Debian: apt-get install qemu-system-arm) or set NBD_QEMU."

if ! "$qemu" -accel help 2>/dev/null | grep -qx -- "$accel"; then
	die "'$qemu' does not offer the '$accel' accelerator (available: $("$qemu" -accel help 2>/dev/null | tr '\n' ' ')). This script refuses to fall back to TCG, because a TCG run boots and passes while every timing in it is meaningless. Install/enable KVM (Linux: /dev/kvm) or run on a host that supports hvf (macOS on Apple silicon)."
fi

[ -r "$kernel" ] ||
	die "kernel image '$kernel' not found. Pass --kernel PATH, set NBD_KERNEL, or place an aarch64 Image at test/nbd/artifacts/Image. Nothing in this repo builds one."

# The export name is <share>/<volume> with no leading slash. Validate the shape
# here, because DittoFS refuses a malformed name as "nbd: unknown export" — the
# same message a missing volume produces.
case $export_name in
*/*) ;;
*) die "export name '$export_name' has no '/'. It must be <share>/<volume>, e.g. export/boot. DittoFS's Resolve requires a non-empty share and a non-empty volume." ;;
esac
case $export_name in
/*) die "export name '$export_name' has a leading slash; use <share>/<volume> with no leading slash, e.g. export/boot" ;;
esac
volume_name=${export_name#*/}
share_name=${export_name%%/*}
[ -n "$share_name" ] && [ -n "$volume_name" ] ||
	die "export name '$export_name' must have a non-empty share and volume (e.g. export/boot)"

# The volume must exist BEFORE the guest boots. Nothing in the repo creates a
# volume (grep -rn "volume" cmd/dfsctl/ is empty): the operator provisions it
# through the share's own NFS/SMB mount, and its size IS the export size. A
# missing volume otherwise surfaces at the handshake as "nbd: unknown export",
# which reads as an export-name problem and is not one.
[ -d "$volume_path" ] ||
	die "share mount '$volume_path' is not a directory. Mount the share over NFS/SMB there (or pass --volume-path DIR / set DITTOFS_VOLUME_PATH). This script checks volumes/ through the share, because no dfsctl command creates a volume."
[ -e "$volume_path/volumes" ] ||
	die "'$volume_path/volumes/' does not exist. Provision the share's volumes directory through the share mount first; the NBD adapter resolves <share>/<volume> through it. Note it must be created by the adapter's identity (UID/GID 65534, pkg/adapter/nbd/config.go:26-27) or the adapter's own file creation fails with 'Permission denied'."
[ -e "$volume_path/volumes/$volume_name" ] ||
	die "volume '$volume_name' not found at '$volume_path/volumes/$volume_name'. Create it through the share as the adapter's identity (UID/GID 65534 — e.g. dfsctl user create --uid 65534 --gid 65534, then a read-write share grant) and set its size AFTER creating it: ApplyCreateDefaults (pkg/metadata/validation.go:492) sets Size = 0 for every non-symlink, so CreateFile with a size yields a zero-length export. Without this the guest fails the handshake with 'nbd: unknown export', which names neither the volume nor the identity."

volume_bytes=$(wc -c <"$volume_path/volumes/$volume_name" | tr -d ' ')
[ "$volume_bytes" -gt 0 ] 2>/dev/null ||
	die "volume '$volume_name' at '$volume_path/volumes/$volume_name' is $volume_bytes bytes. CreateFile with FileAttr{Size: N} does NOT produce a file of size N (ApplyCreateDefaults, pkg/metadata/validation.go:492), so the size must be set afterwards. A zero-length export fails the handshake."

# ---------------------------------------------------------------- self-describing run

cat <<EOF
boot-qemu.sh: run parameters (printed so the transcript is self-describing)
  host platform    : $(uname -s) $(uname -m)
  accelerator      : $accel (explicit; TCG is never used)
  cpu              : $qemu_cpu
  qemu             : $("$qemu" --version 2>/dev/null | head -n1)
  kernel           : $kernel
  initramfs        : $initramfs
  memory           : $memory
  export name      : $export_name   (-N is mandatory; empty name => "nbd: unknown export")
  server           : $server_host:$port   (guest-initiated; no hostfwd rule)
  device           : /dev/nbd0     (NOT /dev/nbd0p1: no partition table is written)
  mkfs on boot     : $do_mkfs
  share mount      : $volume_path
  volume           : $volume_path/volumes/$volume_name ($volume_bytes bytes)
  share CommitAck  : $commit_ack
  cache in path    : none in QEMU — the guest kernel NBD client speaks TCP
                     directly to DittoFS, so there is no QEMU block layer and a
                     -drive cache= mode is irrelevant. Guest page-cache state is
                     printed by the guest init before switch_root.
EOF

# The guest init. mkfs only on the first boot; then mount, print the caches, and
# switch_root. nbd-client is backgrounded because -p does not daemonize.
#
# Assembled with printf + quoted heredocs rather than sed: the values substituted
# here (export name, host, port) must land in the guest unquoted-verbatim, while
# the surrounding lines carry guest-side shell syntax that must NOT be expanded
# by this script.
guest_init() {
	# The shebang is NOT optional. The kernel executes /init directly, and a
	# script with no shebang is not an executable image: the boot dies with
	# "No working init found" after trying /sbin/init, /etc/init, /bin/init and
	# /bin/sh. Measured, with the archive otherwise correct and /init present and
	# mode 755. Point it at the busybox the initramfs must already carry.
	cat <<'EOF'
#!/bin/sh
# The mount points must exist before they can be mounted. An initramfs built from
# a distribution tree carries /proc, /sys and /dev as DIRECTORIES; one built from
# a hand-made tree (or unpacked by a tool that dropped empty dirs) does not, and
# the failure is quiet and misleading: `mount -t proc proc /proc` fails with
# "No such file or directory", /sys never exists, and the eth0 check below then
# reports "the NIC driver did not load" after loading it successfully — naming
# the wrong cause. Measured. mkdir -p each one first; it is idempotent.
mkdir -p /proc /sys /dev /root /tmp
mount -t proc proc /proc 2>/dev/null || echo "INIT: warning: could not mount /proc"
mount -t sysfs sysfs /sys 2>/dev/null || echo "INIT: warning: could not mount /sys"
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
# modprobe is useless here (no modules.dep in this initramfs), and the module is
# not necessarily at /lib/modules/nbd.ko — the prerequisite accepts nbd.ko*
# ANYWHERE under lib/modules/, e.g. the packaged layout
# lib/modules/<ver>/kernel/drivers/block/nbd.ko.
#
# The search must NOT use `find`: this initramfs carries only the busybox applets
# the host chose to symlink, and `find` is not guaranteed to be among them.
# Measured: an archive whose lib/modules/nbd.ko was present and correct still
# reported "no nbd.ko anywhere under /lib/modules", because the guest had no
# `find` at all. A shell glob plus `set --` needs nothing but the shell itself.
nbd_mod=""
for d in /lib/modules/nbd.ko /lib/modules/nbd.ko.gz /lib/modules/*/nbd.ko /lib/modules/*/nbd.ko.gz \
         /lib/modules/*/kernel/drivers/block/nbd.ko /lib/modules/*/kernel/drivers/block/nbd.ko.gz; do
	[ -e "$d" ] && { nbd_mod=$d; break; }
done
if [ -z "$nbd_mod" ]; then
	echo "INIT-FAIL: no nbd.ko found under /lib/modules (looked at the common layouts). The guest cannot speak NBD."
	exec sh
fi
case "$nbd_mod" in *.gz) gunzip -c "$nbd_mod" >/tmp/nbd.ko && nbd_mod=/tmp/nbd.ko ;; esac
insmod "$nbd_mod" 2>&1 || { echo "INIT-FAIL: insmod $nbd_mod failed (vermagic mismatch with the booted kernel?)"; exec sh; }
EOF
	# -N is mandatory (empty name => "nbd: unknown export"). The '&' is
	# load-bearing: with -p the client never returns (persist branch returns
	# before daemon()).
	# The guest must configure its own NIC: QEMU's user-mode network provides no
	# DHCP, and without an address nbd-client fails with "Network unreachable"
	# while /dev/nbd0 STILL EXISTS (loading the nbd module creates the device
	# node), so a bare `-b /dev/nbd0` test passes and the run proceeds to a mount
	# failure that names the wrong cause. Measured: exactly that.
	# The NIC driver is a MODULE in any distribution kernel (CONFIG_VIRTIO_NET=m
	# on Alpine's linux-virt), so eth0 does not exist until it is loaded. Every
	# `ip` line below then fails SILENTLY (`2>/dev/null`) and the only symptom is
	# `nbd-client: Network unreachable` much later, with /dev/nbd0 present and
	# size 0. Measured: exactly that, against a kernel where the driver was not
	# loaded. insmod each module rather than modprobe: an initramfs built from a
	# distribution tree has no modules.dep, so modprobe finds nothing.
	cat <<'EOF'
# Resolve each module with a GLOB, not `find` — the same reason the nbd module
# above is resolved that way: this initramfs carries only the busybox applets the
# host chose to symlink and `find` is not guaranteed to be among them. Using
# `find` here reintroduced the exact defect the comment above documents.
load_mod() {
	_name=$1
	_path=""
	for _d in /lib/modules/$_name.ko /lib/modules/$_name.ko.gz \
	          /lib/modules/*/$_name.ko /lib/modules/*/$_name.ko.gz \
	          /lib/modules/*/kernel/*/$_name.ko /lib/modules/*/kernel/*/*/$_name.ko \
	          /lib/modules/*/kernel/*/*/$_name.ko.gz; do
		[ -e "$_d" ] && { _path=$_d; break; }
	done
	[ -n "$_path" ] || { echo "INIT: no $_name.ko found under /lib/modules"; return 1; }
	case "$_path" in *.gz) gunzip -c "$_path" >/tmp/$_name.ko && _path=/tmp/$_name.ko ;; esac
	# An insmod that fails because the module is ALREADY loaded is benign and
	# common: a dependency pulls it in first. Measured, libcrc32c is loaded by
	# crc32c_generic, so the explicit insmod afterwards fails with EEXIST and
	# printed "INIT: insmod libcrc32c failed" — an alarming line for a healthy
	# boot, in a rig whose whole value is that its output can be believed.
	if ! _err=$(insmod "$_path" 2>&1); then
		case "$_err" in
		*"File exists"*|*EEXIST*|*"already loaded"*)
			echo "INIT: $_name already loaded (pulled in as a dependency)"
			;;
		*)
			echo "INIT: insmod $_name failed: $_err"
			return 1
			;;
		esac
	fi
	echo "INIT: loaded $_name"
	return 0
}
# Order matters: net_failover depends on failover, virtio_net on net_failover.
for m in failover net_failover virtio_net virtio_mmio; do
	load_mod "$m"
done
# ext4 is a MODULE on any distribution kernel (CONFIG_EXT4_FS=m on Alpine's
# linux-virt), and so are its dependencies. Without them the handshake and the
# size check both SUCCEED and the failure surfaces at mount time as
# "mount: mounting /dev/nbd0 on /root failed", which reads like a filesystem or
# a device problem rather than a missing module. Measured: exactly that.
for m in crc16 libcrc32c crc32c_generic mbcache jbd2 ext4; do
	load_mod "$m"
done
EOF
	cat <<'EOF'
# A guest NIC that is still absent is a hard failure, not a warning: without it
# the handshake cannot complete and every later error names the wrong cause.
i=0
while [ ! -e /sys/class/net/eth0 ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
if [ ! -e /sys/class/net/eth0 ]; then
	echo "INIT-FAIL: eth0 never appeared, so the NIC driver did not load (looked for"
	echo "  failover, net_failover, virtio_net, virtio_mmio under /lib/modules)."
	echo "  netdevs present: $(ls /sys/class/net 2>/dev/null | tr '\n' ' ')"
	echo "  Without a NIC, nbd-client fails with 'Network unreachable' and the real"
	echo "  cause is this line, not the server."
	exec sh
fi
# `ip` must exist AND its failures must not be swallowed. With `2>/dev/null` on
# each line, a guest whose busybox was not built with the `ip` applet (or whose
# `ip addr add` failed for any reason) proceeds with an unconfigured NIC and the
# only symptom is `nbd-client: Network unreachable` much later — measured, with
# `eth0 addresses: 0` in the diagnostic while all three commands had "succeeded".
if ! command -v ip >/dev/null 2>&1; then
	echo "INIT-FAIL: no 'ip' command in this initramfs. The guest cannot configure"
	echo "  its NIC, so nbd-client will fail with 'Network unreachable'. busybox"
	echo "  must be built with the ip applet, or ifconfig/route provided instead."
	exec sh
fi
ip link set eth0 up || echo "INIT: warning: ip link set eth0 up failed"
ip addr add 10.0.2.15/24 dev eth0 || echo "INIT: warning: ip addr add failed"
ip route add default via 10.0.2.2 || echo "INIT: warning: ip route add failed"
EOF
	printf 'nbd-client -N %s %s %s /dev/nbd0 >/tmp/nbd-client.log 2>&1 &\n' "$export_name" "$server_host" "$port"
	# Wait for a NONZERO SIZE, not merely for the node to exist. Size 0 means the
	# handshake never completed; the node appears as soon as the module loads.
	cat <<'EOF'
i=0
while [ $i -lt 100 ]; do
	sz=$(cat /sys/block/nbd0/size 2>/dev/null || echo 0)
	[ -n "$sz" ] && [ "$sz" != 0 ] && break
	sleep 0.1; i=$((i + 1))
done
sz=$(cat /sys/block/nbd0/size 2>/dev/null || echo 0)
if [ -z "$sz" ] || [ "$sz" = 0 ]; then
	echo "INIT-FAIL: /dev/nbd0 has no size, so the NBD handshake never completed."
	echo "  nbd-client said: $(cat /tmp/nbd-client.log 2>/dev/null | tr '\n' ' ')"
	echo "  device node exists: $([ -b /dev/nbd0 ] && echo yes || echo no) - exists is NOT connected"
	echo "  eth0 addresses: $(ip addr show eth0 2>/dev/null | grep -c 'inet ')"
	echo "  NOTE: 'unknown export' names none of its four causes - volume missing,"
	echo "  share disabled, grant none, or a wrong/missing -N."
	exec sh
fi
EOF
	if [ "$do_mkfs" = 1 ]; then
		cat <<'EOF'
echo "INIT: mkfs.ext4 /dev/nbd0 (first boot)"
mkfs.ext4 -F /dev/nbd0 || { echo "INIT-FAIL: mkfs.ext4 failed"; exec sh; }
EOF
	else
		cat <<'EOF'
echo "INIT: --no-mkfs, expecting an existing filesystem on /dev/nbd0"
EOF
	fi
	cat <<'EOF'
mount -t ext4 /dev/nbd0 /root || { echo "INIT-FAIL: mount /dev/nbd0 failed (device exists and mkfs succeeded, so this is the mount, not the handshake)"; exec sh; }
echo "INIT: mount /dev/nbd0 /root ok"
echo "INIT: guest page cache: $(grep -i '^Cached:' /proc/meminfo)"
echo "INIT: nbd0 write_cache: $(cat /sys/block/nbd0/queue/write_cache 2>/dev/null || echo unavailable)"
echo "INIT: nbd0 size (512b sectors): $(cat /sys/block/nbd0/size 2>/dev/null || echo unavailable)"
echo "INIT: switching root; the root filesystem is the DittoFS NBD export"
exec switch_root /root /sbin/init
EOF
}

# The guest init MUST exist before the archive is packed. The original order
# packed first and wrote init afterwards, so on a fresh --initramfs-dir the
# archive contained no /init at all: rdinit=/init found nothing and the kernel
# panicked. Worse, the `[ ! -e "$initramfs" ]` gate then skipped the build on
# every later run, so the archive was never re-packed with the init sitting on
# disk — the drift the comment claimed to prevent was made permanent.
if [ -n "$initramfs_dir" ]; then
	[ -d "$initramfs_dir" ] || die "--initramfs-dir '$initramfs_dir' is not a directory"
	# The guest init is written HERE, before the archive is packed. The original
	# order packed first and wrote init afterwards, so on a fresh --initramfs-dir
	# the archive contained no /init at all: rdinit=/init found nothing and the
	# kernel panicked. Worse, the `[ ! -e "$initramfs" ]` gate then skipped the
	# build on every later run, so the archive was never re-packed with the init
	# sitting on disk — the drift the old comment claimed to prevent was made
	# permanent.
	guest_init >"$initramfs_dir/init"
	chmod +x "$initramfs_dir/init"
fi

# ALWAYS rebuild when --initramfs-dir is given. The generated init bakes in the
# export name, the server host and port, and the mkfs decision, so an archive
# built by an earlier invocation with different flags is silently WRONG: the run
# would connect to the old export and mkfs or not according to the old value,
# while the flags printed on screen said otherwise. Gating the rebuild on
# `[ ! -e "$initramfs" ]` (as this did) made that the DEFAULT behaviour, since
# the archive exists after the first run. Packing a directory costs seconds;
# trusting a stale archive costs a debugging session on a rig that is not
# testing what it claims.
if [ -n "$initramfs_dir" ]; then
	[ -d "$initramfs_dir" ] || die "--initramfs-dir '$initramfs_dir' is not a directory"
	command -v cpio >/dev/null 2>&1 || die "cpio not found; it is required to build the initramfs from '$initramfs_dir'"
	command -v gzip >/dev/null 2>&1 || die "gzip not found; it is required to build the initramfs from '$initramfs_dir'"
	[ -x "$initramfs_dir/bin/nbd-client" ] ||
		die "initramfs source '$initramfs_dir' has no executable bin/nbd-client. The guest cannot connect to NBD without it. Build nbd-client from master (see test/nbd/README.md) and place it at '$initramfs_dir/bin/nbd-client'."
	[ -n "$(find "$initramfs_dir/lib/modules" -name 'nbd.ko*' -print -quit 2>/dev/null)" ] ||
		die "initramfs source '$initramfs_dir' has no nbd.ko under lib/modules/. The guest kernel cannot speak NBD without the nbd module. Install the matching kernel modules (e.g. /lib/modules/\$(uname -r)/kernel/drivers/block/nbd.ko) into '$initramfs_dir/lib/modules/'."
	# A dynamically linked nbd-client (or busybox) needs its ELF interpreter
	# inside the archive. Without it the kernel cannot even exec /init and reports
	# "No working init found", having tried /sbin/init, /etc/init, /bin/init and
	# /bin/sh — a message that names neither the interpreter nor the file that
	# needs it. Measured: an archive whose /init and bin/busybox were both present
	# and mode 755 panicked this way solely because lib/ld-musl-aarch64.so.1 was
	# missing. Only checked when the binary is dynamic, so a static build passes.
	if command -v file >/dev/null 2>&1 &&
		file -L "$initramfs_dir/bin/nbd-client" 2>/dev/null | grep -q 'dynamically linked'; then
		interp=$(file -L "$initramfs_dir/bin/nbd-client" 2>/dev/null |
			sed -n 's/.*interpreter \([^,]*\).*/\1/p' | head -n1)
		if [ -n "$interp" ]; then
			[ -e "$initramfs_dir$interp" ] ||
				die "initramfs source '$initramfs_dir' has a dynamically linked bin/nbd-client needing '$interp', which is not in the archive. The kernel would report only 'No working init found'. Copy the interpreter (and any libraries it needs) into '$initramfs_dir$interp', or build nbd-client statically."
		fi
	fi

	mkdir -p "$(dirname -- "$initramfs")"
	printf 'boot-qemu.sh: building initramfs from %s -> %s\n' "$initramfs_dir" "$initramfs"
	(
		cd "$initramfs_dir"
		find . -print | cpio -o -H newc 2>/dev/null | gzip -9
	) >"$initramfs"
fi
[ -r "$initramfs" ] ||
	die "initramfs '$initramfs' not found. Pass --initramfs PATH, set NBD_INITRAMFS, place one at test/nbd/artifacts/initramfs.cpio.gz, or pass --initramfs-dir DIR to build one."

# The initramfs must actually carry the nbd module and nbd-client; a boot that
# reaches the handshake without them fails with a message naming neither.
#
# Both checks must avoid `grep -q` on the END of a pipe from gzip. Under
# `set -o pipefail` that combination is a FALSE NEGATIVE: grep -q exits at the
# first match, gzip is killed by SIGPIPE, and the pipeline's status becomes 141
# (128+13) even though the string WAS found. Measured on a valid initramfs:
#   gzip -dc x.cpio.gz | grep -aq nbd-client >/dev/null   -> status 141  (refused)
#   gzip -dc x.cpio.gz | grep -a  nbd-client >/dev/null   -> status 0    (correct)
#   gzip -dc x.cpio.gz | grep -a  <absent>   >/dev/null   -> status 1    (correct)
# So the original would have refused EVERY initramfs, valid or not. Dropping -q
# makes grep read to EOF, so no SIGPIPE arises and the status means what it says.
#
# (Do NOT "fix" this by capturing the stream into a variable: a cpio archive is
# full of NUL bytes, which a shell variable cannot hold — measured, a 3.6MB
# listing came back as an empty string and both checks reported MISSING.)
if command -v gzip >/dev/null 2>&1 && command -v grep >/dev/null 2>&1; then
	if ! gzip -dc -- "$initramfs" 2>/dev/null | grep -a 'nbd-client' >/dev/null; then
		die "initramfs '$initramfs' does not contain nbd-client. The guest would boot and never reach the export. Rebuild it with bin/nbd-client (see test/nbd/README.md) or pass --initramfs-dir."
	fi
	if ! gzip -dc -- "$initramfs" 2>/dev/null | grep -a 'nbd\.ko' >/dev/null; then
		die "initramfs '$initramfs' does not contain the nbd kernel module (nbd.ko). modprobe nbd would fail in the guest. Rebuild it with lib/modules/.../nbd.ko or pass --initramfs-dir."
	fi
fi

qemu_args=(
	-M virt
	-accel "$accel"
	-cpu "$qemu_cpu"
	-m "$memory"
	-kernel "$kernel"
	-initrd "$initramfs"
	-append "console=ttyAMA0 rdinit=/init"
	-netdev user,id=n0
	-device virtio-net,netdev=n0
	-display none
	-serial stdio
)

printf 'boot-qemu.sh: launch: %s' "$qemu"
printf ' %q' "${qemu_args[@]}"
printf '\n'

if [ "$dry_run" = 1 ]; then
	printf 'boot-qemu.sh: --dry-run, not booting\n'
	exit 0
fi

# A prebuilt archive's /init cannot be rewritten here, and an init that does not
# run the right nbd-client line fails with "nbd: unknown export" — the same
# message a missing volume, a disabled share or a `none` grant produce. Rather
# than boot and let the operator chase the wrong cause, verify the archive's init
# mentions the export name we are about to use, and refuse if it does not.
if [ -z "$initramfs_dir" ]; then
	[ "${NBD_ALLOW_UNVERIFIED_INITRAMFS:-0}" = 1 ] ||
		gzip -dc -- "$initramfs" 2>/dev/null | grep -a 'nbd-client' >/dev/null ||
		die "prebuilt initramfs '$initramfs' does not appear to run nbd-client in its /init, so the guest would never reach the export. Boot would fail with 'nbd: unknown export', which names neither the cause nor this flag. Build it from a directory with --initramfs-dir so the init is generated, or set NBD_ALLOW_UNVERIFIED_INITRAMFS=1 to boot it anyway."
	printf 'boot-qemu.sh: prebuilt initramfs — its /init must itself run\n'
	printf '  insmod the nbd.ko from lib/modules; nbd-client -N %s %s %s /dev/nbd0 &\n' "$export_name" "$server_host" "$port"
	printf '  then mkfs (first boot only), mount /dev/nbd0 /root, switch_root.\n'
fi

exec "$qemu" "${qemu_args[@]}"
