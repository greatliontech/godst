// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (mips || mipsle)

package syscall

// See dst_clock_time64_linux.go; MIPS o32 syscall numbers carry the 4000 base.
const dstSysClockGettime64 uintptr = 4403
