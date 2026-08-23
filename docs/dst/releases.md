# DST release and porting contract

How godst tracks upstream Go releases, what it supports, how it is versioned,
and how a release is cut and distributed. Kept current; the Taskfile's `port`
task and the `.github/workflows` files are the executable forms of the
procedures below.

godst is **upstream Go plus DST patches**: an untagged build is the upstream
toolchain (design.md, "Untagged footprint (contract)"), so godst serves as a
consumer's primary `go` as well as the DST toolchain. That positioning drives
the patch cadence below — for a primary toolchain, an unported upstream
release is user-visible lag, security fixes included.

## Repository identity

godst lives at `github.com/greatliontech/godst` as a standalone repository —
upstream Go's full history is its history, but it is not a GitHub fork of
`golang/go` (no fork-network coupling: pull requests target godst, code
search works, the repository can be transferred or mirrored freely). The
`upstream` remote is `https://github.com/golang/go`, fetched one release tag
at a time (`git fetch upstream tag go1.N.M`). Every upstream release tag a
godst line has been based on is pushed to godst as well, so `git diff
go1.N.M main` — the complete dst delta — needs no second remote.

## Branches

- **`main`** — development. Its upstream base is the newest upstream minor
  godst supports. The base moves by **porting to an upstream release tag**
  (below), never by rebasing: `main` is append-only and is never
  force-pushed, so nothing downstream has to chase rewritten history and a
  port is an ordinary change set. Only upstream *tags* are bases — never a
  `release-branch.go1.N` tip — so every base is an upstream release.
- **`release-go1.N`** — the maintenance line of an upstream minor `main` has
  moved past. It is cut from `main` at the last `1.N`-based release tag when
  `main` moves to `1.N+1`. While upstream supports `1.N` it receives ports
  to upstream `go1.N.M` point tags and cherry-picks of dst **fixes** from
  `main` — correctness of the simulation contract (soundness, determinism,
  fences, crash and durability fidelity) and build breakage. Features and
  API additions land on `main` only. When upstream stops supporting `1.N`,
  the line is frozen: no further tags; the branch is kept. Operationally,
  the **live** maintenance line is the `release-go1.*` branch with the
  greatest minor (the support window holds exactly one besides `main`);
  every other `release-go1.*` branch is frozen.

On either branch, `git log --first-parent <base tag>..<branch>` is the
dst-only history since the base, and `git diff <base tag> <branch>` is the
full dst delta on that base — the port commit is built so that this holds
(see "Porting procedure": a port's tree is the new base plus the dst delta,
never git's three-way merge result).

## Support window

godst supports exactly the upstream minors upstream supports: the two most
recent majors. At any time that is `main` (newest) plus one `release-go1.N`
line. A consumer pinned to an older minor builds from the frozen branch's
tags; no further releases are cut for it.

**Patch cadence.** Every upstream release tag on a supported line is ported
(the porting procedure below) and released. The checkable form is a
stale-base refusal: **a line never cuts a release on a base whose upstream
minor has a newer point release** — the port lands before, or as, the line's
next release. Primary-toolchain consumers receive upstream fixes — security
patches included — only through a godst release, so the port of an upstream
point release is the line's highest-priority work from the moment the
upstream tag exists. The refusal is point-release-scoped by design: a new
upstream *minor* is adopted through the porting procedure's "new upstream
minor" steps (which themselves require the old minor's last point release to
be ported and released first), not through this refusal. The refusal makes
stale releases impossible but does not by itself make lag *observable* — a
line that cuts nothing is never refused — so its enforcement pairs with an
upstream tag watch that surfaces new point releases as they appear.
Enforced: the release workflow's gate refuses a tag whose base trails
upstream's newest final point release on the same minor, and the scheduled
`tagwatch` workflow goes red when any supported line's base trails one.
Both run the shared check in `.github/scripts/stale-base.sh` (runnable
locally with a base as its argument), which consults upstream's tags
directly, matches final releases only (rc/beta never), and **fails
closed**: an unanswerable question — a failed upstream query, an empty
match — refuses rather than passes, because a pass is what lets a release
ship without upstream's security patches.

## Versions and tags

