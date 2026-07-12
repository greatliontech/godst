// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"errors"
	"testing"
	"testing/simulation"
	"time"
)

type dstEpochSpyConn struct {
	calls int
}

func (c *dstEpochSpyConn) Read([]byte) (int, error)        { c.calls++; return 0, nil }
func (c *dstEpochSpyConn) Write(b []byte) (int, error)     { c.calls++; return len(b), nil }
func (c *dstEpochSpyConn) Close() error                    { c.calls++; return nil }
func (c *dstEpochSpyConn) LocalAddr() Addr                 { return pipeAddr{} }
func (c *dstEpochSpyConn) RemoteAddr() Addr                { return pipeAddr{} }
func (c *dstEpochSpyConn) SetDeadline(time.Time) error     { c.calls++; return nil }
func (c *dstEpochSpyConn) SetReadDeadline(time.Time) error { c.calls++; return nil }
func (c *dstEpochSpyConn) SetWriteDeadline(time.Time) error {
	c.calls++
	return nil
}

func TestDSTNetHandlesRejectCrossEpochStatefulUse(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var oldConn *dstConn
	var oldListener *dstListener
	var local, remote, listenerAddr string
	simulation.Run(1, func() {
		ln, _ := Listen("tcp", ":0")
		oldListener = ln.(*dstListener)
		accepted := make(chan struct{})
		go func() {
			c, _ := ln.Accept()
			close(accepted)
			defer c.Close()
		}()
		c, err := Dial("tcp", ln.Addr().String())
		if err != nil {
			panic(err)
		}
		oldConn = c.(*dstConn)
		<-accepted
		local, remote, listenerAddr = c.LocalAddr().String(), c.RemoteAddr().String(), ln.Addr().String()
	})

	// Run teardown normally closes these resources. Clear only the wrapper-local
	// close bit to model a leaked capability whose old transport must not be touched.
	oldConn.closed.Store(false)
	oldListener.closed.Store(false)
	spy := new(dstEpochSpyConn)
	oldConn.Conn = spy
	oldListener.accept = make(chan *dstOwnedConn, 1)
	oldListener.done = make(chan struct{})
	localCopy := oldConn.LocalAddr().(*TCPAddr)
	localCopy.Port++
	localCopy.IP[0] ^= 0xff
	remoteCopy := oldConn.RemoteAddr().(*TCPAddr)
	remoteCopy.Port++
	listenerCopy := oldListener.Addr().(*TCPAddr)
	listenerCopy.Port++
	if oldConn.LocalAddr().String() != local || oldConn.RemoteAddr().String() != remote || oldListener.Addr().String() != listenerAddr {
		t.Fatal("mutating returned addresses changed creation-time metadata")
	}
	_, readErr := oldConn.Read(make([]byte, 1))
	if !errors.Is(readErr, ErrClosed) {
		t.Errorf("stale Read = %v, want net.ErrClosed", readErr)
	}
	var readOp *OpError
	if !errors.As(readErr, &readOp) || readOp.Op != "read" || readOp.Source.String() != local || readOp.Addr.String() != remote {
		t.Fatalf("stale Read error = %#v, want read %s→%s", readErr, local, remote)
	}
	readOp.Source.(*TCPAddr).Port++
	readOp.Addr.(*TCPAddr).Port++
	if oldConn.LocalAddr().String() != local || oldConn.RemoteAddr().String() != remote {
		t.Fatal("mutating stale OpError addresses changed connection metadata")
	}
	if _, err := oldConn.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("stale Write = %v, want net.ErrClosed", err)
	}
	for name, set := range map[string]func() error{
		"SetDeadline":      func() error { return oldConn.SetDeadline(time.Time{}) },
		"SetReadDeadline":  func() error { return oldConn.SetReadDeadline(time.Time{}) },
		"SetWriteDeadline": func() error { return oldConn.SetWriteDeadline(time.Time{}) },
	} {
		if err := set(); !errors.Is(err, ErrClosed) {
			t.Errorf("stale %s = %v, want net.ErrClosed", name, err)
		} else {
			var opErr *OpError
			if !errors.As(err, &opErr) || opErr.Source != nil || opErr.Addr.String() != local {
				t.Fatalf("stale %s error = %#v, want nil source and local address", name, err)
			}
			opErr.Addr.(*TCPAddr).Port++
			if oldConn.LocalAddr().String() != local {
				t.Fatalf("mutating stale %s error changed connection metadata", name)
			}
		}
	}
	_, acceptErr := oldListener.Accept()
	if !errors.Is(acceptErr, ErrClosed) {
		t.Errorf("stale Accept = %v, want net.ErrClosed", acceptErr)
	}
	var acceptOp *OpError
	if !errors.As(acceptErr, &acceptOp) || acceptOp.Addr.String() != listenerAddr {
		t.Fatalf("stale Accept error = %#v, want listener address %s", acceptErr, listenerAddr)
	}
	acceptOp.Addr.(*TCPAddr).Port++
	if oldListener.Addr().String() != listenerAddr {
		t.Fatal("mutating stale Accept error changed listener metadata")
	}
	if spy.calls != 0 {
		t.Fatalf("stale stateful operations touched transport %d time(s)", spy.calls)
	}
	outsideSpy := new(dstEpochSpyConn)
	outsideConn := &dstConn{Conn: outsideSpy, epoch: oldConn.epoch, network: oldConn.network, local: oldConn.local, remote: oldConn.remote}
	outsideListener := &dstListener{epoch: oldListener.epoch, network: oldListener.network, addr: oldListener.addr, accept: make(chan *dstOwnedConn), done: make(chan struct{})}
	if err := outsideConn.Close(); err != nil {
		t.Fatalf("first no-run stale conn Close = %v, want nil", err)
	}
	if err := outsideListener.Close(); err != nil {
		t.Fatalf("first no-run stale listener Close = %v, want nil", err)
	}
	if err := outsideConn.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second no-run stale conn Close = %v, want net.ErrClosed", err)
	}
	if err := outsideListener.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second no-run stale listener Close = %v, want net.ErrClosed", err)
	}
	if outsideSpy.calls != 0 {
		t.Fatalf("no-run stale Close touched transport %d time(s)", outsideSpy.calls)
	}

	simulation.Run(2, func() {
		ln, _ := Listen("tcp", ":0")
		accepted := make(chan Conn, 1)
		go func() {
			c, _ := ln.Accept()
			accepted <- c
		}()
		c, err := Dial("tcp", ln.Addr().String())
		if err != nil {
			panic(err)
		}
		server := <-accepted
		dstNet.mu.Lock()
		dstNetRoll()
		listenersBefore := len(dstNet.listeners)
		dstNet.mu.Unlock()
		dstConns.mu.Lock()
		dstConnsRoll()
		connsBefore, holdsBefore := len(dstConns.set), len(dstConns.timeWait)
		dstConns.mu.Unlock()

		if err := oldConn.Close(); err != nil {
			t.Fatalf("first stale conn Close = %v, want nil", err)
		}
		if err := oldListener.Close(); err != nil {
			t.Fatalf("first stale listener Close = %v, want nil", err)
		}
		if err := oldConn.Close(); !errors.Is(err, ErrClosed) {
			t.Fatalf("second stale conn Close = %v, want net.ErrClosed", err)
		}
		if err := oldListener.Close(); !errors.Is(err, ErrClosed) {
			t.Fatalf("second stale listener Close = %v, want net.ErrClosed", err)
		}

		dstNet.mu.Lock()
		dstNetRoll()
		listenersAfter := len(dstNet.listeners)
		dstNet.mu.Unlock()
		dstConns.mu.Lock()
		dstConnsRoll()
		connsAfter, holdsAfter := len(dstConns.set), len(dstConns.timeWait)
		dstConns.mu.Unlock()
		if listenersAfter != listenersBefore || connsAfter != connsBefore || holdsAfter != holdsBefore {
			t.Fatalf("stale Close changed current registries: listeners %d→%d conns %d→%d holds %d→%d", listenersBefore, listenersAfter, connsBefore, connsAfter, holdsBefore, holdsAfter)
		}
		if spy.calls != 0 {
			t.Fatalf("stale Close touched transport %d time(s)", spy.calls)
		}
		c.Close()
		server.Close()
		ln.Close()
	})
}

