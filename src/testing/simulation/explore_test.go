// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"internal/testenv"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

//go:linkname dstAccessYield runtime.dstAccessYield
func dstAccessYield(addr unsafe.Pointer, write bool)

//go:linkname dstAccessYieldFP runtime.dstAccessYieldFP
func dstAccessYieldFP() uint64

//go:linkname dstAccessYieldReset runtime.dstAccessYieldReset
func dstAccessYieldReset()

//go:linkname dstYieldPoint runtime.dstYieldPoint
func dstYieldPoint()

//go:linkname dstSetPostGoYield runtime.dstSetPostGoYield
func dstSetPostGoYield(enabled bool) bool

//go:linkname dstRunningPanicDefersFP runtime.dstRunningPanicDefersFP
func dstRunningPanicDefersFP() uint32

//go:linkname dstCurrentSeqFP runtime.dstCurrentSeqFP
func dstCurrentSeqFP() uint64

func assertUniqueEnabledSeqs(t *testing.T, tr exploreTrace) {
	t.Helper()
	for i, enabled := range tr.enabled {
		seen := map[uint64]bool{}
		for _, seq := range enabled {
			if seq == 0 {
				t.Fatalf("decision %d has unassigned seq in enabled set %v", i, enabled)
			}
			if seen[seq] {
				t.Fatalf("decision %d has duplicate seq %d in enabled set %v", i, seq, enabled)
			}
			seen[seq] = true
		}
	}
}

func TestExploreAccessLogOverflowReportsIncomplete(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	dstExploreInit(16, 64, 16, 0)
	x := 0
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		dstAccessYield(unsafe.Pointer(&x), true)
		x = 1
		return false
	})
	if !tr.overflow {
		t.Fatalf("access-log overflow did not mark trace incomplete")
	}
}

func TestReplayInstallsAccessForces(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("requires -race so manual dstAccessYield uses the shared-address filter")
	}

	x := 0
	sut := func() bool {
		dstAccessYield(unsafe.Pointer(&x), true)
		x = 1
		return false
	}

	dstExploreInit(64, 64, 64, 64)
	dstAccessYieldReset()
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, sut)
	unforced := dstAccessYieldFP()
	force := AccessForce{}
	for i := range tr.accSeq {
		if tr.accCount[i] != 0 && tr.accPC[i] != 0 {
			force = AccessForce{Seq: tr.accSeq[i], Count: tr.accCount[i], PCKey: tr.accPC[i]}
			break
		}
	}
	if force.Count == 0 {
		t.Fatalf("missing replayable access metadata: %#v", tr)
	}

	dstAccessYieldReset()
	failed, raced := Replay(1, Failure{AccessForces: []AccessForce{force}}, sut)
	if failed || raced {
		t.Fatalf("replay reported unexpected failure: failed=%v raced=%v", failed, raced)
	}
	if got := dstAccessYieldFP(); got <= unforced {
		t.Fatalf("Replay did not install the forced access yield: unforced=%d forced=%d", unforced, got)
	}
}

func TestExploreResetsScheduledIdentityAcrossRuns(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			dstYieldPoint()
		}()
		time.Sleep(time.Millisecond)
		wg.Wait()
		return false
	}
	for i := 0; i < 3; i++ {
		_, _, tr := runOnce(1, nil, map[accessForce]bool{}, sut)
		assertUniqueEnabledSeqs(t, tr)
		if seq := dstCurrentSeqFP(); seq != 0 {
			t.Fatalf("run %d left scheduled identity %d on the reused synctest root", i, seq)
		}
	}
}

func TestPublicExploreGuardsBeforeTraceInit(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	t.Cleanup(func() {
		dstExploreInit(exploreMaxDecisions, exploreMaxEnabledTotal, exploreMaxEdges, exploreMaxAccesses)
	})

	for _, tt := range []struct {
		name string
		call func()
	}{
		{
			name: "ExploreWith",
			call: func() {
				ExploreWith(1, ExploreOptions{MaxSchedules: 1}, func() bool { return false })
			},
		},
		{
			name: "Replay",
			call: func() {
				Replay(1, Failure{}, func() bool { return false })
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dstExploreInit(0, 0, 0, 0)
			panicked := false
			runActive.Store(true)
			func() {
				defer func() {
					runActive.Store(false)
					if v := recover(); v != nil {
						panicked = strings.Contains(panicString(v), "called while another simulation operation is active")
					}
				}()
				tt.call()
			}()
			if !panicked {
				t.Fatalf("%s did not reject overlap", tt.name)
			}

			x := 0
			_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
				dstAccessYield(unsafe.Pointer(&x), true)
				x++
				return false
			})
			if !tr.overflow {
				t.Fatalf("%s mutated trace buffers before rejecting overlap", tt.name)
			}
		})
	}
}

func TestRunWithRejectsInvalidOptionsBeforeActivation(t *testing.T) {
	type testCase struct {
		name string
		opts Options
		want string
	}
	cases := []testCase{
		{
			name: "unknown strategy",
			opts: Options{Strategy: Strategy(99)},
			want: "unknown Strategy",
		},
	}
	if strconv.IntSize > 32 {
		tooLarge := int(maxStrategyParam)
		tooLarge++
		cases = append(cases,
			testCase{
				name: "pct depth overflow",
				opts: Options{Strategy: PCT, Depth: tooLarge},
				want: "PCT Depth overflows",
			},
			testCase{
				name: "pct steps overflow",
				opts: Options{Strategy: PCT, Steps: tooLarge},
				want: "PCT Steps overflows",
			},
		)
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			var got string
			func() {
				defer func() {
					got = panicString(recover())
				}()
				RunWith(1, tt.opts, func() { called = true })
			}()
			if !strings.Contains(got, tt.want) {
				t.Fatalf("RunWith panic = %q, want substring %q", got, tt.want)
			}
			if called {
				t.Fatalf("RunWith called the SUT after rejecting %s", tt.name)
			}
			if runActive.Load() {
				t.Fatalf("RunWith left simulation active after rejecting %s", tt.name)
			}
		})
	}
}

