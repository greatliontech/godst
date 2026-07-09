// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && unix

package syscall

import _ "unsafe" // for go:linkname

// dstSimFenced is the compile-time gate for the interception-boundary fences
// (raw syscalls, process spawn; see design.md "The interception boundary").
// True only in -tags dst builds. dstFenceActive is a cross-package linkname the
// compiler cannot fold, so this const — not the predicate — is what dead-code-
// eliminates every fence branch in a stock build, keeping the hot raw-syscall
// path free of an opaque call when DST is not built in.
const dstSimFenced = true

// dstFenceActive reports whether the caller is a bubble goroutine of the active
// simulation (see runtime.dstFenceActive). It is nosplit, alloc-free, and
// lock-free, so the nosplit/uintptrkeepalive raw-syscall trampolines may call it
// without growing their stack, and it is safe from the norace post-fork child
// (where it reads false, letting the child's execve/dup/close proceed).
//
//go:linkname dstFenceActive runtime.dstFenceActive
func dstFenceActive() bool

// dstErrUnsupported is the refusal for fenced entry points whose signature
// carries an error channel (process spawn, exec) — the standard "unsupported
// under deterministic simulation" shape, mirroring os's simulated-filesystem
// refusal. Raw-syscall trampolines have no honest errno for this and panic
// instead (dstSyscallRefuse).
var dstErrUnsupported error = dstUnsupportedError{}

type dstUnsupportedError struct{}

func (dstUnsupportedError) Error() string {
	return "operation unsupported under deterministic simulation"
}

type dstReadHook func(fd int, p []byte) (n int, err Errno, handled bool)
type dstWriteHook func(fd int, p []byte) (n int, err Errno, handled bool)
type dstPreadHook func(fd int, p []byte, offset int64) (n int, err Errno, handled bool)
type dstPwriteHook func(fd int, p []byte, offset int64) (n int, err Errno, handled bool)
type dstCloseHook func(fd int) (err Errno, handled bool)
type dstFstatHook func(fd int, stat *Stat_t) (err Errno, handled bool)
type dstSeekHook func(fd int, offset int64, whence int) (off int64, err Errno, handled bool)
type dstFsyncHook func(fd int) (err Errno, handled bool)
type dstFdatasyncHook func(fd int) (err Errno, handled bool)
type dstFlockHook func(fd int, how int) (err Errno, handled bool)
type dstMmapHook func(fd int, offset int64, length int, prot int, flags int) (data []byte, err Errno, handled bool)
type dstMunmapHook func(data []byte) (err Errno, handled bool)
type dstMprotectHook func(data []byte, prot int) (err Errno, handled bool)
type dstMadviseHook func(data []byte, advice int) (err Errno, handled bool)
type dstKillHook func(pid int, sig Signal) (err Errno, handled bool)

var dstReadHookFn dstReadHook
var dstWriteHookFn dstWriteHook
var dstPreadHookFn dstPreadHook
var dstPwriteHookFn dstPwriteHook
var dstCloseHookFn dstCloseHook
var dstFstatHookFn dstFstatHook
var dstSeekHookFn dstSeekHook
var dstFsyncHookFn dstFsyncHook
var dstFdatasyncHookFn dstFdatasyncHook
var dstFlockHookFn dstFlockHook
var dstMmapHookFn dstMmapHook
var dstMunmapHookFn dstMunmapHook
var dstMprotectHookFn dstMprotectHook
var dstMadviseHookFn dstMadviseHook
var dstKillHookFn dstKillHook

//go:linkname dstSetReadHook
func dstSetReadHook(fn dstReadHook) { dstReadHookFn = fn }

//go:linkname dstSetWriteHook
func dstSetWriteHook(fn dstWriteHook) { dstWriteHookFn = fn }

//go:linkname dstSetPreadHook
func dstSetPreadHook(fn dstPreadHook) { dstPreadHookFn = fn }

//go:linkname dstSetPwriteHook
func dstSetPwriteHook(fn dstPwriteHook) { dstPwriteHookFn = fn }

//go:linkname dstSetCloseHook
func dstSetCloseHook(fn dstCloseHook) { dstCloseHookFn = fn }

//go:linkname dstSetFstatHook
func dstSetFstatHook(fn dstFstatHook) { dstFstatHookFn = fn }

//go:linkname dstSetSeekHook
func dstSetSeekHook(fn dstSeekHook) { dstSeekHookFn = fn }

//go:linkname dstSetFsyncHook
func dstSetFsyncHook(fn dstFsyncHook) { dstFsyncHookFn = fn }

//go:linkname dstSetFdatasyncHook
func dstSetFdatasyncHook(fn dstFdatasyncHook) { dstFdatasyncHookFn = fn }

