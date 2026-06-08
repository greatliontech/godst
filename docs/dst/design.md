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

What DST does **not** virtualize today: real network/file I/O and cgo. A program under test confines
itself to goroutines, channels, sync primitives, and time, and models real I/O in-memory. Bringing I/O
into the fork (an in-memory deterministic network, filesystem, and file/pipe I/O) is the main set of
pending features — see the Roadmap.

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
the user surface is a thin, dependency-light wrapper (`runtime` + `internal/synctest` only), while the
determinism *mechanism* lives in `runtime` and is reached via `//go:linkname`. This mirrors how
`testing/synctest` is the public face of an `internal/synctest` mechanism — the public name is a
testing construct, not a `runtime` sub-package.

- **`simulation.Run(seed uint64, f func())`** is the entry point. It **enforces the determinism
  preconditions itself** — they are not user knobs that can be forgotten: it sets `GOMAXPROCS(1)`,
  disables async preemption, activates DST + seeds, runs `f` in a `synctest` bubble (re-rooted from
  the seed), and restores everything on return (including on panic). `Run` is bubble-scoped: each
  call is an independent, order-immune deterministic universe (the per-g tree re-roots per bubble in
  `synctestRun` via `dstBubbleRoot`), so a failing test reproduces identically in isolation.
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
Both `Run` and `RunWith` fix the identity, so even plain `Run` is reproducible here. This
and the crypto/rand seam below are the only places the fork patches packages other than
`runtime`/`testing/simulation`, and they are unavoidable: the SUT calls `os.*`/`crypto/rand` directly.
The white-box `dstActivate` path leaves identity unset (real values), as it is not a user surface.
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
keeps a process-global SP 800-90A DRBG whose counter the seam does not control, and BoringCrypto uses
its own generator; neither is a simulation configuration (both need cgo/special builds DST does not
use).

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

Under a run, `net.Dial`/`net.Listen` stop touching the OS and run on an in-process **address registry**:
`Listen` registers a simulated listener; `Dial` looks it up and hands the dialer end of a new connection
back while pushing the server end onto the listener's accept queue. A connection is a `net.Pipe`
endpoint (channel I/O, already synctest-durable; deadlines on the fake clock) **wrapped** with the
simulated local/remote `*net.TCPAddr`. The seam is the exported `Dial`/`DialContext`/`ListenConfig.Listen`
(the `os.Getpid` altitude), gated on `dstActive()` so it compiles out without `-tags dst`; net's internal
lookups stay real (the program does not exercise real sockets under DST).

**Determinism is free.** Connection/accept/delivery order is just the goroutine schedule, which is
already deterministic — no new seed, no new RNG. The registry is keyed by a per-run epoch (`dstNetEpoch`,
bumped in `dstActivate`) so it resets between runs with no teardown hook. Enforced by `TestDSTNet`
(a two-node exchange replays byte-identically across processes; the per-run reset lets a second run
re-Listen the same address). This is the reliable, in-order **base** on which network faults
(partition/drop/reorder/latency) layer later as policies on the same registry+conns.

**Caveat (fidelity).** `Dial` returns the `net.Conn` *interface*; code that type-asserts the concrete
`*net.TCPConn` (raw fds, `SetNoDelay`, `syscall.Conn`) will not get one. DNS resolution, UDP
(`PacketConn`), Unix sockets, and `net.Interfaces` (a fixed synthetic set consistent with this
addressing) are follow-on increments. FIPS/Boring-style configs are out of scope as elsewhere.

### Map hash key requires `-tags dst` (a startup constraint the API cannot cover)

