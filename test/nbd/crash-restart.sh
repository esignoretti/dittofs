#!/usr/bin/env bash
#
# crash-restart.sh — SIGKILL a live DittoFS NBD server mid-flight and prove the
# kernel client's I/O PAUSES and then RESUMES without a filesystem error.
#
# LINUX ONLY. macOS has no nbd-client and no Linux block layer to pause, so this
# script refuses cleanly on Darwin.
#
# The ordering below is the whole test. The two orderings that look right and
# prove nothing (both were actually run first, against a real server):
#
#   * Reading BEFORE killing. The read is served from the page cache and returns
#     in elapsed=0s; the kill then lands on an idle device. This passed and meant
#     nothing.
#   * Writing WITHOUT flushing. The write is acknowledged into page cache, dd
#     reports success, and a later read returns the cached copy.
#
# So: kill the server FIRST, before issuing the read, and FLUSH the pattern
# before the kill. A pass without a nonzero pause AND a completed read after the
# restart has tested the page cache, not the adapter.
#
# The mechanism (measured against real DittoFS, and the error the first draft of
# this step made): -T alone does NOT reconnect.
#
#   wait_for_reconnect()                      nbd.c:1101-1114
#       if (!config->dead_conn_timeout) return 0;   <- no window => fail IMMEDIATELY
#       wait_event_timeout(config->conn_wait,
#           DISCONNECTED || live_connections > 0, dead_conn_timeout)
#
#   grep -c nbd_reconnect_socket drivers/block/nbd.c   => 0
#
# `live_connections` is incremented only by userspace netlink operations
# (nbd_add_socket :1339, nbd_genl_reconfigure :1401). The kernel notices the dead
# link, multicasts NBD_CMD_LINK_DEAD, and waits for a client to hand it a new
# socket. So the two settings do different jobs and BOTH are needed:
#
#   -T <seconds> / --dead-timeout  sets the kernel's patience before failing
#                                  in-flight I/O. NOT documented in the man page
#                                  (grep -i dead man/nbd-client.8 finds only
#                                  "deadlock"); args.c:76, handled at :199.
#   -p / --persist                 runs the client's monitor: NBD_CMD_LINK_DEAD ->
#                                  reconnect_with_backoff -> netlink_reconfigure
#                                  -> NBD_CMD_RECONFIGURE. This is the ONLY thing
#                                  that performs the reconnect. It supplies a 30s
#                                  default window (nbd-client.c:188), which -T
#                                  overrides.
#
# -T alone pauses the I/O for the full window and then returns BLK_STS_IOERR,
# which reads exactly like a DittoFS failure and is not one.
#
# -T reaches the kernel ONLY through netlink, as NBD_ATTR_DEAD_CONN_TIMEOUT
# (nbd-client.c:185-186). On the ioctl path it is parsed, stored, and never read.
# With -nonetlink/-L the script would print a configured window the kernel never
# received, so -L is refused and netlink support is a prerequisite.
#
# THE CLIENT MUST BE IN THE INIT NETWORK NAMESPACE. nbd_mcast_index sends
# NBD_CMD_LINK_DEAD with genlmsg_multicast(&nbd_genl_family, ...)
# (drivers/block/nbd.c:2673), and genlmsg_multicast hardcodes &init_net
# (include/net/genetlink.h:530). A client in a container netns never receives it,
# so persist_conn.link_dead is never set, the persist loop never reconnects, and
# the device behaves exactly as with no -p at all: blocks for the full window,
# then BLK_STS_IOERR. Measured, changing only the namespace: zero messages and a
# failed read in a container netns; with --network=host the listener received the
# message, the client logged "Received link dead notification for device 0" ->
# "Successfully reconnected after 3 attempts", and the blocked read completed
# with the pre-crash bytes. There is no way to deliver the message into another
# netns from the kernel side; -nonetlink/-L is worse than useless here.
#
# THE VERSION TRAP. No RELEASED nbd-client can reconnect. The script therefore
# checks the CAPABILITY (does this build accept -T, and is it a git build rather
# than a release tag?) and refuses known-broken releases by name:
#
#   3.27.0 / 3.27.1  gate the persist loop on cur_client->persist_mode
#                    (nbd-client.c:1548) but the CLI sets cur_client->persist
#                    (args.c:186-188) and NOTHING in the tree ever assigns
#                    persist_mode = true. -p is silently ignored, the loop never
#                    starts, the kernel never receives NBD_CMD_RECONFIGURE, and
#                    the device pauses for the window then returns BLK_STS_IOERR.
#   <= 3.26.1        has no -T at all (absent from args.c), never sends
#                    NBD_ATTR_DEAD_CONN_TIMEOUT, and its netlink path does
#                    netlink_configure(...); return 0; — so it never reaches the
#                    ioctl reconnect loop and -p is silently ignored too.
#
# A version floor of "3.27.1 or newer" would pass on exactly the two broken
# releases. The requirement is a build from master (after eac55e0b06fe,
# 2026-08-30), and that is what the refusal names.
#
# A working measured verdict to reproduce: read still blocked at t=4s/8s/12s,
# server back at t=16s, read completed exit=0 elapsed=14s with
# PRE-CRASH-PATTERN-AAAA, and the persist daemon logged "Successfully reconnected
# after 4 attempts". Blocking proves -T; resuming proves -p; both in one run.

