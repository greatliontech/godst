// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"fmt"
	"internal/testenv"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The -v (chatty) test output stream is the testing FRAMEWORK's own host
// plumbing: under a run its writes execute on bubble goroutines (t.Log
// routing, status lines) and carry the framework-stream host-I/O grant, so
// they pass the stdio fence — while a SUT write to os.Stdout stays fenced
// exactly as without -v. These are cross-process tests: chatty mode is a
// property of the test binary's own flags, so a child re-execution of this
// binary with -test.v is the real -v environment, not a reconstruction.

// runVerboseChild re-runs this test binary with -test.v targeting run, with
// env set, and returns the combined output. The caller asserts on it.
func runVerboseChild(t *testing.T, run, env string) string {
	t.Helper()
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^"+run+"$", "-test.v", "-test.count=1")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, env+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("verbose child %s failed: %v\n%s", run, err, out)
	}
	return string(out)
}

// TestVerboseLogStreamsFromBubble: `go test -v` on a simulation test streams
// t.Log lines written INSIDE the bubble — including from spawned bubble
// goroutines across fake-clock sleeps — and the test passes. (Before the
// framework-stream grant this child panicked: the chatty printer's write to
// fd 1 tripped the raw-syscall fence.)
func TestVerboseLogStreamsFromBubble(t *testing.T) {
	if os.Getenv("GO_DST_VERBOSE_LOG_CHILD") == "1" {
		Test(t, 11, func(t *testing.T) {
			t.Log("bubble-log-main")
			done := make(chan struct{})
			go func() {
				time.Sleep(time.Second) // fake clock
				t.Log("bubble-log-worker")
				close(done)
			}()
			<-done
			t.Log("bubble-log-done")
		})
		return
	}
	out := runVerboseChild(t, "TestVerboseLogStreamsFromBubble", "GO_DST_VERBOSE_LOG_CHILD")
	for _, want := range []string{"bubble-log-main", "bubble-log-worker", "bubble-log-done", "--- PASS: TestVerboseLogStreamsFromBubble"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose child output missing %q:\n%s", want, out)
		}
	}
}

// TestVerboseSUTStdioStaysFenced: the framework-stream grant is scoped to the
// chatty printer — under -v a SUT write to os.Stdout from inside the bubble
// still panics with the raw-syscall fence shape, and the escape payload never
// reaches the host stream.
func TestVerboseSUTStdioStaysFenced(t *testing.T) {
	if os.Getenv("GO_DST_VERBOSE_FENCE_CHILD") == "1" {
		Test(t, 13, func(t *testing.T) {
			t.Log("before-sut-write")
			var v any
			func() {
				defer func() { v = recover() }()
				os.Stdout.Write([]byte("sut-stdout-escape\n"))
			}()
			s, ok := v.(string)
			if !ok || !strings.Contains(s, "unsupported under deterministic simulation") {
				t.Fatalf("SUT os.Stdout.Write under -v = %v, want the fence panic", v)
			}
			t.Log("after-sut-write")
		})
		return
	}
	out := runVerboseChild(t, "TestVerboseSUTStdioStaysFenced", "GO_DST_VERBOSE_FENCE_CHILD")
	if strings.Contains(out, "sut-stdout-escape") {
		t.Errorf("SUT stdout write reached the host stream under -v:\n%s", out)
	}
	for _, want := range []string{"before-sut-write", "after-sut-write", "--- PASS: TestVerboseSUTStdioStaysFenced"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose child output missing %q:\n%s", want, out)
		}
	}
}

