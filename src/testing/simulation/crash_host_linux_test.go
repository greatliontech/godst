// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// syncDir fsyncs the directory holding name, making the ENTRY durable (POSIX:
// data durability and name durability are separate).
func syncDir(t *testing.T, dir string) {
	t.Helper()
	d, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir %s: %v", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		t.Fatalf("sync dir %s: %v", dir, err)
	}
}

// TestDSTCrashHostRestoresDurableImage: power loss restores exactly what was
// committed to the disk, and nothing else. The POSIX two-part shape is pinned
// head-on: fsync(file)+fsync(dir) survives; fsync(file) alone loses the NAME;
// neither loses everything; and bytes written after the last fsync revert to
// the synced content.
func TestDSTCrashHostRestoresDurableImage(t *testing.T) {
	var bothErr, dataOnlyErr, neitherErr error
	var bothData, revertData string
	Test(t, 1, func(t *testing.T) {
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				// (1) data + entry durable.
				f, err := os.Create("/both")
				if err != nil {
					t.Errorf("create /both: %v", err)
					return
				}
				f.Write([]byte("durable"))
				f.Sync()
				f.Close()
				syncDir(t, "/")

				// (2) data durable, entry NOT (no dir fsync after create).
				g, err := os.Create("/dataonly")
				if err != nil {
					t.Errorf("create /dataonly: %v", err)
					return
				}
				g.Write([]byte("bytes"))
				g.Sync()
				g.Close()

				// (3) neither.
				h, err := os.Create("/neither")
				if err != nil {
					t.Errorf("create /neither: %v", err)
					return
				}
				h.Write([]byte("gone"))
				h.Close()

				// (4) post-sync overwrite of a durable file: reverts.
				r, err := os.OpenFile("/both", os.O_RDWR, 0)
				if err != nil {
					t.Errorf("reopen /both: %v", err)
					return
				}
				r.WriteAt([]byte("XXXXXXX"), 0)
				r.Close()
				select {} // stay alive until the machine dies
			})
			for range 30 {
				runtime.Gosched()
			}
		})

		CrashHost("h")

		// Reboot the machine and read its recovered disk.
		Host("h", HostConfig{}, func() {
			Process("db", func() {
				b, err := os.ReadFile("/both")
				bothErr, bothData = err, string(b)
				_, dataOnlyErr = os.Stat("/dataonly")
				_, neitherErr = os.Stat("/neither")
				rb, _ := os.ReadFile("/both")
				revertData = string(rb)
			})
		})
	})
	if bothErr != nil || bothData != "durable" {
		t.Fatalf("fsync(file)+fsync(dir) after reboot = %q, %v; want %q, nil", bothData, bothErr, "durable")
	}
	if !errors.Is(dataOnlyErr, os.ErrNotExist) {
		t.Fatalf("fsync(file) without fsync(dir) after reboot = %v, want not-exist (the NAME was never durable)", dataOnlyErr)
	}
	if !errors.Is(neitherErr, os.ErrNotExist) {
		t.Fatalf("unsynced file after reboot = %v, want not-exist", neitherErr)
	}
	if revertData != "durable" {
		t.Fatalf("post-sync overwrite after reboot = %q, want %q (unsynced bytes are lost)", revertData, "durable")
	}
}

// TestDSTCrashHostPreservesMkfsImage: the initial tree a run boots with
// (root and /tmp) is durable from birth — the mkfs image is on the platter,
// so a host crash preserves /tmp, and fsync-disciplined state under it
// (fsync(file) then fsync("/tmp"), the full POSIX discipline, with NO fsync
// of "/") survives byte-exactly. A tree born unsynced fails this: the crash
// rebuilds root from an empty durable set, "tmp" vanishes, and the read
// fails ENOENT.
func TestDSTCrashHostPreservesMkfsImage(t *testing.T) {
	for _, tear := range []bool{false, true} {
		t.Run(map[bool]string{false: "untorn", true: "torn"}[tear], func(t *testing.T) {
			testCrashHostPreservesMkfsImage(t, tear)
		})
	}
}

// The torn variant pins that a page-granular tear cannot drop the mkfs
// image either: a durable, unchanged entry is kept deterministically, never
// coin-flipped.
func testCrashHostPreservesMkfsImage(t *testing.T, tear bool) {
	var readErr error
	var readData string
	var tmpStatErr error
	TestWith(t, 1, Options{CrashTear: tear}, func(t *testing.T) {
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				f, err := os.Create("/tmp/state")
				if err != nil {
					t.Errorf("create /tmp/state: %v", err)
					return
				}
				f.Write([]byte("checkpoint"))
				f.Sync()
				f.Close()
				syncDir(t, "/tmp")
				select {} // stay alive until the machine dies
			})
			for range 30 {
				runtime.Gosched()
			}
		})

		CrashHost("h")

		Host("h", HostConfig{}, func() {
			Process("db", func() {
				_, tmpStatErr = os.Stat("/tmp")
				b, err := os.ReadFile("/tmp/state")
				readErr, readData = err, string(b)
			})
		})
	})
	if tmpStatErr != nil {
		t.Fatalf("/tmp after reboot: %v, want present (the mkfs image is durable from birth)", tmpStatErr)
	}
	if readErr != nil || readData != "checkpoint" {
		t.Fatalf("fsync-disciplined /tmp/state after reboot = %q, %v; want %q, nil", readData, readErr, "checkpoint")
	}
}

// TestDSTCrashHostSecondCrashIsNoop: a host-crash restore commits the
// restored image as the new durable image (it is, by definition, what the
// platter holds), so a second crash with ZERO intervening writes changes
// nothing — torn and untorn. Left uncommitted, a torn restore leaves synced
// at the pre-crash durable image, and the second crash re-tears live-vs-stale
// and redraws pages: bytes on the platter revert with nothing having written,
// a state no real crash ordering can produce.
func TestDSTCrashHostSecondCrashIsNoop(t *testing.T) {
	for _, tear := range []bool{false, true} {
		t.Run(map[bool]string{false: "untorn", true: "torn"}[tear], func(t *testing.T) {
			testCrashHostSecondCrashIsNoop(t, tear)
		})
	}
}

