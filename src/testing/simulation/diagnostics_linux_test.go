// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"bytes"
	"fmt"
	"internal/testenv"
	"io"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func runDiagnosticsChild(t *testing.T, run, env string, wantFail bool, extraArgs ...string) string {
	t.Helper()
	testenv.MustHaveExec(t)
	args := []string{"-test.run=^" + run + "$", "-test.count=1"}
	args = append(args, extraArgs...)
	cmd := testenv.Command(t, testenv.Executable(t), args...)
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, env+"=1")
	out, err := cmd.CombinedOutput()
	if wantFail && err == nil {
		t.Fatalf("child %s unexpectedly passed:\n%s", run, out)
	}
	if !wantFail && err != nil {
		t.Fatalf("child %s failed: %v\n%s", run, err, out)
	}
	return string(out)
}

func dstParkedExitWorker(ch <-chan struct{}) {
	<-ch
}

func TestDiagnosticsProcessExitKilledGoroutinesOnFailure(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_EXIT_KILL_CHILD") == "1" {
		Test(t, 41, func(t *testing.T) {
			Host("n1", HostConfig{}, func() {
				Process("srv", func() {
					ready := make(chan struct{}, 3)
					block := make(chan struct{})
					for range 3 {
						go func() {
							ready <- struct{}{}
							dstParkedExitWorker(block)
						}()
					}
					for range 3 {
						<-ready
					}
					// Process returns while the 3 child goroutines remain blocked.
				})
			})
			t.Fatal("expected message never arrived")
		})
		return
	}

	out := runDiagnosticsChild(t, "TestDiagnosticsProcessExitKilledGoroutinesOnFailure", "GO_DST_DIAG_EXIT_KILL_CHILD", true)
	for _, want := range []string{
		"--- simulation diagnostics (seed 41) ---",
		"killed goroutines on process exit:",
		`host="n1" proc="srv"`,
		"3 live goroutine(s) killed on process exit",
		"[chan receive (durable)]:",
		"diagnostics_linux_test.go:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("failure output missing %q:\n%s", want, out)
		}
	}

	re := regexp.MustCompile(`g#(\d+) \[`)
	matches := re.FindAllStringSubmatch(out, -1)
	if len(matches) != 3 {
		t.Fatalf("killed goroutine lines = %d, want 3:\n%s", len(matches), out)
	}
	var prev uint64
	for i, m := range matches {
		seq, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			t.Fatalf("parse seq %q: %v", m[1], err)
		}
		if i > 0 && seq <= prev {
			t.Errorf("killed goroutine seqs not in strictly ascending canonical order: %v", matches)
		}
		prev = seq
	}
}

func TestDiagnosticsProcessExitKillsReportedOnDeadlockStall(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_DEADLOCK_CHILD") == "1" {
		Test(t, 43, func(t *testing.T) {
			reply := make(chan int)
			Host("db-host", HostConfig{}, func() {
				Process("db-proc", func() {
					ready := make(chan struct{})
					never := make(chan struct{})
					go func() {
						close(ready)
						<-never
						reply <- 1
					}()
					<-ready
					// Early Process return silently kills the sender above.
				})
			})
			<-reply // stalls the bubble in durable deadlock
		})
		return
	}

	out := runDiagnosticsChild(t, "TestDiagnosticsProcessExitKillsReportedOnDeadlockStall", "GO_DST_DIAG_DEADLOCK_CHILD", true)
	for _, want := range []string{
		"--- simulation diagnostics (seed 43) ---",
		"killed goroutines on process exit:",
		`host="db-host" proc="db-proc"`,
		"1 live goroutine(s) killed on process exit",
		"deadlock: all goroutines in bubble are blocked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deadlock output missing %q:\n%s", want, out)
		}
	}
}

func TestDiagnosticsPostRunAssertionEmitsViaCleanup(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_POSTRUN_CHILD") == "1" {
		var got int
		Test(t, 47, func(t *testing.T) {
			Host("h1", HostConfig{}, func() {
				Process("worker", func() {
					fmt.Println("worker-boot-msg")
					got = 1
				})
			})
		})
		if got != 2 {
			t.Fatalf("got = %d, want 2", got)
		}
		return
	}

	out := runDiagnosticsChild(t, "TestDiagnosticsPostRunAssertionEmitsViaCleanup", "GO_DST_DIAG_POSTRUN_CHILD", true)
	for _, want := range []string{
		"--- simulation diagnostics (seed 47) ---",
		"captured bubble stdio:",
		`[host="h1" proc="worker"`,
		"worker-boot-msg",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("post-run failure output missing %q:\n%s", want, out)
		}
	}
}

