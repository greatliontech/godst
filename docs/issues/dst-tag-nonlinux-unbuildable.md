# `-tags dst` does not build on non-linux GOOS — plain-`dst` files reference linux-only helpers

Pre-existing at go1.27.0-dst.10 (verified at 291ca747):
`GOOS=js GOARCH=wasm go build -tags dst os` fails with
`dst_fs.go: undefined: dstNodeContains`,
`dst_host_crash.go: undefined: dstFutexTeardownHost`,
`dst_process.go: undefined: dstFutexTeardownProc` — plain-`dst` files
call helpers declared under NARROWER constraints than their callers:
the futex teardown pair lives in `dst_futex_linux.go` (`dst && linux`)
while `dstNodeContains` lives in `dst_root.go`
(`dst && (unix || wasip1)`), so js/wasm fails on both classes and a
linux-only framing would misdescribe the second (and a fix that pinned
everything to linux would break the wasip1 support the root helpers
carry today). No gate covers tagged cross-builds (test:cross builds
untagged only), so the state-move change set briefly widened the
breakage without any leg noticing.

The fix is a build-tag alignment sweep: give every plain-`dst` sim file
a constraint no wider than its callees' union (with stubs where the
untagged-caller surface needs symbols), or record the supported tagged
platform set in design.md and align the tags to it — and either way
test:cross grows a tagged cross-build leg as the enforcement half.

Lands: with the next port or build-surface change set that touches the
dst build-tag matrix; a tagged cross-build leg in CI is the enforcement
half either way.
