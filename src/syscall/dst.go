// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

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
