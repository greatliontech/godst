// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package net

import (
	"syscall"
	"unsafe"
	_ "unsafe" // for go:linkname
)

// The numeric (level, option) surface of the simulated option layer: the
// syscall package's raw-boundary dispatch routes setsockopt/getsockopt on a
// virtual socket descriptor to these hooks (registered at init). Values
// travel as raw native-endian bytes, the kernel's view; errno shapes follow
// do_tcp_setsockopt/sock_setsockopt — EBADF for a descriptor the run never
// issued (or already closed), EINVAL for a short optlen or an out-of-range
// parameter, ENOPROTOOPT for an option outside the modeled set (the fence
// philosophy at option granularity: nothing is silently accepted whose
// behavior the model does not provide).

// TCP_USER_TIMEOUT's number (linux uapi tcp.h; absent from the frozen
// zerrors tables).
const dstTCP_USER_TIMEOUT = 18

//go:linkname dstSetSetsockoptHook syscall.dstSetSetsockoptHook
func dstSetSetsockoptHook(fn func(fd, level, opt int, val []byte) (syscall.Errno, bool))

//go:linkname dstSetGetsockoptHook syscall.dstSetGetsockoptHook
func dstSetGetsockoptHook(fn func(fd, level, opt int, val []byte) (int, syscall.Errno, bool))

func init() {
	dstSetSetsockoptHook(dstSockSetsockopt)
	dstSetGetsockoptHook(dstSockGetsockopt)
}

// dstSockoptInt reads the int the kernel would copy_from_user: the first four
// native-endian bytes. A shorter buffer is the caller's EINVAL.
func dstSockoptInt(val []byte) (int32, bool) {
	if len(val) < 4 {
		return 0, false
	}
	var v int32
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&v)), 4), val[:4])
	return v, true
}

func dstSockoptPutInt(val []byte, v int32) {
	copy(val[:4], unsafe.Slice((*byte)(unsafe.Pointer(&v)), 4))
}

func dstSockSetsockopt(fd, level, opt int, val []byte) (syscall.Errno, bool) {
	o := dstSockFDLookup(fd)
	if o == nil {
		return syscall.EBADF, true
	}
	v, ok := dstSockoptInt(val)
	if !ok {
		return syscall.EINVAL, true
	}
	switch {
	case level == syscall.SOL_SOCKET && opt == syscall.SO_KEEPALIVE:
		o.mu.Lock()
		o.keepAlive = v != 0
		o.mu.Unlock()
		o.kicked()
	case level == syscall.IPPROTO_TCP && opt == syscall.TCP_KEEPIDLE:
		if v < 1 || v > dstKeepIdleMaxSec {
			return syscall.EINVAL, true
		}
		o.mu.Lock()
		o.keepIdleSec = v
		o.mu.Unlock()
		o.kicked()
	case level == syscall.IPPROTO_TCP && opt == syscall.TCP_KEEPINTVL:
		if v < 1 || v > dstKeepIntvlMaxSec {
			return syscall.EINVAL, true
		}
		o.mu.Lock()
		o.keepIntvlSec = v
		o.mu.Unlock()
		o.kicked()
	case level == syscall.IPPROTO_TCP && opt == syscall.TCP_KEEPCNT:
		if v < 1 || v > dstKeepCntMax {
			return syscall.EINVAL, true
		}
		o.mu.Lock()
		o.keepCnt = v
		o.mu.Unlock()
		o.kicked()
	case level == syscall.IPPROTO_TCP && opt == dstTCP_USER_TIMEOUT:
		// Unsigned milliseconds (tcp(7)); the kernel takes any u32.
		o.mu.Lock()
		o.userTimeoutMs = uint32(v)
		o.mu.Unlock()
		o.kicked()
	default:
		return syscall.ENOPROTOOPT, true
	}
	return 0, true
}

func dstSockGetsockopt(fd, level, opt int, val []byte) (int, syscall.Errno, bool) {
	o := dstSockFDLookup(fd)
	if o == nil {
		return 0, syscall.EBADF, true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	var v int32
	switch {
	case level == syscall.SOL_SOCKET && opt == syscall.SO_KEEPALIVE:
		if o.keepAlive {
			v = 1
		}
	case level == syscall.IPPROTO_TCP && opt == syscall.TCP_KEEPIDLE:
		v = o.keepIdleSec
	case level == syscall.IPPROTO_TCP && opt == syscall.TCP_KEEPINTVL:
		v = o.keepIntvlSec
	case level == syscall.IPPROTO_TCP && opt == syscall.TCP_KEEPCNT:
		v = o.keepCnt
	case level == syscall.IPPROTO_TCP && opt == dstTCP_USER_TIMEOUT:
		v = int32(o.userTimeoutMs)
	default:
		return 0, syscall.ENOPROTOOPT, true
	}
	// The kernel clamps the copy to the option's size and permits a SHORT
	// read (sk_getsockopt: len = min(len, sizeof(int)), partial copy,
	// success with the clamped length) — a negative length was already
	// EINVAL at the dispatch.
	n := len(val)
	if n > 4 {
		n = 4
	}
	if n > 0 {
		var buf [4]byte
		dstSockoptPutInt(buf[:], v)
		copy(val[:n], buf[:n])
	}
	return n, 0, true
}
