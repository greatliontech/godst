// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"context"
	"errors"
	"strconv"
	"sync"
	_ "unsafe" // for go:linkname
)

const dstNetEnabled = true

// Under deterministic simulation testing (testing/simulation), net is virtualized
// to a fully in-memory, deterministic network: net.Dial/Listen stop touching the
// OS and run on an in-process registry keyed by address. Determinism comes for
// free from the existing machinery — connections are net.Pipe-backed (channel
// I/O, synctest-durable, deadlines on the fake clock), and connection/accept/
// delivery order is just the goroutine schedule, which is already deterministic.
//
// The exported string Dial/Listen are intercepted (the os.Getpid altitude),
// gated on dstActive(); typed and packet public entry points fail fast instead
// of falling through to host sockets until those surfaces are modeled. net's
// internal lookups stay real. dstActive() is a constant false in a non -tags dst
// build, so this all compiles out there.
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
	mu             sync.Mutex
	epoch          uint64
	listeners      map[string]*dstListener
	nextPort       int // deterministic ephemeral local port for dialers
	nextListenPort int // deterministic ephemeral port for listeners bound to :0
}

const (
	dstDialEphemeralStart   = 40000
	dstListenEphemeralStart = 10000
)

// dstNetRoll resets the registry when the run epoch advances. Caller holds the mu.
func dstNetRoll() {
	if e := dstNetEpoch(); e != dstNet.epoch || dstNet.listeners == nil {
		dstNet.epoch = e
		dstNet.listeners = make(map[string]*dstListener)
		dstNet.nextPort = dstDialEphemeralStart
		dstNet.nextListenPort = dstListenEphemeralStart
	}
}

func dstTCPNetwork(network string) bool {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return true
	}
	return false
}

func dstUnsupportedNetwork(op, network string) error {
	return &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: UnknownNetworkError(network)}
}

func dstUnsupportedNetAPI(op, network string, source, addr Addr) error {
	return &OpError{Op: op, Net: network, Source: source, Addr: addr, Err: errors.New("network API unsupported under deterministic simulation")}
}

func dstUnsupportedNetOption(op, network string, source, addr Addr, option string) error {
	return &OpError{Op: op, Net: network, Source: source, Addr: addr, Err: errors.New(option + " unsupported under deterministic simulation")}
}

func dstTCPAddrFamily(ip IP) string {
	if ip.To4() != nil {
		return "tcp4"
	}
	return "tcp6"
}

func dstResolveLocalTCPAddr(network string, remoteIP IP, local Addr) (*TCPAddr, error) {
	if local == nil {
		return nil, nil
	}
	addr, ok := local.(*TCPAddr)
	if !ok {
		return nil, &AddrError{Err: "mismatched local address type", Addr: local.String()}
	}
	if addr.Port < 0 || addr.Port > 65535 {
		return nil, &AddrError{Err: "invalid port", Addr: addr.String()}
	}
	if addr.IP == nil || addr.IP.IsUnspecified() {
		return &TCPAddr{IP: nil, Port: addr.Port, Zone: addr.Zone}, nil
	}
	if addr.IP.To4() == nil && addr.IP.To16() == nil {
		return nil, &AddrError{Err: errNoSuitableAddress.Error(), Addr: addr.String()}
	}
	switch network {
	case "tcp4":
		if addr.IP.To4() == nil {
			return nil, &AddrError{Err: errNoSuitableAddress.Error(), Addr: addr.String()}
		}
	case "tcp6":
		if addr.IP.To4() != nil || addr.IP.To16() == nil {
			return nil, &AddrError{Err: errNoSuitableAddress.Error(), Addr: addr.String()}
		}
	case "tcp":
		if dstTCPAddrFamily(addr.IP) != dstTCPAddrFamily(remoteIP) {
			return nil, &AddrError{Err: errNoSuitableAddress.Error(), Addr: addr.String()}
		}
	}
	return &TCPAddr{IP: append(IP(nil), addr.IP...), Port: addr.Port, Zone: addr.Zone}, nil
}

