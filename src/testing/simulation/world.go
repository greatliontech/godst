// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"fmt"
	"testing"
)

// The declarative topology layer: a World is a declared set of hosts, each
// carrying the Boot function its machine runs at power-on — initially and
// again at every reboot — plus one SCRIPT goroutine that drives the
// experiment (exchanges, fault injection, assertions) through a Ctl handle.
//
// The layer is sugar over the imperative core and composes ONLY existing
// primitives: each declaration is a Host call, a reboot is the same host's
// re-declaration running the same Boot, faults are the package-level fault
// APIs (legal from the script, a scheduled bubble goroutine), and the end of
// the world powers every machine off (CrashHost) so long-lived SUT
// goroutines — server accept loops, tick loops — end the run without
// hand-rolled teardown plumbing: a crashed machine's goroutines are
// descheduled permanently, which the run's exit accepts. Anything the layer
// cannot express drops down to the core without friction; the two tiers
// share every semantic.
//
// The common entries are World (a self-contained run) and WorldTest (the
// testing.T form). StartWorld/Ctl.End are the composition tier: boot the
// same declared topology inside any already-running simulation — most
// usefully an Explore SUT, so a discovery sweep and its pinned-seed
// regression share one topology.

// A HostDecl declares one host of a World: its name, its configuration, and
// the Boot function its machine runs at power-on. Boot is the host's Host
// body: it runs inline at declaration (declare processes and spawn
// long-lived work, then return) and runs AGAIN at every Ctl.RestartHost —
// a reboot boots the same software, against whatever the host's durable
// disk image preserved.
type HostDecl struct {
	Name   string
	Config HostConfig
	Boot   func()
}

// Ctl is the script's handle on a running world.
type Ctl struct {
	decls []HostDecl
	ended bool
}

// World runs a declared topology under seed: every host is declared and
// booted in declaration order, script runs as the experiment's driver — the
// single goroutine faults are injected from — and when script returns the
// world ENDS: every machine powers off in reverse declaration order, so
// long-lived SUT goroutines die with their machines and the run exits
// cleanly. Assertions belong in the script (or in state it captures);
// after World returns, the world is gone.
//
// Faults are the package-level APIs (Partition, CrashHost, FailDisk, …),
// called from the script; Ctl adds what only the declaration layer can
// offer — RestartHost, the reboot that re-runs the declared Boot.
func World(seed uint64, opts Options, hosts []HostDecl, script func(*Ctl)) {
	RunWith(seed, opts, func() {
		ctl := StartWorld(hosts)
		defer ctl.End()
		script(ctl)
	})
}

// WorldTest is World with the testing integration Test provides (failures
// report through t; the script receives it for assertions).
func WorldTest(t *testing.T, seed uint64, opts Options, hosts []HostDecl, script func(*testing.T, *Ctl)) {
	TestWith(t, seed, opts, func(t *testing.T) {
		ctl := StartWorld(hosts)
		defer ctl.End()
		script(t, ctl)
	})
}

// StartWorld declares and boots hosts inside an already-running simulation
// (a Run, Test, or Explore body) and returns the world's Ctl — the
// composition tier under World/WorldTest. The caller owns the world's end:
// defer Ctl.End so the machines power off when the driving code returns —
// from OUTSIDE any declared host's Host body: powering off the machine the
// calling goroutine currently belongs to meets CrashHost's own refusal
// (World/WorldTest cannot hit this — their deferred End runs after every
// Host body has popped). Call it from a scheduled bubble goroutine, as the
// fault APIs require.
func StartWorld(hosts []HostDecl) *Ctl {
	seen := make(map[string]bool, len(hosts))
	for _, d := range hosts {
		if d.Boot == nil {
			panic("testing/simulation: HostDecl " + d.Name + " has no Boot")
		}
		if seen[d.Name] {
			// A duplicate would run its Boot on the already-up machine with
			// no power-on edge — refuse at the declaration, loudly.
			panic("testing/simulation: duplicate HostDecl " + d.Name)
		}
		seen[d.Name] = true
		Host(d.Name, d.Config, d.Boot)
	}
	return &Ctl{decls: append([]HostDecl(nil), hosts...)}
}