func testCrashHostSecondCrashIsNoop(t *testing.T, tear bool) {
	var first, second string
	var names1, names2 []string
	boot := func(out *string, outNames *[]string) {
		Host("h", HostConfig{}, func() {
			Process("db", func() {
				b, err := os.ReadFile("/tmp/state")
				if err != nil {
					b = []byte("read error: " + err.Error())
				}
				*out = string(b)
				ents, err := os.ReadDir("/tmp")
				if err != nil {
					*outNames = append(*outNames, "readdir error: "+err.Error())
					return
				}
				for _, e := range ents {
					*outNames = append(*outNames, e.Name())
				}
			})
		})
	}
	TestWith(t, 2, Options{CrashTear: tear}, func(t *testing.T) {
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				f, err := os.Create("/tmp/state")
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				durable := make([]byte, 8<<10)
				for i := range durable {
					durable[i] = 'd'
				}
				f.Write(durable)
				f.Sync()
				syncDir(t, "/tmp")
				// Overwrite unsynced, and land an unsynced entry, so a torn
				// first crash has live-vs-durable divergence to tear along.
				unsynced := make([]byte, 8<<10)
				for i := range unsynced {
					unsynced[i] = 'u'
				}
				f.WriteAt(unsynced, 0)
				f.Close()
				os.WriteFile("/tmp/landed", []byte("x"), 0o644)
				select {} // stay alive until the machine dies
			})
			for range 30 {
				runtime.Gosched()
			}
		})

		CrashHost("h")
		boot(&first, &names1) // read-only: no writes between the crashes
		CrashHost("h")
		boot(&second, &names2)
	})
	if first != second {
		t.Fatalf("second crash with zero intervening writes changed the file image:\nfirst=%.32q...\nsecond=%.32q...", first, second)
	}
	if !slices.Equal(names1, names2) {
		t.Fatalf("second crash with zero intervening writes changed the directory image:\nfirst=%v\nsecond=%v", names1, names2)
	}
}

// TestDSTCrashHostResurrectsUnsyncedRemoval: a removal whose parent directory
// was never fsynced is not on the disk, so power loss brings the entry back —
// and the resurrected node must be a usable directory, not a husk that still
// carries the unlinked mark (creation in it would answer ENOENT forever).
func TestDSTCrashHostResurrectsUnsyncedRemoval(t *testing.T) {
	var dirErr, createErr, fileErr error
	Test(t, 1, func(t *testing.T) {
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				if err := os.Mkdir("/d", 0o755); err != nil {
					t.Errorf("mkdir: %v", err)
					return
				}
				if err := os.WriteFile("/d/f", []byte("x"), 0o644); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				// Make BOTH the dir entry and the file entry durable.
				syncDir(t, "/")
				f, _ := os.Open("/d/f")
				f.Sync()
				f.Close()
				syncDir(t, "/d")
				// Now remove them WITHOUT syncing the parents: the removal is
				// page-cache-only and power loss undoes it.
				os.Remove("/d/f")
				os.Remove("/d")
				select {}
			})
			for range 30 {
				runtime.Gosched()
			}
		})
		CrashHost("h")
		Host("h", HostConfig{}, func() {
			Process("db", func() {
				_, dirErr = os.Stat("/d")
				_, fileErr = os.Stat("/d/f")
				// Probe through a Root: the unlinked mark is what rooted
				// creation consults (a named path cannot even reach a removed
				// directory), so this is the assertion that catches a
				// resurrected-but-still-marked husk.
				r, err := os.OpenRoot("/d")
				if err != nil {
					createErr = err
					return
				}
				defer r.Close()
				f, err := r.Create("new")
				if err != nil {
					createErr = err
					return
				}
				f.Close()
			})
		})
	})
	if dirErr != nil {
		t.Fatalf("stat resurrected dir = %v, want nil (the removal was never durable)", dirErr)
	}
	if fileErr != nil {
		t.Fatalf("stat resurrected file = %v, want nil", fileErr)
	}
	if createErr != nil {
		t.Fatalf("create in resurrected dir = %v, want nil (the unlinked mark must be cleared)", createErr)
	}
}

