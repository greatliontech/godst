// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package determinism

import (
	"bufio"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"internal/testenv"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/simulation"
	"time"
)

// sweepProgram is the representative in-bubble program: two skewed hosts, a
// TCP request/reply service under cross-host latency, per-process filesystem
// work with durability points, virtual-clock timers and tickers, channel and
// select races among workers, value-keyed map iteration, and math/rand plus
// crypto/rand draws. It returns a transcript carrying both the values it
// computed and the order in which its goroutines emitted them, with virtual
// timestamps — the full decision-relevant record one string can pin.
func sweepProgram(seed uint64) string {
	var b strings.Builder
	opts := simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 50 * time.Millisecond}}
	simulation.RunWith(seed, opts, func() {
		start := time.Now()
		var mu sync.Mutex
		emit := func(s string) {
			mu.Lock()
			b.WriteString(fmt.Sprintf("%d %s\n", time.Since(start).Nanoseconds(), s))
			mu.Unlock()
		}

		port := make(chan string, 1)
		done := make(chan struct{})

		// Host "svc": listener echoing requests upper-cased; per-host fs work
		// with fsync/rename durability points.
		go simulation.Host("svc", simulation.HostConfig{Clock: simulation.Skew(25 * time.Millisecond), Hostname: "svc", NumCPU: 2}, func() {
			ln, err := net.Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}
			_, p, _ := net.SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				defer close(done)
				for i := 0; i < 3; i++ {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					go func(c net.Conn) {
						defer c.Close()
						line, err := bufio.NewReader(c).ReadString('\n')
						if err != nil {
							return
						}
						fmt.Fprintf(c, "%s", strings.ToUpper(line))
					}(c)
				}
			}()
			simulation.Process("svc-log", func() {
				f, err := os.Create("/log.tmp")
				if err != nil {
					panic(err)
				}
				fmt.Fprintf(f, "boot %d", time.Now().UnixNano())
				f.Sync()
				f.Close()
				if err := os.Rename("/log.tmp", "/log"); err != nil {
					panic(err)
				}
				data, _ := os.ReadFile("/log")
				emit(fmt.Sprintf("svc-log pid=%d %s", os.Getpid(), data))
			})
		})

		// Host "cli": three clients dial the service, measure the virtual RTT.
		var cliWG sync.WaitGroup
		p := <-port
		for i := 0; i < 3; i++ {
			cliWG.Add(1)
			go func(i int) {
				defer cliWG.Done()
				simulation.Host("cli", simulation.HostConfig{Hostname: "cli", NumCPU: 4}, func() {
					t0 := time.Now()
					c, err := net.Dial("tcp", simulation.HostIP("svc")+":"+p)
					if err != nil {
						panic(err)
					}
					defer c.Close()
					fmt.Fprintf(c, "req-%d\n", i)
					reply, err := bufio.NewReader(c).ReadString('\n')
					if err != nil {
						panic(err)
					}
					emit(fmt.Sprintf("cli%d %s rtt=%d", i, strings.TrimSpace(reply), time.Since(t0).Nanoseconds()))
				})
			}(i)
		}

		// Workers: map iteration, rand draws, select races, virtual sleeps.
		ch := make(chan int)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := map[int]int{}
				for k := 0; k < 8; k++ {
					m[k*7+i] = rand.Intn(1000)
				}
				sum := 0
				for k, v := range m { // value-keyed iteration — DST-seeded
					sum += k*1000 + v
				}
				time.Sleep(time.Duration(rand.Intn(20)) * time.Millisecond) // virtual
				select {
				case ch <- sum:
					emit(fmt.Sprintf("w%d sent %d", i, sum))
				case <-time.After(5 * time.Millisecond): // virtual timer race
					emit(fmt.Sprintf("w%d timeout %d", i, sum))
				}
			}(i)
		}
		go func() {
			for i := 0; i < 4; i++ {
				select {
				case v := <-ch:
					emit(fmt.Sprintf("collect %d", v))
				case <-time.After(40 * time.Millisecond):
					emit("collect timeout")
				}
			}
		}()

		// Ticker on the virtual clock.
		tick := time.NewTicker(10 * time.Millisecond)
		go func() {
			defer tick.Stop()
			for i := 0; i < 3; i++ {
				ts := <-tick.C
				emit(fmt.Sprintf("tick %d", ts.UnixNano()))
			}
		}()

		// Deterministic entropy.
		var eb [8]byte
		crand.Read(eb[:])
		emit("entropy " + hex.EncodeToString(eb[:]))

		cliWG.Wait()
		wg.Wait()
		<-done
	})
	return b.String()
}

