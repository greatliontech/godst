// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package net

// The socket-option layer and keepalive death law's conformance suite — the
// modeled-behavior successor of the option-fence cases that
// TestDSTNetRejectsUnsupportedOptions carried before Control and keepalive
// were modeled: every surface that used to be refused now asserts its
// modeled semantics — the Control/ControlContext invocation contract, raw
// sockopt observability on virtual socket descriptors (the
// golang.org/x/sys/unix path: the named wrappers ride the same trampolines),
// option-granular refusal of the unmodeled (ENOPROTOOPT), accept(2)
// inheritance, the keepalive ETIMEDOUT death under cuts and dead hosts with
// its one-shot sk_err ladder, and TCP_USER_TIMEOUT's horizon override and
// zero-window death.

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// tcpUserTimeout is TCP_USER_TIMEOUT's number (linux uapi tcp.h; absent
// from the frozen zerrors tables).
const tcpUserTimeout = 18

// dstKeepaliveServer starts a one-connection echo-less server on host A that
// accepts, optionally serves one initial one-byte exchange, then parks until
// done. It reports the listen port.
func dstKeepaliveServer(port chan<- string, done <-chan struct{}, exchange bool, lc ListenConfig, acceptedConn chan<- Conn) {
	simulation.Host("A", simulation.HostConfig{}, func() {
		ln, err := lc.Listen(context.Background(), "tcp", ":0")
		if err != nil {
			panic(err)
		}
		_, p, _ := SplitHostPort(ln.Addr().String())
		port <- p
		go func() {
			c, err := ln.Accept()
			if err != nil {
				panic(err)
			}
			if acceptedConn != nil {
				acceptedConn <- c
			}
			if exchange {
				buf := make([]byte, 1)
				if _, err := c.Read(buf); err != nil {
					panic(err)
				}
				if _, err := c.Write(buf); err != nil {
					panic(err)
				}
			}
			<-done
			c.Close()
			ln.Close()
		}()
	})
}

// TestDSTNetControlSockopts: a Dialer.Control callback runs with
// production's (network, address) contract and a working virtual socket
// descriptor — the modeled options are settable and readable back (the
// kernel defaults visible before any write), an out-of-range parameter is
// EINVAL, and an option outside the modeled set is ENOPROTOOPT rather than
// silently stored.
func TestDSTNetControlSockopts(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var gotNetwork, gotAddress string
	var defIdle, defIntvl, defCnt, defKA, setKA, setIdle int
	var badIdleErr, nodelayErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		dstKeepaliveServer(port, done, false, ListenConfig{}, nil)
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			target := simulation.HostIP("A") + ":" + p
			d := Dialer{Control: func(network, address string, rc syscall.RawConn) error {
				gotNetwork, gotAddress = network, address
				return rc.Control(func(fd uintptr) {
					s := int(fd)
					defKA, _ = syscall.GetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
					defIdle, _ = syscall.GetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
					defIntvl, _ = syscall.GetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL)
					defCnt, _ = syscall.GetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT)
					if err := syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, 1); err != nil {
						panic(err)
					}
					if err := syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, 30); err != nil {
						panic(err)
					}
					setKA, _ = syscall.GetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
					setIdle, _ = syscall.GetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
					badIdleErr = syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, 0)
					nodelayErr = syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
				})
			}, KeepAlive: -1}
			c, err := d.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			if gotAddress != target {
				panic("Control address = " + gotAddress + ", want " + target)
			}
			c.Close()
			close(done)
		})
	})
	if gotNetwork != "tcp4" {
		t.Errorf("Control network = %q, want tcp4", gotNetwork)
	}
	if defKA != 0 || defIdle != 7200 || defIntvl != 75 || defCnt != 9 {
		t.Errorf("kernel defaults = KA %d idle %d intvl %d cnt %d, want 0/7200/75/9", defKA, defIdle, defIntvl, defCnt)
	}
	if setKA != 1 || setIdle != 30 {
		t.Errorf("after set: KA %d idle %d, want 1/30", setKA, setIdle)
	}
	if !errors.Is(badIdleErr, syscall.EINVAL) {
		t.Errorf("TCP_KEEPIDLE=0 error = %v, want EINVAL", badIdleErr)
	}
	if !errors.Is(nodelayErr, syscall.ENOPROTOOPT) {
		t.Errorf("TCP_NODELAY error = %v, want ENOPROTOOPT (unmodeled options are refused, never silently stored)", nodelayErr)
	}
}

