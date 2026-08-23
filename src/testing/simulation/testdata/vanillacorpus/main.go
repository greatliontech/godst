// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The probe corpus for the untagged text-identity gate (design.md,
// "Untagged footprint (contract)", INV-VANILLA): one program whose import
// closure covers every upstream-present std package the dst delta modifies,
// and whose body references the patched surfaces so the linker keeps their
// symbols for the comparison. Built untagged by godst and by the upstream
// base toolchain; never run.
package main

import (
	cryptorand "crypto/rand"
	"net"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

type big struct{ buf [64]byte }

func main() {
	// runtime: scheduling, GC, finalizers, cleanups.
	println(runtime.NumCPU())
	b := &big{}
	runtime.SetFinalizer(b, func(*big) { println("f") })
	runtime.AddCleanup(new(int), func(int) {}, 0)
	b = nil
	runtime.GC()

	// sync (and through it internal/sync's HashTrieMap instantiations).
	var m sync.Map
	m.Store("k", 1)
	m.Range(func(any, any) bool { return true })
	var mu sync.RWMutex
	mu.Lock()
	mu.Unlock()

	// maps (internal/runtime/maps) and channels.
	mm := map[string]int{"a": 1}
	for range mm {
	}
	ch := make(chan int, 1)
	go func() { ch <- 1 }()
	<-ch

	// os: files, Root, pipes, process identity.
	if f, err := os.CreateTemp("", "corpus"); err == nil {
		f.Write([]byte("x"))
		f.Seek(0, 0)
		f.Sync()
		f.Close()
		os.Chtimes(f.Name(), time.Time{}, time.Now())
		os.Chmod(f.Name(), 0o600)
		os.Remove(f.Name())
	}
	if root, err := os.OpenRoot(os.TempDir()); err == nil {
		root.Mkdir("d", 0o700)
		root.Close()
	}
	if r, w, err := os.Pipe(); err == nil {
		w.Close()
		r.Close()
	}
	println(os.Getpid())

	// syscall wrappers the fork splits or fences.
	var tv syscall.Timeval
	syscall.Gettimeofday(&tv)
	var buf [1]byte
	syscall.Read(-1, buf[:])
	syscall.Write(-1, buf[:])
	syscall.Pread(-1, buf[:], 0)
	syscall.Pwrite(-1, buf[:], 0)
	syscall.Seek(-1, 0, 0)
	syscall.Kill(0, 0)
	syscall.Fsync(-1)
	syscall.Fdatasync(-1)
	syscall.Flock(-1, 0)
	syscall.Madvise(nil, 0)
	syscall.Mprotect(nil, 0)
	syscall.Fstat(-1, new(syscall.Stat_t))

	// time: timers, zones.
	t := time.NewTimer(time.Millisecond)
	t.Stop()
	time.Now().In(time.Local)

	// net, os/user, os/signal, crypto/rand, testing.
	net.LookupTXT("invalid.invalid")
	net.Dial("tcp", "127.0.0.1:0")
	user.Current()
	user.LookupGroupId("0")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, os.Interrupt)
	signal.Stop(sc)
	var rb [8]byte
	cryptorand.Read(rb[:])
	testing.Verbose()
}
