// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// allocSink heap-allocates n bytes and keeps them live without writing any shared
// state, so concurrent process goroutines can use it (unlike the package-level
// memSink, which the sequential mem tests share). The bytes accrue to the calling
// goroutine's process allocation counter.
func allocSink(n int) {
	s := make([]byte, n)
	s[0] = 1
	runtime.KeepAlive(s)
}

// dstModelProgram runs a small distributed system that exercises the whole L2
// substrate at once — multiple hosts with distinct clock skew / NumCPU / hostname,
// co-located processes (per-host FS, per-process pid + memory), and concurrent
// goroutines that synchronize over an unbuffered channel and append to a shared log
// under a mutex. It returns the log: a trace of per-node observables AND their
// scheduling order, so one string captures both value-determinism (the features)
// and order-determinism (the scheduler).
func dstModelProgram(seed uint64) string {
	names := []string{"alpha", "beta", "gamma"}
	skews := []time.Duration{25 * time.Millisecond, -40 * time.Millisecond, 0}
	cpus := []int{2, 4, 1}

	var b strings.Builder
	Run(seed, func() {
		var mu sync.Mutex
		emit := func(s string) { mu.Lock(); b.WriteString(s); b.WriteByte('\n'); mu.Unlock() }
		ch := make(chan int) // unbuffered: maximal send/recv synchronization order-sensitivity
		var wg sync.WaitGroup

		for hi := range names {
			wg.Add(1)
			go func(hi int) {
				defer wg.Done()
				Host(names[hi], HostConfig{Clock: Skew(skews[hi]), Hostname: names[hi], NumCPU: cpus[hi]}, func() {
					for pi := 0; pi < 2; pi++ {
						wg.Add(1)
						go func(pi int) {
							defer wg.Done()
							Process(fmt.Sprintf("%s-p%d", names[hi], pi), func() {
								hn, _ := os.Hostname()
								allocSink((hi + 1) * (pi + 1) * 4096)
								os.WriteFile("/data", []byte(hn), 0o644)
								rb, _ := os.ReadFile("/data")
								ch <- os.Getpid()
								// NOTE: per-process memory (procBytes) is deliberately NOT in this
								// byte-exact determinism trace — it carries sub-observable
								// runtime-pool-refill noise (the per-process analogue of the GC's
								// DST-MEM-1), so it is not a byte-deterministic observable. Memory is
								// still allocated (allocSink) and accounting is checked separately
								// (TestDSTModelIntegration's nd.mem>0, the mem_test suite).
								emit(fmt.Sprintf("host=%s pid=%d cpu=%d now=%d file=%s",
									hn, os.Getpid(), runtime.NumCPU(), time.Now().UnixNano(), rb))
							})
						}(pi)
					}
				})
			}(hi)
		}
		// A collector rendezvous with all six process sends, then logs the total.
		go func() {
			sum := 0
			for i := 0; i < 6; i++ {
				sum += <-ch
			}
			emit(fmt.Sprintf("pidsum=%d", sum))
		}()
		wg.Wait()
	})
	return b.String()
}

// TestDSTModelDeterminism is the backbone property: for every seed in the sweep, the
// whole composed model (scheduling + clock + identity + FS + memory) replays exactly
// — two runs at the same seed produce a byte-identical trace, including the order in
// which concurrent goroutines logged. (The seed is DST's only input, so a 0..N loop
// is the exhaustive form of "fuzz the seed"; true `-fuzz` can't run on a GOROOT
// package.)
func TestDSTModelDeterminism(t *testing.T) {
	for seed := uint64(0); seed < 64; seed++ {
		a := dstModelProgram(seed)
		b := dstModelProgram(seed)
		if a != b {
			t.Fatalf("model is non-deterministic at seed %d:\n--- run a ---\n%s--- run b ---\n%s", seed, a, b)
		}
		// Sanity: the trace is the six process lines + the collector, so the
		// determinism check is not vacuously comparing empty/degenerate strings.
		if got := strings.Count(a, "\n"); got != 7 {
			t.Fatalf("seed %d: trace has %d lines, want 7 (6 processes + collector):\n%s", seed, got, a)
		}
	}
}

