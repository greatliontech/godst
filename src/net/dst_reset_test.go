// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// White-box: references dst-only symbols (dstWirePair), so it is
// build-tagged like the package's other white-box dst test files rather
// than stub-compilable untagged (the untagged test build is gated by
// `vet net` in the Taskfile's untagged leg).

//go:build dst

package net

import (
	"errors"
	"io"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// These tests exercise connection-reset targeting (simulation.Reset host-pair,
// ResetProcess). Invariants:
//   - a reset delivers ECONNRESET to BOTH ends of every targeted conn;
//   - each end is a SURVIVOR (no process died) and receives the RST as a real
//     kernel would: bytes already DELIVERED to its receive queue drain first
//     (tcp_recvmsg reports pending data before the socket error), then the
//     first failing op reports ECONNRESET and writes fail immediately — the
//     kernel's ONE-SHOT sk_err (host-probed): later reads return io.EOF and
//     later writes EPIPE, the CLOSED-socket identities — DST-FAULT-SOUND;
//   - bytes still IN FLIGHT are dropped (the RST beat them to the socket, one
//     of the real orderings an injected RST produces) — DST-FAULT-SOUND;
//   - it touches exactly the victim's conns, no leak onto other pairs/processes
//     (DST-FAULT-VICTIM);
//   - it replays exactly for a given seed (DST-FAULT-REPLAY).

// TestDSTNetResetPair: Reset(A,B) tears down the A-B conn — both the server's
// blocked read and the client's next read see ECONNRESET.
func TestDSTNetResetPair(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var serverErr, clientErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				_, serverErr = c.Read(make([]byte, 16)) // blocked until the reset
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			time.Sleep(10 * time.Millisecond) // let the server park in Read
			simulation.Reset("A", "B")
			_, clientErr = c.Read(make([]byte, 16))
			<-done
			c.Close()
		})
	})
	if !errors.Is(serverErr, syscall.ECONNRESET) {
		t.Errorf("server read after reset = %v, want ECONNRESET", serverErr)
	}
	if !errors.Is(clientErr, syscall.ECONNRESET) {
		t.Errorf("client read after reset = %v, want ECONNRESET", clientErr)
	}
}

// TestDSTNetResetDropsInFlight: bytes in flight (buffered behind a link latency)
// when the conn is reset are dropped — the peer sees ECONNRESET, not the buffered
// data (a real RST discards in-flight; DST-FAULT-SOUND).
func TestDSTNetResetDropsInFlight(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var readErr error
	var gotData bool
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				n, err := c.Read(make([]byte, 16))
				gotData = n > 0
				readErr = err
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("msg"))     // in flight: deliverable only 100ms later
			simulation.Reset("A", "B") // reset now, before delivery
			<-done
			c.Close()
		})
	})
	if gotData {
		t.Errorf("peer received in-flight bytes across a reset, want them dropped")
	}
	if !errors.Is(readErr, syscall.ECONNRESET) {
		t.Errorf("read after reset = %v, want ECONNRESET", readErr)
	}
}

// TestDSTNetResetDropsInFlightEvenAfterDelay: in-flight bytes die AT the
// reset instant, not merely before the survivor's next wake — virtual time
// passing beyond their would-be delivery after the reset must never
// resurrect them (the injected RST destroyed the connection state that would
// have accepted them; a late read still finds an empty queue and the reset
// error).
func TestDSTNetResetDropsInFlightEvenAfterDelay(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var readErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		resetDone := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-resetDone // read only after the reset AND the would-be delivery time
				n, readErr = c.Read(make([]byte, 16))
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("msg"))             // in flight: deliverable only 100ms later
			simulation.Reset("A", "B")         // reset now, before delivery
			time.Sleep(200 * time.Millisecond) // let virtual time pass the would-be deliverAt
			close(resetDone)
			<-done
			c.Close()
		})
	})
	if n != 0 {
		t.Errorf("read %d bytes after the reset's would-be delivery time, want 0: dead in-flight bytes must not resurrect", n)
	}
	if !errors.Is(readErr, syscall.ECONNRESET) {
		t.Errorf("late read after reset = %v, want ECONNRESET", readErr)
	}
}

