// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var procIsolationMappingSink byte

//go:noinline
func registerUnreachableCleanup(ran *atomic.Bool) {
	// Allocate >= maxTinySize (16 B) so the runtime tiny allocator cannot batch
	// the target into the same 16-byte block as a live string or scalar.
	obj := new([32]byte)
	runtime.AddCleanup(obj, func(v bool) { ran.Store(v) }, true)
}

func TestDSTCrossHostAndRebootPIDProcfsAndNamespaceIsolation(t *testing.T) {
	var h1CleanupRan, h2CleanupRan atomic.Bool
	RunWith(1, Options{PidMax: 6}, func() {
		var h1Pid2, h1Pid4, h2Pid2 int
		var h1NS, h2NS string
		var h1PreStat os.FileInfo
		h1Hold := make(chan struct{})
		h1Boot2Hold := make(chan struct{})
		h2Hold := make(chan struct{})
		var closeH1, closeH1Boot2, closeH2 sync.Once
		defer closeH1.Do(func() { close(h1Hold) })
		defer closeH1Boot2.Do(func() { close(h1Boot2Hold) })
		defer closeH2.Do(func() { close(h2Hold) })

		w1Started := make(chan struct{})
		h1Ready := make(chan struct{})
		h1Boot2Ready := make(chan struct{})
		h2Ready := make(chan struct{})

		Host("h1", func() {
			go ProcessWith("w1", ProcessConfig{PIDNamespace: "ns-shared"}, func() {
				h1Pid2 = os.Getpid()
				h1NS, _ = os.Readlink("/proc/self/ns/pid")
				fi, err := os.Stat("/proc/" + strconv.Itoa(h1Pid2) + "/stat")
				if err != nil {
					t.Errorf("h1 stat own procfs: %v", err)
				}
				h1PreStat = fi
				registerUnreachableCleanup(&h1CleanupRan)
				close(w1Started)
				<-h1Hold
			})
			<-w1Started
			ProcessWith("spacer", ProcessConfig{PIDNamespace: "ns-shared"}, func() {})
			go ProcessWith("w2", ProcessConfig{PIDNamespace: "ns-shared"}, func() {
				h1Pid4 = os.Getpid()
				close(h1Ready)
				<-h1Hold
			})
		})

		Host("h2", func() {
			go ProcessWith("w1", ProcessConfig{PIDNamespace: "ns-shared"}, func() {
				h2Pid2 = os.Getpid()
				h2NS, _ = os.Readlink("/proc/self/ns/pid")
				registerUnreachableCleanup(&h2CleanupRan)
				close(h2Ready)
				<-h2Hold
			})
		})

		<-h1Ready
		<-h2Ready

		if h1Pid2 != 2 || h2Pid2 != 2 || h1Pid4 != 4 {
			t.Fatalf("per-host pid allocation = (h1:%d,%d h2:%d), want (2,4 and 2)", h1Pid2, h1Pid4, h2Pid2)
		}
		// Each host's first named PID namespace gets its own local pid:[2] handle,
		// isolated across machines.
		if h1NS != "pid:[2]" || h2NS != "pid:[2]" {
			t.Fatalf("per-host PIDNamespace handles = (%q, %q), want (pid:[2], pid:[2])", h1NS, h2NS)
		}

		// From h2's "ns-shared" (where probe itself occupies pid 3), h1's live pid 4
		// is completely invisible (ESRCH / ENOENT), while h1's "ns-shared" sees it cleanly.
		Host("h2", func() {
			ProcessWith("probe", ProcessConfig{PIDNamespace: "ns-shared"}, func() {
				if err := syscall.Kill(h1Pid4, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("cross-host Kill(%d, 0) from h2 = %v, want ESRCH", h1Pid4, err)
				}
				if _, err := os.Stat("/proc/" + strconv.Itoa(h1Pid4) + "/stat"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cross-host /proc/%d/stat from h2 = %v, want ENOENT", h1Pid4, err)
				}
			})
		})
		Host("h1", func() {
			ProcessWith("probe", ProcessConfig{PIDNamespace: "ns-shared"}, func() {
				if err := syscall.Kill(h1Pid4, 0); err != nil {
					t.Fatalf("same-host same-ns Kill(%d, 0) = %v, want nil", h1Pid4, err)
				}
			})
		})

		// Reboot h1: pre-crash PIDs vanish immediately on boot 2, and when boot 2
		// allocates PID 2 again its /proc/2/stat inode is distinct from boot 1's.
		CrashHost("h1")
		Host("h1", func() {
			if err := syscall.Kill(h1Pid2, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("post-CrashHost Kill(stale pid %d, 0) = %v, want ESRCH", h1Pid2, err)
			}
			if _, err := os.Stat("/proc/" + strconv.Itoa(h1Pid2) + "/stat"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("post-CrashHost /proc/%d/stat before reuse = %v, want ENOENT", h1Pid2, err)
			}
			go ProcessWith("w1", ProcessConfig{PIDNamespace: "ns-shared"}, func() {
				postPid := os.Getpid()
				if postPid != 2 {
					t.Errorf("post-reboot first pid = %d, want 2", postPid)
				}
				postNS, err := os.Readlink("/proc/self/ns/pid")
				if err != nil || postNS != "pid:[2]" {
					t.Errorf("post-reboot PIDNamespace = %q, %v; want pid:[2]", postNS, err)
				}
				fiPost, err := os.Stat("/proc/" + strconv.Itoa(postPid) + "/stat")
				if err != nil {
					t.Errorf("post-reboot stat own procfs: %v", err)
				} else if os.SameFile(h1PreStat, fiPost) {
					t.Errorf("os.SameFile across host reboot PID reuse = true, want false (distinct boot epoch)")
				}
				close(h1Boot2Ready)
				<-h1Boot2Hold
			})
		})
		<-h1Boot2Ready

		// Reach synctestRun quiescence (which forces GC + flushes and drains
		// callbacks) while BOTH h1 boot 2 holds live PID 2 AND h2 holds live PID 2:
		// crashed boot-1 w1's cleanup must be discarded despite PID 2 reuse on h1,
		// whereas live h2 w1's cleanup must run before h2 w1 exits.
		time.Sleep(time.Millisecond)

		// Meanwhile h2's live process was untouched by h1's crash and reboot.
		Host("h2", func() {
			ProcessWith("probe2", ProcessConfig{PIDNamespace: "ns-shared"}, func() {
				if err := syscall.Kill(h2Pid2, 0); err != nil {
					t.Fatalf("h2 live process after h1 CrashHost = %v, want nil", err)
				}
			})
		})
		closeH1Boot2.Do(func() { close(h1Boot2Hold) })
		closeH2.Do(func() { close(h2Hold) })
		closeH1.Do(func() { close(h1Hold) })
	})
	if h1CleanupRan.Load() {
		t.Fatalf("crashed h1 pid 2 cleanup executed despite crash (cross-host or reboot pid alias)")
	}
	if !h2CleanupRan.Load() {
		t.Fatalf("live h2 pid 2 cleanup did not execute")
	}
}