// RestartHost reboots the named declared host: the machine comes back up
// (dials reach its kernel again) with its filesystem torn to the durable
// image, and runs its declared Boot — the same software booting against
// what the disk preserved. The host need not have crashed first: restarting
// a live host is the abrupt power-cycle. Panics on a name the world never
// declared, like the fault APIs it composes — and, like End, must be called
// from outside the restarted host's own Host body (CrashHost's refusal).
func (c *Ctl) RestartHost(name string) {
	for _, d := range c.decls {
		if d.Name == name {
			CrashHost(name) // a power-cycle of a live host begins with the power loss; a no-op if already down
			Host(d.Name, d.Config, d.Boot)
			return
		}
	}
	panic("testing/simulation: RestartHost of undeclared host " + name)
}

// End powers off every declared machine in reverse declaration order — the
// end of the world. Long-lived SUT goroutines die with their machines, so
// the surrounding run exits cleanly with no per-test teardown plumbing.
// Idempotent; World/WorldTest call it for you.
func (c *Ctl) End() {
	if c.ended {
		return
	}
	c.ended = true
	for i := len(c.decls) - 1; i >= 0; i-- {
		CrashHost(c.decls[i].Name)
	}
}

// ExploreTest is the discovery sweep's test bridge: it runs sut under seed
// exploration with opts' budgets and reports every failure as a REPLAYABLE
// artifact through t. It skips itself under -short — exploration is
// discovery, off the fast path; the convention (docs/README.md) is that
// gates run pinned-seed Test/TestWith regressions and every failing seed an
// ExploreTest surfaces is promoted to one. Truncated coverage (budgets hit,
// overflow, foreign goroutines) reports through t.Log — visible, never a
// silent cap.
func ExploreTest(t *testing.T, seed uint64, opts ExploreOptions, sut func() bool) {
	t.Helper()
	if testing.Short() {
		t.Skip("exploration sweep: discovery is off the -short path (pin failing seeds as Test regressions)")
	}
	r := ExploreWith(seed, opts, sut)
	failures, notes := formatExploreReport(seed, r)
	for _, msg := range failures {
		t.Errorf("%s", msg)
	}
	for _, n := range notes {
		t.Logf("%s", n)
	}
}

// formatExploreReport renders an ExploreResult as ExploreTest reports it:
// one replayable artifact per failure (the full replay token — Schedule,
// AccessForces, CrashTear, AND ForeignSched, which Replay consumes to
// diagnose prefix divergence correctly on foreign-tainted failures), and
// one note per coverage or oracle condition — each condition independently,
// so no cap or dropped signal is ever silent.
func formatExploreReport(seed uint64, r ExploreResult) (failures, notes []string) {
	for i, f := range r.Failures {
		kind := "assertion"
		switch {
		case f.Race:
			kind = "data race"
		case f.Panic != "":
			kind = "panic: " + f.Panic
		case f.Deadlock != "":
			kind = "deadlock: " + f.Deadlock
		}
		failures = append(failures, fmt.Sprintf("explore seed %d failure %d/%d (%s): replay with\n\tsimulation.Replay(%d, simulation.Failure{Schedule: %#v, AccessForces: %#v, CrashTear: %v, ForeignSched: %v}, sut)",
			seed, i+1, len(r.Failures), kind, seed, f.Schedule, f.AccessForces, f.CrashTear, f.ForeignSched))
	}
	if r.Exhausted {
		notes = append(notes, fmt.Sprintf("explore seed %d: exhausted, %d schedules", seed, r.Schedules))
	} else {
		notes = append(notes, fmt.Sprintf("explore seed %d: %d schedules, coverage NOT exhausted", seed, r.Schedules))
	}
	if r.BudgetHit {
		notes = append(notes, fmt.Sprintf("explore seed %d: schedule budget hit — coverage bounded by ExploreOptions", seed))
	}
	if r.Overflow {
		notes = append(notes, fmt.Sprintf("explore seed %d: per-run trace budget overflow — coverage incomplete", seed))
	}
	if r.Uninstrumented {
		notes = append(notes, fmt.Sprintf("explore seed %d: outcome-relevant dependencies may be invisible to DPOR (Uninstrumented) — coverage claim downgraded", seed))
	}
	if r.ForeignSched {
		notes = append(notes, fmt.Sprintf("explore seed %d: foreign goroutines were scheduled — completeness downgraded (see ExploreResult.ForeignSched)", seed))
	}
	if r.UnattributedRaces > 0 {
		notes = append(notes, fmt.Sprintf("explore seed %d: %d race report(s) could not be attributed to a replayable schedule (UnattributedRaces) — real oracle signals, not represented in Failures", seed, r.UnattributedRaces))
	}
	return failures, notes
}