func TestRunFatalExitsCaller(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	const helperEnv = "GO_WANT_SIMULATION_FATAL_HELPER=1"
	if os.Getenv("GO_WANT_SIMULATION_FATAL_HELPER") == "1" {
		Run(1, func() {
			t.Fatal("fatal inside simulation")
		})
		t.Fatal("simulation.Run returned after Fatal")
		return
	}

	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestRunFatalExitsCaller$", "-test.count=1")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, helperEnv)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper test passed unexpectedly:\n%s", out)
	}
	if !strings.Contains(string(out), "fatal inside simulation") {
		t.Fatalf("helper output missing simulation fatal:\n%s", out)
	}
	if strings.Contains(string(out), "panic:") {
		t.Fatalf("simulation.Run aborted by panic, want testing Goexit:\n%s", out)
	}
	if strings.Contains(string(out), "simulation.Run returned after Fatal") {
		t.Fatalf("simulation.Run returned after Fatal:\n%s", out)
	}
}

func TestTestFatalExitsCaller(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	const helperEnv = "GO_WANT_SIMULATION_TEST_FATAL_HELPER=1"
	if os.Getenv("GO_WANT_SIMULATION_TEST_FATAL_HELPER") == "1" {
		Test(t, 1, func(t *testing.T) {
			t.Fatal("fatal inside simulation test")
		})
		t.Fatal("simulation.Test returned after Fatal")
		return
	}

	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestTestFatalExitsCaller$", "-test.count=1")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, helperEnv)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper test passed unexpectedly:\n%s", out)
	}
	if !strings.Contains(string(out), "fatal inside simulation test") {
		t.Fatalf("helper output missing simulation Test fatal:\n%s", out)
	}
	if strings.Contains(string(out), "panic:") {
		t.Fatalf("simulation.Test aborted by panic, want testing Goexit:\n%s", out)
	}
	if strings.Contains(string(out), "simulation.Test returned after Fatal") {
		t.Fatalf("simulation.Test returned after Fatal:\n%s", out)
	}
}

func TestTestProvidesBubbleScopedT(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	cleanupDone := make(chan struct{}, 1)
	contextDone := make(chan struct{}, 1)
	Test(t, 1, func(t *testing.T) {
		cleanupCh := make(chan struct{})
		t.Cleanup(func() {
			close(cleanupCh)
		})
		go func() {
			<-cleanupCh
			cleanupDone <- struct{}{}
		}()
		go func() {
			<-t.Context().Done()
			contextDone <- struct{}{}
		}()
	})
	select {
	case <-cleanupDone:
	default:
		t.Fatalf("simulation.Test cleanup did not run inside the bubble")
	}
	select {
	case <-contextDone:
	default:
		t.Fatalf("simulation.Test context was not canceled before returning")
	}
}

func TestTestWithOptions(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	TestWith(t, 1, Options{Hostname: "sim-test", PID: 123, NumCPU: 2}, func(t *testing.T) {
		hostname, err := os.Hostname()
		if err != nil {
			t.Fatal(err)
		}
		if hostname != "sim-test" {
			t.Fatalf("os.Hostname() = %q, want sim-test", hostname)
		}
		if pid := os.Getpid(); pid != 123 {
			t.Fatalf("os.Getpid() = %d, want 123", pid)
		}
		if numCPU := runtime.NumCPU(); numCPU != 2 {
			t.Fatalf("runtime.NumCPU() = %d, want 2", numCPU)
		}
	})
}

func TestTestWithRejectsInvalidOptionsBeforeActivation(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "unknown strategy",
			opts: Options{Strategy: Strategy(99)},
			want: "TestWith unknown Strategy",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			var got string
			func() {
				defer func() {
					got = panicString(recover())
				}()
				TestWith(t, 1, tt.opts, func(*testing.T) { called = true })
			}()
			if !strings.Contains(got, tt.want) {
				t.Fatalf("TestWith panic = %q, want substring %q", got, tt.want)
			}
			if called {
				t.Fatalf("TestWith called the SUT after rejecting %s", tt.name)
			}
			if runActive.Load() {
				t.Fatalf("TestWith left simulation active after rejecting %s", tt.name)
			}
		})
	}
}

func TestExploreClocksModelStepZeroAccesses(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 2},
		accAddr:  []uintptr{0x1000, 0x1000},
		accWrite: []bool{true, false},
		accStep:  []int{0, 0},
	}
	clk, pidx := dporClocks(tr)
	if len(clk[0]) == 0 || len(clk[1]) == 0 {
		t.Fatalf("step-0 accesses were not clocked: %#v", clk)
	}
	if !dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("independent step-0 goroutines should be concurrent: %#v", clk)
	}

	traceClk, tracePidx := dporTraceClocks(tr)
	if dporConcurrent(traceClk, tracePidx, tr, 0, 1) {
		t.Fatalf("trace clocks did not order step-0 conflicting accesses: %#v", traceClk)
	}
}

