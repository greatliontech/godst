// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package poll

import (
	rtatomic "internal/runtime/atomic"
	"unsafe"
)

const dstSimEnabled = true

// dstClearSysfd atomically stores -1 into *sysfd when FD.destroy closes a
// host descriptor so package os's pre-fdmu DST interception loads (dstSysfd)
// never race with concurrent File.Close on host handles, without injecting
// DPOR atomic decision points or spurious TSan release edges.
func dstClearSysfd(sysfd *int) {
	rtatomic.Storeuintptr((*uintptr)(unsafe.Pointer(sysfd)), ^uintptr(0))
}
