// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"context"
	"errors"
	"internal/bytealg"
	"internal/nettrace"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
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

// dstUnsupportedNetwork rejects a network Dial/Listen cannot model. A KNOWN
// but unmodeled network (UDP, Unix, IP) gets the same "unsupported under
// deterministic simulation" shape as the typed APIs — it is a simulation
// boundary, not an unknown name; a genuinely unknown network string keeps the
// production UnknownNetworkError identity.
func dstUnsupportedNetwork(op, network string) error {
	base := network
	if i := bytealg.LastIndexByteString(base, ':'); i >= 0 {
		// Production accepts a ":proto" suffix only on the ip networks
		// (parseNetwork); other colon-bearing strings are unknown networks.
		switch base[:i] {
		case "ip", "ip4", "ip6":
			base = base[:i]
		}
	}
	switch base {
	case "udp", "udp4", "udp6", "unix", "unixgram", "unixpacket", "ip", "ip4", "ip6":
		return &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: errors.New("network " + network + " unsupported under deterministic simulation")}
	}
	return &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: UnknownNetworkError(network)}
}

func dstUnsupportedNetAPI(op, network string, source, addr Addr) error {
	return &OpError{Op: op, Net: network, Source: source, Addr: addr, Err: errors.New("network API unsupported under deterministic simulation")}
}

func dstUnsupportedNetOption(op, network string, source, addr Addr, option string) error {
	return &OpError{Op: op, Net: network, Source: source, Addr: addr, Err: errors.New(option + " unsupported under deterministic simulation")}
}

func dstUnsupportedDNSLookup(name string) error {
	return &DNSError{Err: "DNS lookup unsupported under deterministic simulation", Name: name}
}

func dstUnsupportedServiceLookup(network, service string) error {
	return &DNSError{Err: "service lookup unsupported under deterministic simulation", Name: network + "/" + service}
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

func dstAllocateListenPort(network string, ip IP, wildcard, dual bool) (port int, keys []string, err error) {
	for p := dstNet.nextListenPort; p <= 65535; p++ {
		if dual {
			ks := dstDualKeys(p)
			if !dstAnyListenerConflict(network, ip, ks, p, wildcard, true) {
				dstNet.nextListenPort = p + 1
				return p, ks, nil
			}
			continue
		}
		k := dstListenerKey(network, ip, p, wildcard)
		if !dstListenerConflict(network, ip, k, p, wildcard) {
			dstNet.nextListenPort = p + 1
			return p, []string{k}, nil
		}
	}
	return 0, nil, errors.New("no free ports")
}

// dstConn is a simulated connection: a net.Pipe endpoint (Read/Write/Close/
// deadlines on the bubble's fake clock) wrapped with the connection's real
// local/remote addresses and production-shaped error identity. The raw pipe
// errors (io.ErrClosedPipe, bare os.ErrDeadlineExceeded, OpError{Net:"pipe"})
// would break production-shaped code: after a local Close every op must
// satisfy errors.Is(err, net.ErrClosed); a connection reset (listener backlog
// teardown) and writes to a closed peer must carry syscall.ECONNRESET; reads
// from a gracefully closed peer return io.EOF; deadline errors are *OpError
// wrapping os.ErrDeadlineExceeded with the connection's network and addresses.
type dstConn struct {
	Conn
	network       string
	local, remote Addr
	closed        atomic.Bool  // this end was Closed by its user
	reset         *atomic.Bool // connection reset (shared by both ends)

	// acceptState tracks a server-end connection through the accept backlog:
	// 0 queued, 1 accepted, 2 reset/refused. The listener's Accept claims
	// 0→1; the backlog teardown and the dialer's post-send listener-closed
	// check claim 0→2 — so a connection the server already Accepted can never
	// be retroactively reset by teardown, and a torn-down connection is never
	// handed out by Accept. Nil on the dialer end.
	acceptState *atomic.Int32
}

func (c *dstConn) LocalAddr() Addr  { return c.local }
func (c *dstConn) RemoteAddr() Addr { return c.remote }

func (c *dstConn) opError(op string, err error) error {
	return &OpError{Op: op, Net: c.network, Source: c.local, Addr: c.remote, Err: err}
}

// resetConn tears the connection down as a reset: the peer's subsequent reads
// and writes fail with ECONNRESET (production's RST), not EOF/closed-pipe.
func (c *dstConn) resetConn() {
	c.reset.Store(true)
	c.Conn.Close()
}

// mapConnErr converts a pipe-layer error into the production shape.
func (c *dstConn) mapConnErr(op string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return c.opError(op, os.ErrDeadlineExceeded)
	case c.closed.Load():
		return c.opError(op, errClosed)
	case err == io.EOF:
		return io.EOF // graceful peer close (a reset read is mapped in Read)
	case err == io.ErrClosedPipe:
		// Peer closed or connection reset while we operate: production
		// surfaces a reset. (This also covers ops on a reset end itself: a
		// reset closes the underlying pipe, so its own ops land here.)
		return c.opError(op, syscall.ECONNRESET)
	}
	// Unreachable today: the pipe wraps only deadline errors (caught above) in
	// its own OpError. Kept as a conservative wrap so a future pipe error
	// cannot leak pipe-shaped identity.
	return c.opError(op, err)
}

