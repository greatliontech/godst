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

// An explicit inherited-file capability's host I/O is an outbound,
// schedule-ordered side effect: it must never couple the seeded schedule to
// wall-timed host events. The demonstrated hazard is the entersyscall window
// — a capability operation issued through the Syscall trampoline's
// entersyscall/exitsyscall form exposes the single P's _Psyscall state to a
// host M's exitsyscall-fast-path P reclaim (stale oldp) and to a pending
// stop-the-world, and losing that race sends the returning bubble goroutine
// through exitsyscall's slow path onto the run queue: a reschedule at a
// wall-clock-dependent instant, a same-seed schedule fork. The granted path
// therefore dispatches raw and scheduler-invisible (the Syscall/Syscall6
// trampolines' dstHostIOActive arm), exactly like the framework -v stream.
//
// These pins are the capability analogue of the TestVerboseContended* suite,
// with the window STRETCHED to wall scale: the capability is the write end of
// a host pipe in BLOCKING mode whose read side the parent driver drains
// slowly, so the granted writes spend most of the run blocked inside the
// write syscall — under the windowed form, the staggered sleeper returns
// (each a host M's exitsyscall racing to reclaim the single P) land inside a
// blocked window with high probability and fork the schedule; under the raw
// form a blocked write just holds the P, a wall-time delay that cannot
// reorder the schedule, and four same-seed child runs produce byte-identical
// transcripts.

// dstInheritedWriteScheduleProgram runs the schedule-sensitive simulation
// body with capability writes woven through it: goroutine races decided by
// the seed, per-g map iteration, GC-trigger-crossing allocation, and granted
// blocking host writes interleaved with the scheduling decisions. Returns
// the run's transcript.
func dstInheritedWriteScheduleProgram(t *testing.T, seed uint64, sink *os.File) string {
	var transcript string
	Test(t, seed, func(t *testing.T) {
		capability, err := InheritFile(sink)
		if err != nil {
			t.Fatalf("InheritFile: %v", err)
		}
		defer capability.Close()
		payload := []byte(strings.Repeat("w", 256))
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
					// The granted host write under test: 1600 blocking pipe
					// writes against a slow drain, so the run spends most of
					// its wall time inside the granted write syscall.
					if _, err := capability.Write(payload); err != nil {
						t.Errorf("capability write: %v", err)
						return
					}
					buf := make([]byte, 8<<10)
					buf[0], buf[len(buf)-1] = byte(i), byte(j)
					ring[j%len(ring)] = buf
					sum += int(ring[(j/2)%len(ring)][0])
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
		<-done
		transcript = b.String()
	})
	return transcript
}

// TestInheritedWriteContentionHostSpam is the host-side amplifier, the
// capability twin of TestVerboseContentionHostSpam: staggered goroutines
// blocked in real select(2) sleeps whose wall-timed returns expire across the
// whole run window — each return is a host M's exitsyscall racing to reclaim
// the single P, the exact racer that turns a scheduler-visible syscall window
// on the granted path into a schedule fork — plus a wall-paced logging loop
// keeping the host side allocating and formatting. Child-only.
func TestInheritedWriteContentionHostSpam(t *testing.T) {
	if os.Getenv("GO_DST_INHERITED_CONTENTION_OUT") == "" {
		t.Skip("driver-launched child only")
	}
	t.Parallel()
	var sleepers sync.WaitGroup
	for i := 0; i < 24; i++ {
		sleepers.Add(1)
		go func(i int) {
			defer sleepers.Done()
			// One blocking select(2) sleep per sleeper, entered before the
			// parallel simulation pins GOMAXPROCS(1); durations staggered so
			// the returns land throughout the run window. A sleeper never
			// re-enters a blocking syscall mid-run (that would hold the
			// single gated P for the whole sleep and strangle the run).
			ms := int64(100 + 120*i) // 0.1s .. ~2.9s
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

// TestInheritedWriteContentionSim is the simulation half: it takes the pipe
// write end the driver passed as fd 3, switches it to blocking mode (the
// capability serialization contract's shape: a granted write blocks in the
// syscall, never parks on the poller), inherits it, and runs the
// schedule-sensitive capability-writing program while the host half churns
// in parallel; the run's transcript goes to a host file after the bubble
// exits. Child-only.
func TestInheritedWriteContentionSim(t *testing.T) {
	outf := os.Getenv("GO_DST_INHERITED_CONTENTION_OUT")
	if outf == "" {
		t.Skip("driver-launched child only")
	}
	t.Parallel()
	if err := syscall.SetNonblock(3, false); err != nil {
		t.Fatalf("SetNonblock(3): %v", err)
	}
	sink := os.NewFile(3, "pipe-sink")
	if sink == nil {
		t.Fatal("driver pipe fd 3 missing")
	}
	defer sink.Close()
	transcript := dstInheritedWriteScheduleProgram(t, 41, sink)
	if err := os.WriteFile(outf, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestInheritedWriteContendedSameSeedTranscript: the load-bearing determinism
// pin for granted capability I/O — a bubble goroutine's capability write must
// never couple the seeded schedule to host activity (no scheduler-visible
// entersyscall window, no parking on host-held state). Four child runs of the
// same seed — each writing through a blocking pipe capability the driver
// drains slowly, with a host-parallel test churning wall-timed syscall
// returns — must produce byte-identical simulation transcripts.
func TestInheritedWriteContendedSameSeedTranscript(t *testing.T) {
	if os.Getenv("GO_DST_INHERITED_CONTENTION_OUT") != "" {
		t.Skip("child")
	}
	if testing.Short() {
		t.Skip("-short: skips the 4x~2s contended child sweep")
	}
	testenv.MustHaveExec(t)
	dir := t.TempDir()
	var first string
	for i := 0; i < 4; i++ {
		out := fmt.Sprintf("%s/contended%d", dir, i)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := testenv.Command(t, testenv.Executable(t),
			"-test.run=^TestInheritedWriteContention(HostSpam|Sim)$", "-test.count=1", "-test.parallel=4")
		cmd = testenv.CleanCmdEnv(cmd)
		cmd.Env = append(cmd.Env, "GO_DST_INHERITED_CONTENTION_OUT="+out)
		cmd.ExtraFiles = []*os.File{w} // child fd 3
		// The slow drain that keeps the child's granted writes blocked: pace
		// the pipe so the 400 KiB the run writes take on the order of the
		// run's wall time to pass through the 64 KiB capacity.
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			buf := make([]byte, 16<<10)
			for {
				if _, err := r.Read(buf); err != nil {
					return // EOF once the child (and our dup) closed the write end
				}
				time.Sleep(25 * time.Millisecond)
			}
		}()
		o, err := cmd.CombinedOutput()
		w.Close() // our dup; the child's copy closed with the child
		if err != nil {
			r.Close()
			<-drained
			tail := string(o)
			if len(tail) > 3000 {
				tail = tail[len(tail)-3000:]
			}
			t.Fatalf("contended child %d: %v\n%s", i, err, tail)
		}
		// Drain to EOF, then release the reader.
		<-drained
		r.Close()
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
			t.Errorf("run %d: same-seed transcript differs under capability-write contention\n first=%.200s\n this=%.200s", i, first, b)
		}
	}
}