func dstDialOptionsError(d *Dialer, network string, source, addr Addr) error {
	if d.ControlContext != nil {
		return dstUnsupportedNetOption("dial", network, source, addr, "Dialer.ControlContext")
	}
	if d.Control != nil {
		return dstUnsupportedNetOption("dial", network, source, addr, "Dialer.Control")
	}
	if d.mptcpStatus == mptcpEnabledDial {
		return dstUnsupportedNetOption("dial", network, source, addr, "Dialer.MultipathTCP")
	}
	if d.KeepAlive != 0 || d.KeepAliveConfig != (KeepAliveConfig{}) {
		return dstUnsupportedNetOption("dial", network, source, addr, "Dialer.KeepAlive")
	}
	return nil
}

func dstListenOptionsError(lc *ListenConfig, network string, addr Addr) error {
	if lc.Control != nil {
		return dstUnsupportedNetOption("listen", network, nil, addr, "ListenConfig.Control")
	}
	if lc.mptcpStatus == mptcpEnabledListen {
		return dstUnsupportedNetOption("listen", network, nil, addr, "ListenConfig.MultipathTCP")
	}
	if lc.KeepAlive != 0 || lc.KeepAliveConfig != (KeepAliveConfig{}) {
		return dstUnsupportedNetOption("listen", network, nil, addr, "ListenConfig.KeepAlive")
	}
	return nil
}

func dstParsePort(op, network, port string) (int, error) {
	portnum, needsLookup := parsePort(port)
	if needsLookup || portnum < 0 || portnum > 65535 {
		return 0, &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: &AddrError{Err: "invalid port", Addr: port}}
	}
	return portnum, nil
}

// dstHostIP maps the host strings DST models without doing DNS. Wildcards and
// localhost become simulated loopback; arbitrary DNS names are rejected until DNS
// virtualization lands.
func dstHostIP(network, host string) (IP, bool) {
	switch host {
	case "":
		if network == "tcp6" {
			return IPv6loopback, true
		}
		return IPv4(127, 0, 0, 1), true
	case "0.0.0.0":
		return IPv4(127, 0, 0, 1), true
	case "::", "[::]":
		if network == "tcp4" {
			return IPv4(127, 0, 0, 1), true
		}
		return IPv6loopback, true
	case "localhost":
		if network == "tcp6" {
			return IPv6loopback, true
		}
		return IPv4(127, 0, 0, 1), true
	}
	if ip := ParseIP(host); ip != nil {
		return ip, true
	}
	return nil, false
}

func dstResolveHost(op, network, host string) (IP, error) {
	ip, ok := dstHostIP(network, host)
	if !ok {
		return nil, &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: &AddrError{Err: "DNS lookup unsupported under deterministic simulation", Addr: host}}
	}
	switch network {
	case "tcp4":
		if ip.To4() == nil {
			return nil, &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: &AddrError{Err: errNoSuitableAddress.Error(), Addr: host}}
		}
	case "tcp6":
		if ip.To4() != nil || ip.To16() == nil {
			return nil, &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: &AddrError{Err: errNoSuitableAddress.Error(), Addr: host}}
		}
	}
	return ip, nil
}

func dstAddrFamily(network string, ip IP) string {
	switch network {
	case "tcp4":
		return "tcp4"
	case "tcp6":
		return "tcp6"
	}
	if ip.To4() != nil {
		return "tcp4"
	}
	return "tcp6"
}

// dstWildcard reports whether host means "any address".
func dstWildcard(host string) bool {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}

func dstListenerKey(network string, ip IP, port int, wildcard bool) string {
	family := dstAddrFamily(network, ip)
	if wildcard {
		return family + "/:" + strconv.Itoa(port)
	}
	return family + "/" + ip.String() + ":" + strconv.Itoa(port)
}

func dstKeyHasPort(key string, port int) bool {
	suffix := ":" + strconv.Itoa(port)
	return len(key) >= len(suffix) && key[len(key)-len(suffix):] == suffix
}

func dstKeyHasPrefix(key, prefix string) bool {
	return len(key) >= len(prefix) && key[:len(prefix)] == prefix
}

func dstListenerConflict(network string, ip IP, key string, port int, wildcard bool) bool {
	if _, dup := dstNet.listeners[key]; dup {
		return true
	}
	familyPrefix := dstAddrFamily(network, ip) + "/"
	if wildcard {
		for k := range dstNet.listeners {
			if dstKeyHasPrefix(k, familyPrefix) && dstKeyHasPort(k, port) {
				return true
			}
		}
		return false
	}
	_, dup := dstNet.listeners[familyPrefix+":"+strconv.Itoa(port)]
	return dup
}

