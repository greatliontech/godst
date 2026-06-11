// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"errors"
	"runtime"
	"syscall"
	"testing"
	"testing/simulation"
)

func TestDSTNetClosedListenerDialErrorIdentity(t *testing.T) {
	simulation.Run(1, func() {
		blocked, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer blocked.Close()
		backlog := cap(blocked.(*dstListener).accept)
		var conns []Conn
		defer func() {
			for _, c := range conns {
				c.Close()
			}
		}()
		for i := 0; i < backlog; i++ {
			c, err := Dial("tcp", blocked.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			conns = append(conns, c)
		}
		errc := make(chan error, 1)
		go func() {
			c, err := Dial("tcp", blocked.Addr().String())
			if c != nil {
				c.Close()
			}
			errc <- err
		}()
		runtime.Gosched()
		blocked.Close()
		if err := <-errc; !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("closed-listener Dial error = %v, want errors.Is ECONNREFUSED", err)
		}
	})
}
