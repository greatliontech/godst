// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"testing"
	"time"
)

// TestDSTHostConfigDocExample compiles and runs faults.md's canonical
// Host/Process API example LITERALLY (docs/dst/faults.md, "API (explicit,
// declarative, dynamic)"), so the documented configuration surface cannot
// drift from the landed HostConfig again — the example once named a
// HostConfig.IP field the struct never carried, and only a compile-checked
// copy pins the two together. Host IPs are deterministically assigned and
// queried with HostIP; there is no per-host IP configuration knob.
func TestDSTHostConfigDocExample(t *testing.T) {
	const ms = time.Millisecond
	p1main := func() {}
	p2main := func() {}
	n3main := func() {}
	var h1IP string
	Run(1, func() {
		// The doc example, verbatim.
		Host("h1", HostConfig{NumCPU: 4, Clock: Skew(50 * ms)}, func() {
			Process("p1", p1main) // shares h1's FS, IP, port space, clock
			Process("p2", p2main)
		})
		Process("n3", n3main) // implicit dedicated host
		h1IP = HostIP("h1")   // the deterministically assigned routable IP
	})
	if h1IP == "" {
		t.Fatalf("HostIP(h1) = %q, want the deterministically assigned routable IP", h1IP)
	}
}
