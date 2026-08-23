# gomutant cannot load the fork's std tree

`gomutant discover` over this repository reports zero targets, and
`ephemeral` refuses `os` as "not a loaded package import path": the server's
package load runs under a host toolchain that cannot load the fork's std
module (`src/go.mod` requires go >= 1.27; the host resolves an older
toolchain — the same failure the editor's gopls shows). Until the server
loads std packages with the fork's own `bin/go` (GOROOT at the repo root,
GOTOOLCHAIN=local), every mutation probe and campaign against fork code
falls back to hand edits with the staged copy as the safety net, and
reviewer hand-probes — which run only through `gomutant ephemeral` — are
unavailable entirely.

Lands: when `gomutant discover` reports non-zero targets for the fork's std
tree. (The fix itself is gomutant-side — configuration or support for a
self-hosted GOROOT, outside this repository — but the landing condition is
checkable here in one command.)
