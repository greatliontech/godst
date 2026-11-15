// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

type dstHalfConn interface {
	Conn
	CloseWrite() error
	CloseRead() error
}

func TestDSTNetCanceledMidHandshakeDialReleasesBacklog(t *testing.T) {
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHost:         simulation.LinkConfig{Latency: 50 * time.Millisecond},
		RetransmitTimeout: time.Second,
	}}
	simulation.RunWith(1, opts, func() {
		addrCh := make(chan string, 1)
		readyAccept := make(chan struct{})
		acceptedCh := make(chan string, 2)
		done := make(chan struct{})

		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
			go func() {
				<-readyAccept
				c, err := ln.Accept()
				if err != nil {
					t.Errorf("accept: %v", err)
					return
				}
				defer c.Close()
				b, err := io.ReadAll(c)
				if err != nil {
					t.Errorf("server read: %v", err)
					return
				}
				acceptedCh <- string(b)
			}()
			go func() {
				<-done
				ln.Close()
			}()
		})

		simulation.Host("cli", func() {
			addr := <-addrCh
			const backlog = 128
			var wg sync.WaitGroup
			for range backlog {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
					defer cancel()
					c, err := (&Dialer{}).DialContext(ctx, "tcp", addr)
					if err == nil {
						c.Close()
						t.Errorf("mid-handshake canceled dial unexpectedly succeeded")
						return
					}
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("canceled dial err = %v, want DeadlineExceeded", err)
					}
				}()
			}
			wg.Wait()

			// Every canceled mid-handshake dial must have released its backlog
			// slot and torn down its unaccepted server socket, so the 129th dial
			// succeeds immediately and is the exact connection Accept returns.
			close(readyAccept)
			c, err := Dial("tcp", addr)
			if err != nil {
				t.Fatalf("129th dial after backlog cancellations failed: %v", err)
			}
			if _, err := c.Write([]byte("live-129")); err != nil {
				t.Fatalf("write: %v", err)
			}
			c.Close()
			if got := <-acceptedCh; got != "live-129" {
				t.Fatalf("accepted payload = %q, want live-129", got)
			}
			close(done)
		})
	})
}

func TestDSTNetAcceptWaitsForSYNACKAcrossOneWayPartition(t *testing.T) {
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHost:         simulation.LinkConfig{Latency: 50 * time.Millisecond},
		RetransmitTimeout: 500 * time.Millisecond,
	}}

	// Subcase 1: HealLink before retransmit horizon unblocks both Dial and Accept.
	simulation.RunWith(1, opts, func() {
		addrCh := make(chan string, 1)
		accepted := make(chan Conn, 1)
		done := make(chan struct{})

		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
			go func() {
				c, err := ln.Accept()
				if err == nil {
					accepted <- c
				}
			}()
			go func() {
				<-done
				ln.Close()
			}()
		})

		simulation.Host("cli", func() {
			addr := <-addrCh
			// Cut srv -> cli only: SYN reaches srv at 50ms, but SYN-ACK is held.
			simulation.PartitionOneWay("srv", "cli")
			dialCh := make(chan Conn, 1)
			dialErrCh := make(chan error, 1)
			go func() {
				c, err := Dial("tcp", addr)
				if err != nil {
					dialErrCh <- err
					return
				}
				dialCh <- c
			}()

			time.Sleep(200 * time.Millisecond)
			select {
			case c := <-accepted:
				c.Close()
				t.Fatalf("Accept unblocked before SYN-ACK completed across one-way cut")
			default:
			}

			simulation.HealLink("srv", "cli")
			cliConn := <-dialCh
			srvConn := <-accepted
			if _, err := cliConn.Write([]byte("ping")); err != nil {
				t.Fatalf("cli write: %v", err)
			}
			cliConn.Close()
			got, err := io.ReadAll(srvConn)
			srvConn.Close()
			if err != nil || string(got) != "ping" {
				t.Fatalf("srv read = %q, %v; want ping, nil", got, err)
			}
			close(done)
		})
	})

	// Subcase 2: Permanent srv -> cli cut times out Dial at RetransmitTimeout without ever waking Accept.
	simulation.RunWith(2, opts, func() {
		addrCh := make(chan string, 1)
		acceptUnexpected := make(chan struct{}, 1)
		done := make(chan struct{})

		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
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
			addr := <-addrCh
			simulation.PartitionOneWay("srv", "cli")
			_, err := Dial("tcp", addr)
			if !errors.Is(err, syscall.ETIMEDOUT) {
				t.Fatalf("dial across permanent SYN-ACK cut = %v, want ETIMEDOUT", err)
			}
			select {
			case <-acceptUnexpected:
				t.Fatalf("Accept observed an unestablished connection after SYN-ACK timeout")
			default:
			}
			close(done)
		})
	})
}