Map *iteration order* depends on the process-global hash key (`aeshash`/`memhash`), set at **startup**
in `alginit` from OS entropy for hash-flooding protection. It cannot be re-seeded at runtime without
corrupting maps created before activation (including runtime/stdlib-internal ones the bubble then
touches). So a deterministic map order needs a **build-time** signal: **`-tags dst`** makes `randinit`
seed the global generator from a fixed constant (`dstFixedSeed`), fixing the hash key. Map order is
still *seed-varied* via the per-g `m.seed`; only this one global key is fixed. `simulation.Run` **panics if
the binary was not built with `-tags dst`**, so the constraint can't be silently violated. A
`-tags dst` binary has a fixed hash key for all maps (hash-flooding exposure) — acceptable for a test
build, and absent from normal builds.

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
memory races. Those remain the job of `-race`. The simulation and `-race` are complementary.
Completeness can be *increased* additively by injecting cooperative preemption points at function
entries (PCT-style), still deterministically — see Seq 5.

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
  (`dstSchedRand`) advances only for selections among *bubble* goroutines; runtime-infrastructure
  goroutines (`g.bubble == nil`) are scheduled by a fixed RNG-free policy (`dstFindRunnable` prefers
  them in candidate order). Without this, how often infrastructure goroutines need scheduling — which is
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
  `*net.TCPConn` (raw fds, `SetNoDelay`) does not get one. DNS/UDP/Unix/`net.Interfaces` are follow-ons.

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
once there is something to fault.

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
  "Deterministic GC for DST" below. Current state (Tier 0): `simulation.Run` disables GC during a run and
  reaps `sync.Pool`s on return (a stopgap). The full scope and tiers are written up so the depth of
  fix is a deliberate choice, not a default.
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

