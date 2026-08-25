// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"os"
	"runtime"
	"testing"
	"time"
	_ "unsafe" // for go:linkname
)

//go:linkname dstFileStateStats os.dstFileStateStats
func dstFileStateStats() (next, free int)

// TestDSTFileStateSweptAtRunTeardown pins the out-of-line state
// tables' reclamation wiring end to end: files opened inside a
// simulation register rows, and the run-teardown sweep
// (dstFSRunTeardown, host context) must recycle collected rows'
// indexes, so repeated churn rounds reuse slots instead of growing the
// table's high-water index round after round (os/dst_filestate.go).
// The high-water index is the discriminating observable: it is
// order-independent under other tests' leftovers, where a bare
// free-list count is not — with the sweep, R rounds of K registrations
// grow it by at most ~K; without it, by R*K.
func TestDSTFileStateSweptAtRunTeardown(t *testing.T) {
	const (
		churn  = 64
		rounds = 4
	)
	baseNext, _ := dstFileStateStats()
	deadline := time.Now().Add(60 * time.Second)
	for r := 0; r < rounds; r++ {
		Run(1, func() {
			Host("h", HostConfig{}, func() {
				for i := 0; i < churn; i++ {
					f, err := os.Create("/sweep-probe")
					if err != nil {
						panic(err)
					}
					if err := f.Close(); err != nil {
						panic(err)
					}
				}
			})
		})
		// The rows die once the run's files are collected; the next
		// round's teardown recycles them. Converge each round so a
		// slow collection cannot masquerade as unbounded growth.
		for {
			runtime.GC()
			Run(2, func() { Host("h", HostConfig{}, func() {}) })
			if _, free := dstFileStateStats(); free >= churn {
				break
			}
			if time.Now().After(deadline) {
				next, free := dstFileStateStats()
				t.Fatalf("round %d: rows never swept at run teardown (next=%d free=%d)", r, next, free)
			}
		}
	}
	next, free := dstFileStateStats()
	if grew := next - baseNext; grew >= rounds*churn/2 {
		t.Fatalf("high-water index grew by %d over %d rounds of %d registrations (free=%d) — teardown sweeps are not recycling rows", grew, rounds, churn, free)
	}
}
