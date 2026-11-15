// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package net

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

func setReuseAddrControl(val int) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, val)
		}); err != nil {
			return err
		}
		return serr
	}
}

func TestDSTNetSockoptNoDelayAndReuseAddr(t *testing.T) {
	simulation.Run(1, func() {
		// 1. TCP_NODELAY reports 1 on SyscallConn, accepts nonzero setsockopt,
		// and refuses TCP_NODELAY=0 (unmodeled Nagle coalescing) with ENOPROTOOPT.
		ln, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		accepted := make(chan Conn, 2)
		go func() {
			for {
				c, aerr := ln.Accept()
				if aerr != nil {
					return
				}
				accepted <- c
			}
		}()
		c, err := Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		sc := <-accepted
		rc, err := c.(syscall.Conn).SyscallConn()
		if err != nil {
			t.Fatalf("SyscallConn: %v", err)
		}
		var got int
		if err := rc.Control(func(fd uintptr) {
			var gerr error
			got, gerr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY)
			if gerr != nil {
				t.Fatalf("GetsockoptInt TCP_NODELAY: %v", gerr)
			}
		}); err != nil {
			t.Fatalf("Control: %v", err)
		}
		if got != 1 {
			t.Fatalf("default TCP_NODELAY = %d, want 1", got)
		}
		_ = rc.Control(func(fd uintptr) {
			if serr := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 7); serr != nil {
				t.Fatalf("SetsockoptInt TCP_NODELAY=7: %v", serr)
			}
			got, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY)
		})
		if got != 1 {
			t.Fatalf("TCP_NODELAY after SetsockoptInt(7) = %d, want 1", got)
		}
		_ = rc.Control(func(fd uintptr) {
			if serr := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 0); !errors.Is(serr, syscall.ENOPROTOOPT) {
				t.Fatalf("SetsockoptInt TCP_NODELAY=0 = %v, want ENOPROTOOPT", serr)
			}
		})

		// 2. Active server Close puts server's local port into TIME_WAIT:
		// ListenConfig with SO_REUSEADDR=0 fails with EADDRINUSE, whereas default
		// Listen (SO_REUSEADDR=1) rides over TIME_WAIT cleanly.
		srvAddr := ln.Addr().String()
		ln.Close()
		sc.Close() // active close -> TIME_WAIT on srvAddr
		c.Close()

		lcNoReuse := ListenConfig{Control: setReuseAddrControl(0)}
		if _, err := lcNoReuse.Listen(context.Background(), "tcp", srvAddr); !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("Listen with SO_REUSEADDR=0 over TIME_WAIT = %v, want EADDRINUSE", err)
		}
		lnReuse, err := Listen("tcp", srvAddr)
		if err != nil {
			t.Fatalf("default Listen (SO_REUSEADDR=1) over TIME_WAIT failed: %v", err)
		}
		defer lnReuse.Close()

		// 3. Dialer with explicit LocalAddr + SO_REUSEADDR=1 shares across distinct
		// remote targets and fails with EADDRNOTAVAIL on identical 4-tuple.
		ln2, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen 2: %v", err)
		}
		defer ln2.Close()
		go func() {
			for {
				conn, aerr := lnReuse.Accept()
				if aerr != nil {
					return
				}
				defer conn.Close()
			}
		}()
		go func() {
			for {
				conn, aerr := ln2.Accept()
				if aerr != nil {
					return
				}
				defer conn.Close()
			}
		}()

		probeLn, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("probe listen: %v", err)
		}
		localBind := probeLn.Addr().(*TCPAddr)
		probeLn.Close()

		dReuse := Dialer{
			LocalAddr: localBind,
			Control:   setReuseAddrControl(1),
		}
		d1, err := dReuse.Dial("tcp", lnReuse.Addr().String())
		if err != nil {
			t.Fatalf("first SO_REUSEADDR dial: %v", err)
		}
		defer d1.Close()

		d2, err := dReuse.Dial("tcp", ln2.Addr().String())
		if err != nil {
			t.Fatalf("second SO_REUSEADDR dial to distinct remote port failed: %v", err)
		}
		defer d2.Close()

		if _, err := dReuse.Dial("tcp", lnReuse.Addr().String()); !errors.Is(err, syscall.EADDRNOTAVAIL) {
			t.Fatalf("SO_REUSEADDR dial with duplicate 4-tuple = %v, want EADDRNOTAVAIL", err)
		}
	})
}