func TestDiagnosticsSilentOnPassAndIntentionalCrashFaults(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_PASS_CHILD") == "1" {
		Test(t, 53, func(t *testing.T) {
			Host("n1", HostConfig{}, func() {
				Process("srv", func() {
					ready := make(chan struct{})
					block := make(chan struct{})
					go func() {
						close(ready)
						<-block
					}()
					<-ready
					fmt.Println("quiet-on-pass")
				})
			})
		})
		return
	}
	if os.Getenv("GO_DST_DIAG_CRASH_FAULT_CHILD") == "1" {
		daemonDone := make(chan struct{})
		victimReady := make(chan struct{})
		WorldTest(t, 59, Options{}, []HostDecl{
			{
				Name: "n1",
				Boot: func() {
					Process("daemon", func() {
						ready := make(chan struct{})
						block := make(chan struct{})
						go func() {
							close(ready)
							<-block
						}()
						<-ready
					})
					close(daemonDone)
					ready2 := make(chan struct{})
					block2 := make(chan struct{})
					go func() {
						close(ready2)
						<-block2
					}()
					<-ready2
				},
			},
			{
				Name: "n2",
				Boot: func() {
					ready := make(chan struct{})
					block := make(chan struct{})
					go func() {
						close(ready)
						<-block
					}()
					<-ready
					go func() {
						Process("victim", func() {
							close(victimReady)
							blockV := make(chan struct{})
							<-blockV
						})
					}()
				},
			},
		}, func(t *testing.T, ctl *Ctl) {
			<-daemonDone
			<-victimReady
			Crash("victim")
			CrashHost("n2")
			// daemon on n1 exited normally above -> recorded; n2/victim died via Crash/CrashHost -> NOT recorded.
			t.Fatal("trigger diagnostics check")
		})
		return
	}

	passOut := runDiagnosticsChild(t, "TestDiagnosticsSilentOnPassAndIntentionalCrashFaults", "GO_DST_DIAG_PASS_CHILD", false, "-test.v")
	for _, unwanted := range []string{"simulation diagnostics", "killed goroutines on process exit", "captured bubble stdio", "quiet-on-pass"} {
		if strings.Contains(passOut, unwanted) {
			t.Errorf("passing -v test emitted %q:\n%s", unwanted, passOut)
		}
	}

	crashOut := runDiagnosticsChild(t, "TestDiagnosticsSilentOnPassAndIntentionalCrashFaults", "GO_DST_DIAG_CRASH_FAULT_CHILD", true)
	if !strings.Contains(crashOut, `host="n1" proc="daemon"`) {
		t.Errorf("missing normal process-exit kill on daemon:\n%s", crashOut)
	}
	if strings.Contains(crashOut, `proc="victim"`) || strings.Contains(crashOut, `host="n2"`) {
		t.Errorf("intentional Crash/CrashHost/Ctl.End polluted exit-kill diagnostics:\n%s", crashOut)
	}
}

func TestDiagnosticsStdioAttributionAndExploreFailure(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_WORLD_STDIO_CHILD") == "1" {
		WorldTest(t, 61, Options{}, []HostDecl{
			{
				Name: "alpha",
				Boot: func() {
					Process("raft", func() {
						fmt.Print("term=1 ")
						fmt.Println("elected")
						os.Stderr.WriteString("warn: slow disk\n")
					})
				},
			},
			{
				Name: "beta",
				Boot: func() {
					Process("store", func() {
						syscall.Write(1, []byte("compaction start\n"))
						raw := []byte("err: corrupt block\n")
						syscall.Syscall(syscall.SYS_WRITE, 2, uintptr(unsafe.Pointer(&raw[0])), uintptr(len(raw)))
					})
				},
			},
		}, func(t *testing.T, ctl *Ctl) {
			fmt.Println("driver step 1")
			t.Fatal("consensus mismatch")
		})
		return
	}

	if os.Getenv("GO_DST_DIAG_EXPLORE_CHILD") == "1" {
		ExploreTest(t, 67, ExploreOptions{Mode: Exhaustive, MaxSchedules: 4}, func() bool {
			Host("node-a", HostConfig{}, func() {
				Process("txn", func() {
					ready := make(chan struct{})
					block := make(chan struct{})
					go func() {
						close(ready)
						<-block
					}()
					<-ready
					fmt.Println("aborting uncommitted intent")
				})
			})
			return true
		})
		return
	}

	worldOut := runDiagnosticsChild(t, "TestDiagnosticsStdioAttributionAndExploreFailure", "GO_DST_DIAG_WORLD_STDIO_CHILD", true)
	for _, want := range []string{
		"--- simulation diagnostics (seed 61) ---",
		"captured bubble stdio:",
		`[host="alpha" proc="raft"`,
		"stdout]",
		"    term=1 elected",
		"stderr]",
		"    warn: slow disk",
		`[host="beta" proc="store"`,
		"    compaction start",
		"    err: corrupt block",
		"[root pid=",
		"    driver step 1",
	} {
		if !strings.Contains(worldOut, want) {
			t.Errorf("WorldTest failure output missing %q:\n%s", want, worldOut)
		}
	}

	exploreOut := runDiagnosticsChild(t, "TestDiagnosticsStdioAttributionAndExploreFailure", "GO_DST_DIAG_EXPLORE_CHILD", true)
	for _, want := range []string{
		"simulation.Replay(67,",
		"killed goroutines on process exit:",
		`host="node-a" proc="txn"`,
		"captured bubble stdio:",
		"aborting uncommitted intent",
	} {
		if !strings.Contains(exploreOut, want) {
			t.Errorf("ExploreTest failure output missing %q:\n%s", want, exploreOut)
		}
	}
}

