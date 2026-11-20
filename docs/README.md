# godst — Deterministic Simulation Testing for Go

`godst` is upstream Go plus a deterministic simulation substrate (`testing/simulation`) that executes unmodified concurrent and distributed Go programs as a **pure, reproducible function of a `uint64` seed**:

- **Deterministic scheduling & randomness:** goroutine scheduling, `select` poll order, value-keyed map iteration, `math/rand`, `math/rand/v2`, `hash/maphash`, and `crypto/rand` derive from per-goroutine seeded streams.
- **Virtual time & per-host clocks:** `time` runs on a `synctest` virtual clock advancing instantly at durable quiescence, with per-host static/seeded clock skew, crystal drift, and mid-run NTP steps.
- **In-memory TCP & UDP network:** per-host loopback (`127.0.0.1` / `::1` / `localhost`) and routable `10.0.0.<id>` full-mesh TCP and UDP (`*net.UDPConn`, `ListenPacket`, `ListenUDP`, `DialUDP`) with independent per-host TCP/UDP port spaces, configurable latency, jitter, bandwidth throttling, UDP packet loss/duplication/reorder (`LinkConfig`), TCP send-buffer backpressure, half-close (`CloseWrite`/`CloseRead`), keepalive, `TCP_USER_TIMEOUT`, `SO_REUSEADDR`, UDP `SO_REUSEPORT` load-balancing groups, `SO_RCVBUF`/`SO_SNDBUF`, connected-UDP `ECONNREFUSED`, partitions, and connection resets.
- **Crash-faithful in-memory storage & VM:** per-host sparse 4 KiB page-cache filesystems (`os`, `os.Root`), `MAP_SHARED` `mmap` coherent with `read`/`write`, `flock`, `fallocate`, `SEEK_DATA`/`SEEK_HOLE`, shared `futex`, power-loss durability tears (`CrashHost` + `CrashTear`), `fsyncgate` writeback failures, `ENOSPC` quotas, disk latency, and platter bit-rot.
- **Multi-host / multi-process isolation:** independent per-`(host, boot)` PID spaces, `PIDNamespace` isolation, `/proc/<pid>/stat` + `boot_id`, per-invocation file/socket/environment ownership, and deterministic STW garbage collection.
- **Systematic schedule exploration:** Level-2 memory-access-granularity preemption and source-DPOR (`Explore`, `ExploreWith`, `ExploreTest`, `Replay`) paired with the `-race` detector as a deterministic data-race oracle.

This manual covers building, running, the complete `testing/simulation` public API, idiomatic usage patterns, modeled standard-library surfaces, and determinism rules. Companion specifications live in:

- [dst/design.md](dst/design.md) — top-tier soundness/zero-deviation contracts, runtime hooks, network/filesystem/pipe models, and interception fences.
- [dst/faults.md](dst/faults.md) — Universe / Host / Process distributed model and every fault axis.
- [dst/exploration.md](dst/exploration.md) — `Random` / `PCT` scheduling strategies and Level-2 DPOR + race oracle.
- [dst/gc.md](dst/gc.md) — deterministic stop-the-world GC and per-invocation finalizer/cleanup drain.
- [dst/releases.md](dst/releases.md) — upstream porting, versioning, release tarballs, and toolchain installation.

---

## 1. Building, Installing, and Running

### Toolchain installation

`godst` serves as both your **primary `go` toolchain** (untagged builds emit identical code and type shapes to stock upstream Go) and your **simulation toolchain** (activated with `-tags dst`).

From a clone (requires `task` and an existing Go bootstrap toolchain):

```sh
task install
```

This builds a packaged distribution, validates both untagged and `-tags dst` execution in an isolated environment, installs it under `~/.local/godst/<version>` (linked at `~/.local/godst/current`), and writes `~/.local/bin/go` and `~/.local/bin/gofmt` with `GOTOOLCHAIN=local` baked in and `GOROOT` cleared.

When invoking an extracted tarball or repo build directly without the installed launcher, always pin both `GOROOT` and `GOTOOLCHAIN=local`:

```sh
GOROOT=/path/to/godst GOTOOLCHAIN=local /path/to/godst/bin/go test -tags dst ./...
```

- `GOROOT` prevents an ambient `GOROOT` in your shell from redirecting `godst` to another toolchain's standard library.
- `GOTOOLCHAIN=local` prevents `cmd/go` from auto-downloading a stock upstream toolchain when a module specifies a newer `go` directive, which would silently bypass simulation hooks.

### Build modes

`testing/simulation` exports the **exact same API surface** in untagged and `-tags dst` builds, so test files importing `testing/simulation` compile cleanly in every build mode without requiring `//go:build dst` tags unless you want to skip compiling DST-only suites in plain runs.

| Build command | Behavior | Typical use |
|---|---|---|
| `go build ./...` / `go test ./...` | **Zero-deviation stock Go.** All runtime/stdlib simulation hooks compile out completely (`INV-VANILLA` / `INV-TYPESHAPE`). Calling `simulation.Run` / `Test` / `World` / `Explore` / `Replay` panics loudly naming `-tags dst`. | Production binaries, ordinary unit tests, benchmarks. |
| `go test -tags dst ./...` | **Deterministic simulation enabled** (Linux on `amd64`, `arm64`, `386`, `arm`, `ppc64`, `ppc64le`, `riscv64`, `s390x`). Process-global map hash seed fixed; virtual clock, deterministic scheduler, simulated TCP/FS/identity, and fault injection active inside simulation runs. | Fast pinned-seed regression gates (`Test`, `WorldTest`) and multi-seed `Random`/`PCT` sweeps. |
| `go test -tags dst -race ./...` | **Simulation + race oracle + Level-2 DPOR.** Preserves same-seed schedule determinism while checking happens-before races via TSan and enabling memory-access-granularity yield points for `Explore` / `ExploreTest`. | Race-checked simulation gates and systematic `DPOR` exploration sweeps. |

---

## 2. Quick Start: Three Canonical Usage Patterns

### Pattern 1 — Crash-consistency & recovery (`TestWith` + `Host` + `Process`)

Crashable hosts and processes run their workloads on child goroutines (`go simulation.Process(...)`) while the test's main bubble goroutine drives operations, injects faults, reboots hosts, and asserts state:

```go
package storage_test

import (
	"bytes"
	"io/fs"
	"os"
	"testing"
	"testing/simulation"
)

func TestWALPowerLossRecovery(t *testing.T) {
	opts := simulation.Options{
		CrashTear: true, // explore page-granular torn/unsynced writeback on power loss
	}
	for seed := uint64(1); seed <= 32; seed++ {
		simulation.TestWith(t, seed, opts, func(t *testing.T) {
			committed := make(chan struct{})

			// Boot 1: write a synced record and an unsynced tail on host "db".
			simulation.Host("db", func() {
				go simulation.Process("engine", func() {
					f, err := os.OpenFile("/wal.log", os.O_CREATE|os.O_RDWR, 0o644)
					if err != nil {
						panic(err)
					}
					if _, err := f.Write([]byte("commit-1\n")); err != nil {
						panic(err)
					}
					if err := f.Sync(); err != nil {
						panic(err)
					}
					dir, err := os.Open("/")
					if err != nil {
						panic(err)
					}
					_ = dir.Sync()
					_ = dir.Close()

					// Unsynced tail write — may tear or vanish on power loss.
					_, _ = f.Write([]byte("uncommitted-2\n"))
					close(committed)
					select {} // stay alive until power loss
				})
			})

			<-committed
			simulation.CrashHost("db") // abrupt power loss: no defers run, page cache tears

			// Inspect durable platter directly from the harness before or after reboot:
			// Reboot "db" by re-declaring Host("db", ...) after CrashHost.
			recovered := make(chan []byte, 1)
			simulation.Host("db", func() {
				go simulation.Process("engine", func() {
					data, err := os.ReadFile("/wal.log")
					if err != nil {
						panic(err)
					}
					recovered <- data
				})
			})

			got := <-recovered
			if !bytes.HasPrefix(got, []byte("commit-1\n")) {
				t.Fatalf("seed %d: durable prefix lost after power loss: %q", seed, got)
			}

			// Read-only cross-host assertion via HostFS:
			snap, err := fs.ReadFile(simulation.HostFS("db"), "wal.log")
			if err != nil || !bytes.Equal(snap, got) {
				t.Fatalf("HostFS mismatch: %q, err=%v", snap, err)
			}

			// Power off long-lived server before exiting the simulation bubble.
			simulation.CrashHost("db")
		})
	}
}
```

