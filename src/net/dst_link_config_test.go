// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
	"testing/simulation"
	"time"
)

func dstMeasurePair(fromHost, toHost string) (dialRTT, forwardOneWay, fullRTT time.Duration) {
	portCh := make(chan string, 1)
	done := make(chan struct{})
	var writeAt, srvReadAt, cliRespAt time.Time

	simulation.Host(toHost, func() {
		ln, err := Listen("tcp", ":0")
		if err != nil {
			panic(err)
		}
		_, p, _ := SplitHostPort(ln.Addr().String())
		portCh <- p
		go func() {
			defer close(done)
			defer ln.Close()
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			var buf [16]byte
			n, err := c.Read(buf[:])
			if err != nil {
				panic(err)
			}
			srvReadAt = time.Now()
			if _, err := c.Write(buf[:n]); err != nil {
				panic(err)
			}
		}()
	})

	simulation.Host(fromHost, func() {
		addr := simulation.HostIP(toHost) + ":" + <-portCh
		t0 := time.Now()
		c, err := Dial("tcp", addr)
		if err != nil {
			panic(err)
		}
		dialRTT = time.Since(t0)
		writeAt = time.Now()
		if _, err := c.Write([]byte("ping")); err != nil {
			panic(err)
		}
		var buf [16]byte
		if _, err := c.Read(buf[:]); err != nil {
			panic(err)
		}
		cliRespAt = time.Now()
		_ = c.Close()
	})
	<-done
	return dialRTT, srvReadAt.Sub(writeAt), cliRespAt.Sub(writeAt)
}

func TestDSTNetLinkConfigSymmetricDirectionalAndUnconfigure(t *testing.T) {
	const base = 50 * time.Millisecond
	opts := simulation.Options{
		Network: simulation.NetworkConfig{
			CrossHost: simulation.LinkConfig{Latency: base},
		},
	}
	simulation.RunWith(1, opts, func() {
		simulation.Host("A", func() {})
		simulation.Host("B", func() {})
		simulation.Host("C", func() {})

		// 1. Baseline applies to both B↔A and C↔A.
		if d, ow, rtt := dstMeasurePair("B", "A"); d != 2*base || ow != base || rtt != 2*base {
			t.Fatalf("baseline B→A = (dial %v, ow %v, rtt %v), want (%v, %v, %v)", d, ow, rtt, 2*base, base, 2*base)
		}
		if d, ow, rtt := dstMeasurePair("C", "A"); d != 2*base || ow != base || rtt != 2*base {
			t.Fatalf("baseline C→A = (dial %v, ow %v, rtt %v), want (%v, %v, %v)", d, ow, rtt, 2*base, base, 2*base)
		}

		// 2. Symmetric ConfigureLink overrides A↔B in both directions while leaving C↔A at baseline.
		const slow = 120 * time.Millisecond
		simulation.ConfigureLink("A", "B", simulation.LinkConfig{Latency: slow})
		if d, ow, rtt := dstMeasurePair("B", "A"); d != 2*slow || ow != slow || rtt != 2*slow {
			t.Fatalf("symmetric override B→A = (dial %v, ow %v, rtt %v), want (%v, %v, %v)", d, ow, rtt, 2*slow, slow, 2*slow)
		}
		if d, ow, rtt := dstMeasurePair("C", "A"); d != 2*base || ow != base || rtt != 2*base {
			t.Fatalf("untouched C→A = (dial %v, ow %v, rtt %v), want (%v, %v, %v)", d, ow, rtt, 2*base, base, 2*base)
		}

		// 3. ConfigureLinkOneWay overrides only B→A while A→B keeps 120ms.
		const fast = 10 * time.Millisecond
		simulation.ConfigureLinkOneWay("B", "A", simulation.LinkConfig{Latency: fast})
		if d, ow, rtt := dstMeasurePair("B", "A"); d != fast+slow || ow != fast || rtt != fast+slow {
			t.Fatalf("asymmetric B→A = (dial %v, ow %v, rtt %v), want (%v, %v, %v)", d, ow, rtt, fast+slow, fast, fast+slow)
		}

		// 4. UnconfigureLink clears both directions and restores the CrossHost baseline.
		simulation.UnconfigureLink("A", "B")
		if d, ow, rtt := dstMeasurePair("B", "A"); d != 2*base || ow != base || rtt != 2*base {
			t.Fatalf("unconfigured B→A = (dial %v, ow %v, rtt %v), want (%v, %v, %v)", d, ow, rtt, 2*base, base, 2*base)
		}
		if dstHasActiveLinkOverrides() {
			t.Fatal("dstHasActiveLinkOverrides() = true after UnconfigureLink, want false")
		}
	})
}

