#!/usr/bin/env bash
# Exercise the grader with complete and interrupted prove output.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILURES=0

cat >"$WORK/known.md" <<'EOF'
| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| known/00.t | semantics | expected assertion failure | - |
EOF

run_case() {
    local name="$1" want="$2" message="$3" got
    cat >"$WORK/prove.log"
    "$SCRIPT_DIR/parse-results.sh" "$WORK/prove.log" "$WORK/known.md" \
        >"$WORK/output" 2>&1
    got=$?
    if [[ "$got" -ne "$want" ]] || ! grep -qF "$message" "$WORK/output"; then
        echo "FAIL: $name: exit $got, want $want; expected '$message'"
        cat "$WORK/output"
        FAILURES=$((FAILURES + 1))
    else
        echo "ok: $name"
    fi
}

run_case "complete pass" 0 "All tests successful" <<'EOF'
/fixture/tests/pass/00.t .. ok
All tests successful.
Files=1, Tests=2,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
Result: PASS
EOF

run_case "complete known failure" 0 "All failures are known" <<'EOF'
Test Summary Report
-------------------
/fixture/tests/known/00.t (Wstat: 0 Tests: 2 Failed: 1)
  Failed test: 2
Files=1, Tests=2,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
Result: FAIL
EOF

run_case "complete new failure" 1 "1 new failure(s)" <<'EOF'
Test Summary Report
-------------------
/fixture/tests/new/00.t (Wstat: 0 Tests: 2 Failed: 1)
  Failed test: 2
Files=1, Tests=2,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
Result: FAIL
EOF

# A known name must not excuse an incomplete TAP stream.
run_case "bad plan on known test" 1 "incomplete TAP" <<'EOF'
Test Summary Report
-------------------
/fixture/tests/known/00.t (Wstat: 0 Tests: 1 Failed: 0)
  Parse errors: Bad plan. You planned 2 tests but ran 1.
Files=1, Tests=1,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
Result: FAIL
EOF

run_case "missing plan" 1 "incomplete TAP" <<'EOF'
Test Summary Report
-------------------
/fixture/tests/new/00.t (Wstat: 0 Tests: 1 Failed: 0)
  Parse errors: No plan found in TAP output
Files=1, Tests=1,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
Result: FAIL
EOF

run_case "bailout" 1 "incomplete TAP" <<'EOF'
/fixture/tests/known/00.t ..
1..2
ok 1
Bail out! export disappeared
Bailout called. Further testing stopped: export disappeared
Test Summary Report
-------------------
/fixture/tests/known/00.t (Wstat: 0 Tests: 1 Failed: 0)
Files=1, Tests=1,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
Result: FAIL
EOF

run_case "summary truncated before totals" 1 "did not finish" <<'EOF'
Test Summary Report
-------------------
/fixture/tests/known/00.t (Wstat: 0 Tests: 2 Failed: 1)
EOF

run_case "summary truncated before result" 1 "did not finish" <<'EOF'
Test Summary Report
-------------------
/fixture/tests/known/00.t (Wstat: 0 Tests: 2 Failed: 1)
Files=1, Tests=2,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
EOF

run_case "success marker without footer" 1 "did not finish" <<'EOF'
All tests successful.
EOF

run_case "failure without a gradable test" 1 "no failing test files" <<'EOF'
Test Summary Report
-------------------
Files=1, Tests=1,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
Result: FAIL
EOF

echo "failures: $FAILURES"
[[ "$FAILURES" -eq 0 ]]