// TestDSTCrashHostLosesDirtyMappedBytes: the process/host split. A dirty
// writable MAP_SHARED page survives a PROCESS crash (the kernel's page cache
// outlives the process) and is LOST by a HOST crash (the page cache was never
// on the disk).
func TestDSTCrashHostLosesDirtyMappedBytes(t *testing.T) {
	var afterProcCrash, afterHostCrash byte
	var deadMachineMapping []byte
	var rebootMapping []byte
	Test(t, 1, func(t *testing.T) {
		Host("h", HostConfig{}, func() {
			// Seed a durable baseline: content 'A', data + entry synced.
			f, err := os.Create("/m")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			f.Write([]byte("A"))
			f.Sync()
			f.Close()
			syncDir(t, "/")

			mapped := make(chan struct{})
			live := make(chan struct{})
			go Process("mapper", func() {
				g, err := os.OpenFile("/m", os.O_RDWR, 0)
				if err != nil {
					t.Errorf("open: %v", err)
					return
				}
				b, err := syscall.Mmap(int(g.Fd()), 0, 1, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				g.Close()
				if err != nil {
					t.Errorf("mmap: %v", err)
					return
				}
				b[0] = 'Z' // dirty page, never msync'd/fsync'd
				close(mapped)
				select {}
			})
			<-mapped

			Crash("mapper") // process crash: write-back into the page cache
			pb, err := os.ReadFile("/m")
			if err != nil {
				t.Fatalf("read after process crash: %v", err)
			}
			afterProcCrash = pb[0]

			// A LIVE mapping at the moment the machine dies. Its slice is the
			// dead machine's memory: the crash takes the mapping with the
			// machine (touching it faults — the pages are gone), and a rebooted
			// process's fresh mapping of the same bytes must be its own, never
			// a reuse of the dead machine's address space.
			go Process("livemapper", func() {
				g, err := os.OpenFile("/m", os.O_RDWR, 0)
				if err != nil {
					t.Errorf("live open: %v", err)
					return
				}
				m, err := syscall.Mmap(int(g.Fd()), 0, 1, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				g.Close()
				if err != nil {
					t.Errorf("live mmap: %v", err)
					return
				}
				m[0] = 'Z'
				deadMachineMapping = m
				close(live)
				select {}
			})
			<-live
		})

		CrashHost("h") // power loss: the page cache goes

		Host("h", HostConfig{}, func() {
			Process("reader", func() {
				hb, err := os.ReadFile("/m")
				if err != nil {
					t.Fatalf("read after host crash: %v", err)
				}
				afterHostCrash = hb[0]

				// Map the same bytes on the rebooted machine and dirty them.
				g, err := os.OpenFile("/m", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("reboot open: %v", err)
				}
				m, err := syscall.Mmap(int(g.Fd()), 0, 1, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				g.Close()
				if err != nil {
					t.Fatalf("reboot mmap: %v", err)
				}
				m[0] = 'Q'
				rebootMapping = m
				syscall.Munmap(m)
			})
		})
	})
	if afterProcCrash != 'Z' {
		t.Fatalf("after process crash file byte = %q, want 'Z' (page cache outlives the process)", afterProcCrash)
	}
	if afterHostCrash != 'A' {
		t.Fatalf("after host crash file byte = %q, want 'A' (power loss loses the page cache)", afterHostCrash)
	}
	// The dead machine's memory is unreachable (reading deadMachineMapping
	// would fault: its pages went with the machine), so non-aliasing is pinned
	// structurally: the rebooted machine's mapping is fresh address space, not
	// a reuse of the dead machine's.
	if &rebootMapping[0] == &deadMachineMapping[0] {
		t.Fatalf("the rebooted machine's mapping reuses the dead machine's address %p", &deadMachineMapping[0])
	}
}

// TestDSTCrashHostVictimScoping: a host crash takes exactly one machine —
// DST-FAULT-VICTIM. The sibling host's disk, its unsynced bytes, its flock, and
// a connection between two OTHER hosts all survive.
func TestDSTCrashHostVictimScoping(t *testing.T) {
	var siblingData string
	var siblingLockErr error
	var victimPeerReadErr error
	var victimDialErr error
	Test(t, 1, func(t *testing.T) {
		ready := make(chan struct{})
		crashed := make(chan struct{})
		readDone := make(chan struct{})
		done := make(chan struct{})
		exited := make(chan struct{})
		addrCh := make(chan string, 1)

		Host("victim", HostConfig{}, func() {
			go Process("vproc", func() {
				l, err := net.Listen("tcp", HostIP("victim")+":0")
				if err != nil {
					t.Errorf("victim listen: %v", err)
					return
				}
				addrCh <- l.Addr().String()
				if _, err := l.Accept(); err != nil {
					t.Errorf("victim accept: %v", err)
					return
				}
				select {} // dies with the machine
			})
		})
		addr := <-addrCh

		Host("sibling", HostConfig{}, func() {
			go func() {
				defer close(exited)
				Process("sproc", func() {
					// An UNSYNCED write on the sibling's disk: it must survive
					// the victim's power loss untouched.
					if err := os.WriteFile("/s", []byte("keep"), 0o644); err != nil {
						t.Errorf("sibling write: %v", err)
						return
					}
					f, err := os.Open("/s")
					if err != nil {
						t.Errorf("sibling open: %v", err)
						return
					}
					defer f.Close()
					if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
						t.Errorf("sibling flock: %v", err)
						return
					}
					c, err := net.Dial("tcp", addr)
					if err != nil {
						t.Errorf("sibling dial: %v", err)
						return
					}
					close(ready)
					<-crashed
					// The peer's machine lost power: its sockets RST.
					_, victimPeerReadErr = c.Read(make([]byte, 1))
					close(readDone)
					<-done // hold the lock while the checker probes it
				})
			}()
			<-ready
		})

		CrashHost("victim")
		close(crashed)
		<-readDone

		// The victim's machine is off: a dial to it blackholes to the
		// retransmit horizon (only a live kernel could answer RST).
		Host("sibling", HostConfig{}, func() {
			Process("dialer", func() {
				_, victimDialErr = net.Dial("tcp", addr)
			})
		})

		Host("sibling", HostConfig{}, func() {
			Process("checker", func() {
				b, err := os.ReadFile("/s")
				if err != nil {
					t.Fatalf("sibling disk after the victim host crashed: %v", err)
				}
				siblingData = string(b)
				g, err := os.Open("/s")
				if err != nil {
					t.Fatalf("sibling reopen: %v", err)
				}
				defer g.Close()
				// sproc still holds its exclusive lock: its machine never died.
				siblingLockErr = syscall.Flock(int(g.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			})
		})
		close(done)
		<-exited
	})
	if siblingData != "keep" {
		t.Fatalf("sibling disk = %q, want %q (a host crash tears exactly one machine)", siblingData, "keep")
	}
	if !errors.Is(siblingLockErr, syscall.EWOULDBLOCK) {
		t.Fatalf("sibling flock = %v, want EWOULDBLOCK (its lock table survived)", siblingLockErr)
	}
	if !errors.Is(victimPeerReadErr, syscall.ECONNRESET) {
		t.Fatalf("peer of the crashed host = %v, want ECONNRESET", victimPeerReadErr)
	}
	if !errors.Is(victimDialErr, syscall.ETIMEDOUT) {
		t.Fatalf("dial to the crashed host = %v, want ETIMEDOUT (the machine is off — nothing answers the SYN)", victimDialErr)
	}
}

// TestDSTCrashHostClosesRootProcessResources: the case that forces host-keyed
// teardown. A Host body with no Process declaration runs as the ROOT process
// (proc 0), which every host shares — so a proc-keyed sweep would either miss
// its open files and locks (leaving the dead machine holding a lock forever)
// or, worse, close the root's files on OTHER hosts. Keyed by host, both are
// impossible. The Host body runs on its own goroutine so the driver's machine
// is not the victim.
func TestDSTCrashHostClosesRootProcessResources(t *testing.T) {
	var lockErr error
	var otherHostData string
	var leakedWriteErr error
	var leaked *os.File
	Test(t, 1, func(t *testing.T) {
		locked := make(chan struct{})
		otherReady := make(chan struct{})
		go Host("h", HostConfig{}, func() {
			// proc 0 on host h: no Process declared.
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			f, err := os.Open("/lock")
			if err != nil {
				t.Errorf("open: %v", err)
				return
			}
			f.Sync()
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
				t.Errorf("flock: %v", err)
				return
			}
			syncDir(t, "/")
			leaked = f // the dead machine's open file description
			close(locked)
			select {} // the root process's goroutine on h, killed with the machine
		})
		<-locked

		// The root process also has durable state on ANOTHER host; the victim's
		// teardown must not reach it.
		go Host("other", HostConfig{}, func() {
			if err := os.WriteFile("/o", []byte("other"), 0o644); err != nil {
				t.Errorf("other write: %v", err)
			}
			g, err := os.Open("/o")
			if err != nil {
				t.Errorf("other open: %v", err)
				return
			}
			g.Sync()
			syncDir(t, "/")
			g.Close()
			close(otherReady)
		})
		<-otherReady

		CrashHost("h")

		// The open file description died with the kernel that held it.
		_, leakedWriteErr = leaked.Write([]byte("x"))

		Host("h", HostConfig{}, func() {
			Process("db", func() {
				f, err := os.Open("/lock")
				if err != nil {
					t.Fatalf("reopen after reboot: %v", err)
				}
				defer f.Close()
				lockErr = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			})
		})
		Host("other", HostConfig{}, func() {
			Process("check", func() {
				b, err := os.ReadFile("/o")
				if err != nil {
					t.Fatalf("other host read: %v", err)
				}
				otherHostData = string(b)
			})
		})
	})
	if lockErr != nil {
		t.Fatalf("flock after reboot = %v, want success (the root process's lock on the dead machine died with it)", lockErr)
	}
	if otherHostData != "other" {
		t.Fatalf("other host data = %q, want %q (host teardown must not reach a sibling)", otherHostData, "other")
	}
	if !errors.Is(leakedWriteErr, os.ErrClosed) {
		t.Fatalf("write through a handle open on the dead machine = %v, want ErrClosed", leakedWriteErr)
	}
}

// TestDSTCrashHostKillsProcessGoroutineStampedElsewhere: a goroutine of a
// process on the victim host that is momentarily stamped with ANOTHER host (it
// entered a nested Host body) is still a thread of a process on the dying
// machine — it must die with it. Host-keyed marking alone would miss it, which
// is why the kill is the union of the host's goroutines and its processes'.
func TestDSTCrashHostKillsProcessGoroutineStampedElsewhere(t *testing.T) {
	var ranAfter atomic.Bool
	Test(t, 1, func(t *testing.T) {
		inside := make(chan struct{})
		release := make(chan struct{})
		Host("other", HostConfig{}, func() {}) // declare it, so the name resolves
		Host("victim", HostConfig{}, func() {
			go Process("vproc", func() {
				// The process lives on "victim", but this goroutine is stamped
				// with "other" for the dynamic extent of the nested Host body.
				Host("other", HostConfig{}, func() {
					close(inside)
					<-release
				})
				ranAfter.Store(true)
			})
		})
		<-inside

		CrashHost("victim")

		close(release)
		for range 20 {
			runtime.Gosched()
		}
	})
	if ranAfter.Load() {
		t.Fatalf("a thread of a process on the dead machine kept running (it was stamped with another host)")
	}
}

// TestDSTCrashHostKeepsCreationModeAndBadDisk: two properties a reboot must
// preserve. (1) Metadata durability is an INODE property: once the parent
// directory's fsync makes a file's NAME durable, power loss recovers the file
// with the mode it was created with — not mode 0 — even though the file itself
// was never fsynced. A later chmod, unsynced, reverts. (2) A disk fault is a
// property of the HARDWARE, not of the dead kernel: a bad disk stays bad across
// the reboot.
func TestDSTCrashHostKeepsCreationModeAndBadDisk(t *testing.T) {
	var mode os.FileMode
	var readErr error
	var bornAt, afterReboot time.Time
	Test(t, 1, func(t *testing.T) {
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				f, err := os.OpenFile("/f", os.O_CREATE|os.O_RDWR, 0o640)
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				f.Close()
				if fi, err := os.Stat("/f"); err == nil {
					bornAt = fi.ModTime()
				}
				syncDir(t, "/") // the NAME is durable; the file never fsynced
				if err := os.Chmod("/f", 0o777); err != nil {
					t.Errorf("chmod: %v", err)
					return
				} // unsynced metadata change: reverts
				select {}
			})
			for range 20 {
				runtime.Gosched()
			}
		})

		FailDisk("h") // the media is bad before the power goes
		CrashHost("h")

		Host("h", HostConfig{}, func() {
			Process("db", func() {
				fi, err := os.Stat("/f")
				if err != nil {
					t.Fatalf("stat after reboot: %v", err)
				}
				mode = fi.Mode()
				afterReboot = fi.ModTime()
				_, readErr = os.ReadFile("/f")
			})
		})
	})
	if mode.Perm() != 0o640 {
		t.Fatalf("mode after reboot = %v, want 0640 (the inode reached the disk with its creation mode; the unsynced chmod reverted)", mode.Perm())
	}
	if !afterReboot.Equal(bornAt) {
		t.Fatalf("modTime after reboot = %v, want the creation time %v (the inode reached the disk with its birth timestamp)", afterReboot, bornAt)
	}
	if !errors.Is(readErr, syscall.EIO) {
		t.Fatalf("read from the rebooted machine's bad disk = %v, want EIO (a bad disk stays bad; the fault is hardware, not kernel state)", readErr)
	}
}