// TestDSTNetResetUnderPartitionDropsHeldBytes: an injected reset during a
// partition kills exactly what a real kernel would lose — bytes delivered
// to the survivor's receive queue BEFORE the cut drain (they arrived; no
// RST can destroy them), while bytes the cut is holding die even though
// their nominal delivery time has passed (they never reached the receive
// queue; the connection state that would have accepted them is gone).
func TestDSTNetResetUnderPartitionDropsHeldBytes(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var buf [16]byte
	var firstErr, secondErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 10 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		reset := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-reset
				n, firstErr = c.Read(buf[:])
				_, secondErr = c.Read(make([]byte, 16))
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("pre"))
			time.Sleep(20 * time.Millisecond) // "pre" is delivered to A's receive queue
			c.Write([]byte("held"))
			simulation.Partition("A", "B") // cut before "held" arrives
			time.Sleep(50 * time.Millisecond)
			// "held"'s nominal delivery time has long passed, but the cut
			// kept it out of A's receive queue. The reset must not deliver
			// it — not even after the link heals (the heal would flush held
			// bytes on a LIVE conn; this one died under the cut).
			simulation.Reset("A", "B")
			simulation.Heal("A", "B")
			close(reset)
			<-done
			c.Close()
		})
	})
	if n != 3 || string(buf[:3]) != "pre" || firstErr != nil {
		t.Errorf("first read after reset-under-cut = (%d, %q, %v), want (3, %q, nil): pre-cut-delivered bytes drain", n, buf[:n], firstErr, "pre")
	}
	if !errors.Is(secondErr, syscall.ECONNRESET) {
		t.Errorf("second read = %v, want ECONNRESET with no data: cut-held bytes died with the reset", secondErr)
	}
}

// TestDSTNetResetDrainsDeliveredThenResets: bytes already DELIVERED to a
// survivor's receive queue before an injected Reset drain first — a real RST
// cannot destroy what the receiver's kernel already holds (tcp_recvmsg
// reports pending data before the socket error) — and only then does the
// FIRST failing read report ECONNRESET, consuming the one-shot sk_err
// (host-probed): the write after it carries the CLOSED-socket EPIPE and a
// further read plain io.EOF.
func TestDSTNetResetDrainsDeliveredThenResets(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var buf [16]byte
	var firstErr, secondErr, writeErr, thirdErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 10 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		reset := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-reset
				n, firstErr = c.Read(buf[:])
				_, secondErr = c.Read(make([]byte, 16))
				_, writeErr = c.Write([]byte("after"))
				_, thirdErr = c.Read(make([]byte, 16))
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("msg"))
			time.Sleep(20 * time.Millisecond) // past the link latency: delivered to A's receive queue
			simulation.Reset("A", "B")
			close(reset)
			<-done
			c.Close()
		})
	})
	if n != 3 || string(buf[:3]) != "msg" || firstErr != nil {
		t.Errorf("first read after reset = (%d, %q, %v), want (3, %q, nil): delivered bytes drain before the reset error", n, buf[:n], firstErr, "msg")
	}
	var opErr *OpError
	if !errors.Is(secondErr, syscall.ECONNRESET) || !errors.As(secondErr, &opErr) || opErr.Op != "read" || opErr.Net != "tcp" {
		t.Errorf("second read after drain = %v, want a tcp read *OpError wrapping ECONNRESET (the one-shot sk_err)", secondErr)
	}
	if !errors.Is(writeErr, syscall.EPIPE) {
		t.Errorf("write after the consumed reset = %v, want EPIPE (sk_err consumed by the read; the CLOSED-socket write identity)", writeErr)
	}
	if thirdErr != io.EOF {
		t.Errorf("read after the consumed reset = %v, want io.EOF (the CLOSED-socket read identity)", thirdErr)
	}
}

// TestDSTNetResetWriteFirstConsumesSkErr: the one-shot sk_err is consumed by
// whichever failing op comes FIRST — write-first here (host-probed ladder:
// write ECONNRESET, then write EPIPE, then reads io.EOF). Complements the
// read-first ladder in TestDSTNetResetDrainsDeliveredThenResets.
func TestDSTNetResetWriteFirstConsumesSkErr(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var firstWriteErr, secondWriteErr, readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-done
				c.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			simulation.Reset("A", "B")
			_, firstWriteErr = c.Write([]byte("one"))
			_, secondWriteErr = c.Write([]byte("two"))
			_, readErr = c.Read(make([]byte, 8))
			close(done)
			c.Close()
		})
	})
	// Production error SHAPE, not just identity: both errno legs must be
	// *net.OpError carrying the op and the connection's network — the shape
	// SUTs unwrap with errors.As (a bare errno would satisfy errors.Is and
	// silently lose it).
	var opErr *OpError
	if !errors.Is(firstWriteErr, syscall.ECONNRESET) || !errors.As(firstWriteErr, &opErr) || opErr.Op != "write" || opErr.Net != "tcp" {
		t.Errorf("first write after reset = %v, want a tcp write *OpError wrapping ECONNRESET (consumes the one-shot sk_err)", firstWriteErr)
	}
	opErr = nil
	if !errors.Is(secondWriteErr, syscall.EPIPE) || !errors.As(secondWriteErr, &opErr) || opErr.Op != "write" || opErr.Net != "tcp" {
		t.Errorf("second write after reset = %v, want a tcp write *OpError wrapping EPIPE (sk_err consumed)", secondWriteErr)
	}
	if readErr != io.EOF {
		t.Errorf("read after the consumed reset = %v, want io.EOF", readErr)
	}
}

