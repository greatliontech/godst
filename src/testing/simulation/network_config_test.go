// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestDSTNetworkDelayOverflowRejectedBeforeRun(t *testing.T) {
	opts := Options{Network: NetworkConfig{
		CrossHostLatency: time.Duration(math.MaxInt64),
		CrossHostJitter:  2,
	}}
	for _, tc := range []struct {
		name string
		run  func(func())
	}{
		{"RunWith", func(f func()) { RunWith(1, opts, f) }},
		{"TestWith", func(f func()) { TestWith(t, 1, opts, func(*testing.T) { f() }) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			func() {
				defer func() {
					got := recover()
					if got == nil || !strings.Contains(fmt.Sprint(got), "Network latency plus jitter overflows") {
						t.Fatalf("panic = %v, want network delay overflow", got)
					}
				}()
				tc.run(func() { called = true })
			}()
			if called {
				t.Fatal("invalid network configuration invoked the simulation")
			}
		})
	}
	RunWith(1, Options{Network: NetworkConfig{
		CrossHostLatency: time.Duration(math.MaxInt64),
		CrossHostJitter:  1,
	}}, func() {})
	latency, jitter, bandwidth, _, _ := resolveNetConfig("test", NetworkConfig{
		CrossHostLatency:   -1,
		CrossHostJitter:    -1,
		CrossHostBandwidth: -1,
	})
	if latency != 0 || jitter != 0 || bandwidth != 0 {
		t.Fatalf("negative network delays resolved to latency=%d jitter=%d bandwidth=%d, want disabled zeros", latency, jitter, bandwidth)
	}
	Run(1, func() {})
}