func TestExploreRangeOverlapConflicts(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 2},
		accAddr:  []uintptr{0x1000, 0x1008},
		accSize:  []uintptr{16, 1},
		accPC:    []uintptr{1, 2},
		accCount: []uint64{1, 1},
		accWrite: []bool{true, false},
		accStep:  []int{0, 0},
	}
	clk, pidx := dporClocks(tr)
	if !dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("sync clocks should leave overlapping accesses concurrent: %#v", clk)
	}
	traceClk, tracePidx := dporTraceClocks(tr)
	if dporConcurrent(traceClk, tracePidx, tr, 0, 1) {
		t.Fatalf("trace clocks did not order overlapping range/scalar conflict: %#v", traceClk)
	}
	forces := map[accessForce]bool{}
	if !promoteAccessForces(tr, forces) {
		t.Fatalf("overlapping range/scalar conflict did not promote replay boundaries")
	}
	if len(forces) != 2 {
		t.Fatalf("overlapping conflict promoted %d forces, want 2: %#v", len(forces), forces)
	}
}

func TestExploreRangeAdjacentDoesNotConflict(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 2},
		accAddr:  []uintptr{0x1000, 0x1010},
		accSize:  []uintptr{16, 1},
		accPC:    []uintptr{1, 2},
		accCount: []uint64{1, 1},
		accWrite: []bool{true, false},
		accStep:  []int{0, 0},
	}
	traceClk, tracePidx := dporTraceClocks(tr)
	if !dporConcurrent(traceClk, tracePidx, tr, 0, 1) {
		t.Fatalf("adjacent range/scalar accesses should remain independent: %#v", traceClk)
	}
	forces := map[accessForce]bool{}
	if promoteAccessForces(tr, forces) {
		t.Fatalf("adjacent range/scalar accesses promoted replay boundaries: %#v", forces)
	}
}

func TestExploreClocksOrderSameStepEdgesAgainstAccesses(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 2},
		accAddr:  []uintptr{0x1000, 0x2000},
		accWrite: []bool{true, false},
		accStep:  []int{1, 2},
		edgeFrom: []uint64{1},
		edgeTo:   []uint64{2},
		edgeStep: []int{1},
		edgeAcc:  []int{0},
	}
	clk, pidx := dporClocks(tr)
	if !dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("edge before same-step parent access incorrectly ordered that access before child: %#v", clk)
	}
	traceClk, tracePidx := dporTraceClocks(tr)
	if !dporConcurrent(traceClk, tracePidx, tr, 0, 1) {
		t.Fatalf("trace edge before same-step parent access incorrectly ordered that access before child: %#v", traceClk)
	}

	tr.edgeAcc[0] = 1
	clk, pidx = dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("edge after same-step parent access did not order that access before child: %#v", clk)
	}
}

func TestExploreClocksModelSyncReleaseAcquire(t *testing.T) {
	tr := exploreTrace{
		accSeq:    []uint64{1, 2},
		accAddr:   []uintptr{0x1000, 0x1000},
		accWrite:  []bool{true, false},
		accStep:   []int{1, 3},
		syncKind:  []uint8{syncEventRelease, syncEventAcquire},
		syncID:    []uintptr{0x2000, 0x2000},
		syncSeq:   []uint64{1, 2},
		syncStep:  []int{2, 3},
		syncAcc:   []int{1, 1},
		syncOrd:   []int{1, 2},
		edgeOrder: nil,
	}
	clk, pidx := dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("release/acquire HB did not order protected accesses: %#v", clk)
	}
}

func TestExploreClocksUseReleaseSnapshot(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 1, 2},
		accAddr:  []uintptr{0x1000, 0x2000, 0x2000},
		accWrite: []bool{true, true, false},
		accStep:  []int{1, 3, 5},
		syncKind: []uint8{syncEventRelease, syncEventAcquire},
		syncID:   []uintptr{0x3000, 0x3000},
		syncSeq:  []uint64{1, 2},
		syncStep: []int{2, 4},
		syncAcc:  []int{1, 2},
		syncOrd:  []int{1, 2},
	}
	clk, pidx := dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, 0, 2) {
		t.Fatalf("release snapshot did not carry pre-release access to acquire: %#v", clk)
	}
	if !dporConcurrent(clk, pidx, tr, 1, 2) {
		t.Fatalf("acquire incorrectly observed releaser's post-release access: %#v", clk)
	}
}

func TestExploreClocksDistinguishSyncObjectAux(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 2},
		accAddr:  []uintptr{0x1000, 0x1000},
		accWrite: []bool{true, false},
		accStep:  []int{1, 3},
		syncKind: []uint8{syncEventRelease, syncEventAcquire},
		syncID:   []uintptr{0x2000, 0x2000},
		syncAux:  []uintptr{2, 1},
		syncSeq:  []uint64{1, 2},
		syncStep: []int{2, 3},
		syncAcc:  []int{1, 1},
		syncOrd:  []int{1, 2},
	}
	clk, pidx := dporClocks(tr)
	if !dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("sync events with the same id but different aux were incorrectly ordered: %#v", clk)
	}
}

func TestExploreRecordsCreateHBEdge(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	dstExploreInit(64, 64, 64, 0)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
		}()
		wg.Wait()
		return false
	})
	for i := range tr.edgeFrom {
		for j := range tr.edgeFrom {
			if tr.edgeFrom[i] == tr.edgeTo[j] && tr.edgeTo[i] == tr.edgeFrom[j] && tr.edgeStep[i] < tr.edgeStep[j] {
				return
			}
		}
	}
	t.Fatalf("goroutine creation did not record a parent->child HB edge before child wake: steps=%v acc=%v from=%v to=%v", tr.edgeStep, tr.edgeAcc, tr.edgeFrom, tr.edgeTo)
}

func TestExploreRecordsBufferedChannelHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("buffered channel HB events are emitted by dst-race sync hooks")
	}
	x := new(int)
	addr := uintptr(unsafe.Pointer(x))
	dstExploreInit(128, 512, 128, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		ch := make(chan struct{}, 1)
		var wg sync.WaitGroup
		dstAccessYield(unsafe.Pointer(x), true)
		*x = 1
		ch <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ch
			dstAccessYield(unsafe.Pointer(x), false)
			_ = *x
		}()
		wg.Wait()
		return false
	})
	release, acquire := false, false
	for _, k := range tr.syncKind {
		release = release || k == syncEventRelease
		acquire = acquire || k == syncEventAcquire
	}
	if !release || !acquire {
		t.Fatalf("buffered channel did not record sync release/acquire events: kind=%v", tr.syncKind)
	}
	write, read := -1, -1
	for i := range tr.accSeq {
		if tr.accAddr[i] != addr {
			continue
		}
		if tr.accWrite[i] && write < 0 {
			write = i
		}
		if !tr.accWrite[i] {
			read = i
		}
	}
	if write < 0 || read < 0 || tr.accSeq[write] == tr.accSeq[read] {
		t.Fatalf("missing buffered-channel protected access pair: write=%d read=%d seq=%v addr=%#x log=%#v", write, read, tr.accSeq, addr, tr)
	}
	clk, pidx := dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, write, read) {
		t.Fatalf("buffered channel send/receive HB did not order protected accesses: write=%d read=%d sync=%#v", write, read, tr.syncKind)
	}
}

func TestExploreRecordsBufferedChannelZeroSizeSlotIDs(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("buffered channel HB events are emitted by dst-race sync hooks")
	}
	dstExploreInit(128, 512, 128, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		ch := make(chan struct{}, 2)
		ch <- struct{}{}
		ch <- struct{}{}
		<-ch
		return false
	})
	releases := map[syncObjectKey]bool{}
	for i, k := range tr.syncKind {
		if k == syncEventRelease && tr.syncAux[i] != 0 {
			releases[syncObjectKey{id: tr.syncID[i], aux: tr.syncAux[i]}] = true
		}
	}
	if len(releases) < 2 {
		t.Fatalf("zero-sized buffered channel slots were not distinct sync objects: releases=%v ids=%v aux=%v kind=%v", releases, tr.syncID, tr.syncAux, tr.syncKind)
	}
}

func TestExploreRecordsFullBufferedChannelSenderRelease(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("buffered channel HB events are emitted by dst-race sync hooks")
	}
	x := new(int)
	addr := uintptr(unsafe.Pointer(x))
	sut := func() bool {
		ch := make(chan int, 1)
		ready := make(chan struct{})
		ch <- 0
		go func() {
			dstAccessYield(unsafe.Pointer(x), true)
			*x = 1
			ready <- struct{}{}
			ch <- 1
		}()
		<-ready
		time.Sleep(time.Nanosecond)
		if got := <-ch; got != 0 {
			t.Fatalf("first receive got %d, want initial buffered value", got)
		}
		if got := <-ch; got != 1 {
			t.Fatalf("second receive got %d, want blocked sender value", got)
		}
		return false
	}
	dstExploreInit(128, 512, 128, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, sut)
	senderSeq := uint64(0)
	for i := range tr.accSeq {
		if tr.accAddr[i] == addr && tr.accWrite[i] {
			senderSeq = tr.accSeq[i]
			break
		}
	}
	if senderSeq == 0 {
		t.Fatalf("missing sender access in trace: %#v", tr)
	}
	for i, k := range tr.syncKind {
		if k != syncEventRelease || tr.syncSeq[i] != senderSeq || tr.syncAux[i] == 0 {
			continue
		}
		step := tr.syncStep[i]
		if step > 0 && step-1 < len(tr.procs) && tr.procs[step-1] != senderSeq {
			return
		}
	}
	t.Fatalf("full-buffer receive did not record blocked sender release from receiver step: sender=%d procs=%v syncSeq=%v syncStep=%v syncAux=%v kind=%v", senderSeq, tr.procs, tr.syncSeq, tr.syncStep, tr.syncAux, tr.syncKind)
}

func TestExploreRecordsSelectBufferedChannelHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("buffered channel HB events are emitted by dst-race sync hooks")
	}
	dstExploreInit(128, 512, 128, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		ch := make(chan int, 1)
		other := make(chan int)
		select {
		case ch <- 1:
		case other <- 1:
		}
		select {
		case <-ch:
		case <-other:
		}
		return false
	})
	release := map[syncObjectKey]bool{}
	for i, k := range tr.syncKind {
		if tr.syncAux[i] == 0 {
			continue
		}
		obj := syncObjectKey{id: tr.syncID[i], aux: tr.syncAux[i]}
		if k == syncEventRelease {
			release[obj] = true
		}
		if k == syncEventAcquire && release[obj] {
			return
		}
	}
	t.Fatalf("select buffered send/receive did not record matching release/acquire: id=%v aux=%v kind=%v", tr.syncID, tr.syncAux, tr.syncKind)
}

func TestExploreRecordsUnbufferedChannelHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("unbuffered channel HB events are emitted by dst-race sync hooks")
	}
	for _, tt := range []struct {
		name string
		sut  func(unsafe.Pointer) func() bool
	}{
		{
			name: "SendToReceive",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						<-ch
						dstAccessYield(marker, false)
					}()
					go func() {
						defer wg.Done()
						runtime.Gosched()
						dstAccessYield(marker, true)
						ch <- struct{}{}
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "ReceiveToSendComplete",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						ch <- struct{}{}
						dstAccessYield(marker, false)
					}()
					go func() {
						defer wg.Done()
						runtime.Gosched()
						dstAccessYield(marker, true)
						<-ch
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "ParkedSenderToReceive",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						dstAccessYield(marker, true)
						ch <- struct{}{}
					}()
					go func() {
						defer wg.Done()
						runtime.Gosched()
						<-ch
						dstAccessYield(marker, false)
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "ParkedReceiverToSendComplete",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						dstAccessYield(marker, true)
						<-ch
					}()
					go func() {
						defer wg.Done()
						runtime.Gosched()
						ch <- struct{}{}
						dstAccessYield(marker, false)
					}()
					wg.Wait()
					return false
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr, _, readSeq := assertChannelHBTrace(t, tt.sut)
			releases, acquires := 0, 0
			readAcquire := false
			for i, k := range tr.syncKind {
				if tr.syncAux[i] != 0 {
					continue
				}
				switch k {
				case syncEventRelease:
					releases++
				case syncEventAcquire:
					acquires++
					readAcquire = readAcquire || tr.syncSeq[i] == readSeq
				}
			}
			if releases < 2 || acquires < 2 {
				t.Fatalf("unbuffered channel rendezvous did not record both racesync halves: releases=%d acquires=%d syncKind=%v syncAux=%v", releases, acquires, tr.syncKind, tr.syncAux)
			}
			if !readAcquire {
				t.Fatalf("unbuffered channel rendezvous did not record acquire on parked goroutine %d: syncKind=%v syncSeq=%v syncAux=%v", readSeq, tr.syncKind, tr.syncSeq, tr.syncAux)
			}
		})
	}
}

func TestExploreRecordsChannelCloseHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("channel close HB events are emitted by dst-race sync hooks")
	}
	for _, tt := range []struct {
		name string
		sut  func(unsafe.Pointer) func() bool
	}{
		{
			name: "ReceiveAfterClose",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						dstAccessYield(marker, true)
						close(ch)
					}()
					go func() {
						defer wg.Done()
						<-ch
						dstAccessYield(marker, false)
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "SelectReceiveAfterClose",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					other := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						dstAccessYield(marker, true)
						close(ch)
					}()
					go func() {
						defer wg.Done()
						select {
						case <-ch:
						case <-other:
						}
						dstAccessYield(marker, false)
					}()
					wg.Wait()
					return false
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertChannelHBTrace(t, tt.sut)
		})
	}
}

func assertChannelHBTrace(t *testing.T, sutForMarker func(unsafe.Pointer) func() bool) (exploreTrace, uint64, uint64) {
	t.Helper()
	marker := new(int)
	addr := uintptr(unsafe.Pointer(marker))
	sut := sutForMarker(unsafe.Pointer(marker))
	dstExploreInit(256, 4096, 512, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, sut)
	write, read := -1, -1
	for i := range tr.accSeq {
		if tr.accAddr[i] != addr {
			continue
		}
		if tr.accWrite[i] {
			write = i
		} else {
			read = i
		}
	}
	if write < 0 || read < 0 || tr.accSeq[write] == tr.accSeq[read] || write > read {
		t.Fatalf("missing ordered channel HB marker accesses: write=%d read=%d seq=%v addr=%#x log=%#v", write, read, tr.accSeq, addr, tr)
	}
	clk, pidx := dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, write, read) {
		t.Fatalf("channel HB did not order marker accesses: write=%d read=%d syncKind=%v syncSeq=%v syncID=%v syncAux=%v", write, read, tr.syncKind, tr.syncSeq, tr.syncID, tr.syncAux)
	}
	return tr, tr.accSeq[write], tr.accSeq[read]
}

func TestExploreRecordsMutexHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("mutex HB events are emitted by dst-race sync hooks")
	}
	mu := new(sync.Mutex)
	x := new(int)
	addr := uintptr(unsafe.Pointer(x))
	dstExploreInit(128, 512, 128, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		mu.Lock()
		dstAccessYield(unsafe.Pointer(x), true)
		*x = 1
		mu.Unlock()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			dstAccessYield(unsafe.Pointer(x), false)
			_ = *x
			mu.Unlock()
		}()
		wg.Wait()
		return false
	})
	release, acquire := false, false
	for _, k := range tr.syncKind {
		release = release || k == syncEventRelease
		acquire = acquire || k == syncEventAcquire
	}
	if !release || !acquire {
		t.Fatalf("mutex did not record sync release/acquire events: kind=%v", tr.syncKind)
	}
	write, read := -1, -1
	for i := range tr.accSeq {
		if tr.accAddr[i] != addr {
			continue
		}
		if tr.accWrite[i] && write < 0 {
			write = i
		}
		if !tr.accWrite[i] {
			read = i
		}
	}
	if write < 0 || read < 0 || tr.accSeq[write] == tr.accSeq[read] {
		t.Fatalf("missing mutex protected access pair: write=%d read=%d seq=%v addr=%#x log=%#v", write, read, tr.accSeq, addr, tr)
	}
	clk, pidx := dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, write, read) {
		t.Fatalf("mutex Unlock/Lock HB did not order protected accesses: write=%d read=%d sync=%#v", write, read, tr.syncKind)
	}
}