// TestDSTNetControlContextAndErrors: ControlContext receives the dial's
// context; a Control error aborts the dial (and a ListenConfig.Control
// error the listen) wrapped in production's OpError shape.
func TestDSTNetControlContextAndErrors(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	type ctxKey struct{}
	var gotCtxVal any
	sentinel := errors.New("control refused")
	var dialErr, listenErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		dstKeepaliveServer(port, done, false, ListenConfig{}, nil)
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			target := simulation.HostIP("A") + ":" + p
			d := Dialer{ControlContext: func(ctx context.Context, _, _ string, rc syscall.RawConn) error {
				gotCtxVal = ctx.Value(ctxKey{})
				return nil
			}}
			ctx := context.WithValue(context.Background(), ctxKey{}, "carried")
			c, err := d.DialContext(ctx, "tcp", target)
			if err != nil {
				panic(err)
			}
			c.Close()

			bad := Dialer{Control: func(string, string, syscall.RawConn) error { return sentinel }}
			_, dialErr = bad.Dial("tcp", target)

			lc := ListenConfig{Control: func(string, string, syscall.RawConn) error { return sentinel }}
			_, listenErr = lc.Listen(context.Background(), "tcp", ":0")
		})
	})
	if gotCtxVal != "carried" {
		t.Errorf("ControlContext ctx value = %v, want carried", gotCtxVal)
	}
	var opErr *OpError
	if !errors.As(dialErr, &opErr) || opErr.Op != "dial" || !errors.Is(dialErr, sentinel) {
		t.Errorf("dial Control error = %v, want OpError{Op: dial} wrapping the callback error", dialErr)
	}
	if !errors.As(listenErr, &opErr) || opErr.Op != "listen" || !errors.Is(listenErr, sentinel) {
		t.Errorf("listen Control error = %v, want OpError{Op: listen} wrapping the callback error", listenErr)
	}
}

// TestDSTNetAcceptInheritsListenerSockopts: accept(2) inheritance — a
// ListenConfig.Control write on the listening socket is visible on the
// accepted connection's own descriptor (SyscallConn), and the Go-level
// ListenConfig.KeepAliveConfig resolution is applied on top of it.
func TestDSTNetAcceptInheritsListenerSockopts(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var inheritedUTO, appliedIdle, appliedKA int
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		accepted := make(chan Conn, 1)
		lc := ListenConfig{
			Control: func(_, _ string, rc syscall.RawConn) error {
				return rc.Control(func(fd uintptr) {
					if err := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, 12345); err != nil {
						panic(err)
					}
				})
			},
			KeepAliveConfig: KeepAliveConfig{Enable: true, Idle: 20 * time.Second},
		}
		dstKeepaliveServer(port, done, false, lc, accepted)
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			c, err := Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			defer c.Close()
			sc := (<-accepted).(syscall.Conn)
			rc, err := sc.SyscallConn()
			if err != nil {
				panic(err)
			}
			rc.Control(func(fd uintptr) {
				inheritedUTO, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout)
				appliedKA, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
				appliedIdle, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
			})
		})
	})
	if inheritedUTO != 12345 {
		t.Errorf("accepted conn TCP_USER_TIMEOUT = %d, want the listener's 12345 (accept inheritance)", inheritedUTO)
	}
	if appliedKA != 1 || appliedIdle != 20 {
		t.Errorf("accepted conn KA/idle = %d/%d, want 1/20 (ListenConfig resolution)", appliedKA, appliedIdle)
	}
}

