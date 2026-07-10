// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"errors"
	"io"
	"net"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestDSTProcessExitKillsSleepingSubtree: a Process body's return kills its
// still-running subtree — including a goroutine parked in time.Sleep. The
// sleeper's timer later fires against the crashed goroutine and its wake must
// be swallowed by the bubble's crashed-goroutine accounting (no hang, no
// deadlock, no resurrection). This is the wake path a pre-mark blocked victim
// exercises; without the changegstatus crash guard the bubble would count the
// unrunnable goroutine as running and never idle again.
func TestDSTProcessExitKillsSleepingSubtree(t *testing.T) {
	var ran atomic.Bool
	var advanced time.Duration
	Run(1, func() {
		Process("p", func() {
			started := make(chan struct{})
			go func() {
				close(started)
				time.Sleep(50 * time.Millisecond)
				ran.Store(true)
			}()
			<-started
			for range 5 {
				runtime.Gosched() // let the sleeper park in its timer before exiting
			}
			// Return with the sleeper parked: its armed timer fires against a
			// dead goroutine.
		})
		start := time.Now()
		time.Sleep(200 * time.Millisecond) // advances fake time past the sleeper's deadline
		advanced = time.Since(start)
	})
	if ran.Load() {
		t.Fatalf("a killed process's sleeper ran after its exit")
	}
	if advanced != 200*time.Millisecond {
		t.Fatalf("clock advanced %v, want 200ms (bubble must stay schedulable past the dead timer)", advanced)
	}
}

// TestDSTProcessExitClosesConnsGracefully: process exit closes the exiting
// process's connection ends GRACEFULLY — the kernel close()s a dying process's
// sockets, so the peer drains buffered bytes then reads io.EOF, never the
// crash fault's ECONNRESET — and closes its listeners, so a later dial is
// refused.
func TestDSTProcessExitClosesConnsGracefully(t *testing.T) {
	var payload string
	var readErr, dialErr error
	Run(1, func() {
		Host("h", HostConfig{}, func() {
			addrCh := make(chan string, 1)
			srvErr := make(chan error, 2)
			done := make(chan struct{})
			go Process("server", func() {
				defer close(done)
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					srvErr <- err
					return
				}
				addrCh <- l.Addr().String()
				c, err := l.Accept()
				if err != nil {
					srvErr <- err
					return
				}
				b, err := io.ReadAll(c) // drains "hi", then the peer's exit-close FIN
				payload, readErr = string(b), err
			})
			addr := <-addrCh

			Process("client", func() {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatalf("client Dial: %v", err)
				}
				if _, err := c.Write([]byte("hi")); err != nil {
					t.Fatalf("client Write: %v", err)
				}
				// Exit with the conn open: the exit-close must FIN, not RST.
			})
			<-done
			select {
			case err := <-srvErr:
				t.Fatalf("server: %v", err)
			default:
			}

			// The server exited with it (body returned after the read): its
			// listener died with the process, so a fresh dial is refused.
			_, dialErr = net.Dial("tcp", addr)
		})
	})
	if readErr != nil {
		t.Fatalf("server read after client exit = %v, want graceful EOF (io.ReadAll returns nil)", readErr)
	}
	if payload != "hi" {
		t.Fatalf("server payload = %q, want %q (buffered bytes drain before EOF)", payload, "hi")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Fatalf("dial after server exit = %v, want ECONNREFUSED (listener closed with the process)", dialErr)
	}
}

// TestDSTProcessExitResetsConnWithUnreadData: the kernel's exit-close is
// conditional — a socket closed with UNREAD received data answers the peer
// with RST, not FIN. A client that exits without reading the server's reply
// leaves the server observing ECONNRESET, never a clean EOF.
func TestDSTProcessExitResetsConnWithUnreadData(t *testing.T) {
	var srvReadErr error
	Run(1, func() {
		Host("h", HostConfig{}, func() {
			addrCh := make(chan string, 1)
			srvErr := make(chan error, 1)
			written := make(chan struct{})
			srvDone := make(chan struct{})
			go Process("server", func() {
				defer close(srvDone)
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					srvErr <- err
					return
				}
				addrCh <- l.Addr().String()
				c, err := l.Accept()
				if err != nil {
					srvErr <- err
					return
				}
				if _, err := c.Write([]byte("reply")); err != nil {
					srvErr <- err
					return
				}
				close(written)
				// Blocks until the client's exit-close resets the conn (the
				// client never reads the reply, so close(2) RSTs).
				_, srvReadErr = c.Read(make([]byte, 1))
			})
			addr := <-addrCh

			Process("client", func() {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatalf("client Dial: %v", err)
				}
				_ = c
				<-written // the reply now sits unread in our receive direction
				// Exit without reading it.
			})
			<-srvDone
			select {
			case err := <-srvErr:
				t.Fatalf("server: %v", err)
			default:
			}
		})
	})
	if !errors.Is(srvReadErr, syscall.ECONNRESET) {
		t.Fatalf("server read after client exited with unread data = %v, want ECONNRESET (close(2) with a non-empty receive queue RSTs)", srvReadErr)
	}
}