func TestDSTNetLinkConfigZeroOverrideFastIntraAZInSlowWAN(t *testing.T) {
	opts := simulation.Options{
		Network: simulation.NetworkConfig{
			CrossHost: simulation.LinkConfig{
				Latency:   80 * time.Millisecond,
				Jitter:    20 * time.Millisecond,
				Bandwidth: 512,
			},
		},
	}
	simulation.RunWith(1, opts, func() {
		simulation.Host("az1-a", func() {})
		simulation.Host("az1-b", func() {})
		simulation.Host("wan-c", func() {})

		// Explicit zero LinkConfig creates an instant, unthrottled intra-AZ link
		// inside a slow/jittery/throttled CrossHost WAN baseline.
		simulation.ConfigureLink("az1-a", "az1-b", simulation.LinkConfig{})

		t0 := time.Now()
		portCh := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("az1-a", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			portCh <- p
			go func() {
				defer close(done)
				defer ln.Close()
				c, err := ln.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close()
				b, err := io.ReadAll(c)
				if err != nil || len(b) != 4096 {
					t.Errorf("intra-AZ ReadAll = (%d, %v), want (4096, nil)", len(b), err)
				}
			}()
		})
		simulation.Host("az1-b", func() {
			c, err := Dial("tcp", simulation.HostIP("az1-a")+":"+<-portCh)
			if err != nil {
				t.Fatal(err)
			}
			payload := make([]byte, 4096)
			if _, err := c.Write(payload); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
		})
		<-done
		if elapsed := time.Since(t0); elapsed != 0 {
			t.Fatalf("zero-LinkConfig intra-AZ transfer elapsed = %v, want 0", elapsed)
		}

		// Meanwhile wan-c → az1-a still pays the WAN baseline.
		dialRTT, oneWay, _ := dstMeasurePair("wan-c", "az1-a")
		if dialRTT < 160*time.Millisecond || oneWay < 80*time.Millisecond {
			t.Fatalf("WAN pair dialRTT=%v oneWay=%v, want >=160ms and >=80ms", dialRTT, oneWay)
		}
	})
}

