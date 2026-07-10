// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"testing/simulation"
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
	register("DSTCrashedMappingTouch", DSTCrashedMappingTouch)
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

// DSTCrashedMappingTouch maps a file inside a simulated process, crashes the
// process, and touches the dead mapping from the host body. The memory does
// not exist (the machine's page went with its owner), so the harness must
// abort with the NAMED reason — never "unexpected fault address", which reads
// as a harness bug, and never a laundered process death.
func DSTCrashedMappingTouch() {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			mapped := make(chan []byte, 1)
			go simulation.Process("victim", func() {
				f, err := os.OpenFile("/m", os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					panic(err)
				}
				f.Write(make([]byte, 4096))
				b, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
				f.Close()
				if err != nil {
					panic(err)
				}
				mapped <- b
				select {}
			})
			b := <-mapped
			simulation.Crash("victim")
			fmt.Println("touching:", b[0]) // aborts with the named reason
		})
	})
	fmt.Println("UNREACHED")
}
