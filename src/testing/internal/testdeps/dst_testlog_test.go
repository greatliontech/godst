// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package testdeps

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestDSTBubbleTestlogLockFreeAndErrorPreserving pins the invariants of the
// DST testlog path:
//  1. StartTestLog flushes "# test log\n" before any raw write lands, and
//     bubble writes bypass log.mu completely (succeeding while log.mu is held
//     locked on the same goroutine).
//  2. Host and bubble writes (including >4096B bufio-boundary crossings and
//     >5000B single-line arguments) never splice mid-line.
//  3. Non-file writers keep buffering host writes via bufio while panicking on
//     in-bubble writes.
//  4. Write errors on either the bubble or host raw path latch without heap
//     allocation and surface as a *os.PathError from StopTestLog.
func TestDSTBubbleTestlogLockFreeAndErrorPreserving(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "testlog.txt")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var deps TestDeps
	deps.StartTestLog(f)
	active := true

	prior := dstTestlogInBubble
	defer func() {
		dstTestlogInBubble = prior
		if active {
			_ = deps.StopTestLog()
		}
	}()

	// Hold log.mu on this goroutine while invoking log.Open/Getenv/Stat under
	// the bubble predicate: any attempt to lock log.mu would self-deadlock.
	dstTestlogInBubble = func() bool { return true }
	longBubble := "/" + strings.Repeat("segment/", 600) + "leaf"
	log.mu.Lock()
	log.Open("/alpha")
	log.Getenv("BETA")
	log.Stat(longBubble)
	log.mu.Unlock()

	// Emit host lines that straddle and exceed bufio's 4096-byte buffer size
	// interleaved with bubble writes: if host lines buffered in log.w, a
	// mid-line 4096-byte flush would splice with the bubble write before the
	// host's trailing '\n' flushed at StopTestLog.
	hostPad := "/" + strings.Repeat("h", 4080)
	hostCross := "/host-cross-" + strings.Repeat("c", 64)
	hostMega := "/" + strings.Repeat("m", 6000)

	dstTestlogInBubble = func() bool { return false }
	log.Open(hostPad)
	log.Stat(hostCross)
	dstTestlogInBubble = func() bool { return true }
	log.Open("/bubble-between-host-writes")
	dstTestlogInBubble = func() bool { return false }
	log.Chdir(hostMega)
	dstTestlogInBubble = func() bool { return true }
	log.Getenv("AFTER_MEGA")
	dstTestlogInBubble = func() bool { return false }
	log.Chdir("/gamma")

	active = false
	if err := deps.StopTestLog(); err != nil {
		t.Fatalf("StopTestLog on healthy file = %v, want nil", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := []string{
		"# test log",
		"open /alpha",
		"getenv BETA",
		"stat " + longBubble,
		"open " + hostPad,
		"stat " + hostCross,
		"open /bubble-between-host-writes",
		"chdir " + hostMega,
		"getenv AFTER_MEGA",
		"chdir /gamma",
	}
	want := strings.Join(wantLines, "\n") + "\n"
	if string(raw) != want {
		t.Fatalf("testlog content mismatch:\ngot  (%dB): %q\nwant (%dB): %q", len(raw), clip(raw), len(want), clip([]byte(want)))
	}

	// Non-file io.Writer fallback outside bubbles + refusal inside bubbles.
	var memBuf bytes.Buffer
	deps.StartTestLog(&memBuf)
	active = true
	dstTestlogInBubble = func() bool { return false }
	log.Open("/mem-host")
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		dstTestlogInBubble = func() bool { return true }
		log.Open("/mem-bubble")
	}()
	dstTestlogInBubble = prior
	if !panicked {
		t.Fatalf("bubble write on non-file testlog writer did not panic")
	}
	active = false
	if err := deps.StopTestLog(); err != nil {
		t.Fatalf("StopTestLog on memory buffer = %v, want nil", err)
	}
	if memBuf.String() != "open /mem-host\n" {
		t.Fatalf("non-file host testlog = %q, want %q", memBuf.String(), "open /mem-host\n")
	}

	// Refused descriptor: point an active state at a read-only pipe end so
	// write1 fails deterministically with EBADF on both bubble and host paths,
	// and verify StopTestLog surfaces a write *os.PathError wrapping EBADF.
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rPipe.Close()
	defer wPipe.Close()

	if errno := dstWriteTestlogLine(int(rPipe.Fd()), "open", "/x"); syscall.Errno(errno) != syscall.EBADF {
		t.Fatalf("dstWriteTestlogLine over read-only fd = errno %d (%v), want EBADF", errno, syscall.Errno(errno))
	}

	for _, inBubble := range []bool{true, false} {
		deps.StartTestLog(wPipe)
		active = true
		st := dstActiveTestlog.Load()
		st.fd = int(rPipe.Fd())
		dstTestlogInBubble = func() bool { return inBubble }
		log.Open("/refused")
		dstTestlogInBubble = prior
		active = false
		stopErr := deps.StopTestLog()
		var pathErr *os.PathError
		if !errors.Is(stopErr, syscall.EBADF) || !errors.As(stopErr, &pathErr) || pathErr.Op != "write" || pathErr.Path != wPipe.Name() {
			t.Fatalf("StopTestLog(inBubble=%v) after failed write = %v, want write *os.PathError(%q) wrapping EBADF", inBubble, stopErr, wPipe.Name())
		}
	}
}

func TestDSTBubbleTestlogZeroAllocs(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	var deps TestDeps
	deps.StartTestLog(devNull)
	active := true
	prior := dstTestlogInBubble
	defer func() {
		dstTestlogInBubble = prior
		if active {
			_ = deps.StopTestLog()
		}
	}()

	dstTestlogInBubble = func() bool { return true }
	shortPath := "/short/path"
	longPath := "/" + strings.Repeat("segment/", 150) + "leaf" // > 1200B (> 512B stack threshold)

	if allocs := testing.AllocsPerRun(100, func() {
		log.Open(shortPath)
	}); allocs != 0 {
		t.Fatalf("short bubble testlog write allocs = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		log.Stat(longPath)
	}); allocs != 0 {
		t.Fatalf("long (>512B) bubble testlog write allocs = %v, want 0", allocs)
	}

	// Error-latching transition and post-error fast return must also be 0-alloc.
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rPipe.Close()
	defer wPipe.Close()

	active = false
	if err := deps.StopTestLog(); err != nil {
		t.Fatalf("StopTestLog(devNull) = %v", err)
	}

	deps.StartTestLog(wPipe)
	active = true
	st := dstActiveTestlog.Load()
	st.fd = int(rPipe.Fd())
	if allocs := testing.AllocsPerRun(100, func() {
		st.errErrno.Store(0)
		log.Open(shortPath)
		log.Open(longPath)
	}); allocs != 0 {
		t.Fatalf("error-latching bubble testlog write allocs = %v, want 0", allocs)
	}
	active = false
	_ = deps.StopTestLog()
}

func clip(b []byte) string {
	if len(b) <= 160 {
		return string(b)
	}
	return string(bytes.Clone(b[:80])) + "..." + string(bytes.Clone(b[len(b)-80:]))
}
