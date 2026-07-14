# Post-reset op identities: recorded stable-ECONNRESET collapse vs the kernel's one-shot sk_err

Host-probed on this machine: after an injected RST, the FIRST failing op
consumes `sk_err` (read → ECONNRESET), the SECOND read returns EOF, and a
write on the now-CLOSED socket returns EPIPE (write-first: write ECONNRESET,
then read EOF). A reset received in CLOSE_WAIT (peer's FIN already arrived)
sets EPIPE, not ECONNRESET (cited from tcp_reset's CLOSE_WAIT arm,
unprobed). The simulation instead keeps the STABLE ECONNRESET identity on
every subsequent op — a deliberate, RECORDED collapse (design.md's FIN/RST
section: "the simulation keeps the stable ECONNRESET identity on every
subsequent op so reset-handling paths keying on it never miss — a SUT
distinguishing EPIPE from ECONNRESET post-reset is [the recorded boundary]").

Consequences the record accepts: a SUT branching on EPIPE (ubiquitous
broken-pipe handling) or expecting EOF-after-consumed-reset never exercises
those paths under simulation (false-negative direction), and the sim emits
second-op identities production does not produce. faults.md's "reads fail
ECONNRESET" (plural) glosses the one-shot nuance.

Spec-amend candidate (user ruling): move the identity model to the
kernel-faithful one-shot sk_err semantics (first op consumes; then EOF reads
/ EPIPE writes; CLOSE_WAIT arm included), or keep the recorded stable
identity. Kept-stable, faults.md's wording should acknowledge the one-shot
divergence explicitly.

Lands: user ruling on the post-reset identity model — either the one-shot
sk_err semantics land (with conformance TCP grammar rows for second-read EOF
and post-reset write EPIPE), or the stable-identity record in design.md and
faults.md is extended with the explicit one-shot divergence.