func TestDSTNetTCPConnCloseWriteHalfDuplex(t *testing.T) {
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHost: simulation.LinkConfig{Latency: 10 * time.Millisecond},
	}}
	simulation.RunWith(1, opts, func() {
		addrCh := make(chan string, 1)
		srvDone := make(chan struct{})

		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
			go func() {
				defer close(srvDone)
				defer ln.Close()
				c, err := ln.Accept()
				if err != nil {
					t.Errorf("accept: %v", err)
					return
				}
				tc := c.(dstHalfConn)
				req, err := io.ReadAll(tc)
				if err != nil || string(req) != "request-payload" {
					t.Errorf("server ReadAll = %q, %v; want request-payload, nil", req, err)
					return
				}
				// Reverse direction remains open after client's CloseWrite FIN.
				if _, err := tc.Write([]byte("response-payload")); err != nil {
					t.Errorf("server Write after client FIN: %v", err)
					return
				}
				if err := tc.CloseWrite(); err != nil {
					t.Errorf("server CloseWrite: %v", err)
					return
				}
				tc.Close()
			}()
		})

		simulation.Host("cli", func() {
			c, err := Dial("tcp", <-addrCh)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			tc := c.(dstHalfConn)
			if _, err := tc.Write([]byte("request-payload")); err != nil {
				t.Fatalf("client write: %v", err)
			}
			if err := tc.CloseWrite(); err != nil {
				t.Fatalf("first CloseWrite: %v", err)
			}
			// Repeated CloseWrite while reverse half is still open is idempotent.
			if err := tc.CloseWrite(); err != nil {
				t.Fatalf("second CloseWrite: %v", err)
			}
			if _, err := tc.Write([]byte("late")); !errors.Is(err, syscall.EPIPE) {
				t.Fatalf("Write after CloseWrite = %v, want EPIPE", err)
			}
			resp, err := io.ReadAll(tc)
			if err != nil || string(resp) != "response-payload" {
				t.Fatalf("client ReadAll = %q, %v; want response-payload, nil", resp, err)
			}
			// Once both FINs have completed, socket is in TIME_WAIT/TCP_CLOSE,
			// so further shutdown returns ENOTCONN just like Linux inet_shutdown.
			if err := tc.CloseWrite(); !errors.Is(err, syscall.ENOTCONN) {
				t.Fatalf("CloseWrite after both FINs = %v, want ENOTCONN", err)
			}
			if err := tc.Close(); err != nil {
				t.Fatalf("client Close: %v", err)
			}
			<-srvDone
		})
	})
}

