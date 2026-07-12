// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package simulation

import (
	"runtime"
	"sync/atomic"
	"testing"
	_ "unsafe"
)

//go:linkname dstDisassociatedWaitFP runtime.dstDisassociatedWaitFP
func dstDisassociatedWaitFP(entered, release *uint32)

func TestDSTCrashMarksDisassociatedProcessMember(t *testing.T) {
	testDSTCrashMarksDisassociatedMember(t, false)
}

func TestDSTCrashHostMarksDisassociatedMember(t *testing.T) {
	testDSTCrashMarksDisassociatedMember(t, true)
}

func testDSTCrashMarksDisassociatedMember(t *testing.T, hostCrash bool) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	Test(t, 1, func(t *testing.T) {
		var entered, release, after, deferred uint32
		body := func() {
			defer atomic.StoreUint32(&deferred, 1)
			dstDisassociatedWaitFP(&entered, &release)
			atomic.StoreUint32(&after, 1)
		}
		if hostCrash {
			go Host("machine", HostConfig{}, body)
		} else {
			go Process("worker", body)
		}
		for atomic.LoadUint32(&entered) == 0 {
			runtime.Gosched()
		}
		if hostCrash {
			CrashHost("machine")
		} else {
			Crash("worker")
		}
		atomic.StoreUint32(&release, 1)
		for range 100 {
			runtime.Gosched()
		}
		if atomic.LoadUint32(&after) != 0 || atomic.LoadUint32(&deferred) != 0 {
			t.Fatalf("disassociated crash victim resumed: after=%d deferred=%d", after, deferred)
		}
	})
}
