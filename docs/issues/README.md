# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition, or
"pending feature" for planned roadmap work). At close-out, lasting rationale is
promoted into a kept-current artifact and the resolved entry is deleted.

## Open

- [reset-backlog-conn-accept-handout.md](./reset-backlog-conn-accept-handout.md) —
  a fault-injected reset of a conn still queued in the accept backlog leaves
  `acceptState == 0`, so a later Accept can hand the torn-down conn out
  (first read `ECONNRESET`) — either kernel-legal (then the `acceptState`
  comment's never-handed-out claim over-promises) or a missed `0→2` claim on
  the fault path; pre-existing before the kernel-faithful reset rework.
  Lands: 7
- [untagged-net-test-compile.md](./untagged-net-test-compile.md) —
  untagged `go test net` fails to compile: dst-only symbols referenced from
  untagged test files (`dst_latency_test.go` at HEAD, and the white-box wire
  tests following its convention); no leg gates on it. Convention call:
  build-tag the files or stub the symbols. Lands: 7