A release is an annotated tag `goX.Y.Z-dst.N`:

- `goX.Y.Z` is the upstream base tag the release is built on.
- `N` is the dst release counter: **global and monotonic across all
  lines**. It is allocated at tag time as `1 + max` over every existing
  `*-dst.*` tag in the repository (all lines, all bases), so the counter
  orders releases totally — a larger `N` is a later cut wherever it lives —
  and the base prefix says which upstream line it is on. The release
  workflow rejects a tag whose `N` another tag already carries.

The `-dst.N` suffix is what `cmd/go`'s toolchain-name grammar accepts as a
custom-toolchain suffix (`gover.FromToolchain("go1.27.0-dst.7") == "1.27.0"`),
so the string is a valid toolchain name: a module's `go 1.27` directive is
satisfied by it under `GOTOOLCHAIN=local`.

Consumers reference **tags, never branches**.

## VERSION file

`VERSION` has two lines: the version string `goX.Y.Z-dst.N`, and `time
<RFC 3339>`, the cut time of the release the version string names (distpack
stamps it into every archive entry and the toolchain module's `.info`). At a
release commit both lines are the release's own. Between releases the file
names the line's current base and its **latest released** counter and time —
a build of an untagged commit therefore reports the most recent release
string plus whatever has landed since; only a tag is a release. A port sets
the version string to the new base with the line's latest released counter
(`go1.27.0-dst.6` directly after porting to `go1.27.0` on a line whose last
release was `dst.6`) and keeps the `time` line; the release commit updates
both.

A release-form version string is required (not `devel …`): `cmd/go` derives
a `devel` toolchain's local version as the bare language version `1.N`,
which a consumer module's `go 1.N.0` directive rejects under
`GOTOOLCHAIN=local`. The `-dst.N` suffix also keeps distinct checkouts from
cross-poisoning the shared build cache; the tool-ID mechanism, its
within-checkout stale-compiler trap, and the clean-cache rule are in
design.md "Enforcing test configurations".

## Porting procedure

The same procedure moves a line to an upstream point release and moves
`main` to a new minor; `task port BASE=go1.N.M` and `task port:check` are
its executable steps. A port is reviewed as a change set like any other,
and the review covers upstream's diff over the intercepted surface (step 4)
alongside the port's own delta.

A port commit is a **merge commit** — parents: the line's tip and the
upstream tag — whose **tree is the new base's tree plus the dst delta**. It
is not git's three-way merge: the line's previous base was an upstream
release branch carrying backports the new base may not have (a
`go1.26.7`-based line holds `release-branch.go1.26` commits absent from
`go1.27.0`), and a three-way merge keeps every such hunk the new base does
not overlap — godst would then ship a `1.27` toolchain containing patches
upstream `1.27` does not have, reported as dst delta. Building the tree from
the new base excludes them by construction.

1. `task port BASE=go1.N.M`: fetches the tag (only a tag can be a base),
   records the **old base** — the `VERSION` string minus its `-dst.N`
   suffix, read *before* the tree is touched — and then, in order: `git
   merge -s ours --no-ff --no-commit go1.N.M` (records the merge parent
   without touching the tree; a base that is already an ancestor leaves no
   `MERGE_HEAD` and is refused, so a port is never a single-parent commit),
   `git read-tree -u --reset go1.N.M` (the tree is now the new base), `git
   diff --binary <old base> <tip> | git apply --3way --index` (the dst delta
   re-applied; `--binary` so a binary path in the delta cannot abort the
   application half-way). `git apply` is atomic, so a dst-patched path
   upstream deleted or renamed between the bases would reject the whole
   patch: such paths are left out of the re-application and named — their
   hooks are re-homed by hand and acknowledged in step 3. The tooling
   word-splits its path lists and refuses, before touching anything, a
   dst-patched path containing whitespace, quotes, backslashes or glob
   characters (none exist in the Go tree today). The old base and the merge
   parent are recorded under `.git/` for step 3. Conflicts concentrate in the upstream files the
   fork patches (`proc.go`, `malloc.go`, `mgc*.go`, `chan.go`,
   `synctest.go`, `time/sleep.go`, `ssagen/ssa.go`, `cmd/go/internal/work`,
   `syscall_linux*.go`, …); the bulk of the delta is additive `dst_*` files
   upstream never touches. Resolve against the dst hooks the old delta shows
   for the file (`git diff <old base> <tip> -- <file>`). Format resolutions
   with the repo's own `bin/gofmt` — the system gofmt may disagree.
2. Resolve `VERSION` per the VERSION rule above (the builds below read it).
3. `task port:check`, after every conflict is resolved. It refuses a merge
   `task port` did not make (its record must name the current `MERGE_HEAD`),
   then enforces that **the delta survived** — the three-way application
   can resolve a hunk toward the new base with no conflict, so:
   (a) the path set of the new delta (`git diff --name-only` of the index
   against the new base) equals the old delta's (`git diff --name-only <old
   base> <tip>`); a difference is an upstream deletion/rename or a hook
   upstream has absorbed, acknowledged by name (`ALLOW='path …'`, echoed so
   the acknowledgement rides the port commit message; an entry that is not
   a difference is refused, so the record cannot name a fiction) — never by
   skipping the check; (b) for every dst-patched path upstream did **not** change
   between the bases, the re-applied content is byte-identical to the old
   tip (old and new base agree there, so any difference is a dropped or
   altered hook) — a deliberate re-derivation of a dst-only file, whose hook
   follows a change upstream made in another file, is acknowledged by name
   (`ALTERED='path …'`, echoed, stale entries refused) and explained in the
   port commit message; the dst-patched paths upstream did change are listed —
   those are the hooks hand-reviewed against the new base, and the only
   place a silent drop can still hide; and (c) **both modes build** — `go
   build std` and `go build -tags dst std`: make.bash and the untagged build
   never compile the `dst_*` files, so an upstream signature change that
   breaks only a tagged file (a `dst && unix` file no untagged build
   touches) is invisible until the tagged build runs.
   The same delta-survival pair is re-verifiable long after the port
   landed: `task port:verify REF=<merge>` recovers everything from the
   commit graph — the old tip is the merge's first parent, the old base
   that parent's `VERSION` minus its suffix, and the commit's own `VERSION`
   must name the second parent, so a hand-made merge whose recorded base
   and actual base disagree is refused — and re-runs checks (a) and (b)
   with the acknowledgements re-supplied as `ALLOW`/`ALTERED` (the port
   commit message records them). Builds are deliberately absent from the
   post-commit form: they belong to the branch's ci/matrix runs.
4. **Port audit** — a port re-derives the model against the new base; it
   is not "the old code compiles against the new tree". Walk upstream's
   change set between the bases (`git log`/`git diff <old base>..<new
   base>` over the surface the simulation intercepts or models: the
   runtime's scheduler, timers, GC, map hashing, `rand`, `sync`,
   `synctest`; `net`, `os`, `syscall`, `time`, `crypto/rand`, `testing`;
   design.md "Nondeterminism sources and who owns them" and "The
   interception boundary" enumerate the owned surface) and upstream's
   release notes — the mechanical half of the walk is fixed and scripted:
   `task port:audit REF=<merge>` prints, post-commit and reproducibly,
   upstream's new `//go:linkname` directives over the intercepted surface
   (a push-linkname export of an unfenced entry is how a bubble acquires a
   host read the trampolines never see; `task port:check` prints the same
   list at port time), the diff STAT of the generated syscall tables
   (`zsyscall_*`, `zsysnum_*`, `ztypes_*` — pointing at the diffs to read)
   and the changed lines of the `GOEXPERIMENT`/`GODEBUG` defaults, the
   changed files over the owned surface, and the source-class hits in
   upstream's ADDED lines over those files (`time.Now`, `nanotime`,
   `cputicks`, `runtime_rand`, `getrandom`, `urandom`, `rdrand`, `Getenv`,
   `Environ`); the walk additionally covers what no grep can — goroutine
   ids, addresses as ordering keys, and moves of the functions and fields
   the hooks live in — and dispositions every relevant change as one of:
   *no dst impact* (stated, with the reason); *hook re-derived* — the hook
   is rewritten from upstream's new mechanism (a replaced bubble
   implementation means the drain hooks are redesigned against the new
   bubble model, not patched until they compile); or *new interception
   needed* — upstream added a nondeterminism source, an I/O or syscall path,
   or a public surface the model does not cover: a fix commit on top of the
   port, with the enforcing suites and any generator grammar extended to
   cover it, or an issue doc when it needs its own design. The disposition
   list rides the port commit message. The matrix (step 6) is necessary,
   not sufficient: it proves the existing contract survived, not that the
   new base's additions are modeled.
