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
# test/nfs-conformance/pynfs gives: knfsd's sockets belong to the kernel. On a
# host without nfsd, dispatch the nfs-knfsd-baseline workflow instead.
#
# Not a gate: it measures a third-party server.
#
# Usage:
#   sudo ./baseline-knfsd.sh [--variants v4.0,v4.1] [--export-dir DIR] [--keep]
#
#   ./baseline-knfsd.sh --report-only RESULTS_DIR
#
# --export-dir must not exist yet: the script creates it, makes it
# world-writable and deletes it on exit.
#
# --report-only rewrites baseline-knfsd.md from an earlier run's results
# directory (results/baseline-knfsd-<time>/, or the uploaded artifact) without
# running anything, so the report follows a known-failure table edit without
# another run. It needs neither Linux nor root.

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
REPORT_ONLY=""

log() { echo "[BASELINE] $*"; }
die() { echo "[BASELINE] $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
    --variants) VARIANTS="${2:?--variants requires a value}"; shift 2 ;;
    --export-dir) EXPORT_DIR="${2:?--export-dir requires a value}"; shift 2 ;;
    --keep) KEEP=true; shift ;;
    --report-only) REPORT_ONLY="${2:?--report-only requires a results directory}"; shift 2 ;;
    -h | --help) sed -n '2,39p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
    esac
done

command -v jq >/dev/null || die "jq is required to read test/conformance/suites.json"

# tables VARIANT — the known-failure tables the manifest gives VARIANT,
# colon-separated, exactly as test/conformance/run.sh hands them to run.sh.
tables() {
    jq -r --arg v "$1" --arg d "$TEST_DIR" '.suites.nfstest.known_failures[$v] | map($d + "/" + .) | join(":")' \
        "$TEST_DIR/conformance/suites.json"
}

IFS=',' read -ra variants <<<"$VARIANTS"
for v in "${variants[@]}"; do
    option_set_opts "$v" >/dev/null || die "unknown option set: $v"
done

# run_knfsd — export a directory from knfsd and run every option set against
# it, leaving results.tsv per set and a "meta" file describing the run under
# RESULTS_ROOT.
run_knfsd() {
    if [[ "$(uname -s)" != Linux ]]; then
        echo "[BASELINE] The baseline needs the in-kernel nfsd, so it only runs on Linux." >&2
        die "Elsewhere, dispatch it: gh workflow run nfs-knfsd-baseline.yml"
    fi
    [[ $EUID -eq 0 ]] || die "must run as root (sudo): it exports a filesystem and mounts it"
    [[ ! -e "$EXPORT_DIR" ]] || die "$EXPORT_DIR already exists; pass an --export-dir that does not, since it is deleted on exit"

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

    {
        echo "kernel=$(uname -r)"
        echo "nfstest=$(git -C "$("$SCRIPT_DIR/../fetch.sh" nfstest)" rev-parse --short HEAD 2>/dev/null || echo unknown)"
        echo "run=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    } >"$RESULTS_ROOT/meta"

    local v modules
    for v in "${variants[@]}"; do
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
}

if [[ -n "$REPORT_ONLY" ]]; then
    [[ -f "$REPORT_ONLY/meta" ]] || die "$REPORT_ONLY has no meta file; is it a baseline results directory?"
    RESULTS_ROOT="$(cd "$REPORT_ONLY" && pwd)"
else
    RESULTS_ROOT="$SCRIPT_DIR/results/baseline-knfsd-$(date +%Y%m%d-%H%M%S)"
    mkdir -p "$RESULTS_ROOT"
    run_knfsd
fi

# meta KEY — one field of the run's meta file.
meta() { sed -n "s/^$1=//p" "$RESULTS_ROOT/meta"; }

# knfsd_verdict PATTERN TSV — what knfsd did on the assertions a table row
# names: "fails too (N of M)", "passes (M)", or "not reached" if the run
# produced none. A row's glob can cover assertions knfsd passes as well as ones
# it fails, so the counts say how much of the row the verdict speaks for. When
# the row's module did not finish, the later assertions it names never ran,
# and the verdict says so.
knfsd_verdict() {
    local pattern="$1" tsv="$2" verdict name failed=0 total=0 out
    while IFS=$'\t' read -r verdict name; do
        kf_match_pattern "$pattern" "$name" || continue
        total=$((total + 1))
        [[ "$verdict" == PASS ]] || failed=$((failed + 1))
    done <"$tsv"
    if [[ $total -eq 0 ]]; then
        out="not reached"
    elif [[ $failed -gt 0 ]]; then
        out="fails too ($failed of $total)"
    else
        out="passes ($total)"
    fi
    grep -qE "^(TIMEOUT|INCOMPLETE)"$'\t'"${pattern%%/*}:" "$tsv" && out+=", module did not finish"
    echo "$out"
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
    echo "- Kernel: \`$(meta kernel)\`"
    echo "- nfstest: \`$(meta nfstest)\`"
    echo "- Run: $(meta run)"
    for v in "${variants[@]}"; do
        tsv="$RESULTS_ROOT/$v/results.tsv"
        echo ""
        echo "## $v"
        echo ""
        if [[ ! -s "$tsv" ]]; then
            echo "No verdicts: the run did not produce a gradable log. See \`$v/nfstest.log\` in the run's results."
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
