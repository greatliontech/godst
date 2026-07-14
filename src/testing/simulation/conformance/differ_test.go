// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package conformance

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"slices"
	"syscall"
	"testing"
)

// fullAllowlist is every domain's allowlist, as the canaries exercise
// the differ exactly as the domain sweeps wire it.
func fullAllowlist() []allowEntry {
	var all []allowEntry
	all = append(all, fsAllowlist()...)
	all = append(all, pipeAllowlist()...)
	all = append(all, tcpAllowlist()...)
	return all
}

// The differ must report a divergence no allowlist entry records: a
// neutered comparator or a match-anything allowlist entry makes this
// canary fail. (This is the standing form of the review loop's
// mutation test for the differ's load-bearing arms.)
func TestDSTConformanceDifferCatchesUnallowlistedDivergence(t *testing.T) {
	ops := []op{{name: "canary-op"}}
	host := []outcome{{Err: "PathError(open)/errno:ENOENT", N: -1}}
	sim := []outcome{{Err: "", N: -1}}
	d := diffOutcomes(ops, host, sim, fullAllowlist(), map[string]int{})
	if d == nil {
		t.Fatal("differ accepted a divergence recorded nowhere in the spec allowlist")
	}
	if d.index != 0 || d.op != "canary-op" {
		t.Fatalf("differ located the divergence at %d %q, want 0 canary-op", d.index, d.op)
	}
	// Equal outcomes must stay accepted.
	if d := diffOutcomes(ops, host, host, fullAllowlist(), map[string]int{}); d != nil {
		t.Fatalf("differ reported equal outcomes as divergent: %+v", d)
	}
}

// Each allowlist entry must match exactly its recorded shape: the
// recorded divergence passes (and is counted as fired), a near-miss of
// the same op is still reported.
func TestDSTConformanceAllowlistMatchesOnlyRecordedShapes(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		op       op
		hostOK   outcome // the recorded host side
		simOK    outcome // the recorded sim side
		hostMiss outcome // a near-miss host side that must NOT be allowlisted
		simMiss  outcome
	}{
		{
			name:     "umask",
			key:      "fs-create-umask",
			op:       op{name: `stat("f") fromCreate=true`, permFromCreate: true},
			hostOK:   outcome{N: -1, State: "kind=f perm=0644 size=3"},
			simOK:    outcome{N: -1, State: "kind=f perm=0666 size=3"},
			hostMiss: outcome{N: -1, State: "kind=f perm=0600 size=3"}, // unrelated to 0666&^umask
			simMiss:  outcome{N: -1, State: "kind=f perm=0666 size=3"},
		},
		{
			name:     "pipe-fd",
			key:      "pipe-fd-fenced",
			op:       op{name: "pipe-fd(slot 3)"},
			hostOK:   outcome{N: -1, State: "fd=ok"},
			simOK:    outcome{Err: "panic:dst-unsupported", N: -1},
			hostMiss: outcome{N: -1, State: "fd=ok"},
			simMiss:  outcome{Err: "panic:opaque", N: -1},
		},
		{
			name:     "first-write-after-fin",
			key:      "net-first-write-after-fin",
			op:       op{name: "write-after-peer-close#1(conn 0, 32 bytes)"},
			hostOK:   outcome{N: 32},
			simOK:    outcome{Err: "OpError(write)/errno:ECONNRESET", N: 0},
			hostMiss: outcome{Err: "OpError(write)/errno:EPIPE", N: 0}, // host did NOT accept it
			simMiss:  outcome{Err: "OpError(write)/errno:ECONNRESET", N: 0},
		},
		{
			name:     "close-in-flight-write",
			key:      "net-close-in-flight-fin-ordering",
			op:       op{name: "post-reset-write(conn 2, 48 bytes)", writeSize: 48},
			hostOK:   outcome{N: 48}, // the host end FINned: the write is accepted
			simOK:    outcome{Err: "OpError(write)/errno:ECONNRESET", N: 0},
			hostMiss: outcome{N: 48},
			simMiss:  outcome{Err: "OpError(write)/ErrClosed:net", N: 0}, // not a reset identity at all
		},
		{
			name:     "close-in-flight-read",
			key:      "net-close-in-flight-fin-ordering",
			op:       op{name: "post-reset-read(conn 2, 8 bytes, guard 0s)"},
			hostOK:   outcome{Err: "EOF", N: 0}, // the host end FINned: reads are io.EOF
			simOK:    outcome{Err: "OpError(read)/errno:ECONNRESET", N: 0},
			hostMiss: outcome{Err: "EOF", N: 0},
			simMiss:  outcome{Err: "OpError(read)/errno:EPIPE", N: 0}, // reads never carry EPIPE
		},
		{
			name: "pipe-ring",
			key:  "pipe-ring-byte-exact",
			op: op{
				name:      "pipe-write(slot 5, 4196 bytes, guard 150ms)",
				writeSize: pipeBuf + 100,
				guarded:   true,
			},
			hostOK:   outcome{Err: "PathError(write)/ErrDeadlineExceeded", N: 1024},
			simOK:    outcome{Err: "PathError(write)/ErrDeadlineExceeded", N: 1124},
			hostMiss: outcome{Err: "PathError(write)/ErrDeadlineExceeded", N: 1124}, // sim LOWER: the forbidden direction
			simMiss:  outcome{Err: "PathError(write)/ErrDeadlineExceeded", N: 1024},
		},
	}
	// Host-condition-gated rows skip (not fail) where their entry is
	// inapplicable: the umask fixture assumes 022, and the
	// close-in-flight row needs the host's FIN ordering to be reachable.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.key == "fs-create-umask" && hostUmask&0o777 != 0o022 {
				t.Skipf("host umask %04o != 0022: the recorded-shape fixture assumes 022", hostUmask)
			}
			if tc.key == "net-close-in-flight-fin-ordering" && !hostCloseInFlightCanFIN() {
				t.Skip("host never produced the close-vs-arrival FIN ordering; entry inapplicable")
			}
			fired := map[string]int{}
			if d := diffOutcomes([]op{tc.op}, []outcome{tc.hostOK}, []outcome{tc.simOK}, fullAllowlist(), fired); d != nil {
				t.Errorf("recorded shape was reported as a divergence: %+v", d)
			}
			if fired[tc.key] != 1 {
				t.Errorf("entry %q fired %d times, want 1", tc.key, fired[tc.key])
			}
			if d := diffOutcomes([]op{tc.op}, []outcome{tc.hostMiss}, []outcome{tc.simMiss}, fullAllowlist(), map[string]int{}); d == nil {
				t.Errorf("near-miss shape was silently allowlisted (entry %q over-matches)", tc.key)
			}
		})
	}
}

