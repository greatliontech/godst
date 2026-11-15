// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"internal/poll"
	isync "internal/sync"
	"io"
	"syscall"
	"time"
	"unsafe"
)

// Under deterministic simulation, os.Pipe returns a pair of Files backed by
// an in-memory byte stream — the second implementation of the dstFileBackend
// seam, the stream-shaped backend the disk feature's design reserved this
// slot for. Determinism rides the schedule as everywhere else: operations are
// ordered by the deterministic scheduler, blocking waits on channels created
// inside the bubble (so a blocked pipe read or write is synctest-durably
// blocked), and deadlines ride the bubble's fake clock exactly as the
// simulated network's do. No host descriptor is ever allocated.
//
// Error identity and operation ordering are host-probed (Linux): an expired
// deadline beats both buffered data and the wrong-direction EBADF check; a
// blocked write that already transferred bytes reports the partial count
// alongside EPIPE or the deadline error; a zero-length read or write returns
// (0, nil) ahead of the remaining checks in a direction-specific order
// (reads: after only the closed check; writes: after the closed, deadline,
// and access-mode checks, ahead of EPIPE). Authoritative model:
// docs/dst/design.md, "Deterministic pipes and the stdio stance".

// Linux shapes (host-probed):
const (
	dstPipeCap = 65536 // default pipe capacity (fcntl F_GETPIPE_SZ)
	dstPipeBuf = 4096  // PIPE_BUF: a write of at most this size is atomic
)

// The buffer is a byte-exact ring; the kernel's is a ring of 16 page slots
// whose fragmentation (partially-read head, merge-overflow spills,
// interrupted writes' tails) can admit page-granular slack less than the
// byte count suggests. Byte-exactness is the recorded stance (design.md, the
// pipe section): the divergence is one-directional (the simulation admits at
// least what the kernel would — never a sim-only block, i.e. never a false
// positive), and mirroring the slot ring would couple the model to
// kernel-version-dependent merge semantics.

// dstPipe is the shared stream: one per os.Pipe call, referenced by both
// ends — the analogue of the single pipe inode both host fds share (which is
// also why SameFile(rfi, wfi) is true, via the FileInfo identity field).
// All state is guarded by mu.
type dstPipe struct {
	mu        isync.KernelMutex
	epoch     uint64        // run the pipe belongs to; see dstPipeEnd.enter
	buf       []byte        // unread bytes; len(buf) <= dstPipeCap
	gen       chan struct{} // broadcast: closed and replaced on every state change
	rclosed   bool          // read end closed
	wclosed   bool          // write end closed
	mode      FileMode      // permission bits (fchmod works on a host pipe fd)
	ctime     time.Time     // creation time on the bubble clock, for Stat
	raceSync  uint64        // per-pipe data happens-before word (writer -> reader)
	eofSync   uint64        // writer close -> reader io.EOF happens-before word
	epipeSync uint64        // reader close -> writer syscall.EPIPE happens-before word
}

// bump wakes every goroutine blocked on the pipe's state. Caller holds mu.
// Only in-run callers reach bump: gen is a bubble channel, and touching it
// from outside the pipe's run would panic in synctest (see closeFile for the
// one op that can legitimately run there).
func (p *dstPipe) bump() {
	close(p.gen)
	p.gen = make(chan struct{})
}

// dstPipeEnd is one direction of a dstPipe wired behind an *os.File: the
// read end ("|0") or the write end ("|1"). Whether an end is closed lives on
// the shared pipe (rclosed/wclosed) — single source, also read by the peer
// for EOF/EPIPE — never duplicated here.
var _ dstFileBackend = (*dstPipeEnd)(nil)

type dstPipeEnd struct {
	p         *dstPipe
	reader    bool
	rd, wd    time.Time // read/write deadlines, guarded by p.mu
	closeSync uint64    // per-end close happens-before word (Close -> own-end ErrClosed)
}

// dstNewPipe builds the simulated pipe pair while a run is active.
func dstNewPipe() (r, w *File, handled bool) {
	if !dstFSActive() {
		return nil, nil, false
	}
	now := time.Now()
	dstKernelSectionEnter()
	defer dstKernelSectionExit()
	p := &dstPipe{
		epoch: dstFSEpoch(),
		gen:   make(chan struct{}),
		mode:  0o600,
		ctime: now,
	}
	return dstNewFile(&dstPipeEnd{p: p, reader: true}, "|0"),
		dstNewFile(&dstPipeEnd{p: p, reader: false}, "|1"), true
}

