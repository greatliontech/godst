// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (386 || s390x)

package syscall

import _ "unsafe"

func socketcall1(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err Errno)
func rawsocketcall1(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err Errno)

//go:uintptrkeepalive
//go:nosplit
//go:linkname socketcall
func socketcall(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err Errno) {
	if dstSimFenced && dstFenceActive() {
		if e, handled := dstSocketcallSockopt(call, a0, a1, a2, a3, a4); handled {
			if e != 0 {
				return -1, e
			}
			return 0, 0
		}
		dstSyscallRefuse(SYS_SOCKETCALL)
	}
	return socketcall1(call, a0, a1, a2, a3, a4, a5)
}

//go:uintptrkeepalive
//go:nosplit
//go:linkname rawsocketcall
func rawsocketcall(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err Errno) {
	if dstSimFenced && dstFenceActive() {
		dstSyscallRefuse(SYS_SOCKETCALL)
	}
	return rawsocketcall1(call, a0, a1, a2, a3, a4, a5)
}
