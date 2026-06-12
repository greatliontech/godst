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

What DST does **not** virtualize today: real file I/O, unsupported network kinds, and cgo. TCP
`net.Dial`/`net.Listen` are already modeled by the in-memory deterministic network below; other I/O is
modeled in-memory by the program under test or avoided. Bringing the remaining I/O into the fork (an
in-memory deterministic filesystem and file/pipe I/O) is the main pending feature set — see the Roadmap.

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
re-Listen the same address). This is the reliable, in-order **base** on which network faults
(partition/drop/reorder/latency) layer later as policies on the same registry+conns.

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

### Enforcing test configurations

The DST contract tests are dead in a stock `-short`/untagged run; the enforcing configurations are:
`go test -tags dst runtime testing/simulation net` (non-`-short`: the 802-program sweep, the
race-oracle and auto-instrumentation tests — which build their own `-race` testprogs — and the
build-mode inertness test all skip under `-short`), `go test -tags dst -race testing/simulation` for
the dst-race sync-hook encodings (the suite is `-race`-clean: every SUT that runs under `-race` is
race-free — intentionally racy SUTs are either subprocess testprogs or skip-gated to the non-race leg
via `dstRaceEnabledFP` — so a TSan report in this leg is a real finding; the skip gates are
load-bearing for this invariant), and an
untagged `go test std`-level pass for build-mode inertness. The untagged build-constraint panic is
covered by `TestDSTRunRequiresBuildTag`, which builds its own untagged testprog. The untagged
`-short runtime` leg also enforces that `runtime/testdata/testprog` stays cgo-free: a cgo-pulling
import there (net, os/user — DST fixtures needing those live in `testprognet`) disables the
runtime's deadlock detection and hangs the crash tests loudly.

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
build, and absent from normal builds.

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
5) and, fully, at memory-access granularity pruned by DPOR (see **Level 2**), still deterministically.
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
| filesystem / disk I/O | in-memory deterministic filesystem | ⏳ |
| other I/O (files, pipes, stdio) | in-memory deterministic I/O | ⏳ |
| faults (scheduling / net / disk / crash) | fault-orchestration layer | ⏳ |
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
- **Network / Disk / I/O** — in-memory and deterministic (pending features), with sound faults: latency
  (= a fake timer), partition/reorder/drop/duplicate; EIO/ENOSPC; torn/lost unsynced writes on crash.
- **Faults** — node crash/restart, net faults, disk faults, and *scheduling* faults
  (delay/deprioritize a goroutine) — each anchored to a real degree of freedom, so sound (pending).
- Driven by a seed; replay-exact; failures shrinkable; invariants checked by the program's own
  assertions.

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