func TestDSTNetTCPConnCloseWritePartitionAndDeadPeer(t *testing.T) {
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHost:         simulation.LinkConfig{Latency: 10 * time.Millisecond},
		RetransmitTimeout: 300 * time.Millisecond,
	}}

	// Subcase 1: Partitioned CloseWrite retransmits FIN until ETIMEDOUT.
	simulation.RunWith(1, opts, func() {
		addrCh := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
			go func() {
				c, err := ln.Accept()
				if err == nil {
					<-done
					c.Close()
				}
				ln.Close()
			}()
		})
		simulation.Host("cli", func() {
			c, err := Dial("tcp", <-addrCh)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			tc := c.(dstHalfConn)
			simulation.Partition("cli", "srv")
			if err := tc.CloseWrite(); err != nil {
				t.Fatalf("CloseWrite under partition: %v", err)
			}
			buf := make([]byte, 8)
			_, err = tc.Read(buf)
			if !errors.Is(err, syscall.ETIMEDOUT) {
				t.Fatalf("first Read after unacknowledged FIN = %v, want ETIMEDOUT", err)
			}
			_, err = tc.Read(buf)
			if err != io.EOF {
				t.Fatalf("second Read after ETIMEDOUT consumed = %v, want io.EOF", err)
			}
			tc.Close()
			close(done)
		})
	})

	// Subcase 2: CloseWrite against crashed peer host triggers retransmit horizon;
	// CloseWrite after peer RST returns ENOTCONN without consuming ECONNRESET.
	simulation.RunWith(2, opts, func() {
		addrCh := make(chan string, 1)
		srvConnCh := make(chan dstHalfConn, 1)
		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
			go func() {
				for range 2 {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					srvConnCh <- c.(dstHalfConn)
				}
			}()
		})
		simulation.Host("cli", func() {
			addr := <-addrCh
			// Peer RST before CloseWrite:
			c1, err := Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial 1: %v", err)
			}
			tc1 := c1.(dstHalfConn)
			sc1 := <-srvConnCh
			if _, err := tc1.Write([]byte("unread")); err != nil {
				t.Fatalf("write 1: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
			sc1.Close() // unread data -> RST
			time.Sleep(20 * time.Millisecond)
			if err := tc1.CloseWrite(); !errors.Is(err, syscall.ENOTCONN) {
				t.Fatalf("CloseWrite after RST = %v, want ENOTCONN", err)
			}
			if err := tc1.CloseRead(); !errors.Is(err, syscall.ENOTCONN) {
				t.Fatalf("CloseRead after RST = %v, want ENOTCONN", err)
			}
			if _, err := tc1.Read(make([]byte, 4)); !errors.Is(err, syscall.ECONNRESET) {
				t.Fatalf("Read after failed shutdown = %v, want preserved ECONNRESET", err)
			}
			tc1.Close()

			// Crashed peer host before CloseWrite:
			c2, err := Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial 2: %v", err)
			}
			tc2 := c2.(dstHalfConn)
			<-srvConnCh
			simulation.CrashHost("srv")
			if err := tc2.CloseWrite(); err != nil {
				t.Fatalf("CloseWrite into crashed host: %v", err)
			}
			if _, err := tc2.Read(make([]byte, 4)); !errors.Is(err, syscall.ETIMEDOUT) {
				t.Fatalf("Read after CloseWrite into crashed host = %v, want ETIMEDOUT", err)
			}
			tc2.Close()
		})
	})
}

