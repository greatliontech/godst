// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import _ "unsafe" // for go:linkname

const dstSimEnabled = true

// Under deterministic simulation testing (testing/simulation), the process
// identity functions return a simulated identity so a program under test
// observes reproducible values instead of the real machine's, which vary per run
// and per host and would otherwise leak nondeterminism. The runtime holds the
// per-run simulated identity (set by testing/simulation while a run is active);
// these accessors report it, and the bool is false outside a run, so the callers
// fall through to the real syscall then.

//go:linkname dstSimGetpid runtime.dstSimGetpid
func dstSimGetpid() (int, bool)

//go:linkname dstSimGethostname runtime.dstSimGethostname
func dstSimGethostname() (string, bool)

//go:linkname dstSimGetppid runtime.dstSimGetppid
func dstSimGetppid() (int, bool)

//go:linkname dstSimGetuid runtime.dstSimGetuid
func dstSimGetuid() (int, bool)

//go:linkname dstSimGetgid runtime.dstSimGetgid
func dstSimGetgid() (int, bool)

//go:linkname dstSimGeteuid runtime.dstSimGeteuid
func dstSimGeteuid() (int, bool)

//go:linkname dstSimGetegid runtime.dstSimGetegid
func dstSimGetegid() (int, bool)

// dstFenceActive reports whether the caller is a bubble goroutine of the active
// simulation — the interception-boundary predicate (see runtime.dstFenceActive).
// os uses it to keep the process-global pidfd probe (checkPidfdOnce) off bubble
// goroutines, whose fenced raw syscalls would otherwise panic inside a
// process-global sync.Once and poison it host-wide. See pidfd_linux.go.
//
//go:linkname dstFenceActive runtime.dstFenceActive
func dstFenceActive() bool