// TestDSTNetGoKeepAliveResolution: the Go-level configuration resolves as
// production's newTCPConn does — Dialer.KeepAlive sets Idle with the
// interval/count defaults; an explicit KeepAliveConfig sets each field —
// observable through the established conn's descriptor.
func TestDSTNetGoKeepAliveResolution(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	read := func(c Conn) (ka, idle, intvl, cnt int) {
		rc, err := c.(syscall.Conn).SyscallConn()
		if err != nil {
			panic(err)
		}
		rc.Control(func(fd uintptr) {
			s := int(fd)
			ka, _ = syscall.GetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
			idle, _ = syscall.GetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE)
			intvl, _ = syscall.GetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL)
			cnt, _ = syscall.GetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT)
		})
		return
	}
	var defKA, defIdle, defIntvl, defCnt int
	var durKA, durIdle, durIntvl int
	var cfgIdle, cfgIntvl, cfgCnt int
	var offKA int
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				for range 4 {
					c, err := ln.Accept()
					if err != nil {
						panic(err)
					}
					defer c.Close()
				}
				<-done
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			target := simulation.HostIP("A") + ":" + p

			c, err := Dial("tcp", target) // plain Dial: Go default keepalive
			if err != nil {
				panic(err)
			}
			defer c.Close()
			defKA, defIdle, defIntvl, defCnt = read(c)

			d := Dialer{KeepAlive: 30 * time.Second}
			c2, err := d.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			defer c2.Close()
			durKA, durIdle, durIntvl = func() (int, int, int) { a, b, cc, _ := read(c2); return a, b, cc }()

			d3 := Dialer{KeepAliveConfig: KeepAliveConfig{Enable: true, Idle: 40 * time.Second, Interval: 3 * time.Second, Count: 2}}
			c3, err := d3.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			defer c3.Close()
			_, cfgIdle, cfgIntvl, cfgCnt = read(c3)

			d4 := Dialer{KeepAlive: -1} // disabled: kernel defaults stand, not enabled
			c4, err := d4.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			defer c4.Close()
			offKA, _, _, _ = read(c4)
		})
	})
	if defKA != 1 || defIdle != 15 || defIntvl != 15 || defCnt != 9 {
		t.Errorf("plain Dial keepalive = %d/%d/%d/%d, want 1/15/15/9 (production's default enablement)", defKA, defIdle, defIntvl, defCnt)
	}
	if durKA != 1 || durIdle != 30 || durIntvl != 15 {
		t.Errorf("KeepAlive=30s resolution = %d/%d/%d, want 1/30/15", durKA, durIdle, durIntvl)
	}
	if cfgIdle != 40 || cfgIntvl != 3 || cfgCnt != 2 {
		t.Errorf("KeepAliveConfig resolution = idle %d intvl %d cnt %d, want 40/3/2", cfgIdle, cfgIntvl, cfgCnt)
	}
	if offKA != 0 {
		t.Errorf("KeepAlive=-1 leaves SO_KEEPALIVE = %d, want 0", offKA)
	}
}

// TestDSTNetKeepaliveDeathUnderCut: the death law — an idle connection
// (grpc's recipe: KeepAlive=-1 plus a Control enabling SO_KEEPALIVE, with
// explicit parameters) whose link is cut dies ETIMEDOUT when the probe
// schedule exhausts: max(cut observation, activity+idle) + interval×count.
// The identity is the one-shot sk_err ladder: first failing op ETIMEDOUT,
// later reads io.EOF, later writes EPIPE.
func TestDSTNetKeepaliveDeathUnderCut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var elapsed time.Duration
	var firstErr, secondErr, writeErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		dstKeepaliveServer(port, done, true, ListenConfig{}, nil)
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			d := Dialer{KeepAlive: -1, Control: func(_, _ string, rc syscall.RawConn) error {
				return rc.Control(func(fd uintptr) {
					s := int(fd)
					syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, 1)
					syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, 10)
					syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL, 5)
					syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT, 3)
				})
			}}
			c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			c.Write([]byte("x"))
			if _, err := c.Read(buf); err != nil { // the exchange: activity anchor
				panic(err)
			}
			start := time.Now()
			simulation.Partition("A", "B")
			_, firstErr = c.Read(buf) // parks; the keepalive watchdog kills at the schedule
			elapsed = time.Since(start)
			_, secondErr = c.Read(buf)
			_, writeErr = c.Write([]byte("y"))
			c.Close()
		})
	})
	if !errors.Is(firstErr, syscall.ETIMEDOUT) {
		t.Fatalf("read after keepalive exhaustion = %v, want ETIMEDOUT", firstErr)
	}
	// Death at activity+idle(10s)+intvl×cnt(15s) = 25s after the exchange;
	// the anchor is the cut's observation, marginally after the exchange.
	if elapsed < 24*time.Second || elapsed > 27*time.Second {
		t.Errorf("keepalive death after %v, want ~25s (idle 10s + 5s×3 probes)", elapsed)
	}
	if secondErr != io.EOF {
		t.Errorf("read after the one-shot = %v, want io.EOF", secondErr)
	}
	if !errors.Is(writeErr, syscall.EPIPE) {
		t.Errorf("write after the one-shot = %v, want EPIPE", writeErr)
	}
}