func TestDSTNetTCPConnCloseReadSemantics(t *testing.T) {
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHost: simulation.LinkConfig{Latency: 10 * time.Millisecond},
	}}
	simulation.RunWith(1, opts, func() {
		addrCh := make(chan string, 1)
		srvConnCh := make(chan dstHalfConn, 2)
		done := make(chan struct{})

		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					srvConnCh <- c.(dstHalfConn)
				}
			}()
			go func() {
				<-done
				ln.Close()
			}()
		})

		simulation.Host("cli", func() {
			addr := <-addrCh
			// 1. Arrived bytes drain first after CloseRead, then Read returns io.EOF;
			// late peer write + local Close emits RST to peer.
			c1, err := Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial 1: %v", err)
			}
			tc1 := c1.(dstHalfConn)
			sc1 := <-srvConnCh

			if _, err := sc1.Write([]byte("arrived")); err != nil {
				t.Fatalf("srv write arrived: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
			if err := tc1.CloseRead(); err != nil {
				t.Fatalf("CloseRead: %v", err)
			}
			buf := make([]byte, 16)
			n, err := tc1.Read(buf)
			if err != nil || string(buf[:n]) != "arrived" {
				t.Fatalf("first Read after CloseRead = (%q, %v), want (arrived, nil)", buf[:n], err)
			}
			n, err = tc1.Read(buf)
			if n != 0 || err != io.EOF {
				t.Fatalf("second Read after CloseRead drained = (%d, %v), want (0, io.EOF)", n, err)
			}
			// Peer writes more bytes that sit unread in tc1's receive queue; closing tc1 emits RST.
			if _, err := sc1.Write([]byte("unread-after-shut-rd")); err != nil {
				t.Fatalf("srv write late: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
			if err := tc1.Close(); err != nil {
				t.Fatalf("tc1 Close: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
			if _, err := sc1.Read(buf); !errors.Is(err, syscall.ECONNRESET) {
				t.Fatalf("peer Read after Close with unread post-CloseRead bytes = %v, want ECONNRESET", err)
			}
			sc1.Close()

			// 2. Parked Read wakes immediately when concurrent goroutine calls CloseRead.
			c2, err := Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial 2: %v", err)
			}
			tc2 := c2.(dstHalfConn)
			sc2 := <-srvConnCh
			readDone := make(chan error, 1)
			go func() {
				_, rerr := tc2.Read(make([]byte, 8))
				readDone <- rerr
			}()
			time.Sleep(5 * time.Millisecond)
			if err := tc2.CloseRead(); err != nil {
				t.Fatalf("CloseRead 2: %v", err)
			}
			if rerr := <-readDone; rerr != io.EOF {
				t.Fatalf("parked Read woken by CloseRead = %v, want io.EOF", rerr)
			}
			tc2.Close()
			sc2.Close()
			close(done)
		})
	})
}

func TestDSTNetHalfCloseCloseWaitHorizonDeath(t *testing.T) {
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHost:         simulation.LinkConfig{Latency: 10 * time.Millisecond},
		RetransmitTimeout: 200 * time.Millisecond,
		SendBuffer:        64,
	}}
	simulation.RunWith(1, opts, func() {
		addrCh := make(chan string, 1)
		srvReady := make(chan struct{})
		done := make(chan struct{})

		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				tc := c.(dstHalfConn)
				// Server half-closes write -> FIN arrives at client -> client enters CLOSE_WAIT.
				if err := tc.CloseWrite(); err != nil {
					t.Errorf("srv CloseWrite: %v", err)
				}
				close(srvReady)
				<-done
				tc.Close()
				ln.Close()
			}()
		})

		simulation.Host("cli", func() {
			c, err := Dial("tcp", <-addrCh)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			tc := c.(dstHalfConn)
			<-srvReady
			time.Sleep(25 * time.Millisecond) // let server's FIN arrive -> CLOSE_WAIT

			buf := make([]byte, 8)
			if n, err := tc.Read(buf); n != 0 || err != io.EOF {
				t.Fatalf("Read in CLOSE_WAIT = (%d, %v), want (0, io.EOF)", n, err)
			}

			// Partition link and overrun send buffer in CLOSE_WAIT so Write blocks
			// until the retransmit horizon kills the socket.
			simulation.Partition("cli", "srv")
			payload := make([]byte, 256)
			_, werr := tc.Write(payload)
			if !errors.Is(werr, syscall.ETIMEDOUT) {
				t.Fatalf("Write in CLOSE_WAIT across permanent cut = %v, want one-shot ETIMEDOUT", werr)
			}
			// Reads continue returning io.EOF per SOCK_DONE; subsequent writes return EPIPE.
			if n, rerr := tc.Read(buf); n != 0 || rerr != io.EOF {
				t.Fatalf("Read after CLOSE_WAIT horizon death = (%d, %v), want (0, io.EOF)", n, rerr)
			}
			if _, werr2 := tc.Write([]byte("x")); !errors.Is(werr2, syscall.EPIPE) {
				t.Fatalf("second Write after CLOSE_WAIT horizon death = %v, want EPIPE", werr2)
			}
			tc.Close()
			close(done)
		})
	})
}

