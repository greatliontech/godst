# DST release and porting contract

How this fork tracks upstream Go releases. Kept current; the Taskfile's `port`
task is the executable form of the procedure below.

## Branches

- **`dst`** — the active development branch. It is a linear stack of dst
  commits rebased directly onto the tip of the **newest upstream release
  branch the fork supports** (`upstream/release-branch.go1.N`). It may be
  force-pushed (a rebase rewrites it); nothing downstream may pin it — see
  Tags.
- **`dst-go1.N`** — one frozen maintenance branch per upstream minor, created
  when development moves to `1.N+1`: it points at the last validated
  `1.N`-based state of the stack. It receives upstream `release-branch.go1.N`
  backports (by the same rebase procedure) and critical dst fixes
  cherry-picked from `dst`, only for as long as a consumer actually pins that
  minor — not upstream's full support window by default.

## Tags

`goX.Y.Z-dst.N` — upstream base version plus a dst release counter, cut ONLY
after the full enforcing matrix passes (the four Taskfile legs, the
cross-compile targets, and the 802-program sweep the `test:dst` leg contains).
Consumers (gmdb CI and any other toolchain pin) reference **tags, never
branches**: tags survive rebases, so force-pushes on `dst` cannot break a
consumer.

## VERSION string

The `VERSION` file carries `goX.Y.Z-dst.N`, matching the tag. The `-dst.N`
suffix keeps distinct checkouts from cross-poisoning the shared build cache
(tool IDs derive from the version string — see design.md "Enforcing test
configurations"). It does NOT fix the within-checkout stale-compiler trap (a
suffixed release is still a release to the tool-ID logic), so the
clean-cache-after-compiler-change rule stands unchanged.

## Porting procedure (minor bump and new-minor port alike)

1. `git fetch upstream`.
2. `git rebase --onto upstream/release-branch.go1.NEW $(git merge-base HEAD
   upstream/release-branch.go1.CUR)` — replay exactly the dst stack, never
   upstream's own backports. `rerere` is enabled repo-locally, so conflict
   resolutions replay across repeated rebases; conflicts concentrate in the
   handful of shared upstream files the fork patches (`proc.go`, `select.go`,
   `chan.go`, `synctest.go`, `syscall_linux.go`, `file_unix.go`, …) — the bulk
   of the stack is additive `dst_*` files upstream never touches. At every
   conflict stop AND at the rebased tip, build BOTH modes — `go build std`
   and `go build -tags dst std`: make.bash and the untagged build never
   compile the `dst_*` files, so an upstream signature change breaking only a
   tagged consumer is invisible until the tagged build runs (the 1.26.5 port
   hit exactly this: `splitPathInRoot`'s return-type change broke a
   `dst && unix`-only file no untagged build touches). Format resolutions
   with the repo's own `bin/gofmt` — the system gofmt may disagree.
3. Resolve the `VERSION` conflict to the new base + `-dst.1`.
4. Rebuild the toolchain and clean the build cache (upstream may have touched
   cmd/compile; the tool-ID trap makes a stale compiler silently pass).
5. Run the full enforcing matrix. A red leg blocks the port.
6. Tag `goX.Y.Z-dst.1`. For a NEW minor, first snapshot the old state as
   `dst-go1.CUR`.

The real port gate is step 5, not git: the enforcing legs are what proves the
determinism contract survived the new base.