//go:linkname dstSetFlockHook
func dstSetFlockHook(fn dstFlockHook) { dstFlockHookFn = fn }

//go:linkname dstSetMmapHook
func dstSetMmapHook(fn dstMmapHook) { dstMmapHookFn = fn }

//go:linkname dstSetMunmapHook
func dstSetMunmapHook(fn dstMunmapHook) { dstMunmapHookFn = fn }

//go:linkname dstSetMprotectHook
func dstSetMprotectHook(fn dstMprotectHook) { dstMprotectHookFn = fn }

//go:linkname dstSetMadviseHook
func dstSetMadviseHook(fn dstMadviseHook) { dstMadviseHookFn = fn }

//go:linkname dstSetKillHook
func dstSetKillHook(fn dstKillHook) { dstKillHookFn = fn }

func dstHookActive() bool {
	if !dstFenceActive() {
		return false
	}
	return true
}

func dstTryRead(fd int, p []byte) (n int, err Errno, handled bool) {
	if !dstHookActive() || dstReadHookFn == nil {
		return 0, 0, false
	}
	return dstReadHookFn(fd, p)
}

func dstTryWrite(fd int, p []byte) (n int, err Errno, handled bool) {
	if !dstHookActive() || dstWriteHookFn == nil {
		return 0, 0, false
	}
	return dstWriteHookFn(fd, p)
}

func dstTryPread(fd int, p []byte, offset int64) (n int, err Errno, handled bool) {
	if !dstHookActive() || dstPreadHookFn == nil {
		return 0, 0, false
	}
	return dstPreadHookFn(fd, p, offset)
}

func dstTryPwrite(fd int, p []byte, offset int64) (n int, err Errno, handled bool) {
	if !dstHookActive() || dstPwriteHookFn == nil {
		return 0, 0, false
	}
	return dstPwriteHookFn(fd, p, offset)
}

func dstTryClose(fd int) (err Errno, handled bool) {
	if !dstHookActive() || dstCloseHookFn == nil {
		return 0, false
	}
	return dstCloseHookFn(fd)
}

func dstTryFstat(fd int, stat *Stat_t) (err Errno, handled bool) {
	if !dstHookActive() || dstFstatHookFn == nil {
		return 0, false
	}
	return dstFstatHookFn(fd, stat)
}

func dstTrySeek(fd int, offset int64, whence int) (off int64, err Errno, handled bool) {
	if !dstHookActive() || dstSeekHookFn == nil {
		return 0, 0, false
	}
	return dstSeekHookFn(fd, offset, whence)
}

func dstTryFsync(fd int) (err Errno, handled bool) {
	if !dstHookActive() || dstFsyncHookFn == nil {
		return 0, false
	}
	return dstFsyncHookFn(fd)
}

func dstTryFdatasync(fd int) (err Errno, handled bool) {
	if !dstHookActive() || dstFdatasyncHookFn == nil {
		return 0, false
	}
	return dstFdatasyncHookFn(fd)
}

func dstTryFlock(fd int, how int) (err Errno, handled bool) {
	if !dstHookActive() || dstFlockHookFn == nil {
		return 0, false
	}
	return dstFlockHookFn(fd, how)
}

func dstTryMmap(fd int, offset int64, length int, prot int, flags int) (data []byte, err Errno, handled bool) {
	if !dstHookActive() || dstMmapHookFn == nil {
		return nil, 0, false
	}
	return dstMmapHookFn(fd, offset, length, prot, flags)
}

func dstTryMunmap(data []byte) (err Errno, handled bool) {
	if !dstHookActive() || dstMunmapHookFn == nil {
		return 0, false
	}
	return dstMunmapHookFn(data)
}

func dstTryMprotect(data []byte, prot int) (err Errno, handled bool) {
	if !dstHookActive() || dstMprotectHookFn == nil {
		return 0, false
	}
	return dstMprotectHookFn(data, prot)
}

func dstTryMadvise(data []byte, advice int) (err Errno, handled bool) {
	if !dstHookActive() || dstMadviseHookFn == nil {
		return 0, false
	}
	return dstMadviseHookFn(data, advice)
}

func dstTryKill(pid int, sig Signal) (err Errno, handled bool) {
	if dstKillHookFn == nil {
		return 0, false
	}
	if _, ok := dstSimEnvProc(); !ok {
		return 0, false
	}
	if sig != 0 {
		// The signal-delivery fence is a fence, not an identity read: fences fire
		// only for bubble goroutines of the active run (design.md "The interception
		// boundary"), so a non-bubble harness goroutine keeps host kill(2) access
		// mid-run. The sig==0 liveness probe below is an identity READ and stays
		// process-global like the other identity reads (design.md, identity gating).
		if !dstFenceActive() {
			return 0, false
		}
	}
	return dstKillHookFn(pid, sig)
}