// TestDSTNetDefaultKeepaliveDeath: a PLAIN Dial carries production's default
// keepalive (15s/15s/9), so an idle conn under a permanent cut dies at
// ~150s — the death the pre-model base missed — while a KeepAlive=-1 conn
// (keepalive off, nothing outstanding) blocks to its own deadline.
func TestDSTNetDefaultKeepaliveDeath(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var kaElapsed time.Duration
	var kaErr, offErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				for range 2 {
					c, err := ln.Accept()
					if err != nil {
						panic(err)
					}
					defer c.Close()
				}
				<-done
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			target := simulation.HostIP("A") + ":" + p
			c, err := Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			off, err := (&Dialer{KeepAlive: -1}).Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			simulation.Partition("A", "B")
			start := time.Now()
			buf := make([]byte, 1)
			_, kaErr = c.Read(buf)
			kaElapsed = time.Since(start)
			off.SetReadDeadline(time.Now().Add(400 * time.Second))
			_, offErr = off.Read(buf)
			c.Close()
			off.Close()
		})
	})
	if !errors.Is(kaErr, syscall.ETIMEDOUT) {
		t.Fatalf("default-keepalive read under cut = %v, want ETIMEDOUT", kaErr)
	}
	if kaElapsed < 149*time.Second || kaElapsed > 155*time.Second {
		t.Errorf("default keepalive death after %v, want ~150s (15s idle + 15s×9)", kaElapsed)
	}
	if !errors.Is(offErr, os.ErrDeadlineExceeded) {
		t.Errorf("keepalive-off read under cut = %v, want its own deadline (no keepalive death)", offErr)
	}
}

// TestDSTNetKeepaliveHealAnswersProbes: a heal before the schedule exhausts
// answers the pending probes — the connection survives and delivers.
func TestDSTNetKeepaliveHealAnswersProbes(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var got string
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept()
				if err != nil {
					panic(err)
				}
				<-done
				c.Write([]byte("ok"))
				c.Close()
				ln.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			d := Dialer{KeepAlive: 10 * time.Second} // idle 10s, intvl 15s, cnt 9: death at 145s
			c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			simulation.Partition("A", "B")
			go func() {
				time.Sleep(60 * time.Second) // inside the schedule
				simulation.Heal("A", "B")
				close(done)
			}()
			buf := make([]byte, 2)
			n, err := c.Read(buf) // parks through the cut, survives the heal
			if err != nil {
				panic(err)
			}
			got = string(buf[:n])
			c.Close()
		})
	})
	if got != "ok" {
		t.Errorf("post-heal read = %q, want ok (probes answered, no death)", got)
	}
}

// TestDSTNetUserTimeoutOverridesHorizon: TCP_USER_TIMEOUT set through a
// Control callback replaces the run's RetransmitTimeout for data the cut
// holds — the conn dies at the socket's own bound, far before the default
// two-minute horizon.
func TestDSTNetUserTimeoutOverridesHorizon(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var elapsed time.Duration
	var readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		dstKeepaliveServer(port, done, true, ListenConfig{}, nil)
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			d := Dialer{KeepAlive: -1, Control: func(_, _ string, rc syscall.RawConn) error {
				return rc.Control(func(fd uintptr) {
					syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, 3000)
				})
			}}
			c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			c.Write([]byte("x"))
			if _, err := c.Read(buf); err != nil {
				panic(err)
			}
			simulation.Partition("A", "B")
			start := time.Now()
			c.Write([]byte("held")) // buffered into the cut: undeliverable
			_, readErr = c.Read(buf)
			elapsed = time.Since(start)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Fatalf("read holding dying bytes = %v, want ETIMEDOUT", readErr)
	}
	if elapsed < 3*time.Second || elapsed > 5*time.Second {
		t.Errorf("user-timeout death after %v, want ~3s (TCP_USER_TIMEOUT=3000ms, not the 2m default)", elapsed)
	}
}

