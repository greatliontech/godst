# Plan: DST audit issue burn-down

Derived from every open entry in `docs/issues/README.md` after the audit of
`5bbd22ff78daf010c5bd19c466a0c45ac78503d4..HEAD`. Specs:
`docs/dst/design.md`, `docs/dst/faults.md`, `docs/dst/gc.md`, and
`docs/dst/exploration.md` are authoritative. Chunks are ordered bottom-up and
grouped by the functions they change. WIP = 1.

## Build and public symbol selection

- [x] 1. `os.File` DST field access is selected only on layouts that carry it; ordinary Windows and Plan 9 `os` builds join the enforcing cross-build matrix
- [x] 2. `crypto/internal/sysrand.Read` preserves the allocation-free host path while retaining seeded in-run entropy

## Syscall entry functions

- [x] 3. linux/386 and linux/s390x `socketcall`/`rawsocketcall` enforce the fence without a splittable uintptr-bearing wrapper
- [x] 4. MIPS `Syscall9` fence implementation and runtime verification are assigned to the final architecture-verification chunk
- [x] 5. `dstSyscallAllowedTrap` refuses ioctl because device-specific requests cannot prove read-only, non-minting behavior
- [x] 6. loong64 `fstatFD` isolation implementation and runtime verification are assigned to the final architecture-verification chunk
- [x] 7. `dstTryClockGettime` returns kernel-shaped EFAULT for invalid time32/time64 output ranges (`dst-clock-gettime-invalid-pointer-faults`)
- [x] 8. `dstRawDispatch` gives zero-length mapping operations one named/raw ownership and errno contract (`dst-raw-zero-mapping-bypasses-ownership`)
- [x] 9. `AllThreadsSyscall` and `AllThreadsSyscall6` cannot bypass the active-run fence (`dst-syscall-allthreads-bypasses-fence`)
- [x] 10. allowlisted non-close fd dispatch cannot straddle host-fd creation

## Runtime allocation and GC functions

- [x] 11. `userArena.alloc`/`newUserArenaChunk` feed deterministic heap and process counters, or simulation entry rejects arena builds
- [x] 12. the pooled `_defer` exclusion and marked-side accounting have a reaching mutation test
- [x] 13. foreign `SetGCPercent` and `runtime.GC` cannot move active-run trigger state; direct foreign arms pin `debug.FreeOSMemory` and the goroutine-leak entry, including scavenge and leak-mode flags (`dst-gc-foreign-setgcpercent-perturbs-trigger`, `dst-gc-forced-entry-funnels-unpinned`)
- [x] 14. GC allocation and execution counters key on sticky simulation membership at every transient bubble-clear window (`dst-gc-counter-gates-live-bubble-field`)
- [x] 15. the foreign-GC exploration workload replays stably in normal and race builds, with the mechanism or bound recorded (`dst-explore-gc-workload-churn-flake`)

## Runtime callback functions

- [x] 16. finalizer and cleanup metadata carries process lifecycle ownership so exit/crash cannot run post-mortem callbacks (`dst-process-callbacks-run-after-exit`)
- [x] 17. callback discard keeps queue ledgers exact without incrementing public executed metrics (`dst-gc-discarded-callbacks-count-as-executed`)

## Runtime crash and membership functions

- [x] 18. `dstMarkProcessGoroutinesCrashed` and `dstMarkHostGoroutinesCrashed` scan sticky members across GC disassociation (`dst-runtime-crash-skips-disassociated-members`)
- [x] 19. crashed goroutine stacks stop contributing process roots while remaining permanently unrunnable and non-unwinding (`dst-crash-retains-stacks-and-process-memory`)

## Runtime clock functions

- [x] 20. `dstFakeTimersRoll` and `dstRegisterFakeTimer` preserve new-epoch registrations under multi-P activation (`dst-clock-fake-timer-roll-loses-registration`)
- [x] 21. lazy-fire timestamp semantics on drifted hosts are implemented or recorded with their bound (`dst-clock-lazy-fire-timestamp-drift`)

## Runtime scheduling functions

- [x] 22. same-seed scheduler and network-reset-order probes replay identically under focused repetition and controlled host contention, with their load-dependent inputs eliminated (`dst-schedule-broadcast-replay-flake`, `dst-scheduler-load-replay-flakes`)

## Exploration recorder functions

- [x] 23. panic, deadlock, access, ready-edge, and sync-event recorders all key on active simulation membership (`dst-explore-recorder-gates-not-membership-keyed`)
- [x] 24. race-build tests independently reach and kill each foreign access and sync-event membership mutant, or record structural unreachability (`dst-explore-race-door-reaching-arms`)
- [x] 25. race failures are attributed to the active simulation rather than process-global foreign races (`dst-explore-foreign-races-misattributed`)
- [x] 26. buffered direct handoff records the slot reuse HB relation used by the race detector (`dst-explore-buffered-direct-handoff-misses-hb`)

## Exploration scheduling functions

- [x] 27. idle-window foreign work cannot diverge replay, or produces the precise reported incomplete condition (`dst-explore-foreign-idle-window-divergence`)
- [x] 28. `dstSchedRootPCT` and public option validation support one depth range without silent clamping (`dst-pct-depth-silently-clamped`)
- [x] 29. `ExploreWith` rejects unknown modes before publishing exploration state (`dst-explore-unknown-mode-falls-back`)