// TestDSTProcessRefusesTwoHosts: a logical process lives on one machine at a
// time. Two live invocations of one name on different hosts would give the
// process id two homes, and a host crash would scope its victims by whichever
// was recorded last — silently sparing a pid on the machine that lost power.
// The topology is refused instead.
func TestDSTProcessRefusesTwoHosts(t *testing.T) {
	var got any
	Run(1, func() {
		started := make(chan struct{})
		release := make(chan struct{})
		go Host("a", HostConfig{}, func() {
			go Process("p", func() {
				close(started)
				<-release
			})
		})
		<-started
		func() {
			defer func() { got = recover() }()
			Host("b", HostConfig{}, func() {
				Process("p", func() {})
			})
		}()
		close(release)
	})
	msg, _ := got.(string)
	if got == nil || !strings.Contains(msg, "already live on another host") {
		t.Fatalf("same-name process on a second host = %v, want the one-machine refusal", got)
	}
}

// TestDSTCrashHostRefusesDriverMachine: crashing the machine the run's own
// goroutine runs on is refused loudly, before anything is torn down — a driver
// whose filesystem and sockets vanished under it is a state no power loss
// produces, and the alternative is an undiagnosable hang.
func TestDSTCrashHostRefusesDriverMachine(t *testing.T) {
	var inlineHost, inlineProc any
	Run(1, func() {
		func() {
			defer func() { inlineHost = recover() }()
			Host("driver", HostConfig{}, func() {
				CrashHost("driver")
			})
		}()
		func() {
			defer func() { inlineProc = recover() }()
			Host("h2", HostConfig{}, func() {
				Process("p2", func() { CrashHost("h2") })
			})
		}()
	})
	for name, got := range map[string]any{"host body": inlineHost, "process body": inlineProc} {
		msg, _ := got.(string)
		if got == nil || !strings.Contains(msg, "run's main goroutine") {
			t.Fatalf("CrashHost from an inline %s = %v, want the driver-machine refusal", name, got)
		}
	}
}

