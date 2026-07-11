// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"
)

// Per-layer determinism over a systematic seed sweep. The existing per-feature
// suites and the runtime's testprog determinism tests are mostly single/few-seed;
// these sweep many seeds and ISOLATE one layer (so a regression is localized, and
// the concurrent declaration paths are swept under -race). The seed is DST's only
// input, so a 0..N loop is the exhaustive form of "fuzz the seed" — true `-fuzz`
// can't run on a GOROOT package. Whole-stack composition is swept by
// TestDSTModelDeterminism.

const dstDeterminismSeeds = 256

// dstScheduleProgram exercises ONLY the L0 substrate — goroutine scheduling, the
// per-g math/rand stream, and map iteration order — with no Host/Process/clock/
// identity/memory. The trace captures both the values (RNG/map) and the order
// (scheduling), so one string pins L0 determinism.
func dstScheduleProgram(seed uint64) string {
	var b strings.Builder
	Run(seed, func() {
		var mu sync.Mutex
		emit := func(s string) { mu.Lock(); b.WriteString(s); b.WriteByte(';'); mu.Unlock() }
		ch := make(chan int)
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := map[int]int{}
				r := rand.Intn(1000) // per-g math/rand stream — DST-seeded
				for k := 0; k < 5; k++ {
					m[k] = rand.Intn(100)
				}
				sum := 0
				for k, v := range m { // map iteration order — DST-seeded
					sum += k*100 + v
				}
				ch <- r
				emit(fmt.Sprintf("w%d=%d/sum%d", i, r, sum))
			}(i)
		}
		go func() {
			tot := 0
			for i := 0; i < 5; i++ {
				tot += <-ch
			}
			emit(fmt.Sprintf("tot=%d", tot))
		}()
		wg.Wait()
	})
	return b.String()
}

// TestDSTScheduleDeterminism: for every seed in the sweep, the L0 substrate replays
// exactly — scheduling order, per-g RNG draws, and map iteration order all reproduce.
func TestDSTScheduleDeterminism(t *testing.T) {
	for seed := uint64(0); seed < dstDeterminismSeeds; seed++ {
		a, b := dstScheduleProgram(seed), dstScheduleProgram(seed)
		if a != b {
			t.Fatalf("L0 scheduling/RNG non-deterministic at seed %d:\n a=%s\n b=%s", seed, a, b)
		}
		if strings.Count(a, ";") != 6 { // 5 workers + collector
			t.Fatalf("seed %d: trace has %d entries, want 6:\n%s", seed, strings.Count(a, ";"), a)
		}
	}
}

// dstIdentityProgram exercises ONLY the identity surface — concurrent Host/Process
// declarations and their identity readings (hostname, pid), with no
// clock/memory/filesystem use. It
// re-stresses the concurrent identity-table path (the dstSetHostIdent serialization)
// across the seed sweep under -race.
func dstIdentityProgram(seed uint64) string {
	var b strings.Builder
	Run(seed, func() {
		var mu sync.Mutex
		emit := func(s string) { mu.Lock(); b.WriteString(s); b.WriteByte(';'); mu.Unlock() }
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				Host(fmt.Sprintf("h%d", i), HostConfig{}, func() {
					Process(fmt.Sprintf("p%d", i), func() {
						hn, _ := os.Hostname()
						emit(fmt.Sprintf("h%d=%s/pid%d", i, hn, os.Getpid()))
					})
				})
			}(i)
		}
		wg.Wait()
	})
	return b.String()
}

// TestDSTIdentityDeterminismSweep: for every seed in the sweep, concurrent
// Host/Process identity assignment replays exactly (host ids, pids, hostnames, and
// the order). Distinct from identity_test.go's TestDSTIdentityDeterminism, which
// checks the per-host/process values at one seed; this sweeps the seed space and
// stresses the concurrent declaration path.
func TestDSTIdentityDeterminismSweep(t *testing.T) {
	distinct := map[string]bool{}
	for seed := uint64(0); seed < dstDeterminismSeeds; seed++ {
		a, b := dstIdentityProgram(seed), dstIdentityProgram(seed)
		if a != b {
			t.Fatalf("identity surface non-deterministic at seed %d:\n a=%s\n b=%s", seed, a, b)
		}
		distinct[a] = true
	}
	// The seed must steer the interleaving (non-vacuous): the trace order varies.
	if len(distinct) < 2 {
		t.Errorf("identity trace identical across %d seeds; the seed must vary the interleaving", dstDeterminismSeeds)
	}
}
