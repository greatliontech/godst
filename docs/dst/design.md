# Deterministic Simulation Testing (DST) for Go

Status: **working**. This is the design contract for a fork of the Go toolchain that adds
**deterministic simulation testing** to the runtime. Built with `-tags dst` and driven through the
`testing/simulation` public API, a program's goroutine scheduling, runtime randomness, time, garbage
collection, and process identity become a reproducible function of a seed. It is a general-purpose
facility — any Go program can use it; it is not tied to any particular application. Code conforms to
this doc, not the reverse.

## The problem

A concurrent Go program is hard to test deterministically because the **runtime itself** injects
nondeterminism into "what happens next": the goroutine schedule, `select` poll order, map iteration
order, `math/rand`/`crypto/rand`, GC timing, and wall-clock time all vary run to run. So a
concurrency bug that reproduces one run in a thousand is nearly impossible to debug, and a green test
proves little about the interleavings it did *not* happen to hit.

DST removes that. Inside `testing/simulation.Run(seed, f)`, every source above is a deterministic
function of `seed`: the same seed replays the same execution — every goroutine interleaving, every
random value, every GC cycle — so a failure found once is reproducible forever, and sweeping seeds
explores the interleaving space systematically (optionally PCT-directed; see Seq 5).

It builds on `testing/synctest`, which virtualizes time and provides the goroutine-group "bubble",
and adds the piece synctest deliberately does not: **ordering of runnable goroutines** (synctest's own
docs note it does not order runnable goroutines or mutex acquisition). Without that, determinism on a
concurrent path requires structuring the program so exactly one goroutine is runnable per step — a
fragile discipline that does not survive real concurrency. DST moves that ordering from program
discipline to **runtime enforcement**, so determinism holds with *many* runnable goroutines.

What DST does **not** virtualize today: unsupported network kinds, cgo, and — deliberately — the
standard streams (pre-run host handles under the inherited-handle stance; see "Deterministic pipes
and the stdio stance"). TCP `net.Dial`/`net.Listen` are modeled by the in-memory deterministic
network below, the filesystem by the in-memory deterministic filesystem, and `os.Pipe` by the
in-memory deterministic pipe (all per-bubble, all reset by the run epoch); what remains is modeled
in-memory by the program under test or avoided. Fault orchestration is the main pending feature —
see the Roadmap.

## The core idea (why the minimum is small)

Inside a `synctest` bubble, with `GOMAXPROCS=1`, `asyncpreemptoff=1`, and I/O modeled in-memory, the
*only* remaining nondeterminism in "which goroutine runs next" is:

1. `select` poll order — `select.go` (`cheaprandn`).
2. map iteration order — per-map seed + iterator offsets (`internal/runtime/maps`, via `maps.rand`).
3. sysmon's wall-clock-driven `retake`/`preemptone` (10ms, `proc.go:6672` `forcePreemptNS`) and
   time-driven `forcegc`.

**Make (1)+(2) per-goroutine deterministic; neutralize (3).** Then the bubble is a deterministic
function of the seed *even with many runnable goroutines*. The local run queue at `GOMAXPROCS=1` is
already FIFO and deterministic given deterministic enqueue order — and enqueue order is deterministic
once select and map order are seeded and async/sysmon perturbation is gone.

**Why per-goroutine, not "seed the global RNG" (corrects an earlier cut).** select draws from the
per-m `cheaprand` stream and maps from the per-m `chacha8` stream. Those streams are *also* consumed
by runtime internals — work stealing (`proc.go:3845`), per-goroutine tracking
(`newg.trackingSeq = cheaprand()`, `proc.go:5408`) — and are *per-m*. So merely seeding the global
RNG (Seq 1a) makes select/map order reproducible only in a fully controlled run: under real OS load
the number of those internal draws, which m a goroutine lands on, and load-dependent helper-goroutine
creation all vary, shifting the application's select/map stream (measured: ~1% divergence under CPU
load; one churn run diverged 58/60). The fix is a **per-goroutine deterministic RNG** (`g.dstrand`,
splitmix64) seeded as a **deterministic tree**: the root from the DST seed (via the `testing/simulation`
API — see Enablement below; each `synctest` bubble re-roots independently),
each child from its parent at `newproc1`. select poll order and `maps.rand` draw from this per-g
stream under DST, so a goroutine's select/map order is a function of its own logical history only —
immune to m assignment, scheduler draws, and load (measured: 0 divergences, 150/150 under heavy load,
and 60/60 under GOMAXPROCS=4 M-churn). See `rand.go` `dstrandUint64`.

