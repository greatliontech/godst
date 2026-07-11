# Process-global race counts can attribute a foreign race to the SUT

Lands: when Explore reports only races attributable to the active simulation,
or marks process-global race observations as non-replayable foreign work

## Gap

Severity M. `runOnceResultLocked` samples the process-global
`runtime.RaceErrors` counter and turns every increment into a SUT
`Failure.Race`. Foreign goroutines can race during the sample window, producing
a failure carrying the simulation's schedule even though replaying that
schedule without the foreign workers is clean.

## Required outcome

Race failures have simulation attribution strong enough for the replay claim,
or are reported distinctly as foreign/incomplete observations. A race-free SUT
overlapped with a foreign-only race produces no replayable SUT race failure,
while an in-bubble race still does.
