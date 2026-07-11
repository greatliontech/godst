# DST explore: recording gates key on bubble-ness, not sim membership

Lands: when every explore recording/reporting gate keys on sim-bubble
membership (dstSimG / bubble == dstSimBubble), or per-gate unreachability is
recorded at the gate

## Gap

Severity H (audit-found 2026-07-11; the panic leg reproduced live). Four
gates in src/runtime/dst_explore.go still admit non-members:

- dstExploreRecordUncaughtPanic gates on `gp.bubble != nil`: a FOREIGN
  synctest bubble's goroutine panicking mid-episode is recorded as the
  exploration's own failure (Failures=1 with the foreign panic value) AND
  the panic is consumed at goexit, so the foreign test returns clean —
  identical seeds diverge in Failures on foreign timing, the recorded
  failure replays clean, and a genuine foreign bug is suppressed.
  Reproduced on HEAD.
- dstExploreRecordDeadlock has the same bubble-ness gate: a foreign bubble
  deadlocking mid-episode records a sim Failure.Deadlock and suppresses the
  foreign bubble's own deadlock panic (mechanism-verified, not executed).
- dstYieldAccess and dstRecordSyncEventForGID gate on live `gp.bubble`
  equality while the index chokepoint keys on the sticky dstSimG — divergence
  unreachable today (the nil window is confined to uninstrumented runtime
  code), but a future runtime path borrowing gp.bubble at an instrumented
  point would silently drop a member's accesses (DPOR misses classes with no
  report).

## Required outcome

The panic and deadlock recorders key on the sim bubble (foreign panics and
deadlocks propagate untouched, recorded never); the two live-field gates key
on the sticky bit or carry a recorded unreachability argument; foreign
panic/deadlock arms exist in the test surface.
