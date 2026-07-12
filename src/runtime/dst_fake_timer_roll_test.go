// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime_test

import (
	"runtime"
	"sync/atomic"
	"testing"
)

func TestDSTFakeTimerRollPreservesNewEpochRegistration(t *testing.T) {
	// One P is deliberately held by the paused rollover and one by the
	// contending runtime lock; keep a third available to drive the test.
	oldProcs := runtime.GOMAXPROCS(3)
	defer runtime.GOMAXPROCS(oldProcs)
	oldEpoch := runtime.DSTFakeTimersTestReset(2)
	defer runtime.DSTFakeTimersTestRestore(oldEpoch)

	first := new(runtime.DSTFakeTimer)
	second := new(runtime.DSTFakeTimer)
	runtime.DSTFakeTimersTestInit(first, 1, 100, 10)
	runtime.DSTFakeTimersTestInit(second, 1, 100, 10)
	var entered, release, started, done, rollDone uint32
	go func() {
		runtime.DSTFakeTimersTestRollPaused(first, &entered, &release)
		atomic.StoreUint32(&rollDone, 1)
	}()
	for atomic.LoadUint32(&entered) == 0 {
		runtime.Gosched()
	}
	go func() {
		atomic.StoreUint32(&started, 1)
		runtime.DSTFakeTimersTestRegister(second)
		atomic.StoreUint32(&done, 1)
	}()
	for atomic.LoadUint32(&started) == 0 {
		runtime.Gosched()
	}
	for range 100 {
		runtime.Gosched()
	}
	if atomic.LoadUint32(&done) != 0 {
		atomic.StoreUint32(&release, 1)
		t.Fatal("registration crossed an in-progress epoch rollover")
	}
	atomic.StoreUint32(&release, 1)
	for atomic.LoadUint32(&done) == 0 || atomic.LoadUint32(&rollDone) == 0 {
		runtime.Gosched()
	}
	if got := runtime.DSTFakeTimersTestCount(); got != 2 {
		t.Fatalf("new epoch timer count = %d, want 2", got)
	}
	when := runtime.DSTFakeTimersTestRemap(1, 0, 0, 1_000_000_000, first, second)
	if when[0] != 50 || when[1] != 50 {
		t.Fatalf("remapped timer deadlines = %v, want [50 50]", when)
	}
}

func TestDSTFakeTimerRegistrationEpochDoesNotWrap(t *testing.T) {
	oldEpoch := runtime.DSTFakeTimersTestReset(1 << 32)
	defer runtime.DSTFakeTimersTestRestore(oldEpoch)
	runtime.DSTFakeTimersTestRegister(new(runtime.DSTFakeTimer))
	if got := runtime.DSTFakeTimersTestCount(); got != 1 {
		t.Fatalf("timer count at epoch 2^32 = %d, want 1", got)
	}
}
