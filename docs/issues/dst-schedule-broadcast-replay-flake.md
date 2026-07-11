# Same-seed broadcast scheduling can diverge across runs

Lands: the random-strategy broadcast scenario replays identically under repeated focused and full-suite runs, with the load-dependent input diagnosed and eliminated

## Gap

Severity H. `TestDSTScheduleDeterministic` observed two different interleavings
for the random-strategy `broadcast` scenario at seed 12345 during
`task test:dst`:

```text
first=145302523410021453350142351420
got  =145302523410105243321045152340
```

The immediately following focused `-count=5` run passed, so the fault is
intermittent rather than a stable expected-output mismatch. The test launches
the same built probe repeatedly; a difference means some scheduler input still
depends on process or host timing, violating the same-seed replay contract.

## Required outcome

Identify and remove the load-dependent scheduler input. The broadcast scenario
must replay byte-identically across repeated focused runs and while embedded in
the full tagged enforcing suite. Regression coverage must reach the diagnosed
source rather than only increasing the existing repetition count.

## Reproduction

```sh
GOROOT="$PWD" TMPDIR="$PWD/.tmp" "$PWD/bin/go" test -tags dst -count=5 -run '^TestDSTScheduleDeterministic$' runtime
task test:dst
```
