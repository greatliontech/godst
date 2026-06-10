// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
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
