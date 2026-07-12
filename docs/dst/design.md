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

What DST does **not** virtualize: unsupported network kinds, cgo, raw syscalls, processes, and
signals — each **fenced** (a loud, deterministic refusal for bubble goroutines; see "The
interception boundary") rather than silently reaching the host — and, deliberately, the standard
streams explicitly granted through an inherited-file capability; see "Deterministic pipes and the
stdio stance"). TCP `net.Dial`/`net.Listen` are modeled by the in-memory deterministic
network below, the filesystem by the in-memory deterministic filesystem, and `os.Pipe` by the
in-memory deterministic pipe (all per-bubble, all reset by the run epoch); what remains is modeled
in-memory by the program under test or avoided. The remaining pending fault axes are OOM kill and
scheduling (straggler) — see the Roadmap.

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
itab and symtab cache eviction, `sema` treap ticket (the ticket only balances the treap; wake order
among same-address waiters is upstream queue policy — FIFO for first-time waiters, requeue-to-front
for re-waiters (`queueLifo`) — and is unaffected by the per-m draw), and `lock_spinbit` mutex
anti-starvation (runtime `lock2`, not `sync.Mutex`). Exempted
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
  reproduce. This is the only non-`Run` entry and is not a user surface. Because this path runs at
  `GOMAXPROCS>1`, every DST runtime structure reachable while it is active must be safe under real
  parallelism — "in-bubble single-P cooperative access" is a `Run`-only precondition no shared DST
  list may assume (concretely: the armed-fake-timer registration list, `dstFakeTimers`, serializes
  epoch rollover and intrusive-list publication in one short critical section, not an append-racy slice).

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
real concurrency for the schedule to explore). A custom positive `PID` must fit in the OS pid field
(`int32`); non-positive values select the default, oversized values panic rather than wrapping, and a run
that exhausts the finite pid field while allocating `Process` pids panics instead of reusing or wrapping
pids. The rest are fixed deterministic constants documented on `Options`: `ppid=1`,
`uid=gid=euid=egid=7777` (a distinctive value, not the ubiquitous 1000, so the simulated identity is
observably an override), current user `sim` (uid/gid `7777`, home `/home/sim`).
`Run`, `RunWith`, `Test`, and `TestWith` fix the identity, so even plain `Run` or `Test` is reproducible here. This
and the crypto/rand seam below are the only places the fork patches packages other than
`runtime`/`testing/simulation`, and they are unavoidable: the SUT calls `os.*`/`crypto/rand` directly.
The white-box `dstActivate` path leaves identity unset (real values), as it is not a user surface.