func TestDSTOverlappingProcessInvocationsIsolateSlotsAndMappingFault(t *testing.T) {
	t.Setenv("DST_SLOT_ENV", "base")
	const page = 4096

	Test(t, 1, func(t *testing.T) {
		Host("h", func() {
			os.Mkdir("/dir1", 0o755)
			os.Mkdir("/dir2", 0o755)

			type instInfo struct {
				pid  int
				addr string
				file *os.File
			}
			ready1 := make(chan instInfo, 1)
			ready2 := make(chan instInfo, 1)
			triggerFault1 := make(chan struct{})
			release2 := make(chan struct{})
			done2 := make(chan struct{})

			// Instance 1 of ("h", "worker"): will die via SIGBUS mapping fault.
			go Process("worker", func() {
				if err := os.Chdir("/dir1"); err != nil {
					t.Error(err)
					return
				}
				os.Setenv("DST_SLOT_ENV", "inst1")
				f1, err := os.OpenFile("/lock1", os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					t.Error(err)
					return
				}
				f1.Truncate(page)
				if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Error(err)
					return
				}
				m, err := syscall.Mmap(int(f1.Fd()), 0, 2*page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Error(err)
					return
				}
				ln1, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Error(err)
					return
				}
				ready1 <- instInfo{pid: os.Getpid(), addr: ln1.Addr().String(), file: f1}
				<-triggerFault1
				procIsolationMappingSink = m[page] // SIGBUS beyond EOF — must kill ONLY inst1!
			})

			// Instance 2 of ("h", "worker"): overlapping same-name invocation!
			check2 := make(chan string, 1)
			doCheck2 := make(chan struct{})
			go func() {
				defer close(done2)
				Process("worker", func() {
					if err := os.Chdir("/dir2"); err != nil {
						t.Error(err)
						return
					}
					os.Setenv("DST_SLOT_ENV", "inst2")
					f2, err := os.OpenFile("/lock2", os.O_CREATE|os.O_RDWR, 0o644)
					if err != nil {
						t.Error(err)
						return
					}
					if err := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
						t.Error(err)
						return
					}
					ln2, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Error(err)
						return
					}
					go func() {
						for {
							c, err := ln2.Accept()
							if err != nil {
								return
							}
							c.Close()
						}
					}()
					ready2 <- instInfo{pid: os.Getpid(), addr: ln2.Addr().String(), file: f2}
					<-doCheck2
					cwd, _ := os.Getwd()
					check2 <- fmt.Sprintf("cwd=%s env=%s", cwd, os.Getenv("DST_SLOT_ENV"))
					<-release2
				})
			}()

			i1 := <-ready1
			i2 := <-ready2
			if i1.pid == i2.pid {
				t.Fatalf("overlapping invocations shared pid %d", i1.pid)
			}

			// Trigger SIGBUS in inst1 and wait for inst1's PID death.
			close(triggerFault1)
			for syscall.Kill(i1.pid, 0) == nil {
				runtime.Gosched()
			}

			// inst1's FD, flock, and listener must be torn down immediately even though inst2 is still live!
			if _, err := i1.file.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("inst1 file after SIGBUS = %v, want os.ErrClosed", err)
			}
			probeLock1, err := os.OpenFile("/lock1", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Flock(int(probeLock1.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("inst1 flock still held after SIGBUS while inst2 live: %v", err)
			}
			probeLock1.Close()
			if _, err := net.Dial("tcp", i1.addr); !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("inst1 listener after SIGBUS = %v, want ECONNREFUSED", err)
			}

			// Meanwhile inst2 (same ("h", "worker") name!) is completely alive and isolated!
			if err := syscall.Kill(i2.pid, 0); err != nil {
				t.Fatalf("inst2 killed by inst1's SIGBUS: Kill = %v", err)
			}
			close(doCheck2)
			if got := <-check2; got != "cwd=/dir2 env=inst2" {
				t.Fatalf("inst2 state = %q, want cwd=/dir2 env=inst2", got)
			}
			c2, err := net.Dial("tcp", i2.addr)
			if err != nil {
				t.Fatalf("inst2 listener broken by inst1 teardown: %v", err)
			}
			c2.Close()
			probeLock2, err := os.OpenFile("/lock2", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Flock(int(probeLock2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
				t.Fatalf("inst2 flock released prematurely: %v, want EWOULDBLOCK", err)
			}

			// Let inst2 exit normally and verify immediate cleanup of inst2's resources.
			close(release2)
			<-done2
			if err := syscall.Flock(int(probeLock2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("inst2 flock not released on exit: %v", err)
			}
			probeLock2.Close()
			if _, err := net.Dial("tcp", i2.addr); !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("inst2 listener after exit = %v, want ECONNREFUSED", err)
			}
		})
	})
}

