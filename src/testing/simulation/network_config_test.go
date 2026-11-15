// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"fmt"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDSTLinkConfigValidationStateNeutral(t *testing.T) {
	badConfigs := []struct {
		name string
		cfg  LinkConfig
		want string
	}{
		{
			name: "overflow",
			cfg:  LinkConfig{Latency: time.Duration(math.MaxInt64), Jitter: 2},
			want: "LinkConfig latency plus jitter overflows time.Duration",
		},
		{
			name: "negative-latency",
			cfg:  LinkConfig{Latency: -1},
			want: "LinkConfig.Latency must be non-negative",
		},
		{
			name: "negative-jitter",
			cfg:  LinkConfig{Jitter: -1},
			want: "LinkConfig.Jitter must be non-negative",
		},
		{
			name: "negative-bandwidth",
			cfg:  LinkConfig{Bandwidth: -1},
			want: "LinkConfig.Bandwidth must be non-negative",
		},
	}

	for _, bc := range badConfigs {
		opts := Options{Network: NetworkConfig{CrossHost: bc.cfg}}
		for _, entry := range []struct {
			name string
			run  func(func())
		}{
			{"RunWith", func(f func()) { RunWith(1, opts, f) }},
			{"TestWith", func(f func()) { TestWith(t, 1, opts, func(*testing.T) { f() }) }},
			{"ExploreWith", func(f func()) { ExploreWith(1, ExploreOptions{Options: opts}, func() error { f(); return nil }) }},
			{"Replay", func(f func()) { Replay(1, Failure{Options: opts}, func() error { f(); return nil }) }},
		} {
			t.Run(bc.name+"/"+entry.name, func(t *testing.T) {
				called := false
				func() {
					defer func() {
						got := recover()
						if got == nil || !strings.Contains(fmt.Sprint(got), bc.want) {
							t.Fatalf("panic = %v, want containing %q", got, bc.want)
						}
					}()
					entry.run(func() { called = true })
				}()
				if called {
					t.Fatal("invalid LinkConfig invoked the simulation")
				}
			})
		}

		// Outside an active run, ConfigureLink / ConfigureLinkOneWay must still
		// reject invalid LinkConfig before turning into a no-op.
		for _, call := range []struct {
			name string
			fn   func()
		}{
			{"ConfigureLink", func() { ConfigureLink("a", "b", bc.cfg) }},
			{"ConfigureLinkOneWay", func() { ConfigureLinkOneWay("a", "b", bc.cfg) }},
		} {
			func() {
				defer func() {
					got := recover()
					if got == nil || !strings.Contains(fmt.Sprint(got), bc.want) {
						t.Fatalf("outside-run %s/%s: panic = %v, want containing %q", bc.name, call.name, got, bc.want)
					}
				}()
				call.fn()
			}()
		}

		// In-run dynamic ConfigureLink / ConfigureLinkOneWay must also reject
		// invalid LinkConfig state-neutrally before lookupHost, without mutating
		// declared hosts or overwriting an already-active link policy.
		Run(1, func() {
			portCh := make(chan string, 1)
			done := make(chan struct{})
			serverDone := make(chan struct{})
			Host("a", func() {})
			Host("b", func() {
				ln, err := net.Listen("tcp", ":0")
				if err != nil {
					panic(err)
				}
				_, port, _ := net.SplitHostPort(ln.Addr().String())
				portCh <- port
				go func() {
					defer close(serverDone)
					c, err := ln.Accept()
					if err != nil {
						panic(err)
					}
					buf := make([]byte, 1)
					if _, err := c.Read(buf); err != nil {
						panic(err)
					}
					<-done
					c.Close()
					ln.Close()
				}()
			})
			ConfigureLink("a", "b", LinkConfig{Latency: 25 * time.Millisecond})
			for _, call := range []struct {
				name string
				fn   func()
			}{
				{"ConfigureLink", func() { ConfigureLink("a", "b", bc.cfg) }},
				{"ConfigureLinkOneWay", func() { ConfigureLinkOneWay("a", "b", bc.cfg) }},
				{"ConfigureLink-undeclared", func() { ConfigureLink("a", "undeclared", bc.cfg) }},
				{"ConfigureLinkOneWay-undeclared", func() { ConfigureLinkOneWay("a", "undeclared", bc.cfg) }},
			} {
				func() {
					defer func() {
						got := recover()
						if got == nil || !strings.Contains(fmt.Sprint(got), bc.want) {
							t.Fatalf("in-run %s/%s: panic = %v, want containing %q", bc.name, call.name, got, bc.want)
						}
					}()
					call.fn()
				}()
			}
			// Host table must not have interned "undeclared".
			func() {
				defer func() {
					got := recover()
					if got == nil || !strings.Contains(fmt.Sprint(got), "unknown host") {
						t.Fatalf("in-run %s: undeclared host check panic = %v, want unknown host", bc.name, got)
					}
				}()
				IsolateHost("undeclared")
			}()
			// Active 25ms a↔b link policy (1 RTT = 50ms connect) must remain intact.
			Host("a", func() {
				port := <-portCh
				start := time.Now()
				c, err := net.Dial("tcp", HostIP("b")+":"+port)
				if err != nil {
					panic(err)
				}
				defer c.Close()
				if got := time.Since(start); got != 50*time.Millisecond {
					t.Fatalf("in-run %s: active link connect = %v, want 50ms (prior LinkConfig must survive failed validation)", bc.name, got)
				}
				if _, err := c.Write([]byte("x")); err != nil {
					panic(err)
				}
				close(done)
				<-serverDone
			})
		})
	}

	// Exact boundary: MaxInt64 + (1 - 1) == MaxInt64 fits without overflow.
	RunWith(1, Options{Network: NetworkConfig{
		CrossHost: LinkConfig{
			Latency: time.Duration(math.MaxInt64),
			Jitter:  1,
		},
	}}, func() {
		Host("a", func() {})
		Host("b", func() {})
		ConfigureLink("a", "b", LinkConfig{
			Latency: time.Duration(math.MaxInt64),
			Jitter:  1,
		})
	})

	// Clean subsequent run with default zero config succeeds unaffected.
	Run(1, func() {})
}
