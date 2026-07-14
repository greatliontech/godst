# Untagged `go test net` does not compile (dst-only symbols in untagged test files)

`go test net` / `go vet net` WITHOUT `-tags dst` fails to build the test
package: `net/dst_latency_test.go` (and, following its convention,
`net/dst_reset_test.go`'s white-box wire tests) reference dst-only symbols
(`dstConnectSYN`, `dstNewBaseTimer`, `dstBaseNanos`, `dstWirePair`) from
files carrying no `//go:build dst` tag. The convention elsewhere in the
package is tagless test files guarded by runtime `dstNetEnabled` skips —
which only works when every referenced symbol exists untagged (via
`dst_off.go` stubs). No Taskfile leg compiles untagged net tests
(`test:untagged` runs runtime only; `test:inert-std` builds std packages,
not test binaries), so nothing gates on this; it surfaces only when a
developer runs untagged `go test net` by hand.

Resolution is a convention call: either tag the white-box dst test files
`//go:build dst`, or add untagged stubs for the referenced symbols, and add
whichever is chosen to a leg so it stays enforced.

Lands: 7
