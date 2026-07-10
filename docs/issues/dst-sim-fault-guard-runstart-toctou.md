# DST sim: fault-caller guard has a run-START TOCTOU window

Lands: when the activation edge is closed (guard and runActive publication
ordered), or the window is confirmed accepted and the acceptance recorded at
the guard's comment (this doc then deletes)

## Gap

Adjacent finding, chunk-27 review (2026-07-11). The caller-position guard
loads `runActive`; a foreign call that loads false just before
`enterSimulation`'s CAS can have its fault op execute into the newly-activated
run at wall-clock timing. Mid-run calls are reliably caught (`runActive` holds
true throughout), and the closing edge is clean (`leaveSimulation` runs after
deactivation; all fault-op families verified inert post-run). Only the
activation edge races.

## Required outcome

The activation-edge window is closed or confirmed accepted. The demonstrated
mid-run fault (the chunk-27 fix's subject) is unaffected either way.