// enter validates that the operation runs inside the pipe's own run and
// acquires p.mu. A pipe end leaked out of its run is FENCED (unsupported
// shape): unlike a leaked tree file, whose orphaned nodes still work
// meaningfully nowhere, a pipe op may need to block on the old bubble's
// channels, which a later run (or no run) cannot do. Own-end closure is
// checked by the operations themselves, which also re-check it after every
// wait.
func (e *dstPipeEnd) enter() error {
	if !dstFSActive() {
		return dstErrUnsupportedFS
	}
	e.p.mu.Lock()
	if e.p.epoch != dstFSEpoch() {
		e.p.mu.Unlock()
		return dstErrUnsupportedFS
	}
	return nil
}

func (e *dstPipeEnd) coarseCloseRead() {
	if dstFSActive() && dstInSimBubble() {
		dstCoarseDep(uintptr(unsafe.Pointer(&e.closeSync)), false)
	}
}

// ownClosed reports whether this end has been closed. Caller holds p.mu.
func (e *dstPipeEnd) ownClosed() bool {
	closed := e.p.wclosed
	if e.reader {
		closed = e.p.rclosed
	}
	if closed {
		dstKernelRaceAcquire(unsafe.Pointer(&e.closeSync))
	}
	return closed
}

// deadline returns the deadline governing the given direction.
// Caller holds p.mu.
func (e *dstPipeEnd) deadline(read bool) time.Time {
	if read {
		return e.rd
	}
	return e.wd
}

// deadlineExpired reports whether the direction's deadline is set and has
// passed on the bubble clock. Caller holds p.mu.
func (e *dstPipeEnd) deadlineExpired(read bool) bool {
	d := e.deadline(read)
	return !d.IsZero() && !time.Now().Before(d)
}

// wait blocks until the pipe's state changes or the direction's deadline
// fires, releasing p.mu while blocked and reacquiring it before returning.
// The broadcast channel and the timer are bubble resources: a goroutine
// blocked here is synctest-durably blocked, and the deadline rides the fake
// clock. Callers loop and re-derive everything (closure, deadline, space,
// data) from current state after each wake, so a stale wake — or a timer
// armed from a deadline that has since been moved — is harmless.
func (e *dstPipeEnd) wait(read bool) {
	p := e.p
	gen := p.gen
	d := e.deadline(read)
	p.mu.Unlock()
	if d.IsZero() {
		<-gen
		p.mu.Lock()
		return
	}
	t := time.NewTimer(time.Until(d))
	defer t.Stop()
	select {
	case <-gen:
	case <-t.C:
	}
	p.mu.Lock()
}

func (e *dstPipeEnd) read(b []byte) (n int, err error) {
	p := e.p
	pipeID := uintptr(unsafe.Pointer(p))
	if dstFSActive() && dstInSimBubble() {
		dstCoarseDep(pipeID, true)
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.closeSync)), false)
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.rd)), false)
	}
	var kbuf []byte
	err = func() error {
		if err := e.enter(); err != nil {
			return err
		}
		defer p.mu.Unlock()
		// Host-probed entry order for reads: the closed-fd check, then the
		// zero-length early return — ahead of the deadline and access-mode
		// checks (r.Read(nil) with an expired deadline, and w.Read(nil), are
		// both (0, nil) on the host; a closed own end is ErrClosed even for
		// zero-length).
		if e.ownClosed() {
			return poll.ErrFileClosing
		}
		if len(b) == 0 {
			return nil
		}
		for {
			// Host-probed loop order: closed fd (re-checked after every wait —
			// close while blocked), then expired deadline, then the
			// access-mode check (an expired read deadline on the WRITE end
			// still wins over EBADF).
			if e.ownClosed() {
				return poll.ErrFileClosing
			}
			if e.deadlineExpired(true) {
				return poll.ErrDeadlineExceeded
			}
			if !e.reader {
				return syscall.EBADF
			}
			if len(p.buf) > 0 {
				take := min(len(b), len(p.buf))
				kbuf = make([]byte, take)
				copy(kbuf, p.buf[:take])
				p.buf = p.buf[:copy(p.buf, p.buf[take:])]
				dstKernelRaceAcquire(unsafe.Pointer(&p.raceSync))
				p.bump() // space freed: wake blocked writers
				return nil
			}
			if p.wclosed {
				dstKernelRaceAcquire(unsafe.Pointer(&p.raceSync))
				dstKernelRaceAcquire(unsafe.Pointer(&p.eofSync))
				return io.EOF
			}
			e.wait(true)
		}
	}()
	if len(kbuf) > 0 {
		n = copy(b, kbuf)
	}
	return n, err
}