func TestDSTRootHostEnvIsolationAndCrashHostReset(t *testing.T) {
	t.Setenv("DST_ROOT_HOST_ENV", "initial")

	Test(t, 1, func(t *testing.T) {
		Host("h1", func() {
			if got := os.Getenv("DST_ROOT_HOST_ENV"); got != "initial" {
				t.Fatalf("h1 initial env = %q, want initial", got)
			}
			os.Setenv("DST_ROOT_HOST_ENV", "from-h1")
		})
		Host("h2", func() {
			if got := os.Getenv("DST_ROOT_HOST_ENV"); got != "initial" {
				t.Fatalf("h2 saw h1 root env mutation: got %q, want initial", got)
			}
			os.Setenv("DST_ROOT_HOST_ENV", "from-h2")
		})
		if got := os.Getenv("DST_ROOT_HOST_ENV"); got != "initial" {
			t.Fatalf("unnamed root host saw named host env mutation: got %q, want initial", got)
		}

		CrashHost("h1")
		Host("h1", func() {
			if got := os.Getenv("DST_ROOT_HOST_ENV"); got != "initial" {
				t.Fatalf("h1 root env after CrashHost = %q, want initial", got)
			}
		})
		Host("h2", func() {
			if got := os.Getenv("DST_ROOT_HOST_ENV"); got != "from-h2" {
				t.Fatalf("h2 root env disturbed by h1 CrashHost: got %q, want from-h2", got)
			}
		})
	})
}

