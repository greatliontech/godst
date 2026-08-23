# dst-only exports are outside the exported-API gate

The exported-API gate (`cmd/api -check`, `api/go1-godst.txt`) walks
untagged build contexts, so dst-only exports are invisible to it. The
current tagged surface (`HostFS`, and `HostIP`'s tagged twin, which could
drift from the untagged one the api file pins) is signature-pinned by
compile-time assertions in `testing/simulation/api_pin_test.go`, but an
ADDED dst-only export or twin lands unpinned until hand-added there.

Closing this properly means teaching `cmd/api` a dst-tagged build context
(its walker takes a fixed context list) — its own design against upstream
test code.

The import-policy half this issue originally carried is CLOSED as
disproven: `deps_test`'s `findImports` reads every non-ignore file in a
package regardless of build constraints, and an out-of-policy import in a
`//go:build dst` file fails `TestDependencies` (verified: `net/http` in
`hostfs.go` → "unexpected dependency"). A new dst-only std package fails
loudly too: `HasEdge` returns false for a package absent from `depsRules`.
Scope bounds of that closure: `listStdPkgs` skips `cmd/`, `testdata/`, and
`_`/`.`-prefixed directories, so dst files there (today: the
`runtime/testdata/testprog*` helpers; no dst file exists under `src/cmd`)
are outside the import gate.

Lands: user decision.
