// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"context"
	"errors"
	"testing"
	"testing/simulation"
	"time"
)

// These tests exercise network partition targeting (simulation.Partition / Heal /
// Isolate / HealHost) over the always-wire cross-host transport. Invariants:
//   - a Dial across a cut blocks until heal or the context/deadline (connect blackhole);
//   - an established conn across a cut blocks reads while writes keep buffering, and a
//     heal delivers the buffered data IN ORDER with no loss (DST-FAULT-SOUND, the
//     buffer-and-recover model);
//   - a partition touches exactly the targeted host-pair, no leak onto other pairs
//     (DST-FAULT-VICTIM);
//   - it replays exactly for a given seed (DST-FAULT-REPLAY).

// TestDSTNetPartitionDialBlackhole: a Dial across a partition blocks (the SYN is
// dropped) until its deadline; after a heal the same dial succeeds.
func TestDSTNetPartitionDialBlackhole(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var blockedErr error
	var healedOK bool
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept() // only the post-heal dial gets through
				if err != nil {
					return
				}
				c.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			simulation.Partition("A", "B")
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			_, blockedErr = (&Dialer{}).DialContext(ctx, "tcp", simulation.HostIP("A")+":"+p)
			cancel()
			simulation.Heal("A", "B")
			if c, err := Dial("tcp", simulation.HostIP("A")+":"+p); err == nil {
				healedOK = true
				c.Close()
			}
		})
	})
	var ne Error
	if !errors.As(blockedErr, &ne) || !ne.Timeout() {
		t.Errorf("dial across partition = %v, want a timeout (SYN dropped, blocks until deadline)", blockedErr)
	}
	if !healedOK {
		t.Errorf("dial after heal failed, want success")
	}
}

// TestDSTNetPartitionRecover: an established conn blackholes reads during a
// partition (a deadline'd read times out), while writes keep buffering, and a heal
// delivers the buffered bytes in order with no loss (DST-FAULT-SOUND).
func TestDSTNetPartitionRecover(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var beforeMsg, afterMsg string
	var partitionedReadErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() { // server
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				buf := make([]byte, 32)
				n, _ := c.Read(buf)
				beforeMsg = string(buf[:n])
				c.SetReadDeadline(time.Now().Add(30 * time.Millisecond)) // times out while cut
				_, partitionedReadErr = c.Read(buf)
				c.SetReadDeadline(time.Time{})
				n, _ = c.Read(buf) // resumes after heal, in order
				afterMsg = string(buf[:n])
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() { // client + orchestrator
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("before"))
			time.Sleep(10 * time.Millisecond) // server reads "before"
			simulation.Partition("A", "B")
			c.Write([]byte("during"))          // buffered, blackholed
			time.Sleep(100 * time.Millisecond) // server's 30ms-deadline read times out
			simulation.Heal("A", "B")
			<-done
			c.Close()
		})
	})
	if beforeMsg != "before" {
		t.Errorf("pre-partition read = %q, want %q", beforeMsg, "before")
	}
	var ne Error
	if !errors.As(partitionedReadErr, &ne) || !ne.Timeout() {
		t.Errorf("read during partition = %v, want a timeout", partitionedReadErr)
	}
	if afterMsg != "during" {
		t.Errorf("post-heal read = %q, want %q (buffered data lost across the partition?)", afterMsg, "during")
	}
}

// TestDSTNetPartitionVictim: Partition(A,B) cuts exactly the A-B link — an A-C
// connection is untouched (DST-FAULT-VICTIM, no leak onto a non-victim pair).
func TestDSTNetPartitionVictim(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var abReadErr error
	var acMsg string
	simulation.RunWith(1, simulation.Options{}, func() {
		portB := make(chan string, 1)
		portC := make(chan string, 1)
		done := make(chan struct{})
		serve := func(name string, port chan string, got *string, readErr *error) {
			simulation.Host(name, simulation.HostConfig{}, func() {
				ln, _ := Listen("tcp", ":0")
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				go func() {
					c, _ := ln.Accept()
					buf := make([]byte, 32)
					c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
					n, err := c.Read(buf)
					if got != nil {
						*got = string(buf[:n])
					}
					if readErr != nil {
						*readErr = err
					}
					c.Close()
				}()
			})
		}
		var bGot string
		serve("B", portB, &bGot, &abReadErr)
		serve("C", portC, &acMsg, nil)
		simulation.Host("A", simulation.HostConfig{}, func() {
			pb, pc := <-portB, <-portC
			cb, _ := Dial("tcp", simulation.HostIP("B")+":"+pb) // establish both first
			cc, _ := Dial("tcp", simulation.HostIP("C")+":"+pc)
			simulation.Partition("A", "B") // cut only A-B
			cb.Write([]byte("toB"))        // blackholed: B's read times out
			cc.Write([]byte("toC"))        // delivered: C reads it
			time.Sleep(100 * time.Millisecond)
			cb.Close()
			cc.Close()
			close(done)
		})
		<-done
	})
	var ne Error
	if !errors.As(abReadErr, &ne) || !ne.Timeout() {
		t.Errorf("A-B read = %v, want a timeout (A-B partitioned)", abReadErr)
	}
	if acMsg != "toC" {
		t.Errorf("A-C read = %q, want %q (A-C must be unaffected by the A-B partition)", acMsg, "toC")
	}
}

