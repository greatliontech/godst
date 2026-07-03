// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"math"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestDSTClockExtremeOffsets exercises the wall-offset arithmetic at the edges —
// sub-nanosecond, hours, and multi-day offsets in both directions, several in one
// run. The reading stays exactly base+offset (the year-2000 base keeps even a
// multi-day negative offset's wall positive, so the sec/nsec split is well-formed).
func TestDSTClockExtremeOffsets(t *testing.T) {
	cases := []struct {
		name string
		off  time.Duration
	}{
		{"nsP", time.Nanosecond}, {"nsM", -time.Nanosecond},
		{"hrP", time.Hour}, {"hrM", -time.Hour},
		{"bigP", 100 * 24 * time.Hour}, {"bigM", -10 * 24 * time.Hour},
	}
	Run(1, func() {
		base := time.Now() // root, unskewed; no timer fires, so the base is shared
		for _, c := range cases {
			var got time.Duration
			Host(c.name, HostConfig{Clock: Skew(c.off)}, func() { got = time.Now().Sub(base) })
			if got != c.off {
				t.Errorf("offset %v on host %q: reading - base = %v, want %v", c.off, c.name, got, c.off)
			}
		}
	})
}

// TestDSTIdentityManyDistinct is a scale property for identity: N distinct bare
// processes yield N distinct hostnames (each an implicit host named after the
// process) and N distinct pids — the per-host/per-process keying does not collide as
// the node count grows.
func TestDSTIdentityManyDistinct(t *testing.T) {
	const N = 64
	hostnames := map[string]bool{}
	pids := map[int]bool{}
	Run(1, func() {
		for i := 0; i < N; i++ {
			Process("n"+strconv.Itoa(i), func() {
				hostnames[hostname()] = true
				pids[os.Getpid()] = true
			})
		}
	})
	if len(hostnames) != N {
		t.Errorf("got %d distinct hostnames across %d processes, want %d", len(hostnames), N, N)
	}
	if len(pids) != N {
		t.Errorf("got %d distinct pids across %d processes, want %d", len(pids), N, N)
	}
}

// TestDSTMemColocatedIndependent verifies that two processes SHARING a host still
// have independent allocation counters — the metric is keyed by process, not host,
// so co-location (shared filesystem/network) does not merge memory accounting.
func TestDSTMemColocatedIndependent(t *testing.T) {
	var aBytes, bBytes int64
	Run(1, func() {
		Host("h", HostConfig{}, func() {
			Process("a", func() {
				memSink = make([]byte, 8<<20)
				aBytes = procBytes()
			})
			Process("b", func() { bBytes = procBytes() }) // allocates ~nothing of its own
		})
	})
	if aBytes < 8<<20 {
		t.Errorf("process a accounted %d bytes, want >= 8 MB", aBytes)
	}
	if bBytes >= aBytes {
		t.Errorf("co-located process b (%d) >= a (%d); counters must stay independent on a shared host", bBytes, aBytes)
	}
}

// TestDSTClockSkewBoundary pins the wall-representability boundary (docs/dst/faults.md
// "Clock faults"): a far-FUTURE skew saturates at the farthest int64-ns-representable
// wall time (real kernels accept post-2262 times; this representation cannot — the
// saturation is deterministic and never wraps the sign), while a skew or step that
// would take the wall before the EPOCH is rejected with a panic at application, as
// settimeofday rejects a pre-epoch wall clock — no real machine can hold one, and a
// silently floored wall would freeze the host's clock (Sleep observably taking zero
// host time, the timer-early false-positive class).
func TestDSTClockSkewBoundary(t *testing.T) {
	Run(1, func() {
		base := time.Now()
		var fwd time.Time
		Host("far-future", HostConfig{Clock: Skew(time.Duration(math.MaxInt64))}, func() { fwd = time.Now() })
		if fwd.Before(base) {
			t.Errorf("Skew(MaxInt64) host reads %v, before base %v — the wall wrapped negative instead of saturating", fwd, base)
		}
		var hostPanic, stepPanic any
		func() {
			defer func() { hostPanic = recover() }()
			Host("far-past", HostConfig{Clock: Skew(time.Duration(math.MinInt64))}, func() {})
		}()
		if hostPanic == nil {
			t.Errorf("Skew(MinInt64) did not panic — a pre-epoch wall must be rejected at application")
		}
		func() {
			defer func() { stepPanic = recover() }()
			Host("stepped", HostConfig{}, func() {})
			StepClock("stepped", time.Duration(math.MinInt64))
		}()
		if stepPanic == nil {
			t.Errorf("StepClock(MinInt64) did not panic — a pre-epoch wall must be rejected at application")
		}
		// A rejected step applies nothing: the host still reads base.
		var after time.Time
		Host("stepped", HostConfig{}, func() { after = time.Now() })
		if !after.Equal(time.Now()) {
			t.Errorf("rejected StepClock left partial state: host reads %v, want base %v", after, time.Now())
		}
	})
}

// TestDSTClockDriftFoldSaturates: DriftClock's fold of drift-so-far into the offset is
// a wall-application point like any other — with an extreme accepted skew (far-future,
// stored raw) plus accumulated drift, a plain add would wrap the offset negative and
// resurrect the pre-epoch wall the representability boundary forbids. The fold
// saturates instead: the host stays pinned at the far representable end.
func TestDSTClockDriftFoldSaturates(t *testing.T) {
	Run(1, func() {
		read := make(chan struct{})
		done := make(chan struct{})
		var got time.Time
		Host("h", HostConfig{Clock: Skew(time.Duration(math.MaxInt64)).WithDrift(1_000_000_000)}, func() {
			go func() { // survives the fold; reads through the SAME incarnation's offset
				<-read
				got = time.Now()
				close(done)
			}()
		})
		time.Sleep(time.Second) // root: accumulate drift on h
		DriftClock("h", 0)      // fold drift-so-far into the already-extreme offset
		base := time.Now()
		close(read)
		<-done
		_ = base
		// Exact pin: the saturated wall is the farthest int64-ns-representable time,
		// sec = MaxInt64/1e9, nsec = MaxInt64%1e9. A wrapped offset instead produces a
		// corrupted sec/nsec encoding (observed: a bogus year-2157 reading) — merely
		// asserting got.After(base) would let that corruption pass.
		const ns = int64(1_000_000_000)
		if got.Unix() != math.MaxInt64/ns || int64(got.Nanosecond()) != math.MaxInt64%ns {
			t.Errorf("far-future host reads %v (unix %d/%d), want the saturated wall (unix %d/%d) — the fold must saturate, never wrap", got, got.Unix(), got.Nanosecond(), int64(math.MaxInt64)/ns, int64(math.MaxInt64)%ns)
		}
	})
}