// TestDSTNetResetInCloseWaitIsEPIPE: an injected RST arriving AFTER the
// peer's FIN was delivered meets a CLOSE_WAIT socket — tcp_reset pends EPIPE
// there, not ECONNRESET, and the EPIPE consumption is indistinguishable from
// the post-consumption identities (host-probed): reads drain then io.EOF
// throughout, writes EPIPE throughout, no ECONNRESET arm at all.
func TestDSTNetResetInCloseWaitIsEPIPE(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var buf [16]byte
	var drainErr, readErr, writeErr, secondWriteErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		closed := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				c.Write([]byte("bye"))
				c.Close() // clean close: FIN (nothing unread at this end)
				close(closed)
				<-done
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			<-closed // the FIN has arrived (zero latency): B is in CLOSE_WAIT
			simulation.Reset("A", "B")
			n, drainErr = c.Read(buf[:])
			_, readErr = c.Read(make([]byte, 8))
			_, writeErr = c.Write([]byte("x"))
			_, secondWriteErr = c.Write([]byte("y"))
			close(done)
			c.Close()
		})
	})
	if n != 3 || string(buf[:3]) != "bye" || drainErr != nil {
		t.Errorf("drain after CLOSE_WAIT reset = (%d, %q, %v), want (3, %q, nil): delivered bytes still drain", n, buf[:n], drainErr, "bye")
	}
	if readErr != io.EOF {
		t.Errorf("read after CLOSE_WAIT reset = %v, want io.EOF (tcp_reset's CLOSE_WAIT arm pends EPIPE, which reads never surface)", readErr)
	}
	if !errors.Is(writeErr, syscall.EPIPE) {
		t.Errorf("write after CLOSE_WAIT reset = %v, want EPIPE (never ECONNRESET)", writeErr)
	}
	if !errors.Is(secondWriteErr, syscall.EPIPE) {
		t.Errorf("second write after CLOSE_WAIT reset = %v, want EPIPE", secondWriteErr)
	}
}

// TestDSTNetResetBeatsInFlightFIN: the CLOSE_WAIT discriminant's FALSE
// direction — an injected RST that arrives while the peer's FIN (and its
// preceding bytes) are still IN FLIGHT meets an ESTABLISHED socket, so the
// identity is the one-shot ECONNRESET ladder, never the CLOSE_WAIT
// EPIPE/EOF shape, and the in-flight bytes and FIN died with the RST
// (nothing drains). Complements TestDSTNetResetInCloseWaitIsEPIPE.
func TestDSTNetResetBeatsInFlightFIN(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var firstErr, secondErr, writeErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 10 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		dialed := make(chan struct{})
		closed := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-dialed // the dial must complete before this end closes (a close mid-handshake refuses the dial)
				c.Write([]byte("x"))
				c.Close() // FIN: in flight for the next 10ms
				close(closed)
				<-done
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, err := Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				t.Errorf("dial: %v", err)
				close(dialed)
				close(done)
				return
			}
			close(dialed)
			<-closed
			simulation.Reset("A", "B") // the RST beats the traveling byte and FIN
			n, firstErr = c.Read(make([]byte, 8))
			_, secondErr = c.Read(make([]byte, 8))
			_, writeErr = c.Write([]byte("y"))
			close(done)
			c.Close()
		})
	})
	if n != 0 || !errors.Is(firstErr, syscall.ECONNRESET) {
		t.Errorf("first read after RST-beats-FIN = (%d, %v), want (0, ECONNRESET): the socket was ESTABLISHED (the FIN never arrived) and the in-flight byte died", n, firstErr)
	}
	if secondErr != io.EOF {
		t.Errorf("second read = %v, want io.EOF (sk_err consumed)", secondErr)
	}
	if !errors.Is(writeErr, syscall.EPIPE) {
		t.Errorf("write after the consumed reset = %v, want EPIPE", writeErr)
	}
}

// TestDSTNetResetProcessOwnEndDrains: ResetProcess(p) resets p's connections,
// not p itself — p stays alive, so p's OWN end is a survivor too and drains
// the bytes already delivered to it before failing ECONNRESET.
func TestDSTNetResetProcessOwnEndDrains(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var buf [16]byte
	var firstErr, secondErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 10 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				c.Write([]byte("hi"))
				<-done
				c.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			simulation.Process("worker", func() {
				p := <-port
				c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
				time.Sleep(20 * time.Millisecond) // the server's "hi" is delivered to the worker's queue
				simulation.ResetProcess("worker")
				n, firstErr = c.Read(buf[:])
				_, secondErr = c.Read(make([]byte, 16))
				close(done)
				c.Close()
			})
		})
	})
	if n != 2 || string(buf[:2]) != "hi" || firstErr != nil {
		t.Errorf("worker's first read after ResetProcess = (%d, %q, %v), want (2, %q, nil): the live process drains its delivered bytes", n, buf[:n], firstErr, "hi")
	}
	if !errors.Is(secondErr, syscall.ECONNRESET) {
		t.Errorf("worker's second read = %v, want ECONNRESET", secondErr)
	}
}

