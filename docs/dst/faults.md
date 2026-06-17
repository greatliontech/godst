# DST distributed model & fault orchestration

> **Pending feature** (the last ⏳ row of the source table), governed by the top-tier contract in
> [design.md](./design.md) (Soundness / Non-foreclosure invariants, control surface, scope, roadmap).
> Settles the Universe / Host / Process substrate and the fault axes built on it; implementation is
> bottoms-up (see "Build order"). Code will conform to this contract.

## The distributed model: Universe / Host / Process

Status: **design settled, implementation pending** (it is the substrate the fault feature is built on —
see "Build order"). The fault feature targets *distributed* programs, and to fault one soundly the
substrate must first model what a distributed program **is**: not one process but **N processes on M
hosts**, with the right things shared and the right things isolated. The current substrate is the **N=1
collapse** of this model — one universe, one host, one process — so a program that never declares
hosts/processes runs exactly as today.

There are **three** isolation layers, and the load-bearing subtlety is that the sharing boundary differs
per resource — **filesystem and network identity are shared at the host level, but memory is isolated at
the process level**:

| Resource | Owned by | Why this layer |
|---|---|---|
| scheduler (one interleaving), **base** virtual clock + advance, Go heap/GC, seed + RNG roots, the inter-host network fabric | **Universe** | one Go runtime, one bubble — *physically* cannot be per-host |
| filesystem tree + durability (page cache), hostname, routable IP + **port space** + loopback, `NumCPU`, **clock offset/rate over base**, zone label | **Host** | co-located processes share these; the power-loss tear is a host event |
| pid/ppid, **cwd**, open fds/`*os.File`, goroutines + **memory**, open conns, uid/gid, allocation accounting | **Process** | the crash/restart/schedule unit; memory-isolated even from host-siblings |

**A process is the unit of memory isolation; a host is the unit of FS/network sharing.** Co-located
processes share their host's filesystem and network identity but **never share Go memory** — they
communicate only through files and the network, exactly like real processes with separate address
spaces. This is not merely a convention: it is what *makes* the crash model sound (a crashed process
holds no in-memory resource a sibling waits on), and a SUT that passes a Go channel/pointer between two
`Process` trees is out of model (program discipline — the concurrent dual of the inherited-handle
stance).

### Identity primitives (the shared contract every fault axis targets)

`g` gains **two** ids — `dstHost uint32` and `dstProc uint32` — both **inherited parent→child at
`newproc1`**, alongside `g.dstrand` (`proc.go:5446`). The runtime carries integer ids; the string↔id
interning and the public API live in `testing/simulation`, so no Go string enters the hot `g` copy path
(the same "lean runtime, public face" split as process identity and the scheduling strategy). Host 0 /
process 0 is the default — the test driver — so the N=1 program is host 0, process 0, unchanged.

**API (explicit, declarative, dynamic).** `simulation.Host(name, HostConfig{...}, f)` establishes a host
(its FS, hostname, IP, NumCPU, clock offset, zone); `simulation.Process(name, ..., f)` runs a process. A
`Process` declared inside a `Host` body is on that host; a `Process` outside any `Host` gets an
**implicit dedicated host** (the 1:1 "one process per machine" case — the common distributed topology,
zero-config). Both are callable **at any time**, not only at setup: since there is no `os/exec` under
DST, calling `Host`/`Process` mid-run **is** how a SUT models a node joining (membership change). The id
is stamped+inherited, so a process started mid-run, or added to an existing host, just works; the body
scopes *declaration*, the goroutines it starts outlive it.

```go
simulation.Host("h1", simulation.HostConfig{IP: "10.0.0.1", NumCPU: 4, Clock: simulation.Skew(50*ms)}, func() {
    simulation.Process("p1", p1main)   // shares h1's FS, IP, port space, clock
    simulation.Process("p2", p2main)
})
simulation.Process("n3", n3main)       // implicit dedicated host
```

### Per-host filesystem (process isolation by construction)

Today the FS is one process-global tree + one cwd (`os/dst_fs.go` `dstFS{root, cwd}`), shared by every
goroutine. The model: the **tree** is per-**host**; the **cwd** and the **fd table** are per-**process**.