`syscall.Kill(pid, 0)` is the liveness probe over that simulated identity. During a run it consults only
the simulated pid registry: the root pid is live for the whole run, each `simulation.Process` pid is live
for that process body's dynamic extent, and completed or unknown pids return `ESRCH`. `Kill(0, 0)` and
`Kill(-1, 0)` succeed (the caller's own group and self always exist on Linux); other negative pids name
process groups the simulation does not model — unknown, so `ESRCH`. It never probes a host
process. The liveness READ gates process-globally like the other identity reads; non-zero signals remain
fenced until a signal-delivery model is settled — and, being a fence, only for bubble goroutines (a
non-bubble harness goroutine's `Kill` reaches the host kernel mid-run, per the interception boundary).
Generic raw `SYS_KILL` remains fenced like other unsupported raw syscalls.

The simulated filesystem also owns the procfs identity surface needed for pid-liveness recovery:
`/proc/<pid>/stat` and `/proc/self/stat` are generated for live simulated pids and include a deterministic
field-22 starttime derived from that pid identity; completed, unknown, host, or unrepresentable pids are
not visible, and a zero-padded pid is not a procfs name (Linux's `name_to_int` rejects leading zeros).
`/proc/self/stat` and `/proc/<own-pid>/stat` are one file to `SameFile` (one inode on the host); a
trailing slash on a proc leaf is `ENOTDIR` (the filesystem section's trailing-slash clause).
`/proc/self/ns/pid` readlink returns the stable deterministic namespace identity `pid:[1]`.
Unsupported `/proc` paths stay deterministic simulated results (unsupported or not-exist), never host
passthrough.

The group and user-database surface is simulated to match: `os.Getgroups` is exactly `[7777]`, and
the `os/user` lookup functions resolve against a minimal database containing exactly the simulated
user and its group — `Lookup("sim")`/`LookupId("7777")`/`LookupGroup("sim")`/`LookupGroupId("7777")`
return the simulated records, `User.GroupIds` of the simulated user is `["7777"]` (any other `*User`
resolves to just its primary gid, as the osusergo path does for a user with no group-file
memberships), and anything else is the deterministic production unknown-error identity rather than a
host-database read
(`TestDSTIdentityGroups`).

**Environment surface (enforced).** The
process environment is per-**process** state on a real machine, so under a run it is per-simulated-
process state here: `os.Getenv`/`LookupEnv`/`Environ`/`Setenv`/`Unsetenv`/`Clearenv` operate on a
per-process copy-on-write view, initialized from the host environment at `Run` entry. Writes are
isolated — a `Setenv` in one process is never observable from another process or host (env must not
be a back-channel two real machines never had; this is the environment leg of DST-NODE-ISOLATION).
A process invocation's view is discarded on normal exit, explicit process crash, or host crash; a same-name restart copies the
run-entry host baseline again, never its predecessor's `Setenv`, `Unsetenv`, or `Clearenv` mutations.
Reads of *unmodified* variables return host values: those are machine state, exactly like data read
through an explicitly granted host-file capability — deterministic per machine, and cross-machine
reproducibility of a SUT that branches on them is program discipline. Note
`os.UserHomeDir` (host `$HOME`) and `user.Current().HomeDir` (`/home/sim`) can therefore disagree
in-run; acquire identity through `os/user` for coherence. `os.TempDir` stays fixed at the simulated
`/tmp` (see the filesystem section). The simulated
identity is also process-global while set: a goroutine outside the simulation that reads identity
during a run (or in the brief set/clear windows around it) observes simulated values — identity
gates on the sim-env flag, not per-goroutine. The per-process environment view shares this
process-global gate but adds a *write* path the read-only identity surface lacks: a `Setenv` by a
goroutine *outside* the simulation during a run mutates the root process (proc 0) view rather than
the host, so run determinism additionally requires that no foreign goroutine writes the environment
mid-run. This holds by construction — a SUT's own goroutines run inside the bubble (deterministically
ordered), and the harness does not `Setenv` mid-run — and is the write-side analogue of the read-only
"harmless" argument below.
Environment dispatch is atomic with run activation and deactivation: each API operation holds one
runtime gate from host-versus-simulated selection through its read or mutation, while publishing or
clearing the simulated environment takes the same gate. An operation therefore belongs wholly to the
host or to one run epoch; it cannot choose one world and complete in the other.
This is a deliberate gating asymmetry: identity is gated on `dstSimEnvSet` (set only by
`testing/simulation.run`), whereas the RNG/scheduling/crypto-rand seams are gated on `dstActive()` (set
by `dstActivate` too). So a white-box run sees seeded RNG, scheduling, and crypto/rand but the *real*
host identity — harmless, because the white-box runtime tests exercise the per-g mechanism under
`GOMAXPROCS>1` and never read identity. `uid`/`gid`/`ppid` are fixed (not configurable like
hostname/pid/NumCPU) by deliberate choice — no SUT has needed per-run variation, and the surface stays
lean; they are single constants in `runtime/dst.go` if that changes.

The interface identity surface is virtualized: `net.Interfaces`/`net.InterfaceAddrs` under a run
return a fixed synthetic set consistent with the in-memory network's addressing — a loopback `lo`
and an `eth0` bearing the host's routable IP (`10.0.0.<host>`) and a deterministic per-host MAC —
never the real machine's NICs (`net/dst.go` `dstInterfaces`; enforced by `TestDSTNetInterfaces`
under DST-IDENTITY-SOUND, faults.md).

### Deterministic crypto/rand (the entropy seam)

`crypto/rand` (UUIDs, TLS nonces, tokens, key material) reads OS entropy, a determinism hole one might
expect to handle *app-side* (each program injecting its own crypto seam). That assumes `crypto/rand` is
not runtime-seedable.
It is: in the standard configuration every `crypto/rand` read funnels through the single chokepoint
`crypto/internal/sysrand.Read` (the non-FIPS `drbg.Read` is just `sysrand.Read(b)`), so one hook there
makes *all* of `crypto/rand` a reproducible function of the seed for free — exactly as the runtime RNG
seed already covers `math/rand[/v2]`. `crypto/internal/sysrand/dst.go` bridges to the runtime's
active-and-seeded per-g stream. Admitted run goroutines receive deterministic bytes; every other
caller receives OS entropy, including during an active run and on legacy `/dev/urandom` fallback
platforms. After one-time entropy-source initialization, both paths preserve the allocation-free
`crypto/rand.Read` behavior. Production crypto/rand and process-startup entropy are untouched:
`dstActive()` is false outside a run, and `dstSeed` is only set by `simulation.Run`, which requires
`-tags dst`. This holds under `-race`
(the per-g RNG drives it). Boundary: only the **standard** configuration is deterministic — FIPS mode
keeps a process-global SP 800-90A DRBG whose counter the seam does not control (it consumes the
seam's deterministic bytes only as additional input), and BoringCrypto uses its own generator.
BoringCrypto needs a special build DST does not use, but FIPS mode is one `GODEBUG=fips140=on` away
in any build — so it is **enforced, not just documented**: `enterSimulation` panics when FIPS mode is
latched, rather than letting `crypto/rand` go silently nondeterministic inside a run
(`TestRunRejectsFIPSMode`).

**Invariants enforced by the identity/crypto seams:**

- **INV-CRYPTO** (security-critical): `crypto/rand` returns OS cryptographic entropy in every reachable
  state *except* on a **seeded goroutine inside an active run** — a goroutine of the simulation's own
  tree, whose per-g stream was rooted from the seed — where it returns the deterministic per-seed
  stream. It is never predictable outside a run, never in a non-`-tags dst` build, **and never merely
  because a run happens to be active elsewhere in the process**: a goroutine whose per-g stream was
  not seeded by the active run (created before activation, outside the bubble, or a survivor of a
  PRIOR run — the caller's root is cleared at deactivation) reads real OS entropy even while a
  run is live — the alternative, filling from an unseeded zero-rooted stream, would hand every such
  goroutine the same fixed, seed-independent bytes (the exact "predictable outside a run" violation
  this invariant forbids), and a stale prior-run root would hand its goroutine bytes derived from
  the previous run's seed. Enforced by `TestDSTCryptoRandDeterministic` (deterministic + seed-varying
  inside a run, exact full-word/tail encoding, empty-read stream neutrality, and two reads *outside*
  a run differing) and structurally by the `dstActive()` +
  seeded-per-g gate (`dstSeed` is never set on any production path: its only setters are
  `simulation.Run`, which panics without `-tags dst`, and the unexported `dstActivate` linkname used
  solely by the runtime's own white-box tests). The unseeded-goroutine leg is enforced by
  the active-and-seeded gate together with the **stability of the `gp.dstrand == 0` sentinel**:
  no draw site (`runtime.rand`, select poll order, the fake-timer tie-break) advances an unseeded
  goroutine's stream, and `newproc1` extends the seeded tree only through seeded parents — so a
  pre-run goroutine that spawns, draws math/rand, selects, or adds a fake timer during a run stays
  outside the deterministic stream, and so do its descendants; the one seeded goroutine that
  survives a run and can still execute (its caller — bubble goroutines exit with the run, and the
  goroutines a recovered in-run deadlock abandons stay permanently parked, never reaching a draw
  site) has its root cleared at deactivation, so `g.dstrand != 0` holds only within the run that
  seeded it. Pinned by
  `TestDSTCryptoUnseededGoroutine` (a goroutine created before activation reads real,
  cross-process-varying entropy while a run is live), `TestDSTCryptoUnseededVectors` (each seeding
  vector — spawn, math/rand draw, select, fake-timer add — plus the spawned child, all still real
  entropy), `TestDSTCryptoPriorRunCaller` (a completed run's caller reads real entropy during a
  later run started by another goroutine), and `TestDSTCryptoSeededChildAfterDeactivate` (a
  white-box seeded child surviving deactivation returns to OS entropy despite its nonzero stream root).
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
connection back while pushing the server end onto the listener's accept queue. **Every** connection —
cross-host, same-host, and loopback alike — is **wire-backed** (`net/dst_wire.go`: a buffered,
synctest-durable transport; deadlines on the fake clock) wrapped with the simulated local/remote
`*net.TCPAddr`; a same-host wire simply has zero link latency. One transport shape
everywhere is load-bearing for soundness: a rendezvous pipe (writes blocking until the peer reads) would deadlock
two co-located processes that each write before reading — an execution real TCP, whose send buffer
is never zero, cannot produce (the Soundness invariant's false-positive class).

**Transport model (contract).** The byte-stream, no-lost-wakeup, flow-control,
retransmission-horizon, and connect-cost legs are landed; the FIN/RST leg and the
no-declared-host connect timeout land with a follow-on (both marked below). The wire
models a TCP socket pair, not a message queue:

Network delay configuration is validated before a run publishes simulation state. Nonpositive
latency, jitter, and bandwidth components are disabled. A latency plus the largest possible jitter
draw that cannot fit in `time.Duration` is rejected. Runtime delay arithmetic that also depends on
the current base clock or a write's size saturates at the latest representable virtual-time deadline;
it never wraps a positive delay into an earlier delivery.

- **Byte stream, not messages.** The receive side is a byte buffer: one `Read` returns delivered
  contiguous bytes, up to the buffer's length — reads **coalesce across write
  boundaries** exactly as TCP does. Write-boundary framing is *not* a guarantee the harness gives
  (real TCP does not give it; preserving it would let a SUT that assumes 1-write-per-read pass under
  simulation and fail in production). The inverse is not promised either: a `Read` MAY return fewer
  bytes than are delivered (a future read-boundary exploration axis splits reads deliberately), so
  "all delivered bytes in one read" is not a contract a SUT may grow against.
- **No lost wakeups.** Blocked readers and writers are woken whenever their condition can progress:
  a delivery that leaves further deliverable bytes behind (or a drain that leaves buffer space) while
  another waiter is still blocked re-signals until no waiter can progress — concurrent readers on one
  conn (legal: `net.Conn` is documented concurrency-safe) never strand a goroutine blocked while its
  data sits delivered — a hang no real kernel (which wakes readers while data remains) can produce.
  *Which* eligible waiter proceeds first is the deterministic
  scheduler's choice, as everywhere.
- **Bounded send buffer / backpressure.** Each connection direction has a fixed send-buffer
  capacity (`Options.Network.SendBuffer`, default 1 MiB): a write fills it and **blocks durably**
  when full, resuming as the link drains it. Writes never succeed unboundedly into a partition.
  Same-host and loopback connections carry the SAME bound — loopback TCP has finite socket buffers
  too, so two co-located peers that each write past them before reading deadlock in production, and
  the simulation reproduces that as a loud bubble deadlock instead of masking it in an unbounded
  sim-only buffer (`TestDSTNetSameHostWriteWriteDeadlocks`, `TestDSTNetSameHostBackpressure`).
- **Retransmission horizon.** Bytes that stay undeliverable because the link is **partitioned**
  error the connection with `ETIMEDOUT` after a fixed virtual horizon (`Options.Network.RetransmitTimeout`,
  default 2 minutes of bubble time — kernel-shaped: ~15 retries), on the blocked or subsequent
  operation (`TestDSTNetWriteHorizonTimesOut`, `TestDSTNetDialPartitionHorizonTimesOut`). This holds
  for ANY undeliverable bytes, not only a blocked buffer-full write: a small write into a cut link
  returns immediately (TCP's async send — the bytes buffer), but the conn is dead at the horizon and
  the next operation fails (`TestDSTNetSmallWriteHorizonKillsConn`); a blocked read holding dying
  outbound bytes fails at the horizon instead of hanging (`TestDSTNetWriteThenReadHorizonTimesOut`,
  and for bytes already in flight when the cut lands,
  `TestDSTNetInFlightBytesCutThenReadTimesOut`). The window anchors when the undeliverable bytes are
  first observed (never earlier than the real first retransmission — errs toward later timeouts, the
  sound direction); a heal that delivers the bytes disarms it (`TestDSTNetHorizonHealDisarms`). A
  killed end still drains data the network already delivered before surfacing the error, as
  tcp_recvmsg reports pending data first (`TestDSTNetHorizonDeathDrainsDeliveredData`). A
  deadline-less write or dial into a permanent partition therefore fails in bounded virtual time, as
  it does on a real kernel — it never succeeds-and-forgets. The horizon is **partition-gated**: a
  full send buffer behind a **live** peer that has merely stopped reading is TCP *zero-window
  persist*, not retransmit exhaustion — the write blocks (backpressure) with **no** horizon and
  resumes when the peer drains (`TestDSTNetWritePersistsWithoutPartition`). Firing `ETIMEDOUT` on
  live-peer backpressure would be a sim-only failure a live peer cannot produce — the false-positive
  class Soundness forbids. The horizon has one further trigger, on the CONNECT path: a dial whose
  SYN a full accept backlog drops (`tcp_abort_on_overflow=0`) retransmits and fails `ETIMEDOUT` at
  the horizon unless a slot frees first (`TestDSTNetBacklogFullDialTimesOut`) — kernel-real exhaustion against a
  live listener, so the partition gate above scopes the ESTABLISHED-connection write path, not
  connect. A **heal resets** the horizon window (the timer restarts on the next cut),
  approximating "the retransmit timer resets on ACK progress" — exact only when the heal is long
  enough to deliver a byte; a sub-latency partition *flap* resets it with no real progress, so the
  sim keeps a conn alive a real stack's RTO would eventually kill. That errs toward *fewer* ETIMEDOUTs
  (a false negative — never a false-positive bug report), the soundness-safe direction; exact
  ACK-progress tracking is a deferred refinement. (Timer note: the horizon window is a base-time
  delta, skew-invariant, but the timer fires on the sender's host clock, so under a `DriftClock` rate
  change "2 minutes of base time" shifts slightly — faithful to a real retransmit timer, which runs
  on the sender's own clock.)
- **Connect cost.** Establishing a cross-host connection completes after one round trip of the
  link (SYN + SYN-ACK: two one-way traversals, each paying the link's base latency + a jitter draw;
  throttle exempts the zero-payload control segments) — the SYN half is paid before the server's
  Accept unblocks and is context-interruptible (a connect deadline shorter than the RTT fails
  mid-handshake); the SYN-ACK half remains context-interruptible and aborts if either endpoint is
  reset or torn down before it arrives. It then delays the dialer's return, so the server sees the conn at
  ½ RTT and the dialer returns at one full RTT (`TestDSTNetConnectPaysRTT`). Same-host connects are
  instant. Both endpoints become lifecycle-owned before the server endpoint enters the accept backlog,
  so process or host teardown cannot miss a queued or already accepted connection. A dial across a
  **partition** blackholes: it blocks until the link heals, the
  context/deadline expires, or the retransmit horizon fires `ETIMEDOUT`
  (`TestDSTNetDialPartitionHorizonTimesOut`) — so a deadline-less dial into a permanent cut fails in
  bounded virtual time rather than hanging. A dial to a **declared** host with no listener on the
  port is `ECONNREFUSED` (a live kernel answers with RST); one recorded timing simplification: this
  refusal returns immediately rather than after the SYN's ½-RTT traversal, so a connect deadline
  shorter than ½ RTT that would *time out* before the RST in production instead observes
  `ECONNREFUSED` here (a narrow adversarial-deadline case; recorded, not hidden). Refusal requires
  that live kernel: a dial to a **crashed** declared host (powered off, not yet rebooted by a Host
  re-declaration) blackholes exactly like a partition — the SYN is dropped, the dial blocks until
  the context/deadline expires, the retransmit horizon fires `ETIMEDOUT`, or the machine reboots
  and its kernel answers again (`ECONNREFUSED` until a listener is up, then connect)
  (`TestDSTCrashHostDialBlackholes`). The same holds for a dial already mid-handshake when the
  power fails — it re-enters the blackhole wait instead of surfacing the dead listener's teardown
  as a refusal (`TestDSTCrashHostMidHandshakeDialTimesOut`). The machine-off mark is distinct from a network cut: a
  `HealHost` cannot make a powered-off machine reachable, and a reboot does not heal an injected
  isolation. **Pending (lands
  with the FIN/RST follow-on):** a dial to an address **no declared host owns** should blackhole and
  fail `ETIMEDOUT` (nothing answers SYNs) — the peer-down/unreachable split; today it also returns
  `ECONNREFUSED`. This case is reachable only via a hand-constructed literal `10.x` IP (`HostIP`
  panics on an undeclared host), so it needs net to learn the declared-host set (a query hook like
  the partition hook) before it can distinguish refuse from blackhole.
- **Peer close FIN read semantics — landed; post-FIN write/reset semantics pending.** Reads after a peer's
  graceful close drain buffered data, then `io.EOF` (landed). FIN is a delayed control event: its
  arrival pays the configured one-way base latency plus one deterministic jitter draw, and a shorter
  read deadline fires first; partitions hold a not-yet-arrived FIN like data. The intended write side: the **first
  write after the peer's full close succeeds locally** (the FIN closed the peer's send direction; the
  write is accepted into the send buffer and elicits the peer's RST); the reset then errors
  **subsequent** operations with `ECONNRESET` — matching the RST round trip of a real stack, rather
  than failing the first write instantly (which no kernel does). **Today the first write after a peer
  close fails instantly with `ECONNRESET`** (the wire rejects a write whose peer end is gone); the
  succeed-then-RST round trip is the follow-on's work. One recorded simplification of the target
  shape: real stacks report the reset *once* (`SO_ERROR` consumed) and surface `EPIPE`/EOF on later
  ops; the simulation keeps the stable `ECONNRESET` identity on every subsequent op so reset-handling
  paths keying on it never miss — a SUT distinguishing `EPIPE` from `ECONNRESET` post-reset is
  outside the model (recorded, not hidden).

`DialContext` keeps the public context contract (nil panics,
canceled/deadline contexts error), `Dialer.LocalAddr` chooses the simulated local TCP address when set —
checked against live local bindings on a **2-tuple** (local addr:port) basis, as the real path refuses:
Go binds an explicit `LocalAddr` without `SO_REUSEADDR`, so `bind(2)` fails `EADDRINUSE` on a local
collision even when the destinations differ (a per-4-tuple rule here would admit sim-only successes).
A concrete source IP must belong to the dialing host (its loopback range or routable address), otherwise
the bind fails `EADDRNOTAVAIL`. A valid explicit tuple is reserved before partition, handshake, or backlog
waiting begins; concurrent dials and listeners observe that reservation, and every failed connect releases
it before returning.
An explicit port with no source IP retains bind(2)'s wildcard identity: it conflicts with every
same-family pending, live, or TIME_WAIT binding at that host and port even though a successful
connection reports the route-selected concrete source address.
Conns and listeners share ONE port space per host, in both directions: a dial's local bind — explicit
or ephemeral — conflicts with a live listener at the port (exact or wildcard, same family:
`TestDSTNetDialLocalBindListenerPortEADDRINUSE`, `TestDSTNetEphemeralDialSkipsListenerPort`), and a
new listener conflicts with a live DIALER-end conn's local 2-tuple (specific and wildcard listens,
and the `:0` allocator skips such ports: `TestDSTNetListenConnPortEADDRINUSE`). ACCEPTED server ends
inherit the listener's `SO_REUSEADDR`, so a server restarted while its old connections drain re-binds
its port (`TestDSTNetRelistenWithAcceptedConns`) — among CONN ends, only sockets lacking
`SO_REUSEADDR` (dialer ends) block a listener, exactly the kernel's rule (live listeners block each
other regardless, as two LISTEN sockets always conflict). **TIME_WAIT is modeled**: an ACTIVE
FIN-close of a conn end — dialer or accepted alike — holds its local 2-tuple for 60 seconds of
universe base time (Linux's fixed `TCP_TIMEWAIT_LEN`, the kernel's 2·MSL; base time because a
TCP timer is kernel machinery gated like wire delivery, not the host's possibly-drifted wall
clock). The hold is visible ONLY to the `bind(2)`-without-`SO_REUSEADDR` surface — an
explicit-`LocalAddr` dial of a held tuple fails `EADDRINUSE` until the hold expires
(`TestDSTNetDialerTimeWaitEADDRINUSE`, accepted-end hold
`TestDSTNetAcceptedEndTimeWaitEADDRINUSE`). The EPHEMERAL allocator deliberately ignores holds
(`TestDSTNetEphemeralChurnOutlivesTimeWait` — a dial/close churn loop crossing the whole port
span in zero simulated time keeps dialing): production's connect-time selection is 4-tuple-aware
and reuses a TIME_WAIT port toward any other destination (and toward the SAME destination on
loopback, `tcp_tw_reuse`'s default scope), so a held-port skip would manufacture sim-only
`EADDRNOTAVAIL` churn failures production's selection does not produce. The recorded collapse runs
the other way: an ephemeral dial may receive a port whose tuple production could still refuse
toward the identical destination with `tcp_tw_reuse` unavailable — a corner the SUT cannot
steer, since which ephemeral port a dial receives is sim-defined anyway. Listeners bind over
holds, `SO_REUSEADDR` over TIME_WAIT (`TestDSTNetListenerBindsOverTimeWait`), and accepts never
consult the bind probe, so a live listener keeps accepting on a port whose dead conns are still
held. No hold for the ends production sends to CLOSED directly: the RST shapes (unread-inbound
close `TestDSTNetRSTCloseSkipsTimeWait`, resets, retransmit-exhaustion deaths) and the PASSIVE
closer (`TestDSTNetPassiveCloseSkipsTimeWait`). The reset exemption carries the crash fault's
recorded RST collapse with it: a crashed PROCESS's conns reset, so their tuples hold nothing,
where a production `kill -9` has the kernel FIN clean sockets into TIME_WAIT — the same
already-recorded collapse, observed here through the bind probe. A HOST crash purges the
host's holds outright — TIME_WAIT is kernel socket-table state and dies with power, so a
rebooted host re-binds immediately (`TestDSTNetHostCrashClearsTimeWait`); a process crash
leaves the kernel and its holds alive. The peer's close INSTANT, not its FIN's delivery,
decides active vs passive — the same collapse the close-vs-arrival RST arm records. The first
closer always holds; two closes that interleave before either transport closes BOTH hold —
production's simultaneous-close shape, where each end sent its FIN before receiving the peer's
and both enter TIME_WAIT (`TestDSTNetSimultaneousCloseBothHold` pins the window's discriminant:
a peer mid-Close — committed but transport still open — does not demote our close to
passive). `:0` listeners receive deterministic nonzero ports, wrapping within
[10000, 65535] and reclaiming closed ports on the next pass — a long-lived run listens and closes
indefinitely (`TestDSTNetListenPortAllocatorWrapsAndReclaims`), and a fully live range fails
`EADDRINUSE`, bind(2)'s exhaustion identity, carrying the requested address
(`TestDSTNetListenPortExhaustionEADDRINUSE`); dialer ephemeral ports allocate deterministically
and stay within the valid port range [40000, 65535], wrapping and skipping live local bindings rather
than minting impossible port numbers, listener lookup uses canonical simulated IPs
(`localhost` maps to loopback), a plain-`"tcp"` wildcard listener is dual-stack (it reports the IPv6
wildcard address and accepts dials of both families, conflicting with either family's listeners on the
port; `"0.0.0.0"` and `tcp4`/`tcp6` stay single-family, and a single-family wildcard listen reports
the family wildcard form — `0.0.0.0:p` / `[::]:p`, dialable back to the listener — not the loopback it
maps to internally), and error identity is production-shaped
throughout `errors.Is`: refused connects are `ECONNREFUSED` and duplicate listens `EADDRINUSE`; every
operation on a locally closed connection or listener (including a second `Close`) is `net.ErrClosed`;
reads from a gracefully closed peer drain buffered data then return `io.EOF`; FIN/EOF is persistent
state, so every concurrently blocked reader wakes to consume data or observe EOF without another event — but a `Close()` of an
end whose receive queue holds UNREAD data answers with RST instead of FIN, and the peer's next read
fails `ECONNRESET` without draining (the kernel's close(2) conditional; bytes still in flight count
as queued — the recorded collapse: the sim RSTs immediately, one of the two orderings the real
close-vs-arrival race produces, `TestDSTNetCloseBeforeDeliveryStillResets`;
`TestDSTNetCloseWithUnreadDataResetsPeer`, `TestDSTNetCloseAfterDrainingFINs`) — writes after a peer's
close follow the FIN/RST shape above (first accepted, subsequent `ECONNRESET`), and any operation on
a reset connection carries `ECONNRESET`; deadline failures are `*net.OpError` wrapping
`os.ErrDeadlineExceeded` (a timeout `net.Error`) on the connection's network and addresses, driven by
the bubble's virtual clock. A FULL accept backlog (128 pending connections) drops the dial's SYN, as
`tcp_abort_on_overflow=0` does: the dial blocks (the retransmitted SYNs), connects if a slot frees
within the retransmit horizon, and otherwise fails `ETIMEDOUT` — a deadline-less dial into a
saturated listener fails in bounded virtual time, never a permanent sim-only hang
(`TestDSTNetBacklogFullDialTimesOut`; the horizon arms after the SYN traversal, so the bound is
½RTT + horizon — never earlier than a kernel's connect()-anchored timer). Closing a listener
resets the connections still in its accept backlog (production's RST), so a dialer that already
succeeded observes `ECONNRESET` instead of blocking durably forever, and `Accept` after `Close`
always fails with `net.ErrClosed` — including an `Accept` already blocked in its select when
`Close` runs: the overlap linearizes to close-first (the pending
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
re-Listen the same address). Connections and listeners are stateful capabilities of their creation
epoch: stale reads, writes, accepts, and deadline changes fail as closed without touching transport or
current-run state. Address accessors retain creation-time metadata, while stale `Close` performs only
wrapper-local idempotent cleanup. This is the reliable, in-order **base** on which network faults layer
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
them inside the run. **The mkfs image is part of the durable image**: the initial tree (root and
`/tmp`) is on the platter from birth, so a host crash preserves it and fsync-disciplined state
under `/tmp` — fsync(file) then fsync(`/tmp`), with no fsync of `/` — survives byte-exactly
(`TestDSTCrashHostPreservesMkfsImage`, torn variant included); a tree born unsynced would erase
`/tmp` at the first crash, recoverable only by fsyncing `/` — which no POSIX-disciplined program
does for a pre-existing directory.
Paths resolve against a per-process working directory (initially `/`; normal exit, explicit process
crash, or host crash discards
it, so a same-name restart begins at `/`). **Resolution is component-wise (physical), as the kernel walks:** every intermediate
component must exist and be a directory — `/missing/../tmp` is `ENOENT` (the walk reaches `missing`
first), `/file/../other` and `/file/sub` are `ENOTDIR`, and a trailing slash asserts directory-ness
(`open("/regularfile/")` is `ENOTDIR`; `open("/new/", O_CREATE)` is `EISDIR` — a trailing slash cannot
mint a regular file). `..` is evaluated against the tree during the walk, never
erased lexically first — a purely lexical `path.Clean` would make sim-only successes out of path
bugs a real kernel rejects. (`..` at the root stays at the root, as POSIX resolves it.) The working
directory is walked before operation-specific terminal `.`/`..` restrictions are applied: removal
and rename report an earlier missing or non-directory intermediate first.
The working directory is a PATH, not a node reference: renaming a directory out from
under the cwd leaves the cwd pointing at the old (now missing) path — a deliberate simple model,
recorded here as contractual (the host's fchdir-tracked inode semantics are not promised). A
`DirEntry` from a listing carries its listing-time `Info` snapshot rather than re-statting lazily
as the host does. Directory listings (`os.ReadDir`, `File.ReadDir`/`Readdir`/`Readdirnames`, including
chunked `n > 0` reads against a stable cursor) are **sorted by name** — deterministic, and
consistent with `os.ReadDir`'s documented sorting. Mod times come from the bubble's fake clock.
Permission bits are stored and reported but not enforced in the base model (no simulated
credential checks), and ownership is not represented at all — `Chown`/`Lchown` and `File.Chown`
stay fenced. **umask is not modeled** (recorded stance): created files and directories store the
requested mode verbatim — `os.Create` yields `0666` where a default-umask Linux yields `0644` —
because a simulated umask would be one more machine-state input for no determinism gain; a SUT
asserting host-masked modes is asserting machine state. Mode bits beyond the permission bits that
`Chmod` can set (`setuid`/`setgid`/`sticky`) are preserved on create exactly as `Chmod` preserves
them — create and `Chmod` never disagree about which bits are representable. Error identity is
production-shaped throughout `errors.Is`: `*PathError`/
`*LinkError` wrapping `syscall.ENOENT`/`EEXIST`/`ENOTDIR`/`EISDIR`/`ENOTEMPTY`/`EBADF`/`EINVAL`,
`os.ErrClosed` on use-after-close, exactly as the host would shape them — including `EISDIR` for
`O_TRUNC` on a directory regardless of access mode (an open real kernels reject may not mutate
simulated state, not even a mtime), and `os.Remove` of a non-empty directory surfacing rmdir's
`ENOTEMPTY` (`EINVAL` is reserved for `"."`, as on the host). POSIX namespace
semantics hold where databases depend on them: an open file removed from the namespace
(`Remove`, or replaced by `Rename`) keeps its content readable and writable through the open
handle until close — content lives on the node, names are references.

**Durability contract (spec tier — governs the fault feature; settled here so the write path
cannot foreclose it).** Every mutation — file write, truncate, create, remove, rename, metadata
change — enters the tree as **unsynced**. `File.Sync` and Linux virtual-fd `syscall.Fsync` commit a
regular file's content and size durably; Linux virtual-fd `syscall.Fdatasync` is a distinct modeled
regular-file barrier, and currently commits the same gmdb-relevant durable image. A file's *name*
becoming durable is a property of its **parent directory** (POSIX: data durability and entry durability
are separate — fsync the file, fsync the directory), committed by syncing an open handle on the
directory (`File.Sync` or Linux virtual-fd `syscall.Fsync`). `Fdatasync` on a simulated directory is a
deterministic `EINVAL`; directory entry durability is through `Fsync`. `Rename` is atomic in the namespace
(observers see old or new, never neither/both); its durability rides the parent directories' sync state
like any other entry change. Crash recovery may retain old and new aliases for one renamed inode, but
rename containment is checked by node reachability, so no alias spelling can move a directory into itself
and create a cycle. A simulated **host crash** (`CrashHost`, power loss) restores exactly the durable image:
synced state survives byte-exactly; unsynced data and entries are lost — including an unsynced
REMOVAL, which the crash undoes, since the removal itself was never on the disk. The restore
**commits the restored image as the new durable image** — it is, by definition, what the platter
holds after the reboot — so a second crash with zero intervening writes changes nothing, torn or
untorn (`TestDSTCrashHostSecondCrashIsNoop`); a restore that left the durable image at its
pre-crash state would let the next crash revert platter bytes with nothing having written, a state
no real crash ordering can produce. (A process crash
tears nothing: the kernel survives it, so its page cache does.) The default policy loses everything unsynced; `Options.CrashTear`
instead explores the outcomes the contract permits — each dirty page of a file lands, does not, or tears
at a byte boundary, and each unsynced name change lands or does not, all drawn from the fault RNG and
replay-exact. The tear is page-structured for soundness, not convenience: a page carries the current
bytes of every byte in it, so no crash can persist an older write's bytes for a byte a newer write
covered (see faults.md, the disk Crash axis). No atomicity of
individual `Write` calls is promised beyond what was synced. Metadata durability is an inode property:
a node reaches the disk with the mode and mtime it was created with, so a file whose parent directory was
synced recovers with its creation mode even if the file itself was never synced (a later unsynced `Chmod`
reverts, like any unsynced change). Disk FAULTS are properties of the media, not of the kernel: a bad
sector, a full disk, and a slow device all survive a host crash. The base (no-fault) model is the collapse of this contract where crash never
fires: everything survives, and `Sync` is *not* a no-op — it moves the synced/unsynced boundary,
which the representation carries from day one (per-node durable image + pending state). The fault
feature later adds crash/EIO/ENOSPC/latency as **policies over this representation**, never new
representation — same layering as network faults over the registry. The monotonicity half is
ENFORCED (promoted from spec tier when the durable-image mechanism landed): `TestDSTFSDurabilityMonotonicity`
asserts over a test-only node inspector (current vs durable content, sorted entry-name sets,
metadata image) that content writes, truncate, O_TRUNC, and entry create/remove/rename leave the
durable image untouched — including that the image is a copy, never an alias of live state — and
that on the MUTATION paths sync alone advances it (the one other writer is the host-crash restore,
which re-bases the durable image to the platter's post-crash state — see the crash contract above). `O_SYNC` commits per WRITE through the same single commit point,
but only for a write that WROTE (`n > 0`, matching Linux's `generic_write_sync`): a zero-length
write, or one the ENOSPC cap fully refused, commits nothing (a partial write still commits its `n`
bytes) — so the pending unsynced data stays out of the durable image and a crash can lose it as hardware
would. Linux `O_DSYNC` uses the same `n > 0` rule but commits only file data and size; mtime remains
unsynced and rolls back on crash (`TestDSTFODSyncCommitsDataWithoutMetadata`).
On Linux architectures where `O_SYNC` aliases `O_DSYNC`, the sole exposed flag value has data-sync
semantics; a distinct full-sync request is not representable by that platform's API.
This keeps the crash-tear surface honest (`TestDSTDiskOSyncZeroWriteDoesNotCommit` asserts the
durable image directly — the loss a crash then exposes is unconditional, where a tear restore only
MAY drop the pages).
ftruncate is deliberately not covered (POSIX synchronized I/O is for writes — committing on
truncate would grant durability real disks do not, hiding exactly the bug class DST exists to
catch). The metadata-CHANGE operations (Chmod/Chtimes, named Truncate) are implemented with the same
contract — they mutate current state only; `TestDSTFSMetadata` pins that post-sync Chmod, Chtimes,
and named Truncate all leave the durable image untouched, and that Chmod does not move mtime
(chmod(2) updates only ctime, which is not modeled). One deliberate shape: `Chtimes` on a missing
path is ENOENT even with both times zero — Linux's utimensat both-OMIT-succeeds quirk is not
reproduced.

**The file handle is a backend with a virtual fd.** `os.File` gains a dst backing chosen when the
File is created: the tree-file backend here, and the pipe feature's `os.Pipe` landed exactly there
— a stream-shaped second implementation of the same seam (`dstFileBackend`), a backend rather than
a retrofit, validating the Non-foreclosure invariant this paragraph recorded for that slot when the
seam was built. On Linux, a tree file or directory's `Fd()` returns a **virtual descriptor** owned by the
calling simulated process; non-Linux simulated file `Fd()` remains fenced until its raw-syscall boundary
can fence virtual fd numbers before host dispatch. A virtual descriptor is only meaningful to the DST
syscall boundary: selected split-safe Linux `syscall` package wrappers dispatch it back to the file backend,
and every unsupported operation on it is refused or returns a deterministic kernel-shaped error. Virtual
descriptors live in a **reserved number range** ([2³⁰, 2³⁰+2²⁰)) the simulation owns outright: the named
wrappers answer `EBADF` for any in-range number not in the live table, and the raw boundary refuses the
whole range — issued or not — so no in-range number can reach the host (a genuine host fd there would need
`fs.nr_open` raised beyond 2³⁰; the range is the simulation's namespace). Virtual-fd `Fstat` carries a
synthetic file identity: `st_dev` derives from the owning host's id and `st_ino` is a per-node inode
assigned at creation from a schedule-deterministic counter — stable across rename and while
unlinked-but-open — so `(st_dev, st_ino)`-keyed SUTs (the SQLite/LMDB per-file lock-dedup pattern)
distinguish files; directories report `Nlink` 2 (per-subdirectory increments are not modeled), regular
files 1. Proc-overlay fds carry no tree node and report zero `(st_dev, st_ino)` — synthetic procfs
stats are not an identity surface (recorded; no SUT keys file identity on procfs entries). A virtual fd never allocates or names a
host descriptor, and the raw-syscall fence still catches host-resource minting and unsupported
syscalls before they can reach the host. The **raw boundary dispatches a settled subset**: a SUT that
reaches the kernel through `golang.org/x/sys/unix` (whose asm enters `syscall.Syscall`/`Syscall6`
directly, never the named wrappers) gets the same modeled behavior for the file barriers (`fsync`,
`fdatasync`), advisory locking (`flock`), descriptor `close`, and the mapping operations (`madvise`,
`mprotect`, `munmap`) — so `unix.Fdatasync(fd)` and `syscall.Fdatasync(fd)` are one operation, not two.
"Split-safe" is the constraint that fixes the set: the dispatch allocates and takes locks, so it can grow
the stack, which is fatal once a trampoline has called `entersyscall` (no P) — `Syscall`/`Syscall6` fence
BEFORE that and therefore dispatch, while `RawSyscall`/`RawSyscall6` (reached post-`entersyscall`, and
post-fork with no P) still refuse. Everything else on a virtual fd — `read`/`write`/`pread`/`pwrite`/
`lseek`/`fstat`, whose argument shapes ride the 6-argument form or vary per arch — stays fenced at the
raw boundary and is reached through `os.File`, whose named wrappers dispatch. A raw operation on a HOST
fd, on a non-mapping address, or outside this set meets the fence exactly as before. The virtual fd table is per process; close releases the
descriptor. Process teardown — on crash AND on normal body return, which models process **exit** (see
the crash contract in faults.md) — closes every simulated file owned by the victim process and releases its
virtual descriptors, so stale fd capabilities fail as closed/bad-fd and any fd-owned kernel state is
dropped with the process.

Linux virtual fds support BSD-style `syscall.Flock` on regular tree files and directories. Supported
operations are `LOCK_EX`, `LOCK_SH`, `LOCK_UN`, and `LOCK_NB`. Locks are scoped to the simulated host and
file node, owned by the simulated process and fd, and released when that fd closes. An incompatible
nonblocking lock returns `EWOULDBLOCK`; an incompatible blocking lock waits until the lock becomes
compatible. Lock **conversions** follow Linux's remove-then-try semantics (`fs/locks.c`): the holder's
existing lock of the other type is dropped before the conflict scan, so a successful conversion is atomic
and a FAILED nonblocking conversion has lost the old lock — `EWOULDBLOCK` leaves the caller holding
nothing (retaining it would keep executions no real kernel produces). Exit- and crash-time release are
part of process resource teardown in the fault contract. Other file-lock
front doors remain fenced until they have a split-safe virtual-fd dispatch.

Linux virtual fds support shared file mappings for database page readers and lock-file coordination.
**A regular file's bytes are its page cache**: a memfd the harness owns (no simulated descriptor ever
names it), whose length is the file's length, written and read by every modeled operation through one
private view. `syscall.Mmap(fd, offset, length, prot, MAP_SHARED)` on a regular tree file returns a real
`MAP_SHARED` mapping of that memfd: distinct calls return **distinct views of the same pages** — as on
real Linux — so co-located processes mapping the same file range observe each other's writes, a `write(2)`
is visible through every mapping, and a store through a writable mapping is visible to `read(2)`, because
they are the same pages, not because a ledger copies them. Overlapping and bridging windows, and windows
mapped before a file grew, all cohere the same way; `sync/atomic` operations on shared mapped bytes are
genuine shared-memory atomics. `PROT_READ` requires a readable fd and `PROT_READ|PROT_WRITE` a read/write
fd; protection is **hardware**, per view: a store through a read-only mapping faults. A fault inside a
mapping — a store through a read-only view, or an access to a page the file does not have — is the
simulated process's death: unswallowable (checked ahead of `recover`'s reach, `debug.SetPanicOnFault`
included), killing exactly the touching process while peers and the harness run on, as production SIGBUS
does. Mapping-fault death runs the same exactly-once lifecycle as explicit process crash: all invocation
threads die, files/fds/flocks/mappings/connections/listeners close, and cwd/environment state is discarded
before a same-name restart. **Mapping addresses are a pure function of the schedule**: every mapping is carved `MAP_FIXED` from
a canonical reserved region at bump-allocated offsets, reset at the run boundary, so one seed yields one
address within a process and across invocations — the address is observable (the SUT holds the slice), so
replay-exactness owns it; region exhaustion is therefore also deterministic and reports `ENOMEM`.
**Host capability floor — and its true scope**: the page-cache backing is universal (every regular
file's bytes live in a memfd from birth), so a 64-bit host with 4096-divisible pages and address
space for the canonical region is a requirement of the simulated FILESYSTEM as a whole, not of the
mapping feature: on a 32-bit host, a coarser-than-4096-page host (e.g. 16K-page Apple-silicon VMs),
or a VA-39 host whose address space cannot hold the region (e.g. default Raspberry Pi OS), the FIRST
regular-file creation — any `os.Create` — throws loudly and deterministically, before any mapping
exists. The mapping
lifetime is independent of the descriptor lifetime: closing the fd does not unmap it; `Munmap` unregisters
exactly the mapping passed to it — and a later touch of the unmapped range is the toucher's own death,
as production SIGSEGV delivers it. The run epoch resets any residue, closing every page cache and
taking back the region in one stroke. A mapping whose owning process exited or crashed, or whose
machine died, is gone with its owner: touching it is a loud, NAMED harness abort — a deliberate divergence, since production
would deliver the toucher its own SIGSEGV; a mapping reached after its owner's death means a slice
crossed a process boundary (outside the model — see below), and naming that beats laundering it as one
more process death. Never a silent read, and never an "unexpected fault address" that reads as a
harness bug. Attempts to create writable private mappings, map without the
required fd access mode, or unmap only a subrange fail deterministically. Offsets validate against a
**fixed simulated page size of 4096** — never the host's page geometry, which is machine state (every
16K-aligned offset is also 4K-aligned, so host-derived offsets stay valid; the host MMU enforces at its
own page size, which the 4096 floor guarantees is never coarser). **A mapping may be a reservation**:
mapping past EOF is ordinary, an access to a page wholly past EOF is the simulated process's death, and
growing the file makes the grown pages readable through every existing reservation with no remapping —
the shape a database maps its data file with. The file's last partial page behaves as the kernel's page
cache does: its untouched tail reads zeros, a write to the tail through a writable mapping is visible to
every mapping, and if the file grows over it the tail bytes become file content — tmpfs semantics, one
real Linux behavior, but one POSIX leaves unspecified (a disk filesystem may zero the tail on writeback),
so portable code must not rely on tail-write survival; the bytes are in any case as undurable as every
unsynced write. **Truncating a file under a live mapping is ordinary ftruncate semantics**: the call
succeeds whatever mappings exist; the cut pages trap on later access (the process dies, as under
production SIGBUS), the partial page's tail zeroes, and bytes a shrink dropped never resurface through a
re-growth. The mapping region is also the model's capability ceiling: a file size or mapping set the
region cannot hold (2^40 bytes on the wide architectures, per-run, views and reservations together) is a
loud deterministic refusal — a harness limit stated here so it is never mistaken for a modeled errno.
Empty `Mprotect`, `Madvise`, and `Munmap` ranges return `EINVAL` through both named wrappers and
raw-ABI `Syscall`/`Syscall6`: an empty range names no mapping, so its address cannot establish
simulated ownership. `RawSyscall`/`RawSyscall6` retain the boundary's no-P refusal rule.
`Mprotect` and `Madvise` on a non-empty subrange whose file offset is not 4096-aligned are `EINVAL`
(the deterministic analog of the kernel's page-aligned-address requirement).
`Mprotect` may set any R/W protection the mapping's file DESCRIPTOR allowed at map time —
VM_MAYWRITE follows the fd's access mode, not the map-time prot, so an `O_RDWR`-backed read-only
mapping may upgrade to `PROT_READ|PROT_WRITE` and `PROT_NONE` is always permitted, while an
`O_RDONLY`-backed mapping may never gain write (`TestDSTFSVirtualFDMmapReadOnlyShared`); protections
beyond the R/W bits are unmodeled, refused as at map time.
Process teardown unmaps the victim process's mappings — no write-back exists or is needed, since the
bytes were the page cache's all along, visible to survivors and to a restart; a HOST crash instead
rewinds the page cache to the durable image (exit and crash move no durability boundary). `Madvise`
accepts the page-cache hints used by database readers (`MADV_POPULATE_READ`, `MADV_HUGEPAGE`,
`MADV_COLD`); unsupported advice values fail deterministically. Mapping slices are process-owned
capabilities, not an IPC channel: passing one to another simulated process is outside the model, while
file writes by any process on the same simulated host update mappings through the shared page cache.

`os.OpenRoot` and rooted `Root` operations are modeled over the same tree. Opening a root captures
the directory node identity, so a `Root` keeps addressing that node across namespace renames rather
than re-resolving the path string passed to `OpenRoot`. A captured node REMOVED from the namespace
(`Remove`/`RemoveAll`, or replaced by `Rename`) is an rmdir'd directory: entry creation in it —
create, mkdir, rename-into — fails `ENOENT` through the still-open `Root`, as openat(2) answers on
the host; reads of the (empty) node itself keep working. Root-relative paths are walked component-wise
from that captured node; absolute paths and `..` walks above the opened root fail instead of resolving
against the process cwd, the tree root, or the host filesystem. Rooted file, directory, metadata,
removal, and rename operations preserve the same path, metadata, durability, and no-host-passthrough
contracts as the named `os` surface.
A simulated `Root` is an owned open capability: normal process exit, process crash, and host crash close
every Root created by that process or kernel. Retained values then fail every rooted operation with
`ErrClosed`, exactly like an explicitly closed Root; a reboot never revives the directory capability.

Symlinks and unsupported file-locking surfaces are fenced until modeled — "not yet modeled" never means
"reaches the host": within this feature's surface (the os file and namespace API; `os/exec`'s process
surface is its own roadmap item), every handle-producing or namespace-touching entry point is either
implemented in-sim or fails with the unsupported shape while a run is active
(`os.Pipe` is simulated — see "Deterministic pipes and the stdio stance"). On Linux, a host-backed `File`
carries no authority inside a run unless `simulation.InheritFile` explicitly grants
the root simulation body a capability. `Host` and `Process` bodies cannot grant host files: that would
create a cross-node channel outside the simulated filesystem and network. The capability owns a hidden duplicate of the granted open-file description,
so closing the source does not revoke it and the grant extends that description's lifetime until the
capability closes. Numeric real fds, including the source and the hidden duplicate, are never authority:
`Fd`/`SyscallConn` are unavailable on the capability, `SyscallConn` and deadline mutation are fenced
on ungranted host files, and raw real-fd operations are fenced. A `Root`
opened before the run remains unsupported. Admission holds the source `File`'s operation reference
across duplication, so an ordinary concurrent `File.Close` cannot retarget the grant; code that mutates
the source descriptor behind the `File` through foreign assembly or cgo is outside the same API trust
boundary as every other externally-corrupted `os.File`. Symmetrically, a simulated `File` or `Root` leaked OUT of
its run is refused like a closed handle
(both carry the run epoch; the run's nodes are released with the run — lazily, at the next run's
first filesystem op) — deterministic and
host-isolated, the same discipline applied in reverse, and never a read of a prior run's tree nor a
dereference of its released page caches (`TestDSTRootLeakedAcrossRuns`). An
operation pairing a simulated handle with an inherited-file capability behaves as its two halves: the
simulated side goes through the gated funnels and the capability side does the explicitly granted host
I/O (`io.Copy` takes the generic loop because the zero-copy fast paths bail whenever either side is
simulated).

The inherited capability supports the typed operations represented by the file-backend seam: reads,
writes, positional reads/writes, seek, stat, sync, truncate, chmod, directory reads, deadlines,
and close. Other `os.File` methods retain the simulated-file unsupported behavior. The capability is
Linux-only until another operating system enforces the same no-numeric-authority boundary.

### Deterministic pipes and the stdio stance (the third I/O feature)

`os.Pipe` under a run is **owned by the fork**: it returns a pair of Files backed by one
in-memory byte stream — the stream-shaped second implementation of the `dstFileBackend` seam —
and never allocates a host descriptor (enforced by a /proc/self/fd census across a run). The virtual
fd surface is deliberately file-tree-only until a pipe fd contract is settled: `Fd()` on a simulated
pipe still panics, `SyscallConn` is fenced, zero-copy fast paths bail, `net.FileConn` rejects, and a
simulated pipe handed to `os/exec` hits the `Fd()` panic (loud; the process surface is its own roadmap
item).
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

**Stdio is not implicitly inherited.** `os.Stdin`/`Stdout`/`Stderr` are ordinary pre-run host files,
so their methods are fenced inside a run. Code that deliberately needs host stdio calls
`simulation.InheritFile` inside the run and uses or installs the returned capability; captured or fully
deterministic stdio instead assigns the package variables to simulated files. Capability writes are
outbound, schedule-ordered side effects that feed no nondeterminism back into the run.
The blocked case is covered too: a host write that blocks (a full pipe, a slow terminal) delays
the run in *wall* time but cannot reorder it, because sysmon's **syscall-handoff retake is gated
under an active run** exactly as its preemption retake is (`retake`, `proc.go`) — without that gate,
whether a host write returns within sysmon's 10 ms
window would decide whether the P is handed off mid-syscall, a wall-clock-dependent schedule fork.
A capability's real syscall thus *serializes* the bubble for its duration: one legal execution,
deterministic. The dual failure mode is recorded plainly: a goroutine blocked **reading** an inherited
capability is syscall-blocked, not durably blocked — the bubble can neither advance fake time over it
nor declare deadlock, so the run hangs until the read returns. Reading the real terminal under a
run is therefore an explicit capability choice, not an accidental numeric-fd escape.
Completing the audit of the remaining OS-backed I/O surface: `io.Pipe` is pure memory;
`ReadFile`/`WriteFile`/`CreateTemp`/`MkdirTemp` ride the simulated `OpenFile`; `Hostname` and
`Getpid` are Options-pinned; env APIs operate on the per-process simulated environment (see the
identity section); `os.Executable` is **fenced** under a run (it reads `/proc/self/exe` — a host
path that names nothing in the simulated namespace); processes and signals are fenced (see the
interception boundary below).
One recorded gap: **`os.DevNull`** — the tree starts empty except `/tmp` (that contract stands),
so opening `/dev/null` under a run is `ENOENT`; the in-sim idioms are `io.Discard` for a sink or
an ordinary tree file, and the main host consumer of `/dev/null` (process spawning) is out of
scope here. If a modeled `/dev/null` ever earns its place, it is a new node kind behind the same
seam — an increment, not a retrofit.

### The interception boundary (raw syscalls, processes, signals, cgo)

The `os`/`net`/time/rand seams cover the surface a SUT reaches through the standard library's
portable API. The floor below them — the `syscall` package, and everything that calls through it,
including `golang.org/x/sys/unix` (its wrappers invoke `syscall.Syscall*`) — cannot be *simulated*
(it is the raw kernel ABI), so it is **fenced**: the filesystem section's rule that "'not yet
modeled' never means 'reaches the host'" applies to the whole boundary, not just the `os`
namespace. Without the fence, a dependency calling `syscall.Open` or `unix.Getrandom` mid-run does
real host I/O and reads real entropy silently — same seed, different run, no error — which
falsifies host isolation as an *enforced* invariant. Every fence below fires only for **bubble goroutines while a run is
active** — non-bubble goroutines keep full host access, so the harness around the run is untouched:

- **Resource-minting `syscall` entry points** (`Open`/`Openat`/`Creat`, `Socket`/`Socketpair`,
  `Pipe`/`Pipe2`, `Dup`/`Dup2`/`Dup3`, host-backed `Mmap`, `ForkExec`/`Exec`) fail with the standard
  "unsupported under deterministic simulation" shape — loud and deterministic, exactly like `Fd()`.
  A minted host resource is a simulation escape; refusing it is the fence, absorbing it silently is
  the defect. On the socketcall architectures (386, s390x) the socket-family wrappers dispatch
  through the dedicated `socketcall`/`rawsocketcall` entries rather than the fenced trampolines, so
  those entries carry the same fence (same predicate and refusal shape as the raw `SYS_SOCKETCALL`
  trap; one chokepoint covers every socket-family wrapper). The socket family of
  `golang.org/x/sys/unix` enters through the same fenced entries on 386 — its assembly jumps to
  `syscall.socketcall` by linkname, a path the trampoline choke point never sees there. Pinned by
  `TestDSTSyscallFence`'s `syscall.Socket` and `syscall.Bind` wrapper probes and by
  `TestDSTSocketcallEntryFenced` (the linkname entry x/sys resolves to refuses in-bubble; the pull
  linkname also turns dropping the export into a build failure). On non-bubble fallthrough, these
  entries preserve the raw-syscall pointer-lifetime contract: pointer-derived `uintptr` arguments
  remain live and the caller stack cannot move before kernel dispatch, including through the entry
  symbols used by external assembly callers.
- **The generic trampolines** `Syscall`/`Syscall6`/`RawSyscall`/`RawSyscall6` are fenced the same
  way — this is the choke point that catches `golang.org/x/sys/unix`. A numeric real fd is never a
  capability: read/write/close, lseek, pread64/pwrite64, fstat, fcntl, and every other operation are
  refused before host dispatch. Explicit inherited-file capabilities perform their host operations
  through a scoped trusted path and never expose their hidden descriptor. This is an API boundary,
  not an adversarial machine-code sandbox: a dependency that executes a syscall instruction in its
  own assembly (rather than entering the standard `syscall` symbols, as ordinary `x/sys/unix`
  wrappers do) is outside the model like cgo and unsafe host-memory access.
  `ioctl` is refused entirely: request numbers are interpreted by the target device, so no numeric
  request can prove read-only, non-minting behavior for an arbitrary inherited handle. Terminal
  probing therefore requires a modeled terminal capability rather than host passthrough. The reserved virtual-fd number range never reaches
  the host from this raw boundary: `Syscall`/`Syscall6` DISPATCH the settled subset (`fsync`,
  `fdatasync`, `flock`, `close`, `madvise`, `mprotect`, `munmap` — the surface `golang.org/x/sys/unix`
  reaches, since its asm enters these trampolines rather than the named wrappers), and every other
  in-range number or operation is refused outright, issued or not. `RawSyscall`/`RawSyscall6` refuse the
  range entirely: they run without a P, where the dispatch's allocation cannot grow the stack (see the
  virtual-fd paragraph in the filesystem section); selected split-safe named
  Linux wrappers (`syscall.Read`/`Write`/`Close`/`Seek`/`Pread`/`Pwrite`/`Fstat`, virtual-fd
  `Fsync`/`Fdatasync`, plus the supported `Mmap`/`Munmap`/`Mprotect`/`Madvise` mapping operations)
  dispatch them to the simulated backend. The harness's own page-cache descriptors (the memfds
  backing simulated files) are **invisible in the simulated fd namespace**: a bubble goroutine's
  close naming one answers `EBADF` at the trampolines — exactly what a fd the
  process never opened would get, so the daemonize-style close sweep stays the harmless loop it is
  in production — never host I/O, which would kill a live file's cache (fatal at the next resize or
  mmap) or, after fd-number reuse, silently alias another file's bytes. On the 64-bit hosts the
  page cache admits (32-bit hosts are refused before any memfd exists), every named fd wrapper
  bottoms out in the same trampolines, so one chokepoint covers both surfaces; non-bubble callers
  are untouched, like the rest of the fence (`TestDSTMemfdFDIsolation`). A bubble goroutine's close of **any** real
  (non-virtual) fd number is answered `EBADF` at the trampolines and **never dispatched to the
  kernel**. The invisibility check alone cannot protect a fd that does not exist yet — the fence
  check and the kernel entry are not atomic across Ms, so a dispatched close of a then-free
  number races the harness assigning that number to a newborn host fd (a page-cache memfd, or
  the runtime's lazily-created netpoll epoll fd) and can land after the assignment, killing the
  newborn. Refusing dispatch outright removes bubble-originated *destruction* of host fds
  entirely — a close that is never dispatched cannot straddle a creation — for the whole host-fd
  space rather than one fd class. Refusing every other numeric real-fd operation removes the same
  check-to-dispatch alias window for reads, writes, seeks, stats, and fcntl: no admitted operation can
  straddle creation of a newborn harness descriptor. `EBADF` for close is sound
  because a bubble goroutine can never *mint* a host fd (the fence refuses open/socket/pipe/dup),
  so it owns no real fd to legitimately close: every bubble close of a real number is the
  daemonize-sweep shape, and `EBADF` is exactly what production gives that sweep. The
  knowingly-accepted divergence: a bubble close of a real pre-run handle reports `EBADF` where
  production reports success, and the handle stays open — so a
  non-bubble peer waiting on that handle's closure (an EOF on a harness pipe whose write end the
  bubble "closed") waits forever, and a write-then-close integrity idiom sees the `EBADF` — a
  host-table mutation the bubble is not allowed to make, in exchange for closing the
  creation-race window (`TestDSTHostFDCloseRefused`). Raw Linux `clock_gettime` for `CLOCK_MONOTONIC` and
  `CLOCK_BOOTTIME` is also selected and split-safe — at the 32-bit-time trap on every arch AND the
  time64 trap (`clock_gettime64`, `__kernel_timespec`) on the 32-bit arches that have one: it
  returns the DST virtual base clock, and
  boottime coincides with monotonic time until a suspend model exists. Native and time64 output
  ranges retain kernel copyout semantics once the value is representable: an invalid, read-only,
  partial, or wrapping range returns `EFAULT` rather than becoming a Go panic, fatal fault, or
  simulated-process death; native time32 retains `EOVERFLOW` precedence for unrepresentable
  seconds. Valid unaligned ranges receive the virtual value, and any bytes written before a partial
  copy faults come from that virtual value rather than a host clock.
  Anything outside the family is fenced, deliberately erring loud.
- **All-thread syscalls**: `AllThreadsSyscall` and `AllThreadsSyscall6` are fenced at entry for a
  bubble goroutine, before cgo selection or runtime all-thread dispatch. Applying a syscall to every
  host thread is process-wide host mutation, not a simulatable raw-kernel operation; non-bubble
  harness callers remain untouched while a run is active.
- **Processes**: `os/exec` and `os.StartProcess` are fenced with the same shape (a real child is
  wall-clock, host-visible work no seed controls). Today a spawn fails only *accidentally*
  (a misleading simulated-FS `ENOENT` on the `/dev/null` stdin open, or the `Fd()` panic when
  stdio is simulated); the fence makes the refusal designed and nameable.
- **Signals**: the `os/signal` operations that touch the process's signal machinery from a bubble
  goroutine are fenced — `Notify`/`NotifyContext` (subscribe: a real signal delivery is an
  outside-bubble event on a wall clock; today it crashes the bubble only *if* a signal happens to
  arrive — the fence makes the refusal deterministic instead of luck), and `Ignore`/`Reset`/`Stop`
  (which mutate the process-global signal *disposition* via `rt_sigaction` — `Ignore`/`Reset`
  unconditionally, even with no subscribers — so a bubble's `Ignore(SIGINT)` would set the whole
  host process, harness included, to ignore the signal: a bubble effect leaking into host state).
  `Ignored` is a host-state *read* that mints and mutates nothing, so like the other ⛔ reads it is
  program discipline, not fenced.
- **cgo**: fenced at the call, not the build — `cgocall` from a bubble goroutine while a run is
  active panics with the unsupported shape (mirroring the FIPS gate's enforced-not-documented
  stance). Gating on `iscgo` at run entry would be too coarse: a binary may *link* cgo it never
  calls in-run.

What this fence deliberately does **not** cover, recorded as program discipline (the ⛔ rows of
the sources table): reads of host machine state through APIs that mint nothing (`runtime`
introspection — `NumGoroutine` counts process-wide goroutines including the harness,
`runtime/metrics` and `ReadMemStats` carry wall-time- and history-dependent fields,
`runtime.Stack(all=true)` dumps non-bubble goroutines), and address-derived observables —
including **iteration order of a pointer-keyed map** (`map[*T]V`, `map[unsafe.Pointer]V`,
`unique.Handle` keys): the fixed `-tags dst` hash key makes the *function* deterministic but the
*addresses* are process-history-dependent, so pointer-keyed iteration order is the `%p` class of
nondeterminism even under a fixed seed. DST's own substrate contains no observable pointer-keyed
iteration (the reset registry iterates in registration order — see faults.md); a SUT's is its own.

One raw-syscall path is deliberately outside the fence: the `syscall` package's `rawSyscallNoError`
(a fifth, asm-implemented raw entry that bypasses the four generic trampolines) backs
`Getpid`/`Getuid`/`Getppid`/`Gettid`/`Getegid` and `Umask`. The identity reads are the same
program-discipline stance as the ⛔ rows — `os.Getpid` and friends are intercepted to the simulated
identity, but a *direct* `syscall.Getpid` reads the host value; that is a host-state read that mints
nothing. `syscall.Umask` is the one *mutation* in this set: called directly from a bubble it changes
the process-global umask (nothing simulated observes it, but a later host file create would). It is
left to program discipline for the same reason the reads are — the fence's choke point is the four
generic trampolines (which catch `golang.org/x/sys/unix`, whose asm never routes through the
unexported `rawSyscallNoError`) plus the resource-minting wrappers, not this asm path. A SUT that
needs process identity or umask under a run uses the `os` API, which is modeled or fenced.

### Enforcing test configurations

**Untagged footprint (contract).** A non-`-tags dst` build carries zero CODE footprint: `dstBuild`
is a build constant, so every `if dstActive()`/`if dstBuild` guard — hot paths included: `NumCPU`'s
simulated-count branch, `gopanic`'s explore hook, the finalizer-execution loop's drain legs, and
`synctest`'s drain/teardown calls — is dead-code-eliminated. `TestDSTUntaggedCodeFootprint` pins the fold by objdump at the panic,
finalizer, NumCPU, and generic-AddCleanup anchors; the synctest legs share the same constant-guard
pattern and were objdump-verified at the change. The DATA layout is NOT zero-footprint, deliberately, in every build: `g` carries
fourteen per-goroutine DST words (the six identity/RNG stamps, the seven race-access staging
fields, and the sticky simulation-membership bit the scheduler classification keys on), `p` carries the run-queue overflow flag, `timer` carries fake-timer state (arming
host, full-width registration epoch, list link, and the overdue-conversion delivery shift), `synctestBubble` carries the GC-drain
bookkeeping, `specialfinalizer` carries epoch+sequence+PID, `specialCleanup` carries epoch while its
embedded cleanup carries sequence+PID, and `finalizer`/`cleanupFn` each carry registration sequence
plus run-epoch/process-invocation ownership (so untagged builds fit slightly
fewer entries per block). Restoring the untagged layouts would fork per-tag variants of the runtime's central `g`
struct and of hand-maintained GC bitmap construction (`finptrmask` and
`cleanupBlockPtrMask`, whose repeating patterns are load-bearing on queued callback layouts) — an
unsafe-critical duplication for a few words per object; recorded here as the deliberate limit, with
the rationale at the claim in `runtime/dst.go`.

The DST contract tests are dead in a stock `-short`/untagged run. The enforcing configurations are
the tasks in `Taskfile.yml` at the repo root (the A2-25 runner choice); each task name below is the
authoritative statement of its leg, and the `go test` command in the Taskfile is its definition:

- **`test:untagged`** (`go test -count=1 -short runtime`, untagged): DST hooks are inert; also
  enforces that `runtime/testdata/testprog` stays cgo-free — a cgo-pulling import there (net,
  os/user — DST fixtures needing those live in `testprognet`) disables the runtime's deadlock
  detection and hangs the crash tests loudly.
- **`test:dst`** (`go test -tags dst -count=1 -timeout 60m runtime testing/simulation crypto/rand net os syscall os/exec os/signal`,
  non-`-short`): the
  802-program sweep, the race-oracle and auto-instrumentation tests — which build their own
  `-race` testprogs — and the build-mode inertness test all skip under `-short`. The untagged
  build-constraint panic is covered by `TestDSTRunRequiresBuildTag`, which builds its own untagged
  testprog. `syscall`/`os/exec`/`os/signal` are in the list because the interception-boundary
  fences land there; the leg exercises their standard suites under `-tags dst` to enforce that the
  fences are inert for non-bubble goroutines (a fence firing outside a run would break these).
  `crypto/rand` enforces that the tagged outside-run entropy path remains allocation-free.
- **`test:dst-race`** (`go test -tags dst -race -count=1 testing/simulation`): the dst-race
  sync-hook encodings. The suite is `-race`-clean: every SUT that runs under `-race` is race-free —
  intentionally racy SUTs are either subprocess testprogs or skip-gated to the non-race leg via
  `dstRaceEnabledFP` — so a TSan report in this leg is a real finding; the skip gates are
  load-bearing for this invariant.
- **`test:inert-std`** (`go test -count=1 -short std`, untagged): build-mode inertness across all
  of std. Heavy; runs separately from the `test` aggregate, which runs the other three legs
  sequentially and fail-fast.
- **`test:cross`** (`GOOS=windows GOARCH=amd64` and `GOOS=plan9 GOARCH=amd64`, each
  `CGO_ENABLED=0 go build std`, untagged): ordinary standard-library builds remain valid for the
  two file layouts that do not carry DST backend storage. This leg makes no tagged DST support
  claim for Windows or Plan 9; the tagged filesystem requires a supported Unix, js/wasm, or wasip1
  file layout. It runs separately from the `test` aggregate.

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
clean-cache rule stands unchanged. All five legs gate green; a red leg is a regression against
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
| map iteration order (value-keyed) | per-g `g.dstrand` (`maps.rand`) + fixed process hash key (`-tags dst`); pointer-keyed maps are ⛔ — see the last row | ✅ |
| `math/rand`, `math/rand/v2` (top-level funcs) | `//go:linkname`'d to `runtime.rand` → per-g stream | ✅ |
| `crypto/rand` | `crypto/internal/sysrand.Read` seam → per-g stream | ✅ |
| time, timers, tickers | `testing/synctest` fake clock | ✅ |
| GC (count, finalizer/weak set, memory bound) | STW in-bubble GC + per-bubble relative trigger | ✅ |
| process identity (pid/ppid/hostname/uid/gid/NumCPU/user) | `os`/`os/user` seams + sim-env | ✅ |
| network I/O | in-memory deterministic `net` (`Dial`/`Listen`/`Conn`, address registry) | ✅ |
| filesystem / disk I/O | in-memory deterministic filesystem (os surface, per-bubble tree) | ✅ |
| pipes (`os.Pipe`) | in-memory deterministic pipe (stream backend behind the `os.File` seam) | ✅ |
| standard streams (stdio) | fenced unless explicitly granted with `simulation.InheritFile`; swap package vars to simulated files for capture; syscall-retake gated so a blocked capability write serializes, never reorders | ✅ |
| environment (`os.Getenv`/`Setenv`/`Environ`) | per-process COW env view (isolation enforced; unmodified reads are host-derived machine state) | ✅ |
| faults: net (latency/jitter/throttle/partition/reset), disk (EIO/ENOSPC/latency), clock (skew/step/drift) | policies at the existing seams over the Host/Process victim contract (see [faults.md](./faults.md)) | ✅ |
| faults: crash tear (torn/lost unsynced writes and names on power loss) | page-granular durable/current selection drawn from the fault RNG (`Options.CrashTear`) | ✅ |
| faults: process crash + restart | pid-keyed goroutine death + proc-keyed resource teardown (see [faults.md](./faults.md)) | ✅ |
| faults: host crash (power loss) + reboot | host-keyed goroutine and kernel-state death + durable-image restore (see [faults.md](./faults.md)) | ✅ |
| faults: seeded clock drift (`Drift`/`BoundedDrift` declared, `DriftClock` mid-run) | per-host rate over the base clock (see [faults.md](./faults.md)) | ✅ |
| faults: OOM kill, scheduling (straggler) | fault-orchestration layer (see [faults.md](./faults.md)) | ⏳ |
| raw `syscall` / `golang.org/x/sys` | fenced for bubble goroutines: numeric real fds carry no authority; explicit inherited-file capabilities are typed `os.File` values with no raw descriptor surface; close of a real fd is answered `EBADF`, never dispatched | ✅ |
| processes (`os/exec`, `os.StartProcess`, `syscall.ForkExec`/`Exec`) | fenced (loud "unsupported under deterministic simulation") | ✅ |
| signals (`os/signal.Notify`/`NotifyContext`/`Ignore`/`Reset`/`Stop`) | fenced for bubble goroutines (subscribe + host-disposition mutation) | ✅ |
| `os.Executable` | fenced (a host path naming nothing in the simulated namespace) | ✅ |
| cgo | fenced at `cgocall` for bubble goroutines (a binary may link cgo it never calls in-run) | ✅ |
| runtime introspection (`NumGoroutine`, `runtime/metrics`, `ReadMemStats`, `Stack(all)`) | process-wide, history/wall-time-dependent readings (see "The interception boundary") | ⛔ (program discipline) |
| raw pointer addresses (ASLR, `%p`, `uintptr`, **pointer-keyed map iteration order**) | — | ⛔ (program discipline) |

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
- **Network / Disk / I/O** — in-memory and deterministic (landed), with sound faults: on the
  reliable TCP base, flow-granular latency (= a fake timer), partition/blackhole, connection reset,
  throttle (byte-granular drop/reorder/duplicate are a UDP follow-on, not sound on a reliable
  stream); EIO/ENOSPC/disk latency (landed); torn and lost unsynced writes on host crash (landed:
  page-granular subset + byte-granular tear, `Options.CrashTear`).
- **Faults** — net, disk, clock skew/step/drift, process crash+restart, and host crash+reboot axes
  landed; OOM kill and *scheduling* faults (straggler) pending — each anchored to a real degree of
  freedom, so sound (see [faults.md](./faults.md)).
- Driven by a seed; replay-exact; failures shrinkable; invariants checked by the program's own
  assertions.

## Companion design documents

The mechanism designs live in companion docs under `docs/dst/`, each
governed by the top-tier contract in this file (read this contract first — Spec-first). This file is
the canonical spec; the companions are lower-tier designs that collapse from it.

- **[exploration.md](./exploration.md)** — the scheduling/exploration axis: Seq 5 seeded interleaving
  diversity + strategies, and Level 2 access-granularity DPOR.
- **[gc.md](./gc.md)** — deterministic GC for DST (full scope; the landed state is Tier 2).
- **[faults.md](./faults.md)** — the distributed model (Universe / Host / Process) and fault
  orchestration (the fault AXES are landed except OOM kill and the straggler; the declarative
  Options.Faults / seeded FaultPolicy layer and failure shrinking remain pending — see Pending
  features; designed upfront, built bottoms-up).

## Roadmap

The runtime substrate and the **I/O surface** are **landed**: the per-g RNG + scheduling (Seq 1,
Seq 5), the `testing/simulation` API, and — beyond the original sequence — deterministic process
identity, crypto/rand, GC, memory bounding, a hardening pass, the in-memory filesystem and network,
and the **interception boundary** (raw syscalls, processes, signals, `os.Executable`, cgo, and the
per-process environment), and the fault axes over them (net, disk, clock, process crash + restart, host
crash + reboot, crash tear) — each documented in its own section. The remaining work is the
**fault-orchestration** layer's remaining axes and its declarative surface (OOM kill, scheduling
stragglers — the one ⏳ row of the source table, see [faults.md](./faults.md)). Each step respects the fixed seams, so
later steps add, never rewrite.

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
  a fixed RNG-free policy (`dstFindRunnable` prefers them in candidate order). Infrastructure-first is
  **bounded**: after an infrastructure pick, a runnable simulation candidate gets the next decision,
  selected over the sim-only subset in stable simulation creation-index order, so physical
  local/global/runnext placement and the hand-off change only when decisions happen,
  never which goroutine one picks. **Exception: the simulation's own drain.** Under the scheduled
  strategies the bubble's finalizer/cleanup drain is infra-classified but has sim-visible effects
  (user callbacks), so it is exempt from the alternation in both directions: it outranks every other
  infrastructure candidate, and its pick neither owes the simulation the next slot nor can be
  displaced by the hand-off — the drain runs at the same logical points, uninterrupted between its
  yields, as in a foreign-free execution (its scheduling is not an interleaving degree of freedom
  the scheduler models). The root driver and transient GC-internal execution are transparent by the
  same rule, so they consume neither Random draws nor PCT steps. A persistently-runnable foreign goroutine (a user Gosched loop, a
  spinning foreign bubble) gets at most every other non-drain slot and cannot starve the bubble —
  the livelock an unconditional infrastructure-first policy would produce, undiagnosable because the
  bubble stays runnable and the durably-blocked deadlock detection never fires. Under the scheduled
  (exploration) strategy the same subset rule keeps foreign candidates out of recorded schedules and
  DPOR enabled sets, and foreign presence at a simulation decision is REPORTED
  (`ExploreResult.ForeignSched`, downgrading `Exhausted`): for the pinned workload classes —
  including the GC/finalizer class that used to diverge — recorded traces are byte-identical with
  and without churn in BOTH build modes — simulation membership is a sticky per-goroutine property
  (`g.dstSimG`), so a simulation goroutine parked inside GC-assist paths that transiently nil
  `gp.bubble` never becomes churn-displaceable infrastructure; consecutive singleton no-choice yields
  are coalesced after their first attributed transition; enabled sets are ordered by stable goroutine
  index rather than run-queue position; and the synctest root driver plus transient GC-disassociated
  execution are scheduled transparently. These rules remove the physical-order inputs that used to
  shrink `-race` coverage under churn and report foreign work where none ran. The downgrade stays conservative (foreign turns inside a
  simulation-idle fake-time-advance window can move a later decision across timer-wake publication and produce the explicit `DST-L2-2` prefix-divergence panic) — coverage under churn is best-effort and says
  so, never a silent cap. Enforced by `TestDSTSchedForeignSpinner` (run completes under a spinner;
  fingerprints with/without the spinner identical, random and PCT), `TestExploreForeignSpinner`
  (exploration completes with identical coverage and byte-identical recorded traces under spinners,
  churn reported), `TestExploreForeignGCWorkloadInsensitive` (the GC/finalizer workload class:
  churn-equal coverage and traces in both modes, exhaustion claimable foreign-free),
  `TestExploreForeignPriorRootSpinner` (membership does not leak across runs through the root),
  `TestExploreForeignSpinnerDrainCallback` (a mid-callback drain yield is neither
  displaced by foreign entries nor interrupted by the hand-off) and
  `TestExploreForeignSchedReported` (churn reported and exhaustion downgraded, including under
  `-race`). The simulation claims
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
  wire-backed connection pair (buffered byte-stream transport, synctest-durable, deadlines on the fake
  clock — see the transport-model contract in the "In-memory deterministic network" section) wrapped
  with simulated addresses, so unmodified networked code is reproducible without modeling the network
  itself. Determinism rides the scheduler (no new seed plumbing); the registry is keyed by a per-run
  epoch so it resets between runs. The reliable, in-order base for network faults. See the "In-memory
  deterministic network" section above; tested by `TestDSTNet`. Caveat: only the `net.Conn` interface — code type-asserting
  `*net.TCPConn` (raw fds, `SetNoDelay`) does not get one. DNS/service-name ports/UDP/Unix are
  follow-ons (`net.Interfaces` is landed — the synthetic `lo`+`eth0` set); public DNS and service-name
  lookups fail under DST rather than touching host resolver state.

- **Disk (in-memory deterministic filesystem). LANDED (second I/O feature).** Under DST the exported
  `os` surface operates on a per-bubble in-memory tree (empty root + a pre-seeded `/tmp`; fixed
  `os.TempDir`), reset by the run epoch: the full file-handle surface, the namespace ops with sorted
  deterministic listings and a per-bubble path-model cwd, unlinked-but-open POSIX semantics, named
  metadata ops, and the durability representation with its enforced monotonicity invariant (the
  synced/unsynced split crash faults will tear along). Everything not modeled is fenced — host
  isolation is an enforced invariant, not a convention. See the "In-memory deterministic filesystem"
  section above; tested by the `TestDSTFS*` family, the durability white-box, and the cross-process
  `TestDSTDiskReplay`. Caveats: no symlinks yet (a fenced follow-on; BSD-style `syscall.Flock`
  advisory locking IS landed — see "flock" above), no ownership
  model (`Chown` fenced; permission bits stored, not enforced), tree-file `Fd()` is virtual on Linux and only
  selected syscall wrappers consume it, `Sys()` is nil.

- **I/O (deterministic pipes + the stdio stance). LANDED (third I/O feature).** `os.Pipe` under DST
  is an in-memory stream behind the `os.File` backend seam the disk feature built — Linux anonymous
  pipe semantics host-probed end to end (64 KiB capacity, PIPE_BUF atomicity under contention, the
  full error-precedence ladders, fake-clock deadlines, partial counts, SameFile across the pair),
  synctest-durable blocking, no host descriptor ever. Stdio is settled as NOT implicitly inherited
  (programs explicitly grant a host file or swap the package streams in-run for capture), and
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
injection on top of the virtualized substrate. Most of its axes are landed (see the source table); the
open work is the OOM and scheduling faults and the declarative orchestration layer.

- **Fault orchestration** — compose scheduling, network, disk, clock, OOM, and crash/restart faults under
  one seed, with replay and failure shrinking. Each fault is anchored to a real degree of freedom (sound);
  all axes share one *victim-designation* contract — the **Host/Process** model (FS/network shared at the
  host, memory isolated at the process). **Contract settled** in [faults.md](./faults.md) (every axis +
  the shared contract designed up front, so no axis forecloses
  another); implementation is **bottoms-up** — the Host/Process substrate first, faults last (see "Build
  order"). The net, disk, clock, **process-crash/restart**, **host-crash/reboot**, and **crash-tear**
  axes are landed and replay-exact; what remains is the OOM (allocation-triggered) and scheduling
  (straggler) axes, and the L4 layer itself: the declarative `Options.Faults` set, a seeded
  `Options.FaultPolicy` as an `Explore` dimension, and failure shrinking.
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
