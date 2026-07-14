// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"strconv"
	"syscall"
	"testing"
)

// TestDSTGettimeofdayFence: syscall.Gettimeofday and syscall.Time refuse
// in-bubble. On amd64 they enter the kernel through an assembly vDSO path
// none of the fenced trampolines see (every other dst arch routes them
// through the fenced RawSyscall wrapper), so the named wrappers carry the
// fence; a silent success would flow HOST wall time into the seeded
// schedule — the unreproducible-failure class. Outside a run they keep
// host semantics. amd64-specific: syscall.Time exists only on the arches
// with the asm entry.
func TestDSTGettimeofdayFence(t *testing.T) {
	wantPanic := "syscall: raw syscall " + strconv.FormatUint(uint64(syscall.SYS_GETTIMEOFDAY), 10) + " unsupported under deterministic simulation"
	var gtodPanic, timePanic any
	Run(1, func() {
		gtodPanic = dstPanicValue(func() {
			var tv syscall.Timeval
			syscall.Gettimeofday(&tv)
		})
		timePanic = dstPanicValue(func() {
			var tt syscall.Time_t
			syscall.Time(&tt)
		})
	})
	if gtodPanic != wantPanic {
		t.Errorf("in-bubble Gettimeofday panic = %v, want %q", gtodPanic, wantPanic)
	}
	if timePanic != wantPanic {
		t.Errorf("in-bubble Time panic = %v, want %q", timePanic, wantPanic)
	}
	var tv syscall.Timeval
	if err := syscall.Gettimeofday(&tv); err != nil || tv.Sec == 0 {
		t.Errorf("outside-bubble Gettimeofday = (sec %d, %v), want host wall time, nil", tv.Sec, err)
	}
}
