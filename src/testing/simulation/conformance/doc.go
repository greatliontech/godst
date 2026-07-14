// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package conformance holds the differential host-vs-sim conformance
// harness for the deterministic simulation's modeled I/O surfaces
// (pipes, TCP, filesystem — docs/dst/design.md is the authoritative
// contract). The package carries no library code: everything lives in
// dst-tagged Linux test files, run by the `test:conformance` Taskfile
// leg.
//
// Method: in a `-tags dst` binary, code OUTSIDE a simulation.Run
// executes the real host paths (build-mode inertness), so one test
// binary generates a seeded op sequence once, executes it against real
// Linux primitives, executes the SAME sequence inside simulation.Run,
// and diffs the normalized outcomes. Outcome normalization is error
// identity (a fixed errors.Is/errors.As probe chain over curated
// sentinels, syscall.Errno extraction, and PathError/LinkError/OpError
// Op fields — never error-message strings, except the recorded
// exception for the DST fence shapes, which export no sentinel),
// returned counts, and op-defined observable state (content hashes,
// stat fields). Wall-clock-domain values (mtimes not explicitly set,
// addresses, fd numbers, inode/dev numbers) are excluded from outcomes;
// identity is compared relationally (os.SameFile) instead.
//
// The allowlist in the harness is machine-checked spec: every entry is
// one model-vs-host divergence the spec RECORDS as deliberate, with a
// citation. A divergence matching no entry fails the test with the op
// sequence as reproducer; an applicable entry that never fires in a
// sweep fails as stale (the spec may have outrun the list). An
// unrecorded divergence is a fidelity defect by definition — the fix
// belongs in the model (or, when the model is right and the spec merely
// under-documents, in the spec), never in the allowlist.
//
// Determinism stance: the sim leg must be same-seed stable (enforced by
// a transcript-equality test); the host leg is tolerant of legitimate
// host nondeterminism. Grammar sequences are single-goroutine except
// targeted two-goroutine cases (close-while-blocked), which compare
// outcome SETS: the host observation and the deterministic sim outcome
// must each be a MEMBER of the host-legal set, rather than equal.
// Host-underspecified observables are handled where each lives: the op
// itself normalizes them away (chunked directory listings compare a
// sorted union plus per-chunk counts, since host getdents order is
// filesystem-defined; wall-domain stat fields never enter outcomes),
// and the one spec-recorded one-directional window — the partial count
// of a deadline-interrupted oversize pipe write, host-ring-dependent —
// is absorbed by its scoped allowlist entry (pipe-ring-byte-exact),
// not by a comparison mode.
//
// Deliberate scope bounds (not silent caps): crash-tear visibility is
// STRUCTURALLY outside a differential harness — every compared op needs
// a host counterpart leg, and host power-loss cannot be injected, so
// there is no host observation for a sim tear to diff against; the
// sim-side tear model stays pinned by the fork's own
// TestDSTFSDurability*/crash suites. The accept-backlog-overflow ladder
// depends on host somaxconn tuning and stays with the fork's unit
// suite; host /dev/null is probed with non-mutating ops only (a
// root-privileged harness must never chmod or unlink the real device).
// The fs grammar drives the process-cwd os namespace surface only:
// os.Root-scoped operations are not in the grammar (a recorded
// coverage bound, not a silent cap) — the rooted surface, including
// os.Root.Rename's preamble ordering, is pinned against a host-probed
// matrix by the fork's own suite (TestDSTFSOpenRootRenameHostMatrix
// and the TestDSTFSOpenRoot* tests) rather than differentially.
//
// The default sweep is bounded; set DST_CONFORMANCE_SEEDS=<n> for a
// wider sweep (see Taskfile.yml).
package conformance
