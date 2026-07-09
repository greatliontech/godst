// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package syscall

import (
	"unsafe"
	_ "unsafe" // for go:linkname
)

const (
	dstClockMonotonic = 1
	dstClockBoottime  = 7
)

//go:linkname dstVirtualMonotonicNow runtime.dstVirtualMonotonicNow
func dstVirtualMonotonicNow() (int64, bool)

//go:nosplit
//go:norace
func dstTryClockGettime(trap, clockid, ts uintptr) (r1, r2 uintptr, err Errno, handled bool) {
	time64 := dstSysClockGettime64 != 0 && trap == dstSysClockGettime64
	if (trap != SYS_CLOCK_GETTIME && !time64) || (clockid != dstClockMonotonic && clockid != dstClockBoottime) {
		return 0, 0, 0, false
	}
	if ts == 0 {
		return ^uintptr(0), 0, EFAULT, true
	}
	now, ok := dstVirtualMonotonicNow()
	if !ok {
		return 0, 0, 0, false
	}
	sec := now / 1_000_000_000
	nsec := now % 1_000_000_000
	if time64 {
		// __kernel_timespec: int64 seconds and int64 nanoseconds regardless of
		// the arch's word size (the trap exists exactly to carry 64-bit time
		// on 32-bit kernels), so no EOVERFLOW leg.
		*(*int64)(unsafe.Pointer(ts)) = sec
		*(*int64)(unsafe.Add(unsafe.Pointer(ts), 8)) = nsec
		return 0, 0, 0, true
	}
	if unsafe.Sizeof(_C_long(0)) == 4 && (sec < -1<<31 || sec > 1<<31-1) {
		return ^uintptr(0), 0, EOVERFLOW, true
	}
	secp := (*_C_long)(unsafe.Pointer(ts))
	*secp = _C_long(sec)
	*(*_C_long)(unsafe.Add(unsafe.Pointer(ts), unsafe.Sizeof(*secp))) = _C_long(nsec)
	return 0, 0, 0, true
}
