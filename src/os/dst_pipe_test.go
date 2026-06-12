// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// Host-probed constants the simulated pipe models (Linux).
const (
	pipeCap = 65536 // default pipe capacity
	pipeBuf = 4096  // PIPE_BUF atomic-write bound
)

// hostFDs lists the process's open descriptors. Called OUTSIDE runs only
// (inside a run it would hit the simulated tree and miss the point).
func hostFDs(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("reading /proc/self/fd: %v", err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestDSTPipeBasic: transfer, EOF, Stat shape, identity, and the
// host-isolation invariant (no descriptor is allocated for a simulated
// pipe — including ends deliberately leaked unclosed past the run).
func TestDSTPipeBasic(t *testing.T) {
	before := hostFDs(t)
	simulation.Run(1, func() {
		start := time.Now()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("Pipe: %v", err)
		}
		if r.Name() != "|0" || w.Name() != "|1" {
			t.Fatalf("names = %q, %q", r.Name(), w.Name())
		}

		// Stat shape: fifo mode, 0600 perms, size 0 even with bytes
		// buffered, creation time on the bubble clock, both ends one inode.
		if _, err := w.Write([]byte("buffered")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		time.Sleep(time.Second) // virtual: ModTime must stay the creation instant
		rfi, err := r.Stat()
		if err != nil {
			t.Fatalf("r.Stat: %v", err)
		}
		wfi, err := w.Stat()
		if err != nil {
			t.Fatalf("w.Stat: %v", err)
		}
		if rfi.Mode() != os.ModeNamedPipe|0o600 {
			t.Fatalf("mode = %v, want prw-------", rfi.Mode())
		}
		if rfi.Size() != 0 {
			t.Fatalf("size = %d, want 0 (host reports 0 regardless of buffered bytes)", rfi.Size())
		}
		if !rfi.ModTime().Equal(start) {
			t.Fatalf("ModTime = %v, want creation time %v", rfi.ModTime(), start)
		}
		if rfi.Name() != "|0" || wfi.Name() != "|1" {
			t.Fatalf("stat names = %q, %q", rfi.Name(), wfi.Name())
		}
		if !os.SameFile(rfi, wfi) {
			t.Fatal("SameFile(r, w) = false, want true (one pipe inode)")
		}
		tf, err := os.Create("/f")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		tfi, _ := tf.Stat()
		if os.SameFile(rfi, tfi) {
			t.Fatal("SameFile(pipe, tree file) = true")
		}
		tf.Close()

		// Transfer + EOF.
		buf := make([]byte, 16)
		n, err := r.Read(buf)
		if n != 8 || err != nil || string(buf[:8]) != "buffered" {
			t.Fatalf("Read = %d, %v, %q", n, err, buf[:n])
		}
		if n, err := r.Read(nil); n != 0 || err != nil {
			t.Fatalf("zero-len read = %d, %v, want 0, nil", n, err)
		}
		if n, err := w.Write(nil); n != 0 || err != nil {
			t.Fatalf("zero-len write = %d, %v, want 0, nil", n, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("w.Close: %v", err)
		}
		if n, err := r.Read(buf); n != 0 || err != io.EOF {
			t.Fatalf("read after writer close = %d, %v, want 0, EOF", n, err)
		}

		// Fd has no honest answer (recorded stance, shared with tree files).
		func() {
			defer func() {
				if recover() == nil {
					t.Error("Fd() on a simulated pipe did not panic")
				}
			}()
			r.Fd()
		}()

		// Deliberately leak r unclosed past the run: the census below must
		// still match — a simulated pipe never occupies a descriptor.
	})
	if after := hostFDs(t); !equalStrings(before, after) {
		t.Fatalf("host fds changed across a pipe run: before %v, after %v", before, after)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDSTPipeErrorIdentity pins every host-probed error shape through
// errors.Is and the *PathError op/path structure.
func TestDSTPipeErrorIdentity(t *testing.T) {
	simulation.Run(1, func() {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("Pipe: %v", err)
		}
		wantPath := func(label string, err error, op, path string, target error) {
			t.Helper()
			var pe *os.PathError
			if !errors.As(err, &pe) {
				t.Fatalf("%s: %v is not a *PathError", label, err)
			}
			if pe.Op != op || pe.Path != path || !errors.Is(err, target) {
				t.Fatalf("%s = op %q path %q err %v; want op %q path %q is-target %v",
					label, pe.Op, pe.Path, pe.Err, op, path, target)
			}
		}

		// Wrong direction: EBADF — for writes even at zero length (the
		// host checks write access before the count); zero-length reads
		// return (0, nil) before the access check.
		_, err = r.Write([]byte("x"))
		wantPath("write on read end", err, "write", "|0", syscall.EBADF)
		_, err = w.Read(make([]byte, 1))
		wantPath("read on write end", err, "read", "|1", syscall.EBADF)
		_, err = r.Write(nil)
		wantPath("zero-len write on read end", err, "write", "|0", syscall.EBADF)
		if n, err := w.Read(nil); n != 0 || err != nil {
			t.Fatalf("zero-len read on write end = %d, %v, want 0, nil", n, err)
		}

		// SyscallConn rides the seam's fence on pipe ends too.
		if _, err := r.SyscallConn(); !isDSTUnsupportedFS(err) {
			t.Fatalf("SyscallConn on pipe = %v, want unsupported fence", err)
		}

		// Not seekable: ESPIPE, with the host's op names.
		_, err = r.Seek(0, io.SeekStart)
		wantPath("seek", err, "seek", "|0", syscall.ESPIPE)
		_, err = r.ReadAt(make([]byte, 1), 0)
		wantPath("ReadAt", err, "read", "|0", syscall.ESPIPE)
		_, err = w.WriteAt([]byte("x"), 0)
		wantPath("WriteAt", err, "write", "|1", syscall.ESPIPE)

		// No durability, no truncation: EINVAL.
		wantPath("truncate", w.Truncate(0), "truncate", "|1", syscall.EINVAL)
		wantPath("sync", w.Sync(), "sync", "|1", syscall.EINVAL)

		// Not a directory.
		_, err = r.Readdirnames(1)
		wantPath("readdirnames", err, "readdirent", "|0", syscall.ENOTDIR)
		wantPath("chdir", r.Chdir(), "chdir", "|0", syscall.ENOTDIR)

		// Chmod works on a pipe (host fchmod does) and shows in Stat.
		if err := w.Chmod(0o644); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		if fi, _ := w.Stat(); fi.Mode() != os.ModeNamedPipe|0o644 {
			t.Fatalf("mode after chmod = %v, want prw-r--r--", fi.Mode())
		}

		// EPIPE on write after reader close; zero-length write stays nil.
		if err := r.Close(); err != nil {
			t.Fatalf("r.Close: %v", err)
		}
		_, err = w.Write([]byte("x"))
		wantPath("write after r closed", err, "write", "|1", syscall.EPIPE)
		if n, err := w.Write(nil); n != 0 || err != nil {
			t.Fatalf("zero-len write after r closed = %d, %v, want 0, nil", n, err)
		}

		// Use after own close, double close — a closed own end beats the
		// zero-length early return in both directions.
		_, err = r.Read(make([]byte, 1))
		wantPath("read after own close", err, "read", "|0", os.ErrClosed)
		_, err = r.Read(nil)
		wantPath("zero-len read after own close", err, "read", "|0", os.ErrClosed)
		err = r.Close()
		wantPath("double close", err, "close", "|0", os.ErrClosed)
		if err := w.Close(); err != nil {
			t.Fatalf("w.Close: %v", err)
		}
		_, err = w.Write(nil)
		wantPath("zero-len write after own close", err, "write", "|1", os.ErrClosed)
	})
}

// TestDSTPipeBlocking proves the durable-blocking invariant and the
// blocking/wakeup semantics: the bubble clock only advances while every
// goroutine is durably blocked, so each time.Sleep below advancing AT ALL
// proves the concurrently blocked pipe op is durably blocked.
func TestDSTPipeBlocking(t *testing.T) {
	simulation.Run(1, func() {
		// Reader blocks until a writer supplies data; virtual time crossed
		// the sleep, proving the read blocked durably.
		r, w, _ := os.Pipe()
		start := time.Now()
		var got string
		var rerr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 16)
			n, err := r.Read(buf)
			got, rerr = string(buf[:n]), err
			if since := time.Since(start); since != 5*time.Second {
				t.Errorf("read unblocked at +%v, want +5s", since)
			}
		}()
		time.Sleep(5 * time.Second)
		if _, err := w.Write([]byte("ping")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		<-done
		if got != "ping" || rerr != nil {
			t.Fatalf("blocked read = %q, %v", got, rerr)
		}

		// Writer blocks at capacity; a reader draining unblocks it and the
		// full count comes back.
		big := make([]byte, pipeCap+pipeBuf+123)
		for i := range big {
			big[i] = byte(i)
		}
		wstart := time.Now()
		wdone := make(chan struct{})
		go func() {
			defer close(wdone)
			n, err := w.Write(big)
			if n != len(big) || err != nil {
				t.Errorf("big write = %d, %v, want %d, nil", n, err, len(big))
			}
			if time.Since(wstart) != 3*time.Second {
				t.Errorf("big write finished at +%v, want +3s (after the drain)", time.Since(wstart))
			}
		}()
		time.Sleep(3 * time.Second) // advances only once the writer is durably blocked at capacity
		var drained bytes.Buffer
		if _, err := io.CopyN(&drained, r, int64(len(big))); err != nil {
			t.Fatalf("drain: %v", err)
		}
		<-wdone
		if !bytes.Equal(drained.Bytes(), big) {
			t.Fatal("drained bytes differ from written bytes")
		}

		// Close-while-blocked, all three flavors.
		// (a) Writer closes: blocked reader gets EOF.
		r2, w2, _ := os.Pipe()
		done2 := make(chan struct{})
		go func() {
			defer close(done2)
			if n, err := r2.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Errorf("blocked read after w.Close = %d, %v, want 0, EOF", n, err)
			}
		}()
		time.Sleep(time.Second)
		w2.Close()
		<-done2
		r2.Close()

		// (b) Own end closes: blocked reader gets ErrClosed.
		r3, w3, _ := os.Pipe()
		done3 := make(chan struct{})
		go func() {
			defer close(done3)
			_, err := r3.Read(make([]byte, 1))
			if !errors.Is(err, os.ErrClosed) {
				t.Errorf("blocked read after own close = %v, want ErrClosed", err)
			}
		}()
		time.Sleep(time.Second)
		r3.Close()
		<-done3
		w3.Close()

		// (c) Reader closes under a blocked oversize write: partial count
		// plus EPIPE — the capacity worth of bytes went in, the rest could
		// not (host-probed: n=65536).
		r4, w4, _ := os.Pipe()
		done4 := make(chan struct{})
		go func() {
			defer close(done4)
			n, err := w4.Write(make([]byte, pipeCap+999))
			if n != pipeCap || !errors.Is(err, syscall.EPIPE) {
				t.Errorf("blocked write after r.Close = %d, %v, want %d, EPIPE", n, err, pipeCap)
			}
		}()
		time.Sleep(time.Second)
		r4.Close()
		<-done4
		w4.Close()
	})
}

// TestDSTPipeAtomicity: the PIPE_BUF guarantee — concurrent writes of at
// most 4096 bytes are never interleaved, *including* when the buffer lacks
// space for the whole record (the case that matters: a non-atomic writer
// would put a partial record in and let another writer's bytes land inside
// it). The pipe is pre-filled to within one record of capacity and the
// reader drains in sips smaller than any record, so every concurrent write
// runs through the contended wait-for-full-space path. Records are framed
// (1-byte writer id, 2-byte length, payload of repeated id) and the whole
// stream must parse back intact.
//
// The invariant quantifies over schedules, so the workload runs under a
// seed sweep: under several of these seeds (probed: 1, 3, 4, 6) a
// non-atomic write demonstrably interleaves, while under others the FIFO
// wake order happens to mask it — a single-seed version of this test was
// vacuous.
func TestDSTPipeAtomicity(t *testing.T) {
	for seed := uint64(1); seed <= 8; seed++ {
		testDSTPipeAtomicitySeed(t, seed)
		if t.Failed() {
			return
		}
	}
}

func testDSTPipeAtomicitySeed(t *testing.T, seed uint64) {
	t.Logf("seed %d", seed)
	simulation.Run(seed, func() {
		r, w, _ := os.Pipe()
		writeRec := func(id, size int) bool {
			rec := make([]byte, 3+size)
			rec[0] = byte(id)
			rec[1], rec[2] = byte(size>>8), byte(size)
			for j := 0; j < size; j++ {
				rec[3+j] = byte(id)
			}
			if n, err := w.Write(rec); n != len(rec) || err != nil {
				t.Errorf("writer %d: Write = %d, %v", id, n, err)
				return false
			}
			return true
		}

		// Pre-fill (single goroutine, parseable records) to free < any
		// phase-2 record, so the contended path is forced from the start.
		const fillID = 9 // distinct from writer ids
		total := 0
		for i := 0; i < 15; i++ {
			writeRec(fillID, pipeBuf-3)
			total += pipeBuf
		}
		writeRec(fillID, 2996)
		total += 2999
		// Buffer now holds 64439 of 65536: free = 1097, below the smallest
		// phase-2 record (1503).

		sizes := []int{1500, 2000, 2700, 3300, 4000, pipeBuf - 3} // payloads; +3 header keeps every record <= PIPE_BUF
		const writers, records = 4, 12
		var wg sync.WaitGroup
		for id := 0; id < writers; id++ {
			for i := 0; i < records; i++ {
				total += 3 + sizes[(id+i)%len(sizes)]
			}
		}
		for id := 0; id < writers; id++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; i < records; i++ {
					if !writeRec(id, sizes[(id+i)%len(sizes)]) {
						return
					}
				}
			}(id)
		}
		go func() {
			wg.Wait()
			w.Close()
		}()

		// Drain in sips smaller than any record so free space grows in
		// sub-record increments and writers keep hitting partial-space
		// states.
		var all []byte
		sip := make([]byte, 700)
		for {
			n, err := r.Read(sip)
			all = append(all, sip[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
		}
		if len(all) != total {
			t.Fatalf("read %d bytes, want %d", len(all), total)
		}
		counts := make([]int, writers)
		fills := 0
		for off := 0; off < len(all); {
			id := int(all[off])
			if id != fillID && id >= writers {
				t.Fatalf("offset %d: corrupt writer id %d (interleaved record)", off, id)
			}
			size := int(all[off+1])<<8 | int(all[off+2])
			if off+3+size > len(all) {
				t.Fatalf("offset %d: record overruns stream (interleaved record)", off)
			}
			for j := 0; j < size; j++ {
				if all[off+3+j] != byte(id) {
					t.Fatalf("offset %d: payload byte %d corrupted: writer %d got %d (interleaved record)",
						off, j, id, all[off+3+j])
				}
			}
			if id == fillID {
				fills++
			} else {
				counts[id]++
			}
			off += 3 + size
		}
		if fills != 16 {
			t.Fatalf("parsed %d fill records, want 16", fills)
		}
		for id, c := range counts {
			if c != records {
				t.Fatalf("writer %d: parsed %d records, want %d", id, c, records)
			}
		}
		r.Close()
	})
}

// TestDSTPipeDeadline: deadlines ride the bubble clock with the host's
// exact precedence (expired deadline beats buffered data AND the
// wrong-direction EBADF), fire at the precise virtual instant while
// blocked, re-arm when moved, and clear with the zero time. Tree files
// keep the host's regular-file shape: bare ErrNoDeadline.
func TestDSTPipeDeadline(t *testing.T) {
	simulation.Run(1, func() {
		r, w, _ := os.Pipe()
		start := time.Now()

		// Blocked read fails at exactly the virtual deadline.
		if err := r.SetReadDeadline(start.Add(2 * time.Second)); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		_, err := r.Read(make([]byte, 1))
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("read past deadline = %v, want ErrDeadlineExceeded", err)
		}
		var pe *os.PathError
		if !errors.As(err, &pe) || pe.Op != "read" || pe.Path != "|0" {
			t.Fatalf("deadline error shape = %v", err)
		}
		if since := time.Since(start); since != 2*time.Second {
			t.Fatalf("deadline fired at +%v, want exactly +2s", since)
		}

		// Expired deadline beats buffered data...
		w.Write([]byte("data"))
		if _, err := r.Read(make([]byte, 4)); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("read with data past deadline = %v, want ErrDeadlineExceeded", err)
		}
		// ...but NOT a zero-length read (host: the empty-buffer return
		// precedes the deadline check on the read side only)...
		if n, err := r.Read(nil); n != 0 || err != nil {
			t.Fatalf("zero-len read past deadline = %d, %v, want 0, nil", n, err)
		}
		// ...nor positional I/O: the host bypasses the poller for
		// pread/pwrite, so ESPIPE wins even with the deadline expired...
		if _, err := r.ReadAt(make([]byte, 1), 0); !errors.Is(err, syscall.ESPIPE) {
			t.Fatalf("ReadAt past read deadline = %v, want ESPIPE (deadlines do not apply to pread)", err)
		}
		// ...while a zero-length WRITE does lose to its expired deadline
		// (and pwrite stays ESPIPE there too).
		if err := w.SetWriteDeadline(start.Add(time.Second)); err != nil {
			t.Fatalf("SetWriteDeadline(w): %v", err)
		}
		if _, err := w.Write(nil); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("zero-len write past deadline = %v, want ErrDeadlineExceeded", err)
		}
		if _, err := w.WriteAt([]byte("x"), 0); !errors.Is(err, syscall.ESPIPE) {
			t.Fatalf("WriteAt past write deadline = %v, want ESPIPE (deadlines do not apply to pwrite)", err)
		}
		if err := w.SetWriteDeadline(time.Time{}); err != nil {
			t.Fatalf("clear w deadline: %v", err)
		}
		// ...and an expired WRITE deadline on the READ end beats EBADF
		// (host-probed precedence).
		if err := r.SetWriteDeadline(start.Add(time.Second)); err != nil {
			t.Fatalf("SetWriteDeadline: %v", err)
		}
		if _, err := r.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("write on read end past write deadline = %v, want ErrDeadlineExceeded", err)
		}

		// Zero time clears; the buffered data is readable again.
		if err := r.SetDeadline(time.Time{}); err != nil {
			t.Fatalf("clear deadline: %v", err)
		}
		buf := make([]byte, 4)
		if n, err := r.Read(buf); n != 4 || err != nil || string(buf) != "data" {
			t.Fatalf("read after clear = %d, %v, %q", n, err, buf[:n])
		}
		if _, err := r.Write([]byte("x")); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("write on read end after clear = %v, want EBADF", err)
		}

		// Setting a deadline WAKES a blocked op (the bump-on-SetDeadline
		// edge): reader blocks with no deadline, then a past deadline lands.
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err := r.Read(make([]byte, 1))
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("blocked read after deadline set = %v, want ErrDeadlineExceeded", err)
			}
		}()
		time.Sleep(time.Second) // reader is durably blocked
		if err := r.SetReadDeadline(time.Now().Add(-time.Nanosecond)); err != nil {
			t.Fatalf("SetReadDeadline(past): %v", err)
		}
		<-done
		r.SetReadDeadline(time.Time{})

		// A moved deadline re-arms: blocked write at capacity, deadline
		// extended mid-block, fires at the SECOND instant.
		if _, err := w.Write(make([]byte, pipeCap)); err != nil { // fill
			t.Fatalf("fill: %v", err)
		}
		w.SetWriteDeadline(time.Now().Add(2 * time.Second))
		wstart := time.Now()
		wdone := make(chan struct{})
		go func() {
			defer close(wdone)
			n, err := w.Write(make([]byte, 10))
			if n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("write past moved deadline = %d, %v, want 0, ErrDeadlineExceeded", n, err)
			}
			if since := time.Since(wstart); since != 4*time.Second {
				t.Errorf("moved deadline fired at +%v, want +4s", since)
			}
		}()
		time.Sleep(time.Second) // writer durably blocked on the first deadline's timer
		if err := w.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatalf("move deadline: %v", err)
		}
		<-wdone

		// Tree files and directories: host regular-file shape, bare
		// ErrNoDeadline.
		f, err := os.Create("/plain")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := f.SetDeadline(time.Now()); !errors.Is(err, os.ErrNoDeadline) {
			t.Fatalf("tree file SetDeadline = %v, want ErrNoDeadline", err)
		}
		var pe2 *os.PathError
		if errors.As(f.SetDeadline(time.Now()), &pe2) {
			t.Fatal("tree file SetDeadline wrapped in PathError; host returns it bare")
		}
		f.Close()
		d, err := os.Open("/tmp")
		if err != nil {
			t.Fatalf("Open dir: %v", err)
		}
		if err := d.SetReadDeadline(time.Now()); !errors.Is(err, os.ErrNoDeadline) {
			t.Fatalf("dir SetReadDeadline = %v, want ErrNoDeadline", err)
		}
		d.Close()

		// SetDeadline on a CLOSED handle: the host's bare "use of closed
		// file" (poll's closing sentinel, unwrapped, NOT ErrNoDeadline) —
		// on every backend.
		r.Close()
		w.Close()
		wantClosedBare := func(label string, err error) {
			t.Helper()
			var pe *os.PathError
			if err == nil || errors.As(err, &pe) || errors.Is(err, os.ErrNoDeadline) ||
				err.Error() != "use of closed file" {
				t.Fatalf("%s = %v, want bare \"use of closed file\"", label, err)
			}
		}
		wantClosedBare("closed pipe SetDeadline", w.SetDeadline(time.Now()))
		wantClosedBare("closed tree file SetDeadline", f.SetDeadline(time.Now()))
	})
}

