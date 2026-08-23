# Tagged build contexts are outside two std gates (import policy, exported API)

`go/build`'s `TestDependencies` (`deps_test.go`) — the std import-policy gate
the fork extends for `testing/simulation` — evaluates untagged build contexts
only. An import added in a `//go:build dst` file therefore bypasses the
policy: a dst-tagged std package could acquire an out-of-policy dependency
and no gate would notice before hand review.

The exported-API gate (`cmd/api -check`, `api/go1-godst.txt`) has the same
scope: dst-only exports are invisible to it. The one dst-only export (`HostFS`) and the tagged twin of `HostIP` are
signature-pinned by compile-time assertions in `testing/simulation`'s tagged
tests, but an ADDED dst-only export or twin lands unpinned until hand-added
there.

Lands: 9 (primary-go plan) — the coverage-legs chunk dispositions whether a
tagged-context evaluation of the dependency policy joins the untagged one.
