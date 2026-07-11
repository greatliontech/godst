// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package rand provides cryptographically secure random bytes from the
// operating system.
package sysrand

import (
	"internal/abi"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var firstUse atomic.Bool

func warnBlocked() {
	println("crypto/rand: blocked for 60 seconds waiting to read random data from the kernel")
}

// fatal is [runtime.fatal], pushed via linkname.
//
//go:linkname fatal
func fatal(string)

var testingOnlyFailRead bool

// Read fills b with cryptographically secure random bytes from the operating
// system. It always fills b entirely and crashes the program irrecoverably if
// an error is encountered. The operating system APIs are documented to never
// return an error on all but legacy Linux systems.
//
// Note that Read is not affected by [testing/cryptotest.SetGlobalRand], and it
// should not be used directly by algorithm implementations.
func Read(b []byte) {
	if dstReadRandomEnabled && dstReadRandom(b) {
		// Deterministic simulation testing: filled from the run's seeded RNG. See
		// crypto/internal/sysrand/dst.go. No-op (returns false) outside a run.
		return
	}
	if firstUse.CompareAndSwap(false, true) {
		// First use of randomness. Start timer to warn about
		// being blocked on entropy not being available.
		t := time.AfterFunc(time.Minute, warnBlocked)
		defer t.Stop()
	}
	if err := read(b); err != nil || testingOnlyFailRead {
		var errStr string
		if !testingOnlyFailRead {
			errStr = err.Error()
		} else {
			errStr = "testing simulated failure"
		}
		fatal("crypto/rand: failed to read random data (see https://go.dev/issue/66821): " + errStr)
		panic("unreachable") // To be sure.
	}
}

// The urandom fallback is only used on Linux kernels before 3.17 and on AIX.

var urandomOnce sync.Once
var urandomFile *os.File
var urandomErr error

func urandomRead(b []byte) error {
	urandomOnce.Do(func() {
		urandomFile, urandomErr = openUrandom()
	})
	if urandomErr != nil {
		return urandomErr
	}
	for len(b) > 0 {
		// Tagged os.File.Read has a simulated-backend interface arm, so escape
		// analysis cannot prove its buffer stays synchronous. This file is
		// structurally host-backed and Read never retains the slice.
		p := unsafe.Slice((*byte)(abi.NoEscape(unsafe.Pointer(unsafe.SliceData(b)))), len(b))
		n, err := urandomFile.Read(p)
		runtime.KeepAlive(b)
		// Note that we don't ignore EAGAIN because it should not be possible to
		// hit for a blocking read from urandom, although there were
		// unreproducible reports of it at https://go.dev/issue/9205.
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
