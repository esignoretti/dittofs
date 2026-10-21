#!/usr/bin/env bash
#
# bench-fio.sh — measure a live NBD export with fio.
#
# The pipelining plan has cited this script since it was written and it did not
# exist. This is it, plus the honest account of why the numbers in that plan were
# measured a different way.
#
# WHAT IT MEASURES
#
#   Random 4 KiB reads at increasing queue depth. This is the measurement that
#   separates a serialized dispatcher from a pipelined one: a server that serves
#   one command at a time makes depth 8, 16, 32 and 64 report the same number,
#   because the client's queue is only a queue of waits. A server that can have
#   several commands in flight keeps climbing with depth until it hits the block
#   store's own ceiling.
#
#   Sequential read and write, 64 MiB each, at depth 1: the bandwidth number.
#
#   fsync-per-4 KiB: the latency of a durable write, which is what a database or
#   a filesystem journal actually waits for.
#
# TWO WAYS TO REACH THE EXPORT, AND WHY BOTH ARE HERE
#
#   fio can speak NBD itself, but only through its `nbd` engine, which is built
#   only when libnbd was present at build time. It usually is not: the macOS
#   Homebrew build has no nbd engine at all, and neither do most distro packages.
#   So this script does not assume it.
#
#   --device /dev/nbdX   measure an already-attached block device. Requires the
#                        caller to have run nbd-client; this script will not
#                        attach for you, because attaching needs root and a
#                        device name, and guessing either is worse than asking.
#
#   (no --device)        use fio's nbd engine against host:port/export. Refused
#                        with an explanation when the engine is absent.
#
#   qemu-img bench is the third option and the one the recorded numbers used: it
#   speaks the NBD protocol directly, needs no root and no libnbd, and reports
#   elapsed time rather than IOPS. It measures the same thing. See
#   docs/nbd-benchmarks.md for that sweep, which is what the pipelining results
#   were measured with.
#
# OUTPUT
#
#   One `depth<TAB>iops` line per depth, then the sequential and fsync lines, so
#   two runs can be compared with paste:
#
#     paste /tmp/bench-before.txt /tmp/bench-after.txt
#
# Requires: fio. Plus libnbd for the engine path, or a root-attached device for
# --device.
#
#   ./bench-fio.sh 127.0.0.1 10809 export/boot
#   ./bench-fio.sh --device /dev/nbd0 export/boot --depth 16 --runtime 30
#   ./bench-fio.sh 10.0.0.5 10809 export/boot --depth "1 2 4 8 16 32 64"

set -euo pipefail

PROG="$(basename "$0")"

usage() {
	cat <<EOF
Usage: $PROG <host> <port> <export> [options]
       $PROG --device <path> <export> [options]

Measures a live NBD export with fio and prints one line per result.

Arguments:
  <host> <port> <export>   the NBD endpoint, e.g. 127.0.0.1 10809 export/boot

Options:
  --device PATH     measure an attached block device instead of using fio's nbd
                    engine. Needs nbd-client already run; this script will not
                    attach for you.
  --depth LIST      space-separated depths for the random-read sweep
                    (default: "1 8 16 32 64")
  --runtime SECONDS seconds per measurement (default: 20)
  --size BYTES      transfer size for the random sweep (default: 4096)
  --seq-size BYTES  size of each sequential run (default: 64M)
  -h, --help        this text

Why depth matters: a server that handles one command at a time reports the same
IOPS at every depth above 1, because the client's queue is only a queue of waits.
EOF
}

die() {
	echo "$PROG: $*" >&2
	exit 2
}

# --- arguments ---------------------------------------------------------------

HOST=""
PORT=""
EXPORT=""
DEVICE=""
DEPTHS="1 8 16 32 64"
RUNTIME=20
BLOCK_SIZE=4096
SEQ_SIZE=64M

positional=()
while [ $# -gt 0 ]; do
	case "$1" in
	--device)
		[ $# -ge 2 ] || die "--device needs a path"
		DEVICE="$2"
		shift 2
		;;
	--depth)
		[ $# -ge 2 ] || die "--depth needs a list"
		DEPTHS="$2"
		shift 2
		;;
	--runtime)
		[ $# -ge 2 ] || die "--runtime needs a number"
		RUNTIME="$2"
		shift 2
		;;
	--size)
		[ $# -ge 2 ] || die "--size needs a number"
		BLOCK_SIZE="$2"
		shift 2
		;;
	--seq-size)
		[ $# -ge 2 ] || die "--seq-size needs a size"
		SEQ_SIZE="$2"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		die "unknown option '$1' (try --help)"
		;;
	*)
		positional+=("$1")
		shift
		;;
	esac
done

if [ -n "$DEVICE" ]; then
	[ "${#positional[@]}" -eq 1 ] || die "--device needs exactly one positional argument (the export name, for the report)"
	EXPORT="${positional[0]}"
	[ -b "$DEVICE" ] || die "--device '$DEVICE' is not a block device. Attach it first: nbd-client <host> <port> $DEVICE -N <export>"
	TARGET="$DEVICE"
	ENGINE_PATH=0
else
	[ "${#positional[@]}" -eq 3 ] || { usage >&2; exit 2; }
	HOST="${positional[0]}"
	PORT="${positional[1]}"
	EXPORT="${positional[2]}"
	TARGET="nbd://${HOST}:${PORT}/${EXPORT}"
	ENGINE_PATH=1
fi

