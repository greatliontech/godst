# Stale inherited capability refuses with a misdirecting ErrClosed shape

`dstInheritedFile.begin` returns `poll.ErrFileClosing` on run-epoch mismatch
or outside-bubble use, so a capability used in a LATER run (or after its run)
reports "file already closed" with `errors.Is(err, os.ErrClosed) == true` —
while the capability is not closed (its hidden dup stays open, a leaked host
fd until Close). Probe-verified. This is exactly the shape the node-scope
refusal rationale rejects for the spatial leak ("a closed-shape refusal
misdirects diagnosis... an error-swallowing log pipeline silently drops every
record" — design.md's capability section), and it is inconsistent with the
simulated pipe's temporal-leak stance (fenced, unsupported shape). The spec
records no cross-run capability shape at all.

Fix: the temporal (cross-run/post-run) refusal returns a typed, distinguishable
error per the settled refusal taxonomy (the node-scoped refusal's sibling),
never a closed shape; the spec's capability section gains the temporal-scope
sentence; a pin covers both the second-run and post-run arms.

Lands: when a stale capability's refusal carries a typed non-closed identity
recorded in design.md's capability taxonomy, pinned by a
second-run/post-run test.