- **Tier 0 — current.** GC off during a run + `sync.Pool` reap on `simulation.Run` return. *Unsound*: no
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
- **Tier 2 — general deterministic GC.** Deterministic **heap-ratio-triggered STW GC** that can fire
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
> (`runCleanups`).

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
  (production's `fing` blocks too, just stalling all later finalizers). A finalizer that **spawns** a
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
reachable only through object A's still-pending finalizer) would otherwise be discovered only by the
post-`Run` reap — run on the async `fing`/cleanup goroutine (`g.bubble == nil`) — and the tail's
channel op would fatal. So `dstStopGCDrain` loops GC+drain until a GC discovers nothing new
(`dstDrainAtQuiescence` returns whether it made progress), resolving the whole chain **in-bubble**
before teardown. This is bounded (`dstRunEndDrainRounds`) so a self-re-registering callback
(`SetFinalizer`/`AddCleanup` of the object from its own callback) cannot spin forever — at the cap the
residual falls through to the reap as before, the pathological case, not a chain. It is sound because
the SUT has exited (everything is dead, so running the full chain is correct) and changes no in-run
quiescence behavior; the cleanup drain (Chunk C) is covered identically (the loop checks both
`finPending` and `cleanupPending`). Regression: `DSTFinChain` (a 3-level chain with a channel-touching
tail) fatals on the post-`Run` reap without the fixpoint, prints `ok` with it.

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
- **`Options.MemoryLimit`** — a per-run knob that bounds the bubble's *own* heap growth
  (`heapLive - dstHeapBase`), which is bubble-local and deterministic, so `NumGC` under the limit is
  reproducible (`TestDSTMemoryLimit`; the trigger in `mgc.go` `gcTrigger.test`). Redefined semantics
  under DST: *bound bubble heap growth*, not *bound total RSS*. It is an upper bound on top of the GOGC
  trigger; when `GOGC=off` it is the sole bound (the `defaultHeapMinimum` floor is skipped so a limit
  set above it is honored).
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
So Tier 2 **retains** an end-of-`Run` reap (the current `runtime.GC()×2`, sized for the 2-generation
cache) — or, equivalently, one forced GC at the `Run` boundary — as a *pool-lifetime* mechanism
distinct from in-run memory bounding. Only a change to pool *lifetime* across bubbles (out of scope
here) would remove it. **[R]**

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
   - *`fing` gated at the wake site, not in `queuefinalizer`.* `queuefinalizer` still sets `fingWake`
     (harmless); the scheduler's wake of `fing` (`proc.go` `findRunnable`) is gated under `!dstActive()`,
     so during a Run `finq` accumulates and only the drain drains it. Smaller change, and the gate is
     byte-identical for non-DST.
   - *Drain exit handshake.* The drain is a bubble goroutine and counts toward `bubble.total`, so it must
     exit before the `total != 1` deadlock check; `dstStopGCDrain` runs a final drain, sets `gcDrainExit`,
     and waits for the drain to die (invariant DST-FIN-3).
4. **Bubble-scoped drain — cleanups** (D4 for `mcleanup`; **Chunk C — landed**). The *same* drain
   goroutine (`synctestGCDrain`), same quiescence wake, drains `gcCleanups` after `finq` via a factored
   `runCleanupBlock`/`dstDrainCleanups`; `cleanupPending` joins `finPending` in the wake decision; the
   quiescence GC's sweep already flushes per-P cleanup blocks (`mgcsweep.go`). Depends on 3. Foreclosure
   check: none. **As built — the async pools are fully dormant during a Run (four gates under
   `!dstActive()`):** the finalizer goroutine `fing` and the cleanup-pool goroutines must neither *run*
   nor be *created* during a Run, because either would run a callback with `g.bubble == nil` (fatal on a
   bubble channel op) or, on creation via `go`, draw from the creating goroutine's DST RNG stream and
   persist across Runs (breaking reproducible-in-isolation). So: gate the `fing` wake (`proc.go`,
   tested by the finalizer channel-op test) and `createfing` (`mfinal.go`); gate the cleanup wake
   (`proc.go`, tested by the *prior-goroutine* cleanup channel-op test) and `createGs` (`mcleanup.go`,
   tested by the cleanup RNG-isolation test). The wake gate matters when an async goroutine pre-exists the
   Run; the create gate when the first callback of its kind is inside the Run. Pre-bubble and prior-Run
   finalizers/cleanups are drained bubble-less in `dstActivate`, so the async pools are never needed
   during or around a Run. (`createfing` is the one gate not independently testable in the harness — fing
   pre-exists from a stdlib import; same mechanism as the tested `createGs`.)
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
separate axes.** A.5's relative trigger (adopted) makes *discovery* per-cycle deterministic in a fixed
normal build: which GC cycle queues a given object is a function of the seed. (That per-cycle
byte-exactness is **not in the contract** and is not tested — it is sub-observable byte-trigger noise,
perturbed by -race or binary composition; the hardening that downgraded the discovery test to set-level
removed the `dstFinqSeq` per-cycle probe. See the layered-contract section below.) That is independent
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
heap trigger, so finalizer/weak **discovery is per-cycle deterministic** and **Chunk B's drain runs
per-GC** (simpler, more production-faithful than quiescence-only). DST-GC-1 and D4 tighten from
set-at-quiescence to **per-cycle discovery determinism**.

**A.5 — IMPLEMENTED (GOGC-scaled, full-faithful).** Landed as the GOGC-scaled-with-entry-GC version
(the production-faithful option, user-chosen):
- `dstActivate` forces a full GC at bubble entry (STW under DST) and snapshots `dstHeapBase =
  gcController.heapMarked` — the process *live* set, with pre-bubble garbage collected so the baseline
  is not polluted by entry garbage the first in-bubble GC would otherwise free (which would drive
  `heapMarked` below `base`). (`runtime/dst.go`.)
- `gcTrigger.test` (`gcTriggerHeap`, `mgc.go`) under `dstActive` fires when
  `heapLive ≥ heapMarked + max((heapMarked − dstHeapBase)·GOGC/100, heapMinimum)` — the production GOGC
  rule with the scaling term on the bubble's *own* live (`heapMarked − dstHeapBase`), excluding the
  run-to-run-varying process baseline. No per-cycle rebase is needed: `base` is fixed at entry and
  `heapMarked` updates each cycle, so the target tracks the bubble's live set faithfully.
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

**The layered determinism contract (the `-race` boundary) — what makes A.5 *trustworthy*, not just
clever.** A.5's per-cycle discovery determinism lives in the **physical** layer (it fires on heap
*bytes*: `heapLive` vs `heapMarked − base`). Any tool that rewrites the heap byte-for-byte — `-race`,
`-msan`, a different build — perturbs it, because it perturbs the bubble's own per-allocation sizes
(not just the baseline, which A.5 *does* subtract out). This is **inherent**, not a defect: you cannot
keep byte-exact heap accounting while an instrumenter rewrites the heap. So the determinism guarantee is
**layered**, and the layering is the trust contract (mirrored in the `testing/simulation` package doc):