// TestDSTNetResetBacklogBlockedDialFailsPromptly: a dial blocked on a FULL
// backlog (its SYN dropped, retransmitting) fails promptly at an injected
// reset, never stranding to the retransmit horizon's ETIMEDOUT — the
// drain-then-reset teardown closes no transport, so the wake is the dial's
// own rstKill arm. The identity is ECONNREFUSED, not ECONNRESET: the
// dialer's socket is in SYN_SENT (connect(2) has not returned), and
// tcp_reset maps an RST received in SYN_SENT to ECONNREFUSED — the
// connection-refused mechanism itself (host-probed via the closed-listener
// shape).
func TestDSTNetResetBacklogBlockedDialFailsPromptly(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var dialErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				<-done // never Accept; the backlog fills and holds
				ln.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			addr := simulation.HostIP("A") + ":" + p
			conns := make([]Conn, 0, 128)
			for i := 0; i < 128; i++ { // fill the accept backlog
				c, err := Dial("tcp", addr)
				if err != nil {
					t.Errorf("backlog-filling dial %d: %v", i, err)
					close(done)
					return
				}
				conns = append(conns, c)
			}
			blocked := make(chan struct{})
			go func() {
				_, dialErr = Dial("tcp", addr) // SYN dropped: blocks on the full backlog
				close(blocked)
			}()
			time.Sleep(10 * time.Millisecond) // let the dial park
			simulation.Reset("A", "B")
			<-blocked
			for _, c := range conns {
				c.Close()
			}
			close(done)
		})
	})
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Errorf("dial blocked on a full backlog across a reset = %v, want ECONNREFUSED (prompt SYN_SENT abort, not ECONNRESET and not a horizon ETIMEDOUT)", dialErr)
	}
}

// TestDSTNetResetRacingZeroLatencyDial: a reset racing a ZERO-LATENCY dial
// can land in the window between the backlog send and the (instant) SYN-ACK
// completion; the aborted dial carries the SYN_SENT ECONNREFUSED, never the
// established-state ECONNRESET, whatever the seed's interleaving (nil means
// the dial won the race — the reset then hits an established conn, which is
// the drain-then-reset surface, not the dial's). Seed-swept so the seeded
// scheduler explores the orderings around the send's scheduling point.
func TestDSTNetResetRacingZeroLatencyDial(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	for seed := uint64(1); seed <= 50; seed++ {
		var dialErr error
		simulation.RunWith(seed, simulation.Options{}, func() {
			port := make(chan string, 1)
			done := make(chan struct{})
			simulation.Host("A", simulation.HostConfig{}, func() {
				ln, _ := Listen("tcp", ":0")
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				go func() {
					<-done
					ln.Close()
				}()
			})
			simulation.Host("B", simulation.HostConfig{}, func() {
				addr := simulation.HostIP("A") + ":" + <-port
				dialDone := make(chan struct{})
				go func() {
					var c Conn
					c, dialErr = Dial("tcp", addr)
					if c != nil {
						c.Close()
					}
					close(dialDone)
				}()
				simulation.Reset("A", "B")
				<-dialDone
				close(done)
			})
		})
		if dialErr != nil && !errors.Is(dialErr, syscall.ECONNREFUSED) {
			t.Fatalf("seed %d: reset racing a zero-latency dial = %v, want nil or ECONNREFUSED (never the established-state ECONNRESET)", seed, dialErr)
		}
	}
}

