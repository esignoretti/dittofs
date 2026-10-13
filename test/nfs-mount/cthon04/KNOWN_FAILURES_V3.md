# cthon04 known failures — NFSv3 overlay

Failures expected under the `v3` option set only, graded together with `KNOWN_FAILURES.md`.

`run.sh` grades every run against this table through `test/common/known-failures.sh`: a failure listed here is reported but does not fail CI, and any failure not listed does. A listed test that passes is reported as "consider removing". Which tables grade which option set is declared in `test/conformance/suites.json`.

cthon04 is graded per program (`basic/test6`, `special/telldir`), not per assertion, so a row excuses any failure of that program, not only the one its reason documents. Shell globs work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| basic/test6 | semantics | NFSv3 READDIR returns no "." and ".." entries; knfsd returns both and the Linux client passes the server's list through as-is | [#2978](https://github.com/marmos91/dittofs/issues/2978) |
| special/op_chmod | bug | a WRITE flushed after the file's owner chmods it to 0 is refused; NFSv3 has no open state, and knfsd lets the owner through regardless of mode. Passes on the v4 sets | [#3097](https://github.com/marmos91/dittofs/issues/3097) |