func (c *dstConn) Read(b []byte) (int, error) {
	if c.closed.Load() {
		return 0, c.opError("read", errClosed)
	}
	n, err := c.Conn.Read(b)
	if err == io.EOF && c.reset.Load() {
		return n, c.opError("read", syscall.ECONNRESET)
	}
	return n, c.mapConnErr("read", err)
}

func (c *dstConn) Write(b []byte) (int, error) {
	if c.closed.Load() {
		return 0, c.opError("write", errClosed)
	}
	n, err := c.Conn.Write(b)
	return n, c.mapConnErr("write", err)
}

func (c *dstConn) Close() error {
	if c.closed.Swap(true) {
		return c.opError("close", errClosed)
	}
	c.Conn.Close()
	return nil
}

// setDeadline applies a deadline with production error identity: after a
// local Close it fails with net.ErrClosed; after a peer close it succeeds (a
// peer FIN does not invalidate the local endpoint — subsequent reads return
// EOF/ECONNRESET immediately anyway, so the dropped deadline is unobservable).
func (c *dstConn) setDeadline(set func(time.Time) error, t time.Time) error {
	// Production shape for a set-deadline failure: Source nil, Addr local.
	closedErr := &OpError{Op: "set", Net: c.network, Source: nil, Addr: c.local, Err: errClosed}
	if c.closed.Load() {
		return closedErr
	}
	if err := set(t); err != nil && !c.closed.Load() {
		// The pipe rejects deadlines once either side is done; only a LOCAL
		// close is an error in production.
		return nil
	} else if err != nil {
		return closedErr
	}
	return nil
}

func (c *dstConn) SetDeadline(t time.Time) error {
	return c.setDeadline(c.Conn.SetDeadline, t)
}

func (c *dstConn) SetReadDeadline(t time.Time) error {
	return c.setDeadline(c.Conn.SetReadDeadline, t)
}

func (c *dstConn) SetWriteDeadline(t time.Time) error {
	return c.setDeadline(c.Conn.SetWriteDeadline, t)
}

// dstListener is a simulated Listener. Dial pushes the server end of a new
// connection onto accept; Accept receives it. A dual-stack wildcard listener
// (plain "tcp" on a wildcard host) registers under both family keys.
type dstListener struct {
	network string
	addr    *TCPAddr
	keys    []string
	accept  chan Conn
	done    chan struct{}
	closed  atomic.Bool
}

func (l *dstListener) opError(op string, err error) error {
	return &OpError{Op: op, Net: l.network, Source: nil, Addr: l.addr, Err: err}
}

func (l *dstListener) Accept() (Conn, error) {
	// Closed-first: production Accept after Close always fails with ErrClosed,
	// even if connections were still queued in the backlog (Close reset them).
	select {
	case <-l.done:
		return nil, l.opError("accept", errClosed)
	default:
	}
	for {
		select {
		case c := <-l.accept:
			if dc, ok := c.(*dstConn); ok && dc.acceptState != nil && !dc.acceptState.CompareAndSwap(0, 1) {
				// Torn down (reset/refused) while queued; never hand it out.
				continue
			}
			// An Accept already parked in this select when Close ran can win
			// the queued-connection case over the just-closed done case (the
			// seeded select picks among ready cases); production unblocks
			// every pending Accept with ErrClosed unconditionally. Recheck:
			// if Close has begun, this connection belongs to the backlog
			// Close resets — our claim above (acceptState 0→1) made Close's
			// drain skip it, so the reset is ours to perform.
			select {
			case <-l.done:
				if dc, ok := c.(*dstConn); ok {
					dc.resetConn()
				} else {
					c.Close()
				}
				return nil, l.opError("accept", errClosed)
			default:
			}
			return c, nil
		case <-l.done:
			return nil, l.opError("accept", errClosed)
		}
	}
}