func TestDiagnosticsStdioPipeSwapAndInheritPreserved(t *testing.T) {
	hostPath := t.TempDir() + "/granted.txt"
	hostFile, err := os.Create(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hostFile.Close()

	Test(t, 71, func(t *testing.T) {
		capFile, err := InheritFile(hostFile)
		if err != nil {
			t.Fatalf("InheritFile: %v", err)
		}
		defer capFile.Close()
		if _, err := capFile.WriteString("granted-host-write\n"); err != nil {
			t.Fatalf("capFile.WriteString: %v", err)
		}

		if os.Stdout.Fd() != 1 || os.Stderr.Fd() != 2 {
			t.Fatalf("stdio Fd() = (%d, %d), want (1, 2)", os.Stdout.Fd(), os.Stderr.Fd())
		}

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		oldStdout := os.Stdout
		os.Stdout = w
		defer func() { os.Stdout = oldStdout }()
		fmt.Print("via-sim-pipe")
		w.Close()
		os.Stdout = oldStdout

		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(got) != "via-sim-pipe" {
			t.Fatalf("pipe read = (%q, %v), want %q", got, err, "via-sim-pipe")
		}
	})

	gotHost, err := os.ReadFile(hostPath)
	if err != nil || string(gotHost) != "granted-host-write\n" {
		t.Fatalf("granted host file = (%q, %v)", gotHost, err)
	}
}

func TestDiagnosticsStdioScheduleGCAndExploreNeutrality(t *testing.T) {
	runTranscript := func(emitStdio bool) (transcript string, gcDelta uint32) {
		payload := []byte("bubble-stdio-neutrality-line-0123456789abcdef\n")
		Run(73, func() {
			var ms0, ms1 runtime.MemStats
			runtime.ReadMemStats(&ms0)

			var mu sync.Mutex
			var b strings.Builder
			record := func(s string) {
				mu.Lock()
				b.WriteString(s)
				b.WriteByte(';')
				mu.Unlock()
			}

			ch := make(chan int)
			var wg sync.WaitGroup
			for i := range 4 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					ring := make([][]byte, 32)
					for j := range 200 {
						if emitStdio {
							os.Stdout.Write(payload)
							os.Stderr.Write(payload)
						}
						buf := make([]byte, 16<<10)
						buf[0] = byte(i + j)
						ring[j%len(ring)] = buf
						m := map[int]int{i: j, j + 1: i + 3, i + j + 2: i ^ j}
						s := int(ring[(j/2)%len(ring)][0])
						for k, v := range m {
							s += k*7 + v
						}
						select {
						case ch <- s:
						default:
							record(strconv.Itoa(i*1000 + s))
						}
					}
				}(i)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				for v := range ch {
					record("c" + strconv.Itoa(v))
				}
			}()
			wg.Wait()
			close(ch)
			<-done

			runtime.ReadMemStats(&ms1)
			gcDelta = ms1.NumGC - ms0.NumGC
			transcript = b.String()
		})
		return transcript, gcDelta
	}

	quietTranscript, quietGC := runTranscript(false)
	noisyTranscript, noisyGC := runTranscript(true)
	if quietTranscript == "" || quietTranscript != noisyTranscript {
		t.Fatalf("stdio writes perturbed schedule transcript:\n quiet=%.200s\n noisy=%.200s", quietTranscript, noisyTranscript)
	}
	if quietGC == 0 {
		t.Fatalf("neutrality workload did not cross a GC boundary (NumGC delta = 0)")
	}
	if quietGC != noisyGC {
		t.Fatalf("stdio writes perturbed NumGC: quiet=%d noisy=%d", quietGC, noisyGC)
	}

	exploreRun := func(emitStdio bool) ExploreResult {
		msg := []byte("dpor-stdio-line\n")
		return ExploreWith(79, ExploreOptions{Mode: DPOR}, func() bool {
			var x int
			var mu sync.Mutex
			var wg sync.WaitGroup
			for i := range 2 {
				wg.Add(1)
				go func(v int) {
					defer wg.Done()
					if emitStdio {
						os.Stdout.Write(msg)
					}
					mu.Lock()
					x = x*10 + (v + 1)
					mu.Unlock()
				}(i)
			}
			wg.Wait()
			return false
		})
	}

	rQuiet := exploreRun(false)
	rNoisy := exploreRun(true)
	if rQuiet.Schedules != rNoisy.Schedules ||
		rQuiet.Footprint.PeakDecisions != rNoisy.Footprint.PeakDecisions ||
		rQuiet.Footprint.PeakEnabledTotal != rNoisy.Footprint.PeakEnabledTotal ||
		rQuiet.Footprint.PeakEdges != rNoisy.Footprint.PeakEdges ||
		rQuiet.UnattributedRaces != rNoisy.UnattributedRaces ||
		rQuiet.ForeignSched != rNoisy.ForeignSched {
		t.Fatalf("stdio writes perturbed Explore schedule/HB footprint: quiet=%+v noisy=%+v", rQuiet, rNoisy)
	}
}

