#!/usr/bin/env bash
# Run nfstest's dio and delegation modules against the Linux kernel NFS server,
# for comparison with the DittoFS runs.
#
# Most of what those modules assert is what the kernel *client* puts on the
# wire: which WRITEs it sends, whether it caches them, when it returns a
# delegation. A failure against DittoFS is therefore one of two things, and
# only a second server on the same kernel can tell them apart:
#
#   knfsd passes, DittoFS fails  -> ours: something in our replies changes
#                                   what the client decides to send
#   knfsd fails too              -> no server meets the assertion with this
#                                   client, and the known-failure row says
#                                   nothing about DittoFS
#
# The modules run through run.sh, unchanged, so the two runs differ only in the
# server. Each option set is graded against the same tables the DittoFS cell
# uses, and baseline-knfsd.md records, per option set, every assertion knfsd
# fails and what knfsd does on each dio/delegation row of those tables.
#
# Linux only, and not containerised, for the reason baseline-knfsd.sh in
# test/nfs-conformance/pynfs gives: knfsd's sockets belong to the kernel. On
# macOS, run it from CI:
#   gh workflow run nfs-knfsd-baseline.yml
#
# Not a CI gate: it measures a third-party server.
#
# Usage:
#   sudo ./baseline-knfsd.sh [--variants v4.0,v4.1] [--export-dir DIR] [--keep]

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=../option-sets.sh
source "$SCRIPT_DIR/../option-sets.sh"
# shellcheck source=../../common/known-failures.sh
source "$TEST_DIR/common/known-failures.sh"

EXPORT_DIR="/srv/nfstest-baseline"
VARIANTS="$(option_set_names | paste -sd, -)"
KEEP=false

log() { echo "[BASELINE] $*"; }
die() { echo "[BASELINE] $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
    --variants) VARIANTS="${2:?--variants requires a value}"; shift 2 ;;
    --export-dir) EXPORT_DIR="${2:?--export-dir requires a value}"; shift 2 ;;
    --keep) KEEP=true; shift ;;
    -h | --help) sed -n '2,29p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
    esac
done

if [[ "$(uname -s)" != Linux ]]; then
    echo "[BASELINE] The baseline needs the in-kernel nfsd, so it only runs on Linux." >&2
    die "From a macOS box, run it in CI: gh workflow run nfs-knfsd-baseline.yml"
fi
[[ $EUID -eq 0 ]] || die "must run as root (sudo): it exports a filesystem and mounts it"
command -v jq >/dev/null || die "jq is required to read test/conformance/suites.json"

if ! command -v exportfs >/dev/null 2>&1; then
    log "Installing nfs-kernel-server..."
    { apt-get update -qq && apt-get install -y -qq nfs-kernel-server; } || die "could not install nfs-kernel-server"
fi

EXPORTS_FILE="/etc/exports.d/nfstest-baseline.exports"
cleanup() {
    pkill -f 'tcpdump .*nfstest_' 2>/dev/null || true
    if [[ "$KEEP" == true ]]; then
        log "Leaving the export in place (--keep)."
        return
    fi
    rm -f "$EXPORTS_FILE"
    exportfs -ra 2>/dev/null || true
    rm -rf "$EXPORT_DIR"
}
trap cleanup EXIT

# No fsid=0: the export is then reachable at its own path over NFSv3 and
# through the kernel's pseudo-root over NFSv4, so one --export serves every
# option set. no_root_squash because nfstest creates its files as root.
log "Exporting $EXPORT_DIR..."
mkdir -p "$EXPORT_DIR" /etc/exports.d
chmod 777 "$EXPORT_DIR"
echo "$EXPORT_DIR *(rw,insecure,no_root_squash,no_subtree_check)" >"$EXPORTS_FILE"
modprobe nfsd 2>/dev/null || true
systemctl start nfs-server 2>/dev/null || service nfs-kernel-server start 2>/dev/null || true
rpc.nfsd 8 || die "rpc.nfsd failed to start"
exportfs -ra
exportfs -v

RESULTS_ROOT="$SCRIPT_DIR/results/baseline-knfsd-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_ROOT"

