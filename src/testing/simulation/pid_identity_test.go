// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestDSTPidMaxWrapAndReuse pins Options.PidMax's alloc_pid semantics: forward
// scan, wrap into [2, PidMax], and REUSE of a dead process's pid — with the
// reused pid serving its new incarnation's starttime, the same-pid-new-start
// discriminator clients parse /proc for.
func TestDSTPidMaxWrapAndReuse(t *testing.T) {
	body := func() (pids []int, firstStart, reuseStart uint64) {
		spawn := func(capture *uint64) int {
			var pid int
			Process("churn", func() {
				pid = os.Getpid()
				if capture != nil {
					st, err := processStartTime(pid)
					if err != nil {
						t.Errorf("starttime: %v", err)
					}
					*capture = st
				}
			})
			return pid
		}
		pids = append(pids, spawn(&firstStart))
		sleepMillis(30)
		for range 4 {
			pids = append(pids, spawn(nil))
		}
		pids = append(pids, spawn(&reuseStart))
		return
	}
	var pids1, pids2 []int
	var first1, reuse1, first2, reuse2 uint64
	RunWith(21, Options{PidMax: 6}, func() { pids1, first1, reuse1 = body() })
	RunWith(21, Options{PidMax: 6}, func() { pids2, first2, reuse2 = body() })

	want := []int{2, 3, 4, 5, 6, 2}
	if fmt.Sprint(pids1) != fmt.Sprint(want) {
		t.Fatalf("pid sequence = %v, want %v (forward scan, wrap at PidMax, reuse of the dead pid)", pids1, want)
	}
	if fmt.Sprint(pids2) != fmt.Sprint(pids1) || first2 != first1 || reuse2 != reuse1 {
		t.Fatalf("replay diverged: pids %v/%v starts %d,%d/%d,%d", pids1, pids2, first1, reuse1, first2, reuse2)
	}
	if reuse1 == first1 {
		t.Fatalf("reused pid 2's starttime = %d for both incarnations, want the new incarnation's own (the reuse discriminator)", reuse1)
	}
}

// TestDSTPidMaxSkipsLivePids pins the skip-live half of alloc_pid: a wrap
// passes over a pid whose process is still running.
func TestDSTPidMaxSkipsLivePids(t *testing.T) {
	var holderPid int
	var churn []int
	RunWith(22, Options{PidMax: 4}, func() {
		hold := make(chan struct{})
		held := make(chan int)
		go Process("holder", func() {
			held <- os.Getpid()
			<-hold
		})
		holderPid = <-held
		spawn := func(name string) int {
			var pid int
			Process(name, func() { pid = os.Getpid() })
			return pid
		}
		churn = append(churn, spawn("c1"), spawn("c2"), spawn("c3"))
		close(hold)
	})
	if holderPid != 2 {
		t.Fatalf("holder pid = %d, want 2", holderPid)
	}
	if fmt.Sprint(churn) != fmt.Sprint([]int{3, 4, 3}) {
		t.Fatalf("churn pids = %v, want [3 4 3] (the wrap skips the live pid 2)", churn)
	}
}

// TestDSTPidMaxExhaustionPanics pins the every-pid-live refusal: the kernel
// answers fork with EAGAIN there, and Process fails loud, STATE-NEUTRALLY — a
// failed fork leaves the parent unchanged, so the recovered caller keeps its
// node identity, no name is interned, and a later Process still gets its own
// implicit host (the unbounded exhaustion refusal's discipline, on the
// bounded path).
func TestDSTPidMaxExhaustionPanics(t *testing.T) {
	var exhaust any
	var bodyRan bool
	var neutralityBroken string
	var laterHostname string
	RunWith(23, Options{PidMax: 3}, func() {
		hold := make(chan struct{})
		ready := make(chan struct{}, 2)
		exited := make(chan struct{}, 2)
		go func() { Process("h1", func() { ready <- struct{}{}; <-hold }); exited <- struct{}{} }()
		go func() { Process("h2", func() { ready <- struct{}{}; <-hold }); exited <- struct{}{} }()
		<-ready
		<-ready
		rootHost, rootProc := dstCurrentNode()
		rootPID, rootHostname := os.Getpid(), pidTestHostname()
		nodeReg.mu.Lock()
		hosts, procs, nsNames := len(nodeReg.hosts), len(nodeReg.procs), len(nodeReg.pidns)
		nodeReg.mu.Unlock()
		func() {
			defer func() { exhaust = recover() }()
			ProcessWith("h3", ProcessConfig{PIDNamespace: "burned?"}, func() { bodyRan = true })
		}()
		host, proc := dstCurrentNode()
		nodeReg.mu.Lock()
		hosts2, procs2, nsNames2 := len(nodeReg.hosts), len(nodeReg.procs), len(nodeReg.pidns)
		nodeReg.mu.Unlock()
		if host != rootHost || proc != rootProc || os.Getpid() != rootPID || pidTestHostname() != rootHostname {
			neutralityBroken = fmt.Sprintf("node identity moved: host/proc %d/%d pid %d hostname %q", host, proc, os.Getpid(), pidTestHostname())
		} else if hosts2 != hosts || procs2 != procs || nsNames2 != nsNames {
			neutralityBroken = fmt.Sprintf("interns published: hosts %d->%d procs %d->%d ns %d->%d", hosts, hosts2, procs, procs2, nsNames, nsNames2)
		}
		close(hold)
		<-exited
		<-exited
		// With the holders exited, a later Process must land on its OWN
		// implicit host, not the refused declaration's.
		Process("h4", func() { laterHostname = pidTestHostname() })
	})
	msg, _ := exhaust.(string)
	if !strings.Contains(msg, "EAGAIN") || bodyRan {
		t.Fatalf("exhaustion panic = %v (body ran %v), want the fork-EAGAIN refusal and no body", exhaust, bodyRan)
	}
	if neutralityBroken != "" {
		t.Fatalf("refused admission mutated state: %s", neutralityBroken)
	}
	if laterHostname != "h4" {
		t.Fatalf("post-refusal Process hostname = %q, want its own implicit host h4", laterHostname)
	}
}