func TestDSTNetHandleRejectsForeignCallerDuringCreationRun(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	type handles struct {
		conn     *dstConn
		listener *dstListener
	}
	type results struct {
		write, connClose, listenerClose error
	}
	handle := make(chan handles)
	result := make(chan results)
	go func() {
		h := <-handle
		_, writeErr := h.conn.Write([]byte("foreign"))
		result <- results{write: writeErr, connClose: h.conn.Close(), listenerClose: h.listener.Close()}
	}()
	simulation.Run(1, func() {
		ln, _ := Listen("tcp", ":0")
		accepted := make(chan Conn, 1)
		go func() {
			c, _ := ln.Accept()
			accepted <- c
		}()
		c, err := Dial("tcp", ln.Addr().String())
		if err != nil {
			panic(err)
		}
		server := <-accepted
		dc, dl := c.(*dstConn), ln.(*dstListener)
		handle <- handles{conn: dc, listener: dl}
		got := <-result
		if !errors.Is(got.write, ErrClosed) || !errors.Is(got.connClose, ErrClosed) || !errors.Is(got.listenerClose, ErrClosed) {
			t.Fatalf("foreign current-run operations = %#v, want net.ErrClosed", got)
		}
		if dc.closed.Load() || dl.closed.Load() {
			t.Fatal("foreign Close poisoned a live current-run handle")
		}
		c.Close()
		server.Close()
		ln.Close()
	})
}
