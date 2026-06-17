// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
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
