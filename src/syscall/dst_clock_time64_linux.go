// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (386 || arm)

package syscall

// dstSysClockGettime64 is the 64-bit-time clock_gettime trap 32-bit kernels
// added for the 2038 horizon (__kernel_timespec: two int64s). It exists only
// on 32-bit arches; virtualizing it keeps a SUT's clock reads deterministic
// when its syscall wrapper migrates from the 32-bit-time trap.
const dstSysClockGettime64 uintptr = 403
