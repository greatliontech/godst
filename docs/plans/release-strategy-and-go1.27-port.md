# Plan: release strategy cut-over and the Go 1.27 port

Spec: `docs/dst/releases.md` (branches, support window, versioning, porting
procedure, release gate, CI, distribution); `docs/dst/design.md` "Enforcing
test configurations" (the matrix).

- [x] 1. Release contract: rewrite `docs/dst/releases.md` to the merge model
      with by-construction port trees, global release counter, standalone
      `greatliontech/godst` identity, CI and distribution contract; Taskfile
      `port`/`port:check` tasks become the executable port steps; README gains the
      `GOTOOLCHAIN=local` consumer rule and the releases pointer.
- [x] 2. History cleanup: drop the committed `compile` binary from the commits
      that carry it (no tag contains it; one last force-push under the old
      contract's allowance); delete the merged/superseded local and remote
      branches (`dst-127base`, `dst-audit-fixes`, `dst-disk-corrupt`,
      `dst-fallocate`, `dst-mmu-mappings`, `master`); `provenance` stays
      pending the user's call.
- [ ] 3. Consumer-neutral tree: godst carries no reference to any particular
      consumer — rename `src/testing/simulation/gmdb_compat_linux_test.go` and
      reword its doc comment, the two comment mentions in
      `src/os/dst_proc.go` / `src/os/dst_fd_linux_test.go`, and
      `docs/dst/design.md` "durable image" line to describe the modeled
      recovery surface on its own terms.
- [ ] 4. Repository cut-over: create `greatliontech/godst` (standalone, public);
      re-create the lightweight release tags (`go1.26.5-dst.3`,
      `go1.26.5-dst.6`) as annotated on the same commits; push the cleaned
      branch as `main` plus all tags (dst releases and the upstream tags
      reachable from it); set `main` as default; move the local clone to
      `~/repos/github.com/greatliontech/godst` with `origin` → godst,
      `upstream` → golang/go; archive `thegrumpylion/go` with a pointer (user
      action).
- [ ] 5. CI workflows: `.github/workflows/ci.yml` (toolchain build, both-mode
      `std` builds, `test:untagged`, `test:cross`, committed-artifact tree
      check on PRs and pushes), `matrix.yml` (full matrix; nightly on `main`
      and `release-go1.*`, `workflow_dispatch` on any ref), `release.yml`
      (tag-triggered: VERSION==tag, unique `N`, green `matrix` run on the
      release commit's parent; native-runner jobs for linux-amd64 and
      linux-arm64 running `make.bash -distpack` and the tagged `-short` smoke;
      GitHub Release with tarballs, toolchain module files, `SHA256SUMS`).
- [ ] 6. Maintenance baseline: `task port BASE=go1.26.7` on `main`;
      `task port:check`; port audit; full matrix; release `go1.26.7-dst.7` (first release through the
      new pipeline); cut `release-go1.26` at the tag.
- [ ] 7. Port `main` to Go 1.27: `task port BASE=go1.27.0`; resolve the
      overlapping upstream files; `task port:check`; port audit over
      upstream's 1.26.7→1.27.0 changes to the intercepted surface with every
      disposition in the port commit message; `task compiler`; full matrix
      with fixes as follow-on commits; release `go1.27.0-dst.8`.
