// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os_test

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
	"unsafe"
)

const bootIDPath = "/proc/sys/kernel/random/boot_id"

// readBootID reads the boot_id leaf and validates the host kernel's shape:
// canonical lowercase 8-4-4-4-12 UUID text, RFC 4122 version 4, variant 10,
// trailing newline.
func readBootID(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(bootIDPath)
	if err != nil {
		t.Fatalf("read boot_id: %v", err)
	}
	s := string(raw)
	if len(s) != 37 || s[36] != '\n' {
		t.Fatalf("boot_id = %q, want 36-char UUID + newline", s)
	}
	u := s[:36]
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				t.Fatalf("boot_id %q: byte %d = %q, want '-'", u, i, c)
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				t.Fatalf("boot_id %q: byte %d = %q, want lowercase hex", u, i, c)
			}
		}
	}
	if u[14] != '4' {
		t.Fatalf("boot_id %q: version nibble = %q, want '4'", u, u[14])
	}
	if v := u[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Fatalf("boot_id %q: variant nibble = %q, want one of 89ab", u, v)
	}
	return u
}

// TestDSTProcBootID pins the boot_id leaf's surface: a valid per-host UUID —
// stable across re-reads and across co-located processes, different per host —
// with Linux procfs's metadata (size 0, mode 0444, regular-file readlink
// EINVAL, trailing-slash ENOTDIR, read-only), served identically through the
// Root resolver, while unmodeled siblings stay not-exist.
func TestDSTProcBootID(t *testing.T) {
	var rootID, rootID2, hostA1, hostA2, hostB, rootRootID string
	var info, info2, rootInfo, hostBInfo os.FileInfo
	var statErr, stat2Err, rootStatErr, writeErr, slashErr, readlinkErr, siblingErr error
	var statReadlinkErr, deadReadlinkErr, rootReadlinkErr, rootStatReadlinkErr, rootDeadReadlinkErr error
	var nsSlashReadlinkErr, rootNSSlashReadlinkErr, deadSlashStatErr, liveSlashStatErr error

	simulation.Run(1, func() {
		rootID = readBootID(t)
		rootID2 = readBootID(t)
		simulation.Host("a", simulation.HostConfig{}, func() {
			simulation.Process("a1", func() { hostA1 = readBootID(t) })
			simulation.Process("a2", func() { hostA2 = readBootID(t) })
		})
		simulation.Host("b", simulation.HostConfig{}, func() {
			hostB = readBootID(t)
			hostBInfo, _ = os.Stat(bootIDPath)
		})
		info, statErr = os.Stat(bootIDPath)
		info2, stat2Err = os.Stat(bootIDPath)
		writeErr = os.WriteFile(bootIDPath, []byte("x"), 0o644)
		_, slashErr = os.Stat(bootIDPath + "/")
		_, readlinkErr = os.Readlink(bootIDPath)
		_, statReadlinkErr = os.Readlink("/proc/self/stat")
		_, siblingErr = os.ReadFile("/proc/sys/kernel/random/uuid")
		var deadPID int
		simulation.Process("dead", func() { deadPID = os.Getpid() })
		_, deadReadlinkErr = os.Readlink("/proc/" + strconv.Itoa(deadPID) + "/stat")
		// The trailing slash resolves the entry first, as the kernel does:
		// ENOTDIR on an existing leaf, ENOENT on a dead pid's — and on the
		// ns/pid symlink the slash forces the resolver past the link.
		_, nsSlashReadlinkErr = os.Readlink("/proc/self/ns/pid/")
		_, deadSlashStatErr = os.Stat("/proc/" + strconv.Itoa(deadPID) + "/stat/")
		_, liveSlashStatErr = os.Stat("/proc/self/stat/")

		root, err := os.OpenRoot("/")
		if err != nil {
			t.Fatalf("OpenRoot(/): %v", err)
		}
		defer root.Close()
		b, err := root.ReadFile("proc/sys/kernel/random/boot_id")
		if err != nil {
			t.Fatalf("Root.ReadFile(boot_id): %v", err)
		}
		rootRootID = strings.TrimSuffix(string(b), "\n")
		rootInfo, rootStatErr = root.Stat("proc/sys/kernel/random/boot_id")
		_, rootReadlinkErr = root.Readlink("proc/sys/kernel/random/boot_id")
		_, rootStatReadlinkErr = root.Readlink("proc/self/stat")
		_, rootDeadReadlinkErr = root.Readlink("proc/" + strconv.Itoa(deadPID) + "/stat")
		_, rootNSSlashReadlinkErr = root.Readlink("proc/self/ns/pid/")
	})

	if statErr != nil || stat2Err != nil || rootStatErr != nil {
		t.Fatalf("stat boot_id: %v / %v / Root %v", statErr, stat2Err, rootStatErr)
	}
	if rootID != rootID2 {
		t.Fatalf("boot_id re-read = %q, want stable %q", rootID2, rootID)
	}
	if hostA1 != hostA2 {
		t.Fatalf("co-located processes read boot_id %q / %q, want equal", hostA1, hostA2)
	}
	if hostA1 == rootID || hostB == rootID || hostA1 == hostB {
		t.Fatalf("boot_id root=%q a=%q b=%q, want pairwise distinct per host", rootID, hostA1, hostB)
	}
	if rootRootID != rootID {
		t.Fatalf("Root-resolver boot_id = %q, want the plain path's %q", rootRootID, rootID)
	}
	if info.Size() != 0 || info.Mode() != 0o444 || info.Name() != "boot_id" {
		t.Fatalf("boot_id info = size %d mode %v name %q, want 0/0444/boot_id", info.Size(), info.Mode(), info.Name())
	}
	if rootInfo.Name() != "boot_id" || rootInfo.Mode() != 0o444 {
		t.Fatalf("Root boot_id info = mode %v name %q, want 0444/boot_id", rootInfo.Mode(), rootInfo.Name())
	}
	if !os.SameFile(info, info2) {
		t.Fatalf("SameFile(boot_id, boot_id) = false, want true (one procfs inode within a boot)")
	}
	if !errors.Is(writeErr, syscall.EACCES) {
		t.Fatalf("WriteFile(boot_id) = %v, want EACCES", writeErr)
	}
	if !errors.Is(slashErr, syscall.ENOTDIR) {
		t.Fatalf("Stat(boot_id/) = %v, want ENOTDIR", slashErr)
	}
	// The readlink contract is one ladder on both resolvers: a modeled
	// regular leaf answers EINVAL when it exists and its lookup errno when it
	// does not.
	for name, pair := range map[string]struct {
		err  error
		want syscall.Errno
	}{
		"boot_id":            {readlinkErr, syscall.EINVAL},
		"live stat":          {statReadlinkErr, syscall.EINVAL},
		"dead-pid stat":      {deadReadlinkErr, syscall.ENOENT},
		"ns/pid slash":       {nsSlashReadlinkErr, syscall.ENOTDIR},
		"Root boot_id":       {rootReadlinkErr, syscall.EINVAL},
		"Root live stat":     {rootStatReadlinkErr, syscall.EINVAL},
		"Root dead-pid stat": {rootDeadReadlinkErr, syscall.ENOENT},
		"Root ns/pid slash":  {rootNSSlashReadlinkErr, syscall.ENOTDIR},
	} {
		if !errors.Is(pair.err, pair.want) {
			t.Fatalf("Readlink(%s) = %v, want %v", name, pair.err, pair.want)
		}
	}
	if !errors.Is(deadSlashStatErr, syscall.ENOENT) {
		t.Fatalf("Stat(dead-pid stat/) = %v, want ENOENT (the missing entry resolves before the trailing slash matters)", deadSlashStatErr)
	}
	if !errors.Is(liveSlashStatErr, syscall.ENOTDIR) {
		t.Fatalf("Stat(live stat/) = %v, want ENOTDIR", liveSlashStatErr)
	}
	if hostBInfo == nil || os.SameFile(info, hostBInfo) {
		t.Fatalf("SameFile(root boot_id, host b boot_id) = true, want false (two machines never share a procfs inode)")
	}
	if !errors.Is(siblingErr, syscall.ENOENT) {
		t.Fatalf("ReadFile(random/uuid) = %v, want ENOENT (unmodeled sibling)", siblingErr)
	}
}

