// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
)

// TestDSTExploreReplaysMappingFault: a mapping fault is a pure function of the
// schedule — Explore finds the interleaving where a reader touches a
// reservation page before the grower backs it, and Replay reproduces exactly
// that failure from the recorded schedule. This is the whole mapping stack
// under the replay contract: deterministic addresses, the sigpanic conversion,
// and the crash machinery, none of which may depend on anything but the seed
// and the decision sequence.
func TestDSTExploreReplaysMappingFault(t *testing.T) {
	if dstRaceEnabledFP() {
		// Under -race the explorer works at access granularity (the D5
		// oracle's watchpoints), and exhaustive coverage of even this small
		// body is combinatorial. The replay contract this test pins is
		// leg-independent and fully exercised without the detector.
		t.Skip("exhaustive exploration is access-granular under -race")
	}
	sut := func() bool {
		var died bool
		// The load sink is PER RUN: a package global written by every
		// schedule's reader would itself be a cross-run data race under -race,
		// and the D5 oracle would convert the schedules into Race-kind
		// failures whose replay carries access watchpoints — a different, far
		// slower failure class than the mapping fault this test pins.
		var sink atomic.Uint32
		Host("h", HostConfig{}, func() {
			f, err := os.OpenFile("/db", os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				panic(err)
			}
			f.Write(make([]byte, 4096))

			readerPID := make(chan int, 1)
			readerOK := make(chan struct{})
			growerDone := make(chan struct{})
			go Process("grower", func() {
				defer close(growerDone)
				g, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				defer g.Close()
				if err := g.Truncate(2 * 4096); err != nil {
					panic(err)
				}
			})
			go Process("reader", func() {
				readerPID <- os.Getpid()
				b, err := syscall.Mmap(int(f.Fd()), 0, 2*4096, syscall.PROT_READ, syscall.MAP_SHARED)
				if err != nil {
					panic(err)
				}
				sink.Store(uint32(b[4096])) // faults iff scheduled before the grower's Truncate
				close(readerOK)
			})
			pid := <-readerPID
			<-growerDone
			for range 100 {
				runtime.Gosched()
			}
			select {
			case <-readerOK:
			default:
				died = syscall.Kill(pid, 0) == syscall.ESRCH
			}
			f.Close()
		})
		return died
	}

	res := ExploreWith(1, ExploreOptions{Mode: Exhaustive}, sut)
	// Only assertion-kind failures are this test's subject: the reader's death
	// observed by the sut. Panic/Deadlock kinds would re-panic under Replay,
	// and Race kinds replay with access watchpoints — different contracts.
	var reproduced, subject int
	for _, f := range res.Failures {
		if f.Race || f.Panic != "" || f.Deadlock != "" {
			continue
		}
		subject++
		if failed, _ := Replay(1, f, sut); failed {
			reproduced++
		} else {
			t.Errorf("a recorded mapping-fault failure did not reproduce under Replay")
		}
	}
	if subject == 0 {
		t.Fatalf("Explore found no assertion-kind mapping-fault failure (%d schedules, %d failures)", res.Schedules, len(res.Failures))
	}
	if reproduced == 0 {
		t.Fatalf("no recorded failure reproduced")
	}
}