func TestDSTNetNonBubbleGoroutineUsesRealHostNetwork(t *testing.T) {
	inRun := make(chan struct{})
	outsideDone := make(chan error, 1)

	go func() {
		<-inRun
		// Running concurrently with an active simulation bubble, a non-bubble
		// goroutine must reach the real OS resolver and loopback stack rather
		// than the simulated network or DNS fences.
		var dialed atomic.Bool
		r := &Resolver{
			PreferGo: true,
			Dial: func(context.Context, string, string) (Conn, error) {
				dialed.Store(true)
				return nil, errors.New("custom-resolver-dial")
			},
		}
		if _, err := r.LookupHost(context.Background(), "example.com"); isDSTUnsupportedDNSLookup(err) || !dialed.Load() {
			outsideDone <- errors.New("non-bubble Resolver.LookupHost was intercepted by simulation fence")
			return
		}
		ln, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			outsideDone <- err
			return
		}
		defer ln.Close()
		accepted := make(chan error, 1)
		go func() {
			sc, aerr := ln.Accept()
			if aerr != nil {
				accepted <- aerr
				return
			}
			defer sc.Close()
			b, rerr := io.ReadAll(sc)
			if rerr != nil {
				accepted <- rerr
				return
			}
			if string(b) != "real-kernel" {
				accepted <- errors.New("unexpected payload: " + string(b))
				return
			}
			accepted <- nil
		}()
		cc, err := Dial("tcp", ln.Addr().String())
		if err != nil {
			outsideDone <- err
			return
		}
		if _, err := cc.Write([]byte("real-kernel")); err != nil {
			cc.Close()
			outsideDone <- err
			return
		}
		cc.Close()
		outsideDone <- <-accepted
	}()

	simulation.Run(1, func() {
		ln, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("bubble listen: %v", err)
		}
		defer ln.Close()
		close(inRun)
		if err := <-outsideDone; err != nil {
			t.Fatalf("non-bubble host network operation failed during active simulation: %v", err)
		}
	})
}

func TestDSTNetLocalhostResolutionInBubble(t *testing.T) {
	ctx := context.Background()
	simulation.Run(1, func() {
		for _, name := range []string{"localhost", "LOCALHOST", "localhost.", "LoCaLhOsT."} {
			hosts, err := LookupHost(name)
			if err != nil {
				t.Fatalf("LookupHost(%q) = %v", name, err)
			}
			if len(hosts) != 2 || hosts[0] != "127.0.0.1" || hosts[1] != "::1" {
				t.Fatalf("LookupHost(%q) = %v, want [127.0.0.1 ::1]", name, hosts)
			}
			ips, err := LookupIP(name)
			if err != nil || len(ips) != 2 {
				t.Fatalf("LookupIP(%q) = %v, %v", name, ips, err)
			}
			v4, err := DefaultResolver.LookupNetIP(ctx, "ip4", name)
			if err != nil || len(v4) != 1 || v4[0] != netip.MustParseAddr("127.0.0.1") {
				t.Fatalf("LookupNetIP(ip4, %q) = %v, %v", name, v4, err)
			}
			v6, err := DefaultResolver.LookupNetIP(ctx, "ip6", name)
			if err != nil || len(v6) != 1 || v6[0] != netip.MustParseAddr("::1") {
				t.Fatalf("LookupNetIP(ip6, %q) = %v, %v", name, v6, err)
			}
			ta4, err := ResolveTCPAddr("tcp4", JoinHostPort(name, "8080"))
			if err != nil || !ta4.IP.Equal(IPv4(127, 0, 0, 1)) || ta4.Port != 8080 {
				t.Fatalf("ResolveTCPAddr(tcp4, %q) = %v, %v", name, ta4, err)
			}
			ta6, err := ResolveTCPAddr("tcp6", JoinHostPort(name, "8080"))
			if err != nil || !ta6.IP.Equal(IPv6loopback) || ta6.Port != 8080 {
				t.Fatalf("ResolveTCPAddr(tcp6, %q) = %v, %v", name, ta6, err)
			}
		}

		// Dialing "localhost:<port>" inside the bubble connects cleanly to loopback listener.
		ln, err := Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		_, port, _ := SplitHostPort(ln.Addr().String())
		done := make(chan struct{})
		go func() {
			defer close(done)
			sc, err := ln.Accept()
			if err == nil {
				sc.Close()
			}
		}()
		cc, err := Dial("tcp4", "localhost:"+port)
		if err != nil {
			t.Fatalf("Dial(localhost:%s) = %v", port, err)
		}
		cc.Close()
		<-done

		// Arbitrary DNS names remain deterministically refused.
		if _, err := LookupHost("localhost.example.com"); !isDSTUnsupportedDNSLookup(err) {
			t.Fatalf("LookupHost(localhost.example.com) = %v, want unsupported DNS error", err)
		}
	})
}

