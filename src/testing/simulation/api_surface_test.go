// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

func didPanicWith(fn func()) (panicked bool, msg string) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			if s, ok := r.(string); ok {
				msg = s
			}
		}
	}()
	fn()
	return
}

func TestAPISurfaceUniformOutsideRun(t *testing.T) {
	// 1. Empty names on Host, HostWith, Process, and ProcessWith are rejected
	// uniformly across both !dst and -tags dst builds.
	for _, tc := range []struct {
		name string
		call func()
	}{
		{"Host", func() { Host("", func() {}) }},
		{"HostWith", func() { HostWith("", HostConfig{}, func() {}) }},
		{"Process", func() { Process("", func() {}) }},
		{"ProcessWith", func() { ProcessWith("", ProcessConfig{}, func() {}) }},
	} {
		panicked, msg := didPanicWith(tc.call)
		if !panicked || !strings.Contains(msg, "must be non-empty") {
			t.Errorf("%s(\"\") outside run: panicked=%v msg=%q, want non-empty name panic", tc.name, panicked, msg)
		}
	}

	// 2. HostFS is available on both !dst and -tags dst builds, validates fs.ValidPath
	// before checking run liveness, and reports fs.ErrClosed outside an active run.
	hfs := HostFS("node1")
	if _, ok := hfs.(fs.StatFS); !ok {
		t.Fatalf("HostFS does not implement fs.StatFS")
	}
	if _, ok := hfs.(fs.ReadDirFS); !ok {
		t.Fatalf("HostFS does not implement fs.ReadDirFS")
	}
	if _, err := hfs.Open("../escape"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("HostFS.Open(\"../escape\") = %v, want fs.ErrInvalid", err)
	}
	if _, err := fs.Stat(hfs, "../escape"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("fs.Stat(HostFS, \"../escape\") = %v, want fs.ErrInvalid", err)
	}
	if _, err := fs.ReadDir(hfs, "../escape"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("fs.ReadDir(HostFS, \"../escape\") = %v, want fs.ErrInvalid", err)
	}
	if _, err := hfs.Open("."); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("HostFS.Open(\".\") outside run = %v, want fs.ErrClosed", err)
	}
	if _, err := fs.Stat(hfs, "."); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("fs.Stat(HostFS, \".\") outside run = %v, want fs.ErrClosed", err)
	}
	if _, err := fs.ReadDir(hfs, "."); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("fs.ReadDir(HostFS, \".\") outside run = %v, want fs.ErrClosed", err)
	}

	// 3. Every <Verb><Target> fault and clock entry point is a uniform no-op outside
	// an active run across both !dst and -tags dst builds, and ExploreOptions /
	// ReplayResult / Failure.Err are part of the build-tag-uniform type surface.
	for _, tc := range []struct {
		name string
		call func()
	}{
		{"CrashHost", func() { CrashHost("h") }},
		{"CrashProcess", func() { CrashProcess("h", "p") }},
		{"Partition", func() { Partition("a", "b") }},
		{"PartitionOneWay", func() { PartitionOneWay("a", "b") }},
		{"PartitionRefuse", func() { PartitionRefuse("a", "b") }},
		{"HealLink", func() { HealLink("a", "b") }},
		{"IsolateHost", func() { IsolateHost("h") }},
		{"UnisolateHost", func() { UnisolateHost("h") }},
		{"ConfigureLink", func() { ConfigureLink("a", "b", LinkConfig{Latency: time.Millisecond, Jitter: time.Millisecond, Bandwidth: 1024}) }},
		{"ConfigureLinkOneWay", func() { ConfigureLinkOneWay("a", "b", LinkConfig{Latency: time.Millisecond}) }},
		{"UnconfigureLink", func() { UnconfigureLink("a", "b") }},
		{"ResetLinkConns", func() { ResetLinkConns("a", "b") }},
		{"ResetProcessConns", func() { ResetProcessConns("h", "p") }},
		{"SlowDisk", func() { SlowDisk("h", time.Millisecond) }},
		{"UnslowDisk", func() { UnslowDisk("h") }},
		{"StepClock", func() { StepClock("h", time.Second) }},
		{"DriftClock", func() { DriftClock("h", 100) }},
	} {
		panicked, msg := didPanicWith(tc.call)
		if panicked {
			t.Errorf("%s outside run panicked: %q, want documented no-op", tc.name, msg)
		}
	}

	var eo ExploreOptions
	eo.Hostname = "uniform"
	var f Failure
	f.Err = "boom"
	var rr ReplayResult
	if rr.Failed() {
		t.Errorf("zero ReplayResult.Failed() = true, want false")
	}
	rr.Err = errors.New(f.Err)
	if !rr.Failed() {
		t.Errorf("ReplayResult{Err: non-nil}.Failed() = false, want true")
	}

	// 4. Invalid LinkConfig arguments panic uniformly across both !dst and -tags dst
	// builds even outside an active run, matching DriftClock's range check.
	for _, tc := range []struct {
		name string
		call func()
		want string
	}{
		{"ConfigureLink/neg-lat", func() { ConfigureLink("a", "b", LinkConfig{Latency: -1}) }, "LinkConfig.Latency must be non-negative"},
		{"ConfigureLink/neg-jit", func() { ConfigureLink("a", "b", LinkConfig{Jitter: -1}) }, "LinkConfig.Jitter must be non-negative"},
		{"ConfigureLink/neg-bw", func() { ConfigureLink("a", "b", LinkConfig{Bandwidth: -1}) }, "LinkConfig.Bandwidth must be non-negative"},
		{"ConfigureLinkOneWay/neg-lat", func() { ConfigureLinkOneWay("a", "b", LinkConfig{Latency: -1}) }, "LinkConfig.Latency must be non-negative"},
	} {
		panicked, msg := didPanicWith(tc.call)
		if !panicked || !strings.Contains(msg, tc.want) {
			t.Errorf("%s outside run: panicked=%v msg=%q, want containing %q", tc.name, panicked, msg, tc.want)
		}
	}
}