func TestDSTSignalIgnoredFenceAndNonBubbleIdentityPassthrough(t *testing.T) {
	realPID := os.Getpid()
	realPPID := os.Getppid()
	realUID := os.Getuid()
	realHost, err := os.Hostname()
	if err != nil {
		t.Fatalf("host Hostname: %v", err)
	}
	realUser, err := user.Current()
	if err != nil {
		t.Fatalf("host user.Current: %v", err)
	}
	realCPU := runtime.NumCPU()

	// signal.Ignored works normally before Run.
	_ = signal.Ignored(os.Interrupt)

	inBubble := make(chan struct{})
	nonBubbleErr := make(chan error, 1)
	go func() {
		<-inBubble
		if p := dstDidPanic(func() { _ = signal.Ignored(os.Interrupt) }); p {
			nonBubbleErr <- errors.New("non-bubble signal.Ignored panicked during active simulation")
			return
		}
		if got := os.Getpid(); got != realPID {
			nonBubbleErr <- fmt.Errorf("non-bubble Getpid = %d, want %d", got, realPID)
			return
		}
		if got := os.Getppid(); got != realPPID {
			nonBubbleErr <- fmt.Errorf("non-bubble Getppid = %d, want %d", got, realPPID)
			return
		}
		if got := os.Getuid(); got != realUID {
			nonBubbleErr <- fmt.Errorf("non-bubble Getuid = %d, want %d", got, realUID)
			return
		}
		if got, herr := os.Hostname(); herr != nil || got != realHost {
			nonBubbleErr <- fmt.Errorf("non-bubble Hostname = (%q, %v), want (%q, nil)", got, herr, realHost)
			return
		}
		if u, uerr := user.Current(); uerr != nil || u.Uid != realUser.Uid || u.Username != realUser.Username {
			nonBubbleErr <- fmt.Errorf("non-bubble user.Current = (%+v, %v), want %+v", u, uerr, realUser)
			return
		}
		if got := runtime.NumCPU(); got != realCPU {
			nonBubbleErr <- fmt.Errorf("non-bubble NumCPU = %d, want %d", got, realCPU)
			return
		}
		if got := os.Getenv("DST_ONLY_IN_BUBBLE"); got != "" {
			nonBubbleErr <- fmt.Errorf("non-bubble Getenv leaked bubble env: %q", got)
			return
		}
		if err := syscall.Kill(realPID, 0); err != nil {
			nonBubbleErr <- fmt.Errorf("non-bubble Kill(realPID, 0) = %v, want nil", err)
			return
		}
		nonBubbleErr <- nil
	}()

	var bubbleIgnoredPanicked bool
	RunWith(1, Options{Hostname: "sim-box", PID: 77, NumCPU: 3}, func() {
		os.Setenv("DST_ONLY_IN_BUBBLE", "secret")
		bubbleIgnoredPanicked = dstDidPanic(func() {
			_ = signal.Ignored(os.Interrupt)
		})
		close(inBubble)
		if err := <-nonBubbleErr; err != nil {
			t.Fatalf("%v", err)
		}
		if got := os.Getenv("DST_ONLY_IN_BUBBLE"); got != "secret" {
			t.Fatalf("bubble env = %q, want secret", got)
		}
	})

	if !bubbleIgnoredPanicked {
		t.Fatalf("signal.Ignored inside bubble did not panic")
	}
	// Works normally again after Run returns.
	_ = signal.Ignored(os.Interrupt)
}

