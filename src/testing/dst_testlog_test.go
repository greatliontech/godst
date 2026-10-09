// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package testing

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"
)

// The in-bubble test-log write returns its host error: the harness's
// buffered writer keeps it and StopTestLog's flush fails the binary, as
// the host path does, so a test log the harness could not deliver whole
// never reads as complete. A write that completes reports every byte,
// chunked at the atomicity budget, and the reader sees them all; a
// refused descriptor reports the error with nothing counted, spelled as
// the host path spells it; and the buffered writer above Write's
// in-bubble leg — driven through the bubble predicate — surfaces the
// error at its flush.
func TestDSTTestlogWriterReturnsTheHostWriteError(tt *T) {
	r, w, err := os.Pipe()
	if err != nil {
		tt.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	var got bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(&got, r)
		done <- err
	}()
	payload := bytes.Repeat([]byte("open /etc/hosts\n"), 2*dstPipeBufChunk/len("open /etc/hosts\n")+3)
	tl := &dstTestlogWriter{f: w, fd: int(w.Fd())}
	n, err := tl.writeFromBubble(payload)
	if err != nil || n != len(payload) {
		tt.Fatalf("a complete in-bubble write reported %d, %v; want %d, nil", n, err, len(payload))
	}
	w.Close()
	if err := <-done; err != nil {
		tt.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		tt.Fatalf("the reader saw %d bytes, want the %d written", got.Len(), len(payload))
	}
	// A descriptor the host refuses — the pipe's read end, open for
	// reading only, so EBADF is deterministic with no close and no reuse
	// window: the error returns with nothing counted delivered, as a
	// *os.PathError naming the log, the shape the host path's Write gives.
	dead := &dstTestlogWriter{f: w, fd: int(r.Fd())}
	n, err = dead.writeFromBubble([]byte("getenv HOME\n"))
	var pathErr *os.PathError
	if !errors.Is(err, syscall.EBADF) || !errors.As(err, &pathErr) || pathErr.Op != "write" || n != 0 {
		tt.Fatalf("a refused in-bubble write reported %d, %v; want 0 and a write *os.PathError wrapping EBADF", n, err)
	}
	// Write's in-bubble leg, end to end through the harness's buffered
	// writer: with the bubble predicate answering true, a buffered record
	// flushed over the refused descriptor fails the flush with the error.
	prior := dstTestlogInBubble
	dstTestlogInBubble = func() bool { return true }
	defer func() { dstTestlogInBubble = prior }()
	bw := bufio.NewWriter(dead)
	if _, err := bw.WriteString("getenv HOME\n"); err != nil {
		tt.Fatalf("the buffered record refused before its flush: %v", err)
	}
	if err := bw.Flush(); !errors.Is(err, syscall.EBADF) {
		tt.Fatalf("the buffered writer's flush over the refused descriptor answered %v, want EBADF kept", err)
	}
}