// TestDSTNetIsolate: Isolate(A) cuts A from every other host; HealHost(A) restores.
func TestDSTNetIsolate(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var isolatedErr error
	var healedOK bool
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept() // only the post-heal dial gets through
				if err != nil {
					return
				}
				c.Close()
			}()
		})
		simulation.Host("client", simulation.HostConfig{}, func() {
			p := <-port
			simulation.Isolate("client")
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			_, isolatedErr = (&Dialer{}).DialContext(ctx, "tcp", simulation.HostIP("srv")+":"+p)
			cancel()
			simulation.HealHost("client")
			if c, err := Dial("tcp", simulation.HostIP("srv")+":"+p); err == nil {
				healedOK = true
				c.Close()
			}
		})
	})
	var ne Error
	if !errors.As(isolatedErr, &ne) || !ne.Timeout() {
		t.Errorf("dial while isolated = %v, want a timeout", isolatedErr)
	}
	if !healedOK {
		t.Errorf("dial after HealHost failed, want success")
	}
}

// TestDSTNetIsolateVictim: Isolate(A) cuts host A from everyone, but a B-C
// connection that does not involve A is untouched (DST-FAULT-VICTIM for the
// isolate form — isolation must not leak onto unrelated pairs).
func TestDSTNetIsolateVictim(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var bcMsg string
	simulation.RunWith(1, simulation.Options{}, func() {
		portC := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("C", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			portC <- p
			go func() {
				c, _ := ln.Accept()
				buf := make([]byte, 32)
				c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
				n, _ := c.Read(buf)
				bcMsg = string(buf[:n])
				c.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			pc := <-portC
			cc, _ := Dial("tcp", simulation.HostIP("C")+":"+pc)
			simulation.Isolate("A") // isolate a third host, not on this conn
			cc.Write([]byte("bc"))
			time.Sleep(50 * time.Millisecond)
			cc.Close()
			close(done)
		})
		<-done
	})
	if bcMsg != "bc" {
		t.Errorf("B-C read = %q, want %q (Isolate(A) must not affect an unrelated B-C conn)", bcMsg, "bc")
	}
}

// TestDSTNetPartitionDeterminism: partitioned runs replay exactly for a given seed
// (DST-FAULT-REPLAY). The whole exchange's observable outcome is reproduced.
func TestDSTNetPartitionDeterminism(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	run := func(seed uint64) (before, after string, timedOut bool) {
		simulation.RunWith(seed, simulation.Options{}, func() {
			port := make(chan string, 1)
			done := make(chan struct{})
			simulation.Host("A", simulation.HostConfig{}, func() {
				ln, _ := Listen("tcp", ":0")
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				go func() {
					c, _ := ln.Accept()
					buf := make([]byte, 32)
					n, _ := c.Read(buf)
					before = string(buf[:n])
					c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
					_, err := c.Read(buf)
					timedOut = err != nil
					c.SetReadDeadline(time.Time{})
					n, _ = c.Read(buf)
					after = string(buf[:n])
					c.Close()
					close(done)
				}()
			})
			simulation.Host("B", simulation.HostConfig{}, func() {
				p := <-port
				c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
				c.Write([]byte("x"))
				time.Sleep(10 * time.Millisecond)
				simulation.Partition("A", "B")
				c.Write([]byte("y"))
				time.Sleep(100 * time.Millisecond)
				simulation.Heal("A", "B")
				<-done
				c.Close()
			})
		})
		return
	}
	for seed := uint64(0); seed < 8; seed++ {
		b1, a1, t1 := run(seed)
		b2, a2, t2 := run(seed)
		if b1 != b2 || a1 != a2 || t1 != t2 {
			t.Fatalf("seed %d: partition run not reproducible: (%q,%q,%v) vs (%q,%q,%v)", seed, b1, a1, t1, b2, a2, t2)
		}
		if b1 != "x" || a1 != "y" || !t1 {
			t.Fatalf("seed %d: unexpected outcome (%q,%q,%v), want (x,y,true)", seed, b1, a1, t1)
		}
	}
}