// TestDSTCrashHostRestartFreshResources: after the reboot, a restarted process
// gets a fresh pid and clean process-owned resources — the dead machine's
// flocks are gone, so it re-acquires immediately.
func TestDSTCrashHostRestartFreshResources(t *testing.T) {
	var lockErr error
	var oldPID, newPID int
	var killErr error
	var cwdAfter string
	Test(t, 1, func(t *testing.T) {
		locked := make(chan int, 1)
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				f, err := os.Open("/lock")
				if err != nil {
					t.Errorf("open: %v", err)
					return
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					t.Errorf("flock: %v", err)
					return
				}
				// Make the file durable so it survives the reboot.
				f2, _ := os.Open("/lock")
				f2.Sync()
				f2.Close()
				if err := os.Mkdir("/sub", 0o755); err == nil {
					syncDir(t, "/")
					os.Chdir("/sub") // kernel-held cwd, lost on reboot
				}
				syncDir(t, "/")
				locked <- os.Getpid()
				select {}
			})
		})
		oldPID = <-locked

		CrashHost("h")
		killErr = syscall.Kill(oldPID, 0)

		Host("h", HostConfig{}, func() {
			Process("db", func() {
				newPID = os.Getpid()
				// A rebooted machine starts its processes at the root: the cwd
				// lived in the dead kernel's task struct.
				cwdAfter, _ = os.Getwd()
				f, err := os.Open("/lock")
				if err != nil {
					t.Fatalf("reopen after reboot: %v", err)
				}
				defer f.Close()
				lockErr = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			})
		})
	})
	if !errors.Is(killErr, syscall.ESRCH) {
		t.Fatalf("Kill(pid of a process on the crashed host) = %v, want ESRCH", killErr)
	}
	if newPID == oldPID {
		t.Fatalf("restart after reboot reused pid %d", oldPID)
	}
	if lockErr != nil {
		t.Fatalf("flock after reboot = %v, want success (the dead kernel's lock table is gone)", lockErr)
	}
	if cwdAfter != "/" {
		t.Fatalf("cwd after reboot = %q, want %q (the cwd lived in the dead kernel)", cwdAfter, "/")
	}
}