func TestDSTNetCloseWriteUnreadRSTPartitionAndInFlightFIN(t *testing.T) {
	// Subcase A: CloseWrite + unread-inbound Close across an active blackhole
	// partition must suppress both FIN and RST across the cut.
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{
		CrossHost:         simulation.LinkConfig{Latency: 10 * time.Millisecond},
		RetransmitTimeout: 250 * time.Millisecond,
	}}, func() {
		addrCh := make(chan string, 1)
		srvConnCh := make(chan dstHalfConn, 1)
		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
			go func() {
				c, aerr := ln.Accept()
				if aerr == nil {
					srvConnCh <- c.(dstHalfConn)
				}
				ln.Close()
			}()
		})
		simulation.Host("cli", func() {
			c, err := Dial("tcp", <-addrCh)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			tc := c.(dstHalfConn)
			sc := <-srvConnCh
			if _, err := tc.Write([]byte("req")); err != nil {
				t.Fatalf("cli write req: %v", err)
			}
			time.Sleep(20 * time.Millisecond) // "req" arrives unread in sc's receive buffer

			simulation.Partition("srv", "cli")
			if err := sc.CloseWrite(); err != nil {
				t.Fatalf("srv CloseWrite under partition: %v", err)
			}
			if err := sc.Close(); err != nil {
				t.Fatalf("srv Close with unread under partition: %v", err)
			}

			// Neither FIN (io.EOF) nor RST (ECONNRESET/EPIPE) may traverse the blackhole.
			_ = tc.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
			if n, rerr := tc.Read(make([]byte, 8)); !errors.Is(rerr, os.ErrDeadlineExceeded) || n != 0 {
				t.Fatalf("cli Read across blackhole = (%d, %v), want (0, ErrDeadlineExceeded)", n, rerr)
			}
			_ = tc.SetReadDeadline(time.Time{})
			if _, werr := tc.Write([]byte("more")); werr != nil {
				t.Fatalf("cli buffered Write across blackhole failed instantaneously: %v", werr)
			}
			if _, rerr := tc.Read(make([]byte, 8)); !errors.Is(rerr, syscall.ETIMEDOUT) {
				t.Fatalf("cli Read waiting on unacked Write across blackhole = %v, want ETIMEDOUT", rerr)
			}
			tc.Close()
		})
	})

	// Subcase B: CloseWrite + unread-inbound Close when FIN is STILL IN FLIGHT
	// (no partition): peer has not entered CLOSE_WAIT yet, so an immediate Write
	// before FIN arrival reports one-shot ECONNRESET, then EPIPE.
	simulation.RunWith(2, simulation.Options{Network: simulation.NetworkConfig{
		CrossHost: simulation.LinkConfig{Latency: 100 * time.Millisecond},
	}}, func() {
		addrCh := make(chan string, 1)
		srvConnCh := make(chan dstHalfConn, 1)
		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
			go func() {
				c, aerr := ln.Accept()
				if aerr == nil {
					srvConnCh <- c.(dstHalfConn)
				}
				ln.Close()
			}()
		})
		simulation.Host("cli", func() {
			c, err := Dial("tcp", <-addrCh)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			tc := c.(dstHalfConn)
			sc := <-srvConnCh
			if _, err := tc.Write([]byte("req")); err != nil {
				t.Fatalf("cli write req: %v", err)
			}
			time.Sleep(120 * time.Millisecond) // "req" delivered unread at srv

			// srv sends FIN (due in +100ms) and immediately closes with unread bytes (RST).
			if err := sc.CloseWrite(); err != nil {
				t.Fatalf("srv CloseWrite: %v", err)
			}
			if err := sc.Close(); err != nil {
				t.Fatalf("srv Close: %v", err)
			}

			// Only 10ms later, FIN has NOT arrived at cli (cli is still ESTABLISHED, not CLOSE_WAIT).
			time.Sleep(10 * time.Millisecond)
			if _, werr := tc.Write([]byte("more")); !errors.Is(werr, syscall.ECONNRESET) {
				t.Fatalf("first cli Write before FIN arrival = %v, want ECONNRESET", werr)
			}
			if _, werr2 := tc.Write([]byte("again")); !errors.Is(werr2, syscall.EPIPE) {
				t.Fatalf("second cli Write = %v, want EPIPE", werr2)
			}
			tc.Close()
		})
	})

	// Subcase C: CloseWrite FIN ALREADY DELIVERED to peer (peer in CLOSE_WAIT)
	// before unread-inbound Close: tcp_reset's CLOSE_WAIT arm reports io.EOF / EPIPE
	// without ECONNRESET.
	simulation.RunWith(3, simulation.Options{Network: simulation.NetworkConfig{
		CrossHost: simulation.LinkConfig{Latency: 10 * time.Millisecond},
	}}, func() {
		addrCh := make(chan string, 1)
		srvConnCh := make(chan dstHalfConn, 1)
		simulation.Host("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, port, _ := SplitHostPort(ln.Addr().String())
			addrCh <- simulation.HostIP("srv") + ":" + port
			go func() {
				c, aerr := ln.Accept()
				if aerr == nil {
					srvConnCh <- c.(dstHalfConn)
				}
				ln.Close()
			}()
		})
		simulation.Host("cli", func() {
			c, err := Dial("tcp", <-addrCh)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			tc := c.(dstHalfConn)
			sc := <-srvConnCh
			if _, err := tc.Write([]byte("req")); err != nil {
				t.Fatalf("cli write req: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
			if err := sc.CloseWrite(); err != nil {
				t.Fatalf("srv CloseWrite: %v", err)
			}
			time.Sleep(20 * time.Millisecond) // FIN delivered -> cli is in CLOSE_WAIT
			if err := sc.Close(); err != nil {
				t.Fatalf("srv Close: %v", err)
			}
			if n, rerr := tc.Read(make([]byte, 8)); n != 0 || rerr != io.EOF {
				t.Fatalf("cli Read in CLOSE_WAIT after peer unread-RST = (%d, %v), want (0, io.EOF)", n, rerr)
			}
			if _, werr := tc.Write([]byte("after")); !errors.Is(werr, syscall.EPIPE) {
				t.Fatalf("first cli Write in CLOSE_WAIT after peer unread-RST = %v, want EPIPE", werr)
			}
			tc.Close()
		})
	})
}