5. Commit the merge. Rebuild the toolchain and clean the build cache
   (upstream may have touched cmd/compile; the tool-ID trap makes a stale
   compiler silently pass).
6. Run the full enforcing matrix. A red leg blocks the port; fixes land as
   their own commits on top of the merge.
7. Push the port commit and the base tag together (`git push origin <line>
   tag go1.N.M`), so `git diff go1.N.M <line>` works in every clone.

The real port gate is steps 4 and 6, not git: the audit and the enforcing
legs are what proves the determinism contract survived the new base.

**New upstream minor** (`go1.N+1.0` tagged; `main` is on `1.N`): first bring
`main` to the latest `go1.N.M` by this procedure and release it; cut
`release-go1.N` at that tag; then port `main` to `go1.N+1.0` and release.
The maintenance line thus starts at a validated, released state.

## Cutting a release

1. The full enforcing matrix (design.md "Enforcing test configurations":
   `task test`, `task test:inert-std`, `task test:inert-diff`,
   `task test:cross`) is green on the
   candidate commit, as a run of the `matrix` workflow on that commit. A red
   leg blocks.
2. The release commit is made on top of the candidate and changes only
   `VERSION` (counter allocated per "Versions and tags", `time` = cut
   time). A `VERSION`-only change cannot alter the matrix's verdict, which
   is why the gate binds to the parent.
