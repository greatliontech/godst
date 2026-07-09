// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && !(386 || arm || mips || mipsle)

package syscall

// 64-bit-time-only arches have no separate time64 trap; 0 never matches a
// fenced trap in dstTryClockGettime's guard.
const dstSysClockGettime64 uintptr = 0
