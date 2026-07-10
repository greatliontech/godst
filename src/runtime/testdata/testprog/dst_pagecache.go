// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package main

import (
	"fmt"
	_ "unsafe"
)

//go:linkname dstPageCacheNew runtime.dstPageCacheNew
func dstPageCacheNew() int32

//go:linkname dstPageCacheResize runtime.dstPageCacheResize
func dstPageCacheResize(fd int32, size int64)

//go:linkname dstPageCacheMap runtime.dstPageCacheMap
func dstPageCacheMap(fd int32, n uintptr, prot int32) uintptr

func init() {
	register("DSTMappingAddr", DSTMappingAddr)
}

// DSTMappingAddr performs a fixed mapping sequence and prints the addresses.
// The test runs this binary twice: replay-exactness requires the output be
// identical across process invocations, which kernel-chosen (ASLR'd) mapping
// addresses would fail.
func DSTMappingAddr() {
	const protRW = 0x1 | 0x2
	fd := dstPageCacheNew()
	dstPageCacheResize(fd, 4096)
	a := dstPageCacheMap(fd, 64<<10, protRW)
	fd2 := dstPageCacheNew()
	dstPageCacheResize(fd2, 8192)
	b := dstPageCacheMap(fd2, 1<<20, protRW)
	fmt.Printf("%#x %#x\n", a, b)
}