// TestDSTProcBootIDDeterministicPerSeed pins the derivation contract: the same
// seed yields the same boot_id in every run, and a different seed yields a
// different one — a replayed failure sees the identical boot epoch.
func TestDSTProcBootIDDeterministicPerSeed(t *testing.T) {
	get := func(seed uint64) (root, host string) {
		simulation.Run(seed, func() {
			root = readBootID(t)
			simulation.Host("m", simulation.HostConfig{}, func() { host = readBootID(t) })
		})
		return
	}
	r1, h1 := get(7)
	r2, h2 := get(7)
	r3, h3 := get(8)
	if r1 != r2 || h1 != h2 {
		t.Fatalf("seed 7 replay boot_id root %q/%q host %q/%q, want identical", r1, r2, h1, h2)
	}
	if r3 == r1 || h3 == h1 {
		t.Fatalf("seed 8 boot_id root %q host %q, want different from seed 7's %q/%q", r3, h3, r1, h1)
	}
	if h1 == r1 {
		t.Fatalf("host and root boot_id both %q, want distinct", h1)
	}
}

// TestDSTBootIDReboot pins the boot edge: a process crash/restart and a
// clock-only Host re-declaration of a live machine keep the boot_id; only a
// CrashHost + Host re-declaration (a reboot) regenerates it, freshly per boot.
func TestDSTBootIDReboot(t *testing.T) {
	var boot1, boot1Restart, boot1Live, boot2, boot3 string
	var fiBoot1, fiBoot2 os.FileInfo

	simulation.Run(3, func() {
		simulation.Host("m", simulation.HostConfig{}, func() {
			simulation.Process("w", func() { boot1 = readBootID(t) })
			// A process restart is not a boot.
			simulation.Process("w", func() { boot1Restart = readBootID(t) })
			fiBoot1, _ = os.Stat(bootIDPath)
		})
		// A clock-only re-declaration of a LIVE machine is not a boot.
		simulation.Host("m", simulation.HostConfig{}, func() {
			boot1Live = readBootID(t)
		})
		simulation.CrashHost("m")
		simulation.Host("m", simulation.HostConfig{}, func() {
			simulation.Process("w", func() { boot2 = readBootID(t) })
			fiBoot2, _ = os.Stat(bootIDPath)
		})
		simulation.CrashHost("m")
		simulation.Host("m", simulation.HostConfig{}, func() {
			boot3 = readBootID(t)
		})
	})

	if boot1 == "" || boot1 != boot1Restart || boot1 != boot1Live {
		t.Fatalf("boot 1 ids = %q / restart %q / live re-declare %q, want one stable id", boot1, boot1Restart, boot1Live)
	}
	if boot2 == boot1 {
		t.Fatalf("boot 2 id = %q, want different from boot 1 %q", boot2, boot1)
	}
	if boot3 == boot2 || boot3 == boot1 {
		t.Fatalf("boot 3 id = %q, want different from boots 1 %q and 2 %q", boot3, boot1, boot2)
	}
	if fiBoot1 == nil || fiBoot2 == nil {
		t.Fatalf("boot_id stat across boots: %v / %v, want both non-nil", fiBoot1, fiBoot2)
	}
	if os.SameFile(fiBoot1, fiBoot2) {
		t.Fatalf("SameFile(boot 1 boot_id, boot 2 boot_id) = true, want false (each boot mounts a fresh procfs instance)")
	}
}

