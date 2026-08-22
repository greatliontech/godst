# enterSimulation's build-mode refusals share one shape

**Lands:** user decision.

`testing/simulation.enterSimulation` refuses three build-time conditions the
simulation cannot run under, each with its own shape: the FIPS latch
(`fips140Mode`, a startup-latched GODEBUG read), the arenas experiment
(`goexperiment.Arenas`, a build constant), and — until the go1.27 port retired
it — the size-specialized-malloc experiment. The remaining two are the same
concept ("this binary was built in a mode the model does not cover; refuse
before publishing any run state") expressed twice, and `internal/goexperiment`
is imported for the arenas check alone. Collapse: one table of
(predicate, message) refusals evaluated at entry, pinned by one table-driven
test (`TestDSTFIPSModeRefused` and `TestDSTArenasRefused` today), so a future
refused mode is one row, not a new code path.