func (l *dstListener) Close() error {
	if l.closed.Swap(true) {
		return l.opError("close", errClosed)
	}
	close(l.done)
	dstNet.mu.Lock()
	for _, k := range l.keys {
		if dstNet.listeners[k] == l {
			delete(dstNet.listeners, k)
		}
	}
	dstNet.mu.Unlock()
	// Production TCP resets connections still sitting in the accept backlog
	// when the listener closes; mirror it, so a dialer that already got a
	// successful Dial observes ECONNRESET on its next op instead of blocking
	// durably forever on a connection no one will ever accept.
	for {
		select {
		case c := <-l.accept:
			if dc, ok := c.(*dstConn); ok {
				if dc.acceptState == nil || dc.acceptState.CompareAndSwap(0, 2) {
					dc.resetConn()
				}
			} else {
				c.Close()
			}
		default:
			return nil
		}
	}
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
	// A plain-"tcp" wildcard listen is dual-stack in production (it accepts
	// both IPv4 and IPv6 peers); model it by registering under both family
	// keys. "0.0.0.0" stays IPv4-only and "tcp4"/"tcp6" stay single-family,
	// as in production.
	dual := network == "tcp" && wildcard && host != "0.0.0.0"
	// The address the listener reports (Addr and error texts): production
	// reports the wildcard form for wildcard listens (0.0.0.0:p / [::]:p, the
	// IPv6 form for dual-stack), not the loopback the simulation maps them to
	// internally. The reported form stays dialable: dstHostIP maps it back to
	// the simulated loopback of the same family.
	reportIP := ip
	if dual {
		reportIP = IPv6zero
	} else if wildcard {
		if dstAddrFamily(network, ip) == "tcp4" {
			reportIP = IPv4zero
		} else {
			reportIP = IPv6zero
		}
	}
	if err := dstListenOptionsError(lc, network, &TCPAddr{IP: reportIP, Port: portnum}); err != nil {
		return nil, err
	}

	dstNet.mu.Lock()
	defer dstNet.mu.Unlock()
	dstNetRoll()
	var keys []string
	if portnum == 0 {
		portnum, keys, err = dstAllocateListenPort(network, ip, wildcard, dual)
		if err != nil {
			return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: nil, Err: err}
		}
	} else if dual {
		keys = dstDualKeys(portnum)
	} else {
		keys = []string{dstListenerKey(network, ip, portnum, wildcard)}
	}
	addr := &TCPAddr{IP: reportIP, Port: portnum}
	if dstAnyListenerConflict(network, ip, keys, portnum, wildcard, dual) {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: addr, Err: syscall.EADDRINUSE}
	}
	l := &dstListener{
		network: network,
		addr:    addr,
		keys:    keys,
		accept:  make(chan Conn, 128), // backlog
		done:    make(chan struct{}),
	}
	for _, k := range keys {
		dstNet.listeners[k] = l
	}
	return l, nil
}

func dstDualKeys(port int) []string {
	p := strconv.Itoa(port)
	return []string{"tcp4/:" + p, "tcp6/:" + p}
}

// dstAnyListenerConflict reports whether registering keys would conflict; a
// dual-stack wildcard conflicts with any listener of either family on the port.
func dstAnyListenerConflict(network string, ip IP, keys []string, port int, wildcard, dual bool) bool {
	if dual {
		return dstListenerConflict("tcp4", nil, keys[0], port, true) ||
			dstListenerConflict("tcp6", nil, keys[1], port, true)
	}
	return dstListenerConflict(network, ip, keys[0], port, wildcard)
}

// dstDial is net.Dial under DST: find the matching listener and hand back the
// dialer end of a new in-memory connection.
func dstDial(ctx context.Context, d *Dialer, network, address string) (retConn Conn, retErr error) {
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
	// Fire the nettrace connect callbacks around the simulated connect with
	// the RESOLVED address, as production does per connect attempt — and not
	// at all for addresses that fail validation above.
	if trace, _ := ctx.Value(nettrace.TraceKey{}).(*nettrace.Trace); trace != nil {
		if trace.ConnectStart != nil {
			trace.ConnectStart(network, serverAddr.String())
		}
		if trace.ConnectDone != nil {
			defer func() { trace.ConnectDone(network, serverAddr.String(), retErr) }()
		}
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
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.ECONNREFUSED}
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
	reset := new(atomic.Bool)
	dialer := &dstConn{Conn: p1, network: network, local: localAddr, remote: serverAddr, reset: reset}
	server := &dstConn{Conn: p2, network: network, local: serverAddr, remote: localAddr, reset: reset, acceptState: new(atomic.Int32)}
	select {
	case <-ctx.Done():
		p1.Close()
		p2.Close()
		return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: mapErr(ctx.Err())}
	case l.accept <- server:
		select {
		case <-l.done:
			// The listener closed while this connection sat in (or entered)
			// the backlog; its teardown resets QUEUED connections, but this
			// send may have landed after the drain — or the server may have
			// already Accepted it before we resumed. Claim it: if it is still
			// queued, refuse the dial; if Accept won, the connection stands.
			if server.acceptState.CompareAndSwap(0, 2) {
				server.resetConn()
				p1.Close()
				return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.ECONNREFUSED}
			}
			return dialer, nil
		default:
			return dialer, nil
		}
	case <-l.done:
		p1.Close()
		p2.Close()
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.ECONNREFUSED}
	}
}
