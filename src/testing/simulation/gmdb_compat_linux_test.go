// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// End-to-end compatibility harness for an embedded database's real recovery
// surface (github.com/thegrumpylion/gmdb). It is a harness rather than a vendored
// dependency — std cannot import a module — and it is faithful where fidelity
// matters: every syscall below is issued exactly as the database issues it,
// including the ones that arrive through golang.org/x/sys/unix rather than the
// named syscall wrappers. x/sys's assembly enters the generic trampolines
// directly, so `unix.Fdatasync(fd)` is `syscall.Syscall(SYS_FDATASYNC, fd, 0, 0)`
// — a different code path through the interception boundary than
// `syscall.Fdatasync(fd)`, and the one that used to meet the fence.
//
// Covered, in the order the database performs them: open/create with the
// directory barrier, single-writer flock, read-only data mmap with page-cache
// advice, the fdatasync barrier, shared lock-file mmap coordination across
// processes, pid-liveness recovery (Kill(pid,0) + /proc/<pid>/stat starttime +
// the pid namespace), virtual monotonic and boottime clocks, and recovery after
// both a process crash (page cache survives) and a host crash (durable image).

func sleepMillis(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

// rawFdatasync issues fdatasync the way golang.org/x/sys/unix does.
func rawFdatasync(fd uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_FDATASYNC, fd, 0, 0); e != 0 {
		return e
	}
	return nil
}

// rawMadvise issues madvise the way golang.org/x/sys/unix does.
func rawMadvise(b []byte, advice int) error {
	if _, _, e := syscall.Syscall(syscall.SYS_MADVISE, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(advice)); e != 0 {
		return e
	}
	return nil
}

// rawClockGettime issues clock_gettime the way golang.org/x/sys/unix does.
func rawClockGettime(clockid int) (int64, error) {
	var ts syscall.Timespec
	if _, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, uintptr(clockid), uintptr(unsafe.Pointer(&ts)), 0); e != 0 {
		return 0, e
	}
	// Timespec's fields are int32 on 32-bit arches and int64 on 64-bit ones.
	return int64(ts.Sec)*1_000_000_000 + int64(ts.Nsec), nil
}

const (
	clockMonotonic     = 1
	clockBoottime      = 7
	madvPopulateRead   = 22
	lockSlotAcquired   = 1
	dbHeader           = "GMDB"
	unsyncedTailMarker = "TAIL"
)

