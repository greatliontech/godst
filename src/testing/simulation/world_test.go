// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// worldEchoServer is a HostDecl Boot: a process serving one-byte echoes on
// port 9000 until its machine dies. Reboots re-run it against the host's
// durable disk, appending a boot marker to an fsync'd file.
func worldEchoBoot(boots *int) func() {
	return func() {
		*boots++
		go Process("srv", func() {
			f, err := os.OpenFile("/boots", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				panic(err)
			}
			fmt.Fprintf(f, "boot\n")
			f.Sync()
			f.Close()
			// A file's CREATE is durable only once its parent directory is
			// fsync'd — without this, the first boot's marker dies with the
			// power loss as an unsynced directory entry (the crash
			// contract's own lesson).
			if d, err := os.Open("/"); err == nil {
				d.Sync()
				d.Close()
			}
			ln, err := net.Listen("tcp", ":9000")
			if err != nil {
				panic(err)
			}
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					buf := make([]byte, 1)
					for {
						if _, err := c.Read(buf); err != nil {
							return
						}
						if _, err := c.Write(buf); err != nil {
							return
						}
					}
				}()
			}
		})
	}
}

// worldDial dials with bounded retries: a Boot's listener comes up on its
// own goroutine, and a real client retries through the window where the
// port refuses.
func worldDial(addr string) (net.Conn, error) {
	var err error
	for range 100 {
		var c net.Conn
		if c, err = net.Dial("tcp", addr); err == nil {
			return c, nil
		}
		time.Sleep(time.Millisecond)
	}
	return nil, err
}

// TestDSTWorldBootsFaultsAndEnds: the declarative layer end-to-end — hosts
// boot in order, the script drives exchanges and faults through the same
// package-level APIs, and the world's END powers the machines off so the
// server's accept loop and echo goroutines die with them: World returning
// cleanly (no bubble-deadlock panic over parked SUT goroutines) is itself
// the teardown assertion.
func TestDSTWorldBootsFaultsAndEnds(t *testing.T) {
	var log []string
	serverBoots := 0
	World(1, Options{}, []HostDecl{
		{Name: "a", Boot: worldEchoBoot(&serverBoots)},
		{Name: "b", Boot: func() {}},
	}, func(ctl *Ctl) {
		Host("b", HostConfig{}, func() {
			Process("cli", func() {
				c, err := worldDial(HostIP("a") + ":9000")
				if err != nil {
					panic(err)
				}
				defer c.Close()
				buf := make([]byte, 1)
				c.Write([]byte("x"))
				if _, err := c.Read(buf); err != nil {
					panic(err)
				}
				log = append(log, "echo:"+string(buf))

				Partition("a", "b")
				c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				if _, err := c.Read(buf); err == nil {
					panic("read across the cut succeeded")
				}
				log = append(log, "cut-held")
				Heal("a", "b")
				c.SetReadDeadline(time.Time{})
				c.Write([]byte("y"))
				if _, err := c.Read(buf); err != nil {
					panic(err)
				}
				log = append(log, "healed:"+string(buf))
			})
		})
	})
	want := "echo:x,cut-held,healed:y"
	if got := strings.Join(log, ","); got != want {
		t.Errorf("script log = %q, want %q", got, want)
	}
	if serverBoots != 1 {
		t.Errorf("server booted %d times, want 1", serverBoots)
	}
}