func TestExploreLiveSyncHBFiltersProtectedAccesses(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("live sync HB filtering is active under dst-race access hooks")
	}
	for _, tt := range []struct {
		name string
		sut  func(unsafe.Pointer, bool) func() bool
	}{
		{
			name: "Mutex",
			sut: func(marker unsafe.Pointer, record bool) func() bool {
				return func() bool {
					var mu sync.Mutex
					var wg sync.WaitGroup
					wg.Add(2)
					for g := 0; g < 2; g++ {
						go func() {
							defer wg.Done()
							for i := 0; i < 3; i++ {
								mu.Lock()
								if record {
									dstAccessYield(marker, true)
								}
								mu.Unlock()
							}
						}()
					}
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "ChannelToken",
			sut: func(marker unsafe.Pointer, record bool) func() bool {
				return func() bool {
					ch := make(chan struct{}, 1)
					ch <- struct{}{}
					var wg sync.WaitGroup
					wg.Add(2)
					for g := 0; g < 2; g++ {
						go func() {
							defer wg.Done()
							for i := 0; i < 3; i++ {
								<-ch
								if record {
									dstAccessYield(marker, true)
								}
								ch <- struct{}{}
							}
						}()
					}
					wg.Wait()
					return false
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			marker := new(int)
			count := func(record bool) uint64 {
				dstAccessYieldReset()
				dstExploreInit(512, 8192, 1024, 4096)
				_, _, tr := runOnce(1, nil, map[accessForce]bool{}, tt.sut(unsafe.Pointer(marker), record))
				if tr.overflow {
					t.Fatalf("trace overflowed while measuring %s live HB filtering: %#v", tt.name, tr)
				}
				return dstAccessYieldFP()
			}
			baseline := count(false)
			withMarker := count(true)
			if withMarker != baseline {
				t.Fatalf("%s HB-ordered marker accesses added live yield points: baseline=%d withMarker=%d", tt.name, baseline, withMarker)
			}
		})
	}
}

func TestExploreRWMutexFailedTryLockDoesNotRecordHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("RWMutex HB events are emitted by dst-race sync hooks")
	}
	marker := new(int)
	addr := uintptr(unsafe.Pointer(marker))
	sut := func() bool {
		var rw sync.RWMutex
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			rw.RLock()
			runtime.Gosched()
			rw.RUnlock()
		}()
		go func() {
			defer wg.Done()
			if !rw.TryLock() {
				dstAccessYield(unsafe.Pointer(marker), false)
				return
			}
			rw.Unlock()
		}()
		wg.Wait()
		return false
	}

	dstExploreInit(256, 4096, 512, 512)
	stack := [][]uint64{nil}
	seen := map[string]bool{}
	for len(stack) > 0 && len(seen) < 200 {
		prefix := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		key := encodePrefix(prefix)
		if seen[key] {
			continue
		}
		seen[key] = true
		_, _, tr := runOnce(1, prefix, map[accessForce]bool{}, sut)
		read := -1
		for i := range tr.accSeq {
			if tr.accAddr[i] != addr {
				continue
			}
			if !tr.accWrite[i] {
				read = i
			}
		}
		if read >= 0 {
			seq := tr.accSeq[read]
			for i, k := range tr.syncKind {
				if tr.syncSeq[i] == seq {
					t.Fatalf("failed public RWMutex.TryLock recorded DST HB event kind=%d id=%#x for goroutine %d: syncKind=%v syncSeq=%v syncID=%v", k, tr.syncID[i], seq, tr.syncKind, tr.syncSeq, tr.syncID)
				}
			}
			return
		}
		for i := len(prefix); i < len(tr.procs); i++ {
			for _, g := range tr.enabled[i] {
				if g == tr.procs[i] {
					continue
				}
				child := make([]uint64, i+1)
				copy(child, tr.procs[:i])
				child[i] = g
				stack = append(stack, child)
			}
		}
	}
	t.Fatalf("failed to reach a trace with reader-caused failed RWMutex.TryLock after %d schedules", len(seen))
}

func TestExploreRecordsRWMutexHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("RWMutex HB events are emitted by dst-race sync hooks")
	}
	for _, tt := range []struct {
		name string
		sut  func(unsafe.Pointer) func() bool
	}{
		{
			name: "UnlockToLock",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					var rw sync.RWMutex
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						rw.Lock()
						dstAccessYield(marker, true)
						runtime.Gosched()
						rw.Unlock()
					}()
					go func() {
						defer wg.Done()
						rw.Lock()
						dstAccessYield(marker, false)
						rw.Unlock()
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "UnlockToTryLock",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					var rw sync.RWMutex
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						rw.Lock()
						dstAccessYield(marker, true)
						runtime.Gosched()
						rw.Unlock()
					}()
					go func() {
						defer wg.Done()
						for {
							if rw.TryLock() {
								dstAccessYield(marker, false)
								rw.Unlock()
								return
							}
							runtime.Gosched()
						}
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "UnlockToRLock",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					var rw sync.RWMutex
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						rw.Lock()
						dstAccessYield(marker, true)
						runtime.Gosched()
						rw.Unlock()
					}()
					go func() {
						defer wg.Done()
						rw.RLock()
						dstAccessYield(marker, false)
						rw.RUnlock()
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "RUnlockToLock",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					var rw sync.RWMutex
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						rw.RLock()
						dstAccessYield(marker, true)
						runtime.Gosched()
						rw.RUnlock()
					}()
					go func() {
						defer wg.Done()
						rw.Lock()
						dstAccessYield(marker, false)
						rw.Unlock()
					}()
					wg.Wait()
					return false
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			marker := new(int)
			addr := uintptr(unsafe.Pointer(marker))
			sut := tt.sut(unsafe.Pointer(marker))
			dstExploreInit(256, 4096, 512, 512)
			stack := [][]uint64{nil}
			seen := map[string]bool{}
			for len(stack) > 0 && len(seen) < 200 {
				prefix := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				key := encodePrefix(prefix)
				if seen[key] {
					continue
				}
				seen[key] = true
				_, _, tr := runOnce(1, prefix, map[accessForce]bool{}, sut)
				write, read := -1, -1
				for i := range tr.accSeq {
					if tr.accAddr[i] != addr {
						continue
					}
					if tr.accWrite[i] {
						write = i
					} else {
						read = i
					}
				}
				if write >= 0 && read >= 0 && write < read && tr.accSeq[write] != tr.accSeq[read] {
					clk, pidx := dporClocks(tr)
					if dporConcurrent(clk, pidx, tr, write, read) {
						t.Fatalf("RWMutex public HB did not order %s accesses: write=%d read=%d seq=%v syncKind=%v syncSeq=%v syncID=%v", tt.name, write, read, tr.accSeq, tr.syncKind, tr.syncSeq, tr.syncID)
					}
					return
				}
				for i := len(prefix); i < len(tr.procs); i++ {
					for _, g := range tr.enabled[i] {
						if g == tr.procs[i] {
							continue
						}
						child := make([]uint64, i+1)
						copy(child, tr.procs[:i])
						child[i] = g
						stack = append(stack, child)
					}
				}
			}
			t.Fatalf("failed to reach %s RWMutex HB trace after %d schedules", tt.name, len(seen))
		})
	}
}