set -euo pipefail

usage() {
	cat <<'EOF'
crash-restart.sh — SIGKILL a live DittoFS NBD server and prove client I/O pauses
and resumes inside the dead-connection window. LINUX ONLY (needs root, the Linux
block layer, netlink, and a master-built nbd-client).

Usage:
  sudo test/nbd/crash-restart.sh [options]

Options:
  --export NAME        Export name <share>/<volume> (default $DITTOFS_NBD_EXPORT,
                       else export/boot). -N is mandatory; an omitted -N sends an
                       EMPTY name and fails as "nbd: unknown export".
  --port PORT          DittoFS NBD port (default $DITTOFS_NBD_PORT, else 10809).
  --server ADDR        Server address (default 127.0.0.1).
  --device DEV         NBD device (default /dev/nbd0). Not a partition: the
                       export is a single file served whole.
  --dead-timeout SECS  Kernel dead_conn_timeout, passed as -T (default
                       $NBD_DEAD_TIMEOUT, else 60). Kept under ~120s: over that
                       the hung-task detector trips.
  --dfs-pid PID        PID to SIGKILL (default: discovered via pgrep).
  --dfs-pattern RE     pgrep pattern for the server (default "dfs").
  --restart-cmd CMD    Shell command that restarts the server, run after the
                       kill (default $DITTOFS_RESTART_CMD). Required unless
                       --keep-dead.
  --volume-path DIR    Host path where the share is mounted over NFS/SMB
                       (default $DITTOFS_VOLUME_PATH, else /mnt/dittofs/export).
                       Used to print the export size and adapter identity before
                       the run.
  --pattern STR        Pattern written and flushed before the kill
                       (default PRE-CRASH-PATTERN-AAAA).
  --write-size BYTES   Bytes written (default 4096).
  --keep-dead          Do not restart; expect the I/O to FAIL at the window.
                       This is the negative control: it proves -T reached the
                       kernel even when -p cannot reconnect.
  -h, --help           This message.

Environment:
  NBD_CLIENT           nbd-client binary (default nbd-client).
  NBD_CLIENT_ALLOW_UNVERIFIED=1
                       Bypass the master-build capability check. Only for a
                       build you know is fixed; the default refuses releases.

Exit status is the verdict. Every prerequisite is checked with a readable
refusal naming the missing thing and the fix; nothing falls back silently.
EOF
}

die() {
	printf 'crash-restart.sh: REFUSING: %s\n' "$*" >&2
	exit 1
}

step() {
	printf '\n=== %s\n' "$*"
}

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)