### Pattern 2 — Multi-node cluster & fault scripting (`WorldTest`)

For a fixed fleet of machines with long-lived background servers, `World` and `WorldTest` declare each node's `Boot` callback once, run a driver `script(t, ctl)` goroutine, support one-call power-cycling via `ctl.RestartHost(name)`, and automatically power off all machines when the script returns:

```go
package cluster_test

import (
	"net"
	"testing"
	"testing/simulation"
	"testing/synctest"
	"time"
)

func TestClusterPartitionAndReboot(t *testing.T) {
	opts := simulation.Options{
		CrashTear: true,
		Network: simulation.NetworkConfig{
			CrossHost: simulation.LinkConfig{
				Latency: 5 * time.Millisecond,
				Jitter:  2 * time.Millisecond,
			},
		},
	}

	bootNode := func(name string) func() {
		return func() {
			go simulation.Process("raftd", func() {
				ln, err := net.Listen("tcp", ":9000")
				if err != nil {
					return
				}
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					_, _ = c.Write([]byte(name))
					_ = c.Close()
				}
			})
		}
	}

	hosts := []simulation.HostDecl{
		{Name: "n1", Config: simulation.HostConfig{Clock: simulation.BoundedSkew(20 * time.Millisecond)}, Boot: bootNode("n1")},
		{Name: "n2", Config: simulation.HostConfig{Clock: simulation.BoundedSkew(20 * time.Millisecond)}, Boot: bootNode("n2")},
		{Name: "n3", Config: simulation.HostConfig{Clock: simulation.BoundedSkew(20 * time.Millisecond)}, Boot: bootNode("n3")},
	}

	simulation.WorldTest(t, 42, opts, hosts, func(t *testing.T, ctl *simulation.Ctl) {
		// Let spawned Process goroutines bind :9000 and reach Accept() in 0ns of virtual time.
		synctest.Wait()

		// Isolate n1 and let virtual time advance across the blackhole cut:
		simulation.IsolateHost("n1")
		if c, err := net.DialTimeout("tcp", simulation.HostIP("n1")+":9000", 50*time.Millisecond); err == nil {
			_ = c.Close()
			t.Fatal("expected dial to isolated n1 to time out")
		}
		simulation.UnisolateHost("n1")

		ctl.RestartHost("n2") // CrashHost("n2") + re-run n2's Boot against durable disk
		synctest.Wait()       // settle rebooted n2's listener in 0ns

		// Dial healed n1 and rebooted n2 by their deterministic routable IPs:
		for _, target := range []string{"n1", "n2"} {
			c, err := net.DialTimeout("tcp", simulation.HostIP(target)+":9000", time.Second)
			if err != nil {
				t.Fatalf("dial %s: %v", target, err)
			}
			_ = c.Close()
		}
		// Returning from script automatically calls ctl.End(), powering off n1..n3 cleanly.
	})
}
```

### Pattern 3 — Systematic DPOR discovery & pinned replay (`ExploreTest` + `Replay`)

Build and run discovery sweeps with `-tags dst -race` (`ExploreTest` automatically skips under `-short`). When `ExploreTest` discovers an assertion error, panic, deadlock, or data race, it prints a ready-to-paste `simulation.Replay(...)` call that you promote into a fast pinned regression test:

```go
package queue_test

import (
	"errors"
	"sync"
	"testing"
	"testing/simulation"
)

func sutQueue() error {
	var mu sync.Mutex
	items := 0
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			items++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if items != 2 {
		return errors.New("lost increment")
	}
	return nil
}

// Discovery sweep (runs when -short is NOT passed; best with -tags dst -race).
func TestExploreQueue(t *testing.T) {
	simulation.ExploreTest(t, 1, simulation.ExploreOptions{
		Mode:         simulation.DPOR,
		MaxSchedules: 500,
	}, sutQueue)
}

// Pinned regression test promoted from an ExploreTest failure report.
func TestReplayQueueRegression(t *testing.T) {
	res := simulation.Replay(1, simulation.Failure{
		Schedule: []uint64{1, 2, 3, 2, 3, 1},
	}, sutQueue)
	if res.Failed() {
		t.Fatalf("replay failed: err=%v raced=%v", res.Err, res.Raced)
	}
}
```

### Controlling Virtual Time & Quiescence (`time.Sleep` vs `synctest.Wait`)

`testing/simulation` deliberately exposes no custom `Advance(d)` or `Tick()` API because Go's standard `time.Sleep(d)` and `testing/synctest.Wait()` already drive the virtual clock directly inside every simulation bubble:

1. **`0ns` CPU execution rule:** All non-blocking Go statements execute in **`0ns` of virtual time**. The virtual clock advances *only* at **durable quiescence** — when every live goroutine in the simulation bubble is durably parked (waiting on channels, `select`, timers/tickers, in-bubble `sync.Mutex`/`RWMutex`/`WaitGroup`/`Cond`/`Once`, simulated TCP/pipe I/O, `SlowDisk`, or simulated `futex`). Consequently, calling `IsolateHost("n1")` immediately followed by `UnisolateHost("n1")` with no blocking wait in between toggles and clears the fault at the exact same virtual nanosecond before any timer or peer wakes.
2. **Advancing cluster virtual time (`time.Sleep(d)`):** Calling `time.Sleep(d)` (or waiting on a deadline-bearing operation such as `net.DialTimeout` or `context.WithTimeout`) from your root driver script parks the driver for `d` of universe base virtual time. As soon as runnable work settles, the runtime steps the virtual clock **chronologically** through every intermediate deadline in `[now, now + d]` — firing Raft heartbeat/election tickers, delivering in-flight TCP segments (`NetworkConfig.CrossHost` / `ConfigureLink` latency + jitter + bandwidth), expiring retransmit/keepalive horizons, and completing `SlowDisk` operations in exact causal timestamp order, all in microseconds of real wall-clock time:
   ```go
   simulation.IsolateHost("n1")
   time.Sleep(3 * time.Second) // 3s of cluster timers, RPC retries, & elections run chronologically
   simulation.UnisolateHost("n1")
   ```