const sweepSeeds = 24

// TestSameSeedCrossRunTranscript: for every seed in the sweep, two in-process
// runs of the whole composed program produce byte-identical transcripts, and
// the seed steers the interleaving (distinct transcripts across seeds).
func TestSameSeedCrossRunTranscript(t *testing.T) {
	distinct := map[string]bool{}
	for seed := uint64(0); seed < sweepSeeds; seed++ {
		a, b := sweepProgram(seed), sweepProgram(seed)
		if a != b {
			t.Fatalf("same-seed transcripts differ at seed %d:\n--- a ---\n%s\n--- b ---\n%s", seed, a, b)
		}
		distinct[a] = true
	}
	if len(distinct) < 2 {
		t.Errorf("transcript identical across %d seeds; the seed must vary the run", sweepSeeds)
	}
}

// TestSweepChild is the cross-process half's child body: it runs the sweep
// program at the seed named in the environment and writes the transcript to
// the named file. Child-only.
func TestSweepChild(t *testing.T) {
	outf := os.Getenv("GO_DST_DETERMINISM_CHILD_OUT")
	if outf == "" {
		t.Skip("driver-launched child only")
	}
	seed, err := strconv.ParseUint(os.Getenv("GO_DST_DETERMINISM_SEED"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outf, []byte(sweepProgram(seed)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSameSeedCrossProcessTranscript: a fresh process at the same seed
// reproduces the in-process transcript byte-identically — under a changed
// timezone, locale, working directory, and GOMAXPROCS, and (implicitly, in
// every child) a fresh address-space layout. Any environmental influence
// reaching a decision inside a run breaks this.
func TestSameSeedCrossProcessTranscript(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the child re-execution sweep")
	}
	testenv.MustHaveExec(t)
	const seed = 11
	want := sweepProgram(seed)
	if want == "" {
		t.Fatal("empty in-process transcript")
	}
	dir := t.TempDir()
	perturbations := []struct {
		name string
		env  []string
		dir  bool
	}{
		{name: "baseline"},
		{name: "timezone", env: []string{"TZ=Australia/Lord_Howe"}},
		{name: "locale", env: []string{"LC_ALL=tr_TR.UTF-8", "LANG=tr_TR.UTF-8"}},
		{name: "cwd", dir: true},
		{name: "gomaxprocs-1", env: []string{"GOMAXPROCS=1"}},
		{name: "gomaxprocs-8", env: []string{"GOMAXPROCS=8"}},
	}
	for _, p := range perturbations {
		t.Run(p.name, func(t *testing.T) {
			out := dir + "/" + p.name
			cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestSweepChild$", "-test.count=1")
			cmd = testenv.CleanCmdEnv(cmd)
			cmd.Env = append(cmd.Env,
				"GO_DST_DETERMINISM_CHILD_OUT="+out,
				"GO_DST_DETERMINISM_SEED="+strconv.FormatUint(seed, 10))
			cmd.Env = append(cmd.Env, p.env...)
			if p.dir {
				cmd.Dir = t.TempDir()
			}
			o, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child (%s): %v\n%s", p.name, err, o)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Errorf("cross-process transcript differs under %s:\n--- in-process ---\n%s\n--- child ---\n%s", p.name, want, got)
			}
		})
	}
}