export_name=${DITTOFS_NBD_EXPORT:-export/boot}
port=${DITTOFS_NBD_PORT:-10809}
server=${NBD_SERVER:-127.0.0.1}
device=${NBD_DEVICE:-/dev/nbd0}
dead_timeout=${NBD_DEAD_TIMEOUT:-60}
dfs_pid=${DITTOFS_DFS_PID:-}
dfs_pattern=${DITTOFS_DFS_PATTERN:-dfs}
restart_cmd=${DITTOFS_RESTART_CMD:-}
volume_path=${DITTOFS_VOLUME_PATH:-/mnt/dittofs/export}
pattern=${NBD_PATTERN:-PRE-CRASH-PATTERN-AAAA}
write_size=${NBD_WRITE_SIZE:-4096}
nbd_client=${NBD_CLIENT:-nbd-client}
keep_dead=0

while [ $# -gt 0 ]; do
	case $1 in
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
	--server)
		[ $# -ge 2 ] || die "--server needs an address"
		server=$2
		shift 2
		;;
	--device)
		[ $# -ge 2 ] || die "--device needs a path"
		device=$2
		shift 2
		;;
	--dead-timeout)
		[ $# -ge 2 ] || die "--dead-timeout needs seconds"
		dead_timeout=$2
		shift 2
		;;
	--dfs-pid)
		[ $# -ge 2 ] || die "--dfs-pid needs a pid"
		dfs_pid=$2
		shift 2
		;;
	--dfs-pattern)
		[ $# -ge 2 ] || die "--dfs-pattern needs a regex"
		dfs_pattern=$2
		shift 2
		;;
	--restart-cmd)
		[ $# -ge 2 ] || die "--restart-cmd needs a command"
		restart_cmd=$2
		shift 2
		;;
	--volume-path)
		[ $# -ge 2 ] || die "--volume-path needs a directory"
		volume_path=$2
		shift 2
		;;
	--pattern)
		[ $# -ge 2 ] || die "--pattern needs a string"
		pattern=$2
		shift 2
		;;
	--write-size)
		[ $# -ge 2 ] || die "--write-size needs bytes"
		write_size=$2
		shift 2
		;;
	--keep-dead)
		keep_dead=1
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

# Linux only, and it must say why rather than half-run: macOS has no nbd-client
# and no Linux block layer whose I/O could pause.
[ "$(uname -s)" = Linux ] ||
	die "this rig is Linux-only; host is '$(uname -s)'. The crash test needs the Linux block layer's dead_conn_timeout, netlink, and a master-built nbd-client — none of which exist on macOS (which also has no nbd-client). Run it on a Linux host with root. The boot half (boot-qemu.sh) does run on macOS via hvf."

[ "$(id -u)" = 0 ] ||
	die "must run as root (uid $(id -u)): configuring /dev/nbd0 needs CAP_SYS_ADMIN and the netlink NBD family. Re-run with sudo."

command -v "$nbd_client" >/dev/null 2>&1 ||
	die "nbd-client not found on PATH (looked for '$nbd_client'). Install/build it from master — no release can reconnect (see the header and test/nbd/README.md) — or set NBD_CLIENT."

# Capability check, not a version floor: a floor of "3.27.1 or newer" passes on
# exactly the two releases that cannot reconnect.
#
# And NOT a --help check either: the master build ACCEPTS -T while its --help
# does not list it. Measured, master (version 0.unknown) --help output:
#   [-persist|-p] [-nofork|-n] [-systemd-mark|-m] [-i ident|-nonetlink|-L]
# — -T/--dead-timeout is absent, even though `-T 1` is accepted and the option
# is handled in args.c:199. A --help-based check therefore REJECTS the one
# binary this rig requires, with a message telling the operator to build
# exactly what they already built. Probe the behaviour instead: getopt reports
# an unknown option and nothing else does.
#
# Measured discriminator:
#   master:  `-T 1` -> "ERROR: no information specified"   (option accepted)
#   old:     `-T 1` -> "nbd-client: unrecognized option '-T'"
client_probe=$("$nbd_client" -T 1 2>&1 || true)
client_help=$("$nbd_client" --help 2>&1 || true)
case $client_probe in
*"unrecognized option"*)
	die "'$nbd_client' does not accept -T/--dead-timeout (it reports 'unrecognized option'). That is nbd-client 3.26.1 or older: it has no way to set dead_conn_timeout at all, its netlink path returns before the ioctl reconnect loop, and -p is silently ignored. Build nbd-client from master (after eac55e0b06fe, 2026-08-30) and re-run. Set NBD_CLIENT to that binary."
	;;
