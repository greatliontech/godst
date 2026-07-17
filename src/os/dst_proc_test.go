// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os_test

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

func TestDSTProcStatStarttimeAndNamespace(t *testing.T) {
	realPID := os.Getpid()
	simPID := realPID + 100000
	var rootStat, selfStat, dottedStat, procStat []byte
	var rootErr, selfErr, dottedErr, procErr error
	var rootInfo os.FileInfo
	var statErr error
	var rootNS, procNS string
	var rootNSErr, procNSErr error
	var procPID, proc2PID int
	var proc2Stat []byte
	var proc2Err error
	var signedErr, dotLeafErr, unsupportedProcWalkErr, deadErr, hostErr, dottedHostErr, unsupportedErr, unsupportedProcReadlinkWalkErr error
	var mkdirProcErr, trailingMkdirErr, aliasWriteErr, unsupportedWriteErr error
	var openRootProcErr error
	var rootRootStat []byte
	var rootReadErr, rootDotLeafErr, rootUnsupportedProcWalkErr, rootReadlinkErr, rootUnsupportedProcReadlinkWalkErr error
	var rootMkdirAllErr, rootMkdirAllAliasErr, rootMkdirAllThroughProcErr, rootWriteErr, rootAliasWriteErr error
	var rootReadlink string
	var procDirCreated bool

	simulation.RunWith(1, simulation.Options{PID: simPID}, func() {
		rootStat, rootErr = os.ReadFile("/proc/" + strconv.Itoa(simPID) + "/stat")
		selfStat, selfErr = os.ReadFile("/proc/self/stat")
		dottedStat, dottedErr = os.ReadFile("/proc/" + strconv.Itoa(simPID) + "/./stat")
		rootInfo, statErr = os.Stat("/proc/self/stat")
		rootNS, rootNSErr = os.Readlink("/proc/self/ns/pid")

		simulation.Process("p", func() {
			procPID = os.Getpid()
			procStat, procErr = os.ReadFile("/proc/self/stat")
			procNS, procNSErr = os.Readlink("/proc/self/ns/pid")
		})
		// The restart 150ms later starts on the ALREADY-booted implicit host:
		// its starttime is 15 USER_HZ ticks of host uptime — the boot-relative
		// contract, distinguishable from zero and from any pid-derived value.
		time.Sleep(150 * time.Millisecond)
		simulation.Process("p", func() {
			proc2PID = os.Getpid()
			proc2Stat, proc2Err = os.ReadFile("/proc/self/stat")
		})
		_, signedErr = os.ReadFile("/proc/+" + strconv.Itoa(simPID) + "/stat")
		_, dotLeafErr = os.ReadFile("/proc/self/stat/.")
		_, unsupportedProcWalkErr = os.ReadFile("/proc/self/not-a-dir/../stat")
		_, deadErr = os.ReadFile("/proc/" + strconv.Itoa(procPID) + "/stat")
		_, hostErr = os.ReadFile("/proc/" + strconv.Itoa(realPID) + "/stat")
		_, dottedHostErr = os.ReadFile("/proc/" + strconv.Itoa(realPID) + "/./stat")
		_, unsupportedErr = os.Readlink("/proc/self/ns/mnt")
		_, unsupportedProcReadlinkWalkErr = os.Readlink("/proc/self/ns/mnt/../pid")
		mkdirProcErr = os.Mkdir("/proc", 0o755)
		trailingMkdirErr = os.Mkdir("/proc/", 0o755)
		if err := os.Mkdir("/missing", 0o755); err != nil {
			t.Fatalf("Mkdir(/missing): %v", err)
		}
		aliasWriteErr = os.WriteFile("/missing/../proc/self/stat", []byte("fake\n"), 0o644)
		unsupportedWriteErr = os.WriteFile("/proc/self/ns/mnt", []byte("fake\n"), 0o644)
		_, openRootProcErr = os.OpenRoot("/proc")

		root, err := os.OpenRoot("/")
		if err != nil {
			t.Fatalf("OpenRoot(/): %v", err)
		}
		defer root.Close()
		rootRootStat, rootReadErr = root.ReadFile("proc/self/stat")
		_, rootDotLeafErr = root.ReadFile("proc/self/stat/.")
		_, rootUnsupportedProcWalkErr = root.ReadFile("proc/self/not-a-dir/../stat")
		rootReadlink, rootReadlinkErr = root.Readlink("proc/self/ns/pid")
		_, rootUnsupportedProcReadlinkWalkErr = root.Readlink("proc/self/ns/mnt/../pid")
		rootMkdirAllErr = root.MkdirAll("proc/self", 0o755)
		rootMkdirAllAliasErr = root.MkdirAll("missing-mkdirall/../proc", 0o755)
		rootMkdirAllThroughProcErr = root.MkdirAll("proc/../ok", 0o755)
		entries, err := os.ReadDir("/")
		if err != nil {
			t.Fatalf("ReadDir(/): %v", err)
		}
		for _, entry := range entries {
			if entry.Name() == "proc" {
				procDirCreated = true
			}
		}
		rootWriteErr = root.WriteFile("proc/self/stat", []byte("fake\n"), 0o644)
		if err := root.Mkdir("missing-root", 0o755); err != nil {
			t.Fatalf("Root.Mkdir(missing-root): %v", err)
		}
		rootAliasWriteErr = root.WriteFile("missing-root/../proc/self/stat", []byte("fake\n"), 0o644)
	})

	for name, err := range map[string]error{
		"root stat":      rootErr,
		"self stat":      selfErr,
		"dotted stat":    dottedErr,
		"root read stat": rootReadErr,
		"stat metadata":  statErr,
		"root namespace": rootNSErr,
		"proc stat":      procErr,
		"proc namespace": procNSErr,
		"root readlink":  rootReadlinkErr,
	} {
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	rootFields := procStatFields(t, rootStat)
	selfFields := procStatFields(t, selfStat)
	dottedFields := procStatFields(t, dottedStat)
	procFields := procStatFields(t, procStat)
	rootRootFields := procStatFields(t, rootRootStat)
	if rootFields[0] != strconv.Itoa(simPID) || selfFields[0] != strconv.Itoa(simPID) {
		t.Fatalf("root proc pid fields = %q/%q, want %d", rootFields[0], selfFields[0], simPID)
	}
	if dottedFields[0] != strconv.Itoa(simPID) {
		t.Fatalf("dotted proc pid field = %q, want %d", dottedFields[0], simPID)
	}
	if rootRootFields[0] != strconv.Itoa(simPID) {
		t.Fatalf("Root proc pid field = %q, want %d", rootRootFields[0], simPID)
	}
	if proc2Err != nil {
		t.Fatalf("restarted process stat: %v", proc2Err)
	}
	proc2Fields := procStatFields(t, proc2Stat)
	// Field-22 starttime is USER_HZ (100) ticks of the OWNING host's uptime at
	// process start: the root process and the implicit host's first process
	// both started at their machine's boot (0 — an implicit host boots at its
	// first declaration), and the restart 150ms later reads 15 ticks.
	if rootFields[21] != "0" || selfFields[21] != rootFields[21] || dottedFields[21] != rootFields[21] || rootRootFields[21] != rootFields[21] {
		t.Fatalf("root proc starttime fields = %q/%q/%q/%q, want 0 (started at the machine's boot)", rootFields[21], selfFields[21], dottedFields[21], rootRootFields[21])
	}
	if procFields[0] != strconv.Itoa(procPID) || procFields[21] != "0" {
		t.Fatalf("process proc fields pid/start = %q/%q, want %d/0 (the implicit host boots with its first process)", procFields[0], procFields[21], procPID)
	}
	if proc2PID == procPID {
		t.Fatalf("restart pid = %d, want a fresh pid (got the predecessor's)", proc2PID)
	}
	if proc2Fields[0] != strconv.Itoa(proc2PID) || proc2Fields[21] != "15" {
		t.Fatalf("restarted process fields pid/start = %q/%q, want %d/15 (15 USER_HZ ticks of host uptime at start)", proc2Fields[0], proc2Fields[21], proc2PID)
	}
	if rootNS != "pid:[1]" || procNS != rootNS {
		t.Fatalf("pid namespace readlink = root %q process %q, want stable pid:[1]", rootNS, procNS)
	}
	if rootReadlink != rootNS {
		t.Fatalf("Root pid namespace readlink = %q, want %q", rootReadlink, rootNS)
	}
	if rootInfo.Size() != 0 || rootInfo.Mode() != 0o444 {
		t.Fatalf("/proc/self/stat info size/mode = %d/%v, want 0/0444", rootInfo.Size(), rootInfo.Mode())
	}
	for name, err := range map[string]error{
		"signed pid":            signedErr,
		"dot leaf":              dotLeafErr,
		"unsupported proc walk": unsupportedProcWalkErr,
		"dead process pid":      deadErr,
		"host real pid":         hostErr,
		"dotted host pid":       dottedHostErr,
	} {
		if !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("ReadFile(%s proc stat) = %v, want ENOENT", name, err)
		}
	}
	if !isDSTUnsupportedFS(unsupportedErr) {
		t.Fatalf("Readlink(unsupported proc namespace) = %v, want deterministic unsupported", unsupportedErr)
	}
	if !isDSTUnsupportedFS(unsupportedProcReadlinkWalkErr) {
		t.Fatalf("Readlink(unsupported proc walk) = %v, want deterministic unsupported", unsupportedProcReadlinkWalkErr)
	}
	if !isDSTUnsupportedFS(mkdirProcErr) {
		t.Fatalf("Mkdir(/proc) = %v, want deterministic unsupported", mkdirProcErr)
	}
	if !isDSTUnsupportedFS(trailingMkdirErr) {
		t.Fatalf("Mkdir(/proc/) = %v, want deterministic unsupported", trailingMkdirErr)
	}
	if !isDSTUnsupportedFS(openRootProcErr) {
		t.Fatalf("OpenRoot(/proc) = %v, want deterministic unsupported", openRootProcErr)
	}
	if !errors.Is(aliasWriteErr, syscall.EACCES) {
		t.Fatalf("WriteFile(proc alias) = %v, want EACCES (write access to a proc stat is refused before any write)", aliasWriteErr)
	}
	if !errors.Is(unsupportedWriteErr, syscall.ENOENT) {
		t.Fatalf("WriteFile(unsupported proc path) = %v, want ENOENT", unsupportedWriteErr)
	}
	if !isDSTUnsupportedFS(rootMkdirAllErr) {
		t.Fatalf("Root.MkdirAll(proc path) = %v, want deterministic unsupported", rootMkdirAllErr)
	}
	if !isDSTUnsupportedFS(rootMkdirAllAliasErr) {
		t.Fatalf("Root.MkdirAll(proc alias) = %v, want deterministic unsupported", rootMkdirAllAliasErr)
	}
	if !isDSTUnsupportedFS(rootMkdirAllThroughProcErr) {
		t.Fatalf("Root.MkdirAll(through proc) = %v, want deterministic unsupported", rootMkdirAllThroughProcErr)
	}
	if procDirCreated {
		t.Fatalf("Root.MkdirAll(through proc) created mutable /proc directory")
	}
	if !errors.Is(rootDotLeafErr, syscall.ENOENT) {
		t.Fatalf("Root.ReadFile(proc dot leaf) = %v, want ENOENT", rootDotLeafErr)
	}
	if !errors.Is(rootUnsupportedProcWalkErr, syscall.ENOENT) {
		t.Fatalf("Root.ReadFile(unsupported proc walk) = %v, want ENOENT", rootUnsupportedProcWalkErr)
	}
	if !isDSTUnsupportedFS(rootUnsupportedProcReadlinkWalkErr) {
		t.Fatalf("Root.Readlink(unsupported proc walk) = %v, want deterministic unsupported", rootUnsupportedProcReadlinkWalkErr)
	}
	if !errors.Is(rootWriteErr, syscall.EACCES) {
		t.Fatalf("Root.WriteFile(proc stat) = %v, want EACCES (write access to a proc stat is refused before any write)", rootWriteErr)
	}
	if !errors.Is(rootAliasWriteErr, syscall.EACCES) {
		t.Fatalf("Root.WriteFile(proc alias) = %v, want EACCES (write access to a proc stat is refused before any write)", rootAliasWriteErr)
	}
}

func procStatFields(t *testing.T, data []byte) []string {
	t.Helper()
	fields := strings.Fields(string(data))
	if len(fields) < 22 {
		t.Fatalf("proc stat %q has %d fields, want at least 22", data, len(fields))
	}
	return fields
}