// TestDSTNetResetAfterParkedSendCommits pins the parked-send-commit window:
// a dial PARKED on a full backlog has its send committed by the RECEIVER —
// Accept's dequeue moves the parked value into the buffer and commits the
// select's send arm while the dialer is descheduled — so a reset landing
// between that commit and the dialer's resume is observed by the
// zero-latency SYN-ACK check, not the select's rstKill case, and the dial
// fails ECONNREFUSED (the SYN_SENT identity). Seed-swept with a
// non-vacuity floor: at least one seed must exercise the refusal window
// (the other seeds' dials win the race and succeed).
func TestDSTNetResetAfterParkedSendCommits(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	refused := 0
	for seed := uint64(1); seed <= 50; seed++ {
		var dialErr error
		simulation.RunWith(seed, simulation.Options{}, func() {
			port := make(chan string, 1)
			parked := make(chan struct{})
			accepted := make(chan struct{})
			done := make(chan struct{})
			simulation.Host("A", simulation.HostConfig{}, func() {
				ln, _ := Listen("tcp", ":0")
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				go func() {
					<-parked
					c, err := ln.Accept() // frees one slot: commits the parked send
					if err == nil {
						c.Close()
					}
					close(accepted)
					<-done
					ln.Close()
				}()
			})
			simulation.Host("B", simulation.HostConfig{}, func() {
				addr := simulation.HostIP("A") + ":" + <-port
				conns := make([]Conn, 0, 128)
				for i := 0; i < 128; i++ { // fill the accept backlog
					c, err := Dial("tcp", addr)
					if err != nil {
						t.Errorf("seed %d: backlog-filling dial %d: %v", seed, i, err)
						close(parked)
						close(done)
						return
					}
					conns = append(conns, c)
				}
				dialDone := make(chan struct{})
				go func() {
					var c Conn
					c, dialErr = Dial("tcp", addr) // parks on the full backlog
					if c != nil {
						c.Close()
					}
					close(dialDone)
				}()
				go func() {
					time.Sleep(10 * time.Millisecond) // let the dial park
					close(parked)
				}()
				<-accepted // the parked send has been committed by the dequeue
				simulation.Reset("A", "B")
				<-dialDone
				for _, c := range conns {
					c.Close()
				}
				close(done)
			})
		})
		if dialErr == nil {
			continue // the dialer resumed before the reset: a legitimate win
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			t.Fatalf("seed %d: reset in the parked-send-commit window = %v, want ECONNREFUSED (the SYN_SENT identity, never ECONNRESET)", seed, dialErr)
		}
		refused++
	}
	if refused == 0 {
		t.Fatalf("no seed exercised the parked-send-commit refusal window — the sweep is vacuous")
	}
}

// TestDSTNetCrashHostBlockedDialTimesOut: a dial blocked on a FULL backlog
// when the server HOST loses power fails ETIMEDOUT, never a reset identity —
// a powered-off machine emits no RST, so the connect retransmits into
// silence and dies at its horizon (contrast the injected-reset case above,
// which aborts promptly with ECONNRESET). Swept across seeds: the crash
// teardown closes several of the blocked dial's wake channels, the seeded
// select picks among the ready ones, and every arm must classify the dead
// host into the redial/blackhole path.
func TestDSTNetCrashHostBlockedDialTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	for seed := uint64(1); seed <= 8; seed++ {
		var dialErr error
		simulation.RunWith(seed, simulation.Options{}, func() {
			port := make(chan string, 1)
			parked := make(chan struct{})
			dialDone := make(chan struct{})
			simulation.Host("A", simulation.HostConfig{}, func() {
				ln, _ := Listen("tcp", ":0")
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p // never Accept; the listener dies with the host
			})
			simulation.Host("B", simulation.HostConfig{}, func() {
				addr := simulation.HostIP("A") + ":" + <-port
				for i := 0; i < 128; i++ { // fill the accept backlog
					if _, err := Dial("tcp", addr); err != nil {
						t.Errorf("seed %d: backlog-filling dial %d: %v", seed, i, err)
						close(parked)
						close(dialDone)
						return
					}
				}
				go func() {
					_, dialErr = Dial("tcp", addr) // SYN dropped: blocks on the full backlog
					close(dialDone)
				}()
				go func() {
					time.Sleep(10 * time.Millisecond) // let the dial park
					close(parked)
				}()
			})
			<-parked
			simulation.CrashHost("A")
			<-dialDone
		})
		if !errors.Is(dialErr, syscall.ETIMEDOUT) {
			t.Errorf("seed %d: dial blocked on a full backlog across a host crash = %v, want ETIMEDOUT (a dead machine emits no RST)", seed, dialErr)
		}
	}
}

// TestDSTNetResetBacklogAcceptHandsOutResetChild: a fault reset landing on a
// conn still QUEUED in the accept backlog does not unlink it — a later Accept
// hands it out and its first read fails ECONNRESET (host-probed: Linux keeps
// an RST-aborted established child in the accept queue; accept(2) succeeds
// and the first read reports the pending error). The write assertion pins
// the kernel's one-shot sk_err (host-probed): the read consumed the error,
// so the write carries the CLOSED-socket EPIPE.
func TestDSTNetResetBacklogAcceptHandsOutResetChild(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var acceptErr, readErr, writeErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		reset := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				<-reset // Accept only after the reset landed on the queued conn
				c, err := ln.Accept()
				acceptErr = err
				if err == nil {
					_, readErr = c.Read(make([]byte, 16))
					_, writeErr = c.Write([]byte("after"))
					c.Close()
				}
				ln.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, err := Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				t.Errorf("dial: %v", err)
				close(reset)
				return
			}
			time.Sleep(10 * time.Millisecond) // conn sits queued, never accepted
			simulation.Reset("A", "B")
			close(reset)
			<-done
			c.Close()
		})
	})
	if acceptErr != nil {
		t.Errorf("Accept of an RST-torn backlog conn = %v, want success (the kernel hands the aborted child out)", acceptErr)
	}
	if !errors.Is(readErr, syscall.ECONNRESET) {
		t.Errorf("first read on the handed-out child = %v, want ECONNRESET", readErr)
	}
	if !errors.Is(writeErr, syscall.EPIPE) {
		t.Errorf("write on the handed-out child = %v, want EPIPE (the read consumed the one-shot sk_err)", writeErr)
	}
}