// TestDSTPidMaxValidation pins the option boundary: negative values, a PidMax
// not exceeding the root pid, and a pid_t-overflowing PidMax are refused
// before any state moves.
func TestDSTPidMaxValidation(t *testing.T) {
	// On 64-bit int this is 1<<31 (the overflow branch); on 32-bit it wraps
	// negative (the negative branch) — both must refuse with a PidMax panic.
	overPidT := math.MaxInt32
	overPidT++
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"negative", Options{PidMax: -1}},
		{"at root pid", Options{PidMax: 1}},
		{"below custom root pid", Options{PID: 500, PidMax: 300}},
		{"pid_t overflow", Options{PidMax: overPidT}},
	} {
		var p any
		func() {
			defer func() { p = recover() }()
			RunWith(1, tc.opts, func() {})
		}()
		msg, _ := p.(string)
		if !strings.Contains(msg, "PidMax") {
			t.Fatalf("%s: panic = %v, want a PidMax refusal", tc.name, p)
		}
	}
}

// TestDSTStarttimeAcrossReboot pins the cross-boot shape: starttime is host
// uptime at start, so a post-reboot process reads SMALL again — numerically
// collidable with a stale pre-reboot stamp, exactly the real machine's hazard
// the boot-epoch discriminator exists to catch.
func TestDSTStarttimeAcrossReboot(t *testing.T) {
	var preStart, postStart uint64
	Run(24, func() {
		Host("m", HostConfig{}, func() {
			sleepMillis(500)
			Process("w", func() {
				st, err := processStartTime(os.Getpid())
				if err != nil {
					t.Errorf("pre-reboot starttime: %v", err)
				}
				preStart = st
			})
		})
		CrashHost("m")
		sleepMillis(10)
		Host("m", HostConfig{}, func() {
			sleepMillis(20)
			Process("w", func() {
				st, err := processStartTime(os.Getpid())
				if err != nil {
					t.Errorf("post-reboot starttime: %v", err)
				}
				postStart = st
			})
		})
	})
	if preStart != 50 || postStart != 2 {
		t.Fatalf("starttimes pre/post reboot = %d/%d, want 50/2 (uptime ticks of each boot)", preStart, postStart)
	}
}

// TestDSTStarttimeSameTickCollision pins the host's own resolution caveat:
// two processes started within one 10ms USER_HZ tick share a starttime — the
// reason clients treat (pid, starttime) as a compound identity rather than
// relying on starttime uniqueness.
func TestDSTStarttimeSameTickCollision(t *testing.T) {
	var p1, p2 int
	var s1, s2 uint64
	Run(25, func() {
		Host("m", HostConfig{}, func() {
			sleepMillis(15)
			Process("a", func() { p1 = os.Getpid(); s1, _ = processStartTime(p1) })
			Process("b", func() { p2 = os.Getpid(); s2, _ = processStartTime(p2) })
		})
	})
	if p1 == p2 {
		t.Fatalf("pids = %d/%d, want distinct", p1, p2)
	}
	if s1 != 1 || s2 != s1 {
		t.Fatalf("same-tick starttimes = %d/%d, want both 1 (the 10ms resolution caveat)", s1, s2)
	}
}

