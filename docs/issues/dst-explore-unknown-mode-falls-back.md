# Unknown Explore modes silently select Exhaustive

Lands: when ExploreWith rejects every mode other than Exhaustive and DPOR
before invoking the SUT

## Gap

Severity L. `ExploreWith` selects DPOR only by equality and routes every other
numeric value to `exhaustiveExplore`. Invalid public configuration can therefore
run a different algorithm and return an exhausted verdict instead of failing
loudly.

## Required outcome

Explore mode validation is explicit, pure, and state-neutral. A test passes an
unknown value, proves the SUT was not invoked, and verifies no exploration or
run state was published.