func (e *dstPipeEnd) write(b []byte) (n int, err error) {
	p := e.p
	pipeID := uintptr(unsafe.Pointer(p))
	if dstFSActive() && dstInSimBubble() {
		dstCoarseDep(pipeID, true)
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.closeSync)), false)
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.wd)), false)
	}
	// Host-probed entry order for writes — stricter than reads: closed fd,
	// then expired deadline, then access mode, and only then the
	// zero-length early return (w.Write(nil) with an expired write
	// deadline is ErrDeadlineExceeded and r.Write(nil) is EBADF on the
	// host), which in turn precedes the EPIPE check (w.Write(nil) with the
	// read end closed is (0, nil)).
	done, entryErr := func() (bool, error) {
		if err := e.enter(); err != nil {
			return true, err
		}
		defer p.mu.Unlock()
		if e.ownClosed() {
			return true, poll.ErrFileClosing
		}
		if e.deadlineExpired(false) {
			return true, poll.ErrDeadlineExceeded
		}
		if e.reader {
			return true, syscall.EBADF
		}
		if len(b) == 0 {
			return true, nil
		}
		return false, nil
	}()
	if done {
		return 0, entryErr
	}
	for n < len(b) {
		remLen := len(b) - n
		want, waitErr := func() (int, error) {
			if err := e.enter(); err != nil {
				return 0, err
			}
			defer p.mu.Unlock()
			for {
				// Same host-probed order as read; EPIPE after them. A wake-up
				// that lands on an error path reports the bytes already
				// transferred alongside the error (host-probed partial count).
				if e.ownClosed() {
					return 0, poll.ErrFileClosing
				}
				if e.deadlineExpired(false) {
					return 0, poll.ErrDeadlineExceeded
				}
				if e.reader {
					return 0, syscall.EBADF
				}
				if p.rclosed {
					dstKernelRaceAcquire(unsafe.Pointer(&p.epipeSync))
					return 0, syscall.EPIPE
				}
				free := dstPipeCap - len(p.buf)
				if remLen <= dstPipeBuf {
					// PIPE_BUF atomicity: the remainder goes in whole or not at
					// all, so concurrent small writes never interleave.
					if free >= remLen {
						return remLen, nil
					}
				} else if free > 0 {
					return min(free, remLen), nil
				}
				e.wait(false)
			}
		}()
		if waitErr != nil {
			return n, waitErr
		}
		// Copy only this chunk outside p.mu so caller slice accesses remain
		// race- and DPOR-visible per chunk and mapping traps/panics never hold
		// kernel locks.
		chunk := make([]byte, want)
		copy(chunk, b[n:n+want])
		wrote, postErr := func() (int, error) {
			if err := e.enter(); err != nil {
				return 0, err
			}
			defer p.mu.Unlock()
			if e.ownClosed() {
				return 0, poll.ErrFileClosing
			}
			if e.deadlineExpired(false) {
				return 0, poll.ErrDeadlineExceeded
			}
			if e.reader {
				return 0, syscall.EBADF
			}
			if p.rclosed {
				dstKernelRaceAcquire(unsafe.Pointer(&p.epipeSync))
				return 0, syscall.EPIPE
			}
			free := dstPipeCap - len(p.buf)
			if remLen <= dstPipeBuf {
				if free < len(chunk) {
					return 0, nil // raced with another writer; re-wait
				}
				p.buf = append(p.buf, chunk...)
				dstKernelRaceReleaseMerge(unsafe.Pointer(&p.raceSync))
				p.bump()
				return len(chunk), nil
			}
			if free == 0 {
				return 0, nil // raced with another writer; re-wait
			}
			k := min(free, len(chunk))
			p.buf = append(p.buf, chunk[:k]...)
			dstKernelRaceReleaseMerge(unsafe.Pointer(&p.raceSync))
			p.bump() // data available: wake blocked readers
			return k, nil
		}()
		n += wrote
		if postErr != nil {
			return n, postErr
		}
	}
	return n, nil
}

// pread/pwrite: a pipe is not seekable, and deadlines deliberately do NOT
// apply — host poll.FD.Pread/Pwrite bypass the poller entirely ("using the
// poller doesn't make sense for pread"): only the closed check precedes the
// syscall, so ESPIPE wins even over an expired deadline (host-probed).
func (e *dstPipeEnd) pread(b []byte, off int64) (int, error) {
	return 0, e.simpleErr(syscall.ESPIPE)
}

func (e *dstPipeEnd) pwrite(b []byte, off int64) (int, error) {
	return 0, e.simpleErr(syscall.ESPIPE)
}

func (e *dstPipeEnd) seek(offset int64, whence int) (int64, error) {
	e.coarseCloseRead()
	if err := e.enter(); err != nil {
		return 0, err
	}
	defer e.p.mu.Unlock()
	if e.ownClosed() {
		return 0, poll.ErrFileClosing
	}
	return 0, syscall.ESPIPE
}

func (e *dstPipeEnd) truncate(size int64) error {
	return e.simpleErr(syscall.EINVAL)
}