func TestExplorePostGoBoundaryNonRace(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if dstRaceEnabledFP() {
		t.Skip("non-race post-go boundary regression")
	}
	sut := func() bool {
		x := 0
		read := -1
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			read = x
		}()
		x = 1
		wg.Wait()
		return read == 0
	}
	res := Explore(1, Exhaustive, sut)
	if !res.Exhausted || res.Overflow || res.BudgetHit {
		t.Fatalf("post-go SUT did not cleanly exhaust: exhausted=%v overflow=%v budget=%v", res.Exhausted, res.Overflow, res.BudgetHit)
	}
	if len(res.Failures) == 0 {
		t.Fatalf("Explore missed child-before-parent-write after go statement: schedules=%d", res.Schedules)
	}
	failed, raced := Replay(1, res.Failures[0], sut)
	if !failed || raced {
		t.Fatalf("post-go failure did not replay as assertion failure: failed=%v raced=%v failure=%#v", failed, raced, res.Failures[0])
	}
}

func TestExplorePostGoBoundaryCanBeDisabled(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if dstRaceEnabledFP() {
		t.Skip("non-race post-go boundary regression")
	}
	old := dstSetPostGoYield(false)
	defer dstSetPostGoYield(old)
	res := Explore(1, Exhaustive, func() bool {
		x := 0
		read := -1
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			read = x
		}()
		x = 1
		wg.Wait()
		return read == 0
	})
	if len(res.Failures) != 0 {
		t.Fatalf("disabled post-go boundary still explored child-before-parent write: %#v", res.Failures)
	}
}

type emptyPanicError struct{}

func (emptyPanicError) Error() string { return "" }

type exploreCallbackPanicObj struct{ b byte }

type exploreCallbackSignal struct {
	ch  chan struct{}
	msg string
}

//go:noinline
func makeExploreFinalizerPanic(msg string) {
	o := &exploreCallbackPanicObj{}
	runtime.SetFinalizer(o, func(*exploreCallbackPanicObj) { panic(msg) })
	runtime.KeepAlive(o)
}

//go:noinline
func makeExploreCleanupPanic(msg string) {
	o := &exploreCallbackPanicObj{}
	runtime.AddCleanup(o, func(msg string) { panic(msg) }, msg)
	runtime.KeepAlive(o)
}

//go:noinline
func makeExploreFinalizerChanTouch(ch chan struct{}) {
	o := &exploreCallbackPanicObj{}
	runtime.SetFinalizer(o, func(*exploreCallbackPanicObj) { ch <- struct{}{} })
	runtime.KeepAlive(o)
}

// TestExploreDrainPanicDiscardsResidualCallbacks verifies that after a
// drain-callback panic is recorded as a Failure, callbacks queued later in the
// run — including bubble-channel-touching ones — are deterministically
// discarded at teardown. Before the fix they leaked past dstDeactivate to the
// bubble-less async workers, which fataled the process ("send on synctest
// channel from outside bubble") after Explore had already returned.
func TestExploreDrainPanicDiscardsResidualCallbacks(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	want := "finalizer callback boom"
	sut := func() bool {
		makeExploreFinalizerPanic(want)
		runtime.GC()
		time.Sleep(time.Millisecond) // drain panics; recorded, drain dead
		ch := make(chan struct{}, 1)
		makeExploreFinalizerChanTouch(ch)
		runtime.GC()
		time.Sleep(time.Millisecond) // dead drain: the queued finalizer must be discarded
		return false
	}
	res := Explore(1, DPOR, sut)
	if len(res.Failures) != 1 || res.Failures[0].Panic != want {
		t.Fatalf("drain panic not reported as the failure: %#v", res.Failures)
	}
	// Surface any leaked bubble-stamped callback now (it would fatal the
	// process on the async workers) rather than after the test exits.
	for range 3 {
		runtime.GC()
	}
	time.Sleep(10 * time.Millisecond)
}

//go:noinline
func makeExploreFinalizerSignalPanic(sig exploreCallbackSignal) {
	o := &exploreCallbackPanicObj{}
	runtime.SetFinalizer(o, func(*exploreCallbackPanicObj) {
		sig.ch <- struct{}{}
		panic(sig.msg)
	})
	runtime.KeepAlive(o)
}

//go:noinline
func makeExploreCleanupSignalPanic(sig exploreCallbackSignal) {
	o := &exploreCallbackPanicObj{}
	runtime.AddCleanup(o, func(sig exploreCallbackSignal) {
		sig.ch <- struct{}{}
		panic(sig.msg)
	}, sig)
	runtime.KeepAlive(o)
}

