# Pre-run-minted randomness is not seed-pure (demonstrated same-seed divergence)

Draws made BEFORE `dstActivate` go through the per-m chacha8 streams derived
from the fixed dst seed, but at a scheduling-dependent stream POSITION
(pre-run draw counts vary with GC/sysmon interleaving). Any pre-run-minted
value captured for in-run use is therefore not seed-pure. Probe-demonstrated
(same machine, same binary, same seed, 60 runs): a package-level
`map[string]int` populated at `init()` and ranged in-run produced 2 distinct
iteration orders (55/5, ~8% minority class); init-time `maphash.MakeSeed()`,
init-time `rand.Uint64()`, and a pre-run-populated `sync.Map` shift together.
An SUT whose behavior depends on registry-map range order fails at seed S and
replays at seed S only ~92% of the time — the mandated-out unreproducible
failure. Affected sources: per-map group-placement seeds at map creation
(`internal/runtime/maps`), HashTrieMap seeds (`internal/sync`),
`maphash.MakeSeed`, init-time `math/rand` values. In-run map ITERATOR start
offsets draw from the per-g stream and are pure even over pre-run maps — the
escape is group placement and captured values.

Spec contradictions to resolve with the fix (spec-amend candidates, user's
ruling):

- design.md's sources table claims full coverage for "map iteration order
  (value-keyed)" with no created-pre-run qualifier — the probe map is
  value-keyed and nondeterministic.
- design.md's library-randomness paragraph says randomness bottoming out in
  `math/rand` top-level "is covered with no patch" — false when the draw
  happens at package init and the value is captured for in-run use.
- `maphash` appears in neither spec.

Adjacent unrecorded boundaries found by the same audit:

- Hash-FUNCTION selection changes same-seed in-run map order for maps >8
  entries: `GODEBUG=cpu.aes=off` vs default (AES vs generic hash) produce
  different stable orders, and cross-architecture replay diverges likewise.
  Same-machine determinism holds; the cross-machine/CPU-feature replay
  boundary is unstated in the specs.
- The determinism sweep is structurally blind to all of the above: no
  pre-run-state axis, 8-element worker maps (single-group — placement never
  exercised), no GODEBUG perturbation leg. (A pre-run-state axis added
  before the escape is fixed would flake at ~8% — land the axis WITH the
  fix.)
- `GODEBUG=randautoseed=0` silently collapses v1 `math/rand` top-level
  values to seed-INDEPENDENT constants (reproducible, false-negative
  direction only); the run neither rejects nor records the knob.

Lands: when pre-run-minted entropy is made seed-pure (or the spec's coverage
claims are narrowed to exclude pre-run state by user ruling), together with a
determinism-sweep axis that would have caught this class (pre-run-populated
map + captured init-time draws in the sweep program).