esac

if [ "${NBD_CLIENT_ALLOW_UNVERIFIED:-0}" != 1 ]; then
	client_version=$("$nbd_client" --version 2>&1 | head -n1 || true)
	# A release tag (e.g. "3.27.1") is a git build only if it carries a hash or
	# is otherwise not a bare release. 3.27.0 and 3.27.1 both have -T and both
	# silently ignore -p; everything older has no -T. Refuse a bare release.
	if printf '%s' "$client_version" | grep -Eq '^[0-9]+\.[0-9]+(\.[0-9]+)?$'; then
		die "nbd-client reports a bare release version ('$client_version'). NO released nbd-client can reconnect: 3.27.0/3.27.1 gate the persist loop on persist_mode, which nothing ever sets true (so -p is ignored and the device returns BLK_STS_IOERR after the window, which looks like a DittoFS bug), and <=3.26.1 has no -T and returns from the netlink path before the loop. Build nbd-client from master (after eac55e0b06fe, 2026-08-30) and re-run. Override only with NBD_CLIENT_ALLOW_UNVERIFIED=1 for a build you know is fixed."
	fi
fi

# netlink must be compiled in and used: -T is delivered to the kernel only via
# NBD_ATTR_DEAD_CONN_TIMEOUT on the netlink path.
#
# A --help check is correct HERE and only here: -L/--nonetlink IS listed in the
# usage text (measured on master: "[-i ident|-nonetlink|-L]"), unlike -T which
# the same --help omits. The two checks must not be conflated for that reason.
case $client_help in
*--nonetlink* | *-L*) ;;
*)
	die "'$nbd_client' does not offer -L/--nonetlink, so it was built without the netlink NBD family. -T is delivered to the kernel ONLY over netlink (NBD_ATTR_DEAD_CONN_TIMEOUT); on the ioctl path the option is parsed, stored, and never read, so the window this script prints would never reach the kernel. Build nbd-client from master with netlink support."
	;;
esac

# The client must be in the init network namespace, or the kernel's
# NBD_CMD_LINK_DEAD multicast (genlmsg_multicast hardcodes &init_net,
# include/net/genetlink.h:530) never reaches it and the device behaves exactly as
# with no -p at all.
if [ -r /proc/self/ns/net ] && [ -r /proc/1/ns/net ]; then
	self_netns=$(readlink /proc/self/ns/net)
	init_netns=$(readlink /proc/1/ns/net)
	[ "$self_netns" = "$init_netns" ] ||
		die "the client would run in network namespace '$self_netns' but the kernel multicasts NBD_CMD_LINK_DEAD only in the init netns ('$init_netns'). In a container netns a netlink listener receives ZERO messages, so persist_conn.link_dead is never set, the persist loop never reconnects, and the device blocks for the full window then fails with BLK_STS_IOERR — measured, changing only the namespace. Re-run the container with --network=host (Docker/Podman). There is no kernel-side workaround and -nonetlink is worse than useless here."
else
	die "/proc/self/ns/net or /proc/1/ns/net is unreadable, so the network namespace cannot be verified. The crash rig must prove it is in the init netns (genlmsg_multicast hardcodes &init_net); run it on a normal Linux host with /proc mounted, or in a container with --network=host."
fi

