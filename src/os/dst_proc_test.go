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
	var procPID int
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
	if rootFields[21] != strconv.Itoa(simPID) || selfFields[21] != rootFields[21] || dottedFields[21] != rootFields[21] || rootRootFields[21] != rootFields[21] {
		t.Fatalf("root proc starttime fields = %q/%q/%q/%q, want deterministic %d", rootFields[21], selfFields[21], dottedFields[21], rootRootFields[21], simPID)
	}
	if procFields[0] != strconv.Itoa(procPID) || procFields[21] != strconv.Itoa(procPID) {
		t.Fatalf("process proc fields pid/start = %q/%q, want %d", procFields[0], procFields[21], procPID)
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
	if aliasWriteErr == nil {
		t.Fatalf("WriteFile(proc alias) succeeded, want proc overlay reserved from mutable tree state")
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
	if rootWriteErr == nil {
		t.Fatalf("Root.WriteFile(proc stat) succeeded, want proc overlay reserved from mutable tree state")
	}
	if rootAliasWriteErr == nil {
		t.Fatalf("Root.WriteFile(proc alias) succeeded, want proc overlay reserved from mutable tree state")
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