// TestDSTNetUserTimeoutZeroWindowDeath: TCP_USER_TIMEOUT bounds a
// zero-window stall — a write parked on a full send buffer against a LIVE
// peer that never reads dies ETIMEDOUT at the socket's bound, the one
// per-socket exception to the persist-forever model.
func TestDSTNetUserTimeoutZeroWindowDeath(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var elapsed time.Duration
	var writeErr error
	opts := simulation.Options{Network: simulation.NetworkConfig{SendBuffer: 1 << 10}}
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		dstKeepaliveServer(port, done, false, ListenConfig{}, nil)
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			d := Dialer{KeepAlive: -1, Control: func(_, _ string, rc syscall.RawConn) error {
				return rc.Control(func(fd uintptr) {
					syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, 2000)
				})
			}}
			c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			start := time.Now()
			_, writeErr = c.Write(make([]byte, 8<<10)) // overruns the 1 KiB buffer, parks
			elapsed = time.Since(start)
			c.Close()
		})
	})
	if !errors.Is(writeErr, syscall.ETIMEDOUT) {
		t.Fatalf("zero-window write with user timeout = %v, want ETIMEDOUT", writeErr)
	}
	if elapsed < 2*time.Second || elapsed > 4*time.Second {
		t.Errorf("zero-window death after %v, want ~2s", elapsed)
	}
}

// TestDSTNetKeepaliveEnabledPostEstablish: SO_KEEPALIVE enabled through the
// established connection's SyscallConn (a stashed-RawConn shape) governs a
// read already parked — the option write pokes the parked op to arm the
// watchdog.
func TestDSTNetKeepaliveEnabledPostEstablish(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var readErr error
	var elapsed time.Duration
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		dstKeepaliveServer(port, done, true, ListenConfig{}, nil)
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			c.Write([]byte("x"))
			if _, err := c.Read(buf); err != nil {
				panic(err)
			}
			simulation.Partition("A", "B")
			start := time.Now()
			go func() {
				time.Sleep(5 * time.Second) // the read below is parked by now
				rc, err := c.(syscall.Conn).SyscallConn()
				if err != nil {
					panic(err)
				}
				rc.Control(func(fd uintptr) {
					s := int(fd)
					syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, 10)
					syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL, 5)
					syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT, 2)
					syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, 1)
				})
			}()
			_, readErr = c.Read(buf)
			elapsed = time.Since(start)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Fatalf("read after post-establish keepalive enable = %v, want ETIMEDOUT", readErr)
	}
	// Enabled at +5s with activity at ~0: the first probe waits out the
	// idle time from the ACTIVITY (10s), then 5s×2 of probing: death ~20s.
	if elapsed < 19*time.Second || elapsed > 23*time.Second {
		t.Errorf("post-establish keepalive death after %v, want ~20s", elapsed)
	}
}

// TestDSTNetListenControlContract: a ListenConfig.Control callback runs
// before bind with the socket's family network and the REQUESTED address
// (":0" form, the dual-stack wildcard reporting "tcp6"/"[::]:0" as
// production's AF_INET6 socket does), its writes land on the listening
// socket (readable through the listener's SyscallConn), and a callback
// error preempts any bind conflict. Raw Read/Write on a RawConn are refused
// in production's raw-* OpError shapes.
func TestDSTNetListenControlContract(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var gotNetwork, gotAddress string
	var listenerUTO int
	var rawReadErr, rawWriteErr error
	sentinel := errors.New("control refused")
	var precedenceErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		lc := ListenConfig{Control: func(network, address string, rc syscall.RawConn) error {
			gotNetwork, gotAddress = network, address
			return rc.Control(func(fd uintptr) {
				if err := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, 777); err != nil {
					panic(err)
				}
			})
		}}
		ln, err := lc.Listen(context.Background(), "tcp", ":0")
		if err != nil {
			panic(err)
		}
		defer ln.Close()
		rc, err := ln.(syscall.Conn).SyscallConn()
		if err != nil {
			panic(err)
		}
		rc.Control(func(fd uintptr) {
			listenerUTO, _ = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout)
		})
		rawReadErr = rc.Read(func(uintptr) bool { return true })
		rawWriteErr = rc.Write(func(uintptr) bool { return true })

		// A Control error preempts the bind conflict on the occupied port.
		_, p, _ := SplitHostPort(ln.Addr().String())
		bad := ListenConfig{Control: func(string, string, syscall.RawConn) error { return sentinel }}
		_, precedenceErr = bad.Listen(context.Background(), "tcp", ":"+p)
	})
	if gotNetwork != "tcp6" || gotAddress != "[::]:0" {
		t.Errorf("listen Control contract = (%q, %q), want (tcp6, [::]:0)", gotNetwork, gotAddress)
	}
	if listenerUTO != 777 {
		t.Errorf("listener sockopt readback = %d, want 777", listenerUTO)
	}
	var opErr *OpError
	if !errors.As(rawReadErr, &opErr) || opErr.Op != "raw-read" {
		t.Errorf("RawConn.Read = %v, want raw-read OpError refusal", rawReadErr)
	}
	if !errors.As(rawWriteErr, &opErr) || opErr.Op != "raw-write" {
		t.Errorf("RawConn.Write = %v, want raw-write OpError refusal", rawWriteErr)
	}
	if !errors.Is(precedenceErr, sentinel) {
		t.Errorf("Control error on an occupied port = %v, want the callback error (Control runs before bind)", precedenceErr)
	}
}