func TestExploreReportsPanicFailure(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool { panic("boom") }
	res := Explore(1, DPOR, sut)
	if len(res.Failures) != 1 || res.Failures[0].Panic != "boom" || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
		t.Fatalf("panic was not reported as a replayable failure: %#v", res.Failures)
	}
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		Replay(1, res.Failures[0], sut)
	}()
	if !panicked {
		t.Fatalf("Replay of panic failure did not panic")
	}
	res = Explore(1, DPOR, func() bool { panic(emptyPanicError{}) })
	if len(res.Failures) != 1 || res.Failures[0].Panic == "" || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
		t.Fatalf("empty-message error panic was not reported as a replayable failure: %#v", res.Failures)
	}
}

func TestExploreReportsChildPanicFailure(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			panic("child boom")
		}()
		wg.Wait()
		return false
	}
	res := Explore(1, DPOR, sut)
	if len(res.Failures) != 1 || res.Failures[0].Panic != "child boom" || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
		t.Fatalf("child panic was not reported as a replayable failure: %#v", res.Failures)
	}
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		Replay(1, res.Failures[0], sut)
	}()
	if !panicked {
		t.Fatalf("Replay of child panic failure did not panic")
	}
}

func TestExploreReportsDrainCallbackPanicFailure(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	for _, tt := range []struct {
		name string
		make func(string)
	}{
		{name: "finalizer", make: makeExploreFinalizerPanic},
		{name: "cleanup", make: makeExploreCleanupPanic},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.name + " callback boom"
			sut := func() bool {
				tt.make(want)
				time.Sleep(time.Millisecond)
				return false
			}
			res := Explore(1, DPOR, sut)
			if len(res.Failures) != 1 || res.Failures[0].Panic != want || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
				t.Fatalf("%s panic was not reported as a replayable failure: %#v", tt.name, res.Failures)
			}
			panicked := false
			func() {
				defer func() {
					if v := recover(); v != nil && panicString(v) == want {
						panicked = true
					}
				}()
				Replay(1, res.Failures[0], sut)
			}()
			if !panicked {
				t.Fatalf("Replay of %s callback panic failure did not panic with %q", tt.name, want)
			}
		})
	}
}

func TestExploreReportsDrainCallbackPanicBeforeLaterTopPanic(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	for _, tt := range []struct {
		name string
		make func(exploreCallbackSignal)
	}{
		{name: "finalizer", make: makeExploreFinalizerSignalPanic},
		{name: "cleanup", make: makeExploreCleanupSignalPanic},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.name + " callback boom"
			sut := func() bool {
				ch := make(chan struct{})
				tt.make(exploreCallbackSignal{ch: ch, msg: want})
				<-ch
				panic("top boom")
			}
			res := Explore(1, DPOR, sut)
			if len(res.Failures) != 1 || res.Failures[0].Panic != want || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
				t.Fatalf("%s callback panic was not preserved before later top panic: %#v", tt.name, res.Failures)
			}
		})
	}
}

func TestExploreReportsNestedChildPanicClearsPanicDefers(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	before := dstRunningPanicDefersFP()
	sut := func() bool {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { panic("inner") }()
			panic("outer")
		}()
		wg.Wait()
		return false
	}
	res := Explore(1, DPOR, sut)
	if len(res.Failures) != 1 || res.Failures[0].Panic != "inner" || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
		t.Fatalf("nested child panic was not reported as a replayable failure: %#v", res.Failures)
	}
	if got := dstRunningPanicDefersFP(); got != before {
		t.Fatalf("nested child panic leaked runningPanicDefers: before=%d after=%d", before, got)
	}
}

func TestExploreReportsDeadlockFailure(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		ch := make(chan struct{})
		<-ch
		return false
	}
	res := Explore(1, Exhaustive, sut)
	if len(res.Failures) != 1 || res.Failures[0].Deadlock == "" || res.Failures[0].Panic != "" || res.Failures[0].Race {
		t.Fatalf("deadlock was not reported as a replayable failure: exhausted=%v overflow=%v failures=%#v", res.Exhausted, res.Overflow, res.Failures)
	}
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		Replay(1, res.Failures[0], sut)
	}()
	if !panicked {
		t.Fatalf("Replay of deadlock failure did not panic")
	}
	clean := Explore(1, Exhaustive, func() bool { return false })
	if !clean.Exhausted || clean.Overflow || clean.BudgetHit || len(clean.Failures) != 0 {
		t.Fatalf("deadlocked bubble state affected later Explore run: %#v", clean)
	}
}

func budgetedExploreSUT() bool {
	var x int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		dstAccessYield(unsafe.Pointer(&x), true)
		x = 1
	}()
	go func() {
		defer wg.Done()
		dstAccessYield(unsafe.Pointer(&x), true)
		x = 2
	}()
	wg.Wait()
	return false
}

func TestExploreWithScheduleBudgetReportsIncomplete(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	res := ExploreWith(1, ExploreOptions{Mode: Exhaustive, MaxSchedules: 1}, budgetedExploreSUT)
	if res.Schedules != 1 || !res.BudgetHit || res.Exhausted || res.Overflow {
		t.Fatalf("schedule budget not reported distinctly: schedules=%d exhausted=%v overflow=%v budget=%v", res.Schedules, res.Exhausted, res.Overflow, res.BudgetHit)
	}
}

func TestExploreWithStepBudgetReportsIncomplete(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	res := ExploreWith(1, ExploreOptions{Mode: Exhaustive, MaxSteps: 1}, budgetedExploreSUT)
	if !res.BudgetHit || res.Exhausted || res.Overflow {
		t.Fatalf("step budget not reported distinctly: schedules=%d exhausted=%v overflow=%v budget=%v", res.Schedules, res.Exhausted, res.Overflow, res.BudgetHit)
	}
}