func TestDSTPublicAPISurfaceEndToEnd(t *testing.T) {
	// 1. Empty names panic inside an active Run as well.
	Run(1, func() {
		for _, tc := range []struct {
			name string
			call func()
		}{
			{"Host", func() { Host("", func() {}) }},
			{"HostWith", func() { HostWith("", HostConfig{}, func() {}) }},
			{"Process", func() { Process("", func() {}) }},
			{"ProcessWith", func() { ProcessWith("", ProcessConfig{}, func() {}) }},
		} {
			panicked, msg := didPanicWith(tc.call)
			if !panicked || !strings.Contains(msg, "must be non-empty") {
				t.Fatalf("%s(\"\") inside run: panicked=%v msg=%q, want non-empty name panic", tc.name, panicked, msg)
			}
		}

		// 2. SlowDisk / UnslowDisk round-trip.
		Host("disknode", func() {
			f, err := os.OpenFile("/lat", os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			SlowDisk("disknode", 50*time.Millisecond)
			t0 := time.Now()
			if _, err := f.Write([]byte("slow")); err != nil {
				t.Fatal(err)
			}
			if err := f.Sync(); err != nil {
				t.Fatal(err)
			}
			slowElapsed := time.Since(t0)
			if slowElapsed < 50*time.Millisecond {
				t.Fatalf("SlowDisk elapsed = %v, want >= 50ms", slowElapsed)
			}

			UnslowDisk("disknode")
			t1 := time.Now()
			if _, err := f.Write([]byte("fast")); err != nil {
				t.Fatal(err)
			}
			if err := f.Sync(); err != nil {
				t.Fatal(err)
			}
			if fastElapsed := time.Since(t1); fastElapsed != 0 {
				t.Fatalf("UnslowDisk elapsed = %v, want 0", fastElapsed)
			}
		})
	})

	// 3. ExploreWith threads embedded Options (Hostname, PID, NumCPU, Network)
	// into explored runs and Failure, records Failure.Err from func() error, and
	// Replay restores the identical Options and returns structured ReplayResult.
	sut := func() error {
		h, _ := os.Hostname()
		if h != "explore-root" || os.Getpid() != 9 || runtime.NumCPU() != 2 {
			return fmt.Errorf("unexpected identity: host=%q pid=%d cpu=%d", h, os.Getpid(), runtime.NumCPU())
		}
		addrCh := make(chan string, 1)
		done := make(chan struct{})
		Host("srv", func() {
			ln, err := net.Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}
			_, p, _ := net.SplitHostPort(ln.Addr().String())
			addrCh <- HostIP("srv") + ":" + p
			go func() {
				c, err := ln.Accept()
				if err == nil {
					c.Close()
				}
				<-done
				ln.Close()
			}()
		})
		var dialElapsed time.Duration
		Host("cli", func() {
			t0 := time.Now()
			c, err := net.Dial("tcp", <-addrCh)
			if err != nil {
				panic(err)
			}
			dialElapsed = time.Since(t0)
			c.Close()
			close(done)
		})
		if dialElapsed != 50*time.Millisecond {
			return fmt.Errorf("cross-host dial elapsed = %v, want 50ms (1 RTT = 2 * 25ms)", dialElapsed)
		}
		return errors.New("intentional sut failure")
	}

	expOpts := ExploreOptions{
		Mode:         Exhaustive,
		MaxSchedules: 1,
		Options: Options{
			Hostname: "explore-root",
			PID:      9,
			NumCPU:   2,
			Network: NetworkConfig{
				CrossHostLatency: 25 * time.Millisecond,
			},
		},
	}
	res := ExploreWith(7, expOpts, sut)
	if len(res.Failures) != 1 {
		t.Fatalf("ExploreWith failures = %d, want 1", len(res.Failures))
	}
	f := res.Failures[0]
	if f.Err != "intentional sut failure" {
		t.Fatalf("Failure.Err = %q, want %q", f.Err, "intentional sut failure")
	}
	if f.Options.Network.CrossHostLatency != 25*time.Millisecond || f.Options.Hostname != "explore-root" {
		t.Fatalf("Failure.Options not recorded: %+v", f.Options)
	}
	rep := Replay(7, f, sut)
	if !rep.Failed() || rep.Err == nil || rep.Err.Error() != "intentional sut failure" || rep.Raced {
		t.Fatalf("ReplayResult = %+v (Failed=%v), want Err=\"intentional sut failure\", Raced=false", rep, rep.Failed())
	}
}