3. **Settling `0ns` work without advancing virtual time (`synctest.Wait()`):** Calling `synctest.Wait()` parks the caller until every other live goroutine in the bubble is durably blocked, advancing **`0ns`** of virtual time. Use `synctest.Wait()` when you want newly spawned `go simulation.Process(...)` goroutines to finish binding listeners, `0ms` loopback pipelines to drain, or immediate `CrashProcess` `RST` notifications to propagate without firing future timers.
4. **Driver clock domain:** The root test/script goroutine runs on the unnamed root host (`rate = 1`, `skew = 0`), so `time.Sleep(d)` on the driver always measures exact universe base virtual time regardless of `Skew` or `Drift` configured on cluster `Host`s (see [dst/faults.md](dst/faults.md)).

---

## 3. Complete `testing/simulation` Public API Reference

Package import path: `import "testing/simulation"`

Simulation operations (`Run`, `RunWith`, `Test`, `TestWith`, `World`, `WorldTest`, `Explore`, `ExploreWith`, `ExploreTest`, `Replay`) are **process-global and non-overlapping**: they must not be nested, run concurrently (`t.Parallel()` across simultaneous runs in one process), or called from inside an existing `testing/synctest` bubble. Every rejected invocation panics cleanly without mutating active simulation state.

### 3.1 Simulation Entry Points & Run Configuration

```go
func Run(seed uint64, f func())
func RunWith(seed uint64, opts Options, f func())
func Test(t *testing.T, seed uint64, f func(*testing.T))
func TestWith(t *testing.T, seed uint64, opts Options, f func(*testing.T))
```

- **`Run` / `RunWith`** execute `f` inside a deterministic simulation bubble seeded by `seed`. For the duration of the call, `GOMAXPROCS` is pinned to `1` (attempts to resize via `runtime.GOMAXPROCS` inside the run are ignored and restored on return), asynchronous preemption is disabled, GC runs deterministically stop-the-world with synchronous sweep, and two quiet `sync.Pool` generations are drained before bubble teardown so bubble-stamped objects never leak into subsequent runs.
- **`Test` / `TestWith`** wrap `RunWith` with a bubble-scoped `*testing.T` (matching `testing/synctest.Test` rules: `t.Cleanup` and `t.Context()` run inside the bubble; do not call `t.Run`, `t.Parallel`, or `t.Deadline` on the inner `*testing.T`). If the test fails, panics, or deadlocks, the chronological failure-only causal event timeline (up to the last 256 topology, fault, TCP/UDP lifecycle/timeout/reset/drop, and filesystem fault-hit transitions), activated `Sometimes` fault sites, abnormal process-exit goroutine kill diagnostics, and captured in-bubble `stdout`/`stderr` output are automatically formatted and logged to `t`.

#### `Strategy`

```go
type Strategy int

const (
	Random Strategy = iota // Uniformly-random sound interleaving per seed (default)
	PCT                    // Probabilistic Concurrency Testing priority-change scheduling
)
```

- **`Random`** chooses uniformly among runnable simulation goroutines at each scheduling point using the run's dedicated scheduler RNG.
- **`PCT`** assigns random priorities to goroutines with `Options.Depth` priority-change points placed across `Options.Steps` decisions, providing higher probabilistic yield for deep ordering bugs across seed sweeps.

#### `Options`

```go
type Options struct {
	Strategy           Strategy
	Depth              int           // PCT bug depth in [1, 16]; <= 0 selects default 3 (panics if > 16)
	Steps              int           // PCT step horizon; <= 0 selects default 1000
	Hostname           string        // Root-host os.Hostname(); "" selects default "sim"
	PID                int           // Root-host init/driver os.Getpid(); <= 0 selects default 1
	PIDMax             int           // Per-(host, boot) kernel pid_max wrap bound; 0 = unbounded monotonic
	NumCPU             int           // Default runtime.NumCPU() for hosts; <= 0 selects default 8
	MemoryLimit        int64         // Bubble-local heap growth soft GC trigger in bytes; 0 = GOGC default
	Network            NetworkConfig // Simulated TCP/UDP latency, jitter, bandwidth, datagram faults, buffer, and timeout policy
	CrashTear          bool          // Enable seeded page/entry/size power-loss tears on CrashHost
	WedgeDecisionLimit int64         // Max decisions without durable quiescence; 0 = default 1<<26, < 0 disables
	WedgeWallLimit     time.Duration // Max wall time without a scheduler decision; 0 = default 60s, < 0 disables
}
```

Key `Options` details:

- **Fixed deterministic OS identity:** Inside a bubble, `os.Getppid()` is `1`, `os.Getuid()`/`Geteuid()`/`Getgid()`/`Getegid()` are `7777`, `os.Getgroups()` is `[7777]`, and `os/user.Current()` reports user `"sim"` (`uid=7777`, `gid=7777`, `HomeDir="/home/sim"`). Non-bubble goroutines in the same test binary continue to observe real host identity.
- **`PIDMax`:** When `0` (default), each `(host, boot)` allocates PIDs monotonically starting at `PID + 1` (`2` by default). When `> 0` (must exceed `PID` and fit `int32`), allocation wraps into `[2, PIDMax]` and skips live PIDs on that `(host, boot)`, modeling Linux `alloc_pid` wraparound so `/proc/<pid>/stat` starttime discrimination against PID reuse can be tested deterministically. If every PID in `[2, PIDMax]` is simultaneously live on a host, `Process` panics (`fork` `EAGAIN` equivalent).
- **Wedge detection:** Virtual time advances only when all live bubble goroutines are durably blocked. `WedgeDecisionLimit` catches busy-spin loops that yield (`for { runtime.Gosched() }`), and `WedgeWallLimit` catches call-free tight loops (`for !done.Load() {}`) that never enter the scheduler. Neither perturbs healthy schedules.
- **Refused ambient modes:** `GODEBUG=fips140=on|only|debug` and `GOEXPERIMENT=arenas` panic on entry because they bypass deterministic entropy/allocation hooks.

#### `NetworkConfig` & `LinkConfig`

```go
type LinkConfig struct {
	Latency         time.Duration // One-way base virtual delay (0 = 0ns delay)
	Jitter          time.Duration // Max extra seeded per-segment/packet delay in [0, Jitter) (0 = disabled)
	Bandwidth       int64         // Per-flow per-direction throughput limit in bytes/sec (0 = unlimited)
	PacketLoss      float64       // Cross-host UDP packet drop probability in [0, 1] (0 = disabled; ignored by TCP)
	PacketDuplicate float64       // Cross-host UDP packet duplication probability in [0, 1] (0 = disabled; ignored by TCP)
	PacketReorder   bool          // Allow jittered cross-host UDP datagrams to arrive out of order (ignored by TCP)
}

type NetworkConfig struct {
	CrossHost         LinkConfig    // Baseline cross-host link policy (default 0s; loopback/same-host always instant)
	SendBuffer        int           // Per-direction TCP send buffer in bytes (0 = 1 MiB default, < 0 = unbounded)
	RetransmitTimeout time.Duration // Retransmission death horizon for cut/unanswered traffic (0 = 2m default, < 0 = disabled)
}
```

- Cross-host TCP `net.Dial` pays a full 1-RTT (`SYN` + `SYN-ACK`) before completing, and `Listener.Accept` yields connections only once `ESTABLISHED`. Cross-host UDP datagrams traverse each target host's deterministic in-flight min-heap and bounded `SO_RCVBUF` receive queue (`256 KiB` default), dropping mid-flight datagrams across partitions/power-loss and delivering transient one-shot `ECONNREFUSED` on connected UDP sockets targeting unbound ports on live hosts.
- Specific host pairs can override `NetworkConfig.CrossHost` symmetrically or directionally via `ConfigureLink`, `ConfigureLinkOneWay`, and `UnconfigureLink`. Negative `Latency`, `Jitter`, or `Bandwidth`, `Jitter > 0` where `Latency + (Jitter - 1)` overflows `time.Duration`, or `PacketLoss` / `PacketDuplicate` outside `[0, 1]` (including `NaN`) panics state-neutrally before publishing or mutating run state.
- All link delays are measured in universe **base virtual time**, unaffected by per-host wall-clock skew.