3. `git tag -a goX.Y.Z-dst.N` on the release commit; push the tag.
4. The tag push triggers the `release` workflow (below); the release exists
   when its assets are attached. A tag whose workflow fails is not a
   release: fix forward, cut the next counter.

## Continuous integration

Workflows under `.github/workflows`:

- **`ci`** — on pull requests and pushes to `main` and `release-go1.*`:
  build the toolchain, both-mode `std` builds, the per-PR legs
  (`test:untagged`, `test:cross`, `test:api`; `test:testdir`, `test:cmd`,
  and `test:inert-std` — the cmd and breadth legs covering the fork's
  toolchain changes and untagged std; and `test:inert-diff` —
  the INV-VANILLA differential gate against the upstream base toolchain,
  which the workflow installs), and a tree check that
  no build artifact is committed: no tracked linked executable (ELF
  `ET_EXEC`/`ET_DYN`, Mach-O executable, PE image) outside a `testdata`
  directory — upstream's tracked `.syso` objects are relocatable, not
  linked, and its executable fixtures live under `testdata`. It is a smoke
  gate, not the release gate.
- **`matrix`** — the full enforcing matrix: scheduled nightly on `main` and
  on the live `release-go1.*` line (the scheduled run on `main` dispatches
  the workflow on that line, so the line gets a run on its own tip),
  dispatchable on any ref (a dispatch runs on the ref's tip; the release
  candidate is the line's tip, so dispatching on the line covers it), and
  triggered by pushes to `port-*` branches — a convenience convention for
  preparing a port on a branch; the procedure does not mandate one, and
  step 6 also runs by dispatching on any ref. A red
  nightly on a line blocks the line's next release until green.
- **`tagwatch`** — scheduled daily: goes red when any supported line's base
  trails an upstream final point release on its minor (the observability
  half of the Patch cadence's stale-base refusal; the refusal itself lives
  in the release gate). Red here means the port is that line's
  highest-priority work.
- **`port-rehearsal`** — dispatched with an upstream release tag: runs
  `task port BASE=<tag>` on a throwaway workspace and reports what the real
  port will face — the conflict inventory when the re-application
  conflicts, or the built-toolchain `port:check` acknowledgement inventory
  when it is clean (reported, not gating). Nothing is pushed; the rehearsal
  prices a port before anyone starts it.
- **`release`** — on a `go*-dst.*` tag: verifies that the tag is annotated,
  that `VERSION` equals the tag, that no other tag carries the same `N`,
  that the release commit changes only `VERSION` (the premise that lets the
  gate bind to the parent), and that a successful `matrix` run exists for
  the release commit's parent; then, on a native runner per published
  platform, builds that platform's distribution with upstream's own
  packaging (`make.bash -distpack` — identical layout and naming to
  upstream's downloads), validates the EXTRACTED binary tarball — the
  artifact consumers download, not the checkout it was packed from, so a
  distpack packaging omission fails the release: the Taskfile `verify:dist`
  task (the extracted `bin/go` reports the tag and builds and tests an
  untagged module, invoked the way a consumer invokes it — no `GOROOT`, in
  a hermetic environment per the cache paragraph below) and the
  `smoke:dist` task (tagged `-short`
  `testing/simulation` from the extraction's own source tree, proving the
  `dst_*` files rode the tarball) — and publishes a GitHub
  Release with the assets below — the source tarball once (the platforms'
  copies must be identical), each platform's binary tarball and module
  files, `SHA256SUMS`. The release workflow enforces the gate; it is not
  itself the gate. It can be **rehearsed**: dispatched on an existing
  `go*-dst.*` tag it runs the gate and the builds and publishes nothing, so
  a change to the release workflow itself is proven on a tag that already
  exists before a counter is spent on it (the checked-out tree — composite
  action, Taskfile, scripts — is the tag's, so only `release.yml` is
  exercised at the dispatched ref).

Every workflow builds the toolchain from source on a fresh runner and never
restores a Go build cache across commits: this fork's release-form version
string makes tool IDs version-derived (design.md "Enforcing test
configurations"), so a cached object built by a different compiler at the
same version string would be served silently. The extracted-distribution
validation goes further and runs with its own empty `GOCACHE`: the
extraction's tools are indistinguishable from the checkout's to any shared
cache (`GOROOT` package directories are deliberately not hashed into action
IDs), so a shared cache would serve checkout-built objects and never execute
the extraction's compiler — vacuously passing exactly the packaging faults
the validation exists to catch. The Taskfile is the single source of how the
built toolchain is built against and tested; workflows build and test
through its tasks (`build:both`, the legs, `dist:extract`, `verify:dist`,
`smoke:dist`) rather than open-coding `go build`/`go test` commands.

## Distribution

Each release carries, as GitHub Release assets:

- `goX.Y.Z-dst.N.src.tar.gz` — the source distribution;
- `goX.Y.Z-dst.N.<os>-<arch>.tar.gz` — binary distributions for every
  platform the simulation supports and godst publishes: `linux-amd64` and
  `linux-arm64` (other architectures in design.md's supported scope build
  from source);
- the toolchain module files distpack emits
  (`v0.0.1-goX.Y.Z-dst.N.<os>-<arch>.{zip,info,mod}`), so a module proxy
  serving `golang.org/toolchain` can offer godst to the go command's
  `GOTOOLCHAIN`/`toolchain`-directive download path — not operated today,
  not foreclosed;
- `SHA256SUMS`.

A binary tarball extracts to `go/` (upstream's layout). Consumers run the
extracted `go/bin/go` with `GOTOOLCHAIN=local` and `-tags dst`; the asset
names and the tag are the consumer contract.

## Configurations the matrix does not cover

`GOEXPERIMENT=staticlockranking` is incompatible with `-tags dst`. Starting a
simulation aborts with `not holding required lock! (rank sched)` from
`schedEnabled`, reached when the background mark workers start: the simulation
suspends the garbage collector and drives the scheduler in ways the static rank
assertions do not expect. The matrix therefore does not run that experiment,
and code written for the simulation cannot rely on it to audit lock order.

Fork code must still respect the rules lock ranking would enforce — above all,
never allocate or park while holding a runtime `mutex`, since `mallocgc` may
take `mheap_.lock` beneath a leaf-ranked lock or assist the collector. Prefer
the lock-free copy-on-write shape (`runtime.dstSetPidLive`,
`runtime.dstSpanAdd`): build the successor, then compare-and-swap it in.
