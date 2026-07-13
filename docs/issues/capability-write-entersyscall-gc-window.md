# Capability writes carry the entersyscall window that wall-timed events can turn into a schedule fork

An inherited-file capability operation (`os/dst_inherited_unix.go`) performs
its host I/O through `internal/poll` → `syscall.write` → the `Syscall`
trampoline, i.e. inside an entersyscall/exitsyscall window. In that window
the single P's `_Psyscall` state is scheduler-visible shared state that
wall-timed host events race against — a host M's own exitsyscall fast path
reclaiming the P it also enters syscalls through, or a pending
stop-the-world claiming `_Psyscall` Ps — and when the bubble M loses the
race, its returning goroutine takes exitsyscall's slow path onto the run
queue: a scheduling decision at a wall-clock-dependent instant, i.e. a
same-seed schedule fork.

Mechanism demonstrated one seam over: the testing framework's `-v` stream
write had the identical shape and produced reproducible same-seed transcript
divergence under `-race` with a host-parallel test logging concurrently
(kill data: trampoline form diverges in roughly a third of pin runs, the
raw form in none of 8+; `GOGC=off` runs did not diverge either, so a
GC-timed stop is one plausible racer, not the proven one). The framework
stream now writes with `RawSyscall` — scheduler-invisible, no window
(`testing/dst_hostio.go`, `TestVerboseContendedSameSeedTranscript` is the
regression pin); capability writes still use the poll path.

Reachable in-spec shape: a simulation using `simulation.InheritFile` (e.g. a
consumer's stderr logging relay) while a host-parallel test runs syscall or
allocation churn — nothing forbids the combination. Not yet witnessed by a
consumer: protodb's DST tier runs its simulation tests without host-parallel
churn.

Candidate fixes, for the determinism-escape sweep to weigh: route capability
host I/O through a raw, scheduler-invisible path like the framework
stream's (per-operation, preserving the poll layer's deadline support where
deadlines are armed — the hard part); or close the window at the runtime
level for bubble goroutines under an active run (deterministic resumption
for a syscall-returning bubble goroutine whose P was taken). The second
repairs the spec's "a capability's real syscall serializes the bubble: one
legal execution, deterministic" claim for every current and future granted
syscall, not one call site — and would also pin down which racer (P steal
vs pending stop) is live.

Scope extension for the same sweep: `fmt`'s process-shared printer pool
makes any in-bubble formatting call's hit-or-miss allocation profile
host-coupled shared state — a whole-of-fmt property (SUT formatting
included, and the framework stream's own `fmt.Appendf`), pre-existing, not
schedule-visible in any current pin; the sweep owns deciding whether it is
a reachable escape and at what level to close it (bubble-scoped pooling,
pool epoch gating, or a recorded bound).

Lands: with the determinism-escape sweep, or when a consumer runs
capability I/O alongside host-parallel load, whichever first.