---

### 3.2 Distributed Topology: Hosts, Processes, Clocks, & Inspection

`godst` models three isolation layers:

1. **Universe (per-run):** scheduler, base virtual clock, Go heap/GC, seeded RNG streams, and inter-host network mesh.
2. **Host (per-machine, up to 4096 per run):** filesystem tree + page cache + durable platter, routable IP (`10.0.0.<id>`), private loopback (`127.0.0.1` / `::1` / `"localhost"`), independent TCP and UDP port spaces, wall/drift clock, `os.Hostname()`, `runtime.NumCPU()`, `/proc/sys/kernel/random/boot_id`, and per-`(host, boot)` PID & `PIDNamespace` tables.
3. **Process (`(host, name)`, up to 4096 logical processes per run):** crash/restart unit with cumulative heap-allocation accounting across restarts (`g.dstProc`) plus a **unique per-invocation resource slot** (`g.dstProcSlot`) owning `os.Getpid()`, open `*os.File` handles and virtual FDs, `flock` advisory locks, `mmap` mappings, working directory (`Chdir`/`Getwd`), copy-on-write environment (`Getenv`/`Setenv`/`Environ`), TCP listeners/sockets, UDP sockets, and GC finalizers/cleanups.

#### Hosts

```go
func Host(name string, f func())
func HostWith(name string, config HostConfig, f func())

type HostConfig struct {
	Clock    ClockConfig // Wall-clock skew and/or crystal drift; zero value = synced with base clock
	Hostname string      // Overrides os.Hostname(); "" defaults to the declared host name
	NumCPU   int         // Overrides runtime.NumCPU(); <= 0 uses Options.NumCPU (default 8)
}
```

- `name` must be **non-empty** (empty `name` always panics; the unnamed root host `""` / ID `0` is the default environment outside any `Host` block).
- `Host` / `HostWith` stamp the calling goroutine with that machine's identity for the duration of `f()`, and every goroutine spawned inside `f()` inherits that host identity even after `f()` returns.
- **Re-declaration & reboot semantics:**
  - Calling `Host` / `HostWith` for a host that is **powered off** (first declaration, or first declaration after `CrashHost(name)`) **boots** the machine: increments its kernel boot counter, rotates `/proc/sys/kernel/random/boot_id`, resets its monotonic uptime and PID allocator, restores its filesystem from durable storage, brings its network stack up, and establishes `config.Clock`.
  - Calling `Host` / `HostWith` for a host that is **already up** leaves running processes, sockets, and filesystem state intact and re-establishes its clock configuration (`config.Clock`).

#### Host Clocks (`ClockConfig`)

```go
type ClockConfig struct { /* unexported fields */ }

func Skew(offset time.Duration) ClockConfig
func BoundedSkew(bound time.Duration) ClockConfig
func Drift(ppb int64) ClockConfig
func BoundedDrift(maxPPB int64) ClockConfig

func (c ClockConfig) WithDrift(ppb int64) ClockConfig
func (c ClockConfig) WithBoundedDrift(maxPPB int64) ClockConfig
```