case "$RUNTIME" in
'' | *[!0-9]*) die "--runtime must be a whole number of seconds, got '$RUNTIME'" ;;
esac
case "$BLOCK_SIZE" in
'' | *[!0-9]*) die "--size must be a whole number of bytes, got '$BLOCK_SIZE'" ;;
esac

# --- prerequisites -----------------------------------------------------------

# fio's own message for a missing engine is "engine 'nbd' not loadable", which
# reads like a bug rather than a missing build option. Say what it means.
command -v fio >/dev/null 2>&1 || die "fio is not on PATH. Install it and re-run:
  macOS:  brew install fio
  Debian: apt-get install fio
  Fedora: dnf install fio"

if [ "$ENGINE_PATH" -eq 1 ]; then
	if ! fio --enghelp 2>/dev/null | grep -qx 'nbd'; then
		cat >&2 <<EOF
$PROG: fio was built without its 'nbd' engine, so it cannot open an NBD URI.

fio only compiles that engine when libnbd is present at build time, and most
packaged builds omit it. Three ways forward:

  1. Attach the export yourself and measure the block device:

       sudo nbd-client $HOST $PORT /dev/nbd0 -N $EXPORT
       $PROG --device /dev/nbd0 $EXPORT

  2. Use qemu-img bench, which speaks NBD directly and needs neither root nor
     libnbd. It reports elapsed time rather than IOPS; the sweep in
     docs/nbd-benchmarks.md is measured this way:

       qemu-img bench -f raw -c 10000 -s 4096 -S 4096 -t none -d 8 \\
         nbd://$HOST:$PORT/$EXPORT

  3. Build fio with libnbd and re-run this command.
EOF
		exit 2
	fi
fi

# --- measurement -------------------------------------------------------------

# fio writes its JSON to stdout, but NOT cleanly: a run that trips one of its
# advisory checks prefixes the document with a plain-text "note: ..." line, so the
# output is not parseable JSON as-is. Everything before the first '{' is stripped.
#
# Two extraction bugs are worth recording, because both produced a plausible
# number rather than an error:
#
#   * Taking the FIRST "iops" in the document returns the read section, which is
#     0.000000 for a write job. Every write measurement read as zero.
#   * The same for "bw": the sequential write reported the read section's 0.
#
# So the field is selected by job direction, not by position. python3 does the
# parsing; run-oracle.sh already requires it, so this adds no new dependency.
run_fio() {
	fio --output-format=json --eta=never "$@" 2>/dev/null
}

# fio_field <json> <direction> <field>
#
# direction is read or write; field is iops, bw (KiB/s) or clat_mean (ns).
fio_field() {
	printf '%s' "$1" | sed -n '/^{/,$p' | python3 -c '
import json, sys
direction, field = sys.argv[1], sys.argv[2]
try:
    doc = json.load(sys.stdin)
except json.JSONDecodeError as e:
    sys.exit("bench-fio.sh: fio did not produce JSON: %s" % e)
job = doc["jobs"][0][direction]
if field == "clat_mean":
    print(job["clat_ns"]["mean"])
else:
    print(job[field])
' "$2" "$3"
}

echo "# NBD benchmark: target=$TARGET export=$EXPORT runtime=${RUNTIME}s size=${BLOCK_SIZE}B"
echo "# fio $(fio --version 2>/dev/null | head -1)"

ENGINE="libaio"
[ "$ENGINE_PATH" -eq 1 ] && ENGINE="nbd"

# Random reads at increasing depth. randread over the whole device so the store
# cannot serve the workload from one hot region: qemu-img bench's default step is
# the block size, which re-reads the same offsets in order and measures the cache
# rather than the export.
for d in $DEPTHS; do
	json=$(run_fio --name="randread-d$d" --ioengine="$ENGINE" \
		--filename="$TARGET" --direct=1 --rw=randread --bs="$BLOCK_SIZE" \
		--iodepth="$d" --numjobs=1 --runtime="$RUNTIME" --time_based --group_reporting)
	iops=$(fio_field "$json" read iops) || die "fio produced no readable result for depth $d"
	printf 'depth%s\t%s\n' "$d" "$iops"
done

# Sequential throughput. One job at depth 1: this is bandwidth, not concurrency,
# and raising the depth would measure the queue instead.
json=$(run_fio --name=seqread --ioengine="$ENGINE" --filename="$TARGET" --direct=1 \
	--rw=read --bs=1M --iodepth=1 --numjobs=1 --size="$SEQ_SIZE" --group_reporting)
printf 'seqread_kib_s\t%s\n' "$(fio_field "$json" read bw)"

json=$(run_fio --name=seqwrite --ioengine="$ENGINE" --filename="$TARGET" --direct=1 \
	--rw=write --bs=1M --iodepth=1 --numjobs=1 --size="$SEQ_SIZE" --group_reporting)
printf 'seqwrite_kib_s\t%s\n' "$(fio_field "$json" write bw)"

# fsync-per-4 KiB: the durable-write latency. fsync=1 makes every 4 KiB write its
# own commit, which is what a journal or a database log actually pays.
json=$(run_fio --name=fsync4k --ioengine="$ENGINE" --filename="$TARGET" --direct=1 \
	--rw=randwrite --bs="$BLOCK_SIZE" --iodepth=1 --numjobs=1 --fsync=1 \
	--runtime="$RUNTIME" --time_based --group_reporting)
printf 'fsync4k_iops\t%s\n' "$(fio_field "$json" write iops)"
printf 'fsync4k_clat_mean_ns\t%s\n' "$(fio_field "$json" write clat_mean)"