- **Per-host tree.** `dstFS` becomes `nodes map[hostId]*dstFSDisk` (each `{root, …}`), created lazily
  with the `/tmp` pre-seed on a host's first FS op; a goroutine's FS ops resolve against *its host's* tree
  (via `g.dstHost`). This makes "process A reads/corrupts process B's file" **unrepresentable** across
  hosts (A's resolver never reaches B's host tree) — the Structural-enforcement win: collapse to one
  source of truth (the host's own tree) rather than guard against the bad state. Co-located processes *do*
  share their host tree (lock files, a shared data dir, a shared SQLite file) — the "N processes on one
  host" requirement.
- **Per-process cwd + fds.** `Chdir`/`Getwd` are per-process (p1 changing cwd must not move p2's, even on
  one host); an open `*os.File` belongs to a process (a process crash closes *its* handles while the file
  content survives on the host tree). The cwd is a path *into* the host tree.
- **N=1 collapse.** One host → one tree, one process → one cwd = today's behavior; existing `TestDSTFS*`
  pass unchanged.
- **Inspection.** A process reads its own host's disk via ordinary `os` calls inside its `Process`/`Host`
  body (idiom 1 — what real recovery code does on restart, needs no API). For *harness* assertions across
  hosts (do all replicas agree? did h2 persist the commit?), `simulation.HostFS(name)` returns a
  **read-only** `io/fs.FS` over a host's current tree — read-only by construction so it can never become a
  cross-host back-channel write.

### Per-host network address space

Today the registry is one flat keyed-by-string map (`net/dst.go` `dstNet`). The model makes addressing
per-host:

- **Loopback is host-private** — `127.0.0.1`/`localhost` resolve within the *dialing process's host*, so
  `p1` on `h1` dialing `localhost:80` reaches a listener on `h1`, never `h2`.
- **Port space is per-host** — two processes on `h1` cannot both bind `:80` (`EADDRINUSE`); the same port
  on `h2` is independent. The registry keys by `(hostId, addr)`.
- **Each host has a routable IP** (`HostConfig.IP`, or deterministically assigned like ephemeral ports
  are now), so a process on `h2` dials `h1`'s service by `h1`'s IP:port. Hosts form an **implicit full
  mesh** — every host's IP:port is dialable; "connecting" is ordinary `net.Listen`/`net.Dial`, unmodified
  code. There is **no "virtual switch"**: the network topology is the full mesh *minus active partition
  faults*, plus a base-latency matrix (fault section). A switch object would re-express, with extra
  machinery, exactly what partition/latency faults already express, and adds no physical behavior in a
  deterministic sim.
- **Conn attribution** records both the host and process of each end (dialer = the `Dial` caller's
  `dstHost`/`dstProc`; server = the `Listen` caller's host/process — the process that owns the listener and
  accepts on it — stamped on the conn at Dial), so net faults target a host-pair (partition) or a process
  (reset). The **host** half is **landed** (`dstConn.localHost`/`remoteHost` + `dstListener.host`,
  `net/dst.go`), consumed by the base-latency link lookup; the **process** half lands with the reset/crash
  faults that target a process, at the same Dial stamp.
- **DNS by hostname is deferred.** Dial by assigned IP:port; `hostname → IP` is a planned minimal sim-DNS
  increment (until then DNS-by-name stays fenced, as today) — a thin lookup over the host IP assignment,
  same address model.

### Per-host clock (the seam for skew — a hard requirement, in model)

Time is **no longer purely universe-global**: the Universe owns the *base* virtual clock and the synctest
advance; each **Host** owns a clock *function over base time*. A goroutine's `time.Now()` wall reading is
`wall = base + offset_h(t)`, applying the calling goroutine's host offset (carried on `g.dstClockOffset`,
stamped from the host's config and inherited like `g.dstHost`). This is the primitive an HLC database is
*built to tolerate*, so it cannot be a single global clock.

- **Foundation (substrate): static per-host offset** — **landed**. `HostConfig.Clock` is a `ClockConfig`
  built by `Skew(d)` (a fixed offset) or `BoundedSkew(max)` (an offset drawn deterministically from the run
  seed within ±max, independently per host — the per-seed knob for exploring bounded skew, stable across a
  host restart since it depends only on `(seed, host id)`). The offset is stamped on the Host body's
  goroutine (`g.dstClockOffset`, inherited at `newproc1` like `g.dstHost`, so co-located processes and the
  host's whole subtree share one clock) and added to **only** the wall split in `runtime/time.go`
  `time_runtimeNow`, guarded by `dstActive` (so it folds away in non-dst builds). `bubble.now`, monotonic
  time (`time_runtimeNano`), timer deadlines, and the synctest "advance to next deadline" machinery are
  untouched — an offset shifts what `time.Now()` *reads*, not durations, so relative timers fire at the
  same base time on every host (only the rare absolute-wall-time timer shifts by the offset). Default 0 is
  the N=1 collapse, byte-identical to the universe-global clock.
- **Drift/step are clock faults (later).** *Drift* (`rate ≠ 1`) and *step* (an NTP jump) perturb
  `offset_h(t)` dynamically — **sequencing, not a concession**: the representation is `wall = f_h(base)`
  (offset = `base + c`, drift = `base*rate + c`, the same function slot), so drift fills the slot with no
  redesign. Offset-first unblocks HLC skew testing immediately; drift (which needs a per-host *monotonic*
  clock and rate-aware deadline conversion at the synctest wake) follows. See "Clock faults".

### Per-process identity and memory accounting

- **Identity split** — **landed**. Per-**host**: `os.Hostname` (defaults to the host's declared name,
  `HostConfig.Hostname` overrides), `runtime.NumCPU` (`HostConfig.NumCPU`, else the run default), and
  `net.Interfaces` (a fixed synthetic per-host set — `lo` plus `eth0` bearing the host's routable
  `10.0.0.<id>`, retiring the real-NIC nondeterminism the net section recorded). Per-host identity lives in
  a lock-free copy-on-write `atomic.Pointer` table keyed by `g.dstHost` (the string stays off `g`).
  Per-**process**: `os.Getpid`/`Getppid` (a fresh per-*invocation* pid on `g.dstPid`, so a restart gets a
  new pid — no stable-pid), cwd; `os.Getuid`/`Getgid`/user stay the uniform `7777`/"sim" constants
  (per-process possible later, non-foreclosing). Host 0 / unconfigured uses the run defaults
  (`Options.Hostname`/`PID`/`NumCPU`), so the N=1 program is unchanged.
- **Memory accounting** — **landed**. Per-process **allocation accounting** extends the existing
  per-object hook (`malloc.go`, inside the simulation-bubble gate where `cur` and `elemsize` are already in
  hand) to also attribute `elemsize` to `cur.dstProc` — deterministic, `-race`-invariant, ~free. The
  counters live in a **fixed-size** per-run vector keyed by process id (`dstProcAlloc`), allocated once on
  the first `Process` declaration and never grown — a stable backing array, so the hot path is a single
  atomic add with no table copy to race (a grow-on-demand table would race the copy against concurrent
  declarations); read via `dstProcAllocBytes` (the L3 OOM fault's metric). A run is bounded to
  `dstMaxSimProcs` distinct processes (it panics past that, never silently drops accounting). This is
  *allocation flow*, **not** true RSS/live-set: faithful
  per-process residency would need per-object/per-span process attribution + a mark-time sum and would
  have to resolve the shared-package-globals ambiguity (one Go program has one set of globals) —
  deliberately **not** built. The counter drives the **OOM fault** (fault section); the metric source is a
  seam (counter now; live-set later if a SUT ever needs RSS-accurate thresholds), non-foreclosing. The
  universe-wide `Options.MemoryLimit` keeps its faithful live-set bound.

### Project invariants (distributed model)

- **DST-NODE-ISOLATION (entailed: isolation boundary).** A goroutine's FS ops resolve only against its
  *host's* tree, and processes share no Go memory; a process observes another's state only over the
  simulated network or a shared *host* filesystem. *violation:* process A on host hA reads a path process
  B wrote on host hB and sees B's bytes — a back-channel two separate machines never had, so a SUT passes
  only because the nodes secretly shared a disk (a false negative) — or a crash on B corrupts A's file.
  *Enforced:* structural (per-host tree, resolver keyed by `g.dstHost`; per-`(host, process)` cwd) +
  `TestDSTNodeFSIsolation`/`TestDSTNodeCwdIsolation` (`os/dst_node_fs_test.go`): two hosts writing the same
  path get independent files, per-host `/tmp` is independent, and per-process cwd does not leak — *and* the
  inverse, co-located processes *do* share their host tree. The crash-tear half (a crash on one host
  leaving another intact) enforces with the crash fault.
- **DST-CLOCK-DET (clause-explicit: determinism).** Same seed + same host clock config → identical
  per-host `time.Now()` readings and identical timer firings. *violation:* a host offset or drift
  conversion drawn from a load-dependent source (real time, per-m RNG) varies run-to-run. *Enforced:*
  offsets are deterministic functions of seed/config (`runtime.dstHostSeededClockOffset` hashes the seed
  with the host id, advancing no RNG stream; `Skew` is a constant); `TestDSTClockDeterminism` /
  `TestDSTClockBoundedSeeded` (`testing/simulation/clock_test.go`) probe a skewed multi-host run across two
  same-seed runs and across seeds, mutation-tested.
- **DST-CLOCK-DURATION (entailed: the offset preserves durations).** A static per-host wall offset shifts
  only `time.Now()`'s wall reading, never monotonic time, durations, or timer deadlines — so relative
  timers fire at the same base time on every host regardless of offset. *violation:* folding the offset
  into the shared base clock (`bubble.now`) or the monotonic reading fires a skewed host's 1 s relative
  timer early/late and corrupts `time.Since` across the offset boundary, while a naive "`Now()` differs per
  host" check still passes (the strongest counterexample — every per-host reading looks right yet durations
  are wrong). *Enforced:* `TestDSTClockDurationPreserved` (`testing/simulation/clock_test.go`) asserts that
  under a non-zero offset an in-bubble interval's `time.Since` and the base-clock advance over a host's
  sleep are byte-identical to offset 0, mutation-tested against the `bubble.now`-corruption implementation.
- **DST-IDENTITY-DET (entailed: determinism + consistency).** Every goroutine of a host observes one
  `os.Hostname`/`runtime.NumCPU`; every goroutine of a process observes one `os.Getpid`; distinct
  hosts/processes observe distinct values; same seed + config → identical run-to-run. *violation:*
  `os.Hostname`/`Getpid` returns different values on two goroutines of one host/process (an inheritance
  gap), or a value drawn from a load-dependent source varies run-to-run, so a SUT that derives node ids
  from them diverges across replays. *Enforced:* per-host identity is a deterministic table keyed by
  `g.dstHost`, per-process pid is `g.dstPid` inherited at `newproc1` and allocated from a seed-ordered
  counter; `TestDSTIdentity{PerHostHostname,PerHostNumCPU,PerProcessPid,Determinism}`
  (`testing/simulation/identity_test.go`), mutation-tested.
- **DST-IDENTITY-SOUND (entailed: simulated replaces real).** Under an active run the simulated identity
  fully replaces the real machine's — `os.Hostname`/`Getpid`/`runtime.NumCPU`/`net.Interfaces` never leak
  the developer's box. *violation:* a real hostname/pid/interface leaks into a run → behavior depends on
  the dev machine and is unreproducible elsewhere (a soundness break — an execution the simulated universe
  could not produce identically on another machine). *Enforced:* the accessors gate on the run being active
  (`dstSimEnvSet` / `dstActive`) and return only synthetic values; `TestDSTIdentitySound`
  (`testing/simulation`, for hostname/NumCPU/pid) and `TestDSTNetInterfaces` (`net`, the synthetic
  `lo`+`eth0` set replacing the real NICs).
- **DST-MEMALLOC-DET (entailed: OOM-relevant determinism + attribution).** Each heap allocation accrues to
  the *allocating* goroutine's process (`cur.dstProc`, inherited by its subtree); distinct processes have
  independent counters; and the per-process counter is deterministic *to the granularity the OOM fault
  needs* — the budget-**crossing** decision (does process P exceed budget B?) is a deterministic function
  of the seed. The *exact* byte count is **not** byte-deterministic across runs: it carries sub-observable
  runtime-pool-refill noise — a `sudog` cache refill from a channel op is charged to whichever process
  empties the process-global, cross-run pool — the per-process analogue of the GC's `DST-MEM-1` byte-noise,
  sound for the same reason (it cannot flip a budget-scale crossing; an OOM budget sits far above the noise
  floor of a few KB). *violation:* an allocation attributed to the wrong process (or the root), or counts
  that diverge at the *budget* scale (not within the sub-observable band), so the OOM fault fires
  nondeterministically or on the wrong process. *Enforced:* `elemsize` (size-class size, `-race`-invariant)
  summed per-object at the one mallocgc hook under the bubble gate, counters keyed by `g.dstProc`;
  `TestDSTMem{PerProcessAccounting,ChildAttributed,Independent}` assert attribution, and
  `TestDSTMemDeterminism` asserts the budget crossing replays exactly + the counts stay within the
  sub-observable noise band across two same-seed *concurrent* runs (`testing/simulation`), mutation-tested.
- **DST-NET-FIFO (entailed: in-order delivery / soundness).** On one direction of a connection, bytes
  become readable in write order; the simulated link never reorders a live stream. *violation:* a later
  write is delivered before an earlier one (e.g. a future jitter draw not clamped monotone), so a peer
  reads bytes a reliable in-order transport (TCP) could never produce out of order — a soundness false
  positive while every send is correctly ordered. *Enforced:* the reader always consumes the head (oldest)
  segment first (`pop`, `net/dst_wire.go`), so delivery order equals append (write) order *whatever* the
  per-segment delivery times — a jitter draw varies only *when* a segment is released, and head-of-line
  bunches a later, smaller-jitter segment behind an earlier one rather than letting it overtake. No reorder
  is representable, with no `deliverAt` clamp needed. `TestDSTNetLatencyFIFO` (two time-separated writes)
  and `TestDSTNetJitterFIFO` (a jittered burst) assert in-order delivery (`net`).
- **DST-NET-LATENCY-DET (entailed: determinism).** A connection's delivery virtual-times are a
  deterministic function of the seed and the configured latency, and a same-host/loopback connection
  delivers instantly. *violation:* delivery timing drawn from a load-dependent source (wall clock, per-m
  RNG), or measured in host-skewed wall time instead of universe base time, varies run-to-run or with a
  peer's clock skew — breaking replay (and making a cross-host link's delay depend on which hosts' clocks
  disagree, the exact HLC bug surface this substrate exists to test). *Enforced:* delivery is gated in
  base time (`time.Now` minus the calling goroutine's host clock offset) and waited out with a relative
  fake-clock timer (`net/dst_wire.go`); `TestDSTNetCrossHostLatency` / `TestDSTNetSameHostInstant` /
  `TestDSTNetLatencyDeterminism` pin the one-way/RTT delay, the same-host-instant rule, and same-seed
  reproducibility, and `TestDSTNetLatencyDeadline` confirms the delay is a real fake-timer wait (a read
  deadline shorter than the latency times out before delivery) (`net`), mutation-tested.

(The fault-feature invariants are in the next section.)

## Fault orchestration design

Status: **design settled, implementation pending.** Fault orchestration is the last ⏳ row of the source
table: faults over the now-distributed substrate (the Universe/Host/Process model above), composed under
one seed, replay-exact, with failure shrinking. **Methodology: every axis and seam is designed upfront
(this section + the distributed model above) and implemented bottoms-up — the substrate layers first,
faults last — not as vertical per-axis slices** (see "Build order").

The governing principle is the one every prior feature obeyed: a fault is a **policy at an existing
seam, never new base representation and never a new scheduler choice** — exactly as Random/PCT are
policies at `dstSchedSelect`, net/disk virtualization rides the schedule, and crash tears along the
durability split the FS representation already carries. Soundness is therefore the load-bearing
property, and it is the same top-tier invariant: every fault must correspond to a **real degree of
freedom** at its seam, so the set of faulted executions stays ⊆ the set the real runtime+OS produce.

### Spec-first gate

- canonical: this doc's top-tier **Soundness** / **Non-foreclosure** invariants + the control-surface
  table; the "In-memory deterministic network" (the TCP base), "...filesystem" (the durability split),
  and "...pipes" sections; the Seq-5 seam (`dstSchedSelect`) and its "scheduling faults fold in"
  passage; the `testing/simulation` `Options`/`RunWith`/`Explore` surface.
- contract (top-tier): *the controllable surface is "which runnable goroutine proceeds next" + "what
  wakes at quiescence," hooked only where the runtime already makes a nondeterministic choice;
  executions ⊆ real; faults are policies at those seams, each anchored to a real degree of freedom; the
  net/disk/crash/scheduling faults share one victim-designation contract.*
- mechanism: the **Host/Process** victim contract (the distributed model above), shared by all axes;
  faults as seeded/declarative policies read at the per-host net registry+conn, the per-host FS
  tree+durability, the per-host clock function, the per-process allocation counter, the per-process
  goroutine set, and the `dstSchedSelect` seam; replay rides the seed (a dedicated fault RNG); shrinking
  extends the `Explore`/`Failure` machinery.
- collapse-check: faithful. **Not finer** — every fault is a real network/disk/scheduler event (latency
  = a fake timer; partition/reset = real TCP flow events; EIO/ENOSPC/crash-tear = real disk events;
  straggler = reordering only already-runnable Gs); none adds a choice the real stack lacks. **Not
  coarser** — each axis hooks the seam that already mediates its whole surface. **Not foreclosing** — the
  one Host/Process contract hosts every axis (net, disk, clock, scheduling, OOM, crash) *and* the UDP
  packet-granular follow-on (drop/reorder/duplicate) with no different shape; the
  fault-as-`dstSchedSelect`-policy slot was reserved by Seq 5. Single-tier:
  GOMAXPROCS=1, the soundness collapse unchanged from Seq 5 ("reorder only the already-runnable set").

### Targeting (the convenience fault API over the mesh)

A fault names its victim at the layer that owns the faulted resource — a **host**, an **(un)ordered
host-pair**, a **zone-pair**, a **process**, or (network sugar) an **address / conn-predicate** resolving
to the owning host/process (a listen address is owned by the host that registered it). The convenience
API compiles to per-host-pair / per-process fault records over the full mesh — **not** topology objects:

- `Isolate("h3")` / `Heal("h3")` — cut / restore a host from all others.
- `Partition({"h1","h2"}, {"h3"})`, `Partition(Zone("dc-a"), Zone("dc-b"))` — split groups (the
  zone-pair form needs only the host `zone` label, no topology object).
- `Link("h1","h2").Latency(50*ms)`, `.Blackhole()`, `.Reset()` — per-link policy.
- `Crash("p2")` / `CrashHost("h1")` / `OOM("p2", budget)` / `Straggle("p2")` / `Skew("h1", drift)`.

Attribution is what makes targeting sound and leak-free: a conn records both endpoints' host+process, a
file its host, a goroutine its host+process — so a host-pair partition touches exactly that pair's
cross-host conns, and a process crash exactly that process's resources (DST-FAULT-VICTIM).

### The fault model: policies at existing seams

A fault is a record `{kind, victim, activation}` consulted at the seam its kind owns. It adds **no new
base representation** (the disk durability split, the net registry+conn, the per-g host/process ids, and
the runnable set already carry everything) and **no new scheduler choice** (the seam still selects only
among runnable Gs). A fault's *machinery* — a latency conn's fake-timer delivery queue, a straggler's
priority floor — is the policy's own state, present only when that policy is active, exactly as
`g.dstPrio` is PCT-only; that is the fault's mechanism, not a retrofit of the base representation.

**The fault RNG.** Seeded fault decisions (which eligible victim, when within a window, a jitter draw)
come from a dedicated per-bubble **fault RNG** (`dstFaultRand`, splitmix64), re-rooted per bubble and
salted independently of the per-g tree and the scheduling RNG — so faults are a deterministic function
of the seed (replay-exact for free, like everything else) and the fault policy's draw count never
shifts the scheduling RNG's stream (the same stream-isolation discipline as system-goroutine
scheduling: each policy's determinism is independent of the other's draw count). A fault *changing* the
execution — a reset waking a blocked reader, a latency adding a timer — is intended: it is a different,
still-deterministic execution; only the fault *choices* are stream-isolated.

### The control surface: declarative faults + seeded exploration

Two surfaces, one format (the second produces the first):

- **Declarative** (`Options.Faults []Fault`): the SUT hands `RunWith`/`TestWith` an explicit fault set —
  kind + victim + activation (immediately; after a virtual duration; between two virtual times; on the
  Nth matching event). Deterministic and replay-trivial (static config). For targeted scenarios —
  "partition `n1|n2` for 2s, crash `n3` at t=2s."
- **Seeded exploration** (`Options.FaultPolicy` + `Explore`/`ExploreWith`): a *policy* declares the
  enabled kinds, the eligible victims, and a **fault budget** (max concurrent / total injections); the
  explorer draws specific injections from the fault RNG and sweeps seeds to search the fault space. This
  is the load-bearing form for "compose under one seed, with replay and failure shrinking."

Faults are the **third replayable dimension** of the existing `Explore`/`Replay`/`Failure` machinery,
beside the schedule prefix and the access forces: `Failure` gains a `Faults []Fault` field recording the
injections that reproduced a failure; `Replay(seed, failure, sut)` re-applies all three (the recorded
faults replay *as a declarative set* — so an explored failure's minimal repro **is** a hand-runnable
declarative configuration). **Shrinking** minimizes the `Faults` set (and the schedule) subject to
"still fails" — the same delta-debugging shape the DPOR backtrack already embodies; a budget that bounds
coverage is reported, never silently capped (`BudgetHit`, per No silent downscoping).

### Network faults (the first axis)

Seam: the `dstNet` registry + `dstConn` (`src/net/dst.go`), reached from the runtime fault table via
`//go:linkname` exactly as `dstNetEpoch` is, and gated on `dstActive()` so it compiles out untagged.
Each fault is checked on the conn's `Read`/`Write` and on `Dial`/`Accept`; all are **sound on the
reliable, in-order TCP base** — i.e. **flow/connection-granular**, never byte/message-granular:

- **Latency / jitter** — delay delivery by a virtual duration, FIFO preserved (in-order, as TCP). A
  latency policy interposes a deterministic, **fake-clock-driven delivery queue** on the conn
  (`net/dst_wire.go`): written bytes become readable after the delay. The **base latency** — every
  cross-host link's always-on delay even with no fault, load-bearing for HLC where delayed delivery *while
  clocks differ* is the bug surface — is **landed** as `Options.Network.CrossHostLatency`: a single
  base-time scalar applied to every distinct-host connection (same-host/loopback instant; default 0, so the
  N=1 collapse and any run that sets no latency are byte-identical), gated in universe base time so per-host
  clock skew never changes the wire delay (DST-NET-FIFO, DST-NET-LATENCY-DET). **Jitter** — the first
  network *fault* — is **landed** as `Options.Network.CrossHostJitter`: each cross-host segment is delayed
  by the base latency plus a value drawn from `[0, jitter)` by the dedicated, seeded, stream-isolated fault
  RNG (`dstFaultRand`), so it replays exactly and never perturbs the schedule (DST-FAULT-REPLAY). It only
  *delays*; FIFO is preserved with no clamp because the reader is head-of-line (a smaller later draw bunches
  behind an earlier segment, never overtakes it). The per-host-pair **matrix** (asymmetric per-link latency
  / jitter) is the L4 targeting API (`Link("h1","h2").Latency()`). DoF: a real link has variable latency.
  Sound — it is a fake timer, the contract `time` already virtualizes.
- **Partition** — between a host-pair (symmetric or one-directional) or isolating a host, over a virtual
  window. *On connect:* a Dial across the partition either **refuses** (`ECONNREFUSED`, peer-down
  semantics) or **blackholes** (the Dial blocks until its context/deadline — packets-dropped semantics);
  the mode is **selectable per fault** (the `Fault` record carries refuse | blackhole) — both are real TCP
  outcomes and a SUT tests against each, so the choice is the SUT's, not hardcoded. *On an established conn:* bytes across the partition are blackholed —
  reads block durably on the fake clock, writes fill a send buffer that never drains — until the
  partition **heals** (in-order delivery resumes; TCP buffers and recovers) or a deadline/`Close` errors
  the conn. DoF: a transient partition. **"Drop" lives here**, at flow granularity — a partition window
  drops everything between A↔B; there is *no* single-byte drop on a live stream (TCP forbids it — that is
  the UDP follow-on).
- **Connection reset** — inject `ECONNRESET` on a targeted conn, or a process's / a host-pair's conns, reusing
  the conn's existing `resetConn()` (already wired for backlog teardown: peer reads & writes then carry
  `ECONNRESET`). DoF: a real RST (peer crash, middlebox).