// A ≤PIPE_BUF guarded pipe write with sim.N > host.N is an ATOMICITY
// regression (small writes transfer all-or-nothing on both legs), not
// the recorded oversize fragmentation slack: the pipe-ring-byte-exact
// entry must not absorb it, and the differ must report it.
func TestDSTConformancePipeAtomicWriteRegressionNotAllowlisted(t *testing.T) {
	o := op{
		name:      "pipe-write(slot 4, 20 bytes, guard 150ms)",
		writeSize: 20,
		guarded:   true,
	}
	host := []outcome{{Err: "PathError(write)/ErrDeadlineExceeded", N: 0}}
	sim := []outcome{{Err: "PathError(write)/ErrDeadlineExceeded", N: 20}}
	d := diffOutcomes([]op{o}, host, sim, fullAllowlist(), map[string]int{})
	if d == nil {
		t.Fatal("a ≤PIPE_BUF atomicity regression (host n=0, sim n=20, both deadline-exceeded) was silently allowlisted")
	}
	if d.index != 0 {
		t.Fatalf("divergence reported at op %d, want 0", d.index)
	}
}

// The stale-entry detector must flag an applicable entry that never
// fired.
func TestDSTConformanceStaleAllowlistEntryDetected(t *testing.T) {
	stale := []allowEntry{
		{
			key:   "canary-stale",
			cite:  "canary",
			match: func(o op, host, sim outcome) bool { return false },
		},
		{
			key:        "canary-inapplicable",
			cite:       "canary",
			applicable: func() bool { return false },
			match:      func(o op, host, sim outcome) bool { return false },
		},
	}
	got := staleAllowlistEntries(stale, map[string]int{})
	if !slices.Equal(got, []string{"canary-stale"}) {
		t.Fatalf("stale detection returned %v, want exactly [canary-stale] (never-fired applicable entry flagged, inapplicable entry skipped)", got)
	}
	if got := staleAllowlistEntries(stale, map[string]int{"canary-stale": 2}); len(got) != 0 {
		t.Fatalf("a fired entry was reported stale: %v", got)
	}
}

