# Declarative sub-host process UTS and root-directory overrides (`ProcessConfig.Hostname` / `RootDir`)

**Lands:** deferred until triggered by user if a co-pod container workload
requires distinct hostnames or chroot trees on a shared host filesystem
(user-authorized deferral; zero retrofit — additive fields on `ProcessConfig`).

## Architectural stance: why kernel namespace syscalls stay out of scope

A single Go runtime shares one address space and cannot `fork(2)` or `clone(2)`
real OS processes inside a deterministic `synctest` bubble (`os/exec` and
`syscall.Unshare` / `Setns` / `Mount` are fenced by design; `design.md`). Instead,
`godst` already models the **observable resource boundaries** that Linux namespaces
provide:

| Linux Namespace | Observable Resource | `godst` Modeling Primitive |
|---|---|---|
| `CLONE_NEWNET` | Private `lo`, routable IP, port space, interfaces | `simulation.Host` / `HostWith` |
| `CLONE_NEWNS` | Isolated filesystem tree + page cache | `simulation.Host` / `HostWith` |
| `CLONE_NEWUTS` | `os.Hostname()` | `HostConfig.Hostname` |
| `CLONE_NEWTIME` | Wall skew, NTP steps, crystal drift, boot uptime | `HostConfig.Clock`, `StepClock`, `DriftClock` |
| `CLONE_NEWPID` | Isolated `Getpid`, `Kill(pid, 0)`, `/proc/<pid>/stat`, `/proc/self/ns/pid` | Per-`(host, boot)` PID table + `ProcessConfig.PIDNamespace` |
| `CLONE_NEWIPC` | Advisory `flock`, `MAP_SHARED` coherence, `futex` | Per-host / per-process-slot tables |

Therefore:
- **Separate containers on separate volumes/IPs** are already modeled with 100%
  fidelity as distinct `simulation.Host` declarations.
- **Co-pod sidecar containers sharing a host volume and network** while isolated in
  PID visibility are already modeled via co-located `ProcessWith` declarations on
  one `Host` using distinct `ProcessConfig.PIDNamespace` values.
- Simulating imperative `unix.Unshare`, `unix.Setns`, or `unix.Mount` syscalls inside
  one Go process would add substantial VFS mount-propagation complexity while serving
  container runtimes (`runc`/`kubelet`) rather than distributed systems under test,
  and remains deliberately out of scope.

## Additive scope if triggered

If a multi-container pod workload ever needs co-located processes on the **same**
`Host` (sharing a volume or loopback) to observe different UTS hostnames or
container-private root views alongside a shared volume path, extend `ProcessConfig`
declaratively rather than modeling namespace syscalls:

1. **`ProcessConfig.Hostname string`:** Optional per-process-invocation UTS override
   returned by `os.Hostname()` for goroutines in that invocation (`""` inherits the
   enclosing `Host`'s hostname, preserving current behavior).
2. **`ProcessConfig.RootDir string`:** Optional host-absolute directory acting as `/`
   for path resolution inside that process invocation (reusing `os.Root`'s
   `..`-clamped / escape-safe path walk logic over the host's `dstFSDisk` tree), or
   explicit bind-subdir mounts from the host tree, while sharing underlying inode
   durability and `CrashHost` power-loss fate with the enclosing `Host`.
