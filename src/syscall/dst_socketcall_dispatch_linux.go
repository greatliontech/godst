// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (386 || s390x)

package syscall

// dstSocketcallSockopt dispatches the multiplexed setsockopt/getsockopt forms
// of socketcall(2) on a virtual SOCKET descriptor — the socketcall-era twin of
// the direct-number arm in dstRawDispatch, reached from the fenced socketcall
// wrapper (golang.org/x/sys/unix's 386/s390x assembly enters it by name).
// Args arrive pre-demuxed by the Go wrapper: (fd, level, optname, optval,
// optlen) for setsockopt, (fd, level, optname, optval, *optlen) for
// getsockopt. handled=false — another socketcall form, or a host fd — falls
// through to the fence. Nosplit for the same reason as dstRawDispatch: the
// "not mine" decision must precede any call that can grow the stack, and the
// uintptr→pointer conversions (inside dstRawSockopt, itself nosplit) must run
// before a stack move can strand the wrapper's uintptrkeepalive arguments.
// rawsocketcall takes no dispatch, mirroring the RawSyscall trampolines: it
// may run with no P, where the splittable option layer must not be entered.
//
//go:nosplit
func dstSocketcallSockopt(call int, a0, a1, a2, a3, a4 uintptr) (err Errno, handled bool) {
	if call != _SETSOCKOPT && call != _GETSOCKOPT {
		return 0, false
	}
	if a0 < dstVirtualSockFDBase || a0 >= dstVirtualSockFDBase+dstVirtualSockFDCount {
		return 0, false
	}
	_, e, _ := dstRawSockopt(call == _SETSOCKOPT, a0, a1, a2, a3, a4)
	return e, true
}