## Filesystem path and metadata functions

- [x] 30. `dstRemove`, `dstRemoveAll`, and `dstRename` resolve physical intermediates before terminal dot restrictions (`dst-fs-terminal-dot-bypasses-walk`)
- [x] 31. named and rooted create functions preserve the special mode bits Chmod can represent (`dst-fs-create-drops-special-mode-bits`)
- [x] 32. `dstChdir` pays one SlowDisk path traversal delay (`dst-disk-latency-skips-chdir`)
- [x] 33. the O_DSYNC data-only contract is modeled or its O_SYNC conflation is recorded (`dst-fs-odsync-conflates-osync`)

## Filesystem crash-image functions

- [x] 34. `residentLocked` counts unique nodes in every crash-tear alias image (`dst-fs-crash-alias-double-counts-capacity`)
- [x] 35. `dstRename` uses node containment so crash-recovered directory aliases cannot form cycles (`dst-fs-crash-directory-alias-allows-cycle`)

## File, Root, and process-state functions

- [x] 36. `dstRoot` records host/process ownership and closes on process and host teardown (`dst-root-not-owned-by-process-or-host`)
- [x] 37. process teardown removes cwd and environment views before a same-name restart (`dst-process-restart-inherits-cwd`, `dst-process-restart-inherits-environment`)
- [x] 38. environment host/COW dispatch is atomic with activation and deactivation (`dst-env-dispatch-straddles-run-edge`)
- [ ] 39. mapping-fault death invokes the complete process resource lifecycle exactly once (`dst-mapping-fault-skips-resource-teardown`)

## Network stream functions

- [ ] 40. `dstStream` persistent EOF signaling wakes all legal concurrent readers (`dst-net-concurrent-eof-strands-reader`)
- [ ] 41. FIN is a delayed control event and pays the configured one-way link latency/jitter (`dst-net-fin-bypasses-link-delay`)
- [ ] 42. link delivery and handshake timers consume universe base time under host drift while the sender-clock retransmission rule remains intact (`dst-net-base-delay-scaled-by-host-drift`)
- [ ] 43. throttle and delivery timestamp arithmetic clamps or rejects every signed overflow (`dst-net-delay-arithmetic-overflows`)

## Network partition and bind functions

- [ ] 44. directional cut state composes refuse and blackhole sources with blackhole dominance (`dst-net-blackhole-cannot-override-refuse`)
- [ ] 45. dialer and listener ephemeral counters are host-scoped (`dst-net-port-allocator-cross-host-coupling`)
- [ ] 46. explicit LocalAddr validates source ownership, reserves before parking, and releases on every failure (`dst-net-local-bind-lifecycle-incomplete`)
- [ ] 47. IP-less wildcard LocalAddr behavior is modeled or recorded (`dst-net-wildcard-localaddr-bind-collapse`)

## Network establishment and handle functions

- [ ] 48. the SYN-ACK traversal observes cancellation, reset, process exit, and host crash before success (`dst-net-synack-ignores-cancel-and-teardown`)
- [ ] 49. connection ownership publishes atomically with listener handoff and teardown visibility (`dst-net-accept-handoff-precedes-ownership`)
- [ ] 50. stateful connection/listener operations reject stale epochs; address accessors stay immutable and Close remains local cleanup (`dst-net-handles-cross-run-epochs`)

## Simulation process lifecycle functions

- [ ] 51. different-host liveness validation and active-process registration are atomic (`dst-process-cross-host-admission-race`)
- [ ] 52. process-crash preflight rejects run-main victims before any registration or pid mutation (`dst-crash-main-refusal-mutates-state`)
- [ ] 53. normal exit orders invocation-thread death and pid/procfs liveness coherently, and closes logical-process resources only on the last invocation (`dst-process-exit-publishes-dead-too-early`)
- [ ] 54. a pre-run Process exit cannot tear into a newly active run (`dst-sim-process-exit-teardown-spans-activation`)
- [ ] 55. PID exhaustion rolls back node identity and victim registration completely (`dst-process-pid-exhaustion-leaks-stamp`)

## Simulation Host and caller-gate functions

- [ ] 56. Host declaration validation and publication are all-or-nothing on invalid clocks and table exhaustion (`dst-host-declaration-failure-not-atomic`)
- [ ] 57. Host NumCPU is exact over its accepted public range or rejected before publication (`dst-host-numcpu-wraps-int32`)
- [ ] 58. CrashHost tracks root-process host ancestry through nested Host bodies (`dst-host-crash-misses-nested-root-goroutine`)
- [ ] 59. caller-gate readers cannot be killed while parked holding the read side (`dst-sim-guarded-reader-killed-while-parked`)
- [ ] 60. representative guarded APIs have killing tests for their complete hold extent (`dst-sim-guard-hold-extent-unpinned`)

## Deferred architecture verification

- [ ] 61. linux/mips and linux/mipsle `Syscall9` enter the same fence and dispatch policy as the generic trampolines, and `clock_gettime64` copyout behavior executes under qemu-user (`dst-mips-syscall9-bypasses-fence`, `dst-mips-clock-gettime64-runtime-unverified`)
- [ ] 62. linux/loong64 `fstatFD` applies virtual and page-cache descriptor classification before direct statx, with the kernel-facing behavior executed under qemu-user (`dst-loong64-fstat-exposes-pagecache-fd`)