// TestVerboseSameSeedTranscript: the chatty printer's in-bubble host writes
// are outbound, schedule-ordered side effects — they must not perturb the
// seeded schedule. The child runs a scheduling-sensitive program (goroutine
// races decided by the seed, per-g RNG draws, map iteration) interleaved with
// t.Log streaming, and prints one transcript line; two -v runs of the same
// seed must produce byte-identical transcripts.
func TestVerboseSameSeedTranscript(t *testing.T) {
	if os.Getenv("GO_DST_VERBOSE_TRANSCRIPT_CHILD") == "1" {
		Test(t, 17, func(t *testing.T) {
			var mu sync.Mutex
			var b strings.Builder
			emit := func(s string) { mu.Lock(); b.WriteString(s); b.WriteByte(';'); mu.Unlock() }
			ch := make(chan int)
			var wg sync.WaitGroup
			for i := 0; i < 5; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					t.Logf("worker %d up", i) // streams to the host under -v
					m := map[int]int{}
					for k := 0; k < 5; k++ {
						m[k] = k * i
					}
					sum := 0
					for k, v := range m { // map iteration order — DST-seeded
						sum += k*100 + v
					}
					ch <- sum
					emit(fmt.Sprintf("w%d=%d", i, sum))
				}(i)
			}
			go func() {
				tot := 0
				for i := 0; i < 5; i++ {
					tot += <-ch
					t.Logf("collected %d", i) // interleaved framework writes
				}
				emit(fmt.Sprintf("tot=%d", tot))
			}()
			wg.Wait()
			t.Log("DSTTRANSCRIPT " + b.String())
		})
		return
	}
	extract := func(out string) string {
		for _, line := range strings.Split(out, "\n") {
			if i := strings.Index(line, "DSTTRANSCRIPT "); i >= 0 {
				return line[i:]
			}
		}
		t.Fatalf("no transcript line in child output:\n%s", out)
		return ""
	}
	a := extract(runVerboseChild(t, "TestVerboseSameSeedTranscript", "GO_DST_VERBOSE_TRANSCRIPT_CHILD"))
	b := extract(runVerboseChild(t, "TestVerboseSameSeedTranscript", "GO_DST_VERBOSE_TRANSCRIPT_CHILD"))
	if a != b {
		t.Errorf("same seed, -v on: transcripts differ\n a=%s\n b=%s", a, b)
	}
}

// dstVerboseScheduleProgram is the shared schedule-sensitive simulation body
// for the transcript-equality pins: goroutine races decided by the seed,
// per-g map iteration, and t.Log streaming interleaved with the scheduling
// decisions. It deliberately allocates enough (8 workers x 200 iterations x
// 8 KiB retained in per-worker rings — roughly 13 MB and 1600 logged lines)
// to cross the deterministic GC trigger several times mid-run, so an
// alloc-coupling regression in the -v path that only manifests across
// trigger crossings shifts the transcript. Returns the run's transcript.
func dstVerboseScheduleProgram(t *testing.T, seed uint64) string {
	var transcript string
	Test(t, seed, func(t *testing.T) {
		var mu sync.Mutex
		var b strings.Builder
		emit := func(s string) { mu.Lock(); b.WriteString(s); b.WriteByte(';'); mu.Unlock() }
		ch := make(chan int)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ring := make([][]byte, 64)
				sum := 0
				for j := 0; j < 200; j++ {
					t.Logf("sim worker %d iter %d", i, j) // the framework-stream write under test
					// The GC-crossing pressure: an 8 KiB retained allocation
					// per iteration, content derived from schedule-visible
					// state so the ring cannot be optimized away.
					buf := make([]byte, 8<<10)
					buf[0], buf[len(buf)-1] = byte(i), byte(j)
					ring[j%len(ring)] = buf
					sum += int(ring[(j/2)%len(ring)][0]) // slot j/2 (mod ring) is always filled by iteration j
					m := map[int]int{i: j, j: i, i + j: i * j}
					s := 0
					for k, v := range m { // map iteration order — DST-seeded
						s += k ^ v
					}
					select {
					case ch <- s:
					default:
						emit(fmt.Sprintf("d%d.%d=%d", i, j, s))
					}
				}
				emit(fmt.Sprintf("r%d=%d", i, sum))
			}(i)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				s, ok := <-ch
				if !ok {
					return
				}
				emit(fmt.Sprintf("c=%d", s))
			}
		}()
		wg.Wait()
		close(ch)
		<-done // join the collector: its last emit must happen-before the read
		transcript = b.String()
	})
	return transcript
}