- **Throttle / bandwidth** — pace delivery so ≤ B bytes cross per virtual time unit (latency
  proportional to bytes, the same fake-timer queue as latency). DoF: finite link bandwidth.

Collapse-check (net axis): **not finer** — each fault is a real TCP-flow event; crucially **no
drop/reorder/duplicate of bytes on a live stream** (those are not real degrees of freedom of a reliable
in-order transport — injecting them would make executions ⊄ real, the false positive the Soundness
invariant forbids). **Not coarser** — hooks the registry+conn seam that already mediates every simulated
`Dial`/`Listen`/`Read`/`Write`. **Not foreclosing** — packet-granular drop/reorder/duplicate are degrees
of freedom of a **datagram** transport and land with the **UDP/`PacketConn` follow-on** (already a named
net increment), keyed by the *same* node victim contract and fault table; building the TCP-flow faults
now reserves nothing they need.

### Disk faults

Seam: the per-bubble FS tree + the **durability representation** (durable image + pending state) the
disk feature built and froze monotonicity on precisely so crash could tear along it. Faults:

- **EIO** — a targeted file / a host's disk fails `read`/`write`/`Sync` with `EIO`. DoF: real disks
  return EIO. (Sound only where the real call can fail — read/write/sync can; this never makes a truly
  infallible call fallible, the Soundness boundary.)