func TestDSTNetLinkConfigDirectionalHandshakeAndMidSYNACKOverride(t *testing.T) {
	// 1. Directional SYN vs SYN-ACK context deadline sensitivity.
	simulation.Run(1, func() {
		simulation.Host("srv", func() {})
		simulation.Host("cli", func() {})
		simulation.ConfigureLinkOneWay("cli", "srv", simulation.LinkConfig{Latency: 30 * time.Millisecond})
		simulation.ConfigureLinkOneWay("srv", "cli", simulation.LinkConfig{Latency: 90 * time.Millisecond})

		portCh := make(chan string, 1)
		acceptUnexpected := make(chan struct{}, 1)
		done := make(chan struct{})
		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			portCh <- p
			go func() {
				if c, err := ln.Accept(); err == nil {
					c.Close()
					acceptUnexpected <- struct{}{}
				}
			}()
			go func() {
				<-done
				ln.Close()
			}()
		})
		simulation.Host("cli", func() {
			addr := simulation.HostIP("srv") + ":" + <-portCh
			// 100ms timeout passes SYN (30ms) but expires during SYN-ACK (90ms, total 120ms).
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			t0 := time.Now()
			c, err := (&Dialer{}).DialContext(ctx, "tcp", addr)
			elapsed := time.Since(t0)
			if c != nil || !errors.Is(err, context.DeadlineExceeded) || elapsed != 100*time.Millisecond {
				t.Fatalf("DialContext(100ms) = (%v, %v) after %v, want (nil, DeadlineExceeded) at 100ms", c, err, elapsed)
			}
			select {
			case <-acceptUnexpected:
				t.Fatalf("Accept woke before directional SYN-ACK completed")
			default:
			}
			close(done)
		})
	})

	// 2. Dynamic ConfigureLinkOneWay on srv→cli while SYN is in flight updates SYN-ACK delay.
	opts := simulation.Options{
		Network: simulation.NetworkConfig{
			CrossHost: simulation.LinkConfig{Latency: 40 * time.Millisecond},
		},
	}
	simulation.RunWith(2, opts, func() {
		portCh := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			portCh <- p
			go func() {
				defer ln.Close()
				c, err := ln.Accept()
				if err == nil {
					c.Close()
				}
				<-done
			}()
		})
		simulation.Host("cli", func() {
			addr := simulation.HostIP("srv") + ":" + <-portCh
			dialDone := make(chan time.Duration, 1)
			go func() {
				t0 := time.Now()
				c, err := Dial("tcp", addr)
				if err != nil {
					t.Errorf("dial: %v", err)
					dialDone <- 0
					return
				}
				dialDone <- time.Since(t0)
				c.Close()
			}()
			// At t=20ms, SYN (40ms) is still in flight; reconfigure srv→cli to 75ms before SYN-ACK starts.
			time.Sleep(20 * time.Millisecond)
			simulation.ConfigureLinkOneWay("srv", "cli", simulation.LinkConfig{Latency: 75 * time.Millisecond})
			if got := <-dialDone; got != 115*time.Millisecond {
				t.Fatalf("dial RTT with mid-SYN srv→cli override = %v, want 115ms (40ms SYN + 75ms SYN-ACK)", got)
			}
			close(done)
		})
	})
}