// TestDSTCrashHostDropsInFlightBytes: a host crash RSTs each of its
// connections AT THE SURVIVING PEER — queued and in-flight bytes are discarded
// and the peer's next read fails ECONNRESET without draining. A real RST
// destroys the receive queue; the simulation must not deliver bytes the
// powered-off machine's teardown destroyed (DST-FAULT-SOUND).
func TestDSTCrashHostDropsInFlightBytes(t *testing.T) {
	var n int
	var readErr error
	TestWith(t, 1, Options{Network: NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}, func(t *testing.T) {
		addrCh := make(chan string, 1)
		written := make(chan struct{})
		dialed := make(chan struct{})
		crashed := make(chan struct{})
		readDone := make(chan struct{})
		exited := make(chan struct{})

		Host("victim", HostConfig{}, func() {
			go Process("writer", func() {
				l, err := net.Listen("tcp", HostIP("victim")+":0")
				if err != nil {
					t.Errorf("victim listen: %v", err)
					return
				}
				addrCh <- l.Addr().String()
				c, err := l.Accept()
				if err != nil {
					t.Errorf("victim accept: %v", err)
					return
				}
				if _, err := c.Write([]byte("abc")); err != nil {
					t.Errorf("victim write: %v", err)
					return
				}
				close(written)
				select {} // dies with the machine
			})
		})
		addr := <-addrCh

		Host("survivor", HostConfig{}, func() {
			go func() {
				defer close(exited)
				Process("reader", func() {
					c, err := net.Dial("tcp", addr)
					if err != nil {
						t.Errorf("survivor dial: %v", err)
						return
					}
					// Dial returned, so both conn ends are registered — the
					// crash below cannot benignly miss an unregistered conn on
					// any seed's schedule.
					close(dialed)
					<-crashed
					// The writer's 3 bytes are still in flight (100ms link;
					// no virtual time passed between the write and the crash).
					// The first read must fail — never deliver them.
					n, readErr = c.Read(make([]byte, 8))
					close(readDone)
				})
			}()
		})

		<-written
		<-dialed
		CrashHost("victim")
		close(crashed)
		<-readDone
		<-exited
	})
	if n != 0 || !errors.Is(readErr, syscall.ECONNRESET) {
		t.Fatalf("first read after the writer host crashed = (%d, %v), want (0, ECONNRESET): in-flight bytes must be dropped, not drained", n, readErr)
	}
}

// TestDSTCrashHostSparesAppClosedConns: a conn whose victim-host end the
// application already close()d before the power loss is NOT reset at the
// surviving peer — the data and FIN are on the wire, and a powered-off machine
// emits no packet that could destroy bytes its peer already holds
// (DST-FAULT-SOUND). The peer drains and reads EOF, exactly as the pre-crash
// teardown left it.
func TestDSTCrashHostSparesAppClosedConns(t *testing.T) {
	var n int
	var firstErr, secondErr error
	var got [8]byte
	TestWith(t, 1, Options{Network: NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}, func(t *testing.T) {
		addrCh := make(chan string, 1)
		closed := make(chan struct{})
		dialed := make(chan struct{})
		crashed := make(chan struct{})
		readDone := make(chan struct{})
		exited := make(chan struct{})

		Host("victim", HostConfig{}, func() {
			go Process("writer", func() {
				l, err := net.Listen("tcp", HostIP("victim")+":0")
				if err != nil {
					t.Errorf("victim listen: %v", err)
					return
				}
				addrCh <- l.Addr().String()
				c, err := l.Accept()
				if err != nil {
					t.Errorf("victim accept: %v", err)
					return
				}
				if _, err := c.Write([]byte("abc")); err != nil {
					t.Errorf("victim write: %v", err)
					return
				}
				c.Close() // graceful FIN before the power loss
				close(closed)
				select {} // dies with the machine
			})
		})
		addr := <-addrCh

		Host("survivor", HostConfig{}, func() {
			go func() {
				defer close(exited)
				Process("reader", func() {
					c, err := net.Dial("tcp", addr)
					if err != nil {
						t.Errorf("survivor dial: %v", err)
						return
					}
					close(dialed)
					<-crashed
					n, firstErr = c.Read(got[:])
					_, secondErr = c.Read(make([]byte, 8))
					close(readDone)
				})
			}()
		})

		<-closed
		<-dialed
		CrashHost("victim")
		close(crashed)
		<-readDone
		<-exited
	})
	if n != 3 || string(got[:3]) != "abc" || firstErr != nil {
		t.Fatalf("read of gracefully-closed conn after the peer host crashed = (%d, %q, %v), want (3, %q, nil): the crash cannot destroy bytes the network already carries", n, got[:n], firstErr, "abc")
	}
	if secondErr != io.EOF {
		t.Fatalf("second read = %v, want io.EOF: the app-closed end FINned before the crash; no RST exists", secondErr)
	}
}

// TestDSTCrashHostFreesVictimPorts: a host crash deregisters the victim's OWN
// conn ends too, so after the machine restarts (a fresh Host declaration over
// the recovered disk) an explicit bind to a 2-tuple a pre-crash conn held
// succeeds — a real reboot clears the port space; a leaked registry entry would
// phantom-EADDRINUSE it.
func TestDSTCrashHostFreesVictimPorts(t *testing.T) {
	var redialErr error
	Test(t, 1, func(t *testing.T) {
		lnCh := make(chan net.Listener, 1)
		dialed := make(chan struct{})
		serverExited := make(chan struct{})

		Host("survivor", HostConfig{}, func() {
			go Process("server", func() {
				defer close(serverExited)
				l, err := net.Listen("tcp", HostIP("survivor")+":0")
				if err != nil {
					t.Errorf("survivor listen: %v", err)
					return
				}
				lnCh <- l
				for {
					if _, err := l.Accept(); err != nil {
						return
					}
				}
			})
		})
		ln := <-lnCh
		addr := ln.Addr().String()

		var src *net.TCPAddr
		Host("victim", HostConfig{}, func() {
			src = &net.TCPAddr{IP: net.ParseIP(HostIP("victim")), Port: 34567}
			go Process("dialer", func() {
				d := net.Dialer{LocalAddr: src}
				if _, err := d.Dial("tcp", addr); err != nil {
					t.Errorf("victim dial: %v", err)
					return
				}
				close(dialed)
				select {} // dies with the machine
			})
		})
		<-dialed

		CrashHost("victim")

		rebooted := make(chan struct{})
		Host("victim", HostConfig{}, func() { // the restart: same machine, fresh kernel
			go Process("dialer2", func() {
				d := net.Dialer{LocalAddr: src}
				_, redialErr = d.Dial("tcp", addr)
				close(rebooted)
			})
		})
		<-rebooted
		ln.Close()
		<-serverExited
	})
	if redialErr != nil {
		t.Fatalf("post-reboot dial from the pre-crash source port = %v, want success: the crash cleared the victim's port space", redialErr)
	}
}