// parseStartTime is the database's own parser: /proc/<pid>/stat's comm field is
// parens-wrapped and may contain ')', so the canonical parse splits on the LAST
// ')' and takes the 20th field after it (index 19).
func parseStartTime(stat string) (uint64, error) {
	rparen := strings.LastIndex(stat, ")")
	if rparen < 0 || rparen+1 >= len(stat) {
		return 0, errors.New("malformed /proc stat: no ')' or no fields after it")
	}
	fields := strings.Fields(stat[rparen+1:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("malformed /proc stat: %d fields after ')'", len(fields))
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

func processStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	return parseStartTime(string(data))
}

// processAlive is the database's liveness probe.
func processAlive(pid int) bool { return syscall.Kill(pid, syscall.Signal(0)) == nil }

// openDatabase performs the database's open path: create-or-open the data file,
// make its NAME durable with a directory fsync, take the single-writer lock, and
// map the data read-only with the page-cache advice the pager wants.
func openDatabase(t *testing.T, dir string) (data *os.File, mapped []byte, lockFile *os.File, slot []byte) {
	t.Helper()
	dataPath := dir + "/data"
	f, err := os.OpenFile(dataPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open data: %v", err)
	}
	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		if _, err := f.Write([]byte(dbHeader)); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if err := rawFdatasync(f.Fd()); err != nil { // the data barrier
			t.Fatalf("raw fdatasync: %v", err)
		}
		d, err := os.Open(dir)
		if err != nil {
			t.Fatalf("open dir: %v", err)
		}
		if err := d.Sync(); err != nil { // the entry barrier
			t.Fatalf("dir fsync: %v", err)
		}
		d.Close()
	}
	// Single-writer lock on the data file.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	// Read-only data mapping, with the pager's advice (issued the x/sys way).
	b, err := syscall.Mmap(int(f.Fd()), 0, len(dbHeader), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("data mmap: %v", err)
	}
	if err := rawMadvise(b, madvPopulateRead); err != nil {
		t.Fatalf("raw madvise: %v", err)
	}
	// Shared lock file: a writable MAP_SHARED slot other processes CAS against.
	lf, err := os.OpenFile(dir+"/lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if fi, _ := lf.Stat(); fi.Size() == 0 {
		lf.Write(make([]byte, 8))
		rawFdatasync(lf.Fd())
	}
	s, err := syscall.Mmap(int(lf.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("lock mmap: %v", err)
	}
	return f, b, lf, s
}

// TestDSTGMDBCompat walks the database's whole surface under one simulation:
// open, lock, map, barrier, cross-process lock-slot coordination, liveness
// recovery, virtual clocks — then a process crash and a host crash, each
// recovered the way the database recovers.
func TestDSTGMDBCompat(t *testing.T) {
	var (
		writerPID              int
		writerStart            uint64
		nsLink                 string
		monoDelta, bootDelta   int64
		contenderLockErr       error
		slotSeenByContender    uint32
		staleAfterCrash        bool
		startTimeAfterCrash    error
		restartReadsUnsynced   string
		rebootReadsDurableOnly string
	)
	Test(t, 1, func(t *testing.T) {
		Host("db-host", HostConfig{}, func() {
			if err := os.Mkdir("/db", 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			d, _ := os.Open("/")
			d.Sync()
			d.Close()

			opened := make(chan struct{})
			holdOpen := make(chan struct{})
			writerReady := make(chan struct{})

			go Process("writer", func() {
				writerPID = os.Getpid()
				data, mapped, lockFile, slot := openDatabase(t, "/db")

				// The mapped header is the file's durable bytes.
				if string(mapped) != dbHeader {
					t.Errorf("mapped header = %q, want %q", mapped, dbHeader)
				}
				// Claim the shared lock slot with an atomic CAS, as the
				// cross-process lock protocol does.
				if !atomic.CompareAndSwapUint32((*uint32)(unsafe.Pointer(&slot[0])), 0, lockSlotAcquired) {
					t.Errorf("lock slot already claimed")
				}
				// Publish identity for a recovering peer: pid + start time.
				st, err := processStartTime(writerPID)
				if err != nil {
					t.Errorf("start time: %v", err)
				}
				writerStart = st
				if l, err := os.Readlink("/proc/self/ns/pid"); err == nil {
					nsLink = l
				} else {
					t.Errorf("readlink ns: %v", err)
				}

				// Virtual clocks, read the x/sys way.
				m0, err := rawClockGettime(clockMonotonic)
				if err != nil {
					t.Errorf("raw clock_gettime MONOTONIC: %v", err)
				}
				b0, err := rawClockGettime(clockBoottime)
				if err != nil {
					t.Errorf("raw clock_gettime BOOTTIME: %v", err)
				}
				sleepMillis(100)
				m1, _ := rawClockGettime(clockMonotonic)
				b1, _ := rawClockGettime(clockBoottime)
				monoDelta, bootDelta = m1-m0, b1-b0

				// An UNSYNCED tail write: it survives a process crash (the
				// kernel's page cache does) and dies with the machine.
				if _, err := data.WriteAt([]byte(unsyncedTailMarker), int64(len(dbHeader))); err != nil {
					t.Errorf("tail write: %v", err)
				}
				close(writerReady)
				close(opened)
				<-holdOpen
				_ = lockFile
				_ = mapped
			})
			<-opened

			// A second process finds the database locked and the slot claimed,
			// and probes the holder's liveness exactly as recovery does.
			Process("contender", func() {
				<-writerReady
				f, err := os.OpenFile("/db/data", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("contender open: %v", err)
				}
				defer f.Close()
				contenderLockErr = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)

				lf, err := os.OpenFile("/db/lock", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("contender lock open: %v", err)
				}
				defer lf.Close()
				s, err := syscall.Mmap(int(lf.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Fatalf("contender lock mmap: %v", err)
				}
				defer syscall.Munmap(s)
				slotSeenByContender = atomic.LoadUint32((*uint32)(unsafe.Pointer(&s[0])))

				// The holder is alive: its pid answers, and its start time
				// matches what it published (the stale-holder discriminator).
				if !processAlive(writerPID) {
					t.Errorf("holder pid %d reported dead while it holds the lock", writerPID)
				}
				st, err := processStartTime(writerPID)
				if err != nil || st != writerStart {
					t.Errorf("holder start time = %d, %v; want %d", st, err, writerStart)
				}
			})

			// The writer's machine keeps running; the writer dies. Its lock and
			// its slot are released by the kernel, its unsynced tail survives.
			Crash("writer")
			staleAfterCrash = !processAlive(writerPID)
			_, startTimeAfterCrash = processStartTime(writerPID)

			Process("recovery", func() {
				f, err := os.OpenFile("/db/data", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("recovery open: %v", err)
				}
				defer f.Close()
				// The dead holder's lock is gone.
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatalf("recovery flock after holder crash: %v", err)
				}
				b := make([]byte, len(dbHeader)+len(unsyncedTailMarker))
				n, _ := f.ReadAt(b, 0)
				restartReadsUnsynced = string(b[:n])
			})
			close(holdOpen)
		})

		// Power loss: the durable image is the header (fdatasync'd), never the
		// tail (page cache only).
		CrashHost("db-host")

		Host("db-host", HostConfig{}, func() {
			Process("reboot", func() {
				b, err := os.ReadFile("/db/data")
				if err != nil {
					t.Fatalf("reboot read: %v", err)
				}
				rebootReadsDurableOnly = string(b)
			})
		})
	})

	if !errors.Is(contenderLockErr, syscall.EWOULDBLOCK) {
		t.Fatalf("second writer's flock = %v, want EWOULDBLOCK (single-writer)", contenderLockErr)
	}
	if slotSeenByContender != lockSlotAcquired {
		t.Fatalf("lock slot seen across processes = %d, want %d (shared mmap coordination)", slotSeenByContender, lockSlotAcquired)
	}
	if writerStart == 0 {
		t.Fatalf("holder start time was 0: /proc/<pid>/stat field 22 not usable")
	}
	if nsLink != "pid:[1]" {
		t.Fatalf("pid namespace = %q, want pid:[1]", nsLink)
	}
	if monoDelta != 100_000_000 || bootDelta != 100_000_000 {
		t.Fatalf("virtual clocks advanced mono=%dns boot=%dns, want 100ms each", monoDelta, bootDelta)
	}
	if !staleAfterCrash {
		t.Fatalf("crashed holder still reports alive: recovery would spin forever")
	}
	if !errors.Is(startTimeAfterCrash, os.ErrNotExist) {
		t.Fatalf("crashed holder's /proc entry = %v, want not-exist", startTimeAfterCrash)
	}
	if restartReadsUnsynced != dbHeader+unsyncedTailMarker {
		t.Fatalf("after a PROCESS crash the file reads %q, want the unsynced tail intact (the kernel survived)", restartReadsUnsynced)
	}
	if rebootReadsDurableOnly != dbHeader {
		t.Fatalf("after a HOST crash the file reads %q, want only the durable header", rebootReadsDurableOnly)
	}
}