func TestDSTNetListenerCloseBlackholeSuppressesRSTAcrossAllRaces(t *testing.T) {
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHost:         simulation.LinkConfig{Latency: 20 * time.Millisecond},
		RetransmitTimeout: 200 * time.Millisecond,
	}}

	// 1. Backlog-parked dialer + PartitionOneWay("srv", "cli") + ln.Close() across seeds 1..8
	// (exercising both <-partWake and <-l.done select choices): must NEVER return ECONNREFUSED
	// across the blackholed return leg, timing out with ETIMEDOUT instead.
	for seed := uint64(1); seed <= 8; seed++ {
		simulation.RunWith(seed, opts, func() {
			var ln Listener
			var addr string
			simulation.Host("srv", func() {
				var err error
				ln, err = Listen("tcp", ":0")
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				_, port, _ := SplitHostPort(ln.Addr().String())
				addr = simulation.HostIP("srv") + ":" + port
			})

			const backlog = 128
			held := make([]Conn, 0, backlog)
			simulation.Host("cli", func() {
				for range backlog {
					c, err := Dial("tcp", addr)
					if err != nil {
						t.Fatalf("saturate backlog dial: %v", err)
					}
					held = append(held, c)
				}
			})

			overflowErr := make(chan error, 1)
			simulation.Host("cli", func() {
				go func() {
					c, err := Dial("tcp", addr)
					if c != nil {
						c.Close()
					}
					overflowErr <- err
				}()
			})
			// Let the 129th dial complete SYN (20ms) and park on full backlogSem.
			time.Sleep(25 * time.Millisecond)

			// Inject srv->cli blackhole AND close listener before the parked dialer runs,
			// making both <-partWake and <-l.done ready simultaneously.
			simulation.Host("srv", func() {
				simulation.PartitionOneWay("srv", "cli")
				_ = ln.Close()
			})

			err := <-overflowErr
			if !errors.Is(err, syscall.ETIMEDOUT) {
				t.Fatalf("seed %d: backlog-parked dial across blackholed ln.Close() = %v, want ETIMEDOUT (no RST across blackhole)", seed, err)
			}
			// TCP never retransmits RST: once srv->cli heals, an idle unaccepted client
			// that sends no traffic must NOT receive a delayed ECONNRESET from the
			// blackholed ln.Close().
			simulation.HealLink("srv", "cli")
			_ = held[0].SetReadDeadline(time.Now().Add(30 * time.Millisecond))
			if _, rerr := held[0].Read(make([]byte, 8)); !errors.Is(rerr, os.ErrDeadlineExceeded) {
				t.Fatalf("seed %d: idle backlog conn Read after HealLink = %v, want ErrDeadlineExceeded (dropped RST is never retransmitted)", seed, rerr)
			}
			for _, c := range held {
				c.Close()
			}
		})
	}

	// 2. Post-SYN-ACK / parked Accept race against Partition("srv", "cli") + ln.Close()
	// across seeds 1..16: once SYN-ACK has arrived before the cut, Dial succeeds and
	// no ECONNREFUSED or ECONNRESET crosses the blackhole while partitioned.
	for seed := uint64(1); seed <= 16; seed++ {
		simulation.RunWith(seed, opts, func() {
			var ln Listener
			var addr string
			acceptDone := make(chan struct{})
			simulation.Host("srv", func() {
				var err error
				ln, err = Listen("tcp", ":0")
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				_, port, _ := SplitHostPort(ln.Addr().String())
				addr = simulation.HostIP("srv") + ":" + port
				go func() {
					defer close(acceptDone)
					c, aerr := ln.Accept()
					if aerr == nil {
						c.Close()
					}
				}()
			})

			dialRes := make(chan struct {
				c   Conn
				err error
			}, 1)
			simulation.Host("cli", func() {
				go func() {
					c, err := Dial("tcp", addr)
					dialRes <- struct {
						c   Conn
						err error
					}{c, err}
				}()
			})

			// At t=40ms (1 RTT = 2 * 20ms), SYN-ACK arrives and races Accept + Partition + ln.Close.
			time.Sleep(40 * time.Millisecond)
			simulation.Host("srv", func() {
				simulation.Partition("srv", "cli")
				_ = ln.Close()
			})

			res := <-dialRes
			<-acceptDone
			if res.err != nil {
				if !errors.Is(res.err, syscall.ETIMEDOUT) {
					t.Fatalf("seed %d: dial racing blackholed ln.Close() = %v, want nil or ETIMEDOUT (never ECONNREFUSED)", seed, res.err)
				}
				return
			}
			defer res.c.Close()
			// While partitioned, neither Accept's abort nor ln.Close()'s backlog drain
			// may have delivered ECONNRESET across the blackhole.
			_ = res.c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
			if _, rerr := res.c.Read(make([]byte, 8)); !errors.Is(rerr, os.ErrDeadlineExceeded) {
				t.Fatalf("seed %d: established dialer Read across blackholed ln.Close() = %v, want ErrDeadlineExceeded", seed, rerr)
			}
		})
	}
}