// TestDSTProcessExitLastInvocationScopesResources: with two live same-name
// invocations, the FIRST body's return must not tear down the logical
// process's proc-keyed resources — the survivor's conn keeps working; the
// LAST return tears down.
func TestDSTProcessExitLastInvocationScopesResources(t *testing.T) {
	var midErr, afterErr error
	Run(1, func() {
		Host("h", HostConfig{}, func() {
			addrCh := make(chan string, 1)
			hold := make(chan struct{})
			done := make(chan struct{})
			srvErr := make(chan error, 1)
			go Process("srv", func() {
				defer close(done)
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					srvErr <- err
					return
				}
				addrCh <- l.Addr().String()
				<-hold
			})
			addr := <-addrCh
			// Second invocation of the SAME logical process enters and exits
			// while the first still lives.
			Process("srv", func() {})
			// The first invocation's listener must have survived the second's
			// exit (resources are proc-keyed; teardown waits for the last).
			c, err := net.Dial("tcp", addr)
			midErr = err
			if err == nil {
				c.Close()
			}
			close(hold)
			<-done
			select {
			case err := <-srvErr:
				t.Fatalf("server: %v", err)
			default:
			}
			_, afterErr = net.Dial("tcp", addr)
		})
	})
	if midErr != nil {
		t.Fatalf("dial with one invocation still live = %v, want success (last-invocation scoping)", midErr)
	}
	if !errors.Is(afterErr, syscall.ECONNREFUSED) {
		t.Fatalf("dial after last invocation exit = %v, want ECONNREFUSED", afterErr)
	}
}

// TestDSTProcessExitResetDropsInFlightBytes: the exit-close RST arm resets the
// conn at BOTH ends — the surviving peer's next read fails ECONNRESET without
// draining, even for a reply the dying process sent moments before exiting
// (the RST destroys the receive queue; a single-end teardown would present as
// a graceful write-close and let the peer drain first).
func TestDSTProcessExitResetDropsInFlightBytes(t *testing.T) {
	var n int
	var readErr error
	RunWith(1, Options{Network: NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}, func() {
		addrCh := make(chan string, 1)
		readDone := make(chan struct{})
		Host("srv", HostConfig{}, func() {
			go Process("server", func() {
				l, err := net.Listen("tcp", HostIP("srv")+":0")
				if err != nil {
					t.Errorf("listen: %v", err)
					return
				}
				addrCh <- l.Addr().String()
				c, err := l.Accept()
				if err != nil {
					t.Errorf("accept: %v", err)
					return
				}
				// The client's data is delivered and sits UNREAD; our reply is
				// in flight when the exit-close lands.
				time.Sleep(300 * time.Millisecond)
				if _, err := c.Write([]byte("resp")); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				// Exit without reading: the kernel close RSTs.
			})
		})
		addr := <-addrCh
		Host("cli", HostConfig{}, func() {
			go Process("client", func() {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					t.Errorf("dial: %v", err)
					return
				}
				c.Write([]byte("data"))
				// Blocks until the server's exit-close resets our end (no data
				// can arrive first: the reply's delivery lies past the reset).
				n, readErr = c.Read(make([]byte, 8))
				close(readDone)
				c.Close()
			})
		})
		// The server Process body returns after its write; its exit teardown
		// runs then. Wait for the whole sequence via the client's read.
		<-readDone
	})
	if n != 0 || !errors.Is(readErr, syscall.ECONNRESET) {
		t.Fatalf("first read after the peer exited with unread data = (%d, %v), want (0, ECONNRESET): the exit RST drops in-flight bytes, not drains them", n, readErr)
	}
}