// TestDSTModelSeedVaries guards against a vacuous determinism: the seed must
// actually steer the run. Across a spread of seeds the model's traces are not all
// identical (the scheduler interleaving — hence the log order and the rendezvous
// order — changes with the seed).
func TestDSTModelSeedVaries(t *testing.T) {
	seen := map[string]bool{}
	for seed := uint64(1); seed <= 16; seed++ {
		seen[dstModelProgram(seed)] = true
	}
	if len(seen) < 2 {
		t.Errorf("the model produced one trace across 16 seeds; the seed must vary the interleaving")
	}
}

// TestDSTModelIntegration asserts the composed observables are individually correct,
// not merely self-consistent: across three hosts each running one process, the
// hostname/NumCPU are the configured per-host values, the clock is skewed by the
// configured offset relative to a common base instant, the filesystem is per-host
// isolated (own files only, checked via the read-only HostFS inspector), memory is
// attributed per process, and pids are distinct.
func TestDSTModelIntegration(t *testing.T) {
	type node struct {
		host string
		cpu  int
		pid  int
		wall time.Time
		mem  int64
		saw  string
	}
	cfgs := []struct {
		name string
		skew time.Duration
		cpu  int
	}{
		{"db1", 30 * time.Millisecond, 8},
		{"db2", -15 * time.Millisecond, 2},
		{"db3", 0, 1},
	}
	var base time.Time
	nodes := map[string]*node{}
	isolation := map[string]string{}
	var mu sync.Mutex
	Run(1, func() {
		base = time.Now() // root, unskewed; no timer fires in this program, so the
		// bubble clock does not advance and every host reads the same base instant.
		var wg sync.WaitGroup
		for _, c := range cfgs {
			wg.Add(1)
			go func(name string, skew time.Duration, cpu int) {
				defer wg.Done()
				Host(name, HostConfig{Clock: Skew(skew), Hostname: name, NumCPU: cpu}, func() {
					Process(name+"-proc", func() {
						hn, _ := os.Hostname()
						os.WriteFile("/shared", []byte("from-"+hn), 0o644)
						os.WriteFile("/"+hn, []byte("x"), 0o644) // host-private crumb
						sb, _ := os.ReadFile("/shared")
						allocSink(cpu * 8192)
						n := &node{host: hn, cpu: runtime.NumCPU(), pid: os.Getpid(), wall: time.Now(), mem: procBytes(), saw: string(sb)}
						mu.Lock()
						nodes[name] = n
						mu.Unlock()
					})
				})
			}(c.name, c.skew, c.cpu)
		}
		wg.Wait()
		// Cross-host FS isolation via the read-only HostFS inspector: each host's
		// tree holds only its own crumb file, none of the others'.
		for _, owner := range cfgs {
			for _, other := range cfgs {
				_, err := fs.ReadFile(HostFS(owner.name), other.name)
				if present, want := err == nil, owner.name == other.name; present != want {
					isolation[owner.name] += fmt.Sprintf(" crumb %q present=%v want=%v;", other.name, present, want)
				}
			}
		}
	})

	if len(nodes) != len(cfgs) {
		t.Fatalf("captured %d/%d nodes", len(nodes), len(cfgs))
	}
	pids := map[int]string{}
	for _, c := range cfgs {
		nd := nodes[c.name]
		if nd == nil {
			t.Errorf("%s: not captured", c.name)
			continue
		}
		if nd.host != c.name {
			t.Errorf("%s: os.Hostname()=%q, want %q", c.name, nd.host, c.name)
		}
		if nd.cpu != c.cpu {
			t.Errorf("%s: NumCPU=%d, want %d", c.name, nd.cpu, c.cpu)
		}
		if got := nd.wall.Sub(base); got != c.skew {
			t.Errorf("%s: clock skew vs base = %v, want %v", c.name, got, c.skew)
		}
		if nd.saw != "from-"+c.name {
			t.Errorf("%s: /shared readback=%q, want %q (per-host FS)", c.name, nd.saw, "from-"+c.name)
		}
		if nd.mem <= 0 {
			t.Errorf("%s: per-process memory=%d, want >0", c.name, nd.mem)
		}
		if prev, dup := pids[nd.pid]; dup {
			t.Errorf("pid %d shared by %s and %s (pids must be distinct)", nd.pid, prev, c.name)
		}
		pids[nd.pid] = c.name
		if isolation[c.name] != "" {
			t.Errorf("%s: cross-host FS isolation breach:%s", c.name, isolation[c.name])
		}
	}
}