func dstAllocateListenPort(network string, ip IP, wildcard bool) (port int, key string, err error) {
	for p := dstNet.nextListenPort; p <= 65535; p++ {
		k := dstListenerKey(network, ip, p, wildcard)
		if !dstListenerConflict(network, ip, k, p, wildcard) {
			dstNet.nextListenPort = p + 1
			return p, k, nil
		}
	}
	return 0, "", errors.New("no free ports")
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
func dstListen(lc *ListenConfig, network, address string) (Listener, error) {
	if !dstTCPNetwork(network) {
		return nil, dstUnsupportedNetwork("listen", network)
	}
	host, port, err := SplitHostPort(address)
	if err != nil {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: nil, Err: err}
	}
	portnum, err := dstParsePort("listen", network, port)
	if err != nil {
		return nil, err
	}
	ip, err := dstResolveHost("listen", network, host)
	if err != nil {
		return nil, err
	}
	wildcard := dstWildcard(host)
	listenAddr := &TCPAddr{IP: ip, Port: portnum}
	if err := dstListenOptionsError(lc, network, listenAddr); err != nil {
		return nil, err
	}

	dstNet.mu.Lock()
	defer dstNet.mu.Unlock()
	dstNetRoll()
	var key string
	if portnum == 0 {
		portnum, key, err = dstAllocateListenPort(network, ip, wildcard)
		if err != nil {
			return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: nil, Err: err}
		}
	} else {
		key = dstListenerKey(network, ip, portnum, wildcard)
	}
	addr := &TCPAddr{IP: ip, Port: portnum}
	if dstListenerConflict(network, ip, key, portnum, wildcard) {
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
func dstDial(ctx context.Context, d *Dialer, network, address string) (Conn, error) {
	if !dstTCPNetwork(network) {
		return nil, dstUnsupportedNetwork("dial", network)
	}
	host, port, err := SplitHostPort(address)
	if err != nil {
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: nil, Err: err}
	}
	portnum, err := dstParsePort("dial", network, port)
	if err != nil {
		return nil, err
	}
	ip, err := dstResolveHost("dial", network, host)
	if err != nil {
		return nil, err
	}
	serverAddr := &TCPAddr{IP: ip, Port: portnum}
	if err := ctx.Err(); err != nil {
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: mapErr(err)}
	}
	localTCPAddr, err := dstResolveLocalTCPAddr(network, ip, d.LocalAddr)
	if err != nil {
		return nil, &OpError{Op: "dial", Net: network, Source: d.LocalAddr, Addr: serverAddr, Err: err}
	}
	if err := dstDialOptionsError(d, network, localTCPAddr.opAddr(), serverAddr); err != nil {
		return nil, err
	}

	dstNet.mu.Lock()
	dstNetRoll()
	l := dstNet.listeners[dstListenerKey(network, ip, portnum, false)]
	if l == nil {
		l = dstNet.listeners[dstListenerKey(network, ip, portnum, true)] // a wildcard listener on this port/family
	}
	localPort := 0
	if localTCPAddr != nil {
		localPort = localTCPAddr.Port
	}
	if localPort == 0 {
		localPort = dstNet.nextPort
		dstNet.nextPort++
	}
	dstNet.mu.Unlock()

	if l == nil {
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: errors.New("connection refused")}
	}
	localIP := IPv4(127, 0, 0, 1)
	if dstAddrFamily(network, ip) == "tcp6" {
		localIP = IPv6loopback
	}
	if localTCPAddr != nil && localTCPAddr.IP != nil && !localTCPAddr.IP.IsUnspecified() {
		localIP = localTCPAddr.IP
	}
	localAddr := &TCPAddr{IP: localIP, Port: localPort}
	if localTCPAddr != nil {
		localAddr.Zone = localTCPAddr.Zone
	}

	p1, p2 := Pipe()
	dialer := &dstConn{Conn: p1, local: localAddr, remote: serverAddr}
	server := &dstConn{Conn: p2, local: serverAddr, remote: localAddr}
	select {
	case <-ctx.Done():
		p1.Close()
		p2.Close()
		return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: mapErr(ctx.Err())}
	case l.accept <- server:
		return dialer, nil
	case <-l.done:
		p1.Close()
		p2.Close()
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: errors.New("connection refused")}
	}
}
