// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package main

/*
void dstCgoNoop(void) {}
*/
import "C"

import (
	"os"
	"strconv"
	"strings"
	"testing/simulation"
)

func init() {
	register("DSTCgoFence", DSTCgoFence)
}

// DSTCgoFence checks that the interception boundary fences cgo at the call site
// (cgocall), not the build: a bubble goroutine calling into C is refused with a
// panic (a real C call is host-visible, wall-clock work no seed controls; gating
// on iscgo at run entry would be too coarse — a binary may link cgo it never
// calls in-run). A non-bubble cgo call is unaffected. Built with -tags=dst and
// cgo. Expected output: "bubblePanicked=true hostOK=true".
func DSTCgoFence() {
	var bubblePanicked bool
	simulation.Run(1, func() {
		func() {
			defer func() {
				if v := recover(); v != nil {
					s, _ := v.(string)
					bubblePanicked = strings.Contains(s, "unsupported under deterministic simulation")
				}
			}()
			C.dstCgoNoop()
		}()
	})

	// Outside the run (non-bubble), the same cgo call must work normally.
	hostOK := func() (ok bool) {
		defer func() { ok = recover() == nil }()
		C.dstCgoNoop()
		return
	}()

	os.Stdout.WriteString("bubblePanicked=" + strconv.FormatBool(bubblePanicked) +
		" hostOK=" + strconv.FormatBool(hostOK) + "\n")
}