// TestDSTNetResetBacklogDrainsPreAcceptBytes: bytes the dialer wrote before
// the conn was ever accepted survive a fault reset of the queued conn — the
// handed-out child drains them first, then reads fail ECONNRESET (host-probed
// tcp_recvmsg shape: the accept queue holds established children with live
// receive queues, never "half-open" conns, so an injected RST cannot destroy
// what the victim's kernel already delivered).
func TestDSTNetResetBacklogDrainsPreAcceptBytes(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var buf [16]byte
	var acceptErr, firstErr, secondErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 10 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		reset := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				<-reset // Accept only after the reset landed on the queued conn
				c, err := ln.Accept()
				acceptErr = err
				if err == nil {
					n, firstErr = c.Read(buf[:])
					_, secondErr = c.Read(make([]byte, 16))
					c.Close()
				}
				ln.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, err := Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				t.Errorf("dial: %v", err)
				close(reset)
				return
			}
			c.Write([]byte("msg"))
			time.Sleep(20 * time.Millisecond) // past the link latency: delivered to the queued child's receive queue
			simulation.Reset("A", "B")
			close(reset)
			<-done
			c.Close()
		})
	})
	if acceptErr != nil {
		t.Errorf("Accept of an RST-torn backlog conn = %v, want success", acceptErr)
	}
	if n != 3 || string(buf[:3]) != "msg" || firstErr != nil {
		t.Errorf("first read on the handed-out child = (%d, %q, %v), want (3, %q, nil): pre-accept bytes drain before the reset error", n, buf[:n], firstErr, "msg")
	}
	if !errors.Is(secondErr, syscall.ECONNRESET) {
		t.Errorf("second read after drain = %v, want ECONNRESET", secondErr)
	}
}

// TestDSTNetResetFailsBlockedWrite: a write parked on a full send buffer
// when the reset lands fails ECONNRESET with only the pre-reset bytes
// written — a real kernel wakes a blocked sender with the socket error, and
// the destroyed connection can never carry the remainder. The adversarial
// interleaving is forced: the peer DRAINS (freeing send-buffer space, so the
// parked writer COMMITS to its freed-space wakeup) and injects the reset in
// the same scheduler slot, before the writer resumes — the resumed writer
// must observe the RST instead of pushing the remaining bytes, whatever
// wakeup it committed to.
func TestDSTNetResetFailsBlockedWrite(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var writeN int
	var writeErr error
	var n1 int
	var buf [8]byte
	var lateN int
	var lateErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency: 10 * time.Millisecond,
		SendBuffer:       4, // the 8-byte write below blocks after 4 bytes
	}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		wrote := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				time.Sleep(30 * time.Millisecond) // first 4 bytes delivered; writer parked on the full buffer
				n1, _ = c.Read(buf[:4])           // drain: frees space, the parked writer commits to its wakeup
				simulation.Reset("A", "B")        // …and the RST lands before the writer resumes (no park between)
				<-wrote
				lateN, lateErr = c.Read(buf[4:]) // anything the woken writer pushed post-RST would land here
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			go func() {
				writeN, writeErr = c.Write([]byte("12345678")) // blocks at 4 bytes
				close(wrote)
			}()
			<-done
			c.Close()
		})
	})
	if n1 != 4 || string(buf[:4]) != "1234" {
		t.Fatalf("pre-reset drain = (%d, %q), want (4, %q)", n1, buf[:n1], "1234")
	}
	if writeN != 4 || !errors.Is(writeErr, syscall.ECONNRESET) {
		t.Errorf("blocked write across a reset = (%d, %v), want (4, ECONNRESET): the woken writer must observe the RST, never push the remainder", writeN, writeErr)
	}
	// The read is the PEER end's first failing op: sk_err is per socket, so
	// the writer's consumption on its own end does not spend this end's shot.
	if lateN != 0 || !errors.Is(lateErr, syscall.ECONNRESET) {
		t.Errorf("post-reset read = (%d, %v), want (0, ECONNRESET): no post-RST byte may be delivered", lateN, lateErr)
	}
}