**Audit of the per-m RNG surface (which draws are routed per-g).** An audit of every `cheaprand`/
`rand` consumer classifies each as application-observable (must be per-g) or runtime-internal
(left per-m — its load-dependent draw count is semantically irrelevant, and routing it per-g would
*re-merge* load-dependent draws into a goroutine's stream). Routed **per-g**: select poll order
(`select.go`), map seed + iteration (`maps.rand` → `rand`), the `math/rand` and `math/rand/v2`
globals and `sync.Pool` (all linkname'd through `runtime.rand`/`runtime.randn`), and the fake-timer
ordering tiebreak for synctest-bubble timers with equal wake time (`time.go`). Left **per-m**
(runtime-internal heuristics): work stealing, `trackingSeq`, GC pacer/mark, mprof/malloc sampling,
itab and symtab cache eviction, `sema` treap ticket (same-address waiters are FIFO, so wake order is
unaffected), and `lock_spinbit` mutex anti-starvation (runtime `lock2`, not `sync.Mutex`). Exempted
specially: `mrandinit` seeds a new m from `bootstrapRand` under DST rather than `rand`, because it can
run on a *user* goroutine's stack (`ready → wakep → newm`) and would otherwise advance that
goroutine's per-g stream by a load-dependent amount.

`randomizeScheduler` (`proc.go:7515`, today `const = raceenabled`) is the Go team's own
"perturb every scheduling decision" hook; it already sits at `runqput`/`runqputslow`/`runqputbatch`
(`proc.go:7534/7585/7623`). The change turns it from a compile-time chaos const into a runtime,
seeded, *controllable* policy — and routes `select` through the same seeded source.

## Enablement: the `testing/simulation` API (the control surface)

DST is enabled and seeded through a **public API**, not GODEBUG. The original `GODEBUG=dstseed`
knob has been **removed**.

The public API lives at **`testing/simulation`**, a sibling of the `testing/synctest` it builds on:
the user surface is a thin wrapper over `runtime`, `internal/synctest`, and the `testing` package's
synctest child-`T` bridge, while the determinism *mechanism* lives in `runtime` and is reached via
`//go:linkname`. This mirrors how `testing/synctest` is the public face of an `internal/synctest`
mechanism — the public name is a testing construct, not a `runtime` sub-package.

- **`simulation.Run(seed uint64, f func())`** is the entry point. It **enforces the determinism
  preconditions itself** — they are not user knobs that can be forgotten: it sets `GOMAXPROCS(1)`,
  disables async preemption, activates DST + seeds, runs `f` in a `synctest` bubble (re-rooted from
  the seed), and restores everything on return (including on panic) — including container-aware
  GOMAXPROCS *auto mode* when the process was in it (the pin sets the manual flag, which is what
  blocks the sysmon auto-updater for the run; the runtime's update-helper goroutine re-parks rather
  than exits when it observes the pin, so an update pushed just before run entry cannot permanently
  kill automatic updates — `TestUpdateMaxProcsHelperSurvivesCustomBail`). The pin is also closed
  against a foreign `GOMAXPROCS`/`SetDefaultGOMAXPROCS` call racing run entry: the setters re-check
  `dstActive` under their stop-the-world and drop the update, and `Run` verifies the pin held after
  activation, panicking loud rather than running a silently nondeterministic simulation
  (`TestDSTGOMAXPROCSDelayedSTWDropped`, `TestDSTGOMAXPROCSEntryRace`). `Run` is bubble-scoped: each
  call is an independent, order-immune deterministic universe (the per-g tree re-roots per bubble in
  `synctestRun` via `dstBubbleMainRoot`, salted relative to the activation root so the bubble main does
  not replay the run caller's draw sequence), so a failing test reproduces identically in isolation.
- **`simulation.Test(t *testing.T, seed uint64, f func(*testing.T))`** is the `testing`-oriented
  entry point. It has the same deterministic envelope as `Run`, and gives `f` a bubble-scoped child
  `*testing.T` with these control semantics: `t.Fatal`/`FailNow` aborts the caller, `t.Cleanup` and
  `t.Context` run inside the bubble, testing durations are finalized outside the bubble, and calling
  it during `t.Cleanup` panics naming the simulation API. One deliberate difference from
  `testing/synctest.Test`: a `FailNow` on an ANCESTOR `T` from inside the simulation aborts the
  whole subtest chain (the `runtime.Goexit` is re-issued to `Test`'s caller, like nested `t.Run`),
  where `synctest.Test` lets the root test continue past its `t.Run`
  (`TestTestWithChainAbortPropagates` pins the choice). `TestWith` is the `Options`-taking form,
  matching `RunWith`.
- **Runtime core** (`runtime/dst.go`): `dstSeed atomic.Uint64` (0 = off) is the live flag the hot
  paths and sysmon read; `dstActive()` is the hot-path check; `dstActivate(seed)` roots the caller's
  per-g stream then sets the flag; `dstSetAsyncPreemptOff`, `dstDeactivate`, `dstBuilt` support `Run`.
  These are reached from `testing/simulation` (and white-box tests) via `//go:linkname`.
- **`dstActivate` is also used directly by white-box runtime tests** (via `$DSTSEED`), so they can
  exercise the per-g mechanism under `GOMAXPROCS>1` M-migration that `Run` (single-P) cannot
  reproduce. This is the only non-`Run` entry and is not a user surface.

### Deterministic process identity (`Options.Hostname` / `Options.PID` / `Options.NumCPU`)

The process identity a SUT can observe returns the **real** machine's values, which vary per run and
per host — a determinism hole for any program that derives identity or seeds from them (node IDs, temp
paths, the `pid`-seeded RNGs some libraries use; pool/shard counts sized by `runtime.NumCPU`; uid-keyed
file modes). So under a run the simulation fixes the whole
surface when DST is active: `os.Getpid`/`Getppid`/`Hostname`, `os.Getuid`/`Getgid`/`Geteuid`/`Getegid`,
`os/user.Current`, and `runtime.NumCPU` (`os/dst.go` and `os/user/dst.go` bridge to `runtime` via
`//go:linkname`; `runtime.NumCPU` reads the runtime state directly; the runtime holds the per-run
values, set by `testing/simulation.run` *before* `dstActivate` so the activation's atomic store
publishes them to the bubble, and cleared on return).

Three values are configurable — `Hostname`, `PID` (defaults `"sim"`, `1`), and `NumCPU` (default `8`,
reported independently of the forced `GOMAXPROCS=1` so a SUT that sizes work by `NumCPU` still creates
real concurrency for the schedule to explore). The rest are fixed deterministic constants documented
on `Options`: `ppid=1`, `uid=gid=euid=egid=7777` (a distinctive value, not the ubiquitous 1000, so the
simulated identity is observably an override), current user `sim` (uid/gid `7777`, home `/home/sim`).
`Run`, `RunWith`, `Test`, and `TestWith` fix the identity, so even plain `Run` or `Test` is reproducible here. This
and the crypto/rand seam below are the only places the fork patches packages other than
`runtime`/`testing/simulation`, and they are unavoidable: the SUT calls `os.*`/`crypto/rand` directly.
The white-box `dstActivate` path leaves identity unset (real values), as it is not a user surface.

The group and user-database surface is simulated to match: `os.Getgroups` is exactly `[7777]`, and
the `os/user` lookup functions resolve against a minimal database containing exactly the simulated
user and its group — `Lookup("sim")`/`LookupId("7777")`/`LookupGroup("sim")`/`LookupGroupId("7777")`
return the simulated records, `User.GroupIds` of the simulated user is `["7777"]` (any other `*User`
resolves to just its primary gid, as the osusergo path does for a user with no group-file
memberships), and anything else is the deterministic production unknown-error identity rather than a
host-database read
(`TestDSTIdentityGroups`). Boundaries that remain host-derived until the I/O features land: the
environment surface (`os.Environ`/`Getenv`, and therefore `os.UserHomeDir` and `os.TempDir`) reads
real host values inside a run — note `os.UserHomeDir` (host `$HOME`) and `user.Current().HomeDir`
(`/home/sim`) disagree in-run; acquire identity through `os/user` for coherence. The simulated
identity is also process-global while set: a goroutine outside the simulation that reads identity
during a run (or in the brief set/clear windows around it) observes simulated values — identity
gates on the sim-env flag, not per-goroutine.
This is a deliberate gating asymmetry: identity is gated on `dstSimEnvSet` (set only by
`testing/simulation.run`), whereas the RNG/scheduling/crypto-rand seams are gated on `dstActive()` (set
by `dstActivate` too). So a white-box run sees seeded RNG, scheduling, and crypto/rand but the *real*
host identity — harmless, because the white-box runtime tests exercise the per-g mechanism under
`GOMAXPROCS>1` and never read identity. `uid`/`gid`/`ppid` are fixed (not configurable like
hostname/pid/NumCPU) by deliberate choice — no SUT has needed per-run variation, and the surface stays
lean; they are single constants in `runtime/dst.go` if that changes.

One identity surface is **not yet virtualized**: `net.Interfaces`/`net.InterfaceAddrs` (interface
MAC/IP) still report the real host's interfaces under a run. They are a follow-on of the landed
in-memory network feature (see "In-memory deterministic network" below): a fixed synthetic interface
set consistent with that network's addressing. In fork scope there is no per-node virtualized-network
subsystem to source per-node identity from, so a fixed synthetic set — with per-run `Options` variation
if ever needed — is the correct shape.

### Deterministic crypto/rand (the entropy seam)

`crypto/rand` (UUIDs, TLS nonces, tokens, key material) reads OS entropy, a determinism hole one might
expect to handle *app-side* (each program injecting its own crypto seam). That assumes `crypto/rand` is
not runtime-seedable.
It is: in the standard configuration every `crypto/rand` read funnels through the single chokepoint
`crypto/internal/sysrand.Read` (the non-FIPS `drbg.Read` is just `sysrand.Read(b)`), so one hook there
makes *all* of `crypto/rand` a reproducible function of the seed for free — exactly as the runtime RNG
seed already covers `math/rand[/v2]`. `crypto/internal/sysrand/dst.go` bridges to
`runtime.dstReadRandom`, which fills the buffer from the calling goroutine's per-g DST stream when a
run is active and returns false otherwise (so production crypto/rand and process-startup entropy are
untouched: `dstActive()` is false outside a run — `dstSeed` is only set by `simulation.Run`, which
requires `-tags dst` — the same cheap atomic load `rand()` already does on its hot path). This holds under `-race`
(the per-g RNG drives it). Boundary: only the **standard** configuration is deterministic — FIPS mode
keeps a process-global SP 800-90A DRBG whose counter the seam does not control (it consumes the
seam's deterministic bytes only as additional input), and BoringCrypto uses its own generator.
BoringCrypto needs a special build DST does not use, but FIPS mode is one `GODEBUG=fips140=on` away
in any build — so it is **enforced, not just documented**: `enterSimulation` panics when FIPS mode is
latched, rather than letting `crypto/rand` go silently nondeterministic inside a run
(`TestRunRejectsFIPSMode`).

**Invariants enforced by the identity/crypto seams:**

- **INV-CRYPTO** (security-critical): `crypto/rand` returns OS cryptographic entropy in every reachable
  state *except* inside an active run, where it returns the deterministic per-seed stream — i.e. it is
  never predictable outside a run, and never in a non-`-tags dst` build. Enforced by
  `TestDSTCryptoRandDeterministic` (deterministic + seed-varying inside a run; two reads *outside* a run
  differ) and structurally by the `dstActive()` gate (`dstSeed` is never set on any production path: its
  only setters are `simulation.Run`, which panics without `-tags dst`, and the unexported `dstActivate`
  linkname used solely by the runtime's own white-box tests).
- **INV-IDENTITY**: within a run, every identity read (`pid`/`ppid`/`hostname`/`uid`/`gid`/`euid`/`egid`/
  `NumCPU`/`os/user.Current`) is a fixed function of the run config, and is restored to the real value
  outside the run. Enforced by `TestDSTProcessIdentity` and `TestDSTIdentityExtra` (the latter also
  asserts the whole surface is restored after the run). The simulated `os/user.Current` is returned
  **uncached** (before `sync.Once`), so the real-user cache is never contaminated and stays valid for
  outside-run use; `uid`/`gid` have a single int source of truth (`os/user` formats the string form),
  so `os.Getuid` and `os/user.Current` cannot disagree.

### In-memory deterministic network (the first I/O feature)

This is a **contract change**: real network I/O moves from "out of scope, the program models it
in-memory" to **owned by the fork**, so unmodified networked code is reproducible under DST without
being rewired through an injected transport.

Under a run, TCP `net.Dial`/`net.Listen` stop touching the OS and run on an in-process **address
registry**: `Listen` registers a simulated listener; `Dial` looks it up and hands the dialer end of a new
connection back while pushing the server end onto the listener's accept queue. A connection is a
`net.Pipe` endpoint (channel I/O, already synctest-durable; deadlines on the fake clock) **wrapped** with
the simulated local/remote `*net.TCPAddr`. `DialContext` keeps the public context contract (nil panics,
canceled/deadline contexts error), `Dialer.LocalAddr` chooses the simulated local TCP address when set,
`:0` listeners receive deterministic nonzero ports, listener lookup uses canonical simulated IPs
(`localhost` maps to loopback), a plain-`"tcp"` wildcard listener is dual-stack (it reports the IPv6
wildcard address and accepts dials of both families, conflicting with either family's listeners on the
port; `"0.0.0.0"` and `tcp4`/`tcp6` stay single-family, and a single-family wildcard listen reports
the family wildcard form — `0.0.0.0:p` / `[::]:p`, dialable back to the listener — not the loopback it
maps to internally), and error identity is production-shaped
throughout `errors.Is`: refused connects are `ECONNREFUSED` and duplicate listens `EADDRINUSE`; every
operation on a locally closed connection or listener (including a second `Close`) is `net.ErrClosed`;
reads from a gracefully closed peer return `io.EOF` while writes to a closed peer and any operation on
a reset connection carry `ECONNRESET`; deadline failures are `*net.OpError` wrapping
`os.ErrDeadlineExceeded` (a timeout `net.Error`) on the connection's network and addresses, driven by
the bubble's virtual clock. Closing a listener resets the connections still in its accept backlog
(production's RST), so a dialer that already succeeded observes `ECONNRESET` instead of blocking
durably forever, and `Accept` after `Close` always fails with `net.ErrClosed` — including an `Accept`
already blocked in its select when `Close` runs: the overlap linearizes to close-first (the pending
accept unblocks with `net.ErrClosed` and its would-be connection is reset with the backlog, as
production unblocks pending accepts on close). The nettrace
`ConnectStart`/`ConnectDone` callbacks fire around a simulated dial, so `httptrace`-instrumented
clients observe connects as in production. The seam is the exported
`Dial`/`DialContext`/`ListenConfig.Listen` (the `os.Getpid` altitude), gated on `dstActive()` so it
compiles out without `-tags dst`; net's internal lookups stay real (the program does not exercise real
sockets under DST).

**Determinism is free.** Connection/accept/delivery order is just the goroutine schedule, which is
already deterministic — no new seed, no new RNG. The registry is keyed by a per-run epoch (`dstNetEpoch`,
bumped in `dstActivate`) so it resets between runs with no teardown hook. Enforced by `TestDSTNet`
(a two-node exchange replays byte-identically across processes; the per-run reset lets a second run
re-Listen the same address). This is the reliable, in-order **base** on which network faults layer
later as policies on the same registry+conns — and because the base is reliable, in-order TCP, the
sound faults are **flow-granular** (latency, partition/blackhole, connection reset, throttle), *not*
byte/message-granular: dropping, reordering, or duplicating bytes on a live stream is not a degree of
freedom TCP has, so packet-granular drop/reorder/duplicate land with the UDP/`PacketConn` follow-on,
not this base. See [faults.md](./faults.md).

**Caveat (fidelity).** `Dial` returns the `net.Conn` *interface*; code that type-asserts the concrete
`*net.TCPConn` (raw fds, `SetNoDelay`, `syscall.Conn`) will not get one. `Dialer.Control`,
`Dialer.ControlContext`, `ListenConfig.Control`, explicit MPTCP enable/use, and explicit keepalive
configuration fail under DST rather than being silently ignored, because they require raw socket semantics
the base model does not provide. Explicitly disabling MPTCP is accepted because the base model is ordinary
TCP. DNS resolution, service-name ports, UDP (`PacketConn`), Unix sockets, and `net.Interfaces` (a fixed
synthetic set consistent with this addressing) are follow-on increments. Public DNS resolver APIs and
service-name port lookups fail under DST rather than touching the host resolver, while literal-IP,
numeric-port, and pre-I/O validation fast paths keep their normal no-I/O behavior. Unsupported networks
at the intercepted `Dial`/`Listen` seam fail under DST rather than being modeled as TCP-like streams or
falling through to the real OS: a known-but-unmodeled network (UDP, Unix, IP) carries the same
"unsupported under deterministic simulation" shape as the typed-API gates, while a genuinely unknown
network string keeps `UnknownNetworkError` identity. `net.FileConn`/`FileListener`/`FilePacketConn`
are likewise rejected fast under a run — an inherited fd is a host socket, the one
conn/listener-producing surface the typed gates did not cover. FIPS/Boring-style configs are out of
scope as elsewhere.

### In-memory deterministic filesystem (the second I/O feature)

Like the network: a **contract change** — real filesystem I/O moves from "out of scope" to **owned
by the fork**, so unmodified file-using code is reproducible under DST. Under a run, the exported
`os` surface (`OpenFile` and the named-path operations, plus the `*os.File` methods) stops touching
the OS and operates on a per-bubble **in-memory tree**, reset per run by the same per-run-epoch
mechanism as the network registry. All operations execute synchronously on the calling goroutine —
no new scheduler choices, no new RNG (the Soundness invariant's "take over, never add" principle);
determinism rides the schedule exactly as it does for the network.

**The tree starts empty — except `/tmp`.** A run observes a root `/` containing one empty `/tmp`
directory (mode `1777`): the host filesystem is NEVER visible (no passthrough, no testdata reads —
a host path is machine state, and reading it would make runs machine-dependent), and `os.TempDir()`
reports the fixed simulated `/tmp` during a run rather than the host's `$TMPDIR`-derived string
(itself machine state), so `CreateTemp`/`MkdirTemp` work unmodified and deterministically — their
random names draw from the seeded runtime stream. A program needing other fixture files creates
them inside the run.
Paths resolve against a per-bubble working directory (initially `/`; `Getwd`/`Chdir` are
per-bubble). The working directory is a PATH, not a node reference: renaming a directory out from
under the cwd leaves the cwd pointing at the old (now missing) path — a deliberate simple model,
recorded here as contractual (the host's fchdir-tracked inode semantics are not promised). A
`DirEntry` from a listing carries its listing-time `Info` snapshot rather than re-statting lazily
as the host does. Directory listings (`os.ReadDir`, `File.ReadDir`/`Readdir`/`Readdirnames`, including
chunked `n > 0` reads against a stable cursor) are **sorted by name** — deterministic, and
consistent with `os.ReadDir`'s documented sorting. Mod times come from the bubble's fake clock.
Permission bits are stored and reported but not enforced in the base model (no simulated
credential checks), and ownership is not represented at all — `Chown`/`Lchown` and `File.Chown`
stay fenced; error identity is production-shaped throughout `errors.Is`: `*PathError`/
`*LinkError` wrapping `syscall.ENOENT`/`EEXIST`/`ENOTDIR`/`EISDIR`/`ENOTEMPTY`/`EBADF`/`EINVAL`,
`os.ErrClosed` on use-after-close, exactly as the host would shape them. POSIX namespace
semantics hold where databases depend on them: an open file removed from the namespace
(`Remove`, or replaced by `Rename`) keeps its content readable and writable through the open
handle until close — content lives on the node, names are references.

**Durability contract (spec tier — governs the fault feature; settled here so the write path
cannot foreclose it).** Every mutation — file write, truncate, create, remove, rename, metadata
change — enters the tree as **unsynced**. `File.Sync` commits that file's content and size
durably; a file's *name* becoming durable is a property of its **parent directory** (POSIX: data
durability and entry durability are separate — fsync the file, fsync the directory), committed by
syncing an open handle on the directory. `Rename` is atomic in the namespace (observers see old
or new, never neither/both); its durability rides the parent directories' sync state like any
other entry change. A simulated **crash** (fault feature) restores exactly the durable image:
synced state survives byte-exactly; unsynced data and entries MAY be lost or, for file content,
torn at arbitrary byte granularity — no atomicity of individual `Write` calls is promised beyond
what was synced. The base (no-fault) model is the collapse of this contract where crash never
fires: everything survives, and `Sync` is *not* a no-op — it moves the synced/unsynced boundary,
which the representation carries from day one (per-node durable image + pending state). The fault
feature later adds crash/EIO/ENOSPC/latency as **policies over this representation**, never new
representation — same layering as network faults over the registry. The monotonicity half is
ENFORCED now (promoted from spec tier at the durability chunk): `TestDSTFSDurabilityMonotonicity`
asserts over a test-only node inspector (current vs durable content, sorted entry-name sets,
metadata image) that content writes, truncate, O_TRUNC, and entry create/remove/rename leave the
durable image untouched — including that the image is a copy, never an alias of live state — and
that sync alone advances it. `O_SYNC` commits per WRITE through the same single commit point;
ftruncate is deliberately not covered (POSIX synchronized I/O is for writes — committing on
truncate would grant durability real disks do not, hiding exactly the bug class DST exists to
catch). The metadata-CHANGE operations (Chmod/Chtimes, named Truncate) are implemented with the same
contract — they mutate current state only; `TestDSTFSMetadata` pins that post-sync Chmod, Chtimes,
and named Truncate all leave the durable image untouched, and that Chmod does not move mtime
(chmod(2) updates only ctime, which is not modeled). One deliberate shape: `Chtimes` on a missing
path is ENOENT even with both times zero — Linux's utimensat both-OMIT-succeeds quirk is not
reproduced.

**The file handle is a backend, not an fd.** `os.File` gains a dst backing chosen when the File
is created: the tree-file backend here, and the pipe feature's `os.Pipe` landed exactly there — a
stream-shaped second implementation of the same seam (`dstFileBackend`), a backend rather than a
retrofit, validating the Non-foreclosure invariant this paragraph recorded for that slot when
the seam was built. `Fd()` on a simulated
file has no honest answer and **panics** with the standard "unsupported under deterministic
simulation" shape — loud, deterministic — rather than returning a host fd that would leak the
simulation; everything `Fd()` feeds (mmap, raw `syscall`, locking) is out of the base model.
Symlinks, `os.Root`, and file locking are follow-on increments **and are fenced until then** —
"not yet modeled" never means "reaches the host": within this feature's surface (the os file and
namespace API; `os/exec`'s process surface is its own roadmap item), every handle-producing or
namespace-touching entry point is either implemented in-sim or fails with the unsupported shape
while a run is active (`os.OpenRoot` included; `os.Pipe` is simulated — see "Deterministic pipes
and the stdio stance"). A `File` or `Root` opened BEFORE the run is a host-backed
handle and stays outside the base model, exactly as inherited fds are for the network — program
discipline, recorded here as the inherited-handle stance — and symmetrically, a simulated `File`
leaked OUT of its run keeps operating on its run's orphaned tree in later runs: deterministic,
host-isolated, and meaningless, the same discipline applied in reverse. An operation pairing a simulated handle
with a pre-run host handle behaves as its two halves: the simulated side goes through the gated
funnels and the host side does real I/O (`io.Copy` from a simulated file to an inherited stdout
takes the generic loop — the zero-copy fast paths bail whenever either side is simulated).

### Deterministic pipes and the stdio stance (the third I/O feature)

`os.Pipe` under a run is **owned by the fork**: it returns a pair of Files backed by one
in-memory byte stream — the stream-shaped second implementation of the `dstFileBackend` seam —
and never allocates a host descriptor (enforced by a /proc/self/fd census across a run). All the
seam-generic stances apply unchanged because they ride the seam, not the backend: `Fd()` panics,
`SyscallConn` is fenced, zero-copy fast paths bail, `net.FileConn` rejects, and a simulated pipe
handed to `os/exec` hits the `Fd()` panic (loud; the process surface is its own roadmap item).
Determinism rides the schedule as everywhere else; blocking operations wait on channels created
inside the bubble, so a blocked pipe read or write is **synctest-durably blocked** — the bubble
clock advances over it and deadlock detection stays sound.

The model is the Linux anonymous pipe, host-probed: capacity 64 KiB (a write blocks at a full
buffer until a read frees space); writes of at most `PIPE_BUF` (4096) bytes are **atomic** —
concurrent small writes never interleave, the guarantee logging and record-framing patterns
depend on — while larger writes chunk and may interleave with other writers, exactly as POSIX
allows. Error identity is production-shaped throughout `errors.Is`, in the host's *probed
precedence*: a closed own end beats everything (`ErrClosed`, including an end closed while
blocked — close wakes its blocked operations); an **expired deadline beats both buffered data
and the wrong-direction check** (an expired write deadline on the read end yields
`ErrDeadlineExceeded`, not `EBADF`); then the wrong direction is `EBADF`, a reader-less write
`EPIPE` (with the partial count when a blocked oversize write had already transferred bytes — no
write atomicity beyond `PIPE_BUF` is promised), and a writer-less empty read is `io.EOF`.
Zero-length operations short-circuit to `(0, nil)` at the host's probed point in each ladder —
reads right after the closed check (so an expired deadline or the wrong direction still reads
`(0, nil)`), writes only after the closed/deadline/direction checks and ahead of `EPIPE` (so
`Write(nil)` on the read end is `EBADF`, with an expired write deadline `ErrDeadlineExceeded`,
but with only the peer closed `(0, nil)`). A closed handle's `SetDeadline` is the host's bare
"use of closed file" on every backend. `Seek`,
`ReadAt`/`WriteAt` are `ESPIPE`; `Truncate` and `Sync` are `EINVAL` — a pipe has no durable image
and sits deliberately outside the filesystem durability contract; `Readdir`/`Chdir` are
`ENOTDIR`; `Chmod` works (host fchmod does) and shows in `Stat`. `Stat` reports
`ModeNamedPipe|0600`, size 0 regardless of buffered bytes, the bubble-clock creation time, and
`SameFile(rfi, wfi) == true` — both ends are one pipe inode, which the shared-identity
representation gives for free. Deadlines (`SetDeadline` family — pipes are pollable on the host,
so these work in production) ride the bubble's fake clock exactly as the simulated network's do;
tree files and directories keep the host's regular-file shape, bare `ErrNoDeadline`. One stance
*tighter* than the tree's: a pipe end **leaked out of its run is fenced** (the unsupported shape)
rather than meaninglessly operable — its blocking machinery is the dead bubble's channels —
except `Close`, which always works (defers and finalizers run anywhere). The *concurrent* dual of
that temporal stance is likewise program discipline: a run-created pipe shared with a goroutine
**outside the bubble while the run is live** (one started before `simulation.Run`) is out of the
model, and synctest makes it loudly fatal — the out-of-bubble operation lands on the bubble's
channels — rather than silently nondeterministic. Enforced by the
`TestDSTPipe*` suite in `src/os/dst_pipe_test.go` (identity pins, durable-blocking and deadline
exactness, PIPE_BUF atomicity under concurrency, fd census, leak fences, same-seed transcript
equality).

**Stdio is not virtualized — settled here.** `os.Stdin`/`Stdout`/`Stderr` are created at process
init, before any run: they are pre-run host handles, covered verbatim by the recorded
inherited-handle stance, and the fork does not swap them under a run. Writes to them are
outbound, schedule-ordered side effects that feed no nondeterminism back into the run — DST's own
cross-process replay fixtures print their transcripts through real stdout from inside runs —
and a program that wants captured or deterministic stdio assigns the package variables to a
simulated file inside the run (the backend seam makes that work with no extra machinery); reading
the real terminal under a run is program discipline, exactly like using any inherited handle.
Completing the audit of the remaining OS-backed I/O surface: `io.Pipe` is pure memory;
`ReadFile`/`WriteFile`/`CreateTemp`/`MkdirTemp` ride the simulated `OpenFile`; `Hostname` and
`Getpid` are Options-pinned; env-derived APIs (`Getenv`, `UserHomeDir` and friends) read process
memory the harness controls, not the OS; processes and signals are the `os/exec` roadmap item.
One recorded gap: **`os.DevNull`** — the tree starts empty except `/tmp` (that contract stands),
so opening `/dev/null` under a run is `ENOENT`; the in-sim idioms are `io.Discard` for a sink or
an ordinary tree file, and the main host consumer of `/dev/null` (process spawning) is out of
scope here. If a modeled `/dev/null` ever earns its place, it is a new node kind behind the same
seam — an increment, not a retrofit.

### Enforcing test configurations

The DST contract tests are dead in a stock `-short`/untagged run. The enforcing configurations are
the tasks in `Taskfile.yml` at the repo root (the A2-25 runner choice); each task name below is the
authoritative statement of its leg, and the `go test` command in the Taskfile is its definition:

- **`test:untagged`** (`go test -count=1 -short runtime`, untagged): DST hooks are inert; also
  enforces that `runtime/testdata/testprog` stays cgo-free — a cgo-pulling import there (net,
  os/user — DST fixtures needing those live in `testprognet`) disables the runtime's deadlock
  detection and hangs the crash tests loudly.
- **`test:dst`** (`go test -tags dst -count=1 -timeout 60m runtime testing/simulation net os`,
  non-`-short`): the
  802-program sweep, the race-oracle and auto-instrumentation tests — which build their own
  `-race` testprogs — and the build-mode inertness test all skip under `-short`. The untagged
  build-constraint panic is covered by `TestDSTRunRequiresBuildTag`, which builds its own untagged
  testprog.
- **`test:dst-race`** (`go test -tags dst -race -count=1 testing/simulation`): the dst-race
  sync-hook encodings. The suite is `-race`-clean: every SUT that runs under `-race` is race-free —
  intentionally racy SUTs are either subprocess testprogs or skip-gated to the non-race leg via
  `dstRaceEnabledFP` — so a TSan report in this leg is a real finding; the skip gates are
  load-bearing for this invariant.
- **`test:inert-std`** (`go test -count=1 -short std`, untagged): build-mode inertness across all
  of std. Heavy; runs separately from the `test` aggregate, which runs the other three legs
  sequentially and fail-fast.

Two operational rules for running these configurations honestly: never let a pipeline eat the exit
code (`go test ... | tail -1` reports the pipe's status, not the test's — this masked real failures
twice; the Taskfile must stay pipeline-free, so each leg's `go test` exit code is the task's exit
code), and after ANY cmd/compile change run `task compiler` — it chains `go clean -cache` after the
reinstall because this fork reports a release-form version string, so tool IDs come from the
version, not the binary hash, and a reinstalled compiler does NOT invalidate cached objects
(stale-compiler builds silently pass). The `VERSION` file carries a `-dst` suffix
(`go1.26.4-dst`), so this checkout's tool IDs differ from stock's and from any sibling worktree
still reporting the bare release — distinct checkouts cannot cross-poison the shared
`~/.cache/go-build`. The suffix does NOT fix the within-checkout trap (a suffixed release is still
a release to the tool-ID logic: the whole `-V=full` line, constant across rebuilds), so the
clean-cache rule stands unchanged. All four legs gate green; a red leg is a regression against
this section.
One environmental failure mode masquerades as a build regression: the `std` leg's parallel build
trees plus accumulated per-test temp dirs can fill a tmpfs `/tmp` mid-leg ("disk quota exceeded"
or "no space left on device" from compile/link/cgo). The Taskfile closes this by construction —
every command pins `TMPDIR` to the gitignored on-disk `.tmp/` at the repo root (freely deletable
between runs); a FAIL of that shape can still appear in a bare `go test` run outside the tasks.
A second environmental dependency: the non-`-short` net suite includes external-DNS tests
(`TestLookupDotsWithRemoteSource` et al.) that require a recursive resolver returning real
answers; a filtering/captive upstream (observed: quad9 echoing the arpa name for 8.8.8.8 reverse
lookup) fails them with no fork involvement — confirm on the base branch or another network, then
re-run with `-skip` for the affected lookup tests, before reading a net FAIL as real.

### Map hash key requires `-tags dst` (a startup constraint the API cannot cover)

Map *iteration order* depends on the process-global hash key (`aeshash`/`memhash`), set at **startup**
in `alginit` from OS entropy for hash-flooding protection. It cannot be re-seeded at runtime without
corrupting maps created before activation (including runtime/stdlib-internal ones the bubble then
touches). So a deterministic map order needs a **build-time** signal: **`-tags dst`** makes `randinit`
seed the global generator from a fixed constant (`dstFixedSeed`), fixing the hash key — but the *seed*
alone is not sufficient; the key is also derived position-independently (see next paragraph). Map order
is still *seed-varied* via the per-g `m.seed`; only this one global key is fixed. `simulation.Run` **panics if
the binary was not built with `-tags dst`**, so the constraint can't be silently violated. A
`-tags dst` binary has a fixed hash key for all maps (hash-flooding exposure) — acceptable for a test
build, and absent from normal builds. Upstream's `TestMemHashGlobalSeed` asserts the opposite
(per-process seed uniqueness), so it skip-gates on `runtime.DSTBuild`; the untagged legs still
enforce it for normal builds.

**The hash key is derived position-independently, so map order is *build*-invariant too.** Fixing the
global RNG *seed* (`dstFixedSeed`) is necessary but not sufficient: `alginit` fills the hash key
(`aeskeysched`, and the non-AES `hashkey` fallback) from `bootstrapRand`, which draws from that
fixed-seeded stream at whatever *position* `alginit` has reached — and the number of startup draws
preceding `alginit` varies with **binary composition** and **`-race`/`-msan` instrumentation**. So a
`bootstrapRand`-derived key is only fixed *per build*: a different build shifts the key (measured: a
`-race` build's `aeskeysched` is the normal build's shifted by exactly one word — `race[i]==normal[i-1]`
across the first 6 key words), and a `≥16`-element
(multi-group) map then iterates in a different order, because keys land in different groups via
`hash & mask` (single-group maps `≤8` place keys in insertion order, so they are invariant regardless).
This is the same defect class as the system-goroutine scheduling leak — a composition/instrumentation-
varying draw count shifting a seeded stream. So under `-tags dst` the key is instead derived from a
**fixed constant** (`alg.go` `dstFixedHashKey`, a salted splitmix64 mirroring `dstFixedSeed`),
position-independently, so the key — and thus map iteration order — is identical across builds and
import sets, not merely fixed per build. Per-map seed variation (`m.seed`) is untouched; only this one
process-global key is fixed. The per-g streams (`g.dstrand`, `dstSchedRand`) are immune already: they
re-root from the DST seed at `dstActivate`/bubble entry, not from the startup global stream. Enforced
by `TestDSTMapHashKeyBuildInvariant` (a normal-`dst` and a `-race`-`dst` build iterate a 48-element map
identically; reverting the key to `bootstrapRand` makes them diverge).

## Top-tier contract (governing invariants)

### Soundness invariant (kind=entailed)

> property: the set of executions the harness can produce ⊆ the set the real runtime+OS can produce.
> Every injectable choice corresponds to a real degree of freedom at that point.
>
> violation: the harness reports a failure (split-brain, lost commit, deadlock, IO-recovery bug)
> reachable *only* because it injected a choice the real system never had — e.g. reordering two
> operations a happens-before edge causally orders, delivering a channel value out of FIFO order,
> firing a timer before its deadline, returning an error from an infallible call. Strongest
> counterexample: every documented ordering guarantee still holds yet the harness produced an
> impossible state. The result is a false positive that erodes trust in the harness.

Sound *by construction* if the control surface hooks only where the runtime already makes an
unspecified/RNG-driven choice (below): by the time a goroutine is runnable, the primitive that woke
it (channel FIFO, mutex handoff, happens-before) has already applied its ordering guarantee.

### Completeness caveat (state plainly — do not oversell)

`GOMAXPROCS=1` + `asyncpreemptoff` is **sound but not complete**. It explores a *subset* of real
interleavings: it cannot reproduce a data race requiring two goroutines to physically execute the
same instant, nor preemption at an arbitrary instruction. It finds **logical** concurrency bugs
(ordering, atomicity-of-logic, deadlock, lost wakeup, stale read, split-brain) — **not** physical
memory races (torn/reordered/stale reads under a weak memory model). Those remain the job of `-race`.
The simulation and `-race` are complementary. Completeness in the *interleaving* dimension (Gap A) is
*increased* additively by injecting cooperative preemption points — at function entries (PCT-style, Seq
5) and, fully, at memory-access granularity pruned by DPOR (see exploration.md, **Level 2**), still deterministically.
The physical-memory-model dimension (Gap B) is a different mechanism and stays with `-race`.

### Non-foreclosure invariant

> property: the control seam expresses the controllable surface as "which runnable goroutine proceeds
> next" + "what wakes when the bubble is quiescent" (`synctest.go:132` `maybeWakeLocked`), routed
> through the existing waiting→runnable choke points: `goready` (`proc.go:486`), `injectglist`
> (`proc.go:4053`), `netpollready` (`netpoll.go:494`).
>
> violation: a lower-tier mechanism that a later axis (transparent I/O interception, an advanced
> exploration strategy) cannot feed without a different *shape*, forcing a throwaway retrofit.

## The control surface = the existing nondeterminism points (sound ≡ minimal)

| Choice point | File:line | Today | Under DST |
|---|---|---|---|
| Order among runnable Gs | `proc.go` `runqget`/`findRunnable` | FIFO local (deterministic at P=1) | seeded; optionally strategy-driven |
| `select` among ready cases | `select.go:191` | uniform random, entropy-seeded | seeded; strategy can force a *ready* case |
| `runnext`/batch shuffles | `proc.go:7534/7585/7623` | off unless `-race` | seeded policy |
| Quiescent-wake decision | `synctest.go:132` | next timer / root | next timer / simulated event / fault |
| select+map RNG source | `select.go`, `maps.rand` | per-M stream (shared w/ scheduler) | per-g deterministic tree (`g.dstrand`) seeded from the knob |

**Principle:** never *add* scheduler choices; *take over* the choices it already makes
nondeterministically. Minimality and soundness are the same property.

## Nondeterminism sources and who owns them

What the fork makes deterministic under DST, what is pending, and what a program under test still owns.

Status: ✅ owned by the fork · ⏳ pending feature (see Roadmap) · ⛔ out of scope (the program models it).

| Source | Mechanism | Status |
|---|---|---|
| Goroutine scheduling order | per-g RNG tree + single-P + sysmon neutralized + the get-side selection hook (Seq 5); system (non-bubble) goroutines isolated from the seeded RNG | ✅ |
| `select` poll order | per-g `g.dstrand` | ✅ |
| map iteration order | per-g `g.dstrand` (`maps.rand`) + fixed process hash key (`-tags dst`) | ✅ |
| `math/rand`, `math/rand/v2` (top-level funcs) | `//go:linkname`'d to `runtime.rand` → per-g stream | ✅ |
| `crypto/rand` | `crypto/internal/sysrand.Read` seam → per-g stream | ✅ |
| time, timers, tickers | `testing/synctest` fake clock | ✅ |
| GC (count, finalizer/weak set, memory bound) | STW in-bubble GC + per-bubble relative trigger | ✅ |
| process identity (pid/ppid/hostname/uid/gid/NumCPU/user) | `os`/`os/user` seams + sim-env | ✅ |
| network I/O | in-memory deterministic `net` (`Dial`/`Listen`/`Conn`, address registry) | ✅ |
| filesystem / disk I/O | in-memory deterministic filesystem (os surface, per-bubble tree) | ✅ |
| pipes (`os.Pipe`) | in-memory deterministic pipe (stream backend behind the `os.File` seam) | ✅ |
| standard streams (stdio) | pre-run host handles (inherited-handle stance; swap the package vars in-program to capture) | ⛔ (program discipline) |
| faults (scheduling / net / disk / clock / OOM / crash) | fault-orchestration layer: the Host/Process victim contract + policies at the existing seams (see [faults.md](./faults.md)) | ⏳ |
| cgo | — | ⛔ |
| raw pointer addresses (ASLR, `%p`, `uintptr`) | — | ⛔ (program discipline) |

**Library randomness — seeded for free or needs a seam?** A dependency's randomness is covered with no
patch iff it bottoms out in the `math/rand`/`math/rand/v2` **top-level** functions (`//go:linkname`'d to
`runtime.rand`) or `crypto/rand` (routed through the `sysrand.Read` seam) — both draw from the per-g
stream. It needs its own seam only if it holds a *private* `rand.New`/`NewSource`/`NewPCG` instance (or
installs a non-default `crypto/rand.Reader`), which the runtime seed cannot reach. Find those with:
`git grep -nE 'rand\.New\(|rand\.NewSource|rand\.NewPCG' -- '*.go' ':!*_test.go'`, then reseed the
instance from a seeded source or inject it.

## Scope (full / final form)

A DST run executes an arbitrary Go program inside one `synctest` bubble as a deterministic function of
a seed. A distributed system under test — N nodes, their storage, their transport — runs as N goroutine
trees in the *same* bubble, all driven by the one seed. The axes:

- **Scheduling** — runtime-enforced deterministic ordering of all runnable goroutines from the seed;
  optional strategy-driven control (random → PCT → exhaustive) at the same hook; cooperative
  preemption-point injection for completeness.
- **Time** — one fake clock (synctest); all timers and tickers driven from it.
- **Random** — seeded everywhere the runtime owns it (select/map/`math/rand`/`crypto/rand`); a library
  holding a private RNG instance reseeds from a seeded source.
- **Network / Disk / I/O** — in-memory and deterministic (I/O landed; faults pending), with sound
  faults: on the reliable TCP base, flow-granular latency (= a fake timer), partition/blackhole,
  connection reset, throttle (byte-granular drop/reorder/duplicate are a UDP follow-on, not sound on a
  reliable stream); EIO/ENOSPC; torn/lost unsynced writes on crash.
- **Faults** — process & host crash/restart, net faults, disk faults, clock skew/drift/step, OOM, and
  *scheduling* faults (straggler) — each anchored to a real degree of freedom, so sound (pending; see
  [faults.md](./faults.md)).
- Driven by a seed; replay-exact; failures shrinkable; invariants checked by the program's own
  assertions.

## Companion design documents

The mechanism designs and the pending fault feature live in companion docs under `docs/dst/`, each
governed by the top-tier contract in this file (read this contract first — Spec-first). This file is
the canonical spec; the companions are lower-tier designs that collapse from it.

- **[exploration.md](./exploration.md)** — the scheduling/exploration axis: Seq 5 seeded interleaving
  diversity + strategies, and Level 2 access-granularity DPOR.
- **[gc.md](./gc.md)** — deterministic GC for DST (full scope; the landed state is Tier 2).
- **[faults.md](./faults.md)** — the distributed model (Universe / Host / Process) and fault
  orchestration (the pending feature; designed upfront, built bottoms-up).

## Roadmap

The runtime substrate is **landed**: the per-g RNG + scheduling (Seq 1, Seq 5), the
`testing/simulation` API, and — beyond the original sequence — deterministic process identity,
crypto/rand, GC, memory bounding, and a hardening pass (each documented in its own section). The
remaining work is the **I/O and fault** features (the ⏳ rows of the source table). Each step respects
the fixed seams, so later steps add, never rewrite.

> Note: Seq 1a/1b originally enabled DST via `GODEBUG=dstseed`; that was pivoted to the public
> `testing/simulation` API (see Enablement). The per-g mechanism is unchanged. The bullets below record
> what each increment built.

### Landed

- **Seq 1a — Runtime RNG seeding (go fork). LANDED.** `GODEBUG=dstseed=<int32>` deterministically
  seeds the global chacha8 RNG (`rand.go` `randinit`), so the `math/rand[/v2]` globals (linkname'd to
  `runtime.rand`) and the global hash seed become a reproducible function of the seed. Companion
  knobs: `GOMAXPROCS=1`, `GODEBUG=asyncpreemptoff=1`. Test: `TestDSTDeterministicSelect`
  (same seed → identical, different seed → different), mutation-verified. *Superseded for select/map
  ordering by the per-g tree (1b) — see "Why per-goroutine" above.*
- **Seq 1b — Sysmon neutralization + per-g select/map RNG (go fork). LANDED.** Two parts under
  `dstseed`:
  (i) Gate sysmon's time-based `retake`/`preemptone` (`proc.go` `forcePreemptNS`) and time-triggered
  `forcegc`, so wall-clock-driven preemption/GC can't perturb a multi-goroutine interleaving. Test:
  `TestDSTSysmonNoPreempt` (a watcher can only observe a burst mid-flight if it was preempted),
  mutation-verified on the `preemptone` gate. The `forcegc` gate is correct-by-inspection but not
  unit-tested (it fires on a multi-minute wall-clock timer — impractical to unit-test).
  (ii) Per-g deterministic RNG tree (`g.dstrand`) driving select poll order and `maps.rand`, so
  select/map order is immune to m assignment and runtime-internal draws. Tests:
  `TestDSTSelectChurn` (identical across runs under GOMAXPROCS=4 M-churn — the test that distinguishes
  per-g from per-m: per-g 0 divergences, per-m ~58/60), `TestDSTDeterministicMap` (same seed →
  identical map order, different seed → different).
  `randomizeScheduler`-as-seeded-var (interleaving *diversity*) is not part of 1b — it is Seq 5 (5a),
  which landed later.

- **Seq 5 — Scheduling control + sound scheduling faults. LANDED (5a/5b).** See exploration.md ("Seq 5
  design") for the validated seam and framing (the residual it addresses is seed-*invariance*, i.e. one explored
  interleaving — not nondeterminism). **5a** seeded interleaving diversity (get-side selection at the
  `findRunnable` seam from a per-bubble scheduling RNG; default strategy = seeded-random); **5b** the
  strategy hook at the same choice point (random → PCT, exposed via `RunWith(Options, f)`). **5c** sound
  scheduling faults (delay/deprioritize a runnable G) folds into the fault-orchestration feature below.
  Turns reproducibility into *directed* exploration.

- **System-goroutine isolation (scheduling robustness). LANDED.** The seeded scheduling RNG
  (`dstSchedRand`) advances only for selections among the *simulation bubble's* goroutines;
  runtime-infrastructure goroutines (`g.bubble == nil`) AND goroutines of any FOREIGN synctest bubble
  (a plain bubble live concurrently with the simulation — `g.bubble != dstSimBubble`) are scheduled by
  a fixed RNG-free policy (`dstFindRunnable` prefers them in candidate order). The simulation claims
  its bubble by activating-goroutine identity (`dstSimRootG`), so a foreign `synctest.Run` — even one
  started between activation and the simulation's own bubble — can neither steal the re-root/drain
  nor consume seed draws; candidate removal is order-preserving so foreign entries cannot permute the
  simulation candidates' relative order, and so is local-ring **overflow**: when the runnable set
  exceeds the ring (256), puts divert to an order-preserving FIFO ring extension (`p.dstRunqOvf`,
  enumerated between the ring and `runnext`, flushed to the global runq at deactivation) instead of
  `runqputslow`'s rotation of the ring head to the global tail — whose spill boundary foreign ring
  occupancy would otherwise shift, permuting which simulation candidates spill and where they
  re-enter the enumeration; and only
  simulation-bubble allocations advance the
  deterministic GC trigger. Mid-run finalizer/cleanup **registrations** are isolated by ownership:
  each finalizer/cleanup special carries the registering goroutine's run-epoch stamp
  (`dstCallbackEpoch`), and queue-time routing defers any callback not registered by THIS run's
  bubble goroutines past deactivation with the pre-bubble queues — so the drain executes exactly the
  run's own callbacks and a foreign callback can never advance the drain's per-g RNG stream (the
  drained set is a pure function of the run's own activity). Enforced by `TestDSTForeignBubbleIsolation`, `TestDSTBubbleStreamIsolation`,
  `TestDSTNonBubbleAllocTrigger`, `TestDSTRunqOverflowOrder` and `TestDSTForeignCallbackDeferred` in addition to the invariant below. Without this, how often infrastructure goroutines need scheduling — which is
  timing- and binary-composition-dependent — would consume a varying number of RNG draws and shift the
  program's interleaving (a rare nondeterminism a heavy `import` like `net` exposed). Invariant:
  `rngDraws == decisions − sysScheds`, enforced by `TestDSTSchedSystemIsolation`.

- **Network (in-memory deterministic `net`). LANDED (first I/O feature).** Under DST, `net.Dial`/`Listen`
  run on an in-process address registry instead of the OS: `Dial` ↔ `Accept` hand each other a
  `net.Pipe`-backed connection pair (channel I/O, synctest-durable, deadlines on the fake clock) wrapped
  with simulated addresses, so unmodified networked code is reproducible without modeling the network
  itself. Determinism rides the scheduler (no new seed plumbing); the registry is keyed by a per-run
  epoch so it resets between runs. The reliable, in-order base for network faults. See the "In-memory
  deterministic network" section above; tested by `TestDSTNet`. Caveat: only the `net.Conn` interface — code type-asserting
  `*net.TCPConn` (raw fds, `SetNoDelay`) does not get one. DNS/service-name ports/UDP/Unix/
  `net.Interfaces` are follow-ons; public DNS and service-name lookups fail under DST rather than touching
  host resolver state.

- **Disk (in-memory deterministic filesystem). LANDED (second I/O feature).** Under DST the exported
  `os` surface operates on a per-bubble in-memory tree (empty root + a pre-seeded `/tmp`; fixed
  `os.TempDir`), reset by the run epoch: the full file-handle surface, the namespace ops with sorted
  deterministic listings and a per-bubble path-model cwd, unlinked-but-open POSIX semantics, named
  metadata ops, and the durability representation with its enforced monotonicity invariant (the
  synced/unsynced split crash faults will tear along). Everything not modeled is fenced — host
  isolation is an enforced invariant, not a convention. See the "In-memory deterministic filesystem"
  section above; tested by the `TestDSTFS*` family, the durability white-box, and the cross-process
  `TestDSTDiskReplay`. Caveats: no symlinks/`os.Root`/locking yet (fenced follow-ons), no ownership
  model (`Chown` fenced; permission bits stored, not enforced), `Fd()` panics, `Sys()` is nil.

- **I/O (deterministic pipes + the stdio stance). LANDED (third I/O feature).** `os.Pipe` under DST
  is an in-memory stream behind the `os.File` backend seam the disk feature built — Linux anonymous
  pipe semantics host-probed end to end (64 KiB capacity, PIPE_BUF atomicity under contention, the
  full error-precedence ladders, fake-clock deadlines, partial counts, SameFile across the pair),
  synctest-durable blocking, no host descriptor ever. Stdio is settled as NOT virtualized (the
  inherited-handle stance covers the package streams; programs swap them in-run for capture), and
  the remaining OS-backed I/O surface is audited closed — `/dev/null` stays `ENOENT` under a run
  (recorded gap; `io.Discard` or a tree file is the in-sim idiom). See the "Deterministic pipes and
  the stdio stance" section above; tested by the `TestDSTPipe*` family and the cross-process
  `TestDSTPipeReplay`. Caveats: a pipe end leaked out of its run is fenced (except `Close`);
  `os/exec` remains its own roadmap item.

- **Level 2 — access-granularity interleaving + DPOR. LANDED.** The `-race` access hooks double as DST
  scheduling decision points (`-tags dst -race` builds), explored systematically via source-DPOR (sleep
  sets + weak-initial backtracks) with the HB race detector as deterministic oracle, exposed as
  `Explore`/`ExploreWith`/`Replay`. `sync/atomic` operations (free and typed APIs) and `len(ch)`
  observations are recorded decision transitions too — the former Completeness-boundary exclusions are
  closed (`TestDSTExploreAtomicAutoInstrument`; the sweep's atomic families). Increments D1–D5 implemented and validated (see
  exploration.md, "Level 2"; enforcement: the 802-program sweep `TestDSTExploreSweep`, `TestDSTExploreComplete`,
  the race-oracle and replay tests, and build-mode inertness `TestDSTAccessYieldBuildModeInert`).

### Pending features

With the three I/O axes landed (network, disk, pipes), one feature remains: layering fault
injection on top of the virtualized substrate.

- **Fault orchestration** — compose scheduling, network, disk, clock, OOM, and crash/restart faults under
  one seed, with replay and failure shrinking. Each fault is anchored to a real degree of freedom (sound);
  all axes share one *victim-designation* contract — the **Host/Process** model (FS/network shared at the
  host, memory isolated at the process). **Contract settled** in [faults.md](./faults.md) (every axis +
  the shared contract designed up front, so no axis forecloses
  another); implementation is **bottoms-up** — the Host/Process substrate first, faults last (see "Build
  order").
Ordering: the landed runtime substrate was the precondition for all; the I/O features (network ✅ →
disk ✅ → pipes ✅) each brought a class of real I/O into the bubble; fault orchestration layers
exploration power on top now that there is something to fault. Level 2 extends the scheduling axis
itself and depends only on the landed Seq-5 seam + `-race`.

## Decisions (settled)

- **Control surface: a public `testing/simulation` API** (not GODEBUG, not internal). `GODEBUG=dstseed` is
  removed. Seq 5 (strategy/faults) extends this same public API (`RunWith(Options, f)`).
- **Bubble-scoped**: the per-g seed/control re-roots per `synctest` bubble (order-immune
  reproduction). Process-level enablers (`GOMAXPROCS=1`, async/sysmon preemption off) are enforced by
  `simulation.Run` for the duration of a call.
- **Map hash key via `-tags dst`** (see Enablement) — the one precondition the runtime API cannot
  cover, enforced by a `simulation.Run` panic.

## Open questions

- **GC under DST** is its own design problem with a full scope of its own — see
  [gc.md](./gc.md). Current state (Tier 2, landed): GC stays enabled in-run with the
  deterministic per-object trigger, STW mark + synchronous sweep, and the bubble-scoped finalizer &
  cleanup drain; the cross-Run `sync.Pool` reap is folded into the in-bubble run-end fixpoint. The
  full scope and tiers are written up so the depth of fix is a deliberate choice, not a default.
- **Upstreamability**: whether the runtime knobs are kept as a fork patch or shaped to be proposable
  upstream (the `randomizeScheduler`-as-knob framing is the most upstream-friendly).