// TestDSTWorldRestartHostRebootsAgainstDurableImage: RestartHost re-runs
// the declared Boot on the power-cycled machine — the old incarnation's
// conns are gone (the survivor's next exchange fails), the fsync'd boot
// marker survives the tear (two markers after two boots), and a fresh dial
// reaches the rebooted server.
func TestDSTWorldRestartHostRebootsAgainstDurableImage(t *testing.T) {
	serverBoots := 0
	var oldConnErr error
	var markers int
	var rebootEcho string
	World(1, Options{}, []HostDecl{
		{Name: "a", Boot: worldEchoBoot(&serverBoots)},
		{Name: "b", Boot: func() {}},
	}, func(ctl *Ctl) {
		var c net.Conn
		Host("b", HostConfig{}, func() {
			Process("cli", func() {
				var err error
				c, err = worldDial(HostIP("a") + ":9000")
				if err != nil {
					panic(err)
				}
				buf := make([]byte, 1)
				c.Write([]byte("x"))
				if _, err := c.Read(buf); err != nil {
					panic(err)
				}
			})
		})

		ctl.RestartHost("a")

		Host("b", HostConfig{}, func() {
			Process("cli2", func() {
				// The old incarnation's conn: the rebooted kernel knows
				// nothing of it — traffic meets its RST.
				c.Write([]byte("z"))
				_, oldConnErr = c.Read(make([]byte, 1))
				c.Close()

				c2, err := worldDial(HostIP("a") + ":9000")
				if err != nil {
					panic(err)
				}
				defer c2.Close()
				buf := make([]byte, 1)
				c2.Write([]byte("r"))
				if _, err := c2.Read(buf); err != nil {
					panic(err)
				}
				rebootEcho = string(buf)
			})
		})
		Host("a", HostConfig{}, func() {
			Process("checker", func() {
				b, err := os.ReadFile("/boots")
				if err != nil {
					panic(err)
				}
				markers = strings.Count(string(b), "boot\n")
			})
		})
	})
	if serverBoots != 2 {
		t.Errorf("Boot ran %d times, want 2 (declaration + RestartHost)", serverBoots)
	}
	if oldConnErr == nil {
		t.Error("old incarnation's conn survived the reboot, want an error")
	}
	if markers != 2 {
		t.Errorf("durable boot markers = %d, want 2 (each boot's fsync'd append survived)", markers)
	}
	if rebootEcho != "r" {
		t.Errorf("post-reboot echo = %q, want r", rebootEcho)
	}
}

// TestDSTWorldDeterministic: two same-seed Worlds produce identical
// transcripts — the layer adds no scheduling or randomness of its own.
func TestDSTWorldDeterministic(t *testing.T) {
	run := func() string {
		var log []string
		boots := 0
		World(7, Options{Network: NetworkConfig{CrossHostLatency: 3 * time.Millisecond}}, []HostDecl{
			{Name: "a", Boot: worldEchoBoot(&boots)},
			{Name: "b", Boot: func() {}},
		}, func(ctl *Ctl) {
			Host("b", HostConfig{}, func() {
				Process("cli", func() {
					c, err := worldDial(HostIP("a") + ":9000")
					if err != nil {
						panic(err)
					}
					defer c.Close()
					buf := make([]byte, 1)
					for _, m := range []string{"1", "2", "3"} {
						start := time.Now()
						c.Write([]byte(m))
						if _, err := c.Read(buf); err != nil {
							panic(err)
						}
						log = append(log, fmt.Sprintf("%s@%v", buf, time.Since(start)))
					}
				})
			})
		})
		return strings.Join(log, ",")
	}
	a, b := run(), run()
	if a != b {
		t.Errorf("same-seed worlds diverged:\n  %s\n  %s", a, b)
	}
	if a == "" {
		t.Error("empty transcript")
	}
}

// TestDSTExploreTestBridge: the discovery bridge runs a StartWorld topology
// under a budgeted exploration and passes when no schedule fails — the
// composition tier sharing one topology between sweeps and pinned
// regressions.
func TestDSTExploreTestBridge(t *testing.T) {
	boots := 0
	ExploreTest(t, 3, ExploreOptions{Mode: DPOR, MaxSchedules: 16}, func() bool {
		ctl := StartWorld([]HostDecl{{Name: "a", Boot: worldEchoBoot(&boots)}})
		defer ctl.End()
		var ok bool
		Host("a", HostConfig{}, func() {
			Process("cli", func() {
				c, err := worldDial("127.0.0.1:9000")
				if err != nil {
					panic(err)
				}
				defer c.Close()
				buf := make([]byte, 1)
				c.Write([]byte("q"))
				_, err = c.Read(buf)
				ok = err == nil && buf[0] == 'q'
			})
		})
		return !ok // a failure iff the echo broke
	})
	if boots == 0 {
		t.Error("the explored SUT never booted its world")
	}
}

