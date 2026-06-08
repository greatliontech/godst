// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"errors"
	"sync"
	_ "unsafe" // for go:linkname
)

// Under deterministic simulation testing (testing/simulation), net is virtualized
// to a fully in-memory, deterministic network: net.Dial/Listen stop touching the
// OS and run on an in-process registry keyed by address. Determinism comes for
// free from the existing machinery — connections are net.Pipe-backed (channel
// I/O, synctest-durable, deadlines on the fake clock), and connection/accept/
// delivery order is just the goroutine schedule, which is already deterministic.
//
// Only the exported string Dial/Listen are intercepted (the os.Getpid altitude),
// gated on dstActive(); net's internal lookups stay real. dstActive() is a
// constant false in a non -tags dst build, so this all compiles out there.
//
// This is the reliable, in-order base; network faults (partition/drop/reorder/
// latency) layer on top of the same registry+conns later.

//go:linkname dstActive runtime.dstActive
func dstActive() bool

//go:linkname dstNetEpoch runtime.dstNetEpoch
func dstNetEpoch() uint64

// dstNet is the process-global simulated network: a per-run registry of listeners
// keyed by address. Keyed by the run epoch (dstNetEpoch) so a new run starts with
// an empty registry, with no explicit teardown hook.
var dstNet struct {
	mu        sync.Mutex
	epoch     uint64
	listeners map[string]*dstListener
	nextPort  int // deterministic ephemeral local port for dialers
}

// dstNetRoll resets the registry when the run epoch advances. Caller holds the mu.
func dstNetRoll() {
	if e := dstNetEpoch(); e != dstNet.epoch || dstNet.listeners == nil {
		dstNet.epoch = e
		dstNet.listeners = make(map[string]*dstListener)
		dstNet.nextPort = 40000
	}
}

// dstAtoiPort parses a decimal port string (already validated by SplitHostPort).
func dstAtoiPort(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// dstHostIP maps a host string to an IP for a simulated address; a wildcard or
// empty host becomes the simulated loopback.
func dstHostIP(host string) IP {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return IPv4(127, 0, 0, 1)
	}
	if ip := ParseIP(host); ip != nil {
		return ip
	}
	return IPv4(127, 0, 0, 1) // unresolved name → loopback (DNS is a later increment)
}

// dstWildcard reports whether host means "any address".
func dstWildcard(host string) bool {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}

// dstConn is a simulated connection: a net.Pipe endpoint (Read/Write/Close/
// deadlines) wrapped with the connection's real local/remote addresses.
type dstConn struct {
	Conn
	local, remote Addr
}

func (c *dstConn) LocalAddr() Addr  { return c.local }
func (c *dstConn) RemoteAddr() Addr { return c.remote }

// dstListener is a simulated Listener. Dial pushes the server end of a new
// connection onto accept; Accept receives it.
type dstListener struct {
	network string
	addr    *TCPAddr
	key     string
	accept  chan Conn
	done    chan struct{}
	once    sync.Once
}

func (l *dstListener) Accept() (Conn, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-l.done:
		return nil, &OpError{Op: "accept", Net: l.network, Source: nil, Addr: l.addr, Err: errClosed}
	}
}

func (l *dstListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		dstNet.mu.Lock()
		if dstNet.listeners[l.key] == l {
			delete(dstNet.listeners, l.key)
		}
		dstNet.mu.Unlock()
	})
	return nil
}

func (l *dstListener) Addr() Addr { return l.addr }

// dstListen is net.Listen under DST: register a simulated listener.
func dstListen(network, address string) (Listener, error) {
	host, port, err := SplitHostPort(address)
	if err != nil {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: nil, Err: err}
	}
	key := host + ":" + port
	if dstWildcard(host) {
		key = ":" + port
	}
	addr := &TCPAddr{IP: dstHostIP(host), Port: dstAtoiPort(port)}

	dstNet.mu.Lock()
	defer dstNet.mu.Unlock()
	dstNetRoll()
	if _, dup := dstNet.listeners[key]; dup {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: addr, Err: errors.New("address already in use")}
	}
	l := &dstListener{
		network: network,
		addr:    addr,
		key:     key,
		accept:  make(chan Conn, 128), // backlog
		done:    make(chan struct{}),
	}
	dstNet.listeners[key] = l
	return l, nil
}

// dstDial is net.Dial under DST: find the matching listener and hand back the
// dialer end of a new in-memory connection.
func dstDial(network, address string) (Conn, error) {
	host, port, err := SplitHostPort(address)
	if err != nil {
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: nil, Err: err}
	}
	serverAddr := &TCPAddr{IP: dstHostIP(host), Port: dstAtoiPort(port)}

	dstNet.mu.Lock()
	dstNetRoll()
	l := dstNet.listeners[host+":"+port]
	if l == nil {
		l = dstNet.listeners[":"+port] // a wildcard listener on this port
	}
	localPort := dstNet.nextPort
	dstNet.nextPort++
	dstNet.mu.Unlock()

	if l == nil {
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: errors.New("connection refused")}
	}
	localAddr := &TCPAddr{IP: IPv4(127, 0, 0, 1), Port: localPort}

	p1, p2 := Pipe()
	dialer := &dstConn{Conn: p1, local: localAddr, remote: serverAddr}
	server := &dstConn{Conn: p2, local: serverAddr, remote: localAddr}
	select {
	case l.accept <- server:
		return dialer, nil
	case <-l.done:
		p1.Close()
		p2.Close()
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: errors.New("connection refused")}
	}
}