// TestVerboseAttributionUnderContention: every bubble -v line must attribute
// to the simulation test that logged it, even with a host-parallel test
// logging into the same stream — the bubble leg's unconditional "=== NAME"
// header, emitted in the same atomic chunk as its payload, guarantees it.
// The child runs in test2json marker mode (the parse-sensitive shape); the
// driver replays test2json's context rule (RUN/NAME/CONT lines switch the
// current test) over the raw stream and asserts every sim line lands under
// the sim test.
//
// Coverage bound: with the raw scheduler-invisible bubble write, the bubble
// effectively holds the single P for the whole run, so host lines interleave
// only around run-boundary syscall returns — the contention pins exercise
// little MID-run line interleaving by construction. The adjacency assertion
// below stays non-vacuous regardless: a conditional-header regression emits
// one header per 1600-line block and fails it outright.
func TestVerboseAttributionUnderContention(t *testing.T) {
	if os.Getenv("GO_DST_VERBOSE_CONTENTION_OUT") != "" {
		t.Skip("child")
	}
	testenv.MustHaveExec(t)
	dir := t.TempDir()
	cmd := testenv.Command(t, testenv.Executable(t),
		"-test.run=^TestVerboseContention(HostSpam|Sim)$", "-test.count=1", "-test.v=test2json", "-test.parallel=4")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, "GO_DST_VERBOSE_CONTENTION_OUT="+dir+"/ignored")
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		t.Fatalf("attribution child: %v\n%s", err, tail)
	}
	current := ""
	prev := ""
	simLines, misattributed, unheadered := 0, 0, 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimPrefix(line, "\x16")
		for _, prefix := range []string{"=== RUN   ", "=== NAME  ", "=== CONT  ", "=== PAUSE "} {
			if rest, ok := strings.CutPrefix(line, prefix); ok {
				current = strings.TrimSpace(rest)
			}
		}
		if strings.Contains(line, "sim worker") {
			simLines++
			// The consumer-visible property: test2json's context rule lands
			// this line under the sim test.
			if current != "TestVerboseContentionSim" {
				misattributed++
				if misattributed <= 3 {
					t.Errorf("sim line attributed to %q: %s", current, line)
				}
			}
			// The mechanism pin: the bubble leg emits the header and its
			// payload as ONE atomic (<= PIPE_BUF) chunk, so every sim line
			// is IMMEDIATELY preceded by its own header — a whole-line host
			// write cannot land between them. (Context tracking alone would
			// miss a regression to a conditional header: host lines that
			// follow a bubble header carry none of their own, so `current`
			// can stay on the sim test for free.)
			if prev != "=== NAME  TestVerboseContentionSim" {
				unheadered++
				if unheadered <= 3 {
					t.Errorf("sim line not immediately preceded by its own header (prev = %q): %s", prev, line)
				}
			}
		}
		prev = line
	}
	if simLines == 0 {
		t.Fatal("child stream contained no sim lines")
	}
	if misattributed > 0 {
		t.Errorf("%d of %d sim lines misattributed", misattributed, simLines)
	}
	if unheadered > 0 {
		t.Errorf("%d of %d sim lines missing their adjacent header", unheadered, simLines)
	}
}

// TestVerboseContentionHostSpam is the host-side half of the contended
// transcript pin: a t.Parallel test hammering the shared chatty printer
// (and the underlying host stream) with wall-clock-paced writes while the
// simulation half below runs. It also runs staggered SLEEPER goroutines
// blocked in real select(2) sleeps whose returns expire across the whole
// run window: each return is a host M's exitsyscall racing to reclaim the
// single P, multiplying the wall-timed P-reclaim attempts per run from
// around one (run-boundary returns only) to dozens — the exact racer that
// turns any scheduler-visible syscall window on the bubble path into a
// schedule fork, so a regression there is caught with high probability
// rather than a coin flip. Child-only.
func TestVerboseContentionHostSpam(t *testing.T) {
	if os.Getenv("GO_DST_VERBOSE_CONTENTION_OUT") == "" {
		t.Skip("driver-launched child only")
	}
	t.Parallel()
	var sleepers sync.WaitGroup
	for i := 0; i < 24; i++ {
		sleepers.Add(1)
		go func(i int) {
			defer sleepers.Done()
			// ONE blocking select(2) sleep per sleeper (not time.Sleep,
			// which parks on a timer and never exits a syscall), entered
			// immediately — before the parallel simulation pins
			// GOMAXPROCS(1), while spare Ps exist — with staggered durations
			// expiring across the run window. Each return is the P-reclaim
			// attempt; a sleeper never RE-enters a blocking syscall, because
			// an in-run entry would hold the single gated P for the whole
			// sleep and strangle the run.
			ms := int64(100 + 80*i) // 0.1s .. ~1.9s
			tv := syscall.Timeval{Sec: ms / 1000, Usec: (ms % 1000) * 1000}
			syscall.Select(0, nil, nil, nil, &tv)
		}(i)
	}
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		t.Logf("host spam %d %s", i, strings.Repeat("x", 200))
	}
	sleepers.Wait()
}