- **Seq 5 — Scheduling control + sound scheduling faults. LANDED (5a/5b).** See "Seq 5 design" below
  for the validated seam and framing (the residual it addresses is seed-*invariance*, i.e. one explored
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

- **Level 2 — access-granularity interleaving + DPOR. LANDED.** The `-race` access hooks double as DST
  scheduling decision points (`-tags dst -race` builds), explored systematically via source-DPOR (sleep
  sets + weak-initial backtracks) with the HB race detector as deterministic oracle, exposed as
  `Explore`/`ExploreWith`/`Replay`. `sync/atomic` operations (free and typed APIs) and `len(ch)`
  observations are recorded decision transitions too — the former Completeness-boundary exclusions are
  closed (`TestDSTExploreAtomicAutoInstrument`; the sweep's atomic families). Increments D1–D5 implemented and validated (see the "Level 2" design
  section below; enforcement: the 802-program sweep `TestDSTExploreSweep`, `TestDSTExploreComplete`,
  the race-oracle and replay tests, and build-mode inertness `TestDSTAccessYieldBuildModeInert`).

### Pending features

These bring the remaining real I/O into the bubble and then layer fault injection on top. Each is
virtualized in-memory and deterministic, riding the existing scheduling/time determinism.

- **Disk / filesystem** — an in-memory, deterministic filesystem under DST (file ops, directory
  iteration), the base for disk faults.
- **I/O** — deterministic file/pipe/stdio I/O for whatever the network and filesystem layers do not
  cover.
- **Fault orchestration** — compose scheduling, network, disk, and crash/restart faults under one seed,
  with replay and failure shrinking. Each fault is anchored to a real degree of freedom (sound); the
  scheduling- and network/disk-fault targets share one *victim-designation* contract designed here.
Ordering: the landed runtime substrate is a precondition for all; the I/O features (network → disk →
io) each bring a class of real I/O into the bubble; fault orchestration layers exploration power on top
once there is something to fault. Level 2 extends the scheduling axis itself and depends only on the
landed Seq-5 seam + `-race`.

## Seq 5 design: seeded interleaving diversity (validated seam + framing)

**The framing correction (measured, not assumed).** Seq 5 is *not* a determinism fix. Seq 1a/1b
already made multi-runnable scheduling under `simulation.Run` both **deterministic** and **sound**: a
GOMAXPROCS=1 bubble with N goroutines contending through `Gosched` replays *one* interleaving across
runs (probe `DSTSchedScenario`: 1 distinct over same-seed runs). The residual is the opposite of
nondeterminism — the schedule is **seed-invariant**: every seed (1, 2, 3, 12345, 999, 777, 424242)
produces the *identical* `4,0,1,2,3` round-robin. So the harness explores exactly **one** sound
interleaving regardless of seed; an ordering bug reachable only under a *different* sound interleaving
is invisible — running N seeds re-runs the same interleaving N times. Seq 5 closes this completeness
gap by making "which runnable goroutine proceeds next" a **seeded** function: different seeds explore
different *sound* interleavings. (An earlier exploratory note that the `Gosched`/global-runq path was
*nondeterministic* was measured on a raw non-DST program; under the actual `simulation.Run` harness — sysmon
gated, asyncpreemptoff, single P, per-g RNG — it is deterministic. The fault is diversity, not noise.)

**The seam: a single unified choke point in `findRunnable` (validated).** At GOMAXPROCS=1 the runnable
set spans three places — `runnext`, the local ring, and the global runq — all drained today in
FIFO/runnext-priority order, hence seed-invariant. The seam is **one** function, `dstFindRunnable`,
spliced into `findRunnable` where it would call `runqget` then `globrunqgetbatch`: under DST@P=1 it
gathers the whole runnable set {`runnext` ∪ local ring ∪ global runq} into a stack-allocated
`dstCandidates` view (no slice — enumerating/removing a candidate allocates nothing, so it cannot
perturb GC determinism) and asks the active **strategy** which one proceeds. A chosen local-ring
element is swap-removed (head into the vacated slot, advance head); a global element is unlinked by
index; runnext is cleared. The `schedtick%61` global-fairness peek is gated off under DST (the unified
pick already considers the global runq).

Why *unified*, not per-queue: a per-queue hook (seed `runqget`, separately seed `globrunqget`) cannot
express a **global**-priority policy — and PCT (below) must run the globally-highest-priority runnable
goroutine, across local and global together. Hooking the single dequeue point in `findRunnable`, which
sits downstream of every readying source (`goready`/`injectglist`/`netpollready` — the non-foreclosure
choke points), is the literal first row of the choice-point table (*order among runnable Gs:
`runqget`/`findRunnable`*) and is strictly more diverse even for uniform-random (a `Gosched`'d
goroutine on the global runq can now interleave ahead of a locally-woken one, which the per-queue
local-before-global structure forbade). No put-side *seeding* is needed: choosing from the union
subsumes the `runnext` decision.

The **only put-side change** is to *defer* the `-race` `randomizeScheduler` shuffle under DST (gate
its three hooks with `!dstActive()`): under `-race`, `randomizeScheduler` is a `const true` and would
reorder the enqueue (and, via `randn → per-g rand`, advance the enqueuing goroutine's stream)
nondeterministically, perturbing the get-side seam's input. This is *not* put-side seeding — it hands
ordering to the seeded get-side seam, exactly as the Spec's "`randomizeScheduler` becomes seeded
policy under DST" intends. It is free in normal builds (`randomizeScheduler` is `const false`, so the
whole branch — and the `dstActive()` call — is elided).

**Gate: `dstActive() && gomaxprocs == 1`.** The non-FIFO ring removal is safe only with no concurrent
stealer — guaranteed at one P. `simulation.Run` pins GOMAXPROCS=1, so the public API always qualifies; the
white-box `dstActivate` path at GOMAXPROCS>1 (the per-g churn tests) leaves scheduling FIFO (those
tests assert per-g RNG robustness, not scheduling order). At P=1 there is no stealer, so the unified
seam holds `sched.lock` (to touch the global runq) and uses plain ring loads/stores rather than the
steal-synchronizing CAS.

**Strategies (the `simulation.RunWith` control surface).** The strategy is a per-run choice consulted at the
unified seam, `dstSchedSelect(candidates) → index`:
- **Random** (default; `simulation.Run` and `simulation.RunWith{Strategy:Random}`): uniform pick over the runnable
  set from the scheduling RNG. Different seeds → different sound interleavings.
- **PCT** (`simulation.RunWith{Strategy:PCT, Depth:d, Steps:K}`): Probabilistic Concurrency Testing. Each
  goroutine gets a random base priority at creation (`g.dstPrio`, drawn from the scheduling RNG in
  `newproc1`, well above the change-point low band); the seam runs the highest-priority runnable
  goroutine (ties by goid, for determinism). `d−1` **priority-change points** are placed at random
  steps in `[1,K]` (re-rooted per bubble); when the step counter reaches one, the goroutine scheduled
  at that step is dropped to a low priority — the priority inversion that exposes a depth-`d` bug. PCT
  gives a probabilistic guarantee (≈ `1/(n·K^{d−1})`) of hitting a depth-`d` interleaving per seed,
  higher yield than uniform-random for deep bugs. `Steps` should approximate the run's
  scheduling-decision count: if `K` overshoots the run length the change points fall past the end and
  never fire (PCT degenerates to a fixed seeded priority order — still sound, diverse, deterministic,
  but without the depth mechanism).

Both strategies share the seam's soundness (only runnable goroutines are ever chosen) and determinism
(every draw is from the seeded scheduling RNG, advanced in a deterministic order at P=1). `g.dstPrio`
and the PCT state are unused under Random and when DST is off.

**Scheduling faults (5c) are folded into the fault-orchestration feature, not built here (deliberate,
Spec-first).** A scheduling fault splits into two shapes on opposite sides of the foreclosure line.
*Jitter* (a per-decision seeded probability to defer the chosen goroutine) is self-contained but
marginal — it largely overlaps what Random already explores and dilutes PCT's directed-search
guarantee. The valuable, distinct form for a distributed program is the *straggler* (pin a designated
node's goroutines low — "what if node 2 is slow?"), but that needs a **victim-designation** contract,
which is exactly what the undesigned fault-orchestration feature (compose net+disk+crash+scheduling
faults under one seed) owns. Building a targeting scheme now would risk a throwaway retrofit. PCT's
change points already provide seeded, sound deprioritization of whichever goroutine runs at a change
point, covering much of the "deprioritize a runnable G" ground in the meantime. So scheduling faults
land with the fault-orchestration feature, designed against its orchestration contract. The seam is ready for them: a fault is just another policy at
`dstSchedSelect`, fed through `RunWith` options.

**Empirical validation (probe suite `DSTSchedScenario`, derisked before folding in).** Five scenarios
exercise the distinct runnable-set paths — `gosched` (global runq), `spawn` (runnext/ring), `mutex`
(sema handoff), `broadcast` (`goready` fan-out via `close`), `chanring` (channel rendezvous):
- **Determinism** (same seed → 1 interleaving over 8 runs): all five, in **normal and `-race`** builds
  — upholding the unconditional logical-determinism layer of the DST contract.
- **Diversity** (10 seeds → distinct interleavings): `gosched`/`spawn`/`mutex`/`broadcast` = 10/10;
  `chanring` = 3/10. The *reduced* `chanring` diversity is a soundness signal — channel
  happens-before pins the token path (the value sequence is causally fixed), so the seam can vary only
  the rendezvous points it is *sound* to vary. Diversity scales with real scheduling freedom.
- **Soundness** (`mutexcount`): a non-atomic counter guarded only by a `sync.Mutex` reaches exactly
  G·K for every seed, normal and `-race` — the seam never runs a goroutine blocked on the mutex (it
  selects only among the runnable set), so mutual exclusion is never violated.

**The scheduling RNG is separate, not per-g.** Selection runs on **g0** (the scheduler goroutine), a
system goroutine with no application-meaningful `g.dstrand`. So seeded scheduling draws from a
dedicated **per-bubble DST scheduling RNG** (splitmix64), re-rooted per bubble exactly like the per-g
tree (`dstBubbleRoot`), advanced once per scheduling decision. At GOMAXPROCS=1 the *sequence* of
scheduling decisions is itself deterministic (1a/1b), so a single stream advanced per decision stays
a deterministic function of the seed. (At GOMAXPROCS>1 it would not be — but `simulation.Run` pins P=1.)

**Soundness (the load-bearing argument).** The seam only ever reorders Gs that are *already runnable*
— present in `runnext`/the local ring/the global runq — i.e. each is already past the primitive
(channel FIFO, mutex handoff, happens-before) that made it runnable. Their *relative* order carries
no happens-before constraint: on a real multi-core machine they could run in either order (different
Ps, or work-stealing). So every reordering the seam can inject corresponds to a real degree of
freedom → executions ⊆ real runtime+OS (the top-tier Soundness invariant). The seam *never* pulls
from a wait queue or a not-yet-readied G, so a causally-ordered pair is unrepresentable as a reorder
candidate — soundness is structural, not a runtime check.

**Collapse-check (Spec-first).** Faithful collapse of the top-tier contract: **not finer** (adds no
scheduler choice the runtime does not already make nondeterministically; only reorders genuinely
concurrent Gs — exactly the real degree of freedom), **not coarser** (the single get-side seam sits
downstream of every readying path, so it covers the whole runnable set), **not foreclosing** (5b's
strategy hook
random→biased→PCT→exhaustive and 5c's scheduling faults specialize the *same* selection seam — "pick
from the runnable set" — so seeded-random is one policy among many, no different shape needed).
Single-tier: GOMAXPROCS=1 is the only tier; no distributed/clustered collapse applies.

### Seq-5 project invariants (enforced by 5a's tests)

- **DST-SCHED-1 (entailed: soundness).** The seam selects only among the already-runnable set; it
  never reorders a G ahead of one it has a happens-before edge to. *violation:* if seeded selection
  drew from a wait queue or reordered across a channel handoff that causally orders two goroutines,
  the harness would produce an execution the real runtime cannot — a false-positive bug report while
  every documented channel/mutex ordering guarantee still holds. *Encoding:* structural (selection
  reads only runq contents) + a regression test that channel/mutex-ordered operations keep their
  order across seeds.
- **DST-SCHED-2 (clause-explicit: determinism).** Same seed → identical interleaving. *violation:* a
  seeded selection that draws from a load-dependent source (per-m RNG, or a global the system
  goroutines advance) makes the interleaving vary run-to-run, breaking replay. *Encoding:* the
  determinism probe (1 distinct over N same-seed runs), mutation-tested.

Seed-*variation* (different seeds → different interleavings) is the feature 5a delivers, asserted by a
diversity test; it is a completeness gain, not a safety invariant (a seed-invariant schedule is sound,
just incomplete).

## Level 2 — access-granularity interleaving + DPOR (systematic concurrency testing)

Status: **implemented and validated** (increments D1–D5; see Landed). This is the completeness extension the Completeness caveat names ("Completeness can
be increased additively by injecting cooperative preemption points … still deterministically — see Seq
5"), carried to its full form: make the `-race` detector's memory-access instrumentation points double
as DST scheduling decision points, so the simulation explores interleavings at **memory-access
granularity**, pruned by **Dynamic Partial-Order Reduction (DPOR)**, with the happens-before race
detector confirming races. The result is a deterministic, systematic concurrency+race explorer: a
reproducible replacement for the "run it a thousand times and hope" race lottery.

### The gap it closes (measured)

Seq 5 made *which runnable goroutine proceeds next* a seeded, strategy-driven choice — but the
**interleaving atoms** at `GOMAXPROCS=1` are still the spans *between* cooperative yield points (channel
block, select, `Gosched`, mutex contention, goroutine create). A bug that requires a context switch at a
specific instruction *between two operations that never yield* is unreachable at this granularity, for
**every** seed and **every** Seq-5 strategy. Two faults split out (the Completeness caveat's two halves):

- **Gap A — interleaving granularity.** The switch point the bug needs falls between yield points. This
  is what Level 2 closes.
- **Gap B — physical-race *consequences* under weak memory** (torn/reordered/stale reads a relaxed
  hardware/compiler memory model permits). Reproducing these needs a relaxed-memory model checker, a
  fundamentally different mechanism from interleaving — a single sequential execution cannot produce a
  reordering. This stays where the top-tier contract already puts it: **the job of `-race`**, the
  complementary tool (Completeness caveat). Level 2 does not address Gap B and is designed not to
  foreclose a later memory-model axis.

Gap A is **demonstrated, and demonstrably pure granularity** (derisk corpus, `testdata/testprog/`
under `simulation.Run`, seeds 1..N):

| corpus fault | random | PCT(d3) | oracle |
|---|---|---|---|
| unconditional data race | 40/40 | 40/40 | `-race` (caught regardless of granularity — HB sees the pair however it runs) **[V]** |
| interleaving-conditional data race | 20/40 | 19/40 | `-race` (caught on the seed fraction whose coarse interleaving makes the pair concurrent) **[V]** |
| atomicity violation (mutex-protected), **no yield in the gap** | **0/200** | **0/200** | none — `-race` stays silent (not a data race; 0/40 control), the SUT assertion never trips **[V]** |
| *identical* atomicity violation, **one yield in the gap** | **93/200** | 2–4/200 | SUT assertion **[V]** |

The two atomicity rows differ by **exactly one scheduling point**. Without it the fault is invisible to
every seed and every current strategy; with it, uniform-random finds it ~half the time. The miss is
**purely interleaving granularity** — not logic, not reachability, not search strategy. Auto-inserting
that yield point at every (shared) memory access is Level 2. (PCT finds the depth-1 atomicity violation
*less* than random — PCT's priority discipline targets depth-*d* bugs; the strategies are complementary,
and neither closes the no-yield case. **[V]**)

### Spec-first gate

- canonical: this doc's Seq-5 seam (`dstFindRunnable`/`dstSchedSelect`, `proc.go`) + the top-tier
  **Soundness**, **Non-foreclosure**, **Completeness** invariants + the control-surface table.
- contract (top-tier): *the controllable surface is "which runnable goroutine proceeds next," hooked only
  where the runtime already makes an unspecified/RNG-driven choice; **executions ⊆ real**.*
- mechanism: (D1) access-granularity cooperative yield points auto-inserted at the `-race` access hooks
  under a dst-race compiler mode, **guarded to safe points**, feeding the **same** `dstFindRunnable`
  seam; (D2) a happens-before tracker; (D3) stateless DPOR as a `dstSchedSelect` strategy; (D4) an outer
  `Explore` loop + public API; the `-race` HB detector is the oracle (D5).
- collapse-check: faithful. **Not finer** — a yield at a user memory access is a real degree of freedom
  (the OS can deschedule a goroutine between any two instructions); the guard forbids exactly the yields
  the runtime could *not* take (lock held, on g0, non-bubble), so no choice the real system lacks is
  added. **Not coarser** — adds transition boundaries, removes none of the existing coarse ones. **Not
  foreclosing** — DPOR specializes the same selection seam (random→PCT→DPOR→optimal-DPOR); access yields
  are the design's own additive completeness mechanism; the outer loop is built *on* the seam, not a new
  shape for it. Gap B (weak memory) is a different mechanism and is left to `-race`, unforeclosed.
- Single-tier: `GOMAXPROCS=1` is the only tier; the soundness collapse is "reorder only the already-
  runnable set," unchanged from Seq 5.

### Why the safe-point soundness is structural, not a runtime gamble (derisked)

The `-race` access hooks (`raceread`/`racewrite`/`racereadrange`/`racewriterange`) are compiler-inserted
**only** into functions the compiler instruments, and the compiler **excludes** the runtime and the
synchronization primitives: `runtime`, `internal/runtime/*`, `sync`, `sync/atomic` carry `NoInstrument`/
`NoRaceFunc` (`cmd/internal/objabi/pkgspecial.go`). Disassembly confirms it: **0** race hooks inside
`sync.(*Mutex).Lock`/`Unlock`, **0** inside `runtime.goyield_m`, **19** inside an ordinary user function.
**[V]** So a yield-at-access can fire only in user / non-primitive-stdlib code — **never** inside a mutex
primitive, a channel op, or the scheduler. The dangerous "yield while manipulating lock state / holding a
runtime lock" is unrepresentable by construction. The residual (e.g. a `procPin` reached from
instrumented code) is caught by the **safe-point guard**, and *skipping a yield point is always sound*
(it only reduces completeness) — so no unsound yield can ever occur. Yielding while holding a *user*
mutex is sound and is exactly the interleaving Gap A needs.

### Project invariants (Level 2)

- **DST-L2-1 (entailed: soundness).** Every injected access yield corresponds to a real degree of
  freedom; the seam still selects only among the already-runnable set. *violation:* a failure
  (split-brain, lost commit, false race) reachable only because the harness switched at a point the real
  runtime could not (lock held, on g0, a not-yet-runnable G) — a false positive while every documented
  ordering guarantee holds. *Encoding:* structural (the guard `dstActive && g.bubble != nil && g ==
  m.curg && m.locks == 0`; selection reads only runq contents) **+** a regression test that a
  mutex-protected non-atomic counter under access-granularity yielding still reaches exactly `G·K` (no
  lost update — a blocked G is never run), and channel/mutex-ordered operations keep their order.
- **DST-L2-2 (clause-explicit: determinism).** Same `(seed, schedule)` → identical execution, identical
  recorded access stream, identical verdict (race report / assertion). *violation:* replay of a fixed
  `(seed, schedule)` diverges, so a found bug is not reproducible. *Encoding:* a per-`(seed, schedule)`
  trace-hash stability probe (1 distinct over N runs, normal **and** `-race`), mutation-tested.
- **DST-L2-3 (entailed: DPOR completeness).** The explored set contains at least one execution from every
  Mazurkiewicz trace-equivalence class reachable by the SUT — equivalently, for every pair of *dependent*
  co-enabled transitions, both orderings are explored, and only provably-equivalent (independent)
  reorderings are pruned. Two transitions are *dependent* iff they record overlapping nonzero memory byte
  intervals (`dstAccessYield`/`dstAccessYieldRange`) **or** the same synchronization object's identity
  (`dstSyncAcquire`) — with ≥1 write, by different goroutines, and are not happens-before-ordered.
  (Synchronization object decision order is a dependency: omitting it drops a class — see "Completeness
  boundary".) *violation:* a non-equivalent interleaving — hence a reachable bug — is omitted, so
  "explored to exhaustion" is a false negative. *Encoding:* **`TestDSTExploreSweep`** — for a generated
  family of small closed programs (reads/writes over shared vars, with/without mutexes; plus channel
  rendezvous-order SUTs), the DPOR explored *outcome set* equals brute-force `exhaustiveExplore` for
  every member (802 SUTs — the original 290 plus the atomic, atomic-plain-mixed, multi-way, and two-variable-mixed families — mutation-tested: 23 of the original family fail with `dstSyncAcquire` neutered; 411 fail with `dstAtomicYield` neutered). The committed
  micro-SUTs (`TestDSTExploreComplete` etc.) are the weak per-shape net this generalizes.
- **DST-L2-4 (clause-explicit: production untouched).** Level-2 hooks are build-mode inert outside
  `-tags dst -race`: a non-`dst-race` build emits no compiler-inserted `dstAccessYield`,
  `dstAccessYieldRange`, or `dstAtomicYield` calls, and runtime sync-decision/HB hooks are inactive unless DST is active under
  the scheduled strategy. Runtime structs may carry inert DST fields in this fork; byte-identical layout is
  not the contract. *violation:* production, plain-`-race`, or `-tags dst` without `-race` emits or executes
  Level-2 hooks, changing behavior or scheduling. *Encoding:* a build-mode objdump test proves user code
  calls `runtime.dstAccessYield`/`runtime.dstAccessYieldRange` only when both `-tags dst` and `-race` are
  present; runtime hooks are gated by `dstBuild`/`raceenabled` and `dstActive`/scheduled-strategy guards.

### Mechanism

#### D1 — Access-granularity yield points (the new transition boundary)

A **transition boundary** under Level 2 is, at a safe point on a SUT (bubble) goroutine, either (a) an
instrumented **memory access** (read/write/range) to a *shared* address (`dstAccessYield`), or (b) a
**synchronization object decision**: mutex/RWMutex acquire, try, release, and channel
send/recv/select/close, recorded as a write-conflict on the sync object's identity
(`dstSyncAcquire`); a `sync/atomic` operation, recorded on the operand's address and real byte
width — write-conflict, or read-conflict for pure loads (`dstAtomicYield`, emitted at instrumented
call sites; see "Atomics and len(ch) are recorded transitions" under the Completeness boundary);
or a `len(ch)` observation, recorded as a read-conflict on the channel identity
(`dstSyncObserve`) — *in addition to* the
existing coarse boundaries (block/select/`Gosched`/create), which remain. At a boundary the active strategy
may switch goroutines, so the scheduler can interleave at the grain of a single access.

The sync-object decision boundary (b) is **load-bearing for completeness, not just granularity**: *which*
contending goroutine acquires/releases/closes a sync object first is a real scheduling choice that can
change the outcome, but it is decided at a transition that performs *no memory access* (a goroutine
reaching its `Lock`, `TryLock`, `Unlock`, or `close` records nothing), so without (b) DPOR treats that
decision as independent of everything and silently drops the alternative sync-decision-order Mazurkiewicz
classes (a DST-L2-3 violation — `TestDSTExploreSweep` fails 23/290 with `dstSyncAcquire` neutered).
Announced *before* the state decision/transition and modeled as a write-conflict (same-object sync
decisions do not commute), two decisions on the same object by different goroutines become a co-enabled,
concurrent, conflicting pair whose **both** orderings the existing HB-DPOR explores — with no change to the
dependency/race test. This is the standard DPOR treatment of locks, extended to non-blocking decisions and
their release/close counterparts; it is faithful (`executions ⊆ real`: the real scheduler can switch before
any goroutine changes the sync state) and sound (a pre-decision yield never runs a blocked G).

- **Where.** The `-race` hooks are the access-observation choke point. They are NOSPLIT ABIInternal
  assembly, deliberately wrapper-free to preserve caller-PC capture for reports (`race_*.s`), so they
  cannot host a splittable yield (`goyield` calls `mcall`). The yield is therefore a **Go-level hook**.
- **Auto-instrumentation: a dst-race compiler mode (DECIDED — Option 1, "separate yield call, oracle
  untouched").** Under `-tags dst` **and** `-race`, the compiler's instrument pass
  (`cmd/compile/internal/ssagen` `instrument2`) emits an **additional** call immediately **before** each
  existing race hook: `runtime.dstAccessYield(addr, isWrite)` before scalar `race{read,write}`, and
  `runtime.dstAccessYieldRange(addr, size, isWrite)` before composite `race{read,write}range`. It does
  **not** replace or reroute the race hook. The DST hook records the access byte interval and write bit and
  makes the guarded yield decision; the unchanged race hook then records in TSan exactly as upstream,
  reading its own return address off `(SP)`, so **report PC attribution and detection behavior are
  byte-identical to a stock `-race` build**. The mode is gated by a compiler flag `cmd/go` sets when both
  the `dst` tag and `-race` are present; with the flag off, `instrument2` emits the plain `race*` hooks
  exactly as upstream (DST-L2-4). Two correctness properties this buys over rerouting the race symbol
  (the rejected Option 2): **(i)** the TSan oracle is never modified — perfect attribution, identical
  detection; **(ii)** `raceread` stays `NOSPLIT`, so instrumented `//go:nosplit` functions still link —
  the *new* splittable `dstAccessYield` is simply **not emitted in nosplit functions** (the compiler
  checks `fn.Pragma&Nosplit`; skipping a yield point is always sound). `dstAccessYield` is DST's own
  transition hook — it is *not* TSan's — so the DPOR access stream rides it, not a hijacked detector
  hook. (Rejected Option 2 — replace `raceread` with a Go shim that yields then forwards to `racereadpc`
  — would make `raceread` splittable, breaking nosplit instrumented callers, and reroute every access
  through a different TSan entry with hand-derived PC bookkeeping across six arch asm files: more
  fragile under rebase and modifies the oracle. We do not optimize for upstreamability — this fork
  rebases continuously — but Option 1 is chosen on *correctness*, not upstreamability: untouched oracle +
  nosplit-safe.)
- **The yield primitive.** `goyield`-shaped: requeue the current G on the local runq and `schedule()` →
  `findRunnable` → `dstFindRunnable` (the existing seam). The current G stays runnable, so the seam never
  runs a blocked G — soundness is inherited from Seq 5 unchanged.
- **The safe-point guard** (DST-L2-1): yield only when `dstActive() && dstSchedKind == dstSchedScheduled
  && g.bubble != nil && g == g.m.curg && g.m.locks == 0` — the strategy condition confines access
  yields to Explore's scheduled strategy, so Random/PCT runs are byte-for-byte unaffected. (No separate
  reentrancy guard is needed: the hook calls only uninstrumented runtime code, so it cannot re-enter
  itself.) Any failure → record the access but do not yield (sound; reduces completeness only).
- **Shared-address filtering (tractability + faithfulness).** A yield is meaningful only at a byte interval
  ≥2 goroutines access — a private/stack/single-owner access is independent (Mazurkiewicz), and yielding
  there explores nothing new while multiplying transitions. An access is a transition iff it *conflicts*
  with a prior access by a different goroutine (overlapping byte intervals, ≥1 write) that is **not**
  happens-before-ordered before it (D2). Single-owner and HB-ordered accesses record but do not yield. This is the
  primary control on the access-granularity explosion; its magnitude is measured in increment 1. **[V]**
  Runtime implementation: under `-tags dst -race`, the DST access hooks maintain a preallocated live HB clock,
  live sync-object clocks for the same release/acquire events recorded offline, and a per-interval /
  per-goroutine epoch table. A memory access that has no prior concurrent conflicting access records into
  the per-bubble access log inline and does not call `goyield`; a conflicting access still yields and is
  logged when the goroutine is resumed, preserving commit order. Because a prior-only
  filter cannot know that a later access will need a split inside the same inline interval, the brain
   promotes observed unsafe inline accesses to forced replay yield points keyed by `(dstSeq, hook ordinal,
   hook PC key)`, where the PC key is function-name + offset based rather than an absolute address, and
   restarts the pass until no new promotion is needed. Auto-instrumented accesses to the
  current goroutine's stack log as `addr=0` (private; no conflict identity), while explicit manual/sync
  identities keep their addresses. If the bounded live filter state overflows, the runtime conservatively
  yields every later access (less pruning, never a dropped class). Non-race manual hooks remain explicit
  transition boundaries for the hand-controlled DPOR brain-validation corpus.

#### D2 — Happens-before tracking (the dependency relation)

DPOR's dependency relation needs, for any two accesses, whether they are causally ordered. The runtime
**records** the synchronization events it owns into the per-bubble transition log: ready/create edges
(`goready`/goroutine creation, the non-foreclosure choke points) and, in `-tags dst -race` builds, explicit
sync release/acquire events for real memory-model edges such as mutex `Unlock`→later successful
`Lock`/`TryLock`, channel send→receive / unbuffered receive→send-completion, and
close→closed-receive. The DPOR engine **computes the vector clocks / HB relation offline**, between Runs.
Ready edges and sync events also record the access-log length and a shared HB-event order at the moment
they fired, so same-step events are ordered against inline filtered accesses in the same interval without
conflating sync-object *decision conflicts* with synchronization HB. Sync events are replayed as object
clocks, so an acquire observes the release-time snapshot rather than the releasing goroutine's later
accesses. Buffered channel events key the object by channel plus ring slot, not by element address, so
zero-sized element slots remain distinct HB objects. The full dependency relation is still computed
offline between Runs: two conflicting accesses are **dependent** iff neither clock dominates the other
(concurrent), and HB-ordered pairs are pruned (their order is fixed and sound). The bounded live clocks are
only the conservative access-yield filter; DPOR source sets and replay decisions use the post-Run clocks.
The recorded events are the same ones the scheduler/sync primitives control, so the HB is self-contained
(no dependence on TSan's C-internal clocks); it must agree with `-race`'s own HB to remain the faithful
oracle, which the conflict-set cross-check against `-race` reports validates. Timer-fire wakeups in synctest fake time are
   validated by `TestDSTExploreTimerHB`: two goroutines sleep until the same virtual time and then race on a
   shared variable; Exhaustive reaches both read outcomes (`timerhb exh=12`), and DPOR matches them while
   exhausted (`timerhb dpor=3`, two outcomes). So the currently-recorded timer wake edges do not over-order
   that reachable timer-gated conflict shape.

#### D3 — Stateless DPOR as a `dstSchedSelect` strategy

DST already **re-executes** `simulation.Run(seed, f)` from the start per run, so **stateless** DPOR fits
exactly: each schedule is one re-execution guided by a thread-choice prefix + backtrack/sleep sets, no
stored global states.

- **Schedule representation.** A prefix `[]goid` of thread choices, plus per-decision backtrack and sleep
  sets. Goids are deterministic given a fixed prefix (per-g tree + deterministic creation order), so a
  prefix replays exactly; the suffix runs the strategy default (lowest-goid, deterministic).
- **At the seam.** `dstSchedSelect` gains a `dstSchedDPOR` branch: if the decision index is within the
  prefix, return the candidate index whose goid matches the prescribed choice; else pick the default and
  let the post-run analysis add backtracks. Installed per bubble at the synctest re-root point (the
  `dstSchedRootPCT()` slot, `synctest.go`), like every other per-bubble scheduling state.
- **Post-run analysis (the DPOR core — source-DPOR).** After each Run, walk the recorded transition
  trace; for every reversible race — a *concurrent* (sync-HB) dependent pair `(t_i, t_j)`, `i<j` — add a
  backtrack point at the pre-`t_i` decision. The backtrack is a **weak-initial** of the race witness
  (`addSourceBacktrack`), computed over the **trace happens-before** (`dporTraceClocks`), NOT `t_j`'s
  thread directly: `t_j`'s thread may be asleep at `t_i`, and a weak-initial is by construction not
  sleep-blocked for the reversal. **Sleep sets** carry each frame's already-explored / inherited threads
  (filtered by independence with the chosen transition) and skip an asleep backtrack choice, removing the
  equivalence-class re-exploration a plain backtrack set incurs. This is **source-DPOR** (Abdulla et al.
  2014): complete (no Mazurkiewicz class dropped — `TestDSTExploreSweep`) and a strong reduction toward
  one schedule per class (not the fully-optimal wakeup-tree variant, which is left unforeclosed).
- **Soundness + determinism inherited:** every decision still picks an enabled (runnable) thread
  (DST-L2-1); each Run is fully determined by its schedule (DST-L2-2).

#### D4 — The `Explore` outer loop + public API

The driver lives above the seam, orchestrating repeated Runs:

- `simulation.Explore(seed uint64, mode ExploreMode, f func() bool) ExploreResult` — runs the selected
  worklist (Exhaustive or DPOR): pop a schedule, execute `f` under it, analyze the trace, push new
  schedules, until the worklist is empty (the pruned interleaving space is **exhausted**) or a budget is
  hit. `simulation.ExploreWith(seed, ExploreOptions{Mode, MaxSchedules, MaxSteps}, f)` is the budgeted
  form. `ExploreResult` carries schedules/exhausted/overflow/budget-hit + every found failure (a `-race`
  report, a SUT assertion, a SUT panic, or a synctest deadlock) with replay metadata: the observing schedule
  plus any forced access-yield watchpoints active when the failure was observed. Top-level SUT callback
  panics are recovered in the Explore driver; unrecovered panics from SUT-created bubble goroutines or
  finalizer/cleanup callbacks on the DST drain are recorded by the runtime after the panicking goroutine's
  defers run, then that goroutine exits. Scheduled
  synctest deadlocks are recorded as `Failure.Deadlock` inside `synctestRun` before it returns, avoiding the
  unsafe outer-recover path while leaving the blocked goroutines isolated in their deadlocked bubble.
- `simulation.Replay(seed, failure, f)` replays one schedule for reproduction/debug using a `Failure`'s
  recorded schedule plus any forced access-yield watchpoints. `RunWith` remains the Random/PCT control
  surface and rejects unknown strategies instead of silently falling back to Random.
- **No silent cap (No silent downscoping):** if a budget truncates exploration, `Report` says so and how
  much was covered — "exhausted" and "budget-hit" are distinct verdicts; the latter is never reported as
  the former.

#### D5 — The race detector as deterministic oracle (**IMPLEMENTED [V]**)

Each explored interleaving runs under `-race`. The HB detector fires for an unsynchronized access pair
even at `GOMAXPROCS=1` serial execution (it is clock-based, not timing-based), and the report is a
deterministic function of the seed/schedule (same seed → 100/100 identical normalized report; even a
   *conditional* race's detect/no-detect verdict is stable per seed, 0/30 vs 30/30 — no flicker). **[V]** So
   a race found in an explored interleaving is a stable observation; exact public replay uses the schedule
   plus the access-force set recorded on the failure. Atomicity violations (invisible to `-race`) are caught
   by the SUT's own assertions in the same explored interleaving.

Wired into `Explore`: `runOnce` reads `runtime.RaceErrors()` (via the build-tagged
`dstRaceErrors` — real under `-race`, 0 otherwise) before and after each scheduled Run; each NEW race
count increment appends one `Failure` with `Race=true`. The detector dedups by signature, so each distinct
race yields exactly one `Race` failure — the first schedule that exhibits it, including any access-force set
active in that pass so `simulation.Replay` can reproduce it in a fresh process even if later passes do not
re-report due to TSan dedup. Enforced by `TestDSTExploreRaceOracle`: an
unconditional write-write race is reported (the oracle fires under `simulation.Run`), and an
*interleaving-conditional* race — manifesting only when the reader acquires a mutex first (`raceCondSUT`) —
 is found by exploring both acquisition orders and reported with a non-trivial schedule, both deterministic
 across same-seed runs. `TestDSTExploreRaceReplay` covers a race first observed under replay-promoted
 access forces and replays the returned schedule+force token in a fresh process. A non-`-race` build still
 enumerates interleavings and reports SUT-assertion failures; it records no data-race failures.

### Soundness argument (load-bearing collapse-check)

The seam still only ever reorders goroutines that are *already runnable* — an access yield merely
requeues the running G and reschedules, so every candidate is past the primitive that made it runnable,
exactly as in Seq 5. The new freedom is *when* a SUT goroutine yields (now also at shared-memory
accesses), and that freedom is real: on a real multi-core machine the OS can deschedule a goroutine
between any two instructions, so any reordering of access-separated spans is an execution the real
runtime+OS can produce. The guard forecloses precisely the points the real runtime cannot take a switch
at (lock held, on g0, non-bubble), and those are *also* the points the compiler never instruments — so
the structural exclusion and the guard agree. Therefore executions ⊆ real (top-tier Soundness), and the
seam never pulls from a wait queue, so a causally-ordered pair is unrepresentable as a reorder candidate
— soundness is structural, not a runtime check.

### Increment sequence (each useful; none forecloses another)

The ordering key: **every piece hooks to the existing `dstFindRunnable` seam or to "an access happened,"
never to a new control shape** — so the strategy (D3), the outer loop (D4), and the auto-instrumentation
(D1) compose without retrofit. **Build order (DECIDED — sequencing (b)): prove the DPOR brain (2–4) on
the *manual* hook first, then switch the transition source to auto-instrumentation (1's compiler half).**
The DPOR algorithm is the hard, research-grade core; validating it is cleaner when the transition set is
hand-controlled (and checkable against brute-force) rather than fed by every auto-inserted access.
Auto-instrumentation then only changes *where* transitions come from, not the algorithm — non-foreclosing
by the ordering key. (The `cmd/compile`/`cmd/go` work is therefore deferred until the brain is proven.)

1. **Access-yield + transition-record substrate.** Runtime `dstYieldPoint`/`dstAccessYield` + the
   safe-point guard + a per-bubble **transition recorder** (an ordered event log: scheduling decisions
   with the enabled goid set, accesses with goid/addr/size/isWrite/step, and sync events for offline HB —
   D2). **Manual-hook half: VALIDATED [V]** — mutex-counter soundness probe reaches exactly `G·K` at
   access granularity incl. yields while holding a user lock (DST-L2-1; 200/200 over 50 seeds, normal and
   `-race`, 0 spurious races), replay deterministic (DST-L2-2; 30/30 per seed), Gap A closed (110/200),
   per-run yield magnitude measured before filtering. **Compiler half
   (Option 1): IMPLEMENTED [V]** — `cmd/compile` `instrument2` (ssagen) emits an additional
   `runtime.dstAccessYield(addr, isWrite)` immediately before scalar `race{read,write}` hooks and
   `runtime.dstAccessYieldRange(addr, size, isWrite)` before composite `race{read,write}range` hooks, gated
   by the `-d=dstrace=1` debug flag that `cmd/go` sets exactly when `-tags dst` **and** `-race` are both
   present; the race hook itself is untouched (oracle byte-identical), the yield is skipped in
   `//go:nosplit` functions (`goyield` is splittable; skipping is sound), and with the flag off the pass
   emits plain `race*` with no DST access-yield call (DST-L2-4 — verified absent in non-dst and
   dst-without-race builds). An UNMODIFIED SUT (no manual hooks) built `-tags dst -race` is then explored end-to-end
   (`TestDSTExploreAutoInstrument`: the lost update is found, DPOR outcome set == Exhaustive). Foreclosure:
   feeds the same seam. **Runtime sync-primitive hooks for `dstSyncAcquire`: IMPLEMENTED [V]** — channel
   ops (`chan.go` `chansend`/`chanrecv` before `lock(&c.lock)`, `closechan` before the closed-state
   transition, identity = the `*hchan`) and mutex/RWMutex state decisions (`internal/sync.Mutex`
   `Lock`/`TryLock`/`Unlock`, identity = the mutex pointer; `sync.RWMutex` reader/writer admission and
   release, identity = the embedded writer mutex) auto-announce a `dstSyncAcquire` write-conflict. RWMutex
   suppresses the embedded writer mutex's DST happens-before events while executing RWMutex internals and
   records public RWMutex HB separately on the same `readerSem`/`writerSem` identities used by the race
   detector, so failed public `TryLock`/`TryRLock` decisions do not synchronize. An
   UNMODIFIED mutex/channel program's sync-object decision order is therefore explored with no hand
   annotation. The mutex hook is in `Lock` (NOT the `semacquire` slow path): `semacquire` is reached only by
   the *contended loser*, after the uncontended winner already took the fast-path CAS, so it is too late to
   record the winner's acquisition as a conflict — only a pre-CAS announce on the path *every* acquirer
   takes makes both orders a co-enabled conflicting pair. `TryLock`/`TryRLock` announce before failed-state
   rejection; `Unlock`/`RUnlock`/`closechan` announce before state transitions that can flip racing
   non-blocking decisions. Gated by the SAME `dstBuild && raceenabled` / `//go:build dst && race` condition
   as the memory auto-instrumentation (so non-dst, plain-`-race`, and dst-without-race builds are
   hook-inert — DST-L2-4), and confined to the scheduled strategy by the runtime guard. Acceptance:
   `TestDSTExploreSyncAutoInstrument` — unmodified mutex/channel/RWMutex/select/close/Once SUTs each reach
   BOTH sync-decision outcomes under DPOR (vs 1 with the corresponding hook neutered). *Coverage:* this
   covers `sync.Mutex.Lock`/`TryLock`/`Unlock`, `sync.RWMutex.Lock` (decision transitively via `rw.w`),
   `sync.RWMutex.RLock`/`TryRLock`/`RUnlock`/`Unlock`, blocking and non-blocking `chansend`/`chanrecv`,
   `closechan`, blocking and non-blocking `selectgo` channel send/recv paths, and `sync.Once`'s first
   execution path (transitively through its internal `Mutex`). Shared-address filtering for the
   auto-instrumentation explosion is implemented in increment 6 and enforced by
   `TestDSTExploreAutoInstrument`.
2. **Happens-before pruning — recorded events, computed offline** (D2; **VALIDATED [V]**). The runtime
   records ready/create edges (readier happens-before readied) and, in `-tags dst -race` builds, explicit
   sync release/acquire events into pre-sized per-bubble buffers under the scheduled strategy only
   (allocation-free, gated so Random/PCT/non-dst/dst-without-race are unaffected); the DPOR engine builds vector clocks **offline** from those events +
   program order (`dporClocks`/`dporConcurrent` in `explore.go`) and refines the dependency to *concurrent*
   conflicting pairs only. Mutex/channel-serialized conflicts are pruned, including non-waking edges such
   as uncontended mutex `Unlock`→`Lock`, buffered channel slot send→receive, unbuffered rendezvous, and
   close→closed-receive. Measured on `twoPairSUT`
   (two channel-ordered producer/consumer pairs interleaving freely): exhaustive 4032, address-only DPOR
   21, **HB-DPOR 4** — all 0 failures (`TestDSTExploreHBPrunes`, mutation-verified: the `<=10` bound fails
   at 21 when the concurrency test is disabled). Synthetic clock tests validate release/acquire object
   clocks, release-time snapshots, and distinct channel-slot identities. `TestExploreRecordsBufferedChannelHB`,
   `TestExploreRecordsBufferedChannelZeroSizeSlotIDs`, `TestExploreRecordsFullBufferedChannelSenderRelease`,
   `TestExploreRecordsSelectBufferedChannelHB`, `TestExploreRecordsUnbufferedChannelHB`,
   `TestExploreRecordsChannelCloseHB`, and `TestExploreRecordsMutexHB` validate the channel and mutex runtime
   hook paths under `-tags dst -race`. On pure-race SUTs (atomicity/counter, no synchronized
   accesses) HB correctly finds the conflicts concurrent and changes nothing — completeness preserved
   (`TestDSTExploreComplete` still green). Full DPOR HB remains offline; the live clocks only suppress
   access yields using the same release/acquire semantics as the recorded sync events. Foreclosure: additive
   recording.
3. **DPOR strategy** (D3; **VALIDATED [V]**). Iterative stateless DPOR over the schedule recorder
   (`dporExplore` in `testing/simulation/explore.go`): dependency = overlapping nonzero memory byte
   intervals or the same sync-object identity, ≥1 write, different stable index (`g.dstSeq`); backtrack
   points added at ancestor decisions; deterministic backtrack pick. Measured against the Exhaustive
   baseline: **sound** (mutex-counter 0 failures over the
   whole space — `TestDSTExploreFindsAtomicityViolation`), **complete** (counter race: DPOR and
   Exhaustive reach the *identical* outcome set — DST-L2-3, `TestDSTExploreComplete`: 2-goroutine
   `{1,2}` committed; 3-goroutine `{1,2,3}` measured in bring-up), **deterministic + seed-independent**,
   and a real reduction (atomicity 180→10; 2-goroutine counter 180→10; 3-goroutine counter 20160→539).
   Foreclosure: a strategy at the seam.
4. **`Explore` outer loop + API** (D4; **VALIDATED [V]**). `simulation.Explore(seed, mode, sut)` drives
   repeated bubble re-executions; `runOnce` follows a prefix and copies out the trace. Exhaustive and
   DPOR modes share the loop. Reports `Schedules`/`Failures`/`Exhausted`/`Overflow`/`BudgetHit`
   (exhausted vs budget-hit distinct — no silent cap), top-level and child-goroutine SUT panics as
   `Failure.Panic`, synctest deadlocks as `Failure.Deadlock`, and one `Failure.Race` per new `RaceErrors`
   increment. The scheduled post-`go` boundary is active in non-race builds too, so assertion-only
   child-before-parent-continuation failures are not silently skipped. Child-goroutine and drain-callback
   panics are recorded in the runtime after ordinary defers; scheduled deadlocks are converted inside
   `synctestRun` before `Run` returns.
   *(After 4: increment 2's HB pruning + increment 6's filtering
   cut the still-inflated counts; then 1's compiler half so real SUTs need no hand-annotation.)*

**As built so far (this session) — the substrate + brain are proven on the manual hook:** the stable
per-bubble index (`g.dstSeq`, lazily assigned at first candidacy — goid is process-global and drifts
across re-executions, so it cannot key a replayable schedule), the allocation-free recorder
(`dstScheduledSelect` runs on g0 under `sched.lock`), the transition-boundary hooks (`dstAccessYield` for
memory accesses, `dstSyncAcquire` for synchronization object decisions — D1), and the Exhaustive + DPOR
engines. Full landed DST suite stays green (`ok runtime`, normal and the scheduling subset). The remaining
trace entries are real conflict/coarse decisions after increment 6 filtering; soundness + completeness for
SUTs whose accesses and sync-object decisions are recorded
is independent of that inflation, and enforced over a generated family by `TestDSTExploreSweep`.

**Completeness boundary (addr=0 transitions).** DPOR's dependency relation pairs transitions that record
a nonzero conflict identity: a shared **memory access** (`dstAccessYield`) **or** a **synchronization object
decision** (`dstSyncAcquire`, recording the sync object's identity as a write-conflict — D1).
Transitions that record none — pure infrastructure decisions (goroutine creation, `WaitGroup` wakeups,
the isolated `gcDrain` finalizer goroutine) — remain independent of everything, which is correct because
they carry no outcome-determining order choice a recorded access/sync-object decision does not already capture
(the created goroutine's own accesses, the post-`Wait` accesses, … are the recorded transitions). So the
relation is sound and complete *for SUTs whose shared memory accesses **and** synchronization
object decisions are recorded as transitions, and that do not observe finalizer/cleanup timing* — enforced
over a generated family (mutex- and channel-decision-order cases included) by the
`TestDSTExploreSweep` equivalence sweep (DPOR outcome set == exhaustive, 802 SUTs).

**Atomics and len(ch) are recorded transitions (former named exclusions — closed).** `sync/atomic`
operations are decision points: the dst-race compiler mode emits `dstAtomicYield(addr, width, kind)`
immediately before each static `sync/atomic` call in instrumented code — the atomic implementations
are NOSPLIT race assembly that cannot host a yield, so this is the D1 Option-1 shape (an ADDITIONAL
call, TSan untouched) applied at the call-site choke point. Both forms are classified: the free
functions and the typed-API methods — the latter are load-bearing because `sync/atomic` is a
noRaceFunc package whose functions are NOT inlinable under `-race`, so a typed call stays out of
line and only the instrumented call site can announce it (the typed structs' zero-size
noCopy/align64 prefixes put the value at offset 0, so the receiver address IS the atomic's
address). Loads announce as read-conflicts (load pairs commute); stores/RMWs/CAS as write-conflicts
on the operand's real byte width, so atomics also pair with PLAIN accesses of the same memory.
After the yield returns — the op commits next, with no further yield — the hook records the op's
happens-before contribution for offline-DPOR pruning and the live filter clocks: Load acquire,
Store release, RMW acquire+release (acquire first, so RMW chains stay transitively ordered), and
**CAS acquire-only** — it always observes but may write nothing, and the hook runs before the
outcome is known; the missed release of a successful CAS only forgoes pruning. The *effective*
release semantics are cumulative, not the literal per-op observed-by edge: release clocks MERGE
on the object (a load after two stores acquires both stores' history, though it observed only the
last — a deliberate over-approximation on the load side; TSan's own AtomicStore overwrites rather
than merges, so this is strictly more ordered than TSan for W→W→R), while the storer side
deliberately under-approximates (a pure Store records no acquire). Where
this merged model claims an edge the strict observed-by reading would not (the W→W→R pattern, or
a failed CAS if it over-claimed a release), the explorer's architecture masks the difference
rather than losing a class: same-address atomic announces always form a conflicting pair, so the
release/acquire ops are always reorderable, and the per-trace re-analysis of the reordered trace
drops the claimed edge — verified by an outcome-equivalent over-claim mutant across the sweep
(including the two-variable A5 family built to defeat it) and a crafted probe. CAS stays
acquire-only as the faithful floor so no MORE of the model rests on that masking than the merge
semantics already do; the enforced contract is the sweep's DPOR == Exhaustive equivalence.
`len(ch)` announces the channel identity as a READ-conflict in `chanlen` (`dstSyncObserve`),
pairing with the ops' write-conflict announces; `cap(ch)` is NOT hooked — capacity is immutable
after `make`, so a cap read carries no ordering decision (the earlier draft of this boundary named
cap as an exclusion to close; it is a non-decision, not an exclusion). Remaining boundary — atomic call forms that record NO transition: (1) calls from inside a
non-instrumented package (the runtime's own atomics; a norace package's internal call sites when
not inlined into instrumented code) — the sync packages' primitives carry their own decision
hooks; (2) DYNAMIC call forms, where the callee is not a static symbol the emission can classify:
interface dispatch on an atomic value, method values (`f := x.Load; f()` — the generated `-fm`
wrapper lives in the noRaceFunc sync/atomic package), and func-valued free functions
(`f := atomic.LoadInt32`); (3) instrumented `//go:nosplit` callers (the yield is splittable;
skipping is sound but unexplored) and embedded-promotion tail-call wrappers. A SUT whose
outcome-determining atomic is reached ONLY through these forms under-explores exactly as the old
exclusion did; the direct static forms — overwhelmingly the way atomics are written — are covered. Enforced by the atomic/mixed/multi-way
sweep families (DPOR == Exhaustive, including the conservative-HB pruning) and
`TestDSTExploreAtomicAutoInstrument` (unmodified SUTs: CAS winner, store/swap order, add-vs-load,
And/Or, 64-bit and pointer widths, typed-API wrappers, len-vs-send — each Outcomes==2,
Exhausted==true).

An **earlier draft of this note was wrong**: it claimed completeness for "SUTs whose shared *accesses* are
all annotated," overlooking that **synchronization object decision order is itself a dependency**. A
mutex-bracketed program with every access annotated still lost a class (`prog#257`: exhaustive 2
outcomes, DPOR 1) because the lock-order-determining decision is an `addr=0` transition. `dstSyncAcquire`
(D1) closes it with no change to the dependency/race test; the sweep enforces it (23/290 → 0). For the
manual-hook validation phase the SUT annotates sync-object decisions; the dst-race compiler/runtime phase
records them automatically — **now implemented** (increment 1): channel ops, close, and mutex/RWMutex
state decisions auto-announce `dstSyncAcquire` under `-tags dst -race`, so an unmodified mutex/channel SUT's
decision order is explored (`TestDSTExploreSyncAutoInstrument`). The mutex hook is
in `Lock` before the CAS, in `TryLock` before even the locked-state rejection, and in `Unlock` before the
release that can flip a racing `TryLock`, not the `semacquire` slow path (which the uncontended fast-path
winner never reaches — too late to record its acquisition as a conflict). `RWMutex.RLock`/`TryRLock` announce
on the same writer-mutex identity as `RWMutex.Lock`, including failed `TryRLock` admission attempts;
`RWMutex.Unlock`/`RUnlock` announce the same identity before release transitions that can flip racing try
attempts. Blocking and non-blocking select channel cases announce each candidate channel before taking
channel locks, `closechan` announces before the closed-state transition that can flip a non-blocking receive,
and `Once` is covered by its internal mutex.
`TestDSTExploreSyncAutoInstrument` now covers mutex, channel, RWMutex reader-vs-writer, TryLock,
failed TryLock/TryRLock decisions, release-vs-try decisions, blocking and non-blocking select send/recv,
close-vs-receive decisions, and Once winner order under `-tags dst -race`.
Finalizer-timing observation
stays out of scope until *every* access is recorded; the `dporExplore` dependency loop documents the
relation.
5. **Source-DPOR — sleep sets + weak-initial source sets** (D3) — **VALIDATED [V].** The former
   `dporExplore` was a *persistent-set* DPOR: sound + complete, but it re-explored Mazurkiewicz-*equivalent*
   interleavings via different prefixes (the per-frame `done` set precludes exact-duplicate prefixes, so
   the residual redundancy is equivalence-class). Source-DPOR (Abdulla, Aronis, Jonsson & Sagonas, POPL
   2014) removes it, tightening DST-L2-3 from "covers every class" toward "explores no class twice".
   Foreclosure: refines the worklist, not the seam. **This was the riskiest remaining algorithmic piece** —
   a wrong sleep set silently drops a class (DPOR misses a reachable bug while still reporting
   `Exhausted=true`); the micro-SUT completeness tests are a weak net for it. The generated-family sweep
   (`TestDSTExploreSweep`, step 1) is the real net, and it earned its keep twice during this build (see
   step 2). **Build order for this increment (done in this order):**
   1. **Validator first** (so the safety net exists before the algorithm): a brute-force-equivalence
      sweep — a *generated family* of small SUTs (vary goroutine count, accesses, sync) — asserting the
      DPOR explored *outcome set* equals `exhaustiveExplore`'s for every member, not just the committed
      micro-SUTs. This is the real DST-L2-3 guard. **VALIDATED [V] — and it immediately earned its keep:**
      it exposed a *pre-existing* DST-L2-3 completeness defect (the persistent-set DPOR dropped every
      **synchronization-object-decision-order** class in the sweep — 23 SUTs of the then-290, all-and-only
      mutex/channel cases), because the lock/rendezvous-order decision is an `addr=0` transition the dependency relation
      ignored. **Fixed before sleep sets** (a reduction layered on an incomplete search would drop even
      more): `dstSyncAcquire` (D1) records sync-object decisions as conflicting transitions — zero brain change —
      and the sweep (`TestDSTExploreSweep`) is now the enforcing artifact (23/290 → 0). Optimality (sleep
      sets) is therefore built on a foundation the sweep proves complete.
   2. **Source-DPOR (sleep sets + weak-initial backtracks) — VALIDATED [V].** Each `dporFrame` gains a
      `sleep` set: a frame inherits the parent's asleep + already-explored goroutines, FILTERED by
      independence with the transition the parent chose (a commuting goroutine stays asleep, a dependent
      one is woken), threaded through the `for d := len(stack); d < n` extension loop; an asleep backtrack
      choice is skipped. Sleep alone is INCOMPLETE with a crude backtrack rule — **the sweep caught it**
      (a write between two independent reads dropped a class, prog#273). The fix is **source sets**: when a
      reversible race (concurrent dependent pair e_i, e_j) is found, add a **weak-initial** of the witness
      to `backtrack(i)` (`addSourceBacktrack`) rather than e_j's process directly, so the reversal survives
      sleep-pruning. The witness/weak-initials require the **TRACE happens-before** (`dporTraceClocks` —
      transitive closure of the dependency relation, ordering conflicting accesses), NOT the sync-only HB
      (`dporClocks`): **the sweep caught the sync-HB version too** (an independent read became a spurious
      weak-initial; prog#274/276). With trace-HB the sweep is complete. Two relations coexist by design:
      the reorderability GATE uses sync-HB (`dporConcurrent` — a conflicting pair with no sync between them
      is reorderable); the weak-initial/`notdep` uses trace-HB. Sleep independence is deliberately
      coarser than the dependency relation: sleep-set entries are carried across stateless
      re-executions, but raw access addresses are run-local (the same logical object can receive a
      different numeric address after explorer-side allocations). So `addr=0` transitions and read/read
      pairs commute; any nonzero pair with at least one write wakes the sleeper. The race gate still uses
      per-run interval overlap + HB. This can only under-prune, never keep a dependent transition asleep
      and drop a class.
   3. **Adversarial review** (incompleteness failure mode + determinism/soundness) — run per the
      Adversarial loop on the change set.
   Measured (at the then-290-program standing sweep, mismatches=0, including the timer-gated HB SUT;
   the sweep now stands at 802 with the atomic families, still mismatches=0): source-DPOR vs
   the persistent-set baseline cuts the worst-program schedule count `maxDpor` 125→69; current totals are
   `totExh=13414`, `totDpor=1965`; a
   370-program run (incl. the timer-HB SUT, 3 goroutines × 2 ops, and 4-way contention; exhaustive up to
   2520/program), reproducible on demand via `DSTSWEEP=heavy`, is also complete (12.5× reduction,
   161254→12895). Payoff is
   modest on tiny SUTs (persistent-set is already near-optimal there), larger as independent transitions
   multiply. `TestDSTExploreSweep` enforces both completeness (mismatches=0) and the optimality bound
   (`maxDpor` < 80; persistent-set regresses to 125, dropping sleep to 85). When the source-set witness has
   no enabled weak-initial at a decision (every process that could lead to e_j is blocked until e_i runs),
   the reversed order is UNREACHABLE from that state and `addSourceBacktrack` adds no backtrack — skipping,
   not the all-enabled add (which under sleep sets could drop a class), and not a panic. The hand-annotated
   family sweep never reaches this path (`fallbacks=0`); the dst-race compiler's denser auto-instrumentation
   does (a read-modify-write whose read and write both yield), and `TestDSTExploreAutoInstrument` validates
   that skipping there stays complete (DPOR outcome set == Exhaustive). (An earlier draft wrongly asserted
   this path unreachable and panicked; auto-instrumentation showed it is reachable.)
6. **Infrastructure isolation + shared-address filtering** (D1, using D2) — **VALIDATED [V].** *gcDrain isolation* —
   **VALIDATED [V]**: the bubble's finalizer-drain goroutine is scheduled RNG-free as infrastructure
   under the scheduled strategy (`firstSystemG`), so it leaves the recorded schedule/DPOR search; cut the
   exhaustive count ~9× (e.g. counter exhaustive 180→20) with no change to DPOR (it already pruned
   gcDrain as addr=0) and no effect on Random/PCT (`TestDSTSchedSystemIsolation` green). *Shared-address
   filtering proper* (only contended, non-HB-ordered addresses are transitions) is now implemented for the
   dst-race auto-instrumented stream at the runtime hook: all accesses are logged, but single-owner/
   HB-ordered accesses do not yield. The live filter uses bounded preallocated clocks/tables and falls back
   to yield-every-access on overflow, so overflow can only under-prune. The brain promotes any observed
   conflicting inline access that needs a missing boundary to a forced yield on replay; race-enabled DPOR
   uses conservative conflict-anchor backtracking while disabling sleep sets, and the non-race sweep keeps
   full source-DPOR sleep-set pruning. Non-race manual hooks remain explicit transitions, so `TestDSTExploreSweep` continues to
   validate the hand-controlled DPOR brain (`mismatches=0`). Validation:
   `TestDSTExploreAutoInstrument` validates filtered auto-instrumented RMWs by checking DPOR==Exhaustive,
   preserving the known `{1,2}` outcome set, and keeping Exhaustive tractable (plain RMW: 19,448 before
   filtering → 49 after; private-noise RMW: 49 after, outcome set preserved). It also checks filtered
   R/W/R shapes with four outcomes (`rwrExh=1580`, `rwrDpor=51`, `manualRWRExh=159`, `manualRWRDpor=6`),
   range-vs-field identity (`rangeExh=24`, `rangeDpor=2`, two outcomes),
   and post-`go` / post-wake parent-write shapes (`createExh=4`, `createDpor=2`, `wakeExh=25`,
   `wakeDpor=10`) so first accesses after goroutine creation or `goready` wake-up do not hide
   child-before-parent classes.
   Foreclosure: narrows
   transitions; fewer yields is sound because skipped accesses are logged and only independent accesses are
   filtered.

### Open questions (resolved by measurement during the build, not pre-judged)

- **Explosion magnitude (RESOLVED).** Shared-address filtering reduces the auto-instrumented RMW exhaustive
  count from the measured ~19,448 baseline to 49 while preserving outcomes; a private-noise RMW is also
  49. `TestDSTExploreAutoInstrument` enforces both tractability (`<1000`) and outcome preservation.
- **HB source (DECIDED).** DST-side, computed **offline** from recorded sync events (not live hot-path
  clocks, not TSan's C-internal clocks) — it must match `-race`'s own HB, cross-checked against `-race`
  reports.
- **Budget policy when the space exceeds the budget (IMPLEMENTED).** `ExploreWith` reports partial
  coverage with `Schedules` plus `BudgetHit=true`; `Exhausted` is false — never a silent cap (DST-L2-3 /
  No silent downscoping).

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
  "Deterministic GC for DST" below. Current state (Tier 2, landed): GC stays enabled in-run with the
  deterministic per-object trigger, STW mark + synchronous sweep, and the bubble-scoped finalizer &
  cleanup drain; the cross-Run `sync.Pool` reap is folded into the in-bubble run-end fixpoint. The
  full scope and tiers are written up so the depth of fix is a deliberate choice, not a default.
- **Upstreamability**: whether the runtime knobs are kept as a fork patch or shaped to be proposable
  upstream (the `randomizeScheduler`-as-knob framing is the most upstream-friendly).

## Deterministic GC for DST (full scope)

Goal: GC under DST that is **deterministic**, **production-faithful** (the SUT sees real GC semantics —
finalizers run, weak refs clear, memory is bounded, memory-pressure behaviour is present), and
**general** (works for *any* program, not only blocking-heavy, finalizer-light ones). "It works for one
program today" is not the bar — the fork's DST must work for any program built with it.

### Why GC is nondeterministic under DST (derisked findings)

Empirical: with GC on, a heavy-alloc multi-goroutine bubble produced ~19 distinct interleavings/40
runs (same seed); `GODEBUG=gcstoptheworld=1` only cut it to ~10 — so STW alone is not enough.
Two independent sources:

1. **The pacer trigger is wall-clock-coupled.** `gcControllerState.endCycle` computes `consMark` from
   `assistDuration = now - markStartTime` (`mgcpacer.go:604`), the wall-clock mark duration, which
   feeds the next GC trigger heap goal (`mgcpacer.go:1327`). So *when* GC fires (which allocation
   point) varies run-to-run. Fractional worker scheduling also reads `nanotime` (`mgcpacer.go:859`).
2. **Concurrent mark interleaves** with runnable goroutines when GC runs while the bubble is *not*
   quiescent.

Validated *safe/deterministic* (3 reader agents + probes):
- GC **at a synctest quiescence point** (every bubble goroutine durably blocked) is deterministic —
  the mark worker runs alone (no runnable app goroutine to interleave with). Probe: 1 distinct/30,
  idle and under load.
- It does **not** corrupt bubble accounting: GC-subsystem goroutines (mark workers, `fing`, `bgsweep`,
  `bgscavenge`) are system goroutines with `g.bubble==nil` (`proc.go:5390` skips bubble inheritance),
  `changegstatus` only touches `gp.bubble` (`proc.go:1322`), and `gcStart` dissociates the caller's
  bubble for the GC (`mgc.go:746`).
- `runtime.GC()` is safe from the `synctestRun` quiescence context, works with `gcPercent<0`
  (`gcTriggerCycle` is independent of it), and mark/sweep is app-deterministic (mark order affects
  perf, not survival; weak-handle clearing is deterministic during sweep).

### The dimensions a *full* design must cover

Each dimension lists: the nondeterminism/hazard, what a general fix needs, and the gap a
quiescence-only MVP leaves.

1. **Trigger.** Wall-clock pacer (above). General fix: a deterministic trigger — a fixed heap-ratio
   (mimic GOGC without the wall-clock `consMark` feedback) *or* quiescence-driven. MVP gap: quiescence
   only fires when the bubble blocks, so it can't bound an alloc-heavy *non-blocking* span (see 11).
2. **Mark/sweep concurrency.** Concurrent mark interleaves when goroutines are runnable. General fix:
   force STW (`gcstoptheworld`) for any *mid-execution* GC; at quiescence it's already alone. MVP gap:
   MVP only GCs at quiescence, so it never needs STW — but also never GCs mid-execution.
3. **Finalizers — determinism.** `fing` runs finalizers asynchronously and unsynchronized with the
   deterministic schedule; *how many* have completed by a given point varies (probe: 7 vs 8 of 8).
   This is invisible **unless the SUT observes finalizer effects** (memory, or a side effect), but a
   *general* SUT may. General fix: **synchronous finalizer drain** at the deterministic GC point (run
   the queued finalizers on a controlled goroutine before resuming), not async `fing`. Order within
   the queue is already deterministic (reverse-LIFO, `mfinal.go:220`) given deterministic discovery.
4. **Finalizers — bubble-awareness.** `fing.bubble==nil`, so a finalizer that does **any channel op on
   a bubble channel fatals** ("channel from outside bubble", `chan.go:193/540`) — confirmed 5/5.
   General fix: run finalizers *in the owning object's bubble context* (set the running g's bubble to
   the object's bubble for the call), or exempt finalizer-context from the check. Required for any SUT
   whose finalizers touch channels.
5. **Finalizers — arbitrary behaviour.** A finalizer may allocate, spawn goroutines, or block. A
   synchronous drain must tolerate all three (a blocking finalizer must not deadlock the drain; a
   spawned goroutine must be accounted to the right bubble). General fix must define the contract.
6. **Cleanups (`runtime.AddCleanup`, Go 1.24+).** The modern finalizer replacement; same determinism +
   bubble-awareness considerations as finalizers. Must be handled identically.
7. **Weak pointers (`weak`).** Cleared during sweep in deterministic (offset) order
   (`mgcsweep.go:574`); deterministic given a deterministic GC schedule. Mostly free once GC is
   deterministic — confirm.
8. **Concurrent sweep (`bgsweep`).** Racing sweepers could make finalizer/weak *discovery* order vary.
   General fix: synchronous/serialized sweep under DST.
9. **Scavenger (`bgscavenge`).** Timer-driven memory return to the OS; logically transparent but runs
   nondeterministically. Likely disable under DST for cleanliness; confirm it's not observable.
10. **GC assists.** A goroutine allocating during a GC cycle does pacer-proportioned assist work
    (`mgcpacer`), wall-clock-influenced. With STW mid-execution GC there's no concurrent assist; with
    quiescence-only GC there's no in-flight cycle to assist. Confirm no assist path remains
    nondeterministic.
11. **Alloc-bound / non-blocking spans (the hard one).** A bubble goroutine that allocates heavily
    **without ever blocking** never reaches a quiescence point → quiescence-only GC never fires → heap
    grows unbounded (probe: 195MB and climbing). A *general* SUT can have compute/alloc-heavy phases.
    Handling this **forces mid-execution GC**, i.e. a deterministic *heap-triggered* GC (dimension 1's
    heap-ratio + dimension 2's STW), not merely quiescence. This is the single biggest reason the full
    scope is larger than the MVP.
12. **Memory-pressure-adaptive SUTs / `GOMEMLIMIT` / `ReadMemStats`.** If the SUT changes behaviour
    based on memory (cache eviction under pressure, `GOMEMLIMIT`-driven GC, reading `MemStats`), then
    GC *timing* becomes app-observable and must be deterministic *and* production-plausible — the
    heap-ratio trigger's numbers matter, not just their determinism. A program sized by explicit config
    (not GC pressure) is fine here; a memory-pressure-adaptive one may not be.

### Tiers (choose deliberately)

- **Tier 0 — superseded (was the stopgap).** GC off during a run + `sync.Pool` reap on `simulation.Run` return. *Unsound*: no
  in-run finalizers, unbounded intra-run memory, relies on a Pool-victim-cache implementation detail.
  A working stopgap, not a design.
- **Tier 1 — quiescence GC.** Force GC at synctest quiescence points; leave `fing` async. Deterministic
  and memory-bounded **for blocking-heavy SUTs that neither observe finalizer timing nor run
  channel-touching finalizers**. Covers dimensions 1–2 (at quiescence), 7–10 (largely
  free). **Leaves** dimensions 3–6 (finalizer determinism/bubble-awareness/cleanups), 11 (alloc-bound),
  12 (memory-pressure). Constraints must be documented, not hidden. **Does *not* delete the Tier-0
  `sync.Pool` reap** — that reap is a *cross-Run pool-lifetime* fix (a channel `Put` in Run 1 is `Get`
  in Run 2 with the wrong bubble stamp), orthogonal to *in-run* memory bounding: in-run quiescence GC
  does not fire after `f`'s final `Put`, so the pooled object survives to the next Run regardless. The
  reap (or an equivalent forced end-of-Run GC) stays until something else evicts cross-Run pool state.
  See "Interaction with the Tier-0 pool reap" below.
- **Tier 2 — current (landed).** Deterministic **heap-ratio-triggered STW GC** that can fire
  mid-execution (dimensions 1, 2, 11) + **synchronous, bubble-aware, deterministic finalizer & cleanup
  drain** (3–6) + synchronous sweep (8) + scavenger off (9) + deterministic assists (10). Works for any
  SUT. This is a substantial, GC-internals-deep change and the riskiest in the whole DST effort; it can
  be built incrementally (e.g. finalizer drain before the heap-trigger) but the *design* should be
  whole so increments don't foreclose each other.

### Tier 2 design (concrete)

The DST-faithful GC is the **collapse of production GC under the DST preconditions** (`GOMAXPROCS=1`,
async/sysmon preemption off, single bubble per `Run`). It is *not finer* than production (it adds no
collection capability the real GC lacks), *not coarser* (finalizers run, weak refs clear, memory is
bounded, `MemStats`/`GOMEMLIMIT` stay live), and *not foreclosing* (every piece hooks to **"a GC
cycle completed,"** never to *what triggered it*, so the two trigger sources compose — see Increments).

Canonical contracts this collapses: the GC pacer/trigger (`mgcpacer.go`, `mgc.go`), the sweep/finalizer
machinery (`mgcsweep.go`, `mfinal.go`, `mcleanup.go`), and the synctest bubble (`synctest.go`,
`chan.go`). All exist and are settled upstream, so there is no undesigned top-tier to foreclose against.

Validation legend: **[V]** empirically validated (prior derisking / probe); **[C]** argued from
construction, to be confirmed empirically when that piece is built; **[R]** confirmed by reading the
cited code this round.

#### D1 — Deterministic trigger (dimensions 1, 2, 11)

The heap trigger fires when `gcController.heapLive.Load() >= trigger` (`mgc.go:712`), where
`trigger, _ = gcController.trigger()` returns `goal - runway` (`mgcpacer.go:1245`). Of the inputs to
`trigger()`:

- `goal = gcPercentHeapGoal = heapMarked + (heapMarked + lastStackScan + globalsScan)*gcPercent/100`
  (`mgcpacer.go:1295`) — **deterministic** at `GOMAXPROCS=1` given a deterministic allocation
  sequence (every term is a byte counter advanced in fixed amounts). **[R]**
- `runway = consMark * (1-gcGoalUtilization)/gcGoalUtilization * (lastHeapScan+lastStackScan+globalsScan)`
  (`mgcpacer.go:1327`) — the **only** wall-clock-derived term: `consMark` comes from
  `assistDuration = now - markStartTime` in `endCycle` (`mgcpacer.go:604`). **[R]**

**The contract is OBSERVABLE determinism, not a byte-exact trigger.** D1 originally proposed zeroing
`runway` (the only wall-clock-derived trigger input: `consMark` carries `idleMarkTime`, and under
`gcForceBlockMode` idle mark workers still run because DST forces the *mode* but not
`debug.gcstoptheworld`, so the pacer never zeroes them — `idleMarkTime` is nonzero and varies
run-to-run). **Both the override and the no-override path were instrumented (Chunk A).** Findings:

> - **The runway override does not make the trigger deterministic.** With `runway:=0`, the trigger value
>   (`gcController.trigger()`, recorded at each `gcStart`) *still* varied run-to-run, because the trigger
>   also depends on `heapLive`/`heapMarked` (via `goal` and `sweepDistMinTrigger`, `mgcpacer.go:1287`),
>   which carry their own run-to-run accounting noise — process heap history at bubble entry plus
>   internal allocations — that cascades through the trigger feedback loop. **[V]**
> - **`numGC` (the GC *count*) is deterministic under STW** regardless of the override (churn workload:
>   17 every run with STW; grow workload: 15 every run). Without STW it can flip by ±1 at some GOGC
>   (e.g. GOGC=100 churn: 20 vs 21) because concurrent mark "allocate-black" floating garbage varies with
>   wall-clock mark timing — see D2. **[V]**
> - So the wall-clock `runway` term is **sub-observable**: it perturbs the trigger by less than the
>   accounting noise already present, and below `numGC` granularity. Pinning it buys no determinism the
>   accounting noise doesn't already deny, and removes none the system needs.

**Decision: no runway code.** Byte-exact trigger determinism is neither achievable (heapLive accounting
noise) nor the contract. What DST guarantees and what Chunk A tests is **observable** determinism:
allocation results, scheduling, and `numGC` (bounded + stable). The earlier "[V] STW makes consMark
deterministic (assistTime=idleMarkTime=0)" claim was **wrong** — `idleMarkTime` is nonzero; that line
is corrected here. (Revised invariant **DST-GC-1**: *under `dstActive`, GC introduces no nondeterminism
into observable program behaviour — allocation results, scheduling, and the GC count; the exact trigger
byte carries sub-observable accounting noise and is explicitly out of scope.*)

Both trigger *sources* feed the same downstream cycle:
- **Heap-ratio** (above) — fires mid-execution from `mallocgc` (`malloc.go:1341/1453/1593/1687/1760`),
  the only thing that bounds an **alloc-heavy non-blocking span** (dimension 11; probe: 195 MB and
  climbing under quiescence-only GC). **[V]**
- **Quiescence** — fires from the `synctestRun` driver loop right after the bubble is detected durably
  blocked (`synctest.go:223`, after `gopark(synctestidle_c, …)`); the existing Tier-1 prototype hook.
  Handles the blocking-heavy common case and runs the cycle while nothing else is runnable. **[V]**

Use **both**: heap-ratio for the alloc-bound axis, quiescence so memory is reclaimed promptly at
natural blocking points (and so a blocking-heavy SUT's heap trajectory matches Tier 1).

#### D2 — STW mark + synchronous sweep (dimensions 2, 8, 10; precondition for D1/D3/D4)

Run every DST GC stop-the-world (the existing `gcstoptheworld=2` / `gcForceBlockMode` path,
`mgc.go:789-797`), gated on `dstActive()` rather than the GODEBUG. `gcForceBlockMode` does mark **and**
sweep with the world stopped (`mgc.go:673`; the synchronous-sweep special case is `mgc.go:2092`), so
this single mode-forcing delivers, in one move: **`numGC` determinism** (no concurrent mark → no
"allocate-black" floating garbage whose volume varies with wall-clock mark timing → the GC count is
stable, where concurrent GC flips it ±1 — D2 below), D3 (synchronous sweep → deterministic finalizer/
weak *discovery*, the load-bearing reason, tested in Chunk B), and dimension 10 (assists):
`gcAssistAlloc1` is gated on `gcBlackenEnabled` (`mgcmark.go:716`), set only during concurrent mark, so
no mutator reaches the assist path under STW. **[R]** (It does **not** make the trigger *byte*-exact —
see D1; that is not the contract.)

**What STW is and is not load-bearing for — empirical (Chunk A mutation testing).** The earlier claim
that STW is needed to stop *concurrent mark from reordering mutators* did **not** survive testing: at
`GOMAXPROCS=1` + `asyncpreemptoff` + the per-g RNG, GC is **transparent to mutator scheduling** for
deterministically-scheduled workloads — CPU-bound and channel-based probes are bit-identical across
runs with or without STW (a removed-STW mutation survived every such probe). The one mutator
nondeterminism reproducible at single-P is **`runtime.Gosched`-contention** (several simultaneously
runnable goroutines racing for the run queue), and it is **GC-independent**: it diverges with GC fully
*off* too. That is a **Seq 5** concern (ordering simultaneously-runnable goroutines), out of scope for
the GC work; GC *amplifies* it but STW does not fix it. So STW's load-bearing roles here are the
**demonstrable** ones: deterministic finalizer/weak **discovery** (D3/D4 — tested in Chunk B), assist
elimination (above), and a **safe in-bubble GC** (no concurrent GC system goroutine competing for the
bubble's single P). Its mutator-scheduling effect is not relied upon. This is why Chunk A's test
(`TestDSTGCAllocBoundDeterministic`) asserts the **demonstrable** Chunk A invariant — `NumGC>0`, i.e.
the heap trigger fires and bounds memory (dimension 11) — and STW's own teeth-test lives in Chunk B.

**Is the heap-trigger crossing point deterministic?** **No — and this is fundamental** (corrected by
Chunk A round-2 instrumentation). The schedule up to the crossing is deterministic, but `heapLive`
accounting is **not** byte-deterministic at `GOMAXPROCS=1`: it advances in span-granular jumps, and span
boundaries relative to the allocation stream depend on the **heap layout** (span addresses from the
process's `mmap` history), which varies run-to-run. So `heapLive` crosses `trigger` at a slightly
different allocation each run (measured: first-GC `heapLive` 4000312–4005784, a ~5 KB spread), and
`heapMarked` (the live set captured at the mark instant) wobbles with it (206728 vs 212048). This noise
is **not** wall-clock-derived and **cannot** be removed by any pacer change (it is the dropped runway
override's true reason for failing — D1). It is below the granularity of the **observable** contract
(`numGC`, allocation results, scheduling all stay deterministic — D1), but it has one real consequence
for finalizers, handled in **D4 (discovery-cycle scoping)**. The STW cycle itself is correct and does
not corrupt bubble accounting: `gcStart` dissociates the caller's bubble (`mgc.go:746-755`) and
GC-subsystem goroutines carry `bubble==nil` (`proc.go:5390`). **[V]**

#### D3 — Synchronous sweep before the drain (dimensions 7, 8)

Finalizers are **queued during sweep pass-2** (`mgcsweep.go:574-590`), the same pass that clears weak
handles — so `finq` is complete and weak clearing is done only **after** sweep finishes. **[R]** Sweep
order is span-class-linear with no RNG and no wall-clock (`mheap_.nextSpanForSweep`,
`mgcsweep.go:96-117`), hence deterministic given a fixed heap. **[R]** Production sweep is lazy/
concurrent (`bgsweep` + proportional mutator sweep via `deductSweepCredit → sweepone`,
`mcentral.go:85`, `mcache.go:254`), which would let finalizer/weak *discovery* order float.
**Mechanism:** under DST, drive sweep to completion **synchronously inside the GC step** —
`for sweepone() != ^uintptr(0) {}` (the `finishsweep_m` idiom, `mgcsweep.go:231-239`) — before waking
the drain, so `finq` is whole and weak clearing (dimension 7) is finished and deterministic. **[C]**

#### D4 — Bubble-scoped deterministic finalizer & cleanup drain (dimensions 3, 4, 5, 6)

The unifying invariant — the load-bearing piece of Tier 2:

> **Invariant (DST finalizer drain).** Finalizer and cleanup callbacks run on a goroutine whose
> `g.bubble == ` the Run's bubble, scheduled by the deterministic scheduler, woken at the deterministic
> **quiescence** point (not at GC-completion — see "Where it runs / when it drains" below, corrected by
> Chunk A round-2). Never on the async system `fing` (`runFinalizers`) or the async cleanup pool
> (`runCleanups`). Scope — **ownership by registration**: the drain executes exactly the callbacks
> registered by THIS run's bubble goroutines among those a simulation GC discovers in-run (a
> run-owned object discovered only after the run finalizes on the ordinary async workers, as always;
> each special carries a run-epoch stamp,
> `dstCallbackEpoch`, recorded at `SetFinalizer`/`AddCleanup`). A callback registered before the run,
> by a goroutine outside the bubble, by a foreign synctest bubble, or by a previous run is
> process-level work: even when a simulation GC discovers it mid-run, queue-time routing
> (`queuefinalizer`/`cleanupQueue.enqueue`) defers it past `dstDeactivate` with the pre-bubble queues
> and it executes on the ordinary async workers afterward. The drained set is therefore a pure
> function of the run's own activity — foreign registrations can neither appear in it nor advance the
> drain's per-g RNG stream (`TestDSTForeignCallbackDeferred`).

Why this is the faithful collapse, dimension by dimension:

- **Determinism (3).** Async `fing` completes a nondeterministic *count* by any given point (probe: 7
  vs 8 of 8). **[V]** Within a finq block the order is already reverse-LIFO
  (`mfinal.go:218-220`) **[R]**; cleanups are block-LIFO and, at `GOMAXPROCS=1`,
  `maxCleanupGs = max(GOMAXPROCS/4, 1) = 1` so the "concurrent" pool collapses to a single goroutine.
  **[R]** Re-hosting both onto one deterministically-scheduled bubble goroutine makes *when each runs*
  a function of the schedule, not wall-clock.
- **Bubble-awareness (4).** A finalizer doing a channel op fatals under async `fing` because
  `fing.bubble == nil` and the check is `c.bubble != nil && getg().bubble != c.bubble → fatal`
  (`chan.go:193/319/418/540/703`); a channel adopts its maker's bubble at creation (`chan.go:116-117`).
  **[R]** **There is exactly one bubble per `Run`** (`Run` is non-nestable), so the drain goroutine
  simply carries that bubble and *every* bubble channel op inside a finalizer succeeds — **no
  per-object bubble tracking** (the dimension-4 "adopt the owning object's bubble" collapses to "adopt
  the one bubble"). This is the *structural* form: the illegal state (finalizer in the wrong bubble) is
  unrepresentable because there is only one bubble to be in. **[R]**
- **Arbitrary behaviour (5).** On a bubble goroutine: a finalizer that **allocates** may re-cross the
  heap trigger → nested STW GC → more finq; the drain loops `for finq != nil` so it absorbs its own
  garbage and terminates when allocation stops producing it. A finalizer that **blocks** on a bubble
  channel parks the drain goroutine; another bubble goroutine wakes it — deterministic, and faithful
  (production's `fing` blocks too, just stalling all later finalizers). The driver's quiescence wake is
  **wait-reason-checked** (`dstDrainParked`): it goreadys the drain only when the drain is parked at its
  own drain park, never while it is blocked inside a callback (waking a g parked in a channel wait
  corrupts that wait's sudog queue); a pending-work quiescence that finds the drain callback-blocked
  skips the wake — sound, because the drain loops until the queues are empty when the callback's wait
  completes. A drain still callback-blocked at Run end is reported by the `total != 1` deadlock check. A
  callback that **panics** (recorded by Explore) or calls **runtime.Goexit** kills the drain: the death
  clears the driver's reference (a dead g must never be woken) and every callback the run queued or
  later discovers is **deterministically discarded** at quiescence and at Run-end teardown
  (`dstDiscardQueuedFinq`/`dstDiscardQueuedCleanups`, accounted so the queue ledger stays exact) — never
  leaked to the bubble-less async workers (DST-FIN-1/DST-CLEANUP-1). A finalizer that **spawns** a
  goroutine: the child inherits `g.bubble` via `newproc1` (normal goroutines inherit; only system
  goroutines skip it at `proc.go:5390`), so it is bubble-accounted and deterministically scheduled.
  **[R]/[C]**
- **Cleanups (6).** Identical treatment: drain the cleanup queue (`gcCleanups`, enqueued from
  `freeSpecial` at `mheap.go:2810` during sweep) on the same bubble goroutine, in block order. **[R]**

**Where it runs / when it drains — at quiescence, not at every GC (corrected by Chunk A round-2).** A
single **persistent per-`Run` drain goroutine** (`synctestGCDrain` — finalizers+cleanups), started inside the bubble by
`synctestRun` (like `bubble.main`, via `newproc1`, so `g.bubble` is the Run's bubble and it dies with the
bubble — no cross-Run leak), parked when idle. It is woken to drain **at synctest quiescence points** —
*not* after every heap-triggered GC. Heap-triggered GCs (which fire mid-burst to bound memory, dimension
11) sweep and **queue** finalizers/cleanups but do **not** wake the drain; the accumulated `finq`/cleanup
queue is drained when the bubble next reaches quiescence (the `synctestRun` driver calls
`(*synctestBubble).dstDrainAtQuiescence` right after the quiescence `gopark(synctestidle_c)` returns,
which runs the quiescence GC and `goready`s the drain). `goready` of the drain from the driver is a plain
enqueue. The drain runs the accumulated callbacks, re-parks. Hooking the drain to **quiescence** (not to
GC-completion) is what makes finalizer *timing* deterministic — see the discovery-cycle invariant next.

**The one classification the build must get right.** The drain's idle park must count as a **durable**
block for synctest quiescence accounting (`changegstatus`) — otherwise a bubble with an idle drain
goroutine never reaches `running == 0`, and the *quiescence* trigger never fires. It is legitimately
durable: the drain is woken only by the driver at quiescence. **As built:** a dedicated wait reason
`waitReasonSynctestGCDrain`, added to `isIdleInSynctest`. Additionally, the drain's start function
is `runtime`-prefixed, so it must be identified (by start-PC) in `isSystemGoroutine` as a *user*
goroutine, or `newproc1` would withhold `g.bubble` from it. **[C]**

> **Driver-stall hazard (why not run finalizers on the root).** The `synctestRun` driver goroutine
> (`bubble.root`) also has `g.bubble` set and *could* run the drain inline at quiescence — but a
> finalizer that blocks would stall the driver and wedge time advancement. Hence a **separate** drain
> goroutine, never the root. **[R]**

**Discovery-cycle scoping — what "deterministic finalizer discovery" can and cannot mean (Chunk A
round-2 finding).** D2 established that the heap-trigger **crossing point wobbles** run-to-run (heap
layout noise), so the live set captured at a *mid-burst* GC's mark instant (`heapMarked`) varies — a
finalizable temporary that is live at one run's mark instant but already dead at another's is
**discovered in a different GC cycle**. Measured: mid-run finalizer-run count 54661 / 54787 / 55417
across runs of the same seed. This is **not** curable by the drain (it is about *which cycle queues* an
object, upstream of the drain) nor by the dropped runway override (it is heap-layout noise, not
wall-clock). So the achievable invariant is **set-at-quiescence, not per-cycle**:

> **Invariant (DST finalizer discovery).** At any **quiescence point**, the set of finalizers/cleanups
> that have run equals the set of objects unreachable from the **quiescent live set** — which is
> deterministic (all goroutines durably blocked at deterministic points). The *cycle* in which a given
> object was discovered during a preceding non-quiescent burst, and the *order* among finalizers in one
> drain (discovery order across cycles + heap addresses, both heap-layout-dependent), are **not**
> guaranteed. A SUT that observes finalizer effects only at quiescence boundaries (the natural
> `synctest` observation points) sees a deterministic set; one that depends on per-cycle discovery
> timing or on finalizer *order* is relying on behaviour production also leaves unspecified.

Draining **at quiescence** (above) is what realizes this: by a quiescence point every mid-burst GC's
queue plus a fresh quiescence GC have been folded in, so the drained set is the deterministic dead set.
The eventual finalized set (by `Run` end) is fully deterministic — confirmed: all objects' finalizers
run. **[V]** Weak-pointer clearing (dimension 7) inherits the same scoping: cleared sets are
quiescence-deterministic; the exact clearing cycle for a boundary object is not. This is the honest
contract; it is **faithful to production**, which specifies neither finalizer timing nor order.

**As built — exactly one GC per quiescence, not a fixpoint (Chunk B).** `dstDrainAtQuiescence` runs a
*single* fresh STW GC, then drains what it queued — it does **not** loop GC+drain to a fixpoint. This is
the faithful realization of the invariant above: an object reachable only through another finalizable
object's *still-pending* finalizer is marked alive by that GC (kept for the pending finalizer), so it is
**in** the quiescent live set and must not run at this quiescence; its finalizer runs at a later
quiescence once the earlier one has run — exactly how production resolves finalizer **chains** across
successive GC cycles. The full set is finalized by `Run` end. (A per-quiescence GC+drain fixpoint would
over-run relative to the invariant *and* loop forever on a finalizer that re-registers itself with
`SetFinalizer`.) The single `gopark` after the drain still waits for any bubble goroutine a finalizer
*unblocked* — e.g. a finalizer that sends on a channel another goroutine receives on — to run and
re-block before virtual time advances.

**Run-end fixpoint (resolves the chain-tail hazard).** The single-GC-per-quiescence rule is right
*during* the run (it matches production chain resolution), but at `Run` **end** there is no later
quiescence, so a chain whose **tail** touches a bubble channel and is dropped near the end (object B
reachable only through object A's still-pending finalizer) would otherwise be left for post-teardown
async `fing`/cleanup processing (`g.bubble == nil`), and the tail's channel op would fatal. So
`dstStopGCDrain` loops GC+drain until a GC discovers nothing new
(`dstDrainAtQuiescence` returns whether it made progress), resolving the whole chain **in-bubble**
before teardown. The loop has no finite round cap: every finite chain completes in the bubble; a callback
that continually re-registers itself is a non-terminating callback workload (like a user goroutine that
never durably blocks) and the run does not complete rather than leaking the residual to a bubble-less
async goroutine. It is sound because the SUT has exited (everything is dead, so running the full chain is
correct) and changes no in-run quiescence behavior; the cleanup drain (Chunk C) is covered identically
(the loop checks both `finPending` and `cleanupPending`). Regressions: `DSTFinChain` (a 3-level chain with
a channel-touching tail) fatals after teardown without the fixpoint; the long-chain tests
(`DSTFinLongChain`, `DSTCleanupLongChain`) require a >256-level tail to run while `dstActive` is still true.

#### D5 — Scavenger off (dimension 9)

`bgscavenge` is timer/`nanotime`-driven (`mgcscavenge.go:507` `slept = nanotime() - start`, plus the
`s.timer` sleep) and so nondeterministic, but
logically transparent (it returns free pages to the OS; it changes RSS, not program semantics). **[R]**
**Mechanism:** under DST, park it permanently (gate `scavenger.wake`/`ready`, or set its goals
unreachable). Observable only by a SUT that reads RSS — see D6. **[C]**

#### D6 — Memory-pressure faithfulness & `GOMEMLIMIT` (dimension 12); upstreamability

Because the per-bubble relative trigger (A.5) reuses production's GOGC ratio, a memory-pressure-adaptive
SUT sees a **production-plausible** GOGC heap trajectory: the heap grows to the GOGC ratio of the
bubble's live set, then STW GC. `NumGC` under GOGC is deterministic (the GC-set-level guarantee), and
`ReadMemStats` is deterministic at observable granularity for `NumGC`; `HeapAlloc`/`NextGC` carry the
sub-observable byte-noise of the heap trigger (D2), so a SUT that branches coarsely on `MemStats`
replays, one that compares them byte-exactly may see noise. Weak-pointer clearing (dimension 7) is
deterministic at the set level — confirmed in Chunk D (`TestDSTWeakClearingDeterministic`).

**`GOMEMLIMIT` and RSS stats under DST — resolved by Chunk G.** The *env* `GOMEMLIMIT` still cannot be
honored deterministically: A.5 replaced the production heap goal (`min(gcPercentHeapGoal,
memoryLimitHeapGoal)`) with a *GOGC-only* bubble-relative trigger, and `memoryLimitHeapGoal` derives
from `mappedReady` (total mapped memory), which is **not bubble-local** and **nondeterministic** under
DST (~115 KB run-to-run: mmap-arena history + ASLR + scavenger-off accumulation); honoring it makes
`NumGC` wobble (8/9 at a tight limit). GOMEMLIMIT's semantics are inherently *total-RSS*, which DST
does not model (the scavenger is parked, D5). Two fixes give the user a deterministic equivalent:
- **`Options.MemoryLimit`** — a per-run knob that bounds the bubble's *own* net heap, expressed as the
  per-object deterministic measure `bubbleMarked + dstHeapAlloc` (live-at-last-mark + per-object bytes
  allocated since), the deterministic analogue of physical `heapLive - dstHeapBase`. This is bubble-local
  and per-object deterministic, so both `NumGC` *and which cycle discovers each object* under the limit
  are reproducible in normal and `-race` builds (`TestDSTMemoryLimit` for the set level;
  `TestDSTGCPerCycleDiscoveryDeterministic`'s memlimit regime for per-cycle; the trigger in `mgc.go`
  `gcTrigger.test`). Redefined semantics under DST: *bound bubble net heap growth*, not *bound total
  RSS*. It is an upper bound on top of the GOGC trigger; when `GOGC=off` it is the sole bound (the
  `defaultHeapMinimum` floor is skipped so a limit set above it is honored).
- **RSS-derived `MemStats` are out-of-contract** — `HeapReleased`, `HeapIdle`, and `HeapSys`'s idle
  component carry `mappedReady`/sweep-`madvise` process noise that is not bubble-local, so they are
  **not** deterministic under DST and a SUT must not assert on them (same status as the init-time
  boundary: nondeterminism the SUT must not read, not nondeterminism the fork hides). An earlier version
  zeroed these fields to make `ReadMemStats` reproducible; that was removed as a SUT-accommodation — it
  *falsified* a runtime value (there are idle spans; it reported 0) to serve a SUT that read fields the
  contract already steers away from, and it didn't even cover the `Sys`/`*Sys` fields that jitter the
  same way. The honest contract is the one below: only the *set-level* observables are deterministic. A
  SUT that sizes by memory should branch on `NumGC` and use `Options.MemoryLimit`, never `HeapReleased`/
  `HeapIdle` (and only coarsely on `HeapAlloc`/`HeapInuse`, which carry the sub-observable byte noise of
  the heap trigger — D2).

**So that memory is always bounded** regardless of config, the DST trigger falls back to a fixed
`defaultHeapMinimum` floor when `GOGC=off` (`gcPercent < 0`), instead of never firing — a GOGC=off
bubble that allocates is then still *deterministically* bounded (`TestDSTGCOffMemoryBounded`) rather
than growing without limit (production would have relied on GOMEMLIMIT here, which DST cannot honor
deterministically). `defaultHeapMinimum`, the constant, not `gcController.heapMinimum`, which is
`defaultHeapMinimum*GOGC/100` and overflows to garbage when `GOGC=off`.

> **Invariant DST-MEM-1 (observable memory determinism).** Under DST, the GC-set-level memory
> observables a SUT can read — `NumGC` (under GOGC or `Options.MemoryLimit`) and the set of weak pointers
> cleared — are a deterministic function of the seed. *Violation:* a SUT branches on `NumGC` or on which
> weak refs cleared and sees different values across runs of one seed. *Enforced:*
> `TestDSTGCAllocBoundDeterministic`, `TestDSTWeakClearingDeterministic`, `TestDSTMemoryLimit`.
> (Out-of-contract — *not* deterministic, the SUT must not assert on them: the RSS-derived fields
> `HeapReleased`/`HeapIdle`/`HeapSys`-idle, the byte-level live-heap fields `HeapAlloc`/`HeapInuse`, the
> process-total `Sys`/`*Sys` fields, and `NumGC` driven by the *env* `GOMEMLIMIT` — all carry process or
> sub-observable byte noise; a SUT sizes by `NumGC` and `Options.MemoryLimit` instead.)
>
> **Invariant DST-MEM-2 (always memory-bounded).** Under DST, a bubble that allocates is
> deterministically memory-bounded for *any* GOGC/GOMEMLIMIT config: the heap trigger always fires for
> sufficient bubble growth — the GOGC-relative target when `GOGC>=0`, the `defaultHeapMinimum` floor when
> `GOGC=off`. *Violation:* a `GOGC=off` bubble allocates without bound (no in-run GC) and the simulation
> OOMs. *Enforced:* `TestDSTGCOffMemoryBounded`.

**Upstreamability.** The whole collapse is expressible as a `gcstoptheworld`-style mode —
`GODEBUG=gcdeterministic=1` ≈ {STW (already `gcstoptheworld`) + fixed `runway` + synchronous sweep +
in-schedule finalizer/cleanup drain + scavenger off}. The STW and synchronous-sweep pieces already
exist upstream; the new, narrowly-scoped knobs (fixed runway, drain hook) are the upstream-friendly
framing if this is ever proposed beyond the fork. Under DST it is gated by `dstActive()`, not a
separate GODEBUG.

#### Interaction with the Tier-0 pool reap

In-run GC (Tier 1/2) bounds **intra-Run** memory but does **not** subsume the Tier-0 inter-Run
`sync.Pool` reap. `sync.Pool` entries are evicted only by `poolCleanup` at GC start (2-generation
victim cache → two GCs to fully clear). The cross-Run hazard is: `f` does its final `Put(ch)` (ch
stamped with Run 1's bubble) and returns; **no GC fires between that `Put` and Run 2's `Get`**, so the
stale-bubble channel survives and Run 2's `Get` fatals. In-run GC, by definition, stops at `f`'s end.
So Tier 2 **retains** an end-of-`Run` reap (two quiet pool generations, sized for the 2-generation
cache) as a *pool-lifetime* mechanism distinct from in-run memory bounding. The reap is folded into
the in-bubble run-end fixpoint before `dstDeactivate`, so objects made unreachable only by Pool
eviction have their finalizers/cleanups drained with the bubble still active. Only a change to pool
*lifetime* across bubbles (out of scope here) would remove it. **[R]**

#### Increment sequence (each useful; none forecloses another)

The ordering key: **the drain hooks to "the bubble reached quiescence," never to a GC trigger** — so the
mid-burst heap trigger can be added independently of the drain, and the drain's determinism rests on the
deterministic quiescent live set, not on a deterministic trigger byte (which does not exist — D2).

1. **STW forcing + GC enabled in-run** (D2; **Chunk A — landed**). Force `gcForceBlockMode` under
   `dstActive`; stop disabling GC in `simulation.Run`; park the scavenger (D5). No runway code (D1: the override
   was tried and dropped as ineffective). Delivers memory bounding (dimension 11) and observable
   determinism (`numGC`, alloc, sched). Tests: `TestDSTGCAllocBoundDeterministic` (numGC>0 + cross-run
   identity). Foreclosure check: none — STW is the safe in-bubble default and the precondition for 2/4.
   (Synchronous sweep, D3, comes free with `gcForceBlockMode`, `mgc.go:2092`.)
2. **Quiescence GC hook** (D1 quiescence source; **Chunk B — landed**). At the `synctestRun` driver
   quiescence point (`synctest.go`, after the `gopark(synctestidle_c)` at the loop top), run **one** fresh
   STW GC so the live set drained next is the deterministic quiescent set. Depends on 1. Foreclosure
   check: none — an added trigger site into the same STW path. **As built:** the GC is run by
   `(*synctestBubble).dstDrainAtQuiescence`, called from the driver right after the quiescence `gopark`
   returns, before virtual time advances; merged with step 3 (the GC and the drain wake are one call).
3. **Bubble-scoped drain — finalizers** (D4 for `fing`; **Chunk B — landed**). Persistent per-`Run`
   bubble goroutine (`synctestGCDrain` — named for finalizers+cleanups after Chunk C; created once in
   `synctestRun` so it does not perturb the root's DST RNG stream); new idle-classified wait reason
   (`waitReasonSynctestGCDrain` in `isIdleInSynctest`); identified as a user goroutine by start-PC in
   `isSystemGoroutine` so `newproc1`
   gives it the bubble. Realizes the **discovery-cycle invariant** (set-at-quiescence). Depends on 1+2.
   Foreclosure check: the drain hooks to *quiescence*, so the mid-burst heap trigger (5) needs no change
   to it. **As built, two refinements:**
   - *`fing` gated at the wake/dequeue sites, not in `queuefinalizer`.* `queuefinalizer` still sets
     `fingWake` (harmless); the scheduler's wake of `fing` (`proc.go` `findRunnable`) and `fing`'s own
     dequeue loop are gated by `dstCallbackWorkersBlocked()` (`dstActive` or the pre-active
     `dstPreparing` pass), so during a Run `finq` accumulates and only the drain drains it. The predicate
     is inert for non-DST because `dstBuild` folds false.
   - *Drain exit handshake.* The drain is a bubble goroutine and counts toward `bubble.total`, so it must
     exit before the `total != 1` deadlock check; `dstStopGCDrain` runs a final drain, sets `gcDrainExit`,
     and waits for the drain to die (invariant DST-FIN-3).
4. **Bubble-scoped drain — cleanups** (D4 for `mcleanup`; **Chunk C — landed**). The *same* drain
   goroutine (`synctestGCDrain`), same quiescence wake, drains `gcCleanups` after `finq` via a factored
   `runCleanupBlock`/`dstDrainCleanups`; `cleanupPending` joins `finPending` in the wake decision; the
   quiescence GC's sweep already flushes per-P cleanup blocks (`mgcsweep.go`). Depends on 3. Foreclosure
   check: none. **As built — the async pools are fully dormant during activation and Run:** the finalizer
   goroutine `fing` and the cleanup-pool goroutines must neither *run* nor be *created* during a Run,
   because either would run a callback with `g.bubble == nil` (fatal on a
   bubble channel op) or, on creation via `go`, draw from the creating goroutine's DST RNG stream and
   persist across Runs (breaking reproducible-in-isolation). So: gate the `fing` wake (`proc.go`) and
   worker dequeue/callback loop (`mfinal.go`) with `dstCallbackWorkersBlocked()`; gate `createfing`
   (`mfinal.go`) under `!dstActive()`; gate the cleanup wake (`proc.go`) and worker dequeue/callback loop
   (`mcleanup.go`) with `dstCallbackWorkersBlocked()`; and gate `createGs` (`mcleanup.go`) under
   `!dstActive()`. The wake/dequeue gates matter when an async goroutine pre-exists the Run; the create
   gates when the first callback of its kind is inside the Run. Pre-bubble finalizers/
   cleanups are excluded before `dstActive`: activation sets a short `dstPreparing` gate so ordinary async
   callback workers cannot dequeue new work, performs two ordinary GC queue-detach passes before storing the seed,
   detaches any queued process-level blocks from the queues the bubble drain observes, snapshots run-local
   queued/executed baselines so detached queued work and prior-run
   callbacks already executing on async workers cannot poison the run-end fixpoint, and releases detached
   work back to the ordinary async pools after `dstDeactivate`. If an async worker is already inside a
   pre-bubble callback when the run starts and that callback later returns, the worker parks before running
   the next callback or dequeuing another block, then resumes after deactivation. If callbacks run before the
   run, they observe `dstActive=false`; if they remain deferred, they cannot enter the run's first
   quiescence drain.
   (`createfing` is the one gate not independently testable in the harness — fing pre-exists from a stdlib
   import; same mechanism as the tested `createGs`.)
5. **Mid-burst heap trigger semantics** (dimension 11 finalizer interaction; **Chunk B — landed**).
   Heap-triggered GCs already fire (step 1); they **queue** finalizers without waking the drain — which
   falls out of the step-3 design directly: nothing wakes the drain except the quiescence hook, so a
   mid-burst GC's queued finalizers simply wait in `finq` for the next quiescence drain. Depends on 3.
   Foreclosure check: none — it only gates *who wakes the drain*.
6. **Scavenger off** (D5). Folded into step 1 (one-liner). Listed for dimension completeness.
7. **Memory-pressure validation** (D6; **Chunk D — landed, with a correction**). Validated: `NumGC`
   under GOGC and weak-pointer clearing are deterministic (`TestDSTGCAllocBoundDeterministic`,
   `TestDSTWeakClearingDeterministic`). The verification **disproved** D6's original `GOMEMLIMIT` claim:
   A.5's relative trigger dropped the `memoryLimitHeapGoal` term, and that goal (and `HeapReleased`)
   derive from non-bubble-local `mappedReady`, which is nondeterministic under DST — so `GOMEMLIMIT` is
   ignored and RSS stats are nondeterministic (open question, filed). Added: a `defaultHeapMinimum`
   floor so a `GOGC=off` bubble is still deterministically memory-bounded (`TestDSTGCOffMemoryBounded`).
   See D6 above.

Tier 1 ≈ steps 1–2 + 6 (memory-bounded, observably deterministic, finalizers still async — the
documented Tier-1 limitation). Tier 2 = all. Because the drain (3–4) hooks to quiescence and the
mid-burst trigger (5) only changes who wakes it, the two compose without a throwaway retrofit — the
non-foreclosure property the design is organized around. **Chunk A landed step 1; Chunks B/C/D build
2–7.**

#### Investigation RESULT — per-bubble relative trigger makes finalizer discovery deterministic ✅

**Outcome: the heap-layout noise IS reducible, cheaply, and per-cycle finalizer discovery determinism
is achievable.** A throwaway prototype (instrumented overlay; reverted) established it empirically. This
**supersedes the set-at-quiescence scoping** of D2/D4 above for the trigger mechanism — those sections'
"byte-exact unachievable / discovery only set-at-quiescence" framing is the *fallback* if the mechanism
below is not adopted; with it adopted, the drain may run **per-GC** and discovery is per-cycle
deterministic.

**As built (Chunk B), discovery is per-cycle but the drain still runs at quiescence — these are
separate axes.** A.5's relative trigger (adopted) makes *discovery* per-cycle deterministic: which GC
cycle queues a given object is a function of the seed, **in the contract** since Phase 2a (the
per-object trigger made it robust to -race and binary composition) and enforced by
`TestDSTGCPerCycleDiscoveryDeterministic` — see the layered-contract section below. That is independent
of *when the queued finalizers run*. Chunk B runs them on the drain
**at quiescence**, not per-GC, deliberately: a per-GC (mid-burst) drain would execute user finalizers
while SUT goroutines are mid-execution (between cooperative yields), interleaving finalizer side effects
with the SUT; at quiescence every SUT goroutine is durably blocked, so finalizers run in isolation and
the run *set* is the deterministic quiescent dead set. So per-cycle discovery determinism (A.5) and the
quiescence drain (D4, "corrected by Chunk A round-2") compose: the cycles that queue are deterministic,
and the drain runs the accumulated set deterministically at the next quiescence.

**Root of the crossing wobble (measured).** The trigger is roughly *absolute* (≈`heapMinimum`), and
`heapLive` at **bubble entry** (`base`) varies run-to-run (349 KB–416 KB; process heap history before
the bubble). So the bubble must allocate `trigger − base` to cross, and that varies with `base`
(hypothesis (a) — baseline — dominant; span-overshoot (b) is a minor ±span residual). It is **not**
address/ASLR (`heapLive` is a byte count).

**The fix (prototype, proven).** A **per-bubble relative trigger with per-cycle re-baseline**:
- snapshot `dstHeapBase = heapLive` at bubble entry;
- fire the heap trigger on `heapLive − dstHeapBase ≥ target` instead of the absolute pacer trigger
  (`gcTrigger.test`, `gcTriggerHeap`);
- after each GC's sweep completes, **re-baseline** `dstHeapBase = heapLive` (in `gcMarkTermination`
  after `gcSweep`), so every cycle measures the bubble's own *net* allocation from the last GC's end.

The re-baseline is essential: relative-to-*entry* alone fixes only the first GC (the first GC collects
entry-time garbage, shifting the baseline); re-baselining per cycle makes every cycle's crossing the
bubble's deterministic `target`.

**Measured determinism (same seed, throwaway hashes of the per-cycle finalizer-queue sequence):**
| workload | absolute trigger | relative + rebase |
|---|---|---|
| churn (single g) | 3 distinct / 8 | **1 / 10** |
| ring buffer, varied lifetimes (single g) | 4 distinct / 6 | **1 / 10** |
| ring buffer, **5 goroutines + Gosched** | (n/a) | **1 / 12** |

Finalizer **discovery is set-based**, so it is even robust to the `Gosched`-ordering nondeterminism
(Seq 5 gap): the *set* of objects dead at each deterministic mark does not depend on interleaving order.
A small ±span `heapLive` residual remains (the GC-crossing hash had 2 distinct values in one churn run)
but it is **sub-discovery** — it did not move the finalizer-queue sequence in any tested workload.

**Open sub-decision — GOGC faithfulness vs determinism (Spec-first collapse-check).** The prototype used
a **fixed** `target` (4 MB net growth per cycle). That is deterministic and sound but **not GOGC-faithful**
for a large/growing live set (it collects every 4 MB regardless of live size, where GOGC would scale
with it). Scaling `target` with the bubble's live set the obvious way — `bubbleLive·GOGC/100` with
`bubbleLive = heapMarked − base` — **reintroduces the wobble**, because `heapMarked` includes the
process baseline live, which varies. A GOGC-faithful *and* deterministic target needs a **clean
per-bubble live measure**: force a GC at bubble entry so `base` is process-*live* (not entry garbage),
then `bubbleLive = heapMarked − base` is deterministic if the process baseline is stable during the
bubble. For a program sized by explicit config (not GC pressure — D6) the **fixed target suffices**; a
memory-pressure-adaptive one wants the GOGC-scaled-with-entry-GC version. Either stays a faithful
GOGC collapse (fixed = a floor; scaled = the real ratio); neither is *finer* than production.

**Decision for the build (supersedes the prior plan):** adopt the per-bubble relative trigger as the DST
heap trigger, so finalizer/weak **discovery is per-cycle deterministic**. Discovery and callback
*execution* are separate axes (see "As built (Chunk B)" above): discovery tightens to per-cycle, while
the drain continues to run at quiescence, where callbacks execute in isolation against a quiescent
bubble. DST-GC-1 and D4 tighten from set-at-quiescence to **per-cycle discovery determinism**.

**A.5 — IMPLEMENTED (GOGC-scaled, full-faithful).** Landed as the GOGC-scaled-with-entry-GC version
(the production-faithful option, user-chosen):
- `dstActivate` forces a full GC at bubble entry (STW under DST) and snapshots `dstHeapBase =
  gcController.heapMarked` — the process *live* set, with pre-bubble garbage collected so the baseline
  is not polluted by entry garbage the first in-bubble GC would otherwise free (which would drive
  `heapMarked` below `base`). (`runtime/dst.go`.)
- `gcTrigger.test` (`gcTriggerHeap`, `mgc.go`) under `dstActive` fires when
  `dstHeapAlloc ≥ max((heapMarked − dstHeapBase)·GOGC/100, heapMinimum)` — the production GOGC rule with
  the scaling term on the bubble's *own* live (`heapMarked − dstHeapBase`), excluding the
  run-to-run-varying process baseline, but driven off **`dstHeapAlloc`** (per-object allocated bytes since
  the last GC) rather than physical `heapLive` (Phase 2a — see the layered-contract section for why). No
  per-cycle rebase is needed: `base` is fixed at entry and `heapMarked` updates each cycle, so the target
  tracks the bubble's live set faithfully.
- Regression tests over the permanent `dstBubbleFinqFP` hook (the bubble-local total finalizers
  discovered, `finqueued − dstFinqBase`): `TestDSTGCFinalizerDiscoveryDeterministic` asserts the
  set-level finalizer discovery (`numGC` + total) is reproducible. The `dstHeapBase` baseline subtraction
  itself — the part that cancels the run-to-run-varying pre-bubble heap — is mutation-guarded by
  `TestDSTMemoryLimit`'s **baseline-independence** check: `numGC` under a fixed limit must not change when
  a large heap is retained *before* the run (16 MiB). Dropping the baseline (absolute trigger) lets that
  pre-bubble heap inflate the live total, so `numGC` explodes (9 → >16000) and the check fails. (The
  discovery workload's sparse `numGC` is robust to the baseline offset, so it is not the test that pins
  it — a gap the byte-exact-per-cycle hash papered over before it was removed; the new check closes it.)
  The per-cycle `dstFinqSeq` probe used during bring-up was removed when the discovery test went
  set-level.

**Validation + the Seq-5 boundary (measured).** The entry-GC baseline holds: `bubbleMarked =
heapMarked − base` is identical across runs (12/12) — the GC trajectory is deterministic. Finalizer
discovery is then **fully deterministic for non-contended workloads** (single goroutine, ring buffer:
20/20). A **multi-goroutine + `Gosched` contention** residual once measured here (~15 %; with GC
entirely off, a race-free interleaving observable was 4 distinct/10) was proven to be **the
scheduling-order axis, not the GC**: removing the `Gosched` made the *same* workload 20/20
deterministic. That residual is now **closed** by the system-goroutine-isolation fix (above): the
remaining nondeterminism was infrastructure goroutines consuming the bubble's scheduling RNG a
timing-varying number of times; isolating them makes the runnable order — and thus *which* objects sit
in each per-goroutine structure at the (deterministic) GC instant — a pure function of the seed even
under contention. **Scope of the A.5 tighten:** per-cycle discovery is deterministic *given* a deterministic
runnable order; under contention it is an interleaving-sensitive observable, dependent on that order
(the scheduling-order axis, Seq 5) exactly as every such observable is — which is why the
discovery test is single-goroutine, to isolate the GC trigger from that axis. (`finqueued` is
process-cumulative, so the test hook
subtracts a bubble-entry baseline — without that subtraction the entry GC's varying pre-bubble finalizer
count masquerades as nondeterminism; a probe-level trap worth recording.)

**The layered determinism contract (the `-race` boundary).** The determinism guarantee is **layered**,
and the layering is the trust contract (mirrored in the `testing/simulation` package doc). Originally the
GC per-cycle layer fired on physical `heapLive` and so was *not* `-race`-robust; Phase 2a moved it to
per-object `dstHeapAlloc` and pulled it into the contract (last row, and the "How per-cycle discovery is
made deterministic" subsection below the table):

| layer | guarantee | basis | under `-race` |
|---|---|---|---|
| **Logical** | scheduling, select, map, `math/rand`, values, **replay** | per-g RNG + single-P | **holds** (verified: 8/8 DST logical tests pass under `-race`, incl. GOMAXPROCS=4 churn; no race reports) |
| **Finalizer set @ quiescence** | the finalizer/cleanup *set* run by a quiescence point = objects logically unreachable there | reachability (logical) | **holds** (lands with Chunk B's drain) |
| **GC set-level** (`numGC`, total finalizer/weak set) | the GC count and the *set* of objects discovered | heap bytes, but target floors at `heapMinimum` | **holds** (the 2 GC tests pass under `-race`) |
| **GC per-cycle** — *which cycle* discovers an object | **per-object allocated bytes** (`dstHeapAlloc`) | **holds** (Phase 2a; `TestDSTGCPerCycleDiscoveryDeterministic`) |

All four layers are unconditional. Every DST heap-trigger crossing fires on `dstHeapAlloc` (per-object
allocated bytes): the floored case (`target == heapMinimum`), the GOGC-scaled case
(`target == (heapMarked − base)·GOGC/100`), and the `Options.MemoryLimit` case (the bubble's net heap
`bubbleMarked + dstHeapAlloc` vs the limit). The set-level test (`numGC` + total finalizers,
`TestDSTGCFinalizerDiscoveryDeterministic`) and the per-cycle test (mid-run partial discovery for the
floored, GOGC-scaled, and `MemoryLimit` regimes, `TestDSTGCPerCycleDiscoveryDeterministic`) both run in
all builds.

**How per-cycle discovery is made deterministic under `-race` (Phase 2a).** The earlier framing scoped
per-cycle determinism out of the contract because the trigger fired on **physical `heapLive`**, which
advances **span-granularly** — `gcController.update` accounts a whole span (`npages*pageSize`) when it is
grabbed (`mcache.go`), so `heapLive − heapMarked` jumps in span chunks and the allocation at which it
crosses the GC trigger depends on the bubble's **entry span-fill phase**, which varies run to run (worse
under `-race`/composition, which shift it). That moved *which cycle* discovered a given object. A
trace-hash localization (instrumenting the raw per-cycle trigger inputs) pinned it
precisely: the **logical crossing point is deterministic, only the span-granular `heapLive` accounting is
not**, and `heapMarked` is deterministic given a deterministic crossing (so no "logical live set" is
needed — that was an over-estimate of the fix).

The fix is to drive the trigger off **`dstHeapAlloc`** — bytes summed **per-object at allocation**
(`mallocgc`), using each object's size-class size (`elemsize`) — instead of `heapLive`, and to check the
trigger on **every** allocation (the `mallocgc` dispatcher, gated `dstActive()`), not only at span grabs.
`elemsize` is a deterministic function of the requested size (size classes are fixed), is
`-race`-invariant (the race detector uses shadow memory, not object redzones — object *sizes* are
identical under `-race`), and is in `heapMarked`'s units (the GC counts the same slot size), so the
GOGC-scaled comparison `dstHeapAlloc ≥ (heapMarked − base)·GOGC/100` is **exact**, not merely
proportional. The cycle boundary then lands at a deterministic allocation, so per-cycle finalizer/weak
discovery is a deterministic function of the seed in normal **and** `-race` builds, and across binary
compositions — for both the floored and the GOGC-scaled regime (measured: 300/300 identical, normal and
`-race`, both regimes).

To give the per-object accumulation a single choke point, `-tags dst` routes every heap allocation
through the `mallocgc` dispatcher (the compiler-emitted per-size-class fast paths are gated off,
`sizeSpecializedMallocEnabled && !dstBuild`; they are off by default and already off under `-race`).
Production behavior is unchanged — every piece is under `dstActive()`/`dstBuild`, dead-code-eliminated or
inactive when off.

**Residual (sub-observable, accepted).** The GOGC-scaled target's basis `heapMarked − base` carries a
rare sub-object wobble from the process baseline captured in `dstHeapBase` at entry (a pre-bubble
transient that survives the entry GC); it does not flip discovery in practice and is the same class as
the `HeapAlloc`/`HeapInuse` byte noise the contract already steers away from (DST-MEM-1). Independently,
the raw `finqueued`-based observable also counts **pre-bubble stdlib finalizers** that survive the entry
GC and die in-bubble, whose count differs between *builds* (process startup) though it is constant within
one binary; a SUT observes its *own* finalizers, which are build-invariant, so this does not affect the
contract — the per-cycle test asserts within-build replay (the `-race` contract) on the mid-run partial.

This closes the **convergence point for "full determinism under `-race`"**: the Phase-1 map confirmed the
byte-based GC trigger was the sole remaining within-build `-race`/composition nondeterminism in the
contract layer (after the system-goroutine-isolation fix and the build-invariant hash key); driving it
off per-object `dstHeapAlloc` removes it. The original investigation plan is retained below.

**Faithful-collapse note (nit).** The DST trigger goal is `heapMarked + (heapMarked − base)·GOGC/100`,
vs production's `heapMarked + (heapMarked + lastStackScan + globalsScan)·GOGC/100`. DST both subtracts
`base` and drops the stack/globals scan runway — both make DST trigger GC *more* often (smaller goal).
This is finer in *frequency* but not in *capability*: an earlier GC is always a state the real runtime
can reach (lower GOGC / memory pressure forces exactly that), and it never skips a GC production would
force. Faithful collapse; no unreachable execution.

----
*Original investigation plan (retained for provenance):*

The design above scopes finalizer discovery to **set-at-quiescence** because the heap-trigger crossing
point wobbles run-to-run (D2: `heapLive` crosses `trigger` at a different allocation each run, so
`heapMarked` and the mid-burst discovery *cycle* vary). That scoping is a *consequence* of accepting the
heap-layout noise as irreducible. **The next investigation tests whether it is actually reducible** — if
the per-bubble heap trajectory can be made byte-exact, the contract tightens to **per-cycle / byte-exact
discovery determinism** and the drain need not be quiescence-only.

Investigation plan (do this *before* committing to Chunk B's quiescence-only drain — the outcome
decides the drain's shape):

1. **Find the root of the crossing wobble.** `heapLive` is a byte counter advanced in span-granular
   jumps, not per object — and it is *not* an address, so ASLR/span-address variation should not by
   itself move it. Instrument (throwaway) at bubble entry and at each `gcStart` under `dstActive`:
   record `heapLive` at bubble entry (`dstActivate`/`synctestRun`), the per-size-class mcache fill
   state, and `heapLive` at the first in-bubble GC. Hypotheses to discriminate: (a) **baseline** —
   `heapLive` at bubble entry varies (process heap history before the bubble); (b) **span-fill state** —
   partially-filled spans inherited from pre-bubble allocation make the first bubble allocations grow
   `heapLive` by a varying amount before a fresh span is grabbed; (c) something else (stack scan bytes,
   runtime-internal allocs during the bubble).
2. **Prototype the fix matching the root.** Likely candidates: **(i)** measure the trigger against a
   **per-bubble baseline** — snapshot `heapLive` at bubble entry as `dstHeapBase` and fire when
   `heapLive - dstHeapBase ≥ target`, so only the bubble's *own* (deterministic) allocations decide the
   crossing; **(ii)** **flush all mcaches at bubble entry** (`prepareForSweep`/a forced
   `stopTheWorld`+flush) so allocations start from a deterministic span-fill state; **(iii)** a forced
   GC at bubble entry to pin `heapMarked`. Combine as the root demands. A full per-bubble arena/allocator
   reset is the heavy end — only if (i)/(ii) are insufficient.
3. **Measure.** Re-run the trigger-value instrumentation (the `dstMixTrigger` overlay from Chunk A) and
   a finalizer-discovery-count probe across many runs/seeds. Success = trigger value AND per-cycle
   finalizer count byte-identical across runs.
4. **Decide the contract.** If byte-exact is achieved cheaply and soundly (no production-faithfulness
   violation — the heap trajectory must stay GOGC-plausible), tighten DST-GC-1 and the D4 discovery
   invariant to per-cycle determinism, and Chunk B's drain may run per-GC (simpler, more faithful to
   production timing). If not achievable cost-proportionately, the set-at-quiescence design stands and
   the drain is quiescence-only. *(Outcome: per-cycle discovery adopted; the quiescence drain was
   retained — see "Decision for the build" and "As built (Chunk B)" above.)* **This is a Spec-first / collapse-check decision: the chosen trigger
   must remain a faithful GOGC collapse, neither finer nor coarser.**