func TestDSTSameBootPidMaxReuseDistinctProcStatSameFile(t *testing.T) {
	opts := Options{PID: 1, PidMax: 2}
	var run1Stat os.FileInfo

	RunWith(1, opts, func() {
		Host("h1", func() {
			var fi1, fi2 os.FileInfo
			// First incarnation on (h1, boot 1) at virtual t=0 (starttime 0): gets pid 2.
			Process("p1", func() {
				if pid := os.Getpid(); pid != 2 {
					t.Fatalf("p1 pid = %d, want 2", pid)
				}
				selfStat, err := os.Stat("/proc/self/stat")
				if err != nil {
					t.Fatalf("p1 stat self: %v", err)
				}
				pidStat, err := os.Stat("/proc/2/stat")
				if err != nil {
					t.Fatalf("p1 stat /proc/2/stat: %v", err)
				}
				if !os.SameFile(selfStat, pidStat) {
					t.Fatalf("p1 SameFile(/proc/self/stat, /proc/2/stat) = false, want true within incarnation")
				}
				fi1 = pidStat
			})

			// Second incarnation on the SAME (h1, boot 1) within the SAME 10ms USER_HZ tick
			// (virtual t=0, starttime 0): reuses pid 2 under PidMax=2, must NOT SameFile-alias p1.
			Process("p2", func() {
				if pid := os.Getpid(); pid != 2 {
					t.Fatalf("p2 pid = %d, want 2 (reused under PidMax=2)", pid)
				}
				selfStat, err := os.Stat("/proc/self/stat")
				if err != nil {
					t.Fatalf("p2 stat self: %v", err)
				}
				pidStat, err := os.Stat("/proc/2/stat")
				if err != nil {
					t.Fatalf("p2 stat /proc/2/stat: %v", err)
				}
				if !os.SameFile(selfStat, pidStat) {
					t.Fatalf("p2 SameFile(/proc/self/stat, /proc/2/stat) = false, want true within incarnation")
				}
				fi2 = pidStat
			})

			if os.SameFile(fi1, fi2) {
				t.Fatalf("os.SameFile across same-(host, boot, tick) PID reuse = true, want false")
			}
			run1Stat = fi1
		})
	})

	// Across separate simulation runs, identical (host, boot, slot, pid, starttime)
	// must also never alias via SameFile.
	RunWith(1, opts, func() {
		Host("h1", func() {
			Process("p1", func() {
				fiRun2, err := os.Stat("/proc/2/stat")
				if err != nil {
					t.Fatalf("run2 p1 stat: %v", err)
				}
				if os.SameFile(run1Stat, fiRun2) {
					t.Fatalf("os.SameFile across separate simulation runs = true, want false")
				}
			})
		})
	})
}

//go:noinline
func registerBlockingHostRootCleanup(started, hold chan struct{}) {
	obj := new([32]byte)
	runtime.AddCleanup(obj, func(pair [2]chan struct{}) {
		close(pair[0])
		<-pair[1]
	}, [2]chan struct{}{started, hold})
}