// TestVerboseContentionSim is the simulation half: it runs the
// schedule-sensitive program under -v while the host half logs in parallel,
// and writes the run's transcript to a host file after the bubble exits.
// Child-only.
func TestVerboseContentionSim(t *testing.T) {
	outf := os.Getenv("GO_DST_VERBOSE_CONTENTION_OUT")
	if outf == "" {
		t.Skip("driver-launched child only")
	}
	t.Parallel()
	transcript := dstVerboseScheduleProgram(t, 29)
	if err := os.WriteFile(outf, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestVerboseContendedSameSeedTranscript: the load-bearing determinism pin
// for the -v bubble path — a bubble goroutine's framework-stream write must
// never couple the seeded schedule to host activity (no parking on
// lastNameMu or the poll layer's fd mutex, no reads of host-shared printer
// state). Four child runs of the same seed under -v, each with a parallel
// host test hammering the shared printer, must produce byte-identical
// simulation transcripts.
func TestVerboseContendedSameSeedTranscript(t *testing.T) {
	if os.Getenv("GO_DST_VERBOSE_CONTENTION_OUT") != "" {
		t.Skip("child")
	}
	if testing.Short() {
		t.Skip("-short: skips the 4x2s contended child sweep")
	}
	testenv.MustHaveExec(t)
	dir := t.TempDir()
	var first string
	for i := 0; i < 4; i++ {
		out := fmt.Sprintf("%s/contended%d", dir, i)
		cmd := testenv.Command(t, testenv.Executable(t),
			"-test.run=^TestVerboseContention(HostSpam|Sim)$", "-test.count=1", "-test.v", "-test.parallel=4")
		cmd = testenv.CleanCmdEnv(cmd)
		cmd.Env = append(cmd.Env, "GO_DST_VERBOSE_CONTENTION_OUT="+out)
		o, err := cmd.CombinedOutput()
		if err != nil {
			tail := string(o)
			if len(tail) > 3000 {
				tail = tail[len(tail)-3000:]
			}
			t.Fatalf("contended child %d: %v\n%s", i, err, tail)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = string(b)
			if first == "" {
				t.Fatal("contended child produced an empty transcript")
			}
		} else if string(b) != first {
			t.Errorf("run %d: same-seed transcript differs under -v cross-test contention\n first=%.200s\n this=%.200s", i, first, b)
		}
	}
}

// TestVerboseOnOffChild runs the schedule-sensitive program and writes its
// transcript to a host file; the driver below runs it once with -v and once
// without. Child-only.
func TestVerboseOnOffChild(t *testing.T) {
	outf := os.Getenv("GO_DST_VERBOSE_ONOFF_OUT")
	if outf == "" {
		t.Skip("driver-launched child only")
	}
	transcript := dstVerboseScheduleProgram(t, 31)
	if err := os.WriteFile(outf, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestVerboseOnOffSameSeedTranscript: flipping -v to diagnose a failure must
// reproduce the SAME run — the framework-stream writes -v adds are outbound
// side effects that consume no scheduler draws and read no host-coupled
// state, so a child run with -v and one without produce byte-identical
// simulation transcripts at the same seed.
func TestVerboseOnOffSameSeedTranscript(t *testing.T) {
	if os.Getenv("GO_DST_VERBOSE_ONOFF_OUT") != "" {
		t.Skip("child")
	}
	testenv.MustHaveExec(t)
	dir := t.TempDir()
	run := func(verbose bool) string {
		out := dir + "/onoff-v-" + fmt.Sprint(verbose)
		args := []string{"-test.run=^TestVerboseOnOffChild$", "-test.count=1"}
		if verbose {
			args = append(args, "-test.v")
		}
		cmd := testenv.Command(t, testenv.Executable(t), args...)
		cmd = testenv.CleanCmdEnv(cmd)
		cmd.Env = append(cmd.Env, "GO_DST_VERBOSE_ONOFF_OUT="+out)
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("onoff child (verbose=%v): %v\n%s", verbose, err, o)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	on, off := run(true), run(false)
	if on == "" {
		t.Fatal("onoff child produced an empty transcript")
	}
	if on != off {
		t.Errorf("same seed, -v on vs off: transcripts differ\n  on=%.200s\n off=%.200s", on, off)
	}
}

// BenchmarkVerboseSimLog is the benchmark half of the bench-arm pin: b.Log
// from inside a simulation bubble routes through writeLine's benchmark
// branch, which must take the granted bubble path like every other
// framework-stream write. Child-only.
func BenchmarkVerboseSimLog(b *testing.B) {
	if os.Getenv("GO_DST_VERBOSE_BENCH_CHILD") == "" {
		b.Skip("driver-launched child only")
	}
	Run(37, func() {
		b.Log("bench-bubble-log")
	})
	for b.Loop() {
	}
}

// TestVerboseBenchLogStreams: `go test -v -bench` with b.Log inside a
// simulation bubble streams the line and passes (the benchmark output branch
// bypasses the chatty printer, so it carries its own granted leg).
func TestVerboseBenchLogStreams(t *testing.T) {
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t),
		"-test.run=^$", "-test.bench=^BenchmarkVerboseSimLog$", "-test.benchtime=1x", "-test.v", "-test.count=1")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, "GO_DST_VERBOSE_BENCH_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bench child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "bench-bubble-log") {
		t.Errorf("bench child output missing the in-bubble b.Log line:\n%s", out)
	}
}