// TestDSTCrashHostDropsInFlightBytesVictimDialer: the no-drain RST holds with
// the roles swapped — the victim host DIALED, so its conn end carries the
// LOWER registration sequence and is torn down first within the crash. The
// survivor's entry must still be matched against the pre-crash snapshot
// (dstMatchedVictims collects before any teardown): an interleaved
// match-and-reset loop would see the victim end's just-closed done channel,
// mistake it for an app-close, skip the survivor's end, and reintroduce the
// drain.
func TestDSTCrashHostDropsInFlightBytesVictimDialer(t *testing.T) {
	var n int
	var readErr error
	TestWith(t, 1, Options{Network: NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}, func(t *testing.T) {
		addrCh := make(chan string, 1)
		written := make(chan struct{})
		crashed := make(chan struct{})
		readDone := make(chan struct{})
		exited := make(chan struct{})

		Host("survivor", HostConfig{}, func() {
			go func() {
				defer close(exited)
				Process("reader", func() {
					l, err := net.Listen("tcp", HostIP("survivor")+":0")
					if err != nil {
						t.Errorf("survivor listen: %v", err)
						return
					}
					addrCh <- l.Addr().String()
					c, err := l.Accept()
					if err != nil {
						t.Errorf("survivor accept: %v", err)
						return
					}
					<-crashed
					n, readErr = c.Read(make([]byte, 8))
					close(readDone)
				})
			}()
		})
		addr := <-addrCh

		Host("victim", HostConfig{}, func() {
			go Process("writer", func() {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					t.Errorf("victim dial: %v", err)
					return
				}
				if _, err := c.Write([]byte("abc")); err != nil {
					t.Errorf("victim write: %v", err)
					return
				}
				close(written)
				select {} // dies with the machine
			})
		})

		<-written
		CrashHost("victim")
		close(crashed)
		<-readDone
		<-exited
	})
	if n != 0 || !errors.Is(readErr, syscall.ECONNRESET) {
		t.Fatalf("first read after the dialing writer's host crashed = (%d, %v), want (0, ECONNRESET): in-flight bytes must be dropped, not drained", n, readErr)
	}
}

// TestDSTCrashHostDialBlackholes: dialing a crashed declared host blackholes —
// a powered-off machine drops SYNs and no kernel exists to answer RST, so a
// deadline-less dial fails ETIMEDOUT at the retransmit horizon (2 virtual
// minutes by default), never instant ECONNREFUSED. A Host re-declaration
// reboots the machine: dials reach its kernel again and connect once a
// listener is up.
func TestDSTCrashHostDialBlackholes(t *testing.T) {
	var deadErr, rebootErr error
	var deadElapsed time.Duration
	Test(t, 1, func(t *testing.T) {
		addrCh := make(chan string, 1)
		Host("victim", HostConfig{}, func() {
			go Process("server", func() {
				l, err := net.Listen("tcp", HostIP("victim")+":0")
				if err != nil {
					t.Errorf("victim listen: %v", err)
					return
				}
				addrCh <- l.Addr().String()
				select {} // dies with the machine
			})
		})
		addr := <-addrCh

		CrashHost("victim")

		dialDone := make(chan struct{})
		Host("survivor", HostConfig{}, func() {
			go Process("dialer", func() {
				defer close(dialDone)
				start := time.Now()
				_, deadErr = net.Dial("tcp", addr)
				deadElapsed = time.Since(start)
			})
		})
		<-dialDone

		// Reboot: fresh Host declaration; its restarted server listens anew.
		addr2Ch := make(chan string, 1)
		lnCh := make(chan net.Listener, 1)
		serverExited := make(chan struct{})
		Host("victim", HostConfig{}, func() {
			go Process("server", func() {
				defer close(serverExited)
				l, err := net.Listen("tcp", HostIP("victim")+":0")
				if err != nil {
					t.Errorf("rebooted victim listen: %v", err)
					return
				}
				lnCh <- l
				addr2Ch <- l.Addr().String()
				for {
					if _, err := l.Accept(); err != nil {
						return
					}
				}
			})
		})
		ln := <-lnCh
		addr2 := <-addr2Ch

		redialDone := make(chan struct{})
		Host("survivor", HostConfig{}, func() {
			go Process("dialer2", func() {
				defer close(redialDone)
				_, rebootErr = net.Dial("tcp", addr2)
			})
		})
		<-redialDone
		ln.Close()
		<-serverExited
	})
	if !errors.Is(deadErr, syscall.ETIMEDOUT) {
		t.Fatalf("dial to the crashed host = %v, want ETIMEDOUT: a powered-off machine blackholes, only a live kernel refuses", deadErr)
	}
	if deadElapsed != 2*time.Minute {
		t.Fatalf("dial to the crashed host returned after %v, want the 2m retransmit horizon (the SYN blackholes until exhausted retries)", deadElapsed)
	}
	if rebootErr != nil {
		t.Fatalf("dial after the host rebooted = %v, want success: the reboot restores reachability", rebootErr)
	}
}