| layer | guarantee | basis | under `-race` |
|---|---|---|---|
| **Logical** | scheduling, select, map, `math/rand`, values, **replay** | per-g RNG + single-P | **holds** (verified: 8/8 DST logical tests pass under `-race`, incl. GOMAXPROCS=4 churn; no race reports) |
| **Finalizer set @ quiescence** | the finalizer/cleanup *set* run by a quiescence point = objects logically unreachable there | reachability (logical) | **holds** (lands with Chunk B's drain) |
| **GC set-level** (`numGC`, total finalizer/weak set) | the GC count and the *set* of objects discovered | heap bytes, but target floors at `heapMinimum` | **holds** (the 2 GC tests pass under `-race`) |
| **GC per-cycle byte-exact** — *which cycle* discovers an object | heap bytes (physical) | **not in contract** (see below) |

The contract is sound because the **unconditional** layers (logical + set-at-quiescence + GC set-level)
are what a SUT normally relies on. The GC-determinism test asserts the **set-level** layer (`numGC` +
total finalizers) and runs in all builds (`TestDSTGCFinalizerDiscoveryDeterministic`); it does **not**
assert the per-cycle split. Fail-loud, not silent.

**Why per-cycle byte-exact is not in the contract.** A.5's per-bubble relative trigger *does* make
finalizer discovery byte-exact per-cycle in a fixed normal build (a real bonus — discovery is per-cycle,
not merely set-at-quiescence), but that byte-exactness is **fragile and sub-observable**: the trigger
fires on heap *bytes*, checked at span-grab boundaries (`mallocgc`'s `checkGCTrigger`), so anything that
shifts the byte↔object mapping by a span moves *which cycle* discovers an object. `-race`/`-msan`
redzones do it (the split goes **bimodal** — two values run-to-run; `numGC` and the total set stay
stable); so does a change in **binary composition** (e.g. linking a heavy import shifts the bubble's
entry span-fill phase). Because no SUT can rely on it portably, the contract is set-level only and the
per-cycle hash is not tested. An earlier build asserted it byte-exact in normal builds (gated on
`internal/race.Enabled`) and tracked the `-race` jitter as an open issue; both were removed when the test
went set-level. Un-claiming it (rather than chasing a race-invariant *logical-allocation* trigger — a GC
redesign, since the runtime tracks only the physical live set) is the right call for the narrow benefit.

**An experiment proved the byte-layer fragility is reducible (and then proved the fix unnecessary).**
Instrumenting `mallocgc` to count bubble allocations showed the bubble's logical allocation **count and
requested bytes are `-race`-invariant** (`allocs = 40000`, `bytes = 10240000`, bit-identical normal vs
`-race`). A requested-bytes trigger under `-race` was built on that — and then **removed as
over-engineering**: a mutation test showed the existing byte/GOGC trigger *already* passes the `-race`
tests (set-level), because the target floors at `heapMinimum` (above). Making *per-cycle byte-exact*
hold under `-race` would require checking the trigger on **every** allocation rather than at span grabs —
the `checkGCTrigger` sites across `malloc.go`, `malloc_stubs.go`, and the *generated* per-size-class
paths plus their generator — genuinely invasive, and not needed (a program must not rely on finalizer
timing; the set-level layer is what the trust contract rests on). So per-cycle byte-exact stays a
normal-build refinement.

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
   the drain is quiescence-only. **This is a Spec-first / collapse-check decision: the chosen trigger
   must remain a faithful GOGC collapse, neither finer nor coarser.**