// TestDSTWorldDeclarationContractPanics: the loud refusals at the
// declaration layer — a Boot-less decl, a duplicate name, and a restart of
// a name the world never declared.
func TestDSTWorldDeclarationContractPanics(t *testing.T) {
	expectPanic := func(name, want string, f func()) {
		defer func() {
			r := recover()
			if r == nil {
				t.Errorf("%s: no panic, want %q", name, want)
				return
			}
			if msg, ok := r.(string); !ok || !strings.Contains(msg, want) {
				t.Errorf("%s: panic = %v, want containing %q", name, r, want)
			}
		}()
		f()
	}
	Run(1, func() {
		expectPanic("nil Boot", "has no Boot", func() {
			StartWorld([]HostDecl{{Name: "x"}})
		})
		expectPanic("duplicate name", "duplicate HostDecl", func() {
			StartWorld([]HostDecl{
				{Name: "d", Boot: func() {}},
				{Name: "d", Boot: func() {}},
			})
		})
		ctl := StartWorld([]HostDecl{{Name: "e", Boot: func() {}}})
		defer ctl.End()
		expectPanic("undeclared restart", "RestartHost of undeclared host", func() {
			ctl.RestartHost("nope")
		})
	})
}

// TestDSTWorldBootOrder: hosts boot in declaration order — the layer's one
// ordering promise at start-up.
func TestDSTWorldBootOrder(t *testing.T) {
	var order []string
	boot := func(name string) func() {
		return func() { order = append(order, name) }
	}
	World(1, Options{}, []HostDecl{
		{Name: "first", Boot: boot("first")},
		{Name: "second", Boot: boot("second")},
		{Name: "third", Boot: boot("third")},
	}, func(*Ctl) {})
	if got := strings.Join(order, ","); got != "first,second,third" {
		t.Errorf("boot order = %q, want declaration order", got)
	}
}

// TestDSTExploreReportFormat: the bridge's rendering — each failure is a
// replayable artifact carrying the FULL replay token (Schedule,
// AccessForces, CrashTear, and ForeignSched, which Replay consumes for
// divergence diagnosis), and every coverage/oracle condition notes itself
// independently: no silent caps, no dropped signals.
func TestDSTExploreReportFormat(t *testing.T) {
	failures, notes := formatExploreReport(42, ExploreResult{
		Schedules: 7,
		Failures: []Failure{
			{Schedule: []uint64{1, 0, 2}, Race: true, ForeignSched: true, CrashTear: true,
				AccessForces: []AccessForce{{Seq: 3, Count: 9, PCKey: 0x40}}},
			{Schedule: []uint64{0}, Panic: "boom"},
			{Deadlock: "all goroutines blocked"},
		},
		BudgetHit:         true,
		Overflow:          true,
		Uninstrumented:    true,
		ForeignSched:      true,
		UnattributedRaces: 2,
	})
	if len(failures) != 3 {
		t.Fatalf("failure artifacts = %d, want 3", len(failures))
	}
	wantInFailure := [][]string{
		{"data race", "Schedule: []uint64{0x1, 0x0, 0x2}", "CrashTear: true", "ForeignSched: true", "AccessForces", "simulation.Replay(42"},
		{"panic: boom", "ForeignSched: false"},
		{"deadlock: all goroutines blocked"},
	}
	for i, wants := range wantInFailure {
		for _, w := range wants {
			if !strings.Contains(failures[i], w) {
				t.Errorf("failure %d missing %q in:\n%s", i, w, failures[i])
			}
		}
	}
	wantNotes := []string{"NOT exhausted", "budget hit", "overflow", "Uninstrumented", "foreign goroutines", "2 race report(s)"}
	joined := strings.Join(notes, "\n")
	for _, w := range wantNotes {
		if !strings.Contains(joined, w) {
			t.Errorf("notes missing %q in:\n%s", w, joined)
		}
	}
	// The clean result: one exhausted note, nothing else.
	_, clean := formatExploreReport(1, ExploreResult{Schedules: 3, Exhausted: true})
	if len(clean) != 1 || !strings.Contains(clean[0], "exhausted, 3 schedules") {
		t.Errorf("clean-result notes = %q, want the single exhausted line", clean)
	}
}
