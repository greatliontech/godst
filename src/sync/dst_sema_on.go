// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package sync

import _ "unsafe"

const dstSemaEnabled = true

//go:linkname runtime_dstSemrelease
func runtime_dstSemrelease(s *uint32, handoff bool, skipframes int) (woke bool, crashed int32, shouldYield bool)

//go:noinline
func dstRWMutexWakeReaders(rw *RWMutex, r int32) {
	rem := int(r)
	var totalCrashed int32
	for rem > 0 {
		woke, crashed, _ := runtime_dstSemrelease(&rw.readerSem, false, 1)
		totalCrashed += crashed
		if woke {
			rem -= 1 + int(crashed)
		} else if crashed > 0 {
			rem -= int(crashed)
		} else {
			rem--
		}
	}
	if totalCrashed > 0 {
		rw.readerCount.Add(-totalCrashed)
	}
}

//go:noinline
func dstWaitGroupWake(sema *uint32, w uint32) {
	rem := int(w)
	for rem > 0 {
		woke, crashed, _ := runtime_dstSemrelease(sema, false, 1)
		if woke {
			rem -= 1 + int(crashed)
		} else if crashed > 0 {
			rem -= int(crashed)
		} else {
			rem--
		}
	}
}