// TestDSTPidNamespaces pins the sibling-container namespace model:
// /proc/self/ns/pid identity per ProcessWith namespace (root pid:[1]; named
// namespaces interned deterministically; same name = same namespace; a restart
// naming the namespace keeps it), cross-namespace kill(pid,0) = ESRCH and
// /proc/<pid>/stat invisible while a SAME-namespace peer sees both, and
// internal crash teardown unfiltered by namespace.
func TestDSTPidNamespaces(t *testing.T) {
	var rootNS, defNS, nsA1, nsA2, nsB, nsARestart, rootNSInNS string
	var writerPid int
	var crossKillErr, sameKillErr, postCrashKillErr error
	var crossStatErr, sameStatErr error
	readNS := func() string {
		l, err := os.Readlink("/proc/self/ns/pid")
		if err != nil {
			t.Errorf("readlink ns: %v", err)
		}
		return l
	}
	RunWith(31, Options{}, func() {
		rootNS = readNS()
		Process("plain", func() { defNS = readNS() })
		hold := make(chan struct{})
		ready := make(chan struct{})
		go ProcessWith("wa", ProcessConfig{PIDNamespace: "container-a"}, func() {
			nsA1 = readNS()
			writerPid = os.Getpid()
			// The Root resolver serves the same namespace identity.
			if root, err := os.OpenRoot("/"); err == nil {
				if l, err := root.Readlink("proc/self/ns/pid"); err != nil || l != nsA1 {
					t.Errorf("Root ns readlink = %q, %v; want %q", l, err, nsA1)
				}
				root.Close()
			} else {
				t.Errorf("OpenRoot: %v", err)
			}
			close(ready)
			<-hold
		})
		<-ready
		// A live pid in a SIBLING namespace: invisible to the root-ns prober.
		crossKillErr = syscall.Kill(writerPid, 0)
		_, crossStatErr = os.ReadFile("/proc/" + strconv.Itoa(writerPid) + "/stat")
		// A same-namespace peer sees it.
		ProcessWith("pa", ProcessConfig{PIDNamespace: "container-a"}, func() {
			nsA2 = readNS()
			sameKillErr = syscall.Kill(writerPid, 0)
			_, sameStatErr = os.ReadFile("/proc/" + strconv.Itoa(writerPid) + "/stat")
		})
		ProcessWith("wb", ProcessConfig{PIDNamespace: "container-b"}, func() {
			nsB = readNS()
			rootNSInNS = "" // the namespaced process probing the ROOT pid: cross-ns, ESRCH
			if err := syscall.Kill(1, 0); errors.Is(err, syscall.ESRCH) {
				rootNSInNS = "esrch"
			}
		})
		// Internal teardown is NOT namespace-filtered: a root-ns caller
		// crashes the container process.
		Crash("wa")
		ProcessWith("pa2", ProcessConfig{PIDNamespace: "container-a"}, func() {
			postCrashKillErr = syscall.Kill(writerPid, 0)
		})
		// A restart naming the namespace keeps it.
		ProcessWith("wa", ProcessConfig{PIDNamespace: "container-a"}, func() {
			nsARestart = readNS()
		})
	})

	if rootNS != "pid:[1]" || defNS != "pid:[1]" {
		t.Fatalf("root/default namespace = %q/%q, want pid:[1]", rootNS, defNS)
	}
	if nsA1 != "pid:[2]" || nsA2 != nsA1 || nsARestart != nsA1 {
		t.Fatalf("container-a namespace = %q/%q/restart %q, want stable pid:[2]", nsA1, nsA2, nsARestart)
	}
	if nsB != "pid:[3]" {
		t.Fatalf("container-b namespace = %q, want pid:[3]", nsB)
	}
	if !errors.Is(crossKillErr, syscall.ESRCH) {
		t.Fatalf("cross-namespace kill(live pid, 0) = %v, want ESRCH (sibling visibility)", crossKillErr)
	}
	if !errors.Is(crossStatErr, syscall.ENOENT) {
		t.Fatalf("cross-namespace /proc/<pid>/stat = %v, want ENOENT", crossStatErr)
	}
	if sameKillErr != nil || sameStatErr != nil {
		t.Fatalf("same-namespace probe kill/stat = %v/%v, want both nil", sameKillErr, sameStatErr)
	}
	if rootNSInNS != "esrch" {
		t.Fatalf("namespaced probe of the root pid succeeded, want ESRCH (siblings, no hierarchy)")
	}
	if !errors.Is(postCrashKillErr, syscall.ESRCH) {
		t.Fatalf("same-namespace kill after Crash = %v, want ESRCH (crash teardown is not namespace-filtered)", postCrashKillErr)
	}
}