// TestDSTNetUserTimeoutBoundsConnect: TCP_USER_TIMEOUT set by the dialing
// socket's Control bounds the connect's SYN retransmissions — a dial into a
// blackhole cut fails ETIMEDOUT at the socket's bound, not the run's
// two-minute default.
func TestDSTNetUserTimeoutBoundsConnect(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var elapsed time.Duration
	var dialErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			// No Accept: the dial dies in the blackhole before any
			// connection could reach the backlog.
			ln, err := Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				<-done
				ln.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			simulation.Partition("A", "B")
			d := Dialer{KeepAlive: -1, Control: func(_, _ string, rc syscall.RawConn) error {
				return rc.Control(func(fd uintptr) {
					syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, 3000)
				})
			}}
			start := time.Now()
			_, dialErr = d.Dial("tcp", simulation.HostIP("A")+":"+p)
			elapsed = time.Since(start)
			simulation.Heal("A", "B")
		})
	})
	if !errors.Is(dialErr, syscall.ETIMEDOUT) {
		t.Fatalf("dial into a cut with user timeout = %v, want ETIMEDOUT", dialErr)
	}
	if elapsed < 3*time.Second || elapsed > 5*time.Second {
		t.Errorf("connect death after %v, want ~3s (TCP_USER_TIMEOUT bounds the SYN horizon)", elapsed)
	}
}

// TestDSTNetKeepaliveReturnOnlyCutDeath: a one-way cut of only the RETURN
// direction (peer→local) starves the probes' ACKs even though the probes
// themselves arrive — the connection dies on schedule, the directional arm
// of the unanswerable-probe law.
func TestDSTNetKeepaliveReturnOnlyCutDeath(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var elapsed time.Duration
	var readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		dstKeepaliveServer(port, done, true, ListenConfig{}, nil)
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			d := Dialer{KeepAliveConfig: KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 5 * time.Second, Count: 3}}
			c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			c.Write([]byte("x"))
			if _, err := c.Read(buf); err != nil {
				panic(err)
			}
			start := time.Now()
			simulation.PartitionOneWay("A", "B") // only A→B cut: B's probes reach A, the ACKs die
			_, readErr = c.Read(buf)
			elapsed = time.Since(start)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Fatalf("read under return-only cut = %v, want ETIMEDOUT (ACK starvation kills keepalive)", readErr)
	}
	if elapsed < 24*time.Second || elapsed > 27*time.Second {
		t.Errorf("return-only-cut keepalive death after %v, want ~25s", elapsed)
	}
}

// TestDSTNetKeepaliveSuppressedByOutstandingData: with unacknowledged data
// in flight the RETRANSMIT machinery owns the death — production's
// keepalive timer is not armed while the retransmit timer is — so a short
// keepalive schedule must NOT kill earlier than the retransmission horizon.
func TestDSTNetKeepaliveSuppressedByOutstandingData(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var elapsed time.Duration
	var readErr error
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: 30 * time.Second}}
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		dstKeepaliveServer(port, done, true, ListenConfig{}, nil)
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			// A 6s keepalive budget (idle 5s + 1s×1), far under the 30s horizon.
			d := Dialer{KeepAliveConfig: KeepAliveConfig{Enable: true, Idle: 5 * time.Second, Interval: time.Second, Count: 1}}
			c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			c.Write([]byte("x"))
			if _, err := c.Read(buf); err != nil {
				panic(err)
			}
			simulation.Partition("A", "B")
			start := time.Now()
			c.Write([]byte("held")) // outstanding: the retransmit horizon owns the death
			_, readErr = c.Read(buf)
			elapsed = time.Since(start)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Fatalf("read = %v, want ETIMEDOUT", readErr)
	}
	if elapsed < 29*time.Second || elapsed > 33*time.Second {
		t.Errorf("death after %v, want ~30s (the RETRANSMIT horizon, not the 6s keepalive schedule)", elapsed)
	}
}