func TestDiagnosticsTestlogHeapAndLineNeutrality(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_TESTLOG_NEUTRAL_CHILD") == "1" {
		shortKey := "DST_SHORT_ENV_PROBE"
		longPath := "/" + strings.Repeat("deep-dir/", 80) + "probe.db" // > 700B (> 512B stack fast path)
		stopHost := make(chan struct{})
		hostDone := make(chan struct{})
		if os.Getenv("GO_DST_DIAG_TESTLOG_HAMMER_HOST") == "1" {
			go func() {
				defer close(hostDone)
				padKey := "HOST_PAD_" + strings.Repeat("P", 4080)
				megaKey := "HOST_MEGA_" + strings.Repeat("M", 5200)
				for {
					select {
					case <-stopHost:
						return
					default:
					}
					_ = os.Getenv(padKey)
					_ = os.Getenv(megaKey)
					// Yield via a short host timer park so the single P returns to
					// simulation.Run while preemption is disabled.
					time.Sleep(250 * time.Microsecond)
				}
			}()
		} else {
			close(hostDone)
		}

		var gcDelta uint32
		var transcript string
		Run(101, func() {
			var ms0, ms1 runtime.MemStats
			runtime.ReadMemStats(&ms0)

			var mu sync.Mutex
			var b strings.Builder
			ch := make(chan int)
			var wg sync.WaitGroup
			for i := range 3 {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					for j := range 120 {
						_ = os.Getenv(shortKey)
						_, _ = os.Stat(longPath)
						buf := make([]byte, 8<<10)
						buf[0] = byte(id + j)
						select {
						case ch <- int(buf[0]) + j:
						default:
							mu.Lock()
							b.WriteString(strconv.Itoa(id*100 + j))
							b.WriteByte(',')
							mu.Unlock()
						}
					}
				}(i)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				for v := range ch {
					mu.Lock()
					b.WriteString("r" + strconv.Itoa(v) + ",")
					mu.Unlock()
				}
			}()
			wg.Wait()
			close(ch)
			<-done

			runtime.ReadMemStats(&ms1)
			gcDelta = ms1.NumGC - ms0.NumGC
			transcript = b.String()
		})
		close(stopHost)
		<-hostDone
		fmt.Printf("TRANSCRIPT=%d:%s\nGC=%d\n", len(transcript), transcript, gcDelta)
		return
	}

	outNoLog := runDiagnosticsChild(t, "TestDiagnosticsTestlogHeapAndLineNeutrality", "GO_DST_DIAG_TESTLOG_NEUTRAL_CHILD", false)
	logPath := t.TempDir() + "/testlog.txt"
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t),
		"-test.run=^TestDiagnosticsTestlogHeapAndLineNeutrality$",
		"-test.count=1",
		"-test.testlogfile="+logPath,
	)
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env,
		"GO_DST_DIAG_TESTLOG_NEUTRAL_CHILD=1",
		"GO_DST_DIAG_TESTLOG_HAMMER_HOST=1",
	)
	outWithLogBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child with -test.testlogfile failed: %v\n%s", err, outWithLogBytes)
	}
	outWithLog := string(outWithLogBytes)
	if outNoLog != outWithLog {
		t.Fatalf("-test.testlogfile perturbed bubble GC count or schedule:\nwithout log:\n%.240s\nwith log:\n%.240s", outNoLog, outWithLog)
	}

	rawLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(testlog): %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(rawLog), "\n"), "\n")
	if len(lines) < 2 || lines[0] != "# test log" {
		t.Fatalf("invalid testlog header: first line = %q (total lines = %d)", lines[0], len(lines))
	}
	for idx, line := range lines[1:] {
		if !(strings.HasPrefix(line, "getenv ") || strings.HasPrefix(line, "stat ") || strings.HasPrefix(line, "open ") || strings.HasPrefix(line, "chdir ")) {
			t.Fatalf("spliced or malformed testlog line %d: %.160q", idx+2, line)
		}
		// None of our probe paths contain embedded "getenv "/"stat "/"open " after column 0.
		rest := line[strings.IndexByte(line, ' ')+1:]
		for _, op := range []string{"getenv ", "stat /", "open /", "chdir /"} {
			if strings.Contains(rest, op) {
				t.Fatalf("mid-line spliced record at line %d: %.200q", idx+2, line)
			}
		}
	}
}