func TestDSTNetLinkConfigMidStreamFIFOUnthrottleAndFINHorizon(t *testing.T) {
	// 1. Decreasing latency mid-stream (100ms → 5ms) preserves FIFO: second segment
	// never overtakes the first segment (head-of-line delivery at 100ms).
	simulation.RunWith(1, simulation.Options{
		Network: simulation.NetworkConfig{
			CrossHost: simulation.LinkConfig{Latency: 100 * time.Millisecond},
		},
	}, func() {
		portCh := make(chan string, 1)
		accepted := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			portCh <- p
			go func() {
				defer close(done)
				defer ln.Close()
				c, err := ln.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close()
				close(accepted)
				t0 := time.Now()
				// Neither segment may arrive by t=50ms even though seg2's nominal 10ms+5ms=15ms passed.
				_ = c.SetReadDeadline(t0.Add(50 * time.Millisecond))
				if n, err := c.Read(make([]byte, 16)); !errors.Is(err, os.ErrDeadlineExceeded) || n != 0 {
					t.Errorf("read at 50ms = (%d, %v), want (0, ErrDeadlineExceeded)", n, err)
					return
				}
				_ = c.SetReadDeadline(time.Time{})
				all, err := io.ReadAll(c)
				if err != nil || string(all) != "first:second" {
					t.Errorf("ReadAll = (%q, %v), want (\"first:second\", nil)", all, err)
					return
				}
				if got := time.Since(t0); got != 100*time.Millisecond {
					t.Errorf("stream drained at %v, want 100ms", got)
				}
			}()
		})
		simulation.Host("cli", func() {
			c, err := Dial("tcp", simulation.HostIP("srv")+":"+<-portCh)
			if err != nil {
				t.Fatal(err)
			}
			<-accepted
			if _, err := c.Write([]byte("first:")); err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
			simulation.ConfigureLink("cli", "srv", simulation.LinkConfig{Latency: 5 * time.Millisecond})
			if _, err := c.Write([]byte("second")); err != nil {
				t.Fatal(err)
			}
			_ = c.Close()
		})
		<-done
	})

	// 2. Switching from throttled (Bandwidth=100 B/s) to unthrottled (Bandwidth=0)
	// anchors unthrottled segments at max(now, linkFreeAt) without advancing linkFreeAt,
	// AND switching to higher latency before CloseWrite/Close anchors FIN at max(now, linkFreeAt)+newLatency.
	simulation.Run(2, func() {
		simulation.Host("srv", func() {})
		simulation.Host("cli", func() {})
		// Establish at 0ns, then configure throttled policy on cli→srv.
		portCh := make(chan string, 1)
		srvConnCh := make(chan Conn, 1)
		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			portCh <- p
			go func() {
				defer ln.Close()
				c, err := ln.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				srvConnCh <- c
			}()
		})

		var cli Conn
		simulation.Host("cli", func() {
			var err error
			cli, err = Dial("tcp", simulation.HostIP("srv")+":"+<-portCh)
			if err != nil {
				t.Fatal(err)
			}
		})
		srv := <-srvConnCh
		defer srv.Close()

		t0 := time.Now()
		// 100 B/s + 10ms propagation: 10-byte write occupies wire until t=100ms, delivers at t=110ms.
		simulation.ConfigureLinkOneWay("cli", "srv", simulation.LinkConfig{
			Latency:   10 * time.Millisecond,
			Bandwidth: 100,
		})
		simulation.Host("cli", func() {
			if _, err := cli.Write([]byte("0123456789")); err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
			// At t=10ms, unthrottle (Bandwidth=0) with Latency=25ms:
			// Must anchor at linkFreeAt (100ms), NOT now (10ms), so deliverAt = 125ms (not 35ms!),
			// and must NOT advance linkFreeAt, so a second 10-byte write ALSO delivers at 125ms!
			simulation.ConfigureLinkOneWay("cli", "srv", simulation.LinkConfig{
				Latency:   25 * time.Millisecond,
				Bandwidth: 0,
			})
			if _, err := cli.Write([]byte("abcdefghij")); err != nil {
				t.Fatal(err)
			}
			if _, err := cli.Write([]byte("klmnopqrst")); err != nil {
				t.Fatal(err)
			}
			// Raise latency to 50ms and emit graceful FIN at t=10ms while linkFreeAt is STILL 100ms:
			// FIN closeAt must be max(10ms, 100ms) + 50ms = 150ms (not 10ms + 50ms = 60ms!).
			simulation.ConfigureLinkOneWay("cli", "srv", simulation.LinkConfig{
				Latency:   50 * time.Millisecond,
				Bandwidth: 0,
			})
			if err := cli.Close(); err != nil {
				t.Fatal(err)
			}
		})

		simulation.Host("srv", func() {
			var b10 [10]byte
			if _, err := io.ReadFull(srv, b10[:]); err != nil {
				t.Fatal(err)
			}
			if got := time.Since(t0); got != 110*time.Millisecond || string(b10[:]) != "0123456789" {
				t.Fatalf("first chunk = %q at %v, want \"0123456789\" at 110ms", b10[:], got)
			}

			// At t=110ms, the two unthrottled chunks (anchored at linkFreeAt=100ms + 25ms = 125ms)
			// must NOT have arrived yet!
			_ = srv.SetReadDeadline(t0.Add(120 * time.Millisecond))
			if n, err := srv.Read(b10[:]); !errors.Is(err, os.ErrDeadlineExceeded) || n != 0 {
				t.Fatalf("read at 120ms = (%d, %v), want (0, ErrDeadlineExceeded): unthrottled segment overtook linkFreeAt", n, err)
			}
			_ = srv.SetReadDeadline(time.Time{})

			// Both 10-byte unthrottled chunks arrive together at t=125ms (linkFreeAt was not advanced).
			var b20 [20]byte
			if _, err := io.ReadFull(srv, b20[:]); err != nil {
				t.Fatal(err)
			}
			if got := time.Since(t0); got != 125*time.Millisecond || string(b20[:]) != "abcdefghijklmnopqrst" {
				t.Fatalf("unthrottled chunks = %q at %v, want \"abcdefghijklmnopqrst\" at 125ms", b20[:], got)
			}

			// FIN (anchored at linkFreeAt=100ms + 50ms = 150ms) must NOT arrive by t=140ms!
			_ = srv.SetReadDeadline(t0.Add(140 * time.Millisecond))
			if n, err := srv.Read(b10[:]); !errors.Is(err, os.ErrDeadlineExceeded) || n != 0 {
				t.Fatalf("FIN read at 140ms = (%d, %v), want (0, ErrDeadlineExceeded): FIN overtook linkFreeAt", n, err)
			}
			_ = srv.SetReadDeadline(time.Time{})

			if n, err := srv.Read(b10[:]); n != 0 || err != io.EOF {
				t.Fatalf("FIN read = (%d, %v), want (0, io.EOF)", n, err)
			}
			if got := time.Since(t0); got != 150*time.Millisecond {
				t.Fatalf("FIN arrived at %v, want 150ms", got)
			}
		})
	})
}

