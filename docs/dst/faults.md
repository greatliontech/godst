# DST distributed model & fault orchestration

> Governs the distributed model (Universe / Host / Process) and the fault axes built on it, under the
> top-tier contract in [design.md](./design.md) (Soundness / Non-foreclosure invariants, control
> surface, scope, roadmap). The substrate and every fault axis below are **landed** except OOM kill
> and scheduling (straggler) — the remaining ⏳ row of the source table. Code conforms to this
> contract.

## The distributed model: Universe / Host / Process

Status: **landed** (the substrate the fault axes are built on; the "Build order" section records the
bottom-up method it was built by). The fault feature targets *distributed* programs, and to fault one soundly the
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
`Process` trees is out of model (program discipline — the concurrent dual of the explicit
host-capability stance).

### Identity primitives (the shared contract every fault axis targets)

`g` gains **two** ids — `dstHost uint32` and `dstProc uint32` — plus immutable entered-`Host` ancestry,
all **inherited parent→child at `newproc1`**, alongside `g.dstrand` (`proc.go`). The runtime carries integer ids; the string↔id
interning and the public API live in `testing/simulation`, so no Go string enters the hot `g` copy path
(the same "lean runtime, public face" split as process identity and the scheduling strategy). Host 0 /
process 0 is the default — the test driver — so the N=1 program is host 0, process 0, unchanged.

**API (explicit, declarative, dynamic).** `simulation.Host(name, HostConfig{...}, f)` establishes a host
(its FS, hostname, deterministically assigned IP, NumCPU, clock offset, zone); `simulation.Process(name, ..., f)` runs a process. A
`Process` declared inside a `Host` body is on that host; a `Process` outside any `Host` gets an
**implicit dedicated host** (the 1:1 "one process per machine" case — the common distributed topology,
zero-config). Both are callable **at any time**, not only at setup: since there is no `os/exec` under
DST, calling `Host`/`Process` mid-run **is** how a SUT models a node joining (membership change). The id
is stamped+inherited, so a process started mid-run, or added to an existing host, just works; the body
scopes *declaration*, the goroutines it starts outlive it. Mid-run declarations come from goroutines
the simulation schedules — a foreign caller panics, like the fault APIs (see "Fault callers fail
loud too"). An explicit or implicit host declaration validates its candidate ID before publishing
the name or identity. `Host` also validates its complete clock configuration before publishing the
caller stamp, timer remap, or reboot. Every rejected declaration is state-neutral. A run is bounded
to 4096 distinct hosts (`dstMaxSimHosts`, the same fixed-table stance as the `dstMaxSimProcs`
process bound below): exceeding it panics loudly and state-neutrally, never silently drops a
declaration (`TestDSTHostTableExhaustionIsStateNeutral`).

```go
simulation.Host("h1", simulation.HostConfig{NumCPU: 4, Clock: simulation.Skew(50 * ms)}, func() {
    simulation.Process("p1", p1main)   // shares h1's FS, IP, port space, clock
    simulation.Process("p2", p2main)
})
simulation.Process("n3", n3main)       // implicit dedicated host
h1IP := simulation.HostIP("h1")        // the deterministically assigned routable IP
```

(The example is compile-checked by `TestDSTHostConfigDocExample` in
`testing/simulation`, so it cannot drift from the landed API.)

### Per-host filesystem (process isolation by construction)

The **tree** is per-**host**; the **cwd** and the **fd table** are per-**process**
(`os/dst_fs.go` — landed; the bullets state the model the code implements).

- **Per-host tree.** `dstFS` holds per-host disks (`nodes map[hostId]*dstFSDisk`), created lazily
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
  cross-host back-channel write, and side-effect-free on simulation state: inspecting an untouched host
  builds a throwaway baseline tree but restores the shared inode counter afterward, so the phantom draw
  never shifts a later file's `st_ino` (`TestDSTHostFSInspectionAllocatesNoInodes`).

### Per-host network address space

Addressing is per-host (`net/dst.go` — the registry keys are host-scoped; landed):

- **Loopback is host-private** — `127.0.0.1`/`localhost` resolve within the *dialing process's host*, so
  `p1` on `h1` dialing `localhost:80` reaches a listener on `h1`, never `h2`.
- **Port space is per-host** — two processes on `h1` cannot both bind `:80` (`EADDRINUSE`); the same port
  on `h2` is independent. The registry keys by `(hostId, addr)`.
- **Each host has a routable IP** — deterministically assigned (`10.0.0.<id>`, like ephemeral ports)
  and queried by `simulation.HostIP(name)`; there is no per-host IP configuration knob (the
  assignment-only contract: a configured IP would have to validate against the deterministic scheme
  it duplicates), so a process on `h2` dials `h1`'s service by `simulation.HostIP("h1")`+port. Hosts form an **implicit full
  mesh** — every host's IP:port is dialable; "connecting" is ordinary `net.Listen`/`net.Dial`, unmodified
  code. There is **no "virtual switch"**: the network topology is the full mesh *minus active partition
  faults*, plus a base-latency matrix (fault section). A switch object would re-express, with extra
  machinery, exactly what partition/latency faults already express, and adds no physical behavior in a
  deterministic sim.
- **Conn attribution** records both the host and process of each end (dialer = the `Dial` caller's
  `dstHost`/`dstProc`; server = the `Listen` caller's host/process — the process that owns the listener and
  accepts on it — stamped on the conn at Dial), so net faults target a host-pair (partition) or a process
  (reset). The **host** half is **landed** (`dstConn.localHost`/`remoteHost` + `dstListener.host`,
  `net/dst.go`), consumed by the base-latency link lookup; the **process** half is likewise **landed**
  (`dstConn.localProc`/`remoteProc`, stamped at the same Dial point), consumed by the reset fault's
  `ResetProcess` targeting.
- **DNS by hostname is deferred.** Dial by assigned IP:port; `hostname → IP` is a planned minimal sim-DNS
  increment (until then DNS-by-name stays fenced, as today) — a thin lookup over the host IP assignment,
  same address model.

### Per-host clock (the seam for skew — a hard requirement, in model)

Time is **no longer purely universe-global**: the Universe owns the *base* virtual clock and the synctest
advance; each **Host** owns a clock *function over base time*. A goroutine's `time.Now()` wall reading is
`wall = base + offset_h(t)`, applying the calling goroutine's host offset (looked up by `g.dstHost` in a
mutable per-host table, `runtime.dstHostClock`). This is the primitive an HLC database is *built to
tolerate*, so it cannot be a single global clock.

