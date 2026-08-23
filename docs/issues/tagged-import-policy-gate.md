# Import-policy gate does not cover dst-tagged build contexts

`go/build`'s `TestDependencies` (`deps_test.go`) — the std import-policy gate
the fork extends for `testing/simulation` — evaluates untagged build contexts
only. An import added in a `//go:build dst` file therefore bypasses the
policy: a dst-tagged std package could acquire an out-of-policy dependency
and no gate would notice before hand review.

Lands: 9 (primary-go plan) — the coverage-legs chunk dispositions whether a
tagged-context evaluation of the dependency policy joins the untagged one.
