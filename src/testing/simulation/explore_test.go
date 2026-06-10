// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"sync"
	"testing"
	"unsafe"
)

//go:linkname dstAccessYield runtime.dstAccessYield
func dstAccessYield(addr unsafe.Pointer, write bool)

//go:linkname dstAccessYieldFP runtime.dstAccessYieldFP
func dstAccessYieldFP() uint64

//go:linkname dstAccessYieldReset runtime.dstAccessYieldReset
func dstAccessYieldReset()

//go:linkname dstSetPostGoYield runtime.dstSetPostGoYield
func dstSetPostGoYield(enabled bool) bool

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

func TestExploreReportsPanicFailure(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool { panic("boom") }
	res := Explore(1, DPOR, sut)
	if len(res.Failures) != 1 || res.Failures[0].Panic != "boom" || res.Failures[0].Race {
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
	if len(res.Failures) != 1 || res.Failures[0].Panic == "" || res.Failures[0].Race {
		t.Fatalf("empty-message error panic was not reported as a replayable failure: %#v", res.Failures)
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