func TestDSTNetProtocolsDeterministicBuiltInAndBubbleIsolated(t *testing.T) {
	inRun := make(chan struct{})
	outsideDone := make(chan error, 1)
	go func() {
		<-inRun
		// Trigger host /etc/protocols load into package-global net.protocols
		// outside the bubble while the simulation is active.
		_, _ = ResolveIPAddr("ip4:gre", "127.0.0.1")
		_, err := ResolveIPAddr("ip4:icmp", "127.0.0.1")
		outsideDone <- err
	}()

	simulation.Run(1, func() {
		close(inRun)
		if err := <-outsideDone; err != nil {
			t.Fatalf("non-bubble ResolveIPAddr(ip4:icmp) failed during simulation: %v", err)
		}
		simulation.Host("h1", func() {
			if err := os.MkdirAll("/etc", 0o755); err != nil {
				t.Fatalf("MkdirAll /etc: %v", err)
			}
			if err := os.WriteFile("/etc/protocols", []byte("simproto 253 SIMPROTO\n"), 0o644); err != nil {
				t.Fatalf("WriteFile /etc/protocols: %v", err)
			}
			simulation.SlowDisk("h1", time.Hour)
			simulation.FailDisk("h1")

			t0 := time.Now()
			for _, tc := range []struct {
				name string
				want int
			}{
				{"icmp", 1},
				{"igmp", 2},
				{"tcp", 6},
				{"TcP", 6},
				{"udp", 17},
				{"ipv6-icmp", 58},
			} {
				p, err := lookupProtocol(context.Background(), tc.name)
				if err != nil || p != tc.want {
					t.Fatalf("lookupProtocol(%q) = (%d, %v), want (%d, nil)", tc.name, p, err, tc.want)
				}
			}
			for _, unknown := range []string{"simproto", "gre", "sctp"} {
				if _, err := lookupProtocol(context.Background(), unknown); err == nil {
					t.Fatalf("lookupProtocol(%q) unexpectedly succeeded inside bubble", unknown)
				}
			}
			if elapsed := time.Since(t0); elapsed != 0 {
				t.Fatalf("in-bubble lookupProtocol touched simulated disk: elapsed = %v, want 0", elapsed)
			}
		})
	})
}

func TestDSTNetDialRedialPreservesSockFDAndOpts(t *testing.T) {
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency:  50 * time.Millisecond,
		RetransmitTimeout: 2 * time.Second,
	}}
	simulation.RunWith(1, opts, func() {
		addrCh := make(chan string, 1)
		srvAccepted := make(chan Conn, 1)
		srvLnCh := make(chan Listener, 1)

		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen boot 1: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
		})

		addr := <-addrCh
		_, port, _ := SplitHostPort(addr)
		dialDone := make(chan struct{})
		var dialConn Conn
		var dialErr error

		simulation.Host("cli", func() {
			go func() {
				defer close(dialDone)
				d := &Dialer{
					KeepAlive: -1, // preserve Control callback's keepalive configuration
					Control: func(_, _ string, raw syscall.RawConn) error {
						var serr error
						if err := raw.Control(func(fd uintptr) {
							serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, 1)
							if serr == nil {
								serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, 7)
							}
						}); err != nil {
							return err
						}
						return serr
					},
				}
				dialConn, dialErr = d.Dial("tcp", addr)
			}()
		})

		// At t=60ms, SYN (50ms) has arrived and dialer is parked in SYN-ACK (50ms).
		// Crashing srv forces dstDial through cleanup() + goto redial.
		time.Sleep(60 * time.Millisecond)
		simulation.CrashHost("srv")

		// Reboot srv on the same port so the redialing client connects to the fresh listener.
		time.Sleep(20 * time.Millisecond)
		simulation.Host("srv", func() {
			ln2, err := Listen("tcp", ":"+port)
			if err != nil {
				t.Fatalf("listen boot 2: %v", err)
			}
			srvLnCh <- ln2
			go func() {
				c, aerr := ln2.Accept()
				if aerr == nil {
					srvAccepted <- c
				}
			}()
		})

		<-dialDone
		if dialErr != nil {
			t.Fatalf("dial across CrashHost + reboot redial failed: %v", dialErr)
		}
		defer dialConn.Close()
		srvConn := <-srvAccepted
		ln2 := <-srvLnCh

		// Verify dialConn still owns its distinct virtual sockFD and Control options,
		// without aliasing srvConn or ln2.
		rawDial, err := dialConn.(syscall.Conn).SyscallConn()
		if err != nil {
			t.Fatalf("dialConn SyscallConn: %v", err)
		}
		rawSrv, err := srvConn.(syscall.Conn).SyscallConn()
		if err != nil {
			t.Fatalf("srvConn SyscallConn: %v", err)
		}

		var dialIdle, srvIdle int
		if err := rawDial.Control(func(fd uintptr) {
			dialIdle, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, 11)
		}); err != nil {
			t.Fatalf("rawDial.Control before server close: %v", err)
		}
		if dialIdle != 7 {
			t.Fatalf("dialConn TCP_KEEPIDLE after redial = %d, want 7 (Control option preserved)", dialIdle)
		}
		if err := rawSrv.Control(func(fd uintptr) {
			srvIdle, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
		}); err != nil {
			t.Fatalf("rawSrv.Control: %v", err)
		}
		if srvIdle == 11 {
			t.Fatalf("mutating dialConn sockopt mutated srvConn sockopt (aliased sockFD across redial)")
		}

		// Closing both server-side sockets must not invalidate dialConn's sockFD.
		srvConn.Close()
		ln2.Close()
		if err := rawDial.Control(func(fd uintptr) {
			dialIdle, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
		}); err != nil {
			t.Fatalf("rawDial.Control after srvConn+ln2 close failed (use-after-free sockFD): %v", err)
		}
		if dialIdle != 11 {
			t.Fatalf("dialConn TCP_KEEPIDLE after srv close = %d, want 11", dialIdle)
		}
	})
}