// sync: fsync(2) on a pipe is EINVAL — a pipe has no durable image, and is
// deliberately outside the filesystem durability contract.
func (e *dstPipeEnd) sync() error {
	return e.simpleErr(syscall.EINVAL)
}

func (e *dstPipeEnd) chdirHandle() error {
	return e.simpleErr(syscall.ENOTDIR)
}

// simpleErr is the shared shape of the always-failing metadata ops: validate
// the run, report closure, then the fixed errno.
func (e *dstPipeEnd) simpleErr(errno error) error {
	e.coarseCloseRead()
	if err := e.enter(); err != nil {
		return err
	}
	defer e.p.mu.Unlock()
	if e.ownClosed() {
		return poll.ErrFileClosing
	}
	return errno
}

func (e *dstPipeEnd) readdir(n int) ([]string, []FileInfo, error) {
	e.coarseCloseRead()
	if err := e.enter(); err != nil {
		return nil, nil, err
	}
	defer e.p.mu.Unlock()
	if e.ownClosed() {
		return nil, nil, poll.ErrFileClosing
	}
	return nil, nil, syscall.ENOTDIR
}

func (e *dstPipeEnd) chmodHandle(mode FileMode) error {
	if dstFSActive() && dstInSimBubble() {
		dstCoarseDep(uintptr(unsafe.Pointer(e.p)), true)
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.closeSync)), false)
	}
	if err := e.enter(); err != nil {
		return err
	}
	defer e.p.mu.Unlock()
	if e.ownClosed() {
		return poll.ErrFileClosing
	}
	e.p.mode = e.p.mode&^dstFSModeMask | mode&dstFSModeMask
	return nil
}

func (e *dstPipeEnd) stat() (FileInfo, error) {
	if dstFSActive() && dstInSimBubble() {
		dstCoarseDep(uintptr(unsafe.Pointer(e.p)), false)
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.closeSync)), false)
	}
	if err := e.enter(); err != nil {
		return nil, err
	}
	defer e.p.mu.Unlock()
	if e.ownClosed() {
		return nil, poll.ErrFileClosing
	}
	name := "|0"
	if !e.reader {
		name = "|1"
	}
	return &dstFileInfo{
		name:    name,
		size:    0, // the host reports st_size 0 for pipes regardless of buffered bytes
		mode:    ModeNamedPipe | e.p.mode,
		modTime: e.p.ctime,
		ident:   e.p,
	}, nil
}

// setDeadline. On a leaked end the enter() fence error surfaces BARE (the
// setDeadline funnels never wrap) — consistent with every other SetDeadline
// error shape, which the host also reports unwrapped.
func (e *dstPipeEnd) setDeadline(rd, wd bool, t time.Time) error {
	if dstFSActive() && dstInSimBubble() {
		dstCoarseDep(uintptr(unsafe.Pointer(&e.closeSync)), false)
		if rd {
			dstCoarseRecord(uintptr(unsafe.Pointer(&e.rd)), true)
		}
		if wd {
			dstCoarseRecord(uintptr(unsafe.Pointer(&e.wd)), true)
		}
	}
	if err := e.enter(); err != nil {
		return err
	}
	defer e.p.mu.Unlock()
	if e.ownClosed() {
		return poll.ErrFileClosing
	}
	if rd {
		e.rd = t
	}
	if wd {
		e.wd = t
	}
	e.p.bump() // wake blocked ops to re-arm against the new deadline
	return nil
}

// closeFile closes this end. Unlike every other operation it is NOT fenced
// outside the pipe's run: Close must always work (defers, finalizers — the
// File finalizer runs it, possibly outside any bubble). It only flips state
// under the mutex; the channel broadcast is skipped outside the run, where
// no waiter can exist (the run's goroutines are gone with its bubble).
func (e *dstPipeEnd) closeFile() error {
	p := e.p
	pipeID := uintptr(unsafe.Pointer(p))
	if dstFSActive() && dstInSimBubble() {
		dstCoarseDep(pipeID, true)
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.closeSync)), true)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.ownClosed() {
		return poll.ErrFileClosing
	}
	if e.reader {
		p.rclosed = true
	} else {
		p.wclosed = true
	}
	if dstFSActive() && p.epoch == dstFSEpoch() {
		dstKernelRaceReleaseMerge(unsafe.Pointer(&e.closeSync))
		if e.reader {
			dstKernelRaceReleaseMerge(unsafe.Pointer(&p.epipeSync))
		} else {
			dstKernelRaceReleaseMerge(unsafe.Pointer(&p.eofSync))
		}
		p.bump() // readers see EOF, writers EPIPE, blocked ops on this end ErrClosed
	}
	return nil
}
