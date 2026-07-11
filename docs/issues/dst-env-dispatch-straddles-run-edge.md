# Environment dispatch can straddle activation and choose the wrong world

Lands: when the host-versus-simulated environment decision is atomic with run
activation and deactivation

## Gap

Severity M. Environment entry points call `dstEnvCurrent` and mutate the
returned COW view or the host later, without holding the simulation caller
gate across that choice. A foreign `Setenv` can choose the host before
activation and write it during the run, changing lazy process baselines; the
reverse edge can mutate a stale COW view after deactivation while returning
success to an outside-run caller.

## Required outcome

Every environment operation lands wholly in the host world or one run epoch.
Barrier tests cross both run edges after dispatch selection and verify the
run-entry snapshot, host environment, and return value remain coherent.