func TestDSTNetListenerCloseBlackholeExploreDPOR(t *testing.T) {
	netOpts := simulation.Options{
		Network: simulation.NetworkConfig{
			CrossHostLatency:  5 * time.Millisecond,
			RetransmitTimeout: 200 * time.Millisecond,
		},
	}

	// 1. Mid-SYN-ACK blackhole + Listener.Close (SYN arrives at t=5ms, SYN-ACK in
	// flight until t=10ms; Partition + Close at t=7ms must blackhole, never ECONNREFUSED).
	simulation.RunWith(1, netOpts, func() {
		var ln Listener
		var addr string
		simulation.Host("srv", func() {
			var err error
			ln, err = Listen("tcp", ":8080")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			addr = simulation.HostIP("srv") + ":8080"
		})
		dialErr := make(chan error, 1)
		simulation.Host("cli", func() {
			go func() {
				d := Dialer{Timeout: 50 * time.Millisecond}
				c, err := d.Dial("tcp", addr)
				if c != nil {
					c.Close()
				}
				dialErr <- err
			}()
		})
		time.Sleep(7 * time.Millisecond)
		simulation.Host("srv", func() {
			simulation.Partition("srv", "cli")
			_ = ln.Close()
		})
		if err := <-dialErr; errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("mid-SYN-ACK blackhole + Listener.Close leaked ECONNREFUSED: %v", err)
		}
	})

	// 2. DPOR exploration at the exact t=10ms SYN-ACK completion boundary racing
	// Partition("srv", "cli") + ln.Close() against handoff.
	simulation.ExploreTest(t, 1, simulation.ExploreOptions{
		Options:      netOpts,
		Mode:         simulation.DPOR,
		MaxSchedules: 32,
	}, func() error {
		done := make(chan struct{})
		defer close(done)
		addrCh := make(chan string, 1)
		simulation.Host("srv", func() {
			go simulation.Process("srv", func() {
				ln, err := Listen("tcp", ":8080")
				if err != nil {
					panic(err)
				}
				addrCh <- simulation.HostIP("srv") + ":8080"
				time.Sleep(10 * time.Millisecond)
				simulation.Partition("srv", "cli")
				ln.Close()
				<-done
			})
		})
		addr := <-addrCh
		dialErr := make(chan error, 1)
		simulation.Host("cli", func() {
			go simulation.Process("cli", func() {
				d := Dialer{Timeout: 50 * time.Millisecond}
				c, err := d.Dial("tcp", addr)
				if err == nil {
					c.Close()
				}
				dialErr <- err
			})
		})
		err := <-dialErr
		if errors.Is(err, syscall.ECONNREFUSED) {
			return errors.New("blackhole partition leaked ECONNREFUSED on concurrent Listener.Close")
		}
		return nil
	})
}
