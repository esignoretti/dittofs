# nfstest known failures — NFSv4 overlay

Failures expected under every v4 option set (`default`, which runs NFSv4.2, and `v4.0`, `v4.1`, `v4.1-noac`, `v4.1-smallio`), graded together with `KNOWN_FAILURES.md`.

`run.sh` grades every run against this table through `test/common/known-failures.sh`: a failure listed here is reported but does not fail CI, and any failure not listed does. A listed test that passes is reported as "consider removing". Which tables grade which option set is declared in `test/conformance/suites.json`.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| delegation/basic04: DELEGRETURN should be sent after the close | suite | write delegation + stat: the client keeps the delegation past close; knfsd fails it too on the same kernel (`baseline-knfsd.md`, `6.17.0-1022-azure`) | - |
| delegation/basic10: DELEGRETURN should be sent after the close | suite | as basic04, on a read-write open; knfsd fails it too on the same kernel (`baseline-knfsd.md`, `6.17.0-1022-azure`) | - |
| delegation/basic0[369]: DELEGRETURN should be sent after the close | bug | the client does not return the delegation after close; knfsd passes these on the same kernel | [#3096](https://github.com/marmos91/dittofs/issues/3096) |
| delegation/basic02: DELEGRETURN should be sent after the close | unverified | `v4.1-noac` and `v4.0` only; knfsd fails it too under noac and grants no write delegation on v4.0, so not yet sorted | [#3096](https://github.com/marmos91/dittofs/issues/3096) |
| delegation/basic*: CLOSE should be sent to the server | suite | under `v4.1-noac` the CLOSE is not in the capture; knfsd fails it too on the same kernel (`baseline-knfsd.md`, `6.17.0-1022-azure`) | [#3019](https://github.com/marmos91/dittofs/issues/3019) |
