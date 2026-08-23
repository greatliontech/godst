# godst — deterministic simulation testing for Go

This fork makes unmodified Go programs run inside a **deterministic
simulation**: virtual time, an in-memory network and filesystem, per-host
clocks and disks, injectable faults, and a schedule that is a pure function
of a seed — so a distributed system's worst afternoons replay exactly, on a
laptop, in milliseconds of wall time.

This page is the user's manual: how to run something under the simulation,
the API tiers, and the testing conventions. The contracts live in
[dst/design.md](dst/design.md) (the model and its soundness invariant),
[dst/faults.md](dst/faults.md) (the distributed model and every fault axis),
and [dst/exploration.md](dst/exploration.md) (schedule exploration).

## Building and running

Simulation support compiles in only under the `dst` build tag, using this
fork as the toolchain — a release tarball extracted to `go/`, or a checkout
built with `src/make.bash`:

```sh
GOROOT=<godst> GOTOOLCHAIN=local <godst>/bin/go test -tags dst ./...
```

Both variables are load-bearing. An exported `GOROOT` naming another
toolchain would redirect this `go` to that toolchain's runtime. Under the
default `GOTOOLCHAIN=auto`, a module whose `go` directive is newer than this
toolchain's version makes the go command fetch and switch to a stock
toolchain, silently bypassing the simulation; `local` forbids the switch.
Releases, the version scheme, and the download assets are specified in
[dst/releases.md](dst/releases.md).

Without `-tags dst` every hook is inert and the toolchain behaves as
upstream Go — godst is upstream Go plus DST patches, and an untagged build
serves as a primary `go` (the contract is design.md's "Untagged footprint
(contract)"). Inside a run, the simulation owns time (`time` reads the fake
clock), the network (`net` TCP is fully in-memory, per-host addressing via
`simulation.HostIP`), and the filesystem (`os` operates on a per-host
in-memory tree with a crash-faithful page cache; the host filesystem is
never visible).

## The API tiers

Everything lives in `testing/simulation`.

**Entry points.** `Run(seed, f)` / `RunWith(seed, opts, f)` execute `f`
inside a simulation; `Test(t, seed, f)` / `TestWith` are the `testing.T`
forms. The seed pins the schedule: same seed, same run, byte for byte.
`Options` carries the run's knobs — network latency/jitter/bandwidth,
send-buffer and retransmit horizons, per-process identity, memory limits.

**Topology.** `Host(name, config, body)` declares a machine (its own
loopback, routable IP, port space, disk, and clock); `Process(name, f)`
declares a crash/restart unit inside it. Re-declaring a POWERED-OFF host is
its reboot — fresh kernel, disk restored to its durable (fsync'd) image, the
body running as the boot; re-declaring a host that is still UP only
re-establishes its clock (processes survive, nothing tears). A power-cycle
is `CrashHost` then re-declaration — exactly what the World layer's
`Ctl.RestartHost` packages.

**The declarative layer.** For the common experiment shape — a fixed fleet,
one driver — declare the topology once:

```go
simulation.World(seed, opts, []simulation.HostDecl{
    {Name: "db1", Boot: bootDB},
    {Name: "db2", Boot: bootDB},
    {Name: "app", Boot: func() {}},
}, func(ctl *simulation.Ctl) {
    // The script: the single goroutine that drives the experiment —
    // exchanges, faults, assertions.
    simulation.Partition("db1", "db2")
    // …
    ctl.RestartHost("db1") // power-cycle: reboots and re-runs bootDB
})
```

Each `Boot` runs at power-on — at declaration and again at every
`Ctl.RestartHost`, against whatever the host's disk durably holds. When the
script returns, the world ENDS: every machine powers off, so long-lived
server goroutines die with their machines and the run exits with no
teardown plumbing. `WorldTest` is the `testing.T` form; `StartWorld` /
`Ctl.End` boot the same declared topology inside any already-running
simulation (most usefully an `Explore` SUT). The layer is pure sugar over
`Host`/`Process` — drop to the imperative core whenever a shape needs it.

**Faults.** Package-level, injected from the script (or any scheduled
goroutine): `Partition`/`PartitionOneWay`/`PartitionRefuse`/`Isolate` and
their heals; `Reset`/`ResetProcess` (injected RSTs); `Crash` (process) and
`CrashHost` (power loss — survivors see *silence*, discovering the death
through retransmission exhaustion, TCP keepalive, or a rebooted kernel's
RST); `FailDisk`/`SlowDisk`/`CorruptFile`/`FailWriteback`;
`StepClock`/`DriftClock`. Every fault is deterministic under the seed. See
[dst/faults.md](dst/faults.md) for each axis's exact contract.

**Exploration.** `Explore(seed, mode, sut)` /
`ExploreWith(seed, opts, sut)` enumerate distinct schedules of one SUT
(DPOR prunes provably-equivalent interleavings), reporting every failing
schedule as a replayable `Failure`; `Replay(seed, failure, sut)` re-executes
one exactly. `ExploreTest(t, seed, opts, sut)` is the test bridge: budgeted
via `ExploreOptions`, skipped under `-short`, every failure printed as a
ready-to-paste `Replay` invocation.

## The convention: pinned seeds in gates, exploration for discovery

Two kinds of simulation tests, two roles:

- **`Test`/`TestWith` with a pinned seed** are REGRESSION tests: fast,
  deterministic, in every gate (CI, pre-commit). They pin behavior a
  specific schedule demonstrates — including every schedule a sweep ever
  caught a bug on.
- **`Explore`/`ExploreTest` sweeps** are DISCOVERY: budgeted schedule
  enumeration, run off the critical path (nightly, idle windows, or locally
  when touching concurrency-sensitive code). They are skipped under
  `-short` by design.

The loop between them: when a sweep fails, `ExploreTest` prints the seed
and schedule as a replayable artifact — **promote every failing seed to a
pinned `Test` regression** (via `Replay` or by reproducing the scenario
seed-pinned), then fix the bug. The sweep finds it once; the pinned test
keeps it dead forever. A failing seed that lives only in a CI log is a bug
report nobody filed.

## Determinism ground rules

A SUT stays deterministic by construction — the simulation owns time,
scheduling, network, disk, and the seeded RNG streams — with a short list
of boundaries the runtime FENCES loudly rather than letting nondeterminism
leak: raw syscalls outside the modeled set, process spawning, cgo, real
fds, host paths. If a run panics with "unsupported under deterministic
simulation", the SUT (or a dependency) touched such a boundary; the panic
names it. `docs/dst/design.md` ("The interception boundary") lists the
modeled surface, which grows by need — a fenced operation a real system
legitimately depends on is a modeling request, not a workaround site.
