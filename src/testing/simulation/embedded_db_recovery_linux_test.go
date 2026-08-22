// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// End-to-end compatibility harness for an embedded database's real recovery
// surface — the mmap-backed, flock-coordinated, fdatasync-barriered shape a
// single-file embedded store performs. It is a harness rather than a vendored
// dependency — std cannot import a module — and it is faithful where fidelity
// matters: every syscall below is issued exactly as such a database issues it,
// including the ones that arrive through golang.org/x/sys/unix rather than the
// named syscall wrappers. x/sys's assembly enters the generic trampolines
// directly, so `unix.Fdatasync(fd)` is `syscall.Syscall(SYS_FDATASYNC, fd, 0, 0)`
// — a different code path through the interception boundary than
// `syscall.Fdatasync(fd)`, so the interception must cover both entries.
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
	dbHeader           = "EMDB"
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

// TestDSTEmbeddedDBRecovery walks the database's whole surface under one
// simulation: open, lock, map, barrier, cross-process lock-slot coordination,
// liveness recovery, virtual clocks — then a process crash and a host crash,
// each recovered the way the database recovers.
func TestDSTEmbeddedDBRecovery(t *testing.T) {
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

			// A real database writer never starts within its machine's first
			// 10ms tick: stagger it so its /proc starttime — host uptime in
			// USER_HZ ticks at process start — is a realistic nonzero value.
			sleepMillis(20)
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
	if writerStart != 2 {
		t.Fatalf("holder start time = %d, want 2 (20ms of host uptime in USER_HZ ticks at start)", writerStart)
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

// TestDSTEmbeddedDBPagerLifecycle is the pager's mmap strategy end to end, the
// shape the database's pager performs: a read-only reservation of the
// maximum size over a short file, growth in fixed extents under the live
// reservation (readable with no remap), per-commit fdatasync then a shrink
// truncate toward the high-water mark, a reader that respects the HWM published
// through the coordination slot — and the negative, a reader that does not,
// whose process dies alone. Then a TORN host crash with the reservation live:
// the durable prefix survives byte-for-byte, the unsynced tail is at the
// tear's mercy, and the rebooted process's fresh mapping reads the restored
// page cache (the same pages read(2) sees, which is what rules out a restore
// that moved the bytes out of the page cache).
func TestDSTEmbeddedDBPagerLifecycle(t *testing.T) {
	const (
		page        = 4096
		maxSize     = 64 * page // the reservation
		growStep    = 4 * page
		commits     = 6
		durablePage = 0 // the header page: fdatasync'd, so the torn crash must preserve it
	)
	var (
		rogueDied            bool
		writerAlive          error
		grownVisible         bool
		shrunkTo             int64
		rebootMappedDurable  []byte
		rebootMappedUnsynced byte
	)
	TestWith(t, 1, Options{CrashTear: true}, func(t *testing.T) {
		Host("db", HostConfig{}, func() {
			f, err := os.OpenFile("/data", os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				t.Fatalf("create data file: %v", err)
			}
			// The minimum size: two pages, header durable.
			f.Write(make([]byte, 2*page))
			f.WriteAt([]byte("HDRv1"), 0)
			rawFdatasync(f.Fd())
			// The data barrier covers the BYTES; the directory fsync makes the
			// NAMES durable — omit it and the torn reboot rightly finds no file.
			syncDir(t, "/")

			// The coordination slot (the lock-file header): HWM in pages.
			lf, err := os.OpenFile("/lock", os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				t.Fatalf("create lock file: %v", err)
			}
			lf.Write(make([]byte, 8))
			slot, err := syscall.Mmap(int(lf.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
			if err != nil {
				t.Fatalf("lock mmap: %v", err)
			}
			hwm := (*uint32)(unsafe.Pointer(&slot[0]))
			atomic.StoreUint32(hwm, 2)

			// The read-only reservation over the two-page file.
			ro, err := syscall.Mmap(int(f.Fd()), 0, maxSize, syscall.PROT_READ, syscall.MAP_SHARED)
			if err != nil {
				t.Fatalf("reservation mmap: %v", err)
			}
			if err := rawMadvise(ro, madvPopulateRead); err != nil {
				t.Fatalf("madvise reservation: %v", err)
			}

			writerPID := make(chan int, 1)
			commitsDone := make(chan struct{})
			freesGo := make(chan struct{})
			freesDone := make(chan struct{})
			go Process("writer", func() {
				writerPID <- os.Getpid()
				g, err := os.OpenFile("/data", os.O_RDWR, 0)
				if err != nil {
					t.Errorf("writer open: %v", err)
					return
				}
				defer g.Close()
				// The single-writer flock, as the database's writer holds it.
				if err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX); err != nil {
					t.Errorf("writer flock: %v", err)
					return
				}
				// Mapping slices are process-owned capabilities: the writer maps
				// the coordination slot ITSELF, as the database's processes each
				// map their own views — never through another process's slice.
				wl, err := os.OpenFile("/lock", os.O_RDWR, 0)
				if err != nil {
					t.Errorf("writer lock open: %v", err)
					return
				}
				wslot, err := syscall.Mmap(int(wl.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				wl.Close()
				if err != nil {
					t.Errorf("writer slot mmap: %v", err)
					return
				}
				whwm := (*uint32)(unsafe.Pointer(&wslot[0]))
				extent := int64(2 * page) // file-backed pages, ahead of the HWM
				for c := 0; c < commits; c++ {
					pageNo := int64(2 + c)
					// Page allocation: grow only when the claimed page is PAST the
					// extent, aligned to the growth extent — one truncate per extent.
					if need := (pageNo + 1) * page; need > extent {
						target := (need + growStep - 1) / growStep * growStep
						if err := g.Truncate(target); err != nil {
							t.Errorf("grow to %d: %v", target, err)
							return
						}
						extent = target
					}
					content := byte('A' + c)
					buf := make([]byte, page)
					for i := range buf {
						buf[i] = content
					}
					if _, err := g.WriteAt(buf, pageNo*page); err != nil {
						t.Errorf("commit %d write: %v", c, err)
						return
					}
					if c < commits-1 { // the LAST commit stays unsynced: the tear's prey
						rawFdatasync(g.Fd())
						atomic.StoreUint32(whwm, uint32(pageNo+1))
					}
					// Shrink: truncate toward HWM under the live reservation.
					hw := int64(atomic.LoadUint32(whwm)) * page
					target := (hw + growStep - 1) / growStep * growStep
					if target < extent {
						if err := g.Truncate(target); err != nil {
							t.Errorf("shrink to %d: %v", target, err)
							return
						}
						extent = target
					}
				}
				close(commitsDone)
				// Frees: after the harness has read the grown pages, the
				// workload drops to 3 live pages and the shrink reclaims the
				// slack — the shape that makes the shrink observable (tight
				// growth alone also ends extent-aligned, at a larger size).
				<-freesGo
				atomic.StoreUint32(whwm, 3)
				target := (int64(3)*page + growStep - 1) / growStep * growStep
				if target < extent {
					if err := g.Truncate(target); err != nil {
						t.Errorf("shrink after frees: %v", err)
						return
					}
					extent = target
				}
				close(freesDone)
				select {} // hold the file and the flock until the machine dies
			})
			wpid := <-writerPID
			<-commitsDone

			// A reader that respects the HWM sees the committed pages through
			// the reservation — including pages that did not exist at map time.
			grownVisible = true
			for p := 2; p < int(atomic.LoadUint32(hwm)); p++ {
				want := byte('A' + p - 2)
				if ro[p*page] != want || ro[(p+1)*page-1] != want {
					grownVisible = false
					t.Errorf("page %d via reservation = %q %q, want %q", p, ro[p*page], ro[(p+1)*page-1], want)
				}
			}

			// Only now may the writer free and shrink: the loop above read the
			// grown pages through the reservation, and the shrink cuts them.
			close(freesGo)
			<-freesDone

			// The rogue reader ignores the HWM and reads wholly past EOF: its
			// process dies; the writer and the harness do not.
			rogueDone := make(chan int, 1)
			go Process("rogue", func() {
				rogueDone <- os.Getpid()
				rf, err := os.Open("/data")
				if err != nil {
					t.Errorf("rogue open: %v", err)
					return
				}
				rro, err := syscall.Mmap(int(rf.Fd()), 0, maxSize, syscall.PROT_READ, syscall.MAP_SHARED)
				rf.Close()
				if err != nil {
					t.Errorf("rogue reservation: %v", err)
					return
				}
				pagerSink.Store(uint32(rro[maxSize-page])) // SIGBUS: page far past EOF
				t.Errorf("rogue read past the reservation's backing did not fault")
			})
			rpid := <-rogueDone
			for range 100 {
				runtime.Gosched()
			}
			rogueDied = syscall.Kill(rpid, 0) == syscall.ESRCH
			writerAlive = syscall.Kill(wpid, 0)

			if fi, err := os.Stat("/data"); err == nil {
				shrunkTo = fi.Size()
			}
		})

		CrashHost("db") // power loss, TORN: page-granular wreckage of the unsynced tail

		Host("db", HostConfig{}, func() {
			Process("recovery", func() {
				g, err := os.OpenFile("/data", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("reboot open: %v", err)
				}
				defer g.Close()
				m, err := syscall.Mmap(int(g.Fd()), 0, maxSize, syscall.PROT_READ, syscall.MAP_SHARED)
				if err != nil {
					t.Fatalf("reboot reservation: %v", err)
				}
				// Read the DURABLE page through the fresh mapping: the restore
				// must have landed in the page cache itself, or read(2) and
				// the mapping would disagree.
				rebootMappedDurable = append([]byte(nil), m[durablePage*page:durablePage*page+8]...)
				rd, err := os.ReadFile("/data")
				if err != nil {
					t.Fatalf("reboot read: %v", err)
				}
				// The durable prefix survives byte-for-byte: every SYNCED commit
				// ('A'..'E' on pages 2..6) reads back through the fresh
				// reservation, and the restored length is one of the two sizes
				// the torn size-draw allows.
				// The size draw decides whether the unsynced shrink's length
				// change landed: the durable image was 8 pages (the last
				// fdatasync), the current file 4. Synced pages beyond a landed
				// truncate are genuinely lost — an unsynced ftruncate frees
				// their blocks — so the byte-for-byte claim is bounded by the
				// restored length.
				if int64(len(rd)) != 4*page && int64(len(rd)) != 8*page {
					t.Errorf("restored length = %d, want 4 or 8 pages", len(rd))
				}
				for p := int64(2); p <= 6 && (p+1)*page <= int64(len(rd)); p++ {
					want := byte('A' + p - 2)
					if m[p*page] != want || m[(p+1)*page-1] != want {
						t.Errorf("durable page %d after torn reboot = %q %q, want %q", p, m[p*page], m[(p+1)*page-1], want)
					}
				}
				// One set of bytes: read(2) and the fresh mapping must agree on
				// EVERY byte of the restored file — a restore that moved the
				// bytes out of the page cache would leave the memfd holding the
				// pre-crash image while read(2) serves the restored one.
				for i := range rd {
					if rd[i] != m[i] {
						t.Errorf("read(2) and the mapping disagree at byte %d: %d vs %d", i, rd[i], m[i])
						break
					}
				}
				// And write-through must hold POST-restore, whatever the tear
				// drew: a write(2) after reboot is visible through the mapping.
				if _, err := g.WriteAt([]byte{0xEE}, int64(durablePage*page+9)); err != nil {
					t.Fatalf("post-reboot write: %v", err)
				}
				if m[durablePage*page+9] != 0xEE {
					t.Errorf("post-reboot write invisible through the mapping: the restore detached the page cache")
				}
				// The last commit was never synced: its page holds either the
				// durable image's bytes (zeros — it did not exist durably) or,
				// page-granularly, the torn survivors. Never a mix within one
				// byte's identity, and never a fault below the restored size.
				lastPage := 2 + commits - 1
				if int64(len(rd)) > int64(lastPage)*page {
					rebootMappedUnsynced = m[lastPage*page]
					if got := rebootMappedUnsynced; got != 0 && got != byte('A'+commits-1) {
						t.Errorf("torn unsynced page reads %q, want zeros or its own content", got)
					}
				}
				syscall.Munmap(m)
			})
		})
	})
	if !rogueDied {
		t.Errorf("the rogue reader survived reading past the reservation's backing")
	}
	if writerAlive != nil {
		t.Errorf("the writer did not survive the rogue's death: Kill = %v", writerAlive)
	}
	if !grownVisible {
		t.Errorf("committed pages were not visible through the pre-growth reservation")
	}
	// Exact, not merely extent-aligned: without the shrink the file ends at
	// 6 extents of growth; with it, alignUp(final HWM of 7 pages) = 8 pages.
	if shrunkTo != 4*page {
		t.Errorf("shrink left size %d, want %d (alignUp of the post-frees HWM)", shrunkTo, 4*page)
	}
	if string(rebootMappedDurable) != "HDRv1\x00\x00\x00" {
		t.Errorf("durable page after torn reboot = %q, want the fdatasync'd header", rebootMappedDurable)
	}
}

// pagerSink keeps the rogue's load from being optimized away: an elided load
// is an elided fault.
var pagerSink atomic.Uint32

// TestDSTEmbeddedDBNotifyFutex — the notification-region idiom exactly as the
// database issues it: a waiter parks in bounded shared-futex slices
// (FUTEX_WAIT, 100ms relative timespec, no PRIVATE flag) on a version word
// in the MAP_SHARED lock file, re-checking value > from after every wake;
// the writer bumps the word through its own mapping and issues a wake-all.
// Virtual-clock slices cost no wall time; the store+wake is never lost.
func TestDSTEmbeddedDBNotifyFutex(t *testing.T) {
	if testing.Short() {
		t.Skip("dst simulation test")
	}
	Run(11, func() {
		Host("db", HostConfig{}, func() {
			mkWord := func() *uint32 {
				lf, err := os.OpenFile("/db.lock", os.O_RDWR|os.O_CREATE, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer lf.Close()
				if err := lf.Truncate(8); err != nil {
					t.Fatal(err)
				}
				m, err := syscall.Mmap(int(lf.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Fatal(err)
				}
				return (*uint32)(unsafe.Pointer(&m[0]))
			}
			futex := func(w *uint32, op int, val uint32, ts *syscall.Timespec) (uintptr, syscall.Errno) {
				r1, _, e := syscall.Syscall6(syscall.SYS_FUTEX,
					uintptr(unsafe.Pointer(w)), uintptr(op), uintptr(val),
					uintptr(unsafe.Pointer(ts)), 0, 0)
				return r1, e
			}
			observed := make(chan uint32, 1)
			go Process("wait-version", func() {
				w := mkWord()
				from := atomic.LoadUint32(w)
				for {
					cur := atomic.LoadUint32(w)
					if cur > from {
						observed <- cur
						return
					}
					ts := syscall.NsecToTimespec((100 * time.Millisecond).Nanoseconds())
					if _, e := futex(w, 0, cur, &ts); e != 0 && e != syscall.EAGAIN && e != syscall.ETIMEDOUT {
						t.Errorf("FUTEX_WAIT slice: %v", e)
						return
					}
				}
			})
			Process("commit", func() {
				w := mkWord()
				time.Sleep(30 * time.Millisecond) // waiter parks mid-slice
				atomic.AddUint32(w, 1)
				if _, e := futex(w, 1, 1<<30, nil); e != 0 {
					t.Errorf("FUTEX_WAKE: %v", e)
				}
			})
			if got := <-observed; got != 1 {
				t.Fatalf("waiter observed version %d, want 1", got)
			}
		})
	})
}

// TestDSTEmbeddedDBBootEpoch walks the database's cross-boot epoch-invalidation
// pattern end-to-end: a writer stamps the boot epoch
// (/proc/sys/kernel/random/boot_id) and a CLOCK_BOOTTIME heartbeat into shared
// state; the host loses power and reboots; the successor reads a DIFFERENT
// boot_id (its epoch reset fires) while the dead writer's heartbeat stamp
// reads as the new boot's FUTURE — under the database's future-stamps-are-
// fresh guard that heartbeat never ages out, which is exactly why the boot
// epoch, not the heartbeat, must recover cross-boot staleness.
func TestDSTEmbeddedDBBootEpoch(t *testing.T) {
	var epoch1, epoch2 string
	var heartbeat1, now2 int64

	Run(11, func() {
		Host("db", HostConfig{}, func() {
			Process("writer", func() {
				b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
				if err != nil {
					t.Fatalf("boot_id: %v", err)
				}
				epoch1 = string(b)
				sleepMillis(50)
				hb, err := rawClockGettime(clockBoottime)
				if err != nil {
					t.Fatalf("CLOCK_BOOTTIME: %v", err)
				}
				heartbeat1 = hb
			})
		})
		CrashHost("db")
		sleepMillis(10)
		Host("db", HostConfig{}, func() {
			Process("writer", func() {
				b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
				if err != nil {
					t.Fatalf("boot_id after reboot: %v", err)
				}
				epoch2 = string(b)
				n, err := rawClockGettime(clockBoottime)
				if err != nil {
					t.Fatalf("CLOCK_BOOTTIME after reboot: %v", err)
				}
				now2 = n
			})
		})
	})

	if epoch1 == "" || epoch2 == "" || epoch1 == epoch2 {
		t.Fatalf("boot epochs %q / %q, want distinct non-empty (the cross-boot reset must fire)", epoch1, epoch2)
	}
	if heartbeat1 <= now2 {
		t.Fatalf("pre-crash heartbeat %d <= post-reboot now %d, want the stamp in the new boot's future (heartbeat aging cannot recover cross-boot staleness)", heartbeat1, now2)
	}
}

// TestDSTEmbeddedDBIdentityDivergence walks the database's two remaining
// stale-writer classification legs end-to-end, with the exact probes the
// database uses (kill(pid,0), /proc/<pid>/stat field 22, /proc/self/ns/pid).
//
// Reuse leg: a writer stamps (pid, starttime) and dies; churn under
// Options.PidMax hands its pid to an UNRELATED process. The recovering peer's
// probe finds the pid ALIVE — the trap pid-liveness alone falls into — and the
// starttime mismatch is what classifies the stamped record stale.
//
// Cross-namespace leg: a LIVE writer in a sibling pid namespace is invisible
// to the prober — kill answers ESRCH for a process that is alive and holding
// state. Namespace-inode comparison is what tells the database the probe is
// meaningless there, routing classification to the heartbeat instead.
func TestDSTEmbeddedDBIdentityDivergence(t *testing.T) {
	var stampedPid, reusedPid int
	var stampedStart, reusedStart, probedStart uint64
	var probeAlive bool
	RunWith(41, Options{PidMax: 5}, func() {
		// One shared machine: the reuse hazard is an intra-machine phenomenon
		// (pids are compared through one host's shared lock file), and
		// starttimes are host-uptime-relative — a dedicated implicit host per
		// process would start every process at its own machine's boot.
		Host("db-host", HostConfig{}, func() {
			runIdentityReuseLeg(t, &stampedPid, &reusedPid, &stampedStart, &reusedStart, &probedStart, &probeAlive)
		})
	})
	if stampedPid != 2 || reusedPid != stampedPid {
		t.Fatalf("pids stamped/reused = %d/%d, want the same pid 2 (the reuse)", stampedPid, reusedPid)
	}
	if !probeAlive {
		t.Fatalf("kill(stamped pid, 0) reported dead, want alive (the impostor holds it — liveness alone misclassifies)")
	}
	if probedStart != reusedStart || probedStart == stampedStart {
		t.Fatalf("starttimes stamped/probed/impostor = %d/%d/%d: the probe must read the impostor's, differing from the stamp (the discriminator)", stampedStart, probedStart, reusedStart)
	}

	var writerPid int
	var writerNS, proberNS string
	var crossKill error
	var writerStillLive bool
	Run(42, func() {
		hold := make(chan struct{})
		ready := make(chan struct{})
		go ProcessWith("ns-writer", ProcessConfig{PIDNamespace: "pod-a"}, func() {
			writerPid = os.Getpid()
			if l, err := os.Readlink("/proc/self/ns/pid"); err == nil {
				writerNS = l
			}
			close(ready)
			<-hold
		})
		<-ready
		Process("prober", func() {
			if l, err := os.Readlink("/proc/self/ns/pid"); err == nil {
				proberNS = l
			}
			crossKill = syscall.Kill(writerPid, 0)
		})
		writerStillLive = dstPidAliveSim(int32(writerPid))
		close(hold)
	})
	if writerNS == "" || proberNS == "" || writerNS == proberNS {
		t.Fatalf("namespaces writer/prober = %q/%q, want distinct (the inode comparison that gates the probe)", writerNS, proberNS)
	}
	if !errors.Is(crossKill, syscall.ESRCH) || !writerStillLive {
		t.Fatalf("cross-namespace kill = %v (writer live %v), want ESRCH against a LIVE holder — the hazard the heartbeat path exists for", crossKill, writerStillLive)
	}
}

// runIdentityReuseLeg is TestDSTEmbeddedDBIdentityDivergence's reuse walk, on
// the caller's (shared) host: writer stamps and dies, churn wraps the pid space,
// the impostor lands on the writer's pid, the probe classifies.
func runIdentityReuseLeg(t *testing.T, stampedPid, reusedPid *int, stampedStart, reusedStart, probedStart *uint64, probeAlive *bool) {
	t.Helper()
	sleepMillis(20)
	Process("writer", func() {
		*stampedPid = os.Getpid()
		st, err := processStartTime(*stampedPid)
		if err != nil {
			t.Errorf("writer starttime: %v", err)
		}
		*stampedStart = st
	})
	// The writer is dead; its (pid, starttime) record is the stale stamp.
	sleepMillis(30)
	for _, n := range []string{"c1", "c2", "c3"} {
		Process(n, func() {})
	}
	hold := make(chan struct{})
	ready := make(chan struct{})
	go Process("impostor", func() {
		*reusedPid = os.Getpid()
		st, err := processStartTime(*reusedPid)
		if err != nil {
			t.Errorf("impostor starttime: %v", err)
		}
		*reusedStart = st
		close(ready)
		<-hold
	})
	<-ready
	// The database's same-namespace classification, verbatim: alive? then
	// compare starttimes.
	*probeAlive = processAlive(*stampedPid)
	st, err := processStartTime(*stampedPid)
	if err != nil {
		t.Errorf("probe starttime: %v", err)
	}
	*probedStart = st
	close(hold)
}
