#!/usr/bin/env bash
# Exercise the actual graders across the eight-bit exit-status boundary.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILURES=0

command -v xmlstarlet >/dev/null 2>&1 || {
    echo "xmlstarlet is required to test the WPTS TRX grader" >&2
    exit 1
}

cat >"$WORK/known.md" <<'EOF'
| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| KNOWN | semantics | expected failure | - |
| known/00.t | semantics | expected failure | - |
EOF

for count in 0 1 254 255 256 257 512; do
    expected="$count"
    [[ "$count" -le 254 ]] || expected=254

    # Include a pass and a known failure in every fixture, so the requested
    # count must be the NEW failures rather than the total number of results.
    {
        echo '**************************************************'
        echo 'PASS1 st_sample.pass : PASS'
        echo 'KNOWN st_sample.known : FAILURE'
        for ((i = 1; i <= count; i++)); do
            echo "NEW$i st_sample.new$i : FAILURE"
        done
        echo '**************************************************'
        echo "Of those: 0 Skipped, $((count + 1)) Failed, 0 Warned, 1 Passed"
    } >"$WORK/pynfs.log"

    {
        echo 'Test Summary Report'
        echo '-------------------'
        echo '/fixture/tests/known/00.t (Wstat: 0 Tests: 1 Failed: 1)'
        for ((i = 1; i <= count; i++)); do
            echo "/fixture/tests/new/$i.t (Wstat: 0 Tests: 1 Failed: 1)"
        done
        echo "Files=$((count + 2)), Tests=$((count + 2)), 0 wallclock secs (0.01 CPU)"
        echo 'Result: FAIL'
    } >"$WORK/prove.log"

    {
        echo '<TestRun xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010"><Results>'
        echo '<UnitTestResult testName="PASS1" outcome="Passed"/>'
        echo '<UnitTestResult testName="KNOWN" outcome="Failed"/>'
        for ((i = 1; i <= count; i++)); do
            echo "<UnitTestResult testName=\"NEW$i\" outcome=\"Failed\"/>"
        done
        echo '</Results><ResultSummary>'
        echo "<Counters total=\"$((count + 2))\" passed=\"1\" failed=\"$((count + 1))\" error=\"0\" timeout=\"0\" aborted=\"0\" notExecuted=\"0\"/>"
        echo '</ResultSummary></TestRun>'
    } >"$WORK/wpts.trx"

    for suite in pynfs posix wpts; do
        case "$suite" in
            pynfs) parser="$TEST_DIR/nfs-conformance/pynfs/parse-results.sh"; input="$WORK/pynfs.log" ;;
            posix) parser="$TEST_DIR/posix/parse-results.sh"; input="$WORK/prove.log" ;;
            wpts) parser="$TEST_DIR/smb-conformance/parse-results.sh"; input="$WORK/wpts.trx" ;;
        esac
        "$parser" "$input" "$WORK/known.md" >"$WORK/raw" 2>&1
        got=$?
        sed $'s/\033\[[0-9;]*m//g' "$WORK/raw" >"$WORK/output"
        if [[ "$got" -ne "$expected" ]] ||
            ! grep -qiE "New failures:[[:space:]]+$count$" "$WORK/output" ||
            ! grep -qiE 'Known failures:[[:space:]]+1$' "$WORK/output"; then
            echo "FAIL: $suite with $count new failures: exit $got, want $expected; exact counts must survive"
            tail -12 "$WORK/output"
            FAILURES=$((FAILURES + 1))
        else
            echo "ok: $suite with $count new failures (exit $got, full count retained)"
        fi
    done
done

echo "failures: $FAILURES"
[[ "$FAILURES" -eq 0 ]]