// TestDSTPipeCopyInterplay: io.Copy across backends — pipe<->tree through
// the gated funnels, pipe->host across the mixed-handle stance (the
// zero-copy fast paths must bail to the generic loop whenever either side
// is simulated; on the host these pairs would take splice/copy_file_range).
func TestDSTPipeCopyInterplay(t *testing.T) {
	host, err := os.CreateTemp("", "dst-pipe-host-*")
	if err != nil {
		t.Fatalf("CreateTemp (host, pre-run): %v", err)
	}
	defer os.Remove(host.Name())
	defer host.Close()

	const payload = "zero-copy interplay payload"
	simulation.Run(1, func() {
		// Tree file -> pipe (File.ReadFrom on the write end).
		src, err := os.Create("/src")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		src.WriteString(payload)
		src.Seek(0, io.SeekStart)
		r, w, _ := os.Pipe()
		copied := make(chan struct{})
		go func() {
			defer close(copied)
			if n, err := io.Copy(w, src); n != int64(len(payload)) || err != nil {
				t.Errorf("Copy(pipe, tree) = %d, %v", n, err)
			}
			w.Close()
		}()

		// Pipe -> tree file (File.ReadFrom on a tree file).
		dst, err := os.Create("/dst")
		if err != nil {
			t.Fatalf("Create dst: %v", err)
		}
		if n, err := io.Copy(dst, r); n != int64(len(payload)) || err != nil {
			t.Fatalf("Copy(tree, pipe) = %d, %v", n, err)
		}
		<-copied
		got, err := os.ReadFile("/dst")
		if err != nil || string(got) != payload {
			t.Fatalf("round trip = %q, %v", got, err)
		}

		// Pipe -> pre-run HOST file: simulated source, host sink — the
		// mixed-handle stance; the host half does real I/O.
		r2, w2, _ := os.Pipe()
		go func() {
			w2.WriteString(payload)
			w2.Close()
		}()
		if n, err := io.Copy(host, r2); n != int64(len(payload)) || err != nil {
			t.Fatalf("Copy(host, pipe) = %d, %v", n, err)
		}
		r2.Close()
		src.Close()
		dst.Close()
		r.Close()
	})

	// Verify the host half actually landed on the host, outside the run.
	if _, err := host.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("host seek: %v", err)
	}
	got, err := io.ReadAll(host)
	if err != nil || string(got) != payload {
		t.Fatalf("host file content = %q, %v", got, err)
	}
}