// TestDSTNetResetKeepsIdentityPastRetransmitHorizon: an injected RST
// destroys the socket AND its retransmit timer — a partition-armed
// retransmit watchdog expiring after the RST must not flip the conn's error
// identity to ETIMEDOUT on later operations: the ladder stays the reset
// one (first op ECONNRESET, then the CLOSED-socket EOF/EPIPE). The reachable
// shape is a HOST-CRASH survivor: its outbound bytes written into the cut
// stay held (only its inbound direction is truncated by the RST), so the
// watchdog still sees them at its horizon; a pair reset truncates both
// directions and the watchdog self-disarms.
func TestDSTNetResetKeepsIdentityPastRetransmitHorizon(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var firstErr, lateReadErr, lateWriteErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency:  10 * time.Millisecond,
		RetransmitTimeout: time.Second,
	}}, func() {
		port := make(chan string, 1)
		accepted := make(chan struct{})
		simulation.Host("victim", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				ln.Accept()
				close(accepted)
				select {} // dies with the machine
			}()
		})
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("victim")+":"+p)
			<-accepted
			simulation.Partition("survivor", "victim")
			c.Write([]byte("held")) // undeliverable: arms the survivor's retransmit watchdog
			simulation.CrashHost("victim")
			_, firstErr = c.Read(make([]byte, 8))
			time.Sleep(3 * time.Second) // well past the watchdog's horizon
			_, lateReadErr = c.Read(make([]byte, 8))
			_, lateWriteErr = c.Write([]byte("x"))
			c.Close()
		})
	})
	if !errors.Is(firstErr, syscall.ECONNRESET) {
		t.Errorf("read after the crash's RST = %v, want ECONNRESET", firstErr)
	}
	if lateReadErr != io.EOF {
		t.Errorf("read past the retransmit horizon = %v, want io.EOF (the RST killed the timer with the socket; never ETIMEDOUT)", lateReadErr)
	}
	if !errors.Is(lateWriteErr, syscall.EPIPE) {
		t.Errorf("write past the retransmit horizon = %v, want EPIPE (the reset ladder's CLOSED-socket identity; never ETIMEDOUT)", lateWriteErr)
	}
}

// TestDSTNetInjectRSTFreezesReceiveQueue: the injected RST freezes the
// receive queue at the RST instant, in PROGRAM order. The teardown loop
// injects a connection's two ends sequentially, so the not-yet-injected
// peer end can still push a byte after this end's RST already landed (its
// own rstArrived is not set yet); that byte must never become readable here
// — a real CLOSED socket answers a late segment with an RST of its own, it
// does not queue it. Conversely a byte pushed BEFORE the receiving end's
// injection is in (or bound for) its receive queue: at zero latency it
// carries deliverAt EQUAL to the freeze horizon and must stay drainable —
// program order, not timestamps, decides both directions, since virtual
// time does not advance across the interleaving. White-box at the wire
// layer so the interleaving is exact rather than schedule-dependent.
func TestDSTNetInjectRSTFreezesReceiveQueue(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	for _, latency := range []time.Duration{10 * time.Millisecond, 0} {
		var lateN, preN int
		var lateErr, preErr error
		simulation.Run(1, func() {
			a, b := dstWirePair(int64(latency), 0, 0, 0, 0, 1, 2, nil, nil)
			ea, eb := a.(*dstWireEnd), b.(*dstWireEnd)
			if _, err := ea.Write([]byte("pre")); err != nil { // toward b, before ANY injection
				t.Errorf("latency %v: pre-RST write = %v, want success", latency, err)
			}
			ea.injectRST()                                      // the RST lands at end a…
			if _, err := eb.Write([]byte("late")); err != nil { // …and its peer, not yet injected, pushes afterward
				t.Errorf("latency %v: peer write before its own injection = %v, want success", latency, err)
			}
			eb.injectRST()
			if latency > 0 {
				time.Sleep(2 * latency) // past the late segment's nominal delivery time
			}
			lateN, lateErr = ea.Read(make([]byte, 8))
			preN, preErr = eb.Read(make([]byte, 8))
		})
		if lateN != 0 || lateErr != io.ErrClosedPipe {
			t.Fatalf("latency %v: read on the reset end = (%d, %v), want (0, io.ErrClosedPipe): the receive queue froze at the RST; a post-RST segment is never delivered", latency, lateN, lateErr)
		}
		if latency == 0 {
			// Same-instant pre-RST byte: deliverAt == the freeze horizon —
			// it reached the receive queue and must drain before the error.
			if preN != 3 || preErr != nil {
				t.Fatalf("latency 0: pre-RST byte at the freeze boundary = (%d, %v), want (3, nil): deliverAt equal to the horizon is DELIVERED", preN, preErr)
			}
		} else {
			// At 10ms the pre-RST byte was still in flight at the freeze:
			// it died with the connection.
			if preN != 0 || preErr != io.ErrClosedPipe {
				t.Fatalf("latency %v: in-flight pre-RST byte = (%d, %v), want (0, io.ErrClosedPipe)", latency, preN, preErr)
			}
		}
	}
}

