// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	crand "crypto/rand"
	"encoding/hex"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"testing/simulation"
)

func init() {
	register("DSTIdentityExtra", DSTIdentityExtra)
	register("DSTCryptoRand", DSTCryptoRand)
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

// DSTCryptoRand checks that crypto/rand is deterministic inside simulation.Run
// (seeded by the run) but real OS entropy outside it. With DSTSEED=s it prints
// "h=<hex> eq=<bool> seedvaries=<bool> realdiffers=<bool>": h is the bytes read
// under seed s (stable across processes — replay), eq that a second seed-s run
// matches (same-seed determinism), seedvaries that seed s+1 differs (not a
// constant), and realdiffers that two reads *outside* a run differ (production
// crypto/rand is untouched). This is the executable form of INV-CRYPTO.
func DSTCryptoRand() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	readSeed := func(seed uint64) [32]byte {
		var b [32]byte
		simulation.Run(seed, func() {
			crand.Read(b[:])
		})
		return b
	}
	a := readSeed(n)
	b := readSeed(n)     // same seed: must equal a
	c := readSeed(n + 1) // different seed: must differ
	// Outside any run, crypto/rand is real entropy: two reads differ.
	var x, y [16]byte
	crand.Read(x[:])
	crand.Read(y[:])
	os.Stdout.WriteString("h=" + hex.EncodeToString(a[:]) +
		" eq=" + strconv.FormatBool(a == b) +
		" seedvaries=" + strconv.FormatBool(a != c) +
		" realdiffers=" + strconv.FormatBool(x != y) + "\n")
}