// TestDSTPipeStdioSwap pins the spec's stdio-stance mechanics: the standard
// streams are plain *File package variables, so a program that wants
// captured, deterministic output assigns one to a simulated file inside the
// run — no fork machinery involved (the fork itself never swaps them).
func TestDSTPipeStdioSwap(t *testing.T) {
	simulation.Run(1, func() {
		r, w, _ := os.Pipe()
		old := os.Stdout
		os.Stdout = w
		defer func() { os.Stdout = old }() // restore even on a mid-swap failure
		fmt.Println("captured deterministically")
		os.Stdout = old
		if err := w.Close(); err != nil {
			t.Fatalf("w.Close: %v", err)
		}
		got, err := io.ReadAll(r)
		if err != nil || string(got) != "captured deterministically\n" {
			t.Fatalf("captured = %q, %v", got, err)
		}
		r.Close()
	})
}

// TestDSTPipeLeakAndReplay: a pipe end leaked out of its run is fenced (its
// blocking machinery belongs to the dead bubble) — except Close, which
// always works; and the same seed yields a byte-identical transcript of a
// concurrent pipe workload (the in-process determinism pin; cross-process
// replay is the testprog fixture).
func TestDSTPipeLeakAndReplay(t *testing.T) {
	var leakedR, leakedW *os.File
	transcript := func() string {
		var mu sync.Mutex
		var b strings.Builder
		simulation.Run(42, func() {
			r, w, _ := os.Pipe()
			// A dedicated pair the run never closes — created in BOTH runs
			// so the two transcripts come from the identical program (the
			// determinism invariant quantifies over identical programs);
			// only the first run's pair is kept for the fence checks below.
			lr, lw, _ := os.Pipe()
			if leakedR == nil {
				leakedR, leakedW = lr, lw
			}
			var wg sync.WaitGroup
			for g := 0; g < 3; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < 5; i++ {
						fmt.Fprintf(w, "[g%d:%d]", g, i)
					}
				}(g)
			}
			done := make(chan struct{})
			var total int
			go func() {
				defer close(done)
				buf := make([]byte, 8)
				for {
					n, err := r.Read(buf)
					mu.Lock()
					b.WriteString(string(buf[:n]))
					total += n
					mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
			wg.Wait()
			w.Close()
			<-done
			fmt.Fprintf(&b, "|total=%d", total)
		})
		return b.String()
	}
	first := transcript()
	second := transcript()
	if first != second {
		t.Fatalf("same seed, different transcripts:\n  %q\n  %q", first, second)
	}
	if !strings.HasSuffix(first, "|total=90") {
		t.Fatalf("transcript = %q, want 90 payload bytes (3 writers x 5 records x 6 bytes)", first)
	}

	// The leaked ends: fenced in a later run and outside any run.
	if _, err := leakedW.Write([]byte("x")); !isDSTUnsupportedFS(err) {
		t.Fatalf("leaked pipe write outside run = %v, want unsupported fence", err)
	}
	simulation.Run(43, func() {
		if _, err := leakedR.Read(make([]byte, 1)); !isDSTUnsupportedFS(err) {
			t.Errorf("leaked pipe read in later run = %v, want unsupported fence", err)
		}
		if _, err := leakedR.Stat(); !isDSTUnsupportedFS(err) {
			t.Errorf("leaked pipe stat in later run = %v, want unsupported fence", err)
		}
		if err := leakedW.SetDeadline(time.Now()); !isDSTUnsupportedFS(err) {
			t.Errorf("leaked pipe SetDeadline in later run = %v, want unsupported fence", err)
		}
	})
	// Close always works, once.
	if err := leakedR.Close(); err != nil {
		t.Fatalf("leaked r.Close = %v, want nil", err)
	}
	if err := leakedW.Close(); err != nil {
		t.Fatalf("leaked w.Close = %v, want nil", err)
	}
	if err := leakedW.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("leaked double close = %v, want ErrClosed", err)
	}
}