// TestDSTNetResetVictim: Reset(A,B) tears down only the A-B conn — a concurrent
// A-C conn keeps working (DST-FAULT-VICTIM, no leak onto a non-victim pair).
func TestDSTNetResetVictim(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var acMsg string
	var acErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		portB := make(chan string, 1)
		portC := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("B", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			portB <- p
			go func() {
				c, _ := ln.Accept()
				c.Read(make([]byte, 16)) // will be reset
				c.Close()
			}()
		})
		simulation.Host("C", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			portC <- p
			go func() {
				c, _ := ln.Accept()
				buf := make([]byte, 16)
				n, err := c.Read(buf) // must survive the A-B reset
				acMsg, acErr = string(buf[:n]), err
				c.Close()
				close(done)
			}()
		})
		simulation.Host("A", simulation.HostConfig{}, func() {
			pb, pc := <-portB, <-portC
			cb, _ := Dial("tcp", simulation.HostIP("B")+":"+pb)
			cc, _ := Dial("tcp", simulation.HostIP("C")+":"+pc)
			time.Sleep(10 * time.Millisecond)
			simulation.Reset("A", "B") // only A-B
			cc.Write([]byte("toC"))    // A-C must still deliver
			<-done
			cb.Close()
			cc.Close()
		})
	})
	if acErr != nil {
		t.Errorf("A-C read errored after an A-B reset: %v (leak onto a non-victim)", acErr)
	}
	if acMsg != "toC" {
		t.Errorf("A-C read = %q, want %q (A-C must be unaffected by the A-B reset)", acMsg, "toC")
	}
}

// TestDSTNetResetProcess: ResetProcess(p) tears down every conn process p owns an
// end of (DST-FAULT-VICTIM, the process leg — the per-process conn attribution),
// resetting BOTH ends so the peer's in-flight bytes are dropped too. The worker
// (dialer) owns the conn; the server end is the worker's peer, so it must reset as
// well — the worker's in-flight "msg" must not reach the server.
func TestDSTNetResetProcess(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var serverErr error
	var gotData bool
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				n, err := c.Read(make([]byte, 16))
				gotData = n > 0
				serverErr = err
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			simulation.Process("worker", func() {
				p := <-port
				c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
				c.Write([]byte("msg"))            // in flight (100ms latency)
				simulation.ResetProcess("worker") // resets BOTH ends -> server drops in-flight
				<-done
				c.Close()
			})
		})
	})
	if gotData {
		t.Errorf("server received the worker's in-flight bytes after ResetProcess, want them dropped (both ends must reset)")
	}
	if !errors.Is(serverErr, syscall.ECONNRESET) {
		t.Errorf("server read after ResetProcess = %v, want ECONNRESET", serverErr)
	}
}

func TestDSTNetResetProcessLeavesListenerOpen(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var dialErr, acceptErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		simulation.Process("worker", func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			simulation.ResetProcess("worker")
			accepted := make(chan struct{})
			go func() {
				c, err := ln.Accept()
				acceptErr = err
				if err == nil {
					c.Close()
				}
				close(accepted)
			}()
			c, err := Dial("tcp", "127.0.0.1:"+p)
			dialErr = err
			if err == nil {
				c.Close()
			}
			<-accepted
			ln.Close()
		})
	})
	if dialErr != nil {
		t.Fatalf("Dial after ResetProcess on listener owner = %v, want nil", dialErr)
	}
	if acceptErr != nil {
		t.Fatalf("Accept after ResetProcess on listener owner = %v, want nil", acceptErr)
	}
}

// TestDSTNetResetDeterminism: reset runs replay exactly for a given seed.
func TestDSTNetResetDeterminism(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	run := func(seed uint64) bool {
		var srvReset bool
		simulation.RunWith(seed, simulation.Options{}, func() {
			port := make(chan string, 1)
			done := make(chan struct{})
			simulation.Host("A", simulation.HostConfig{}, func() {
				ln, _ := Listen("tcp", ":0")
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				go func() {
					c, _ := ln.Accept()
					_, err := c.Read(make([]byte, 16))
					srvReset = errors.Is(err, syscall.ECONNRESET)
					c.Close()
					close(done)
				}()
			})
			simulation.Host("B", simulation.HostConfig{}, func() {
				p := <-port
				c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
				time.Sleep(10 * time.Millisecond)
				simulation.Reset("A", "B")
				<-done
				c.Close()
			})
		})
		return srvReset
	}
	for seed := uint64(0); seed < 8; seed++ {
		if r1, r2 := run(seed), run(seed); r1 != r2 || !r1 {
			t.Fatalf("seed %d: reset not reproducible/effective: %v vs %v", seed, r1, r2)
		}
	}
}