- **Foundation (substrate): static per-host offset** — **landed**. `HostConfig.Clock` is a `ClockConfig`
  built by `Skew(d)` (a fixed offset) or `BoundedSkew(max)` (an offset drawn deterministically from the run
  seed within ±max, independently per host — the per-seed knob for exploring bounded skew, stable across a
  host restart since it depends only on `(seed, host id)`). The offset lives in a **mutable per-host table**
  (`dstHostClock`, a fixed vector keyed by host id, mirroring `dstProcAlloc`): the Host body's goroutine
  writes its configured offset, and every goroutine of the host reads it by `g.dstHost` (inherited at
  `newproc1` like the identity, so co-located processes and the host's whole subtree share one clock). It is
  added to **only** the wall split in `runtime/time.go` `time_runtimeNow`, guarded by `dstActive` (so it
  folds away in non-dst builds). `bubble.now`, monotonic time (`time_runtimeNano`), timer deadlines, and the
  synctest "advance to next deadline" machinery are untouched — an offset shifts what `time.Now()` *reads*,
  not durations, so relative timers fire at the same base time on every host (only the rare absolute-wall-time
  timer shifts by the offset). Raw Linux `clock_gettime(CLOCK_MONOTONIC)` reads the same virtual base clock;
  `CLOCK_BOOTTIME` reads that base too until suspend is modeled, so neither leaks the host clock nor moves
  under wall-clock skew/step. Storing the offset **per host** (not per goroutine) is what lets a *step* move
  a whole host's subtree at once mid-run — a per-g snapshot, fixed at goroutine creation, could not. A nil
  table / unconfigured host reads 0: the N=1 collapse, byte-identical to the universe-global clock.
- **Step and drift are landed.** *Step* (an NTP jump) and *drift* (`rate ≠ 1`) perturb `offset_h(t)`
  over the same representation `wall = f_h(base)`. The implemented per-host clock is the **anchored** form
  `wall = base + offset + (base − t0)·ppb/1e9` — a skew/step `offset` plus a rate departure of `ppb`
  parts-per-billion accumulated from an anchor `t0` (the base time the rate took effect). It is equivalent
  to `base·rate + c` but has no discontinuity when the rate is applied, and composes additively with the
  offset (rate 1 / `ppb` 0 is exactly the offset case). **Step — landed**: `simulation.StepClock(host, delta)`
  adds (forward or backward) to `offset` mid-run (`runtime.dstStepHostClock`), shifting only the wall reading.
  **Drift — landed**: `simulation.Drift(ppb)` sets a host's rate at declaration, and
  `simulation.DriftClock(host, ppb)` changes it mid-run; the wall reading drifts, and a relative timer's
  host-duration `d` is converted to base `d/rate` at the single timer arm choke (`runtime.dstTimerArmForDrift`
  at `(*timer).modify`), so a rate-r host's `d`-timer fires after `d/r` of base. A mid-run change re-anchors
  the wall (continuous) and re-maps every armed timer of the host. Seeded (per-run, seed-drawn) drift is
  `simulation.BoundedDrift(maxPPB)` — a per-host rate in `[-maxPPB, +maxPPB]` ppb from a stateless hash of
  (seed, host id), advancing no RNG stream (`TestDSTClockBoundedDriftSeeded`). See "Clock faults".

### Per-process identity and memory accounting

- **Identity split** — **landed**. Per-**host**: `os.Hostname` (defaults to the host's declared name,
  `HostConfig.Hostname` overrides), `runtime.NumCPU` (`HostConfig.NumCPU`, else the run default), and
  `net.Interfaces` (a fixed synthetic per-host set — `lo` plus `eth0` bearing the host's routable
  `10.0.0.<id>`, retiring the real-NIC nondeterminism the net section recorded). Per-host identity lives in
  a lock-free copy-on-write `atomic.Pointer` table keyed by `g.dstHost` (the string stays off `g`).
  Per-**process**: `os.Getpid`/`Getppid` (a fresh per-*invocation* pid on `g.dstPid`, so a restart gets a
  new pid — no stable-pid), cwd; `os.Getuid`/`Getgid`/user stay the uniform `7777`/"sim" constants
  (per-process possible later, non-foreclosing). `Kill(pid, 0)` is the simulated liveness query over those
  pids: live process pids succeed, completed or unknown pids return `ESRCH`, and host pids are never probed.
  Every positive `Options.NumCPU` and `HostConfig.NumCPU` in the target architecture's `int` range is
  reported exactly; non-positive values select the applicable default.
  Procfs identity follows the same pid registry: `/proc/<pid>/stat` exposes a deterministic field-22
  starttime only for live simulated pids, and `/proc/self/ns/pid` readlink exposes a stable deterministic
  namespace identity with no host `/proc` passthrough. Exhausting the finite PID field fails before a
  `Process` declaration interns names, stamps identity, registers liveness, or runs its body.
  Host 0 / unconfigured uses the run defaults (`Options.Hostname`/`PID`/`NumCPU`), so the N=1 program is
  unchanged.
- **Memory accounting** — **landed**. Per-process **allocation accounting** extends the existing
  per-object hook (`malloc.go`, inside the simulation-bubble gate where `cur` and `elemsize` are already in
  hand) to also attribute `elemsize` to `cur.dstProc` — deterministic, `-race`-invariant, ~free. Like the
  heap trigger (gc.md M4), it **excludes** the runtime-internal pooled structs `g`/`sudog`/`_defer`
  (`dstIsInternalPooledType`): whether a `go`/channel op allocates one or reuses a pooled cache entry is a
  cross-run pooling artifact, not the process's own heap growth, and counting it would make the OOM fault
  fire at a pool-history-dependent allocation — the same determinism rationale, and consistent with this
  metric already being *allocation flow, not RSS* (stacks, the bulk of a goroutine, are likewise uncounted).
  The
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
  *host's* tree, processes share no Go memory, **and no other observable channel exists between
  processes**: a process observes another's state only over the simulated network or a shared *host*
  filesystem. The environment surface is inside this invariant — env is per-**process** state on a real
  machine, so `os.Setenv` in one simulated process must never be observable from another
  (design.md, "Environment surface"; the env leg is enforced by the per-process COW env view —
  design.md's environment row, ✅). *violation:* process A on host hA reads a path process
  B wrote on host hB and sees B's bytes — a back-channel two separate machines never had, so a SUT passes
  only because the nodes secretly shared a disk (a false negative) — or B `Setenv`s a "leader" flag A
  `Getenv`s (the same back-channel through the process env) — or a crash on B corrupts A's file.
  *Enforced:* structural (per-host tree, resolver keyed by `g.dstHost`; per-`(host, process)` cwd) +
  `TestDSTNodeFSIsolation`/`TestDSTNodeCwdIsolation` (`os/dst_node_fs_test.go`): two hosts writing the same
  path get independent files, per-host `/tmp` is independent, and per-process cwd does not leak — *and* the
  inverse, co-located processes *do* share their host tree. The crash-tear half (a crash on one host
  leaving another intact) enforces with the crash fault.
- **DST-CLOCK-DET (clause-explicit: determinism).** Same seed + same host clock config (and same
  `StepClock` schedule) → identical per-host `time.Now()` readings and identical timer firings. *violation:*
  a host offset, step, or drift conversion drawn from a load-dependent source (real time, per-m RNG) varies
  run-to-run. *Enforced:* offsets are deterministic functions of seed/config (`runtime.dstHostSeededClockOffset`
  hashes the seed with the host id, advancing no RNG stream; `Skew` is a constant); a step takes an explicit
  delta at a schedule-deterministic point; `TestDSTClockDeterminism` / `TestDSTClockBoundedSeeded` probe a
  skewed multi-host run across two same-seed runs and across seeds, and `TestDSTClockStepDeterminism` extends
  it to a step schedule (`testing/simulation/clock_test.go`), mutation-tested.
- **DST-CLOCK-DURATION (entailed: an *offset* perturbs reads, not timer deadlines).** A per-host wall offset —
  static (skew) or stepped (`StepClock`) — shifts only `time.Now()`'s wall reading, never the base clock
  (`bubble.now`), the monotonic source, or timer deadlines, so relative timers (`time.After`, context
  deadlines) fire at the same base time on every host regardless of skew or step. (Drift is the deliberate
  exception: a *rate* ≠ 1 does convert timer deadlines — DST-CLOCK-DRIFT-DURATION below. This invariant scopes
  to the offset legs.) (A wall-derived *duration*
  across a step does reflect the step — the recorded soundness boundary under "Clock faults"; a static skew
  cancels in the subtraction, so it preserves durations.) *violation:* folding the offset into the shared
  base clock (`bubble.now`) fires a skewed/stepped host's 1 s relative timer early/late, while a naive
  "`Now()` differs per host" check still passes (the strongest counterexample — every per-host reading looks
  right yet a timer mis-fires). *Enforced:* `TestDSTClockDurationPreserved` (under a non-zero offset an
  in-bubble interval's `time.Since` and the base-clock advance over a host's sleep are byte-identical to
  offset 0) and `TestDSTClockStepTimerImmune` (a step mid-flight does not move a pending timer's base firing)
  (`testing/simulation/clock_test.go`), mutation-tested against the `bubble.now`-corruption implementation.
- **DST-CLOCK-DRIFT-DURATION (entailed: a *rate* scales durations and deadlines coherently).** A host whose
  clock runs at rate `r` (`Drift(ppb)`, `r = 1 + ppb/1e9`) reads `time.Now`/`time.Since` advancing `r×` base,
  *and* its relative timers fire after the rate-converted base interval — a rate-r host's `d`-timer fires
  after `d/r` of base. The two are coherent: a host cannot detect its own drift (its `time.Since` over its
  own `Sleep(d)` reads back **never less than `d`** — the arm conversion rounds up, the wall accumulation
  rounds down, and the composition `floor(ceil(d/r)·r) ≥ d` holds for every rate — and at most ~`(r+1)` ns
  above). *violation:* drifting the wall reading but firing timers at the unconverted base `d` (a rate-2
  host's 1 s timer firing after 1 s of base = 2 s of its own time), or converting deadlines but not the wall
  — either makes the host's own clock self-inconsistent, a state no real crystal produces — **or rounding
  both conversions down**, whose composition reads back `d − 1..2` ns: `Sleep(d)` observably returning
  early in the host's own clock, real Go's documented "at least d" broken, the Soundness invariant's
  "timer before its deadline" false positive at non-dividing rates. *Enforced:* the
  wall split adds `(base−t0)·ppb/1e9` (truncated integer division — Go semantics; for a negative ppb
  truncation adds *less* negative drift, so the ≥ d composition holds a fortiori) and the single timer
  choke `(*timer).modify` converts `d→⌈d·1e9/(1e9+ppb)⌉` (overflow-safe ceil, `dstMulDivClampCeil`,
  mutation-tested against a `big.Int` ceiling oracle; the arm ADDITION clamps to `maxWhen` too —
  `TestDSTClockDriftHugeSleepFires`); `TestDSTClockDrift{Wall,TimerConversion,Ticker,SelfConsistent,
  Property}` plus the never-early property sweep at non-dividing rates
  (`TestDSTClockDriftSleepNeverEarly`, `testing/simulation/clock_drift_test.go`).
- **DST-CLOCK-DRIFT-MONOTONIC (entailed: a drifting clock still moves forward).** A drift rate is strictly
  positive (`ppb > -1e9`, rate in (0, 2]); a clock that stops or reverses is a *step* (a discontinuous wall
  jump), not drift. So a drifting host's `time.Now` advances monotonically across base advances, fast or slow.
  *violation:* a non-positive rate (a frozen or backward-running clock with no NTP step) — the
  DST-FAULT-SOUND counterexample "a clock that runs backward with no NTP step". *Enforced:* `simulation.Drift`
  rejects `ppb ≤ -1e9` and the runtime clamps defensively; `TestDSTClockDriftMonotonic` /
  `TestDSTClockDriftRateValidation`, mutation-tested.
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
  the developer's box, and `Kill(pid, 0)` never probes host pid liveness. *violation:* a real hostname/pid/
  interface leaks into a run, or a host pid-zero probe succeeds → behavior depends on the dev machine and is
  unreproducible elsewhere (a soundness break — an execution the simulated universe could not produce
  identically on another machine). *Enforced:* the accessors gate on the run being active (`dstSimEnvSet` /
  `dstActive`) and return only synthetic values; the pid-zero hook consults the runtime simulated pid table;
  `TestDSTIdentitySound`, `TestDSTKillPidZeroLiveness` (`testing/simulation`, for hostname/NumCPU/pid and
  pid-zero liveness) and `TestDSTNetInterfaces` (`net`, the synthetic `lo`+`eth0` set replacing the real
  NICs).
- **DST-MEMALLOC-DET (entailed: OOM-relevant determinism + attribution).** Each heap allocation accrues to
  the *allocating* goroutine's process (`cur.dstProc`, inherited by its subtree); distinct processes have
  independent counters; and the per-process counter is deterministic *to the granularity the OOM fault
  needs* — the budget-**crossing** decision (does process P exceed budget B?) is a deterministic function
  of the seed. The *exact* byte count is **not** byte-deterministic across runs: it carries sub-observable
  noise the same way the GC's `DST-MEM-1` byte-noise does, sound for the same reason (it cannot flip
  a budget-scale crossing; an OOM budget sits far above the noise floor of a few KB). The pooling
  component of that noise is closed by EXCLUSION, not normalization: the accounting hook skips the
  runtime-internal pooled structs (`g`/`sudog`/`_defer`, `dstIsInternalPooledType` — the Memory
  accounting entry above), so whether a channel op allocates or reuses a pooled cache entry never
  reaches the counter at all. What remains in the band is non-pooled allocator noise (mid-run
  cross-consumer effects on shared runtime state), which stays sub-observable. *violation:* an allocation attributed to the wrong process (or the root), or counts
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
- **DST-NET-THROTTLE (entailed: rate bound / soundness).** On a connection with bandwidth limit B, the
  receiver gets N bytes no faster than N/B of base time — the link transmits segments serially at B.
  *violation:* a transfer delivered faster than B (e.g. segments not serialized through the per-direction
  `linkFreeAt`, or transmission time mis-scaled) lets a SUT pass only because the harness gave it
  impossible bandwidth — a false negative the finite-link DoF forbids. *Enforced:* `push` advances a
  per-direction `linkFreeAt` by `len/B` per segment (store-and-forward) so each segment's `deliverAt` is no
  earlier than the prior segment's transmit-end + its own (`net/dst_wire.go`); `TestDSTNetThrottleRate`
  asserts a 1 MiB transfer's first-to-last read span is ≥ (size−chunk)/B, and `TestDSTNetThrottleDeterminism`
  that it replays exactly (`net`), mutation-tested (disabling the gate, halving the transmission time, and
  dropping the serialization each fail the rate bound).

(The fault-feature invariants are in the next section.)

## Fault orchestration design

Status: **landed** for the net, disk, clock, crash/restart, host-crash, and crash-tear axes —
composed under one seed, replay-exact; OOM kill and the straggler remain the ⏳ row. **Methodology: every axis and seam is designed upfront
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
cross-host conns, and a process crash exactly that process's resources (DST-FAULT-VICTIM). The host-pair
targeting (`Partition`/`Heal`/`Isolate`/`HealHost`) is **landed** as the imperative network-partition API;
the per-process and per-host forms (`Crash`, `ResetProcess`; `CrashHost`) and the crash/restart axes are landed;
the group/zone forms and the `Link`/`OOM`/`Straggle` sugar extend it as those axes land.

**Victim names fail loud.** A fault or configuration API that takes a host name (`StepClock`,
`DriftClock`, `HostIP`, `HostFS`, the partition/reset targets) **panics on a name no `Host`
declaration has established** — it never interns a fresh host id as a side effect. A typo'd victim
would otherwise mutate state no goroutine observes: the fault silently tests nothing, the run stays
green, and the SUT's fault handling goes unexercised — a silent no-op at odds with the fail-loud
posture everywhere else (undeclared-host panics, the `-tags dst` panic). Enforced at the one name→id
lookup choke point in `testing/simulation` (`lookupHost`/`lookupProc`) that every victim-name intake —
clock, HostIP/HostFS, partition, reset, disk — shares; outside a run the calls stay documented no-ops.
`TestDSTFaultVictimUnknownPanics` / `TestDSTFaultVictimOutsideRunNoop`.

**Fault callers fail loud too.** A fault-injection or clock-fault API invoked during an active run
from a goroutine OUTSIDE the run's bubble panics, naming the API and the fix — it never executes at
an OS wall-clock instant the seed does not control (crash, partition, disk) and never silently
no-ops (clock faults, whose caller has no bubble clock to step): both are the same
silently-tests-nothing class the victim rule kills. Faults are injected from goroutines the
simulation schedules — the run body or goroutines started inside it. Outside a run the calls stay
documented no-ops. `TestDSTFaultFromNonBubbleGoroutinePanics` / `TestDSTFaultOutsideRunIsNoop`.
The DECLARATION APIs (`Host`, `Process`) carry the same guard: they mutate run state too — a
mid-run `Host` re-declaration is a reboot (host-up relay plus clock re-establishment) and
`Process` starts SUT goroutines — so a foreign caller during an active run panics identically,
while pre-run and outside-run calls keep their documented behavior.
`TestDSTTopologyFromNonBubbleGoroutinePanics`. The guard's decision and the guarded op are
**atomic against the run activation/deactivation edges**. An inactive caller retains the reader-side
gate through its mutation, so a call that passed the guard just before a run started completes as the
documented pre-run no-op before activation proceeds. An admitted active bubble caller validates on a
fast path without acquiring the gate: its bubble liveness excludes deactivation while it can continue, while
a crash-marked parked or runnable caller never resumes and therefore cannot mutate after losing
liveness. A foreign active caller panics before mutation. (`TestDSTRunActivationExcludesInFlightGuardedOps`,
`TestDSTRunDeactivationExcludesInFlightGuardedOps`). Inactive declaration APIs retain the gate through
their mutations; active declarations rely on bubble liveness after validation. Neither retains the gate
through `f`: user code inside `Host`/`Process` bodies runs ungated. `Process` latches the admitted run epoch and validates it from the still-live bubble before teardown locking; a pre-run body spanning activation cannot apply stale PID or
process identity to the new run, and a killed teardown waiter cannot strand deactivation.

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
  behind an earlier segment, never overtakes it). **Connection establishment pays the link too**: a
  cross-host dial completes after one round trip (SYN + SYN-ACK, each a one-way traversal paying base
  latency + a jitter draw; throttle exempts the zero-payload control segments — see the transport model,
  design.md) — a zero-RTT connect would let a SUT's connect timeout pass under simulation on a link
  where it fails in production. The per-host-pair **matrix** (asymmetric per-link latency
  / jitter) is the L4 targeting API (`Link("h1","h2").Latency()`). DoF: a real link has variable latency.
  Sound — it is a fake timer, the contract `time` already virtualizes.
- **Partition** — between a host-pair (symmetric or one-directional) or isolating a host, over a virtual
  window. *On connect:* a Dial across the partition either **refuses** (`ECONNREFUSED`, peer-down
  semantics) or **blackholes** (the Dial blocks until its context/deadline — packets-dropped semantics);
  the mode is **selectable per fault** (the `Fault` record carries refuse | blackhole) — both are real TCP
  outcomes and a SUT tests against each, so the choice is the SUT's, not hardcoded. *On an established
  conn:* bytes **not yet delivered** at the cut — in flight, or written after it — are held: reads of
  *those* block durably on the fake clock, and writes fill the **bounded** send buffer (the transport
  model, design.md) then block, until the partition **heals** (held bytes flush in order; TCP buffers
  and recovers) or the **retransmission horizon** errors the conn `ETIMEDOUT` (a cut outlasting the
  horizon kills the connection, as ~15 kernel retries do; a deadline/`Close` still errors it sooner).
  Bytes **already delivered** before the cut sit in the receiver's kernel buffer on a real machine, so
  they **stay readable during the cut** — a partition severs the link, never data the receiver already
  holds; blackholing pre-delivered bytes fails a read a real kernel serves (a sim-only failure, the
  false-positive class the Soundness invariant forbids). The reader's arrival horizon is capped at the
  cut-start of the INCOMING direction (`dstPartCutStartDir(peer→local)`): bytes delivered strictly before
  the cut are readable, in-flight and after-cut bytes are held (`TestDSTNetPartitionPreDeliveredReadable`
  / `TestDSTNetPartitionRecover`). DoF: a transient partition. **Landed** via the imperative targeting API
  `simulation.Partition(a,b)` (symmetric blackhole) / `Heal(a,b)` / `Isolate(h)` / `HealHost(h)`, plus the
  two mode variants: `PartitionRefuse(a,b)` — a Dial across the cut fails `ECONNREFUSED` fast rather than
  blackholing (`TestDSTNetPartitionRefuseConnect`) — and `PartitionOneWay(from,to)` — an **asymmetric** cut
  of only `from→to` while `to→from` still flows (`TestDSTNetPartitionOneWay`). (The declarative
  `Options.Faults` + per-fault mode is L4.) Cut state is **directional**: a symmetric cut sets both
  directions, while overlapping sources remain independently active in each direction. The earliest
  active source controls which bytes predate the cut, and any blackhole source dominates refusal. Cut
  targeting uses the conn's host attribution
  (`dstConn.localHost`/`remoteHost`), so a cut touches exactly the targeted pair's cross-host conns in the
  cut direction (DST-FAULT-VICTIM). A dial checks BOTH handshake directions (SYN dialer→target, SYN-ACK
  target→dialer), so a one-directional cut of either fails the connect — and the gate covers the
  WHOLE handshake, not just its front door: a dial parked on a full accept backlog re-checks the
  cut table on every partition change (a slot freed during a cut completes other dials, never the
  parked one — the retransmitted SYN is dropped for the cut's whole duration), and the SYN-ACK
  completion gates on its returning direction — each waiting for heal bounded by the retransmit
  horizon (`ETIMEDOUT`; design.md's accept-backlog and connect-cost paragraphs,
  `TestDSTNetBacklogParkPartitionTimesOut`/`…HealCompletes`, `TestDSTNetSYNACKPartition*`). A refuse cut fails the dial
  IMMEDIATELY (no ½-RTT SYN traversal), the same recorded timing simplification the direct
  declared-host `ECONNREFUSED` carries (design.md "Connect cost"). **Blackhole dominates refuse**: if a
  drop source (an isolated endpoint, or a blackhole-mode cut) is active on either handshake direction,
  the dial blackholes even when a refuse cut is also present — a dropped SYN elicits no RST, so
  reporting `ECONNREFUSED` there would be a sim-only false failure (`dstDialCut`;
  `TestDSTNetPartitionRefuseWithIsolateBlackholes`). ALL conns are
  **wire-backed** (the buffered transport — itself the faithful TCP send-buffer shape), so writes
  during a cut queue up to the buffer bound and **flush in order on heal with no loss** (DST-NET-FIFO +
  the sound buffer-and-recover model). **"Drop" lives here**, at flow granularity — a partition window
  drops everything between A↔B; there is *no* single-byte drop on a live stream (TCP forbids it — that is
  the UDP follow-on). *Recorded flow-level abstraction:* under a **permanent** one-directional cut the
  reverse direction keeps delivering indefinitely — the sim models the cut at flow granularity and does
  not reproduce ACK-starvation (a real sender stalls and eventually `ETIMEDOUT`s when its ACKs travel the
  cut direction). This is a *completeness* limit (the sim MISSES a real fault — the safe, ⊆-real
  direction), never a false failure; ACK-level reverse death is a possible finer-grained follow-on.
- **Connection reset** — inject a connection reset (an RST, surfacing as the one-shot `ECONNRESET`)
  on a process's or a host-pair's conns.
  DoF: a real RST (peer crash, middlebox). **Landed** via `simulation.Reset(a,b)` (host-pair) and
  `ResetProcess(p)` (process), over a per-run conn registry (`net/dst_reset.go`) keyed by the conn's
  host/process attribution (`dstConn.localHost`/`remoteHost`/`localProc`/`remoteProc`) — so a reset touches
  exactly the victim's conns (DST-FAULT-VICTIM, now with its process leg). An injected reset hits **both
  ends**, and each end receives it as a real kernel delivers an RST (both ends are SURVIVORS — these
  faults reset connections, not processes): bytes already **delivered** to that end's receive queue
  drain first — tcp_recvmsg reports pending data before the socket error, host-probed — then the
  first failing op reports `ECONNRESET` and later ops carry the CLOSED-socket identities (the
  kernel's one-shot `sk_err`: later reads `io.EOF`, later writes `EPIPE`; an RST arriving after the
  peer's FIN was delivered takes the `CLOSE_WAIT` arm — `EPIPE`/EOF throughout, no `ECONNRESET` —
  see design.md's FIN/RST paragraph); writes fail immediately with the pending error; bytes still **in flight** toward
  either end are destroyed (the RST beat them to the socket — one of the orderings a real injection
  race produces, so executions stay ⊆-real), and the receive queue is FROZEN at the RST instant —
  a segment sent toward it afterward is never delivered (a CLOSED socket answers a late segment
  with its own RST, it does not queue it). *Enforced:*
  `TestDSTNetResetDrainsDeliveredThenResets`, `TestDSTNetResetProcessOwnEndDrains`,
  `TestDSTNetResetDropsInFlight`, `TestDSTNetInjectRSTFreezesReceiveQueue`. A conn still QUEUED in the
  accept backlog takes the same survivor shape — the accept queue holds only ESTABLISHED children
  (a receive queue exists and may already hold the dialer's bytes), and the kernel does not unlink an
  RST-aborted child from the queue: a later `accept(2)` hands it out, its reads drain the delivered
  bytes, then the one-shot `ECONNRESET` (host-probed, with and without pre-accept data). *Enforced:*
  `TestDSTNetResetBacklogAcceptHandsOutResetChild`, `TestDSTNetResetBacklogDrainsPreAcceptBytes`.
  A dial still blocked mid-establishment aborts promptly with `ECONNREFUSED` — the dialer's socket is
  in SYN_SENT, and `tcp_reset` maps an RST received in SYN_SENT to `ECONNREFUSED` (the
  connection-refused mechanism itself; host-probed via the closed-listener shape), never the
  established-state `ECONNRESET` (`TestDSTNetResetBacklogBlockedDialFailsPromptly`,
  `TestDSTNetSYNACKObservesReset`).
  When a reset matches **several** conns, the victims are
  reset in **connection-registration order** (a per-run sequence id recorded at establishment,
  `dstConn.regSeq`; the victims are collected from the registry and sorted by it —
  `TestDSTNetResetVictimOrderByRegSeq`) — never in
  map-iteration order over pointer keys, whose bucket placement hashes run-varying heap *addresses* and
  would make the wake order of the victims' blocked readers, and thus the downstream schedule, diverge
  across same-seed runs (DST-FAULT-REPLAY). This completes the network axis.
- **Throttle / bandwidth** — pace delivery so ≤ B bytes cross per virtual time unit (latency
  proportional to bytes, the same fake-timer queue as latency). DoF: finite link bandwidth. **Landed** as
  `Options.Network.CrossHostBandwidth` (bytes/sec, **per connection-direction**): the wire serializes
  transmission via a per-direction `linkFreeAt` clock — a segment of S bytes occupies the link `S/B`
  (store-and-forward) before the base latency + jitter propagate it — so a receiver gets bytes no faster
  than B (DST-NET-THROTTLE). Deterministic (no fault-RNG draw) and FIFO-preserving (`linkFreeAt` monotone +
  head-of-line). The transmit-time arithmetic (`S·1e9/B`) is overflow-safe for any in-spec segment (the
  bounded send buffer caps S, and the mul/div is guarded like the clock-drift conversions — a wrapped
  negative transmit time would corrupt `linkFreeAt` and break the rate bound; the guard (`dstTransmitNanos`,
  the `q·1e9 + ceil(r·1e9/B)` split) and the cap (`Options.Network.SendBuffer`) are landed). Default 0 = unlimited;
  same-host always unlimited. Shared-link contention (one budget
  across a host-pair's flows) is the L4 per-link refinement; this is per-flow (each direction an
  independent B-capacity link, so executions stay ⊆ real).

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

- **EIO** — **landed**. A targeted file or a host's disk fails `read`/`write`/`Sync` with `EIO`, injected
  mid-run by `simulation.FailDisk(host)` / `FailFile(host, path)` (and cleared by `HealDisk` / `HealFile`).
  The fault is policy on the host's disk (`os` `dstFSDisk.eio` / `eioFiles`), consulted at the `dstFile`
  I/O choke points (`read`/`pread`/`write`/`pwrite`/`sync`) — never new representation. DoF: real disks
  return EIO. Sound only where the real call can fail: it is injected *before* any mutation, so a faulted
  write writes no bytes and a faulted `fsync` does **not** advance the durable image (it cannot tear
  "durable" state — the durability-monotonicity invariant holds under fault); and never at an infallible
  call (seek, in-memory stat) — the Soundness boundary. A failed DATA sync additionally models
  **fsyncgate** (Linux >= 4.13): the failed writeback drops the file's dirty pages from the writeback
  set, so a RETRIED sync succeeds without the data ever reaching the durable image — the "retry fsync
  after EIO" recovery passes while power loss still loses the data — and only pages REWRITTEN after
  the failure (a write event, not a content diff: rewriting byte-identical content redirties, so the
  correct rewrite-then-fsync recovery works) reach the platter on the next sync; unrewritten pages
  stay durably stale. Recorded bounds: the drop is modeled for file data pages (directory entry
  writeback keeps the full-commit model); a store through a SUT mapping is an event the model
  cannot observe, so it does not redirty a dropped page; and the model never EVICTS a clean-stale
  page — reads keep returning the never-persisted content, one legal Linux schedule (real kernels
  may evict, after which reads return the old platter bytes). A host crash clears the mark (the cache is
  rebuilt from the platter). Enforced by `TestDSTDiskEIOFsyncgate` and the retried-sync leg of
  `TestDSTFSVirtualFDSyncFrontDoorsFailWithoutCommitting`, mutation-tested. The per-file fault keys on the *node*, not the
  path, so a bad sector follows the file across a rename and a removed-but-open handle keeps failing;
  `FailFile` is scoped to a regular file (a directory is a no-op — its own `fsync` stays clean — while a
  whole-disk `FailDisk` does fail a directory `fsync`, a dead disk persisting nothing). Faults are explicit
  toggles (no fault-RNG draw, like the clock step), so the schedule replays directly off the deterministic
  interleaving. Per-host / per-file victim isolation, durable-image preservation, infallible-call immunity,
  and replay are enforced by `TestDSTDiskEIO*` (`os/dst_disk_fault_test.go`), mutation-tested. Driven
  through the runtime relay `dstDiskFaultOp` (os registers the handler from init, mirroring the net
  partition relay), so `testing/simulation` needs no `os` dependency.
- **ENOSPC** — **landed**. Writes/creates on a host's disk fail `ENOSPC` past a budget, injected mid-run
  by `simulation.LimitDisk(host, bytes)` (and removed by `UnlimitDisk`). A capacity on the host's disk
  (`dstFSDisk.capped`/`capacity`) caps total regular-file content; a write that would grow the disk past
  it fills the remaining space and returns **the partial count together with the error — `(n, ENOSPC)`
  in one call** — and a create/`mkdir` on an already-full disk fails ENOSPC. `Write` (a single backend
  call) surfaces the combined `(n, ENOSPC)` directly; `WriteAt` reaches the same shape through
  `os.File.WriteAt`'s own retry loop, so the backend's `pwrite` returns partials as `(n, nil)` and the
  loop surfaces ENOSPC on the following zero-byte call (`TestDSTDiskENOSPCPartialFill`,
  `TestDSTDiskENOSPCStraddleOverQuota`). The one-call surface is the
  real kernel's as Go exposes it: `write(2)` returns the short count and the *retry* gets `ENOSPC`, but
  `internal/poll.FD.Write` **loops** until error or completion, so a real `os.File.Write` that partially
  fills a disk returns `(n, ENOSPC)` and can never return a bare short count (`io.ErrShortWrite`). The
  simulated backend must surface the same shape at its own layer — otherwise `Write` (whose retry loop
  the backend replaces) reports `io.ErrShortWrite`, an error identity real `os.File.Write` cannot
  produce for a regular file, and the SUT's `errors.Is(err, ENOSPC)` recovery path does not fire on
  exactly the write the fault was injected to exercise (and `Write` vs `WriteAt`, which has its own
  loop, would disagree — a state no kernel produces). Space in use is summed on demand from the live
  tree (`residentLocked`) by unique inode identity — a crash-tear rename image may contain both old and
  new names for one inode, whose bytes count once — and is **not** tracked incrementally, so a delete or truncate-down frees room for the
  next write with no accounting in the mutation paths — and never the false ENOSPC a budget-that-ignores-
  frees would produce (DoF: a full disk; sound because in-place overwrites consume nothing, frees are
  honored, and the partial-fill matches real short-write semantics). A write whose effective slice
  is empty — a zero-length write at any offset, or one FULLY refused by the cap — has no effect at
  all, as POSIX gives `write(2)`: no growth to the seek offset, no mtime, no resident-byte charge
  (a refusal that grew the file would break the capacity invariant with no path to recover the
  budget; `TestDSTDiskENOSPCRefusedWriteDoesNotGrow`, `TestDSTFSZeroLengthWriteNoEffect`). Per-host victim isolation, frees,
  partial-fill, and replay are enforced by `TestDSTDiskENOSPC*` (`os/dst_disk_fault_test.go`),
  mutation-tested. **Recorded modeling boundary — logical bytes, not allocated blocks (sparse
  files).** The cap counts a file's LOGICAL content length; a real filesystem charges allocated
  blocks, and a hole allocates nothing (host: a sparse `Truncate`-grow shows `size=N blocks=0`).
  Probe-verified consequences, both directions: (a) a sparse truncate-grow's hole bytes COUNT
  against the cap, so the classic WAL/journal sparse-preallocation pattern can hit `ENOSPC` here
  where a real disk at the same quota has all blocks free (a false-positive window); (b) a write
  INTO a hole is a no-growth overwrite and is never charged, so filling preallocated holes
  succeeds where a really-full disk would `ENOSPC` on block allocation (the paired false-negative
  window); and (c) truncate GROWTH is charged to usage but not itself checked against the cap —
  the disk silently enters the over-quota state, after which growth and creates fail until enough
  is freed. Closing the window needs allocation-granular (extent/hole-aware) accounting — charge
  on materialization, check truncate growth — a capacity-model rebuild, not an accounting-formula
  tweak; until then LimitDisk models a full disk faithfully for densely-written files only, and a
  SUT whose durability discipline relies on sparse preallocation is outside the fault's honest
  surface.
- **Latency** — **landed**. Delay each disk-touching FS op by a virtual duration (a slow disk), set
  mid-run by `simulation.SlowDisk(host, perOp)` (and removed by `SlowDisk(host, 0)`). The calling goroutine
  sleeps the per-host per-op latency (`dstFSDisk.latency`) on the bubble clock *before* the op — every op
  that reads/writes content, traverses directories, or allocates a node (read/write/sync, open, named
  stat and `Chdir` — their path walks touch the disk — mkdir, remove, rename, readdir, truncate, chmod, chtimes);
  pure in-memory ops (seek, `Getwd`, a closed-fd read that returns EBADF without I/O, and an **open
  handle's `File.Stat`** — fstat reads the in-core inode, which a slow disk does not delay) are not
  delayed, as a real slow disk would not delay them. The sleep
  is read lock-free and taken *outside* the tree lock — so a slow disk on one host never stalls another's
  filesystem (sleeping under the shared lock would in fact deadlock the bubble, since virtual time cannot
  advance while a goroutine holds a mutex) — and a composite helper pays the latency once per backend op
  (`os.Rename` = stat + rename = 2×). DoF: a slow disk; sound because the delay only postpones the op (its
  result is unchanged) and only on ops that truly touch the disk. The duration is explicit (no fault-RNG
  draw), so the virtual delays replay deterministically. Per-host victim isolation, host independence,
  in-memory/closed-fd exemption, and replay are enforced by `TestDSTDiskLatency*`
  (`os/dst_disk_fault_test.go`), mutation-tested. This completes the disk axis.
- **Crash (the durability tear)** is the **host (power-loss) crash** — see "Crash / restart faults". Its
  default policy restores a host's disk to **exactly its durable image** (synced survives byte-exact,
  everything unsynced is lost): one legal outcome, deterministic, and the simplest to reason about. With
  `Options.CrashTear` the policy instead explores the outcomes the contract permits, drawn from the fault
  RNG (the policy is per-run, published only after the run is ADMITTED: a rejected nested/concurrent
  attempt panics with no side effect on the active run's policy —
  `TestDSTRejectedNestedRunKeepsCrashTearPolicy`): each dirty **page** of a file independently
  reached the platter, did not, or was caught in flight
  and **tore at a byte boundary** (the physical torn-write shape: the bytes that went out before the cut
  landed, the rest did not — a strict subset of the arbitrary byte mixes the contract permits, which is
  the sound direction to be incomplete in); each unsynced directory-entry change (a create, a remove, a
  rename-over) independently landed or did not; a file's unsynced **size** change draws over what
  writeback can leave in the inode: a SHRINK (truncate-down) is one metadata update — it landed or it
  did not — while a GROWTH additionally reaches every **intermediate page-boundary size** between the
  durable and current lengths, because real writeback flushes the grown tail page by page and advances
  the on-disk i_size as each lands (a file grown by several unsynced appends can crash at an i_size no
  binary durable-or-current draw could produce; a page below the drawn size that did not land reads as
  a hole, the sparse region delayed allocation leaves —
  `TestDSTCrashTearIntermediateSizes`). Synced
  bytes and synced entries never move — no atomicity beyond `Sync`. A page past the live file's end (an
  unsynced truncate whose size change did not land) holds nothing to write back: the platter keeps the
  durable blocks the truncate never freed.

  *Why pages, not a write log (soundness).* Writeback flushes pages, and a dirty page carries the CURRENT
  bytes of every byte in it: if two writes touched a byte before the crash, the page holds the later one.
  So replaying "an arbitrary subset of the unsynced writes, reordered" would persist an older write's
  bytes for a byte a newer write covered — a state no page cache can produce, the false-positive class
  DST-FAULT-SOUND forbids. The subset the contract asks for is therefore an arbitrary subset of dirty
  PAGES (reorder is unobservable *within* a file: its image is a set of pages, not a sequence), while
  ordering ACROSS files and names — persist a file's data but lose its name, persist one file of a
  two-file transaction — falls out of drawing each node's pages and each directory's entries
  independently. Enforced by `TestDSTCrashTearRespectsDurableBytes` (durable bytes stable; every unsynced
  byte is either its written value or unwritten, never an older one; the seed sweep reaches lost, landed,
  and byte-torn), `TestDSTCrashTearEntriesSubset`, and `TestDSTCrashTearReplays` (same seed →
  byte-identical wreckage, different seeds tear differently — DST-FAULT-REPLAY, every draw ordered by
  page index and sorted entry name, never map order).

  Verbatim the durability contract already settled and
  **monotonicity-enforced** (`TestDSTFSDurabilityMonotonicity`); the representation needs no change, only
  a crash policy reading the durable image. A *process* crash does **not** tear the host FS (the kernel
  survives) — that split is the crash section. The reason the durability split exists.

Collapse-check (disk axis): **not finer** — each is a real disk outcome; the crash tear is bounded by
the synced/unsynced split the representation already tracks. **Not coarser** — hooks the single FS commit
point. **Not foreclosing** — `Sync`/durability were designed for this; the fault adds policy, not
representation.

### Clock faults

Over the per-host clock seam (the distributed model's "Per-host clock"):

- **Step** — **landed**. A sudden wall-clock jump (NTP slew/correction), forward or **backward**, injected
  mid-run by `simulation.StepClock(host, delta)`: it adds `delta` to the host's per-host clock offset
  (`runtime.dstStepHostClock`), so the host's `time.Now` jumps while timer deadlines — which read the base
  clock — are untouched. A backward step is exactly the HLC adversary; DoF: real clocks get stepped. The
  delta is explicit (no fault-RNG draw), so the step schedule replays directly off the deterministic
  interleaving. Per-host victim isolation, timer-deadline immunity, forward/backward steps, accumulation
  over a base skew, and seed-deterministic replay are enforced by `TestDSTClockStep*`
  (`testing/simulation/clock_test.go`), mutation-tested.
- **Drift** — **landed**. A host's clock rate departs from 1 (runs fast/slow), declared at Host declaration
  by `simulation.Drift(ppb)` (parts-per-billion; rate `1 + ppb/1e9`, rate in (0, 2]) and changed mid-run by
  `simulation.DriftClock(host, ppb)`. Unlike step/offset, drift scales *durations* and *deadlines*: the wall
  reading drifts (`(base−t0)·ppb/1e9` added at the wall split), and a relative timer's host-duration `d` is
  converted to base `d/rate` at the single timer arm choke `(*timer).modify` (`runtime.dstTimerArmForDrift`)
  — through which every Sleep / After / NewTimer / NewTicker / AfterFunc / context deadline funnels, the
  periodic ticker re-arm reusing the converted period. **Rounding contract:** the arm conversion rounds
  **up** (ceil) and the wall-drift accumulation rounds down (floor), so the two compose to a host-perceived
  elapsed **≥ d always** — `floor(ceil(d/r)·r) ≥ d` — with at most ~`(r+1)` ns of overshoot. A floor at
  the arm would compose to an elapsed of `d − 1..2` ns: a timer firing **before its deadline in the
  host's own clock**, verbatim the Soundness invariant's named false-positive class (`Sleep(d)` returning
  with `Since < d`, which real Go documents can never happen). **Overflow contract:** every arm-side
  computation — the conversion itself AND the `when = now + converted` addition — clamps to `maxWhen`,
  never wrapping negative: bubble base time is ~9.47e17 ns, so a converted span clamped to `maxWhen`
  makes the *addition* wrap unless it is guarded too, and a negative `when` fails `needsAdd` — the timer
  is silently never heaped and never fires, turning `time.Sleep(math.MaxInt64)` (the standard
  block-forever idiom, which `timeSleep` clamps to `maxWhen`) on any slow-drifting host into a
  harness-manufactured deadlock report neither real hardware nor the un-drifted simulation exhibits.
  So the *monotonic* clock the earlier design note anticipated is realized as the
  wall reading itself (bubble durations are wall-based), and the "rate-aware deadline conversion at the
  synctest wake" as arm-time conversion. A **mid-run change** re-anchors the wall so it stays continuous
  (folds drift-so-far into `offset`, resets the anchor) and re-maps every *armed* timer of the host
  (`when' = T + (when−T)·r_old/r_new` — including the formula's NEGATIVE remainder: an OVERDUE
  timer, reachable for a never-heaped channel timer that fires lazily, RE-ANCHORS at the last
  host-period boundary before the change — `when' = T − remapFloor((T−when) mod period_old)`, the
  remainder taken in the old regime where host scaling cancels so the boundary index is exact, and
  floor-remapped so the anchor never lands earlier than the true boundary — and the re-arm catch-up
  then counts whole new-regime periods from a boundary-aligned anchor: never early in
  host-perceived time, late by at most a nanosecond per rounding step (the forward remap's
  contract, mirrored; anchoring on the raw ceil'd span instead double-rounds against the ceil'd
  period, and an exact-multiple overdue span undercounts the catch-up index, duplicating a
  boundary's tick almost a full period early). The DELIVERED timestamp is unchanged by the conversion: a
  lazily-fired timer's value is derived from its fire-time delay, so a one-shot's `when` is not
  converted at all (it never re-arms) and a periodic timer's conversion move is recorded on the
  timer and added back to the delivery delay — an overdue tick or timer crossing a rate change
  reports the same due time it would have reported without the change
  (`TestDSTClockDriftClockOverdueDeliveredTimestamp`). For a STANDING non-unit rate, a channel timer
  first received after its due base instant retains the fake-timer callback's mixed clock domains:
  `sendTime` runs in root/base clock context and reconstructs the base-coordinate due instant. If a
  host duration `d` arms at converted base duration `D=ceil(d/rate)`, and host wall minus base wall at
  arm is `C`, the delivered wall timestamp is displaced from the host-perceived due timestamp by
  `D-C-d`. At a zero-offset drift anchor (`C=0`) this is approximately `(1-rate)*D`; floor/ceil
  composition contributes less than 2 ns beyond that term, so only in that anchored case is the
  absolute displacement bounded by `abs(ppb)*D/1e9 + 2ns`. Standing drift, skew, and steps contribute
  through `C`; there is no duration-only bound independent of that clock offset. The displacement does
  not grow with additional lazy-receive delay. This is
  the modeled timestamp behavior, distinct from the deadline contract (the timer still becomes
  eligible never-early in host time), and avoids pretending the runtime retains historical
  wall-at-due state that it does not store. Enforced for exactly divisible fast and slow rates by
  `TestDSTClockDriftLazyTimerTimestamp`, including delayed-arm cases with accumulated nonzero `C`.
  The conversion skips the `when == 0` "not running"
  sentinel and clamps its result above it — an extreme slowdown can remap the remainder past the
  whole base epoch, and the sentinel value would wedge the lazy fire;
  `TestDSTClockDriftClockOverdueTicker` (dividing and non-dividing rates, exact-multiple and
  fractional overdue spans), `TestDSTClockDriftClockOverdueRemapZeroClamp`,
  `TestDSTClockDriftClockOverdueExtremeSlowdown`, `TestDSTClockDriftClockStoppedTickerStaysStopped`); a periodic timer's **period is converted for
  every armed timer**, including one due exactly at the change instant (its `when` needs no move —
  firing "now" is correct under any rate — but the next re-arm reuses the period, which must
  already be at the new rate; `TestDSTClockDriftClockDueTicker`). Because a channel timer is heaped only while a goroutine is blocked on
  it, the re-map enumerates a per-run list of armed fake timers (`runtime.dstFakeTimers`), not just the heap,
  so a held `NewTimer`/`NewTicker` or a ticker between ticks is re-mapped too; the re-map is in place under the
  timer lock, preserving a zombie (it does not resurrect an unblocked channel timer). Epoch rollover,
  old-list reset, and new-epoch registration are serialized, so multi-P white-box activation cannot
  publish a timer between the new epoch and a later destructive head reset
  (`TestDSTFakeTimerRollPreservesNewEpochRegistration`). **Host
  re-declaration re-establishes the clock completely**: declaring `Host(name, config)` for an
  already-declared name applies the declared clock in full:
  re-map the host's armed timers from the surviving rate to the declared one, overwrite the offset
  to the declared value (a restarted clock is the declared clock, not a continuation — prior steps
  and accumulated drift are discarded, so no fold is needed), re-anchor, and set the declared rate
  (zero `HostConfig` = rate 1, in sync with base, as the `HostConfig` doc promises). An
  implicit-host `Process` restart does **not** re-establish its host's clock: a process restart
  leaves the machine up — its clock, including applied steps, survives (only `Host` models the
  reboot). Cross-run freshness needs no re-establishment at all: the per-host clock table is
  per-run state, reset at run entry (`dstSetSimEnv`), so a host id reused by a later run starts
  in sync by construction (`TestDSTClockTableFreshPerRun`).
  Setting only the offset while a stale rate and anchor survive leaves a "restarted, in-sync" host
  reading ahead of base and sleeping at the old rate — self-consistent to its own probes (the
  strongest-counterexample shape DST-CLOCK-DURATION warns about), so only a base-clock-relative test
  catches it. Enforced by
  `TestDSTClockDrift*` / `TestDSTClockDriftClock*` (`testing/simulation/clock_drift_test.go`,
  `clock_drift_dynamic_test.go`), mutation-tested, incl. a `big.Int` oracle for the conversion, a direct
  overflow-clamp regression, and the unheaped-timer re-map; the re-declaration contract is enforced by
  `TestDSTClockDrift{ResetByRedeclare,RedeclareSameRate}` and `TestDSTClockRedeclareRemapsPendingTimer`
  (`runtime.dstReestablishHostClock` — re-map at the old rate, overwrite the offset, reset the anchor). The
  seed-drawn (bounded) rate is **`BoundedDrift(maxPPB)`** — a per-host ppb in `[-maxPPB, +maxPPB]` from a
  stateless hash of (seed, host id) via `runtime.dstHostSeededDriftPPB` (an independent salt from the seeded
  skew, advancing no RNG stream), resolved at host declaration through the same `dstReestablishHostClock`
  choke, so the `wall = f_h(base)` representation is unchanged and it is stable across a restart
  (`TestDSTClockBoundedDriftSeeded`).

**Wall representability (recorded boundary).** The bubble wall is int64 nanoseconds. A skew or
step that would take a host's wall before the **epoch** is rejected with a panic at application —
`settimeofday` rejects a pre-epoch wall, so no real machine can hold one, and a silently clamped
wall would freeze the host's clock (its `Sleep` observably consuming zero host time — the
timer-early false-positive class); with every application point validated, the wall is non-negative
by construction (it grows from a valid anchor at the host's strictly positive rate). At the far
end the composition **saturates** at the largest representable wall (year ~2262): real kernels
accept later times, this representation cannot — deterministic, never a sign wrap.
`TestDSTClockSkewBoundary`.

**Soundness boundary (recorded, contractual).** In a simulation bubble `time.Now()` carries no monotonic
component (the synctest design — it returns `mono = 0`), so a *duration* is a wall-clock subtraction. A
static skew cancels in the subtraction (durations preserved); a **step changes the offset mid-measurement**,
so a wall-derived duration *across* a step (`time.Since`/`Sub`) shifts by the step — exactly as wall-clock
arithmetic across a real NTP step does on hardware. This is sound for the wall-clock logic a step targets
(HLC, `UnixNano`, lease/timestamp comparison). It is **not** the monotonic-clock immunity real Go gives
`time.Since`: the bubble is wall-only *because* cross-host skew must be observable via `Sub` (a
process-global monotonic reading would zero cross-host `Sub`, and Go's monotonic clock cannot be made
per-host), so that immunity is architecturally unavailable, not deferred to drift. Step-immune deadlines use
timers/contexts (`time.After`, `context.WithTimeout`), which read the base clock; a step never moves them.

Collapse-check (clock axis): **not finer** — virtual time, the contract `time` already owns; a host's
clock is a deterministic function of base time, so every reading is replay-exact. **Not coarser** — hooks
the single `time.Now()`/timer path. **Not foreclosing** — offset (substrate) and drift/step (fault) share
the one clock-function representation (step extends the per-host offset table to mutable; drift will extend
its element from a scalar offset to `(rate, anchor)` — an internal-state change, the Host targeting contract
unchanged).

### Crash / restart faults (the cross-axis fault)

Two distinct faults, because on a real machine the filesystem **page cache belongs to the kernel, not the
process** — so a process dying and the host losing power tear the disk differently:

| Fault | Kills | Host FS | Conns | Models |
|---|---|---|---|---|
| **Process crash** (`Crash("p")`) | that process's goroutines + memory + fds | **intact** — kernel survives, so un-fsync'd writes persist for host-siblings and the restart | its conns RST | a process dying / `kill -9` / OOM |
| **Host crash** (`CrashHost("h")`, power loss) | **all** processes on the host | **tears to the fsync'd durable image** (the disk "Crash" above) | all their conns RST | power loss / kernel panic |

Both, at the victim's next cooperative point: the targeted goroutines (process membership or current /
inherited entered-Host membership names the victim) are
**descheduled permanently** (the `dstSchedSelect` seam never selects them again; their in-flight blocking
ops are abandoned — a crash does not, cannot in Go, force-unwind a goroutine mid-instruction; the sound
model is *they never run again*, what a killed process's threads do), conns RST, fds drop, memory is gone.
The host crash additionally tears the host disk; the process crash leaves it intact.

The crash-RST collapse has one boundary: a connection whose victim-side end the application had
**already closed** before the crash is NOT reset at the surviving peer. Its data and FIN are on the
wire, and a powered-off machine emits no packet — nothing can destroy bytes the network already
carries, so the peer drains and reads `io.EOF` (or whatever error the pre-crash teardown recorded),
exactly as if the crash never happened to that conn (DST-FAULT-SOUND: no real RST exists for an
app-closed end at power loss). *Enforced:* `TestDSTCrashHostSparesAppClosedConns` (host crash),
`TestDSTCrashProcessSparesAppClosedConns` (process crash — the kernel survives a `kill -9` and has
no socket left to answer RST for an fd that left the table at close). The RST applies to
the connections the victim still holds open. The victim's own ends reset outright — their receive
queues died with the process or kernel. The surviving peer's end receives the RST
kernel-faithfully: bytes already **delivered** to the survivor's receive queue drain first (an
incoming RST cannot destroy what the survivor's kernel already holds — tcp_recvmsg reports pending
data before the socket error, host-probed), then its first failing op reports `ECONNRESET` (the
one-shot `sk_err`; later reads `io.EOF`, later writes `EPIPE`); bytes still **in
flight** die with the crashed sender (its kernel's send buffer and its emissions never complete —
in the simulation's one-queue wire model every undelivered byte is destroyable in some real
execution, so the collapse stays ⊆-real).
*Enforced:* `TestDSTCrashHostDropsInFlightBytes` (delivered bytes drain, in-flight bytes die),
`TestDSTCrashProcessSurvivorDrainsDeliveredBytes`, `TestDSTCrashHostFreesVictimPorts`
(the victim's port space clears with the machine). And until a Host re-declaration reboots the
machine, a dial to it **blackholes** — a powered-off kernel answers no SYN, so the connect times
out (deadline or retransmit horizon), never `ECONNREFUSED` (design.md, Connect cost; *enforced:*
`TestDSTCrashHostDialBlackholes`).

Process resource teardown is the shared substrate for process crash, OOM, restart, AND normal exit. On
normal exit, the invocation's goroutines are removed from the scheduler/deadlock-visible set first; if it
is the last live invocation, process-owned simulated files and virtual fds close,
fd-owned `flock`s release, process-owned mappings unregister (their `MAP_SHARED` bytes already are the
host page-cache pages; teardown performs no copy-back), and the process's
connections go down. Only then is its pid marked dead for `Kill(pid, 0)` and procfs. Teardown follows the
kernel's order: the goroutines (threads) die first, then resources close, then PID death is published. A
`Process` body's normal return (or panic unwind) IS the process's exit and routes
through this same teardown — the one difference is the connection shape: exit CLOSES the victim's conn
ends (the kernel close()s a dying process's sockets) with the kernel's own conditional per end — an end
whose receive queue holds unread data answers the peer with RST: the peer still DRAINS bytes already
delivered to it, and bytes the dying process wrote before exiting (they travel ahead of the RST on the
in-order link, and tcp_recvmsg reports pending data before the socket error — host-probed), and only
then does its first read fail ECONNRESET (one-shot, as everywhere:
`TestDSTProcessExitResetDeliversPreExitBytesThenResets`); otherwise
the close FINs and the peer drains buffered bytes then reads EOF — while crash RESETS the still-open
ends unconditionally (a recorded collapse: exit's per-end RST-vs-FIN conditional is not applied at a
crash, every surviving peer of a still-open end gets the reset), with the survivor draining its
delivered bytes first and in-flight bytes dying, per the crash-RST contract above; exit and crash
also differ in the in-flight bytes themselves (exit's close lets pre-exit writes travel ahead of the
RST; a crash destroys them) — and both close its listeners. The same conditional governs a USER-CALLED `Close()` on a live process
(`TestDSTNetCloseWithUnreadDataResetsPeer`, `TestDSTNetCloseAfterDrainingFINs`) — close(2) is
close(2), whoever calls it. The in-flight-counts-as-queued collapse is pinned by
`TestDSTNetCloseBeforeDeliveryStillResets`. One recorded collapse of the conditional: bytes still in flight count as queued —
the sim RSTs immediately, which is one of the two orderings a real close-vs-arrival race produces (the
kernel FINs first and RSTs when the data lands; a peer racing its read can observe either); the
FIN-then-RST arm is not generated. Goroutine teardown is per-invocation (pid-keyed);
resource teardown is per logical process (proc-keyed), so with concurrent same-name invocations it runs
when the LAST live invocation dies. The logical process id remains stable for targeting
and resource registries, while the pid is the invocation generation; a same-name restart gets a fresh pid and
does not revive the crashed goroutines. Finalizers and cleanups registered by an invocation carry its
run epoch and PID through GC discovery and queueing; once PID death is published, queued or later-discovered callbacks
are discarded rather than executed by the root drain. A live sibling with the same logical process id
retains its own callbacks.

Two consequences of pid-keyed goroutine death, both contractual. **A goroutine cannot outlive its
process.** One that escaped the pid-keyed mark because it was inside a NESTED `Process` body when its
enclosing invocation died (it carried the inner pid) is permanently parked the moment it leaves that
body — a thread of a dead process never resumes. **A crashed goroutine never unwinds:** its deferred
functions do not run, exactly as a killed process's threads abandon their stacks. So a `Crash` whose
caller belongs to the victim (a **self-crash** — the shape an allocation-triggered OOM takes, the victim
dying where its own workload takes it) never returns, and the code after it is unreachable.
Although the parked stack allocation and execution state remain intact, a crashed goroutine's stack is
not a GC root: after the GC suspends and owns the goroutine for root processing, it records the stack job
complete without tracing the abandoned frames or defers. Process memory reachable only from dead threads
is therefore collectible without resuming, unwinding, clearing, or recycling those stacks. Enforced by
`TestDSTCrashDropsVictimStackRoots` and `TestDSTCrashDropsVictimDeferredRoots`.
Victim enumeration keys on sticky active-simulation membership, not the live bubble pointer: GC entry
and assist may temporarily clear `g.bubble`, but that disassociation cannot let a process or host thread
escape death. Enforced by `TestDSTCrashMarksDisassociatedProcessMember` and
`TestDSTCrashHostMarksDisassociatedMember`.

One process is not crashable: the one whose goroutine set contains the run's **main** goroutine — a
`Process` declared inline in the run body rather than on a goroutine of its own. Killing it would leave
the universe with no driver (the body's remaining statements never run; the bubble never completes), so
the crash is **refused loudly before anything is torn down**, naming the fix (`go Process(name, f)`).
Silently ending the run instead would let a test's post-crash assertions vanish unexecuted. The same
refusal guards `CrashHost` for the machine the run's main goroutine runs on, and — because a host crash
has many victims — both faults pre-scan every victim and refuse *before* the first teardown step, so a
refused fault never leaves half a universe destroyed.
Process-crash refusal likewise preflights every live invocation before clearing any PID, registration,
or resource; recovering the refusal observes the entire logical process unchanged.

**A host is not a process.** `CrashHost` kills the union of two goroutine sets: every goroutine of a
process declared on the host (pid-keyed — which also catches a goroutine of that process momentarily
stamped with another host, inside a nested `Host` body: it is still a thread of a process on the dying
machine), and every goroutine whose current or inherited entered-`Host` ancestry contains the host
(host-keyed — which catches the ROOT process's goroutines running or descended from the machine's
`Host` body even while nested in another `Host`; proc 0 is the driver's own process, shared by every
host, so no pid names "the threads on this machine"). Host ancestry is an immutable chain inherited at
goroutine creation, so a child retains its machine membership after its parent leaves the `Host` body.
The root process's pid therefore stays live while the
declared processes' pids die. Correspondingly, **one logical process lives on one machine at a time**:
a same-name invocation live on a second host is refused, because a host crash would otherwise scope its
victims by whichever home was recorded last and silently spare a pid on the machine that lost power.
Different-host validation and live registration are one admission transaction, so concurrent starts
cannot both publish.

What a reboot keeps is what the *hardware* keeps. The durable image is the disk; the **disk faults** — a
bad sector (`FailDisk`/`FailFile`), a full disk (`LimitDisk`), a slow device (`SlowDisk`) — are physical
properties of the media, not of the dead kernel, so a bad disk stays bad across the crash. Metadata
durability is an **inode** property: once the parent directory's `fsync` makes a file's name durable, the
crash recovers the file with the mode and timestamp it was *created* with, even if the file itself was
never `fsync`ed; a later unsynced `chmod` reverts, like any unsynced change. For the same reason the kernel-state teardown is
keyed by **host** — open file descriptions, descriptors, advisory locks, mappings, sockets, listeners,
and the per-process working directories of that machine — since a proc-keyed sweep would either miss the
root process's resources on the victim or reach its resources on a sibling host (DST-FAULT-VICTIM).

**OOM** is a **process crash** whose *trigger* is the per-process allocation counter crossing a budget
(cgroup-style per-process budget by default; a host-total kernel-OOM with victim selection is a recordable
variant). The budget must sit above the counter's **noise floor** (a few KB of sub-observable non-pooled
allocator noise — pool refills never reach the counter at all, DST-MEMALLOC-DET); a realistic OOM
budget (KB–MB+) does, so the
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
out of model (program discipline, the concurrent dual of the explicit host-capability stance, and the reason
a process is the memory-isolation unit). Within the model a crashed process holds no in-memory resource a
sibling waits on, so abandonment is sound. File locks are modeled as per-process fd-owned resources;
process crash must also release that process's `flock`s — the kernel does on process death — and the
per-process fd table provides that ownership boundary.

**The load-bearing mechanism (contained, non-foreclosing).** synctest tears down a *whole* bubble; the
process-scoped substrate now handles **per-victim teardown** for one process invocation — descheduling its
goids, abandoning their durable blocks without tripping the bubble's deadlock detector (crashed goroutines are
*permanently parked, not deadlocked*), and resetting process-owned resources. Host-wide process enumeration,
host disk tear, and public crash/restart orchestration build on that same substrate.

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
  *Enforced (jitter + throttle + partition + clock-step + clock-drift + disk-EIO + disk-ENOSPC +
  disk-latency classes landed; each further fault class as it lands):* per-fault structural argument + a
  regression test per fault class that the faulted execution is one the real stack can produce. Jitter is a real link degree of freedom (variable latency) that only
  *delays* — bounded to [0, max), never dropping or reordering a live stream (delivery is head-of-line, in
  order, DST-NET-FIFO): `TestDSTNetJitterBounded` / `TestDSTNetJitterFIFO`. Throttle is finite link
  bandwidth, modeled per-flow as an independent B-capacity link (a real dedicated link, so ⊆ real),
  delivering no faster than B (DST-NET-THROTTLE): `TestDSTNetThrottleRate`. Partition only ever blackholes
  then heals — reads block while cut, buffered writes flush in order on heal, never a missing or reordered
  byte on a healed stream: `TestDSTNetPartitionRecover` (`net`), mutation-tested. A **clock step** is a real
  NTP correction (forward or backward); it moves only a host's wall reading, never timer deadlines, so it
  never fires a timer before its base deadline and the only failures it surfaces are real wall-clock ones —
  `TestDSTClockStepBackward` (wall goes backward, the HLC adversary) / `TestDSTClockStepTimerImmune` (a
  pending timer's base firing is unmoved); the wall-derived-duration boundary is recorded contractually
  under "Clock faults". A **clock drift** is a real crystal running fast/slow: its rate is strictly positive
  (DST-CLOCK-DRIFT-MONOTONIC, never a backward-running clock without a step), and a host's own clock stays
  self-consistent (its `d`-timer fires after exactly `d` of its own time) — so the only failures it surfaces
  are the real ones of two nodes' clocks advancing at different rates: `TestDSTClockDriftSelfConsistent` /
  `TestDSTClockDriftMonotonic` / `TestDSTClockDriftRateValidation`. A **disk EIO** is what a real disk
  returns from a `read`/`write`/`fsync` that hits bad media; it is injected only at those calls (an
  infallible `seek`/in-memory `stat` is untouched — `TestDSTDiskEIOInfallibleOpsUnaffected`) and *before*
  any state change, so a faulted write writes nothing and a faulted `fsync` leaves the durable image where
  it was (`TestDSTDiskEIODurabilityPreserved`) — the only failures it surfaces are the real ones of a disk
  returning errors, never a torn durable image. A **disk ENOSPC** is a full disk: a write is failed only
  for the bytes that do not fit (a real disk fills what it can — the rest short-writes then ENOSPCs), a
  create only when the disk is already full, never an in-place overwrite or a read; space in use is summed
  live so a delete frees room — so the only failures it surfaces are the real ones of a disk out of space
  (`TestDSTDiskENOSPCFreesHonored` / `…PartialFill` / `…OverwriteInPlace`). A **disk latency** is a slow
  disk: it only *delays* a disk-touching op (the result is unchanged), never an in-memory op (seek/`Getwd`)
  or a closed-fd EBADF, and the delay sleeps outside the tree lock so it never stalls another host — the
  only behaviours it surfaces are a real slow device's (`TestDSTDiskLatencyInMemoryOpsUnaffected` /
  `…ClosedFdNoDelay` / `…HostIndependence`). A **process crash** is what `kill -9` does: the victim's
  threads stop where they are and never unwind, its fds close and its flocks release (the kernel's
  exit_files), its sockets RST — and the *kernel survives*, so the host filesystem, un-fsync'd bytes
  included, is exactly what a restart reopens. The only failures it surfaces are a real process death's:
  never a torn disk (that is the host crash), never a resumed thread, never a defer that ran
  (`TestDSTCrashAndRestartOverLiveHostFS` pins the surviving unsynced write and the released lock;
  `TestDSTCrashSelf` the no-unwind, no-return self-crash; `TestDSTCrashNestedInvocationParked` that no
  goroutine outlives its process; `TestDSTCrashProcessBlockedInSynctestWait` that a victim holding the
  bubble's waiter registration does not strand the run). A **host crash** is power loss: the machine
  stops, and what remains on its disk is exactly what `fsync` put there — a file's data if the file was
  synced, a name if its parent directory was synced, and neither otherwise, so an unsynced removal is
  *undone* by the crash exactly as it is by a real one (`TestDSTCrashHostRestoresDurableImage`,
  `TestDSTCrashHostResurrectsUnsyncedRemoval`). Dirty shared mappings die with the page cache rather
  than reaching the disk (`TestDSTCrashHostLosesDirtyMappedBytes` — the same store SURVIVES a process
  crash, which is the whole host/process split), and the failures it surfaces are the real ones of a
  machine losing power: never a torn sibling (`TestDSTCrashHostVictimScoping`), never a resource of the
  dead kernel surviving into the reboot (`TestDSTCrashHostRestartFreshResources`,
  `TestDSTCrashHostClosesRootProcessResources`).
- **DST-FAULT-REPLAY (clause-explicit: determinism).** Same seed + same fault configuration (declarative
  set or policy) → identical execution, including which faults fired when. *violation:* a fault decision
  drawn from a load-dependent source (wall clock, per-m RNG) varies run-to-run, breaking replay.
  *Enforced:* the dedicated per-bubble `dstFaultRand` (splitmix64), rooted at activation and re-rooted per
  bubble, **stream-isolated** from the scheduling RNG by a distinct salt so a fault's draw count never
  shifts the interleaving; `TestDSTFaultRandStreamIsolation` (`runtime`: fault draws leave the scheduling
  RNG untouched, and the two roots differ for the same seed) and `TestDSTNetJitterDeterminism` (`net`: same
  seed → identical jittered delivery, varying with the seed), mutation-tested. The **clock step** takes an
  explicit delta rather than a fault draw, so its replay rides the deterministic schedule directly (no RNG):
  `TestDSTClockStepDeterminism` (same seed + same `StepClock` sequence → identical readings). **Clock drift**
  likewise takes an explicit declared rate (the seeded leg is deferred), so the same seed + same `Drift`
  config replays identically: `TestDSTClockDriftDeterminism`. **Disk EIO** is likewise an explicit toggle
  (`FailDisk`/`FailFile`, no fault draw), so the same seed + same fault schedule yields an identical
  outcome sequence: `TestDSTDiskEIODeterminism`; **disk ENOSPC** (`LimitDisk`/`UnlimitDisk`) likewise:
  `TestDSTDiskENOSPCDeterminism`; **disk latency** (`SlowDisk`, an explicit per-op duration) replays the
  same virtual delays: `TestDSTDiskLatencyDeterminism`. (Extends to each further fault class's draws as it
  lands.)
- **DST-FAULT-VICTIM (entailed: attribution integrity).** Every faultable resource is attributed to its
  owning layer — a goroutine/conn/fd to a **process**, a file/tree/port to a **host** — and a fault on
  host hX (or pair {hX,hY}) / process pX affects exactly that victim's resources, no leak onto a
  non-victim. *violation:* a `Partition(h1,h2)` resets a conn between h1 and h3 because attribution
  confused ownership → a failure on h3 the real partition never caused, while the partition itself is
  correct. *Enforced (network axis landed; per later axis as it lands):* `dstHost`/`dstProc` on `g`
  inherited at `newproc1`; a cross-host conn records its host pair (`dstConn.localHost`/`remoteHost`, the
  always-wire end's `localHost`/`peerHost`), which keys the partition table; `TestDSTNetPartitionVictim`
  (`net`) asserts `Partition(A,B)` cuts the A-B conn while an A-C conn is untouched, mutation-tested (a
  check that blocks all pairs once any partition exists fails it). The **process leg** is landed with reset
  (`dstConn.localProc`/`remoteProc`; `TestDSTNetResetVictim` asserts an A-B reset spares an A-C conn,
  `TestDSTNetResetProcess` that `ResetProcess(p)` resets exactly p's conns — both ends). The **clock leg**
  is landed with the step and drift: `StepClock(host)` shifts exactly the named host's per-host offset entry,
  and `Drift(host rate)` sets exactly that host's rate (both keyed by host id), so a fault on hA leaves hB and
  the root untouched — `TestDSTClockStepVictim` and `TestDSTClockDriftVictim` (a rate-2 host A wakes at base
  0.5 s while a rate-1 host B still wakes at 1 s), each mutation-tested (a read that ignores the host id fails
  it). The **disk leg** is landed with EIO: a `dstFile` records its owning host disk and (per-file) its node,
  so `FailDisk(hA)` fails exactly hA's I/O while hB is untouched (`TestDSTDiskEIOVictimHost`) and
  `FailFile(hA,"/x")` fails exactly that file while a sibling reads clean (`TestDSTDiskEIOPerFile`), each
  mutation-tested (a check ignoring the host id, or the node, fails it). The **ENOSPC** capacity is keyed
  the same way, so `LimitDisk(hA, …)` caps exactly hA's disk while hB writes the same data unimpeded
  (`TestDSTDiskENOSPCVictim`), and `SlowDisk(hA, …)` delays exactly hA's ops while hB's identical read is
  instant (`TestDSTDiskLatencyVictim`). The **process-crash leg** keys goroutine death by the victim's
  per-invocation pids and resource death by its logical process id, so `Crash("p")` takes exactly p's
  goroutines, fds, flocks, mappings, conns, and listeners — a host-sibling's lock on the same file node
  and the host filesystem itself are untouched (`TestDSTCrashProcessReleasesFileResources`,
  `TestDSTCrashProcessResetsConnections`, `TestDSTCrashAndRestartOverLiveHostFS`). The **host-crash leg**
  keys goroutine death by process membership or current/inherited entered-`Host` membership, and
  kernel-state death by the host id, so `CrashHost(h)` takes exactly
  h's threads, disk, locks, mappings, sockets, and listeners while a sibling host's unsynced bytes, held
  lock, and connections among other hosts survive (`TestDSTCrashHostVictimScoping`,
  `TestDSTCrashHostClosesRootProcessResources`).
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
- **L3 — faults over the complete substrate.** Network (partition / latency / reset / throttle) — **done**;
  clock **step** — **done**, **drift** (constant `Drift` + mid-run `DriftClock` + seed-drawn `BoundedDrift`)
  — **done**; disk **EIO** / **ENOSPC** / **latency** — **done**; **process crash** (`Crash`, incl. the
  self-crash form) + **restart** over the live host filesystem — **done**; **host crash** (`CrashHost`,
  power loss) + **reboot** onto the restored durable image — **done**;
  scheduling (straggler), OOM (allocation-triggered process crash) — pending.
  Establishes DST-FAULT-SOUND / -REPLAY / -VICTIM enforcement.
- **L4 — orchestration.** The declarative `Options.Faults` + the convenience targeting API; seeded
  exploration (`Options.FaultPolicy`) as the `Explore`/`Failure` fault dimension; `Replay` of a fault set;
  failure shrinking.

The UDP/`PacketConn` net increment (which unlocks packet-granular drop/reorder/duplicate) stays a
net-feature follow-on, not a fault chunk; when it lands, its packet faults reuse this contract.