// TestDSTCrashHostProcessRestartRefused: a process cannot be restarted on a
// powered-off machine — Process on a crashed, not-yet-rebooted host panics,
// naming the fix (a Host re-declaration models the reboot). Allowing it would
// yield a half-alive machine: its server running and listening while every
// dial to it blackholes — a state reality cannot produce. After the reboot,
// the restart succeeds.
func TestDSTCrashHostProcessRestartRefused(t *testing.T) {
	var panicMsg string
	var rebootRestartOK bool
	Test(t, 1, func(t *testing.T) {
		started := make(chan struct{})
		go Process("node", func() { // implicit dedicated host "node"
			close(started)
			select {} // dies with the machine
		})
		<-started

		CrashHost("node")

		func() {
			defer func() {
				if r := recover(); r != nil {
					panicMsg = r.(string)
				}
			}()
			Process("node", func() {})
		}()

		// The reboot: an explicit Host declaration of the same machine.
		Host("node", HostConfig{}, func() {
			Process("node", func() { rebootRestartOK = true })
		})
	})
	if !strings.Contains(panicMsg, "powered off") {
		t.Fatalf("restarting a process on a crashed host panicked with %q, want a powered-off refusal naming the Host re-declaration", panicMsg)
	}
	if !rebootRestartOK {
		t.Fatal("process restart after the Host re-declaration did not run")
	}
}

// TestDSTCrashHostMidHandshakeDialTimesOut: a dial already mid-handshake
// (sleeping in the SYN traversal) when the target host loses power blackholes
// like any dial to a dead machine — power loss emits no RST, so the connect
// fails ETIMEDOUT at the retransmit horizon, never ECONNREFUSED from the
// closed listener's teardown.
func TestDSTCrashHostMidHandshakeDialTimesOut(t *testing.T) {
	var dialErr error
	var elapsed time.Duration
	TestWith(t, 1, Options{Network: NetworkConfig{CrossHostLatency: 50 * time.Millisecond}}, func(t *testing.T) {
		addrCh := make(chan string, 1)
		Host("victim", HostConfig{}, func() {
			go Process("server", func() {
				l, err := net.Listen("tcp", HostIP("victim")+":0")
				if err != nil {
					t.Errorf("victim listen: %v", err)
					return
				}
				addrCh <- l.Addr().String()
				select {} // dies with the machine
			})
		})
		addr := <-addrCh

		dialDone := make(chan struct{})
		Host("survivor", HostConfig{}, func() {
			go Process("dialer", func() {
				defer close(dialDone)
				start := time.Now()
				_, dialErr = net.Dial("tcp", addr)
				elapsed = time.Since(start)
			})
		})

		// Let the dial enter its 50ms SYN traversal, then cut the power at
		// half-flight.
		time.Sleep(25 * time.Millisecond)
		CrashHost("victim")
		<-dialDone
	})
	if !errors.Is(dialErr, syscall.ETIMEDOUT) {
		t.Fatalf("mid-handshake dial to the crashing host = %v, want ETIMEDOUT: power loss emits no RST", dialErr)
	}
	if want := 50*time.Millisecond + 2*time.Minute; elapsed != want {
		t.Fatalf("mid-handshake dial returned after %v, want %v (the SYN traversal, then the full retransmit horizon)", elapsed, want)
	}
}

// TestDSTCrashHostHealHostCannotResurrect: machine power and network faults
// are distinct facts — HealHost (a network heal) does not make a powered-off
// machine reachable; the dial still blackholes to ETIMEDOUT. Only a Host
// re-declaration reboots it.
func TestDSTCrashHostHealHostCannotResurrect(t *testing.T) {
	var dialErr error
	Test(t, 1, func(t *testing.T) {
		addrCh := make(chan string, 1)
		Host("victim", HostConfig{}, func() {
			go Process("server", func() {
				l, err := net.Listen("tcp", HostIP("victim")+":0")
				if err != nil {
					t.Errorf("victim listen: %v", err)
					return
				}
				addrCh <- l.Addr().String()
				select {} // dies with the machine
			})
		})
		addr := <-addrCh

		CrashHost("victim")
		HealHost("victim") // heals network cuts; cannot power a machine on

		dialDone := make(chan struct{})
		Host("survivor", HostConfig{}, func() {
			go Process("dialer", func() {
				defer close(dialDone)
				_, dialErr = net.Dial("tcp", addr)
			})
		})
		<-dialDone
	})
	if !errors.Is(dialErr, syscall.ETIMEDOUT) {
		t.Fatalf("dial to a crashed host after HealHost = %v, want ETIMEDOUT: a network heal cannot power a machine on", dialErr)
	}
}

// TestDSTHostRebootKeepsIsolation: the converse separation — a Host
// re-declaration (reboot) does not heal an injected network isolation. The
// rebooted machine's listener is up, but the dial still blackholes to
// ETIMEDOUT until HealHost.
func TestDSTHostRebootKeepsIsolation(t *testing.T) {
	var dialErr error
	Test(t, 1, func(t *testing.T) {
		lnCh := make(chan net.Listener, 1)
		serverExited := make(chan struct{})
		Host("island", HostConfig{}, func() {
			go Process("server", func() {
				defer close(serverExited)
				l, err := net.Listen("tcp", HostIP("island")+":0")
				if err != nil {
					t.Errorf("island listen: %v", err)
					return
				}
				lnCh <- l
				for {
					if _, err := l.Accept(); err != nil {
						return
					}
				}
			})
		})
		ln := <-lnCh
		addr := ln.Addr().String()

		Isolate("island")
		Host("island", HostConfig{}, func() {}) // reboot: must NOT heal the isolation

		dialDone := make(chan struct{})
		Host("survivor", HostConfig{}, func() {
			go Process("dialer", func() {
				defer close(dialDone)
				_, dialErr = net.Dial("tcp", addr)
			})
		})
		<-dialDone
		HealHost("island")
		ln.Close()
		<-serverExited
	})
	if !errors.Is(dialErr, syscall.ETIMEDOUT) {
		t.Fatalf("dial to an isolated host after its reboot = %v, want ETIMEDOUT: a reboot does not heal an injected network cut", dialErr)
	}
}