func TestDiagnosticsStdioTSanFidelity(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_RACE_IOSYNC_CHILD") == "1" {
		Run(83, func() {
			if err := os.WriteFile("/probe", []byte("x"), 0o600); err != nil {
				panic(err)
			}
			f, err := os.Open("/probe")
			if err != nil {
				panic(err)
			}
			defer f.Close()
			vfd := int(f.Fd())

			var shared int
			go func() {
				shared = 1
				_, _ = syscall.Write(1, []byte("ungranted-stdout\n"))
				time.Sleep(10 * time.Millisecond)
			}()
			go func() {
				time.Sleep(1 * time.Millisecond)
				var rbuf [1]byte
				_, _ = syscall.Read(vfd, rbuf[:])
				_ = shared
			}()
			time.Sleep(20 * time.Millisecond)
		})
		return
	}
	if os.Getenv("GO_DST_DIAG_RACE_OS_STDOUT_CHILD") == "1" {
		Run(89, func() {
			buf := []byte("hello-stdout\n")
			go func() {
				_, _ = os.Stdout.Write(buf)
				time.Sleep(10 * time.Millisecond)
			}()
			go func() {
				time.Sleep(1 * time.Millisecond)
				buf[0] = 'X'
			}()
			time.Sleep(20 * time.Millisecond)
		})
		return
	}
	if os.Getenv("GO_DST_DIAG_RACE_RAW_STDOUT_CHILD") == "1" {
		Run(97, func() {
			// Fill the 64 KiB stdio diagnostics buffer first so take == 0 on the
			// subsequent raw write: TSan must still observe the caller's read of buf.
			_, _ = os.Stdout.Write(bytes.Repeat([]byte("f"), 70<<10))
			buf := []byte("overflow-tail\n")
			ptr := uintptr(unsafe.Pointer(&buf[0]))
			n := uintptr(len(buf))
			go func() {
				_, _, _ = syscall.Syscall(syscall.SYS_WRITE, 1, ptr, n)
				runtime.KeepAlive(buf)
				time.Sleep(10 * time.Millisecond)
			}()
			go func() {
				time.Sleep(1 * time.Millisecond)
				buf[0] = 'Z'
			}()
			time.Sleep(20 * time.Millisecond)
		})
		return
	}

	if !dstRaceEnabledFP() {
		t.Skip("requires -race to verify TSan read/ioSync edges on modeled stdio")
	}

	for _, env := range []string{
		"GO_DST_DIAG_RACE_IOSYNC_CHILD",
		"GO_DST_DIAG_RACE_OS_STDOUT_CHILD",
		"GO_DST_DIAG_RACE_RAW_STDOUT_CHILD",
	} {
		out := runDiagnosticsChild(t, "TestDiagnosticsStdioTSanFidelity", env, true)
		if !strings.Contains(out, "WARNING: DATA RACE") {
			t.Fatalf("%s failed without reporting a TSan data race:\n%s", env, out)
		}
	}
}