- **`Skew(offset)`** / **`BoundedSkew(bound)`**: fixed wall-clock offset, or deterministic seed-derived offset in `[-bound, +bound]` per host (stable across reboots). Shifts `time.Now()` wall readings without altering relative timer durations. Panics if the resulting initial wall time precedes the Unix epoch.
- **`Drift(ppb)`** / **`BoundedDrift(maxPPB)`**: crystal rate departure in parts-per-billion (`rate = 1 + ppb/1e9`). `Drift` requires `ppb` in `(-1e9, 1e9]` (`rate` in `(0, 2]`); `BoundedDrift` requires `maxPPB` in `[0, 1e9)`. Scales both `time.Now()`/`time.Since()` wall progression and relative timer/ticker deadlines coherently (`Sleep(d)` never wakes early in the host's own perceived clock).
- Skew and drift compose fluently: `simulation.Skew(25 * time.Millisecond).WithBoundedDrift(500_000)` (±500 ppm drift + +25ms skew).

#### Processes

```go
func Process(name string, f func())
func ProcessWith(name string, config ProcessConfig, f func())

type ProcessConfig struct {
	PIDNamespace string // Optional per-(host, boot) sibling PID namespace; "" = host's root namespace
}
```

- `name` must be **non-empty** (empty `name` always panics).
- **Host placement:** A `Process` declared inside a `Host` / `HostWith` body runs on that host. A `Process` declared outside any `Host` runs on the unnamed root host (`host == ""` / ID `0`) — it **never** creates or attaches to a named host implicitly. After `CrashHost(name)` powers off a machine, re-enter `Host(name, ...)` / `HostWith(name, ...)` to reboot the machine and start fresh `Process` invocations on the new boot.
- **Per-host naming:** Process names are scoped per `(host, name)`, so running `Process("server", ...)` on `"n1"`, `"n2"`, and `"n3"` creates three independent logical processes with zero name collision.
- **Crashable process rule:** Always launch crashable processes on their own goroutine (`go simulation.Process("server", fn)`). Declaring `Process` inline on the simulation's main driver goroutine makes that invocation own the driver; `CrashProcess` and `CrashHost` refuse (panic before tearing anything down) if killing the target would destroy the simulation's main goroutine.
- **Process exit semantics:** When `f()` returns (or unwinds via panic), the process **exits**:
  1. Any child goroutines spawned inside `f()` that are still alive are permanently stopped (and recorded in the off-heap exit-kill diagnostic ledger if the test fails).
  2. That invocation slot's open files, `flock`s, `mmap`s, TCP listeners, and UDP sockets close immediately (even if another overlapping invocation of the same process name is running).
  3. Open TCP sockets close with Linux `close(2)` semantics: if unread bytes sit in the socket's receive buffer, the peer receives `RST` (`ECONNRESET` after draining already-delivered bytes); otherwise `FIN` (`io.EOF`).
  4. Pending GC finalizers/cleanups registered by that invocation are discarded, and its PID disappears from `Kill(pid, 0)` and `/proc/<pid>/stat`.
- **`PIDNamespace`:** Interned per `(host, boot)`. Processes in distinct namespaces on the same host (or on distinct hosts/boots) cannot see each other via `syscall.Kill(pid, 0)` (`ESRCH`) or `/proc/<pid>/stat` (`os.ErrNotExist`), while co-pod processes sharing a `PIDNamespace` on the same host boot can probe each other's liveness and `starttime`.

#### Inspection & Host File Capabilities

```go
func HostIP(name string) string
func HostFS(name string) fs.FS
func InheritFile(file *os.File) (*os.File, error)
```

- **`HostIP(name)`**: returns declared host `name`'s deterministic routable IPv4 string (`"10.<b>.<c>.<d>"`, e.g. `"10.0.0.1"` for the first declared host). Panics during an active run if `name` has not been declared or if called from a foreign goroutine outside the simulation's bubble; outside an active run it never panics (returning `""` in untagged builds).
- **`HostFS(name)`**: returns an epoch-fenced, read-only `io/fs.FS` (implementing `fs.StatFS` and `fs.ReadDirFS`) over declared host `name`'s live in-memory filesystem for out-of-band test assertions without running code inside that host or allocating simulation inodes. Using a `HostFS` handle after its run ends returns `fs.ErrClosed`.
- **`InheritFile(file)`**: grants the **root** simulation body (outside any `Host` or `Process`) capability-scoped access to an explicit real host `*os.File` on Linux (for example, writing a structured artifact or reading a fixture intentionally). Returns a duplicated `*os.File` whose numeric `Fd()` and `SyscallConn()` remain fenced; closing the returned capability does not close `file`.

---

### 3.3 Declarative Worlds (`World`, `WorldTest`, `StartWorld`, `Ctl`)

```go
type HostDecl struct {
	Name   string
	Config HostConfig
	Boot   func() // Must be non-nil; runs inside HostWith(Name, Config, Boot) at start and on RestartHost
}

type Ctl struct { /* unexported fields */ }

func World(seed uint64, opts Options, hosts []HostDecl, script func(*Ctl))
func WorldTest(t *testing.T, seed uint64, opts Options, hosts []HostDecl, script func(*testing.T, *Ctl))
func StartWorld(hosts []HostDecl) *Ctl

func (c *Ctl) RestartHost(name string)
func (c *Ctl) End()
```

- **`World` / `WorldTest`** boot every `HostDecl` in slice order (panicking on empty/duplicate names or `Boot == nil`), run `script` on the root simulation goroutine, and `defer ctl.End()` to power off all declared hosts in reverse order on return so background server goroutines need no manual shutdown channels.
- **`StartWorld(hosts)`** boots a `[]HostDecl` inside an already-running `Run`, `Test`, or `Explore` SUT (`defer ctl.End()`). Must be called from outside any declared host's `Host` body.
- **`(*Ctl).RestartHost(name)`** performs a full power-cycle (`CrashHost(name)` followed by `HostWith(d.Name, d.Config, d.Boot)`), rebooting the node's software against its durable disk image. Works whether the host was live or already crashed.
- **`(*Ctl).End()`** idempotently calls `CrashHost` on all declared hosts in reverse declaration order.

---

### 3.4 Fault Injection API

All external fault-mutator functions (network, crash, disk, and clock faults) follow two uniform safety rules — whereas inline `Sometimes` hooks embedded inside SUT library code return `false` outside the active bubble (see below):

1. **Scheduled bubble caller required:** During an active run, external fault mutators must be called from a goroutine inside the simulation bubble (the driver script or a simulated goroutine); calling from a foreign background goroutine panics loudly so wall-clock races never sneak into a run. Outside an active run, all external fault functions are safe no-ops (after validating immediate scalar/struct bounds such as `LinkConfig`, `DriftClock`, `LimitDisk`, and `SlowDisk`).
2. **Loud victim validation:** Passing an undeclared `host` or `proc` name panics immediately so typo'd fault targets never pass tests vacuously. `CrashProcess(host, proc)` additionally panics if `(host, proc)` has no currently live invocation, whereas `ResetProcessConns(host, proc)` is a valid 0-connection no-op when a previously declared `(host, proc)` has already exited. For both `CrashProcess` and `ResetProcessConns`, pass `host == ""` to target a `Process` declared on the unnamed root host.

#### Network Faults & Per-Link Policy

```go
func Partition(a, b string)
func PartitionOneWay(from, to string)
func PartitionRefuse(a, b string)
func HealLink(a, b string)

func IsolateHost(host string)
func UnisolateHost(host string)

func ConfigureLink(a, b string, cfg LinkConfig)
func ConfigureLinkOneWay(from, to string, cfg LinkConfig)
func UnconfigureLink(a, b string)

func ResetLinkConns(a, b string)
func ResetProcessConns(host, proc string)
```

- **`Partition(a, b)`**: symmetric blackhole cut between hosts `a` and `b`. New TCP `Dial`s block (until heal, deadline, or `RetransmitTimeout` `ETIMEDOUT`). Established TCP connections still read bytes delivered *before* the cut, while in-flight and post-cut TCP writes buffer up to `SendBuffer` and flush in FIFO order upon `HealLink(a, b)` (unless `RetransmitTimeout` or keepalive kills the socket first). Cross-host UDP datagrams that arrived before the cut remain readable, while mid-flight and post-cut UDP datagrams (and mid-flight ICMP refusals) are permanently dropped.
- **`PartitionOneWay(from, to)`**: asymmetric blackhole cut dropping `from → to` while `to → from` continues to flow (new TCP handshakes fail in both directions because TCP requires both `SYN` and `SYN-ACK`; UDP drops only `from → to`).
- **`PartitionRefuse(a, b)`**: symmetric cut where new TCP `Dial`s fail immediately with `syscall.ECONNREFUSED` ("peer down" RST) and connected UDP sends pend a 1-RTT one-shot `syscall.ECONNREFUSED` unless a blackhole/isolation cut also covers the link (blackhole dominates refuse).
- **`HealLink(a, b)`**: clears `Partition`, `PartitionOneWay`, and `PartitionRefuse` between `a` and `b` (does not clear `IsolateHost` or per-link `ConfigureLink*` policies).
- **`IsolateHost(host)` / `UnisolateHost(host)`**: cuts / restores blackhole connectivity between `host` and every other host.
- **`ConfigureLink(a, b, cfg)` / `ConfigureLinkOneWay(from, to, cfg)` / `UnconfigureLink(a, b)`**: installs a symmetric (`a ↔ b`) or directional (`from → to`) `LinkConfig{Latency, Jitter, Bandwidth, PacketLoss, PacketDuplicate, PacketReorder}` policy overriding `Options.Network.CrossHost`, or removes overrides in both directions so `a ↔ b` reverts to `Options.Network.CrossHost`. A configured `LinkConfig` replaces the directional policy completely (`0` / `false` means `0` delay / `0` jitter / unlimited bandwidth / lossless in-order delivery, modeling fast intra-AZ links inside a WAN baseline as well as slow WAN or lossy gray-failure links inside a fast baseline). Overrides apply dynamically to new TCP `Dial` handshakes (`SYN` on `dialer → target`, `SYN-ACK` on `target → dialer`), subsequent outbound TCP `Write` / `CloseWrite` / graceful `Close` FIN segments (ignoring `PacketLoss`/`PacketDuplicate`/`PacketReorder` to preserve `DST-NET-FIFO`), and outbound cross-host UDP datagrams; same-host pairs remain instant and lossless, and per-link policies survive both `HealLink` and `CrashHost`.
- **`ResetLinkConns(a, b)` / `ResetProcessConns(host, proc)`**: injects TCP `RST` on every active connection between `a` and `b` (or owned by live invocations of `(host, proc)`), ordered deterministically by connection registration sequence. Already-delivered bytes drain first, followed by one-shot `ECONNRESET` (subsequent reads `io.EOF`, writes `EPIPE`); in-flight bytes are dropped.

#### Crash & Power-Loss Faults

```go
func CrashProcess(host, proc string)
func CrashHost(name string)
```

| Property | `CrashProcess(host, proc)` (`kill -9`) | `CrashHost(name)` (power loss / kernel panic) |
|---|---|---|
| **Goroutines** | Descheduled permanently (no `defer`s run; stack roots dropped for GC); self-crash never returns. | All processes *and* root-process goroutines on `name` descheduled permanently; self-crash never returns. |
| **FDs, flocks, `mmap`s** | Victim's FDs and TCP/UDP sockets close, `flock`s release, shared mappings unregister. | All FDs, `flock`s, mappings, TCP listeners, UDP sockets + in-flight queue, and root-host env/cwd on `name` wiped. |
| **Filesystem / Page cache** | **Intact** — kernel survives; unsynced writes and dirty `MAP_SHARED` bytes remain visible on `host`. | **Tears to durable image** — reverts to `fsync`'d file contents and `fsync`'d parent directory entries (or page-granular subset under `Options.CrashTear`). |
| **Open TCP/UDP peers** | Live kernel emits TCP `RST` (peers drain then `ECONNRESET`) and ICMP `ECONNREFUSED` for subsequent connected UDP sends to closed ports (unless blackholed). | **Silence**: no packet emitted; in-flight bytes/datagrams vanish; TCP peers detect death via `RetransmitTimeout`, keepalive (~150s default), or `RST` after reboot; UDP drops silently. |
| **New incoming TCP `Dial`s** | `ECONNREFUSED` (live kernel, closed listener). | **Blackholed** (`ETIMEDOUT` / deadline) until a subsequent `Host` / `HostWith` reboots `name`. |

#### Disk & Storage Media Faults

```go
func FailDisk(host string)
func HealDisk(host string)

func FailWriteback(host string)
func HealWriteback(host string)

func FailFile(host, path string)
func HealFile(host, path string)

func LimitDisk(host string, bytes int64)
func UnlimitDisk(host string)

func SlowDisk(host string, perOp time.Duration)
func UnslowDisk(host string)

func CorruptFile(host, path string)
```

- **`FailDisk` / `HealDisk`**: all-syscall filesystem shutdown (`read`, `write`, `fsync` fail `EIO`). Failed data `fsync`s model Linux `>= 4.13` **fsyncgate**: dirty pages are dropped from the writeback set, so a naive retry of `Sync()` after `HealDisk` succeeds *without* persisting those pages unless they are rewritten first.
- **`FailWriteback` / `HealWriteback`**: failing storage medium beneath a live page cache. Buffered `Read`/`Write` keep succeeding from cache, while `Sync()`/`Fdatasync()` and `O_SYNC`/`O_DSYNC` writes fail `EIO` (with fsyncgate dirty-page drop, and `O_SYNC` writes leaving file offset unadvanced). **Survives `CrashHost`** (broken hardware stays broken across reboots until `HealWriteback`).
- **`FailFile` / `HealFile`**: per-inode `EIO` on a single regular file at host-absolute `path` (follows renames and unlinked open handles; directories/missing paths are no-ops). Independent of `FailDisk` and `FailWriteback`.
- **`LimitDisk` / `UnlimitDisk`**: caps allocated 4 KiB regular-file blocks on `host` to `bytes / 4096` (`bytes >= 0`). Sparse `Truncate` holes cost `0` blocks, in-place overwrites of allocated pages cost `0` blocks, hole/append writes fill up to the 4 KiB quota boundary and return `(n, syscall.ENOSPC)`, `Fallocate` checks unallocated blocks atomically, `PROT_WRITE` non-zero dirtied pages count against quota, and `Remove`/`Truncate` free blocks immediately. Survives `CrashHost`.
- **`SlowDisk` / `UnslowDisk`**: adds `perOp` (`>= 0`) of virtual latency to each disk-touching backend operation on `host` (`open`, `read`, `write`, `sync`, path `stat`, `mkdir`, `remove`, `rename`, `readdir`, `truncate`, `chmod`, `chtimes`) outside the filesystem lock so other hosts never stall. In-memory operations (`Seek`, `Getwd`, open-handle `File.Stat`) are uncharged. Survives `CrashHost`.
- **`CorruptFile`**: draws one byte offset and one bit within the **durable** allocated 4 KiB blocks of `path` on `host` from the seeded fault RNG and flips it on the platter (bit rot). Masked by the live page cache while `host` is up; surfaces on reads after `CrashHost` + reboot (or immediately on reboot if injected while `host` is powered off). Rewriting and `Sync()`ing a page heals its platter rot.

#### Clock Faults

```go
func StepClock(host string, delta time.Duration)
func DriftClock(host string, ppb int64)
```

- **`StepClock(host, delta)`**: instantaneous forward (`delta > 0`) or backward (`delta < 0`) NTP wall-clock jump on `host`. Relative timers/contexts are unaffected; `time.Since` across the step reflects the wall jump. Panics if the step would take `host`'s wall clock before the Unix epoch.
- **`DriftClock(host, ppb)`**: continuous mid-run crystal rate change (`rate = 1 + ppb/1e9`, `ppb` in `(-1e9, 1e9]`) without wall discontinuity, remapping all armed timers and tickers on `host` to the new rate (`DriftClock(host, 0)` restores unit rate).

#### Inline Fault-Point Activation (`Sometimes`)

```go
func Sometimes(name string, prob float64) bool
```

- **`Sometimes(name, prob)`** evaluates a deterministic two-level rare-event / fault-injection decision inside SUT or harness code (FoundationDB `BUGGIFY` semantics):
  1. **Argument validation (uniform in both `-tags dst` and untagged builds):** `name` must be non-empty and `prob` must be a finite `float64` in `[0.0, 1.0]` (`NaN`, `< 0`, or `> 1` panics immediately, even outside an active run or at `prob == 0`, so invalid fault-site wiring fails fast in ordinary unit tests).
  2. **Boundary gates:** Returns `false` with zero RNG draws when `prob == 0`, in untagged (`!dst`) builds, outside an active simulation run, or from a foreign goroutine outside the active bubble.
  3. **Two-level activation:** In `-tags dst` builds inside the active simulation bubble, level 1 hashes `(seed, name)` statelessly to enable each distinct site name for approximately **50% of seeds** — preventing simultaneous activation of every fault hook from starving deep multi-step state space on every run. Level 2 draws from the calling goroutine's dedicated `dstBugRand` stream (`true` when `prob == 1.0` or the draw `< prob`).
  4. **Observation neutrality & DPOR commutativity:** `dstBugRand` is completely isolated from `g.dstrand` (`math/rand`, `crypto/rand`, `maphash`, map/`select` order), `dstSchedRand`, and `dstFaultRand`, allocates zero Go heap memory, and emits zero synchronization edges — adding or removing `Sometimes` calls that return `false` never shifts schedules, RNG draws, or `DPOR` equivalence classes.
  5. **Failure-only active-site diagnostics:** Every site where `Sometimes` returns `true` is recorded in an off-heap 256-slot ledger (`name`, hit count, and first-fired relative virtual timestamp) and rendered automatically only when `Test`, `TestWith`, `WorldTest`, or `ExploreTest` fails (silent on passing runs, even under `-test.v`).

**Stock-Go library wrapper pattern:** Because `testing/simulation` is shipped in the `godst` toolchain, libraries that also compile under stock upstream Go isolate `simulation.Sometimes` behind a tiny two-file build-tag pair:

```go
// fault_dst.go
//go:build dst

package mypkg

import "testing/simulation"

func buggify(name string, prob float64) bool {
	return simulation.Sometimes(name, prob)
}
```

```go
// fault_prod.go
//go:build !dst

package mypkg

func buggify(string, float64) bool { return false }
```

---

### 3.5 Systematic Exploration & Replay (`Explore`, `ExploreWith`, `ExploreTest`, `Replay`)

```go
type ExploreMode int

const (
	Exhaustive ExploreMode = iota // Brute-force enumeration over scheduling decisions
	DPOR                          // Source-DPOR pruning equivalent Mazurkiewicz traces
)

func Explore(seed uint64, mode ExploreMode, sut func() error) ExploreResult
func ExploreWith(seed uint64, opts ExploreOptions, sut func() error) ExploreResult
func ExploreTest(t *testing.T, seed uint64, opts ExploreOptions, sut func() error)
func Replay(seed uint64, failure Failure, sut func() error) ReplayResult
```

#### `ExploreOptions`

```go
type ExploreOptions struct {
	Options                  // Embedded run configuration (Network, Hostname, PID, PIDMax, NumCPU, MemoryLimit, CrashTear, Wedge*)
	Mode         ExploreMode // Exhaustive (default) or DPOR
	MaxSchedules int         // Stop after exploring N schedules (0 = unlimited; sets BudgetHit if reached before exhaustion)
	MaxSteps     int         // Max scheduling decisions per run (0 = default 1<<14; sets BudgetHit if reached)
	MaxAccesses  int         // Per-run memory-access log capacity (0 = default 1<<16, clamped at 1<<30-1)
	MaxEdges     int         // Per-run happens-before sync-edge capacity (0 = default 1<<16, clamped at 1<<30-1)
}
```

- `ExploreOptions` embeds `Options`, so explored runs and `Failure` tokens carry your exact `NetworkConfig`, `CrashTear`, `PIDMax`, `Hostname`, `PID`, `NumCPU`, `MemoryLimit`, and wedge settings into `Replay` (`Strategy`/`Depth`/`Steps` are validated if set, while `Explore` drives scheduling systematically).
- **Important:** For `DPOR` over shared-memory and `sync.Mutex` programs, build with **`-tags dst -race`** so compiler memory-access hooks, atomic operations, and mutex happens-before edges are visible to DPOR (running `DPOR` without `-race` sets `ExploreResult.Uninstrumented = true` and downgrades `Exhausted` whenever concurrency/mutex dependencies cannot be observed).

#### `ExploreResult` & `ExploreFootprint`

```go
type ExploreResult struct {
	Schedules         int              // Number of schedules executed
	Failures          []Failure        // All failing schedules discovered
	Exhausted         bool             // True iff full (pruned) space covered with no budget/overflow/foreign/uninstrumented downgrade
	Overflow          bool             // Internal decision/access/edge trace table overflowed (raise MaxSteps/MaxAccesses/MaxEdges)
	ForeignSched      bool             // Non-simulation background goroutines ran during exploration (exhaustion downgraded)
	UnattributedRaces int              // Race reports during foreign-tainted runs not attributable to a replayable SUT schedule
	Footprint         ExploreFootprint // Peak per-run trace consumption across the sweep
	BudgetHit         bool             // Stopped at caller-supplied MaxSchedules or MaxSteps
	Uninstrumented    bool             // DPOR ran without -tags dst -race visibility over concurrent/mutex choices
}

type ExploreFootprint struct {
	PeakDecisions    int // Compare against MaxSteps (default 16384)
	PeakEnabledTotal int // Derived automatically from MaxSteps (default 262144)
	PeakEdges        int // Compare against MaxEdges (default 65536)
	PeakAccesses     int // Compare against MaxAccesses (default 65536)
}
```

**Sizing budgets for larger SUTs:** Run a bounded probe (`MaxSchedules: 10`) under `-tags dst -race` and inspect `res.Footprint` (or check `t.Log` notes from `ExploreTest`). If `PeakAccesses` or `PeakEdges` approaches `65536` (common in consensus or storage arcs), set `MaxAccesses` / `MaxEdges` to `4×`–`8×` the observed peak so `Overflow` stays `false`.

#### `Failure`, `AccessForce`, & `ReplayResult`

```go
type Failure struct {
	Schedule     []uint64      // Chosen per-bubble goroutine sequence indices (dstSeq)
	AccessForces []AccessForce // Forced inline memory-access watchpoints required to reproduce pre-promotion races
	Err          string        // Non-empty when sut returned a non-nil error
	Race         bool          // True when -race reported a new data race on this schedule
	Panic        string        // Non-empty when sut (or a child goroutine/callback) panicked
	Deadlock     string        // Non-empty when all bubble goroutines deadlocked durably
	ForeignSched bool          // True if foreign goroutines ran during the failing run
	Options      Options       // Recorded simulation Options restored automatically by Replay
	MaxSteps     int           // Recorded trace capacities restored automatically by Replay
	MaxAccesses  int
	MaxEdges     int
}

type AccessForce struct {
	Seq   uint64
	Count uint64
	PCKey uintptr
}

type ReplayResult struct {
	Err   error // Exact error returned by sut (nil if sut returned nil)
	Raced bool  // True if -race detected a new data race during Replay
}

func (r ReplayResult) Failed() bool // Returns r.Err != nil || r.Raced
```

- **Replay contract:**
  - For assertion failures (`Failure.Err != ""`) and race failures (`Failure.Race`), `Replay(seed, failure, sut)` returns a `ReplayResult` where `res.Failed()` is `true` (note: TSan deduplicates race reports per process, so replaying a `Race` failure detects `Raced == true` in a fresh test process before that race signature has fired).
  - For panic (`Failure.Panic != ""`) and deadlock (`Failure.Deadlock != ""`) failures, `Replay` reproduces the exact panic or `synctest` deadlock panic directly.

---

## 4. Modeled Standard-Library & Syscall Surface

Unmodified application and library code (`net/http`, `crypto/tls`, custom binary RPC muxes, WALs, B-trees, memory-mapped engines) works out of the box inside a simulation across these modeled surfaces:

- **Time, Synchronization, & Randomness (`time`, `testing/synctest`, `sync`, `math/rand`, `math/rand/v2`, `crypto/rand`, `hash/maphash`):**
  - `time.Now`, `Sleep`, `After`, `AfterFunc`, `NewTimer`, `NewTicker`, and `context.WithTimeout`/`WithDeadline` run on the simulation bubble's virtual clock with per-host wall skew (`Skew`/`BoundedSkew`), NTP jumps (`StepClock`), and crystal rate drift (`Drift`/`BoundedDrift`/`DriftClock`); `time.AfterFunc` and `context.WithTimeout` callbacks inherit the arming goroutine's `Host`, `Process`, and seeded RNG streams (`dstrand` and `dstBugRand`), and cancel cleanly without running if that host or process crashes/exits before firing; `synctest.Wait` settles runnable goroutines at `0ns` virtual-time advance.
  - Raw Linux `clock_gettime` for `CLOCK_MONOTONIC` and `CLOCK_BOOTTIME` returns virtual base time rebased to the calling host's current boot instant (resetting to `0` across `CrashHost` + reboot).
  - In-bubble `sync.Mutex`, `sync.RWMutex`, `sync.WaitGroup`, `sync.Cond`, and `sync.Once` waits are durably tracked with deterministic starvation handoff and Level-2 DPOR happens-before edges.
  - `math/rand`, `math/rand/v2`, `crypto/rand`, `hash/maphash`, `select` case order, and value-keyed map iteration derive from per-goroutine seeded streams (`g.dstrand`) immune to OS thread migration and host load.
- **Networking (`net`):**
  - TCP (`net.Dial`, `DialTimeout`, `(*net.Dialer).DialContext`, `net.Listen`, `(*net.ListenConfig).Listen` over `"tcp"`, `"tcp4"`, `"tcp6"`) and UDP (`net.ListenPacket`, `(*net.ListenConfig).ListenPacket`, `net.ListenUDP`, `net.Dial("udp*", ...)`, `net.DialUDP`, `(*net.Dialer).DialUDP` over `"udp"`, `"udp4"`, `"udp6"` returning `*net.UDPConn`).
  - Deterministic `"localhost"` resolution (`127.0.0.1` / `::1`) across `Dial`, `Listen`, `ListenPacket`, `LookupHost`, `LookupIP`, `LookupNetIP`, `ResolveTCPAddr`, and `ResolveUDPAddr`, plus built-in protocol number resolution (`tcp`/`udp`/`icmp`) without touching host `/etc/protocols` or DNS.
  - Synthetic per-host `net.Interfaces()` / `InterfaceAddrs()` (`lo` `127.0.0.1/8` + `::1/128`, and `eth0` `10.<b>.<c>.<d>/24`).
  - Simulated TCP connections implement `net.Conn`, `syscall.Conn` (`Dialer.Control`, `ListenConfig.Control`), and structural TCP half-close (`CloseWrite() error` and `CloseRead() error`), with `TCP_NODELAY=1` by default, `SO_REUSEADDR`, `SO_KEEPALIVE`, `TCP_KEEPIDLE`, `TCP_KEEPINTVL`, `TCP_KEEPCNT`, and `TCP_USER_TIMEOUT`; simulated `*net.UDPConn` sockets implement `net.Conn`, `net.PacketConn`, and `syscall.Conn` with `SO_REUSEADDR`, `SO_REUSEPORT` load-balancing groups, `SO_RCVBUF`/`SetReadBuffer`, and `SO_SNDBUF`/`SetWriteBuffer`.
- **Filesystem, VM, Locking, & Synchronization (`os`, `syscall`, `golang.org/x/sys/unix`):**
  - Per-host in-memory tree initialized with `/tmp` (`01777`, `os.TempDir() == "/tmp"`) and `/dev/null`, full `os.File` / `os.Root` operations (including `os.Truncate` and `(*os.File).Truncate`), `O_SYNC` / `O_DSYNC`, and anonymous pipes (`os.Pipe` with 64 KiB buffer and `PIPE_BUF` atomicity).
  - Linux virtual FDs supporting `syscall.Read`/`Write`/`Pread`/`Pwrite`/`Fsync`/`Fdatasync`/`Fstat`/`Statfs`/`Fstatfs`/`Flock`/`Fallocate`/`Seek` (`SEEK_DATA`/`SEEK_HOLE`).
  - Coherent `syscall.Mmap` / `Munmap` / `Mprotect` / `Madvise` (`MADV_NORMAL`, `MADV_RANDOM`, `MADV_SEQUENTIAL`, `MADV_WILLNEED`, `MADV_POPULATE_READ`, `MADV_HUGEPAGE`, `MADV_COLD`): `MAP_SHARED` mappings share physical backing pages with the simulated page cache (stores through `[]byte` and `File.Read`/`Write` see each other immediately, and accesses beyond `Truncate` EOF raise real `SIGBUS` / `PROT_NONE` `SIGSEGV` caught and converted into simulated process crash teardown).
  - Simulated shared `futex` (`FUTEX_WAIT` and `FUTEX_WAKE` without `FUTEX_PRIVATE_FLAG` on `MAP_SHARED` file-backed pages) integrated with the virtual clock and process/host crash cleanup.
  - Synthetic `/proc/self/ns/pid`, `/proc/<pid>/stat` (deterministic field-22 `starttime` in `USER_HZ` ticks), `/proc/sys/kernel/random/boot_id`, and `syscall.Kill(pid, 0)` liveness probes.
- **Environment & Failure-Only Diagnostics:**
  - Per-`(host, process invocation)` copy-on-write `os.Getenv` / `Setenv` / `Unsetenv` / `Clearenv` / `Environ`.
  - A 256-slot chronological causal event timeline (host/process lifecycle, injected faults & link policies, TCP `Listen`/`Close`/`Dial`/timeout/`RST` transitions, UDP `Bind`/`Close`/`ECONNREFUSED`/drop transitions, and filesystem `EIO`/`fsyncgate`/`ENOSPC`/`CorruptFile` hits), activated `Sometimes` fault sites, abnormal process-exit goroutine kills, and ungranted bubble writes to `os.Stdout` / `os.Stderr` (or raw `fd 1` / `fd 2`) are recorded off-heap (`sysAlloc`) with zero heap, RNG, or scheduler perturbation and printed automatically only if the test or explored schedule fails.

---

## 5. Testing Workflow & Determinism Ground Rules

### Recommended test suite layout

1. **Fast gate tests (`go test -tags dst ./...` and `go test -tags dst -race ./...`):**
   - Keep deterministic `simulation.Test` / `simulation.TestWith` / `simulation.WorldTest` tests with **pinned seeds** (plus short 16–64 seed loops over fault scenarios) in your primary CI and pre-commit suites. They run in milliseconds because virtual sleeps (`time.Sleep(30 * time.Second)`) advance instantly at quiescence.
2. **Discovery sweeps (`ExploreTest` & wide seed campaigns):**
   - Write `ExploreTest(t, seed, ExploreOptions{Mode: DPOR, ...}, sut)` functions alongside your unit tests. `ExploreTest` automatically skips when `-short` is passed and executes during full non-`-short` `-tags dst -race` runs.
   - Whenever discovery surfaces a failure, **copy the printed `simulation.Replay(...)` snippet into a permanent `TestReplay...` regression test** before fixing the bug.

### Determinism ground rules & pitfalls to avoid

1. **Mint state inside the run, never at `init()`:** Package-level `map`s, `rand.New(...)` instances, `maphash.Seed`s, or cached `time.Now()`/`os.Getpid()` values initialized before `simulation.Run` carry host-startup entropy. Construct maps, RNGs, and servers inside the `Run` / `Host` / `Process` body.
2. **Launch crashable processes with `go simulation.Process(...)`:** Never call `CrashProcess` or `CrashHost` on a process/host whose goroutine set includes the simulation's root driver goroutine; start server processes on child goroutines and drive faults from the root script.
3. **Never share Go memory across simulated `Process` boundaries if crashing them:** Real OS processes share only the host filesystem, shared `MAP_SHARED` files, and the network. If Process A holds a Go `sync.Mutex` or channel shared in-memory with Process B and Process A is killed via `CrashProcess`, Process A's threads stop without unwinding and that Go mutex stays locked forever (as documented in [dst/faults.md](dst/faults.md)).
4. **Do not let pointer-keyed map iteration drive externally visible order:** Value-keyed maps (`string`, integers, structs without pointers) iterate deterministically under `-tags dst`, whereas `map[*T]V` hashes heap addresses that can vary with allocator history. Sort keys or key by deterministic IDs when order affects wire/disk output.
5. **Respect interception fences:** Operations that would leak host nondeterminism or mutate the host OS — `os/exec`, `cgo` calls, `os/signal` (`Notify`, `Ignore`, `Reset`, `Stop`, `Ignored`), `os.Executable`, Unix/raw/multicast sockets, arbitrary external DNS lookups, and unmodeled raw syscalls — panic immediately with `"unsupported under deterministic simulation"` naming the operation. Non-bubble goroutines outside the simulation remain completely unfenced.