# tables VARIANT — the known-failure tables the manifest gives VARIANT,
# colon-separated, exactly as test/conformance/run.sh hands them to run.sh.
tables() {
    jq -r --arg v "$1" '.suites.nfstest.known_failures[$v] | map("'"$TEST_DIR"'/" + .) | join(":")' \
        "$TEST_DIR/conformance/suites.json"
}

IFS=',' read -ra variants <<<"$VARIANTS"
for v in "${variants[@]}"; do
    option_set_opts "$v" >/dev/null || die "unknown option set: $v"
    modules="dio"
    [[ "$(option_set_version "$v")" != 3 ]] && modules+=",delegation"
    mkdir -p "$RESULTS_ROOT/$v"
    log "Running $modules under option set $v against knfsd..."
    # An empty NFSTEST_MOUNTPORT leaves the v3 MOUNT port to rpcbind, where
    # knfsd's mountd registers. run.sh exits with the count of failures not on
    # the tables, which here is a finding, not an error.
    NFS_PORT=2049 NFSTEST_EXPORT="$EXPORT_DIR" NFSTEST_MOUNTPORT="" \
        DITTOFS_RESULTS_DIR="$RESULTS_ROOT/$v" DITTOFS_KNOWN_FAILURES="$(tables "$v")" \
        "$SCRIPT_DIR/run.sh" --variant "$v" --modules "$modules" || true
done

# knfsd_verdict PATTERN TSV — what knfsd did on the assertions a table row
# names: "fails too" if any of them failed, "passes" if they all passed, and
# "not reached" if the run produced none.
knfsd_verdict() {
    local pattern="$1" tsv="$2" verdict name seen=""
    while IFS=$'\t' read -r verdict name; do
        kf_match_pattern "$pattern" "$name" || continue
        [[ "$verdict" == PASS ]] || { echo "fails too"; return; }
        seen=1
    done <"$tsv"
    [[ -n "$seen" ]] && echo "passes" || echo "not reached"
}

OUT="$SCRIPT_DIR/baseline-knfsd.md"
{
    echo "# nfstest against Linux knfsd — baseline"
    echo ""
    echo "Generated by \`baseline-knfsd.sh\`. Do not hand-edit."
    echo ""
    echo "nfstest's \`dio\` and \`delegation\` modules mostly assert what the kernel"
    echo "client sends. This is the same run.sh run against the Linux kernel server on"
    echo "the same kernel: a row knfsd *fails too* is about the client, not DittoFS; a"
    echo "row knfsd *passes* is DittoFS-specific."
    echo ""
    echo "- Kernel: \`$(uname -r)\`"
    echo "- nfstest: \`$(git -C "$("$SCRIPT_DIR/../fetch.sh" nfstest)" rev-parse --short HEAD 2>/dev/null || echo unknown)\`"
    echo "- Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    for v in "${variants[@]}"; do
        tsv="$RESULTS_ROOT/$v/results.tsv"
        echo ""
        echo "## $v"
        echo ""
        if [[ ! -s "$tsv" ]]; then
            echo "No verdicts: the run did not produce a gradable log. See the logs in the CI artifact."
            continue
        fi
        echo "Passed $(grep -c '^PASS' "$tsv"), failed $(grep -vc '^PASS' "$tsv")."
        echo ""
        echo "DittoFS known-failure rows for these modules:"
        echo ""
        echo "| Row | knfsd |"
        echo "|-----|-------|"
        # shellcheck disable=SC2034  # KF_REASON is read by kf_reason
        KF_KNOWN=() KF_REASON=()
        IFS=':' read -ra tbls <<<"$(tables "$v")"
        for t in "${tbls[@]}"; do kf_load "$t"; done
        for pattern in "${!KF_KNOWN[@]}"; do
            [[ "$pattern" == dio/* || "$pattern" == delegation/* ]] || continue
            echo "| \`$pattern\` | $(knfsd_verdict "$pattern" "$tsv") |"
        done | sort
        echo ""
        echo "Assertions knfsd fails:"
        echo ""
        echo "| Assertion | On a DittoFS table |"
        echo "|-----------|--------------------|"
        grep -v '^PASS' "$tsv" | sort -u | while IFS=$'\t' read -r verdict name; do
            kf_is_known "$name" && known=yes || known=no
            echo "| \`$name\` ($verdict) | $known |"
        done
    done
} >"$OUT"

log "Wrote $OUT"
log "Logs: $RESULTS_ROOT"