// TestDSTNetUserTimeoutBoundsKeepalive: with keepalive on and
// TCP_USER_TIMEOUT set, the user timeout overrides the probing budget
// (tcp(7)) — death at idle + user-timeout, not idle + interval×count.
func TestDSTNetUserTimeoutBoundsKeepalive(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var elapsed time.Duration
	var readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		dstKeepaliveServer(port, done, true, ListenConfig{}, nil)
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			d := Dialer{KeepAlive: -1, Control: func(_, _ string, rc syscall.RawConn) error {
				return rc.Control(func(fd uintptr) {
					s := int(fd)
					syscall.SetsockoptInt(s, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, 1)
					syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, 10)
					syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL, 5)
					syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT, 9) // 45s budget…
					syscall.SetsockoptInt(s, syscall.IPPROTO_TCP, tcpUserTimeout, 2000)   // …overridden to 2s
				})
			}}
			c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			c.Write([]byte("x"))
			if _, err := c.Read(buf); err != nil {
				panic(err)
			}
			start := time.Now()
			simulation.Partition("A", "B")
			_, readErr = c.Read(buf)
			elapsed = time.Since(start)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Fatalf("read = %v, want ETIMEDOUT", readErr)
	}
	// The kernel's grid: the first probe fires at idle (10s) with the kill
	// check still unarmed (no probe out yet); the NEXT fire (15s) finds the
	// user timeout long elapsed with a probe out and kills — never at the
	// bare idle+uto instant (12s), which no real kernel reaches, and never
	// the 45s count budget the user timeout replaces.
	if elapsed < 14*time.Second || elapsed > 17*time.Second {
		t.Errorf("death after %v, want ~15s (the first probe-grid fire past TCP_USER_TIMEOUT)", elapsed)
	}
}

// TestDSTNetKeepaliveHealRecutRestartsBudget: a heal-then-recut INSIDE the
// watchdog's armed window starts a NEW episode — production's probes were
// answered during the clear instant and its failure counter reset — so the
// death budget restarts at the recut, never firing at the stale pre-heal
// deadline (a premature, sim-only death the Soundness invariant forbids).
func TestDSTNetKeepaliveHealRecutRestartsBudget(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var elapsed time.Duration
	var readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		dstKeepaliveServer(port, done, true, ListenConfig{}, nil)
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			// idle 10s + 5s×3 = a 25s deadline from the first cut.
			d := Dialer{KeepAliveConfig: KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 5 * time.Second, Count: 3}}
			c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			c.Write([]byte("x"))
			if _, err := c.Read(buf); err != nil {
				panic(err)
			}
			start := time.Now()
			simulation.Partition("A", "B")
			go func() {
				time.Sleep(12 * time.Second)
				simulation.Heal("A", "B") // clear instant: probes answered
				time.Sleep(8 * time.Second)
				simulation.Partition("A", "B") // recut at +20s
			}()
			_, readErr = c.Read(buf)
			elapsed = time.Since(start)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Fatalf("read = %v, want ETIMEDOUT", readErr)
	}
	// The heal (observed at the +15s fire) answered the pending probe and
	// RESET the failure counter (kaAckAt 15s, probes out 0). After the
	// recut (+20s) the fresh episode's grid runs from
	// max(ackAt+idle, recut) = +25s: probes at 25/30/35s, the kill check
	// with three out at +40s — never the stale pre-heal deadline (+25s),
	// and never one interval early (+35s, a carried-over probe count).
	if elapsed < 38*time.Second || elapsed > 43*time.Second {
		t.Errorf("death after %v, want ~40s (fresh episode from the recut, counter fully reset)", elapsed)
	}
}

