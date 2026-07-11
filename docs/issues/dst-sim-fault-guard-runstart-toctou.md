# DST sim: caller-position guards have a run-START TOCTOU window

Lands: when the activation edge is closed (guards and runActive publication
ordered), or the window is confirmed accepted and the acceptance recorded at
the guards' comments (this doc then deletes)

## Gap

Adjacent finding, chunk-27 review (2026-07-11). The caller-position guards —
`requireBubbleFaultCaller` and, since the declaration APIs gained the same
guard, `requireBubbleDeclCaller` (Host/Process) — load `runActive`; a foreign
call that loads false just before `enterSimulation`'s CAS can have its fault
op (or topology mutation: an intern/reboot/goroutine start) execute into the
newly-activated run at wall-clock timing. Mid-run calls are reliably caught
(`runActive` holds true throughout), and the closing edge is clean
(`leaveSimulation` runs after deactivation; all fault-op families verified
inert post-run). Only the activation edge races. A fix at the activation edge
(ordering `runActive` publication against in-flight guarded ops) covers both
guards at once; an acceptance must be recorded at both.

## Required outcome

The activation-edge window is closed for both guard classes or confirmed
accepted. The demonstrated mid-run fault (the chunk-27 fix's subject) is
unaffected either way.