// rawUptime reads raw clock_gettime(CLOCK_MONOTONIC) — the per-host uptime
// clock, whose origin a boot stamps.
func rawUptime(t *testing.T) int64 {
	t.Helper()
	var ts syscall.Timespec
	if _, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 1 /* CLOCK_MONOTONIC */, uintptr(unsafe.Pointer(&ts)), 0); e != 0 {
		t.Fatalf("clock_gettime: %v", e)
	}
	return int64(ts.Sec)*1_000_000_000 + int64(ts.Nsec)
}

// TestDSTBootRejectedDeclarationStateNeutral pins the declaration
// state-neutrality contract on the boot edge: a Host re-declaration of a
// powered-off machine that is REJECTED (a pre-epoch clock) boots nothing.
// The boot count is caught by the boot_id comparison against a control run;
// the boot ORIGIN is caught by the uptime probe — the sleep between the
// rejected attempt and the valid declaration separates the two instants, so
// an origin stamped by the rejected attempt reads as nonzero uptime at boot.
func TestDSTBootRejectedDeclarationStateNeutral(t *testing.T) {
	// A skew that takes the wall before the epoch: the bubble base is
	// 2000-01-01, so -100 years is far past it.
	preEpoch := simulation.Skew(-100 * 365 * 24 * time.Hour)
	run := func(withRejected bool) (boot1, boot2 string, uptime2 int64) {
		simulation.Run(13, func() {
			simulation.Host("m", simulation.HostConfig{}, func() { boot1 = readBootID(t) })
			simulation.CrashHost("m")
			if withRejected {
				func() {
					defer func() {
						if recover() == nil {
							t.Fatalf("pre-epoch clock re-declaration did not panic")
						}
					}()
					simulation.Host("m", simulation.HostConfig{Clock: preEpoch}, func() {})
				}()
			}
			time.Sleep(7 * time.Second)
			simulation.Host("m", simulation.HostConfig{}, func() {
				uptime2 = rawUptime(t)
				boot2 = readBootID(t)
			})
		})
		return
	}
	b1Control, b2Control, upControl := run(false)
	b1Rejected, b2Rejected, upRejected := run(true)
	if b1Control != b1Rejected {
		t.Fatalf("boot 1 ids diverge: %q vs %q, want identical replay", b1Control, b1Rejected)
	}
	if b2Control != b2Rejected {
		t.Fatalf("rejected declaration mutated boot state: boot 2 id %q with the rejected attempt, %q without", b2Rejected, b2Control)
	}
	if b2Control == b1Control {
		t.Fatalf("boot 2 id = %q, want different from boot 1", b2Control)
	}
	if upControl != 0 || upRejected != 0 {
		t.Fatalf("uptime at reboot = control %d / rejected %d, want 0/0 (a rejected declaration must not stamp the boot origin)", upControl, upRejected)
	}
}

// TestDSTProcessRestartPoweredOffHostRefused pins the powered-off refusal on
// the simulation-owned state (this binary's behavior does not depend on net
// being linked): restarting a process whose implicit host lost power panics,
// naming the fix; the Host re-declaration then boots the machine with a fresh
// boot_id and admits the restart.
func TestDSTProcessRestartPoweredOffHostRefused(t *testing.T) {
	var before, after string
	var refusal any

	simulation.Run(5, func() {
		simulation.Process("p", func() { before = readBootID(t) })
		simulation.CrashHost("p")
		func() {
			defer func() { refusal = recover() }()
			simulation.Process("p", func() {})
		}()
		simulation.Host("p", simulation.HostConfig{}, func() {
			simulation.Process("p", func() { after = readBootID(t) })
		})
	})

	if refusal == nil || !strings.Contains(fmt.Sprint(refusal), "powered off") {
		t.Fatalf("Process restart on powered-off host: recovered %v, want the powered-off refusal", refusal)
	}
	if before == "" || after == "" || after == before {
		t.Fatalf("boot_id before %q / after reboot %q, want distinct non-empty ids", before, after)
	}
}