// TestDSTNetKeepaliveInboundDataDefersProbes: received data resets the
// keepalive idle clock (the kernel's rcv_tstamp law) — under a one-way cut
// of only the OUTGOING direction, a peer that keeps sending keeps the idle
// clock fresh, so no probe is ever due and the connection never dies, even
// though outgoing probes would be unanswerable.
func TestDSTNetKeepaliveInboundDataDefersProbes(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var got int
	var readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept()
				if err != nil {
					panic(err)
				}
				for i := range 25 { // one byte every 2s for 50s
					if i > 0 {
						time.Sleep(2 * time.Second)
					}
					if _, err := c.Write([]byte("t")); err != nil {
						panic(err)
					}
				}
				<-done
				c.Close()
				ln.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			// A schedule that would kill within ~25s if inbound data did
			// not defer the probes.
			d := Dialer{KeepAliveConfig: KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 5 * time.Second, Count: 3}}
			c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			simulation.PartitionOneWay("B", "A") // outgoing cut: probes die, inbound flows
			buf := make([]byte, 1)
			for got < 25 {
				if _, readErr = c.Read(buf); readErr != nil {
					break
				}
				got++
			}
			c.Close()
		})
	})
	if readErr != nil {
		t.Fatalf("read failed after %d ticks: %v — inbound data must keep deferring the probes", got, readErr)
	}
	if got != 25 {
		t.Errorf("delivered %d/25 ticks", got)
	}
}

// TestDSTNetKeepaliveActivityResetsProbeCount: an arrival between probe
// fires zeroes the probe counter (the kernel's tcp_ack resetting
// icsk_probes_out) — probes sent before separate quiet spells never
// accumulate toward a kill across activity bursts. Under a one-way OUT cut
// with a peer that repeatedly pauses longer than the idle time, each pause
// costs at most its own probes; a counter carried across bursts would reach
// the count and kill a connection production keeps alive.
func TestDSTNetKeepaliveActivityResetsProbeCount(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var got int
	var readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept()
				if err != nil {
					panic(err)
				}
				// Four quiet spells of 12s (> the 10s idle), each ended by a
				// byte: each spell lets exactly one probe fire; a carried
				// counter reaches 3 (= count) and the fourth spell's first
				// fire kills before the final byte arrives.
				for i := range 5 {
					if i > 0 {
						time.Sleep(12 * time.Second)
					}
					if _, err := c.Write([]byte("t")); err != nil {
						panic(err)
					}
				}
				<-done
				c.Close()
				ln.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			defer close(done)
			p := <-port
			d := Dialer{KeepAliveConfig: KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 5 * time.Second, Count: 3}}
			c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			simulation.PartitionOneWay("B", "A") // outgoing cut: probes die, inbound flows
			buf := make([]byte, 1)
			for got < 5 {
				if _, readErr = c.Read(buf); readErr != nil {
					break
				}
				got++
			}
			c.Close()
		})
	})
	if readErr != nil {
		t.Fatalf("read failed after %d bursts: %v — activity must reset the probe counter", got, readErr)
	}
	if got != 5 {
		t.Errorf("delivered %d/5 bursts", got)
	}
}

// TestDSTNetKeepaliveDeterministic: the keepalive death instant is a pure
// function of the seed — two identical runs observe identical virtual
// elapsed times.
func TestDSTNetKeepaliveDeterministic(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	run := func() time.Duration {
		var elapsed time.Duration
		simulation.RunWith(7, simulation.Options{}, func() {
			port := make(chan string, 1)
			done := make(chan struct{})
			dstKeepaliveServer(port, done, true, ListenConfig{}, nil)
			simulation.Host("B", simulation.HostConfig{}, func() {
				defer close(done)
				p := <-port
				d := Dialer{KeepAliveConfig: KeepAliveConfig{Enable: true, Idle: 7 * time.Second, Interval: 3 * time.Second, Count: 4}}
				c, err := d.Dial("tcp", simulation.HostIP("A")+":"+p)
				if err != nil {
					panic(err)
				}
				buf := make([]byte, 1)
				c.Write([]byte("x"))
				if _, err := c.Read(buf); err != nil {
					panic(err)
				}
				simulation.Partition("A", "B")
				start := time.Now()
				c.Read(buf)
				elapsed = time.Since(start)
				c.Close()
			})
		})
		return elapsed
	}
	a, b := run(), run()
	if a != b {
		t.Errorf("keepalive death not replay-exact: %v vs %v", a, b)
	}
	if a == 0 {
		t.Error("keepalive death did not occur")
	}
}