- **ENOSPC** — writes/creates on a host's disk fail `ENOSPC` past a budget. DoF: a full disk.
- **Latency** — delay an FS op by a virtual duration (fake timer). DoF: a slow disk.
- **Crash (the durability tear)** is the **host (power-loss) crash** — see "Crash / restart faults". It
  restores a host's disk to **exactly its durable image** (synced survives byte-exact; unsynced data/
  entries MAY be lost, unsynced content MAY be torn at arbitrary byte granularity, drawn from the fault
  RNG — no atomicity beyond `Sync`). Verbatim the durability contract already settled and
  **monotonicity-enforced** (`TestDSTFSDurabilityMonotonicity`); the representation needs no change, only
  a crash policy reading the durable image. A *process* crash does **not** tear the host FS (the kernel
  survives) — that split is the crash section. The reason the durability split exists.

Collapse-check (disk axis): **not finer** — each is a real disk outcome; the crash tear is bounded by
the synced/unsynced split the representation already tracks. **Not coarser** — hooks the single FS commit
point. **Not foreclosing** — `Sync`/durability were designed for this; the fault adds policy, not
representation.

### Clock faults

Over the per-host clock seam (the distributed model's "Per-host clock"):

- **Drift** — a host's clock rate departs from 1 over a window (runs fast/slow), drawn from the fault RNG
  or declared. Needs the per-host *monotonic* clock and rate-aware deadline conversion at the synctest
  wake (the harder increment — offset-only ships in L2, drift fills the same `wall = f_h(base)` slot).
- **Step** — a sudden jump (NTP slew/correction), forward or **backward** in wall time. DoF: real clocks
  get stepped; a backward step is exactly the HLC adversary.

Collapse-check (clock axis): **not finer** — virtual time, the contract `time` already owns; a host's
clock is a deterministic function of base time, so every reading is replay-exact. **Not coarser** — hooks
the single `time.Now()`/timer path. **Not foreclosing** — offset (substrate) and drift/step (fault) share
the one clock-function representation.

### Crash / restart faults (the cross-axis fault)

Two distinct faults, because on a real machine the filesystem **page cache belongs to the kernel, not the
process** — so a process dying and the host losing power tear the disk differently:

| Fault | Kills | Host FS | Conns | Models |
|---|---|---|---|---|
| **Process crash** (`Crash("p")`) | that process's goroutines + memory + fds | **intact** — kernel survives, so un-fsync'd writes persist for host-siblings and the restart | its conns RST | a process dying / `kill -9` / OOM |
| **Host crash** (`CrashHost("h")`, power loss) | **all** processes on the host | **tears to the fsync'd durable image** (the disk "Crash" above) | all their conns RST | power loss / kernel panic |

Both, at the victim's next cooperative point: the targeted goroutines (`dstProc`/`dstHost == victim`) are
**descheduled permanently** (the `dstSchedSelect` seam never selects them again; their in-flight blocking
ops are abandoned — a crash does not, cannot in Go, force-unwind a goroutine mid-instruction; the sound
model is *they never run again*, what a killed process's threads do), conns RST, fds drop, memory is gone.
The host crash additionally tears the host disk; the process crash leaves it intact.

**OOM** is a **process crash** whose *trigger* is the per-process allocation counter crossing a budget
(cgroup-style per-process budget by default; a host-total kernel-OOM with victim selection is a recordable
variant). The budget must sit above the counter's **noise floor** (a few KB — the counter carries
sub-observable runtime-pool-refill noise, DST-MEMALLOC-DET); a realistic OOM budget (KB–MB+) does, so the
crossing is replay-exact. Same effect, *organic* trigger — the node dies where its own workload takes it, so it tests "how
the cluster handles a node hitting its memory limit" at a workload-determined point. **Hard-kill only**:
soft `GOMEMLIMIT`-style per-node degradation (GC thrash/backpressure *approaching* the limit) is **out** —
it needs the per-process live-set deliberately not built (a consequence of the allocation-accounting
choice, not a separate loss); the universe-wide `Options.MemoryLimit` still gives the soft GC behavior for
the whole sim.

**Restart** re-runs the victim's entry on a fresh goroutine subtree, over the live (process restart) or
torn (host reboot) FS and a clean network — the canonical recovery fault, driven by a SUT supervisor or a
fault policy (`Crash("p", RestartAfter: 3*s)`). A restarted process gets a **new pid** (a real restart
always does; there is **no stable-pid option** — the stable identity is the logical name `"p"`, the
footgun-free contract); `ppid` is `1`/reparented unless a supervision tree is modeled.

**Soundness boundary (recorded, contractual).** A crash is sound exactly when processes interact *only
through the simulated network and host filesystem* — the distributed model the whole feature targets. If a
SUT shares an in-process `sync.Mutex` (or channel, or Go memory) *across* `Process` trees, a crash leaves
it abandoned and another process may block on it forever — but that coupling is not two processes; it is
out of model (program discipline, the concurrent dual of the inherited-handle stance, and the very reason
a process is the memory-isolation unit). Within the model a crashed process holds no in-memory resource a
sibling waits on, so abandonment is sound. (When file locking lands as its net/fs follow-on, a process
crash must also release that process's `flock`s — the kernel does on process death — which the per-process
fd table makes clean.)

**The load-bearing mechanism (the crash chunk's work, contained, non-foreclosing).** synctest tears down a
*whole* bubble; **per-victim teardown** — descheduling one process's (or host's) goids, abandoning their
durable blocks without tripping the bubble's deadlock detector (crashed goroutines are *permanently
parked, not deadlocked*), and resetting their resources — is the crash axis's hard part. It is scoped to
the crash chunk and depends on nothing the network/disk/clock axes defer; those axes do not need it.