// errClass is the false-negative net's normalizer: if it collapsed
// distinct identities (or everything) to one class, both legs would
// trivially agree. Pin the probe chain on concrete error shapes.
func TestDSTConformanceErrClassIdentity(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{io.EOF, "EOF"},
		{&fs.PathError{Op: "open", Path: "/x", Err: syscall.ENOENT}, "PathError(open)/errno:ENOENT"},
		{&fs.PathError{Op: "read", Path: "/x", Err: os.ErrClosed}, "PathError(read)/ErrClosed:file"},
		{&fs.PathError{Op: "write", Path: "/x", Err: os.ErrDeadlineExceeded}, "PathError(write)/ErrDeadlineExceeded"},
		{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.ENOTDIR}, "LinkError(rename)/errno:ENOTDIR"},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "OpError(dial)/errno:ECONNREFUSED"},
		{&net.OpError{Op: "close", Err: net.ErrClosed}, "OpError(close)/ErrClosed:net"},
		{&net.OpError{Op: "dial", Err: context.Canceled}, "OpError(dial)/ContextCanceled"},
		{syscall.EINVAL, "errno:EINVAL"},
		{errors.New("x unsupported under deterministic simulation"), "dst-unsupported"},
		// Unknown errors carry their message: two DISTINCT unknowns must
		// never fold to one matching class.
		{errors.New("mystery"), "opaque:mystery"},
		{errors.New("other mystery"), "opaque:other mystery"},
	}
	for _, tc := range cases {
		if got := errClass(tc.err); got != tc.want {
			t.Errorf("errClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	if got := panicClass("os: Fd on a simulated file: filesystem operation unsupported under deterministic simulation"); got != "panic:dst-unsupported" {
		t.Errorf("panicClass(fence panic) = %q, want panic:dst-unsupported", got)
	}
}

// The sim leg must actually run inside a simulation: were runOpsSim ever
// to execute host-side (losing the simulation.Run wrapper), every domain
// sweep would compare the host with itself and the net would be dead.
// FileInfo.Sys() discriminates the legs independently of the environment:
// nil for a simulated node, *syscall.Stat_t on the host.
func TestDSTConformanceSimLegIsSimulated(t *testing.T) {
	probe := []op{fsStatSys("")} // the world root itself
	sim := runOpsSim(t, 1, probe)
	if sim[0].State != "sys=nil" {
		t.Fatalf("sim-leg root stat = %+v, want the simulated sys=nil shape", sim[0])
	}
	host := runOpsHost(t, probe)
	if host[0].State != "sys=stat" {
		t.Fatalf("host-leg root stat = %+v, want the real sys=stat shape", host[0])
	}
}

// The op generators must be seed-deterministic: the harness itself is
// reproducible (same seed, same sequence — the reproducer contract).
func TestDSTConformanceGeneratorDeterminism(t *testing.T) {
	for _, seed := range []uint64{1, 2, 42} {
		if a, b := opNames(genFSOps(seed, 200)), opNames(genFSOps(seed, 200)); !slices.Equal(a, b) {
			t.Errorf("fs generator diverged from itself at seed %d", seed)
		}
		if a, b := opNames(genPipeOps(seed, 120)), opNames(genPipeOps(seed, 120)); !slices.Equal(a, b) {
			t.Errorf("pipe generator diverged from itself at seed %d", seed)
		}
		if a, b := opNames(genTCPOps(seed, 80)), opNames(genTCPOps(seed, 80)); !slices.Equal(a, b) {
			t.Errorf("tcp generator diverged from itself at seed %d", seed)
		}
	}
	// Distinct seeds must actually vary the sequence (a constant
	// generator would silently shrink the sweep to one sequence).
	if slices.Equal(opNames(genFSOps(1, 200)), opNames(genFSOps(2, 200))) {
		t.Error("fs generator emitted identical sequences for different seeds")
	}
}

// The sim leg must be same-seed stable: two runs of the same sequence
// under the same seed yield identical transcripts (outcome-for-outcome,
// virtual deadlines included).
func TestDSTConformanceSimSameSeedStable(t *testing.T) {
	seed := uint64(3)
	fsOps := genFSOps(seed, 150)
	if a, b := runOpsSim(t, seed, fsOps), runOpsSim(t, seed, fsOps); !slices.Equal(a, b) {
		t.Error("fs sim leg transcript differs across same-seed runs")
	}
	pipeOps := genPipeOps(seed, 100)
	if a, b := runOpsSim(t, seed, pipeOps), runOpsSim(t, seed, pipeOps); !slices.Equal(a, b) {
		t.Error("pipe sim leg transcript differs across same-seed runs")
	}
	tcpOps := genTCPOps(seed, 60)
	if a, b := runOpsSim(t, seed, tcpOps), runOpsSim(t, seed, tcpOps); !slices.Equal(a, b) {
		t.Error("tcp sim leg transcript differs across same-seed runs")
	}
}
