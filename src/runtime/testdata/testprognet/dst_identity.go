// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// DST simulated-identity test fixtures. They live in testprognet, not
// testprog, for the same reason as the DST net fixtures (dst.go here):
// importing os/user links cgo into the program, and a cgo binary disables the
// runtime's deadlock detection, which testprog's crash tests depend on.

package main

import (
	"errors"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"testing/simulation"
)

func init() {
	register("DSTProcessIdentity", DSTProcessIdentity)
	register("DSTIdentityExtra", DSTIdentityExtra)
	register("DSTIdentityGroups", DSTIdentityGroups)
}

// DSTProcessIdentity checks that os.Getpid/os.Hostname return the simulated
// process identity inside simulation.Run (a deterministic default, or the value
// set via Options), and the real machine's identity outside it. Prints
// "def=<pid>/<host> custom=<pid>/<host> restored=<bool> realoverridden=<bool>".
func DSTProcessIdentity() {
	host := func() string { h, _ := os.Hostname(); return h }
	realPID, realHost := os.Getpid(), host()
	var def, custom string
	simulation.Run(1, func() {
		def = strconv.Itoa(os.Getpid()) + "/" + host()
	})
	simulation.RunWith(1, simulation.Options{Hostname: "node7", PID: 4242}, func() {
		custom = strconv.Itoa(os.Getpid()) + "/" + host()
	})
	restored := os.Getpid() == realPID && host() == realHost
	// realoverridden confirms the real identity differs from the simulated default,
	// so def=1/sim is a genuine override and not a coincidence.
	realOverridden := realPID != 1 || realHost != "sim"
	os.Stdout.WriteString("def=" + def + " custom=" + custom +
		" restored=" + strconv.FormatBool(restored) +
		" realoverridden=" + strconv.FormatBool(realOverridden) + "\n")
}

// DSTIdentityExtra checks the rest of the process-identity surface beyond
// pid/hostname: os.Getppid/Getuid/Getgid/Geteuid/Getegid, runtime.NumCPU, and
// os/user.Current return fixed simulated values inside simulation.Run (NumCPU
// overridable via Options), and are restored to real values outside it. Prints
// "inside=[<ppid> <uid> <gid> <euid> <egid> <numcpu> <uid:gid:user:home>]
// customcpu=<n> restoredids=<bool>". restoredids compares the whole identity
// surface read *outside* the run before and after it: equality proves the run
// did not leak simulated identity (and, since the pre-run read caches the real
// os/user, that the in-run synthetic user never poisoned that cache).
func DSTIdentityExtra() {
	read := func() string {
		u, _ := user.Current()
		return strings.Join([]string{
			strconv.Itoa(os.Getppid()),
			strconv.Itoa(os.Getuid()),
			strconv.Itoa(os.Getgid()),
			strconv.Itoa(os.Geteuid()),
			strconv.Itoa(os.Getegid()),
			strconv.Itoa(runtime.NumCPU()),
			u.Uid + ":" + u.Gid + ":" + u.Username + ":" + u.HomeDir,
		}, " ")
	}
	realBefore := read()
	var inside string
	simulation.Run(1, func() { inside = read() })
	// A custom NumCPU unlikely to equal the host's real count proves the override
	// is genuine on any machine (whereas the default 8 could coincide with it).
	var customCPU int
	simulation.RunWith(1, simulation.Options{NumCPU: 3}, func() { customCPU = runtime.NumCPU() })
	realAfter := read()
	os.Stdout.WriteString("inside=[" + inside + "] customcpu=" + strconv.Itoa(customCPU) +
		" restoredids=" + strconv.FormatBool(realAfter == realBefore) + "\n")
}

// DSTIdentityGroups exercises the simulated group list and the minimal
// simulated user/group database: inside a run, os.Getgroups is exactly the
// simulated gid; os/user lookups resolve the simulated user/group by name and
// id and report anything else deterministically unknown; GroupIds of the
// simulated user is its single group. After the run the host values return.
func DSTIdentityGroups() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	hostGroups, _ := os.Getgroups()
	fail := func(msg string) {
		os.Stdout.WriteString(msg + "\n")
	}
	okAll := true
	simulation.Run(n, func() {
		check := func(cond bool, msg string) {
			if !cond {
				okAll = false
				fail(msg)
			}
		}
		gids, err := os.Getgroups()
		check(err == nil && len(gids) == 1 && gids[0] == 7777, "getgroups not [7777]")
		u, err := user.Lookup("sim")
		check(err == nil && u != nil && u.Uid == "7777" && u.HomeDir == "/home/sim", "Lookup(sim) wrong")
		_, err = user.Lookup("nosuchuser")
		var uue user.UnknownUserError
		check(errors.As(err, &uue), "Lookup(nosuchuser) not UnknownUserError")
		u2, err := user.LookupId("7777")
		check(err == nil && u2 != nil && u2.Username == "sim", "LookupId(7777) wrong")
		var uuie user.UnknownUserIdError
		_, err = user.LookupId("1000")
		check(errors.As(err, &uuie), "LookupId(1000) not UnknownUserIdError")
		g, err := user.LookupGroup("sim")
		check(err == nil && g != nil && g.Gid == "7777", "LookupGroup(sim) wrong")
		var uge user.UnknownGroupError
		_, err = user.LookupGroup("wheel")
		check(errors.As(err, &uge), "LookupGroup(wheel) not UnknownGroupError")
		g2, err := user.LookupGroupId("7777")
		check(err == nil && g2 != nil && g2.Name == "sim", "LookupGroupId(7777) wrong")
		if u != nil {
			ids, err := u.GroupIds()
			check(err == nil && len(ids) == 1 && ids[0] == "7777", "GroupIds(sim) wrong")
		}
		other := &user.User{Username: "app", Gid: "1000"}
		ids2, err := other.GroupIds()
		check(err == nil && len(ids2) == 1 && ids2[0] == "1000", "GroupIds(other) not primary gid")
	})
	after, _ := os.Getgroups()
	restored := len(after) == len(hostGroups)
	if restored {
		for i := range after {
			if after[i] != hostGroups[i] {
				restored = false
				break
			}
		}
	}
	if !restored {
		fail("host groups not restored")
		return
	}
	if okAll {
		os.Stdout.WriteString("done\n")
	}
}