# The export name is <share>/<volume> with no leading slash; an empty or
# malformed name is refused by DittoFS as "nbd: unknown export", which reads as
# an export-name problem and is not one.
case $export_name in
/*) die "export name '$export_name' has a leading slash; use <share>/<volume>, e.g. export/boot" ;;
*/*) ;;
*) die "export name '$export_name' has no '/'; it must be <share>/<volume>, e.g. export/boot" ;;
esac
volume_name=${export_name#*/}
share_name=${export_name%%/*}
[ -n "$share_name" ] && [ -n "$volume_name" ] ||
	die "export name '$export_name' must have a non-empty share and volume (e.g. export/boot)"

# Numeric sanity, and the hung-task bound.
case $dead_timeout in
'' | *[!0-9]*) die "--dead-timeout must be a positive integer of seconds, got '$dead_timeout'" ;;
esac
[ "$dead_timeout" -ge 5 ] ||
	die "--dead-timeout $dead_timeout is too small to observe a pause; use at least 5s"
[ "$dead_timeout" -le 120 ] ||
	die "--dead-timeout $dead_timeout exceeds ~120s, where the hung-task detector trips and the guest can be killed by the kernel. Use at most 120 (60 is the recommended value)."

[ "$keep_dead" = 1 ] || [ -n "$restart_cmd" ] ||
	die "no way to restart the server: pass --restart-cmd 'CMD' (or set DITTOFS_RESTART_CMD). Without it the read can only fail at the window, which proves -T but not -p. To run that negative control deliberately, pass --keep-dead."

command -v dd >/dev/null 2>&1 || die "dd not found; it is required to write and read the pattern"
command -v pgrep >/dev/null 2>&1 || die "pgrep not found (procps); it is required to find the server to SIGKILL, or pass --dfs-pid PID"

[ -b "$device" ] ||
	die "'$device' is not a block device. The nbd kernel module must be loaded and /dev/nbd0 present (modprobe nbd). A p1 suffix is wrong: the export is a single file served whole, with no partition table."

# The two unrelated causes of "the write never lands" share one symptom, so the
# script prints the export size and adapter identity BEFORE it starts and never
# treats one message as diagnostic on its own. The adapter runs as UID/GID 65534
# (pkg/adapter/nbd/config.go:26-27) and applyDefaults rewrites a configured
# UID == 0 back to 65534 (:125-129), so the volume and volumes/ must be created
# by that identity; a volume owned by anyone else at mode 600644 refuses the
# adapter's write with AccessDenied (calculatePermissions,
# pkg/metadata/auth_permissions.go:183, takes the "other" bits). And chmod through
# the SMB mount does NOT change the mode (measured: mode_oct=600644 before and
# after chmod 666).
step "pre-flight: export and identity"
printf '  export name      : %s\n' "$export_name"
printf '  server           : %s:%s\n' "$server" "$port"
printf '  device           : %s\n' "$device"
printf '  nbd-client       : %s (%s)\n' "$nbd_client" "${client_version:-version unknown}"
printf '  netns            : $self_netns (init: $init_netns) — OK\n'
printf '  dead_conn_timeout: %ss, set with -T/--dead-timeout AND -p/--persist\n' "$dead_timeout"
printf '  adapter identity : UID/GID 65534 (not configurable; applyDefaults rewrites UID 0)\n'
printf '  share mount      : %s\n' "$volume_path"
if [ -e "$volume_path/volumes/$volume_name" ]; then
	vol_bytes=$(wc -c <"$volume_path/volumes/$volume_name" | tr -d ' ')
	printf '  export size      : %s bytes (%s)\n' "$vol_bytes" "$volume_path/volumes/$volume_name"
	[ "$vol_bytes" -gt 0 ] 2>/dev/null ||
		die "volume '$volume_name' is $vol_bytes bytes. CreateFile with a size does NOT produce a file of that size (ApplyCreateDefaults, pkg/metadata/validation.go:492, sets Size = 0 for every non-symlink), so the size must be set afterwards. A zero-length export fails the handshake."
else
	printf '  export size      : UNKNOWN — %s/volumes/%s not found\n' "$volume_path" "$volume_name"
	die "volume '$volume_name' not found at '$volume_path/volumes/$volume_name'. Provision it through the share as the adapter's identity (UID/GID 65534) and set its size after creating it. If the guest then fails the handshake with 'nbd: unknown export', that names neither the volume nor the identity — and the same message also means a share grant of 'none' (the resolution itself is refused) or an omitted -N."
fi

# ------------------------------------------------------------------ connect

if [ -n "$dfs_pid" ]; then
	[ -d "/proc/$dfs_pid" ] ||
		die "--dfs-pid $dfs_pid is not a running process"
else
	dfs_pid=$(pgrep -f -- "$dfs_pattern" | head -n1 || true)
	[ -n "$dfs_pid" ] ||
		die "no server process matching '$dfs_pattern' found (pgrep -f). Start the server first, or pass --dfs-pid PID / --dfs-pattern RE."
fi
printf '  server pid       : %s (will be SIGKILLed)\n' "$dfs_pid"

cleanup() {
	if [ -n "${read_pid:-}" ] && kill -0 "$read_pid" 2>/dev/null; then
		kill "$read_pid" 2>/dev/null || true
	fi
	"$nbd_client" -d "$device" >/dev/null 2>&1 || true
}
trap cleanup EXIT

step "connect: $nbd_client -N $export_name -p -T $dead_timeout $server $port $device"
# -p does NOT daemonize (the persist branch returns before daemon(0,0),
# nbd-client.c:1571 vs :1580), so it must be backgrounded here too. Both flags are
# passed and both are printed: -T sets the window, -p performs the reconnect.
#
# The client's own output is captured to a file because its "Received link dead
# notification" line is the ONE piece of evidence that the kernel's multicast
# reached this process — the netns gate below is only a proxy for it, and it
# fails open in a container (see the note there). The success path asserts on
# this line.
client_log=$(mktemp "${TMPDIR:-/tmp}/nbd-client.XXXXXX")
"$nbd_client" -N "$export_name" -p -T "$dead_timeout" "$server" "$port" "$device" >"$client_log" 2>&1 &
client_pid=$!
for _ in $(seq 1 100); do
	[ -b "$device" ] && break
	sleep 0.1
done
[ -b "$device" ] ||
	die "nbd-client did not bring up $device. A handshake failure is reported here; check the server is listening on $server:$port, the share grant is read-write (a 'none' grant refuses resolution as 'nbd: unknown export'), and the export name is exactly <share>/<volume>."

step "write pattern and FLUSH it (the flush is what makes the bytes the server's problem)"
printf '%s' "$pattern" | dd of="$device" bs=1 count="${#pattern}" conv=fsync status=none ||
	die "the initial write to $device failed. Check the adapter identity: the volume and volumes/ must be owned by UID/GID 65534, or the write is refused with AccessDenied. A 'CAS object missing ... chunk not found' here instead means the volume was written by a server whose in-memory remote tier has since restarted — a different cause with the same symptom, which is why the export size and identity are printed above."
sync
printf '  wrote %s bytes, flushed\n' "${#pattern}"

step "SIGKILL the server FIRST, before issuing the read"
# Killing before the read is load-bearing: a read issued first is served from the
# page cache in elapsed=0s and the kill lands on an idle device.
kill -9 "$dfs_pid" ||
	die "could not SIGKILL pid $dfs_pid"
for _ in $(seq 1 50); do
	[ -d "/proc/$dfs_pid" ] || break
	sleep 0.1
done
[ -d "/proc/$dfs_pid" ] &&
	die "pid $dfs_pid survived SIGKILL; refusing to continue (nothing was crashed)"
printf '  server pid %s killed\n' "$dfs_pid"

# ------------------------------------------------------- read must PAUSE, then resume

step "issue the read — it must PAUSE, not fail (that is -T reaching the kernel)"
read_out=$(mktemp)
read_start=$SECONDS
dd if="$device" bs=1 count="${#pattern}" status=none >"$read_out" 2>"$read_out.err" &
read_pid=$!

blocked_samples=0
pause_seen=0
deadline=$((dead_timeout + 20))
while kill -0 "$read_pid" 2>/dev/null; do
	elapsed=$((SECONDS - read_start))
	if [ "$elapsed" -gt "$deadline" ]; then
		die "the read is still blocked at ${elapsed}s, past the ${dead_timeout}s window + 20s slack. The server was never restarted, or -p cannot reconnect (a release nbd-client). No verdict: this run would only time out."
	fi
	if [ "$blocked_samples" -gt 0 ] || [ "$elapsed" -ge 1 ]; then
		if [ "$pause_seen" = 0 ] && [ "$elapsed" -ge 2 ]; then
			pause_seen=1
			printf '  read still blocked at t=%ss\n' "$elapsed"
		elif [ "$pause_seen" = 1 ] && [ $((elapsed % 4)) -eq 0 ] && [ "$elapsed" != "${last_logged:-}" ]; then
			last_logged=$elapsed
			printf '  read still blocked at t=%ss\n' "$elapsed"
		fi
	fi
	blocked_samples=$((blocked_samples + 1))

	# Restart inside the window, once, while the read is demonstrably blocked.
	if [ "$keep_dead" = 0 ] && [ "$blocked_samples" -ge 20 ] && [ "${restarted:-0}" = 0 ]; then
		restarted=1
		restart_at=$elapsed
		step "restart the server inside the window (at t=${elapsed}s, window ${dead_timeout}s)"
		if ! sh -c "$restart_cmd"; then
			die "the restart command failed: $restart_cmd. The read cannot resume without it."
		fi
		printf '  server restarted at t=%ss\n' "$restart_at"
	fi
	sleep 0.2
done

wait "$read_pid"
read_status=$?
read_elapsed=$((SECONDS - read_start))

if [ "$keep_dead" = 1 ]; then
	# Negative control: without a restart the I/O must FAIL at the window, which
	# is what proves -T reached the kernel even when nothing reconnects it.
	[ "$read_status" -ne 0 ] ||
		die "--keep-dead: the read SUCCEEDED with no server running. That can only mean it was served from the page cache, so this run tested nothing (the flush or the kill ordering is wrong)."
	printf '\nVERDICT (negative control): the read failed with status %s after %ss with no server running — that is -T reaching the kernel.\n' "$read_status" "$read_elapsed"
	exit 0
fi

# The two halves of the assertion. Blocking alone proves only -T; resuming is
# what proves -p.
[ "$pause_seen" = 1 ] ||
	die "the read never blocked (elapsed=${read_elapsed}s, status=$read_status). It was served from the page cache or the device was already failed, so this run tested the page cache, not the adapter. Nothing is proven; do not report a pass."
[ "$read_status" -eq 0 ] ||
	die "the read failed with status $read_status after ${read_elapsed}s. It blocked (so -T reached the kernel) but never resumed — -p did not hand the kernel a new socket. Check that nbd-client is a master build (no release can reconnect), that the persist daemon is running, and that it logged 'Received link dead notification for device 0'. Do not read this as a DittoFS failure."
[ "${restarted:-0}" = 1 ] ||
	die "the read completed but the server was never restarted by this script; the completion cannot be attributed to a reconnect. Nothing is proven."
[ "$restart_at" -lt "$dead_timeout" ] ||
	die "the server was restarted at t=${restart_at}s, outside the ${dead_timeout}s window; the I/O may have been failed and re-issued rather than paused. Increase --dead-timeout or fix the restart latency. Not a pass."

read_bytes=$(cat "$read_out")
[ "$read_bytes" = "$pattern" ] ||
	die "the read resumed but returned '${read_bytes}', not the pre-crash '${pattern}'. The bytes did not survive the crash — that is a real durability finding, not a harness problem."

printf '\nVERDICT: read paused (blocked for >=2s), server restarted at t=%ss inside the %ss window, read completed status=0 elapsed=%ss with %s.\n' \
	"$restart_at" "$dead_timeout" "$read_elapsed" "$read_bytes"
printf 'Blocking proves -T; resuming proves -p. Both are present in this run.\n'

if command -v fsck.ext4 >/dev/null 2>&1; then
	printf '\nNOTE: run the host-side fsck yourself, against the volume read as a file\n'
	printf 'over NFS — never from inside the guest, whose root IS the volume under\n'
	printf 'test. Example: fsck.ext4 -fn <nfs-mount>/volumes/%s\n' "$volume_name"
fi

exit 0