func TestDSTNetLinkConfigSameHostInstantAndFastPathClean(t *testing.T) {
	// Prime a prior run with a cross-host override so the subsequent run also
	// proves dstHasActiveLinkOverrides() is immediately false on the new epoch.
	simulation.Run(1, func() {
		simulation.Host("a", func() {})
		simulation.Host("b", func() {})
		simulation.ConfigureLink("a", "b", simulation.LinkConfig{Latency: 25 * time.Millisecond})
		if !dstHasActiveLinkOverrides() {
			t.Fatal("cross-host ConfigureLink did not enable dstHasActiveLinkOverrides")
		}
	})

	simulation.Run(1, func() {
		if dstHasActiveLinkOverrides() {
			t.Fatal("new run inherited active link override flag from prior run")
		}
		simulation.Host("solo", func() {
			simulation.ConfigureLink("solo", "solo", simulation.LinkConfig{
				Latency:   100 * time.Millisecond,
				Jitter:    50 * time.Millisecond,
				Bandwidth: 10,
			})
			simulation.ConfigureLinkOneWay("solo", "solo", simulation.LinkConfig{
				Latency: 100 * time.Millisecond,
			})
			if dstHasActiveLinkOverrides() {
				t.Fatal("same-host ConfigureLink enabled dstHasActiveLinkOverrides")
			}

			t0 := time.Now()
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatal(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer ln.Close()
				c, err := ln.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close()
				b, err := io.ReadAll(c)
				if err != nil || string(b) != "same-host" {
					t.Errorf("ReadAll = (%q, %v), want (\"same-host\", nil)", b, err)
				}
			}()
			c, err := Dial("tcp", simulation.HostIP("solo")+":"+p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Write([]byte("same-host")); err != nil {
				t.Fatal(err)
			}
			_ = c.Close()
			<-done
			if elapsed := time.Since(t0); elapsed != 0 {
				t.Fatalf("same-host transfer elapsed = %v, want 0", elapsed)
			}
		})
	})
}

func TestDSTNetLinkConfigSurvivesHealLinkAndCrashHostAndResetsAcrossRuns(t *testing.T) {
	const base = 10 * time.Millisecond
	const custom = 75 * time.Millisecond
	opts := simulation.Options{
		Network: simulation.NetworkConfig{
			CrossHost: simulation.LinkConfig{Latency: base},
		},
	}

	// Run 1: configure A↔B to 75ms, survive Partition+HealLink and CrashHost+reboot,
	// and intentionally leave the override installed when Run 1 exits.
	simulation.RunWith(1, opts, func() {
		simulation.Host("A", func() {})
		simulation.Host("B", func() {})
		simulation.ConfigureLink("A", "B", simulation.LinkConfig{Latency: custom})

		simulation.Partition("A", "B")
		simulation.HealLink("A", "B")
		if d, ow, rtt := dstMeasurePair("B", "A"); d != 2*custom || ow != custom || rtt != 2*custom {
			t.Fatalf("after HealLink: B→A = (%v, %v, %v), want (%v, %v, %v)", d, ow, rtt, 2*custom, custom, 2*custom)
		}

		simulation.CrashHost("A")
		simulation.Host("A", func() {}) // reboot A
		if d, ow, rtt := dstMeasurePair("B", "A"); d != 2*custom || ow != custom || rtt != 2*custom {
			t.Fatalf("after CrashHost+reboot: B→A = (%v, %v, %v), want (%v, %v, %v)", d, ow, rtt, 2*custom, custom, 2*custom)
		}
		if !dstHasActiveLinkOverrides() {
			t.Fatal("Run 1 dstHasActiveLinkOverrides() = false, want true")
		}
	})

	// Run 2: starts immediately clean at baseline (10ms) with dstHasActiveLinkOverrides() == false.
	simulation.RunWith(2, opts, func() {
		if dstHasActiveLinkOverrides() {
			t.Fatal("Run 2 dstHasActiveLinkOverrides() = true at run start, want false")
		}
		simulation.Host("A", func() {})
		simulation.Host("B", func() {})
		if d, ow, rtt := dstMeasurePair("B", "A"); d != 2*base || ow != base || rtt != 2*base {
			t.Fatalf("Run 2 B→A leaked prior run's override: got (%v, %v, %v), want (%v, %v, %v)", d, ow, rtt, 2*base, base, 2*base)
		}
		if dstHasActiveLinkOverrides() {
			t.Fatal("Run 2 dstHasActiveLinkOverrides() = true, want false")
		}
	})
}

func TestDSTNetLinkConfigReplayDeterminism(t *testing.T) {
	run := func(seed uint64) []time.Duration {
		var stamps []time.Duration
		opts := simulation.Options{
			Network: simulation.NetworkConfig{
				CrossHost: simulation.LinkConfig{
					Latency:   15 * time.Millisecond,
					Jitter:    25 * time.Millisecond,
					Bandwidth: 2048,
				},
			},
		}
		simulation.RunWith(seed, opts, func() {
			t0 := time.Now()
			simulation.Host("A", func() {})
			simulation.Host("B", func() {})
			simulation.ConfigureLinkOneWay("B", "A", simulation.LinkConfig{
				Latency:   5 * time.Millisecond,
				Jitter:    30 * time.Millisecond,
				Bandwidth: 1024,
			})
			d1, ow1, rtt1 := dstMeasurePair("B", "A")
			stamps = append(stamps, time.Since(t0), d1, ow1, rtt1)

			simulation.UnconfigureLink("A", "B")
			d2, ow2, rtt2 := dstMeasurePair("B", "A")
			stamps = append(stamps, time.Since(t0), d2, ow2, rtt2)
		})
		return stamps
	}

	distinct := map[string]bool{}
	for seed := uint64(0); seed < 16; seed++ {
		a, b := run(seed), run(seed)
		if !slices.Equal(a, b) {
			t.Fatalf("seed %d: per-link timing not reproducible: %v vs %v", seed, a, b)
		}
		distinct[fmt.Sprint(a)] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("per-link jitter produced identical schedules across 16 seeds")
	}
}