### Scheduling faults (Seq 5's deferred 5c, folded in here)

The straggler form Seq 5 deferred for want of this victim contract:

- **Straggler** — pin a process's (or a whole host's) goroutines low: a `dstPrio` floor applied to every
  `g` with `dstProc == victim` (or `dstHost == victim`), consulted at the existing `dstSchedSelect` seam
  (reusing the PCT priority machinery). "What if node 2 is slow?" DoF: a real OS can starve a process's
  threads. Sound by the seam's existing argument — it reorders only already-runnable Gs, never pulls from
  a wait queue.
- **Jitter** — a per-decision seeded probability to defer the chosen G. Recorded as Seq 5 judged it:
  marginal (it overlaps Random and dilutes PCT's directed search), so low priority; the seam already
  supports it as one more `dstSchedSelect` policy.

Collapse-check (scheduling axis): identical to Seq 5's — the seam is unchanged; a fault is one more
policy at `dstSchedSelect` fed through `RunWith`, now able to name its victim by node.

### Project invariants (fault orchestration) — recorded spec-tier

Recorded only (design stage for these axes); each promotes to enforcement when the axis that can violate
it is built (Issue-triage chunk-start gate). For the `kind=entailed` invariants `from=` is audited via
`violation=`, not reviewer-checked directly.

- **DST-FAULT-SOUND (entailed: soundness).** Every injected fault corresponds to a real degree of freedom
  at its seam. *violation:* a failure reachable only because the harness injected a fault the real stack
  never had — a byte dropped/reordered on a live TCP stream, `EIO` from an infallible call, a goroutine
  killed mid-critical-section another *process's in-process* code depends on, a *process* crash that tears
  the host FS (the kernel would survive it), a clock that runs backward with no NTP step, a timer fired
  before its deadline — a false positive while every documented ordering/durability guarantee still holds.
  *Enforced (jitter class landed; each further fault class as it lands):* per-fault structural argument +
  a regression test per fault class that the faulted execution is one the real stack can produce. Jitter is
  a real link degree of freedom (variable latency) that only *delays* — bounded to [0, max), never dropping
  or reordering a live stream (delivery is head-of-line, in order, DST-NET-FIFO): `TestDSTNetJitterBounded`
  / `TestDSTNetJitterFIFO` (`net`), mutation-tested. (A partition will likewise only ever yield
  refuse/blackhole/heal, never a missing byte on a healed stream.)
- **DST-FAULT-REPLAY (clause-explicit: determinism).** Same seed + same fault configuration (declarative
  set or policy) → identical execution, including which faults fired when. *violation:* a fault decision
  drawn from a load-dependent source (wall clock, per-m RNG) varies run-to-run, breaking replay.
  *Enforced:* the dedicated per-bubble `dstFaultRand` (splitmix64), rooted at activation and re-rooted per
  bubble, **stream-isolated** from the scheduling RNG by a distinct salt so a fault's draw count never
  shifts the interleaving; `TestDSTFaultRandStreamIsolation` (`runtime`: fault draws leave the scheduling
  RNG untouched, and the two roots differ for the same seed) and `TestDSTNetJitterDeterminism` (`net`: same
  seed → identical jittered delivery, varying with the seed), mutation-tested. (Extends to each further
  fault class's draws as it lands.)
- **DST-FAULT-VICTIM (entailed: attribution integrity).** Every faultable resource is attributed to its
  owning layer — a goroutine/conn/fd to a **process**, a file/tree/port to a **host** — and a fault on
  host hX (or pair {hX,hY}) / process pX affects exactly that victim's resources, no leak onto a
  non-victim. *violation:* a `Partition(h1,h2)` resets a conn between h1 and h3 because attribution
  confused ownership → a failure on h3 the real partition never caused, while the partition itself is
  correct. *Encoding when built:* `dstHost`/`dstProc` on `g` inherited at `newproc1`; a conn records both
  endpoints' host+process; a file records its host; a test that a fault on one victim leaves all
  non-victims' resources untouched. *Lands: network axis (conn attribution), then per later axis.*
- **DST-FAULT-NONFORECLOSE (entailed: non-foreclosure).** The Host/Process victim contract + the
  fault-as-seam-policy shape host every axis (net, disk, clock, scheduling, OOM, crash) and the UDP
  packet-granular follow-on with no different shape. *violation:* an axis (disk/clock/crash/scheduling) or
  UDP faults need a targeting scheme the Host/Process contract cannot express, forcing a throwaway
  retrofit. *Encoding:* this full-contract design (all axes collapse from one contract); validated as each
  axis lands — a later axis needing a new targeting shape is the demonstrated violation. *Lands: as each
  axis is built.*

### Build order (bottoms-up — the substrate first, faults last)

Design is complete and upfront (this section + the distributed model); implementation proceeds
**bottoms-up by layer**, not as vertical per-axis slices. Each layer is one or more chunks running the
adversarial loop.

- **L0 — landed substrate.** Scheduler determinism + per-g RNG + universe fake clock + GC + the
  single-host I/O features (net, disk, pipes). Done.
- **L1 — Host/Process identity.** `g.dstHost` + `g.dstProc` (inherited at `newproc1`), the
  `simulation.Host`/`Process` API and `HostConfig`, the string↔id interning. The shared contract every
  later layer keys on; the N=1 collapse leaves L0 behavior unchanged.
- **L2 — re-key the substrate per layer.** Per-**host** FS tree (+ per-process cwd/fds, the `HostFS`
  inspector), per-**host** net address space (loopback / port-space / IP / full mesh + base-latency
  matrix), per-**host** clock offset, per-**host** hostname / `NumCPU` / interfaces, per-**process** pid /
  uid / cwd, per-**process** allocation accounting. Establishes DST-NODE-ISOLATION and DST-CLOCK-DET. No
  faults yet — the substrate is now correctly *distributed*.
- **L3 — faults over the complete substrate.** Network (partition / latency / reset / throttle), disk
  (EIO / ENOSPC / latency), clock (drift / step), scheduling (straggler), OOM (allocation-triggered
  process crash), process crash + host crash, restart. Establishes DST-FAULT-SOUND / -REPLAY / -VICTIM
  enforcement.
- **L4 — orchestration.** The declarative `Options.Faults` + the convenience targeting API; seeded
  exploration (`Options.FaultPolicy`) as the `Explore`/`Failure` fault dimension; `Replay` of a fault set;
  failure shrinking.

The UDP/`PacketConn` net increment (which unlocks packet-granular drop/reorder/duplicate) stays a
net-feature follow-on, not a fault chunk; when it lands, its packet faults reuse this contract.

