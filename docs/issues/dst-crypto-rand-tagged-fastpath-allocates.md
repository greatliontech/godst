# Tagged crypto/rand allocates outside simulations

Lands: when the DST entropy hook preserves the allocation-free host entropy
fast path

## Gap

Severity M. Passing the caller buffer through `dstReadRandom` changes escape
analysis in a `-tags dst` build. The existing `crypto/rand.TestAllocations`
reports one allocation instead of zero even outside a simulation. Inside a run
the extra object also perturbs the modeled application allocation stream and GC
trigger.

## Required outcome

Tagged host entropy reads remain allocation-free and simulation reads retain
determinism. The tagged enforcing matrix includes `crypto/rand`, and its
existing allocation test passes.