//go:noinline
func registerBubbleChanRootCleanup(leaked *atomic.Bool) {
	ch := make(chan struct{})
	obj := new([32]byte)
	runtime.AddCleanup(obj, func(c chan struct{}) {
		// If swept and flushed outside the originating bubble, closing c fatals
		// with "channel belongs to a different synctest bubble"; inside or
		// discarded on teardown, leaked stays false.
		close(c)
		leaked.Store(true)
	}, ch)
}

func TestDSTCrashHostKillsBlockedGCDrainAndReapsSyncPool(t *testing.T) {
	var pool sync.Pool
	var lateRootCleanupRan atomic.Bool

	Run(1, func() {
		// Put a bubble-associated channel into a package/test-scoped sync.Pool:
		// end-of-run 2-generation GC reap must clear it even if CrashHost kills gcDrain.
		pool.Put(make(chan int))

		started := make(chan struct{})
		hold := make(chan struct{})
		Host("h1", func() {
			registerBlockingHostRootCleanup(started, hold)
		})

		// Force GC and advance virtual time so bubble.gcDrain starts running the
		// h1-stamped root cleanup and blocks durably on <-hold.
		runtime.GC()
		time.Sleep(time.Millisecond)
		select {
		case <-started:
		default:
			t.Fatalf("h1 root cleanup did not start on gcDrain")
		}

		// Register an unswept root cleanup after GC while gcDrain is blocked on h1,
		// then crash h1 (killing bubble.gcDrain via dstMarkHostGoroutinesCrashed).
		registerBubbleChanRootCleanup(&lateRootCleanupRan)
		CrashHost("h1")
	})

	// A subsequent simulation run must observe an empty sync.Pool (no cross-bubble
	// channel fatal) and must not execute the previous run's unswept bubble cleanup.
	Run(2, func() {
		if got := pool.Get(); got != nil {
			t.Fatalf("sync.Pool retained bubble value across CrashHost(gcDrain) teardown: %v", got)
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	})
	if lateRootCleanupRan.Load() {
		t.Fatalf("unswept root cleanup from prior run executed after CrashHost killed gcDrain")
	}
}

//go:noinline
func registerHostRootFinalizerAndCleanup(finRan, cleanRan *atomic.Int32, hostObserved *atomic.Value) {
	obj1 := new([32]byte)
	runtime.SetFinalizer(obj1, func(*[32]byte) {
		hn, _ := os.Hostname()
		if hostObserved != nil {
			hostObserved.Store(hn)
		}
		finRan.Add(1)
	})
	obj2 := new([32]byte)
	runtime.AddCleanup(obj2, func(cnt *atomic.Int32) {
		cnt.Add(1)
	}, cleanRan)
}

func TestDSTHostRootCallbacksDiscardedAcrossCrashAndReboot(t *testing.T) {
	Test(t, 1, func(t *testing.T) {
		var boot1Fin, boot1Clean atomic.Int32
		var boot2Fin, boot2Clean atomic.Int32
		var boot2Host atomic.Value

		Host("h1", func() {
			registerHostRootFinalizerAndCleanup(&boot1Fin, &boot1Clean, nil)
		})
		CrashHost("h1")

		// Reboot h1: boot-1 callbacks must remain discarded, while boot-2 callbacks
		// execute on h1's identity.
		Host("h1", func() {
			registerHostRootFinalizerAndCleanup(&boot2Fin, &boot2Clean, &boot2Host)
		})
		runtime.GC()
		time.Sleep(time.Millisecond)

		if boot1Fin.Load() != 0 || boot1Clean.Load() != 0 {
			t.Fatalf("crashed boot-1 host callbacks ran after reboot: fin=%d clean=%d", boot1Fin.Load(), boot1Clean.Load())
		}
		if boot2Fin.Load() != 1 || boot2Clean.Load() != 1 {
			t.Fatalf("boot-2 host callbacks did not run: fin=%d clean=%d", boot2Fin.Load(), boot2Clean.Load())
		}
		if got, _ := boot2Host.Load().(string); got != "h1" {
			t.Fatalf("boot-2 host finalizer observed hostname %q, want %q", got, "h1")
		}
	})
}
