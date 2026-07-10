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
	dstDialEphemeralEnd     = 65535 // the last valid TCP port; the ephemeral range wraps within [start, end]
	dstListenEphemeralStart = 10000
)

// dstAllocEphemeralPort returns the next free ephemeral local port for a dialer on
// host with source IP localIP, advancing the per-run counter and WRAPPING within
// [dstDialEphemeralStart, dstDialEphemeralEnd] — so it never returns a port above
// 65535 (the bare counter reached impossible numbers after ~25k dials) and never
// duplicates a still-live local addr:port (a real ephemeral allocator skips live
// ports — a conn's local end or a LISTENER on the host, exact or wildcard).
// Returns 0 when the whole range is live (EADDRNOTAVAIL). Caller holds
// dstNet.mu; the conn probe nests dstConns.mu under it (a fixed lock order).
func dstAllocEphemeralPort(host uint32, localIP IP) int {
	scope := dstNetScope(host)
	const span = dstDialEphemeralEnd - dstDialEphemeralStart + 1
	for tried := 0; tried < span; tried++ {
		p := dstNet.nextPort
		dstNet.nextPort++
		if dstNet.nextPort > dstDialEphemeralEnd {
			dstNet.nextPort = dstDialEphemeralStart
		}
		if dstLocalBindInUse(host, localIP, p) {
			continue
		}
		if dstListenerConflict(scope, "tcp", localIP, dstListenerKey("tcp", localIP, p, false), p, false) {
			continue
		}
		return p
	}
	return 0
}

// dstNetRoll resets the registry when the run epoch advances. Caller holds the mu.
func dstNetRoll() {
	if e := dstNetEpoch(); e != dstNet.epoch || dstNet.listeners == nil {
		dstNet.epoch = e
		dstNet.listeners = make(map[string]*dstListener)
		dstNet.nextPort = dstDialEphemeralStart
		dstNet.nextListenPort = dstListenEphemeralStart
	}
}

//go:linkname dstNetCurrentNode runtime.dstCurrentNode
func dstNetCurrentNode() (host, proc uint32)

// The simulated network is per HOST (testing/simulation.Host): every listener is
// scoped to its listening host, so loopback is host-private (two hosts each have
// their own 127.0.0.1) and the port space is per-host (two hosts can both bind
// :80). Each host also has a deterministic routable IPv4 (10.<hi>.<mid>.<lo> from
// its host id) so a process on one host reaches another by its routable IP — the
// implicit full mesh. A dial resolves its target to a host scope: a loopback target
// is the dialer's OWN host; a routable 10.x target is the host that IP encodes. The
// default host 0 (a program that declares no Host) is the only host, so its
// loopback registry is the whole network — identical to the pre-per-host behaviour.

// dstNetScope is the registry-key prefix for a host's listeners.
func dstNetScope(host uint32) string {
	return "h" + strconv.FormatUint(uint64(host), 10) + "|"
}

// dstHostRoutableIP is host's deterministic routable IPv4 (the 10.0.0.0/8 block,
// the host id in the low three octets).
func dstHostRoutableIP(host uint32) IP {
	if host >= 1<<24 {
		// The 10.0.0.0/8 block encodes the host id in three octets; a larger id
		// would alias another host's IP and silently misroute. No realistic run
		// declares 2^24 hosts — fail loud rather than misroute.
		panic("net: DST simulated routable-IP space exhausted (max 2^24 hosts)")
	}
	return IPv4(10, byte(host>>16), byte(host>>8), byte(host))
}

// dstHostRoutableIPString is the string form, exposed to testing/simulation.HostIP
// via //go:linkname so a SUT can address a peer host without DNS. HostIP returns a
// string (not a net.IP) so testing/simulation need not import net — which would
// cycle with net's own white-box DST tests.
//
//go:linkname dstHostRoutableIPString
func dstHostRoutableIPString(host uint32) string {
	return dstHostRoutableIP(host).String()
}

// dstInterfaces is the calling host's fixed synthetic interface set: a loopback
// (lo) and one Ethernet (eth0) bearing the host's routable IP. Under simulation
// net.Interfaces returns this instead of the real machine's NICs — deterministic
// per host and with no real-interface leak (DST-IDENTITY-SOUND), and consistent
// with the host-owned address model (loopback host-private, eth0 = 10.0.0.<host>).
func dstInterfaces() []Interface {
	host, _ := dstNetCurrentNode()
	return []Interface{
		{Index: 1, MTU: 65536, Name: "lo", Flags: FlagUp | FlagLoopback | FlagRunning},
		{Index: 2, MTU: 1500, Name: "eth0", HardwareAddr: dstHostMAC(host), Flags: FlagUp | FlagBroadcast | FlagMulticast | FlagRunning},
	}
}

// dstHostMAC is a host's deterministic locally-administered MAC (02:00 prefix, the
// host id in the low four octets), so a SUT that reads HardwareAddr replays.
func dstHostMAC(host uint32) HardwareAddr {
	return HardwareAddr{0x02, 0x00, byte(host >> 24), byte(host >> 16), byte(host >> 8), byte(host)}
}

// dstInterfaceAddrs returns the unicast addresses of interface ifi in the calling
// host's synthetic set (lo: 127.0.0.1/8 and ::1/128; eth0: the routable 10.x /24),
// or every interface's addresses when ifi is nil (net.InterfaceAddrs). An interface
// not in the synthetic set has no addresses.
func dstInterfaceAddrs(ifi *Interface) []Addr {
	host, _ := dstNetCurrentNode()
	lo := []Addr{
		&IPNet{IP: IPv4(127, 0, 0, 1), Mask: CIDRMask(8, 32)},
		&IPNet{IP: ParseIP("::1"), Mask: CIDRMask(128, 128)},
	}
	eth0 := []Addr{
		&IPNet{IP: dstHostRoutableIP(host), Mask: CIDRMask(24, 32)},
	}
	switch {
	case ifi == nil:
		return append(append([]Addr{}, lo...), eth0...)
	case ifi.Index == 1:
		return lo
	case ifi.Index == 2:
		return eth0
	default:
		return nil
	}
}

// dstHostForRoutableIP reports the host a routable 10.x IP encodes (ok=false for a
// non-routable IP, e.g. loopback).
func dstHostForRoutableIP(ip IP) (uint32, bool) {
	v4 := ip.To4()
	if v4 == nil || v4[0] != 10 {
		return 0, false
	}
	return uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3]), true
}

// dstDialScope returns the host scope a dial to ip resolves in: a routable 10.x IP
// names its owning host; anything else (loopback, wildcard mapped to loopback) is
// the dialer's own host. dialer is the calling goroutine's host.
func dstDialScope(ip IP, dialer uint32) string {
	if h, ok := dstHostForRoutableIP(ip); ok {
		return dstNetScope(h)
	}
	return dstNetScope(dialer)
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

func dstListenerConflict(scope, network string, ip IP, key string, port int, wildcard bool) bool {
	if _, dup := dstNet.listeners[scope+key]; dup {
		return true
	}
	familyPrefix := scope + dstAddrFamily(network, ip) + "/"
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

// dstAllocateListenPort picks the next free listener port, advancing the
// per-run counter and WRAPPING within [dstListenEphemeralStart, 65535] — a
// closed listener's port is reclaimed on the next pass, as real kernels reuse
// freed ephemeral ports, so a long-lived run can listen-close indefinitely
// (the unwrapped counter exhausted after ~55k listens with every port free).
// Conflict-occupied candidates (a live listener, or a dialer-end conn's local
// 2-tuple) are skipped. A whole live range fails EADDRINUSE, bind(2)'s
// exhaustion identity. Caller holds dstNet.mu.
func dstAllocateListenPort(scope, network string, ip IP, host uint32, wildcard, dual bool) (port int, keys []string, err error) {
	const span = 65535 - dstListenEphemeralStart + 1
	for tried := 0; tried < span; tried++ {
		p := dstNet.nextListenPort
		dstNet.nextListenPort++
		if dstNet.nextListenPort > 65535 {
			dstNet.nextListenPort = dstListenEphemeralStart
		}
		if dstListenConnConflict(host, network, ip, p, wildcard, dual) {
			continue // a dialer-end conn occupies the port (no SO_REUSEADDR)
		}
		if dual {
			ks := dstDualKeys(p)
			if !dstAnyListenerConflict(scope, network, ip, ks, p, wildcard, true) {
				return p, ks, nil
			}
			continue
		}
		k := dstListenerKey(network, ip, p, wildcard)
		if !dstListenerConflict(scope, network, ip, k, p, wildcard) {
			return p, []string{k}, nil
		}
	}
	return 0, nil, syscall.EADDRINUSE
}

// dstListenConnConflict reports whether a live DIALER-end conn on host blocks a
// new listener at (ip, port): the dialer's socket carries no SO_REUSEADDR, so
// bind(2) fails on any overlap — exact for a specific listen, any IP of the
// listen family for a wildcard, either family for dual. Accepted server ends
// inherit the listener's SO_REUSEADDR and never block (a restarted server
// re-binds its port while old connections drain). Caller holds dstNet.mu.
func dstListenConnConflict(host uint32, network string, ip IP, port int, wildcard, dual bool) bool {
	switch {
	case dual:
		return dstConnBindInUse(host, nil, port, "", true)
	case wildcard:
		return dstConnBindInUse(host, nil, port, dstAddrFamily(network, ip), true)
	default:
		return dstConnBindInUse(host, ip, port, "", true)
	}
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

	// localHost/remoteHost attribute the connection's two ends to their owning
	// hosts (this end's host and the peer's). Stamped at Dial — the dialer's host
	// and the listening host — and the foundation faults target a connection by:
	// a host-pair partition/latency/reset acts on exactly the connections whose
	// {localHost, remoteHost} match the pair (DST-FAULT-VICTIM). The base link
	// latency is the first consumer: a connection between distinct hosts carries
	// the configured cross-host delay, a same-host/loopback one is instant.
	// localProc/remoteProc likewise attribute each end to its owning process (the
	// dialer's process and the listening process), so a process-targeted reset acts
	// on exactly that process's conns (DST-FAULT-VICTIM, the process leg).
	localHost, remoteHost uint32
	localProc, remoteProc uint32

	// regSeq is the per-run registration sequence, stamped at dstConnRegister, so a
	// multi-victim reset orders its victims deterministically (registration order =
	// Dial order, schedule-determined) rather than by pointer-map iteration order.
	regSeq uint64

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
	dstConnDeregister(c)
}

// mapConnErr converts a pipe-layer error into the production shape.
func (c *dstConn) mapConnErr(op string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return c.opError(op, os.ErrDeadlineExceeded)
	case err == syscall.ETIMEDOUT:
		// The retransmit horizon: a write/read into a permanently undeliverable conn
		// (a cut outlasting the horizon). Production identity is OpError{ETIMEDOUT}.
		return c.opError(op, syscall.ETIMEDOUT)
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
	// The kernel's close(2) conditional: an end whose receive queue holds
	// unread data answers the peer with RST — the peer's next read fails
	// ECONNRESET without draining — otherwise the close FINs and the peer
	// drains buffered bytes to io.EOF. Bytes still in flight count as queued
	// (the recorded collapse: the sim RSTs immediately, one of the two
	// orderings the real close-vs-arrival race produces). The type assertion
	// is the same wire-backed transport contract dstCloseProcConns records.
	if e, ok := c.Conn.(*dstWireEnd); ok && e.unreadInbound() {
		dstResetBothEnds(c)
		return nil
	}
	c.Conn.Close()
	dstConnDeregister(c)
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
	host    uint32 // the host that owns this listener (its network identity)
	proc    uint32 // the process that created this listener (owns its accepted conns)
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
	listeningHost, listeningProc := dstNetCurrentNode()
	scope := dstNetScope(listeningHost)
	// A host may bind only an address it owns: its loopback, its own routable IP,
	// or a wildcard. Binding another host's routable IP — or any other literal IP —
	// is EADDRNOTAVAIL, exactly as on a real host.
	if !wildcard && !ip.IsLoopback() {
		if h, ok := dstHostForRoutableIP(ip); !ok || h != listeningHost {
			return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: &TCPAddr{IP: ip, Port: portnum}, Err: syscall.EADDRNOTAVAIL}
		}
	}
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
		portnum, keys, err = dstAllocateListenPort(scope, network, ip, listeningHost, wildcard, dual)
		if err != nil {
			// Production wraps a bind failure with the REQUESTED address
			// (port 0), not a nil Addr.
			return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: &TCPAddr{IP: reportIP, Port: 0}, Err: err}
		}
	} else if dual {
		keys = dstDualKeys(portnum)
	} else {
		keys = []string{dstListenerKey(network, ip, portnum, wildcard)}
	}
	addr := &TCPAddr{IP: reportIP, Port: portnum}
	if dstAnyListenerConflict(scope, network, ip, keys, portnum, wildcard, dual) ||
		dstListenConnConflict(listeningHost, network, ip, portnum, wildcard, dual) {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: addr, Err: syscall.EADDRINUSE}
	}
	// Scope the keys to the listening host: every registry entry is host-scoped, so
	// loopback and the port space are per-host. l.keys holds the scoped keys so
	// Close removes exactly these entries.
	scoped := make([]string, len(keys))
	for i, k := range keys {
		scoped[i] = scope + k
	}
	l := &dstListener{
		network: network,
		addr:    addr,
		keys:    scoped,
		accept:  make(chan Conn, 128), // backlog
		done:    make(chan struct{}),
		host:    listeningHost,
		proc:    listeningProc,
	}
	for _, k := range scoped {
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
func dstAnyListenerConflict(scope, network string, ip IP, keys []string, port int, wildcard, dual bool) bool {
	if dual {
		return dstListenerConflict(scope, "tcp4", nil, keys[0], port, true) ||
			dstListenerConflict(scope, "tcp6", nil, keys[1], port, true)
	}
	return dstListenerConflict(scope, network, ip, keys[0], port, wildcard)
}

// dstConnectSYN sleeps out one one-way link traversal of a connect control segment
// (the SYN, before the server sees the connection): the base latency plus a jitter
// draw. Control segments are zero-payload, so throttle/bandwidth is exempt — only
// latency + jitter apply. A zero-latency, zero-jitter link (same-host, or a cross-host
// link with no delay configured) returns instantly and draws nothing. It is
// ctx-interruptible: a connect deadline shorter than the traversal fails here, before
// anything is established, exactly as a real connect(2) times out mid-handshake. The
// partition table is checked BEFORE this (the dial's blackhole loop); a cut that begins
// mid-flight is not re-checked, so the connect still completes — the safe direction (the
// sim succeeds where production might drop the SYN), a narrow race not worth the reload.
func dstConnectSYN(ctx context.Context, latencyNs, jitterNs int64) error {
	d := latencyNs + dstFaultRandN(jitterNs)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(d))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return mapErr(ctx.Err())
	}
}

// dstConnectSYNACK sleeps out the second half of the connect round trip (the SYN-ACK
// travelling back to the dialer), so a cross-host dial returns one full RTT after it
// began. It runs AFTER the accept handoff, where establishment commits (the existing
// model treats the handoff as the commit point), so it only delays the dialer's return
// and is not ctx-interruptible — like the buffered wire delays that follow. Same-host
// (zero latency/jitter) returns instantly and draws nothing.
func dstConnectSYNACK(latencyNs, jitterNs int64) {
	if d := latencyNs + dstFaultRandN(jitterNs); d > 0 {
		time.Sleep(time.Duration(d))
	}
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

	dialerHost, dialerProc := dstNetCurrentNode()
	// Partition on connect: a Dial across a cut link either REFUSES (the peer answers
	// RST → ECONNREFUSED, fast) or BLACKHOLES (the SYN is dropped → the dial blocks
	// until the link heals, the context/deadline expires, or the retransmit horizon
	// fires ETIMEDOUT — a real kernel's exhausted SYN retries, so a deadline-less dial
	// into a permanent blackhole fails in bounded virtual time rather than hanging).
	// The mode is selectable per fault (Partition vs PartitionRefuse); the cut is
	// checked in BOTH handshake directions (SYN dialer→target, SYN-ACK target→dialer)
	// so a one-directional partition of either also fails the connect. The target host
	// is the routable IP's owner; a loopback/own-host target is never partitioned.
	targetHost := dialerHost
	if h, ok := dstHostForRoutableIP(ip); ok {
		targetHost = h
	}
	retransNs := dstNetRetransmitTimeoutNs()
	// redial re-enters the blackhole wait and the listener lookup after a
	// refusal point discovers the target host is powered off: a crash landing
	// while this dial was mid-handshake (sleeping in the SYN traversal, or
	// parked in the backlog) must blackhole like any dial to a dead machine —
	// power loss emits no RST — and after a reboot the retransmitted SYN
	// reaches the fresh kernel (connect if a listener is up by then, else a
	// genuine ECONNREFUSED). Each episode restarts the horizon (blockStart) —
	// the same soundness-safe class as the heal-resets-the-cut-window
	// precedent: repeated crash/reboot cycles can only extend the wait, erring
	// toward fewer/later ETIMEDOUTs, never a premature one. (Within ONE
	// episode the loop's blockStart deliberately survives wakes — that
	// conservative anchor is untouched.)
redial:
	blockStart := int64(-1) // base-time the dial first blocked; -1 = not yet blocked
	for {
		wake := dstPartWakeCh()
		cut, refuse := dstDialCut(dialerHost, targetHost)
		if !cut {
			break
		}
		if refuse {
			return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.ECONNREFUSED}
		}
		if blockStart < 0 {
			blockStart = dstBaseNanos()
		}
		var horizonC <-chan time.Time
		var horizonT *time.Timer
		if retransNs > 0 {
			remaining := retransNs - (dstBaseNanos() - blockStart)
			if remaining <= 0 {
				return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.ETIMEDOUT}
			}
			horizonT = time.NewTimer(time.Duration(remaining))
			horizonC = horizonT.C
		}
		select {
		case <-ctx.Done():
			if horizonT != nil {
				horizonT.Stop()
			}
			return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: mapErr(ctx.Err())}
		case <-wake:
			if horizonT != nil {
				horizonT.Stop()
			}
		case <-horizonC:
			return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.ETIMEDOUT}
		}
	}
	scope := dstDialScope(ip, dialerHost)
	// The dialer's source IP is known without the registry lock.
	localIP := IPv4(127, 0, 0, 1)
	if _, routable := dstHostForRoutableIP(ip); routable {
		// Cross-host dial: the source address is the dialer's own routable IP.
		localIP = dstHostRoutableIP(dialerHost)
	} else if dstAddrFamily(network, ip) == "tcp6" {
		localIP = IPv6loopback
	}
	if localTCPAddr != nil && localTCPAddr.IP != nil && !localTCPAddr.IP.IsUnspecified() {
		localIP = localTCPAddr.IP
	}

	dstNet.mu.Lock()
	dstNetRoll()
	l := dstNet.listeners[scope+dstListenerKey(network, ip, portnum, false)]
	if l == nil {
		l = dstNet.listeners[scope+dstListenerKey(network, ip, portnum, true)] // a wildcard listener on this port/family
	}
	localPort := 0
	if localTCPAddr != nil {
		localPort = localTCPAddr.Port
	}
	if localPort != 0 {
		// An explicit LocalAddr binds without SO_REUSEADDR: bind(2) fails EADDRINUSE
		// on a live local addr:port collision (a 2-tuple, whatever the destination) —
		// against another conn's local end OR a listener on the dialer's own host
		// (exact or wildcard, same family; the listener's own SO_REUSEADDR does not
		// help a peer socket that lacks it).
		dialerScope := dstNetScope(dialerHost)
		if dstLocalBindInUse(dialerHost, localIP, localPort) ||
			dstListenerConflict(dialerScope, network, localIP, dstListenerKey(network, localIP, localPort, false), localPort, false) {
			dstNet.mu.Unlock()
			src := &TCPAddr{IP: localIP, Port: localPort}
			if localTCPAddr != nil {
				src.Zone = localTCPAddr.Zone
			}
			return nil, &OpError{Op: "dial", Net: network, Source: src, Addr: serverAddr, Err: syscall.EADDRINUSE}
		}
	} else {
		// Ephemeral: advance the per-run counter, wrapping within the valid port
		// range and skipping any local 2-tuple still live on the dialer's IP —
		// a conn's local end or a listener (exact or wildcard) — so a port
		// never exceeds 65535 (an impossible number the bare counter reached after
		// ~25k dials) and a live local binding is never duplicated.
		localPort = dstAllocEphemeralPort(dialerHost, localIP)
		if localPort == 0 {
			dstNet.mu.Unlock()
			return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.EADDRNOTAVAIL}
		}
	}
	dstNet.mu.Unlock()

	if l == nil {
		if dstHostDead(targetHost) {
			// Powered off between the clear-path check and the lookup — a
			// window no cooperative schedule reaches today (no yield between
			// them); kept structural so a future preemption point cannot turn
			// it into a refusal.
			goto redial
		}
		return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.ECONNREFUSED}
	}
	localAddr := &TCPAddr{IP: localIP, Port: localPort}
	if localTCPAddr != nil {
		localAddr.Zone = localTCPAddr.Zone
	}

	// EVERY connection is wire-backed (a buffered byte-stream transport). Cross-host
	// conns carry the configured latency/jitter/bandwidth and are partitionable;
	// same-host and loopback conns use a zero-latency wire (never partitioned, since
	// dstPartitioned is false for a==b). One transport shape everywhere is required
	// for soundness: net.Pipe rendezvouses (a write blocks until the peer reads), so
	// two co-located processes that each write before reading would deadlock in
	// simulation where real TCP — whose send buffer is never zero — completes both
	// instantly, a false positive the Soundness invariant forbids. The wire's
	// buffered write is the faithful TCP shape (a send buffer the link drains).
	// The send buffer is BOUNDED everywhere, same-host included: loopback TCP
	// has finite socket buffers too, so two co-located peers that each write
	// past them before reading deadlock in production — and must deadlock
	// (loudly, as a bubble deadlock) in simulation rather than succeed into an
	// unbounded sim-only buffer that masks the bug. The retransmit horizon
	// value is irrelevant on a same-host wire (never partitioned, so it can
	// never arm) and is passed uniformly.
	latency, jitter, bandwidth := int64(0), int64(0), int64(0)
	capacity, retrans := dstNetSendBufferBytes(), dstNetRetransmitTimeoutNs()
	if l.host != dialerHost {
		latency, jitter, bandwidth = dstNetCrossHostLatencyNs(), dstNetCrossHostJitterNs(), dstNetCrossHostBandwidthBps()
	}
	// SYN: the first half of the connect round trip travels to the server. A connect
	// deadline shorter than this traversal fails now, before anything is established —
	// a zero-RTT connect would instead let a SUT's connect timeout pass under
	// simulation on a link where it fails in production (unsound).
	if err := dstConnectSYN(ctx, latency, jitter); err != nil {
		return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: err}
	}
	p1, p2 := dstWirePair(latency, jitter, bandwidth, capacity, retrans, dialerHost, l.host)
	reset := new(atomic.Bool)
	dialer := &dstConn{Conn: p1, network: network, local: localAddr, remote: serverAddr, reset: reset, localHost: dialerHost, remoteHost: l.host, localProc: dialerProc, remoteProc: l.proc}
	server := &dstConn{Conn: p2, network: network, local: serverAddr, remote: localAddr, reset: reset, acceptState: new(atomic.Int32), localHost: l.host, remoteHost: dialerHost, localProc: l.proc, remoteProc: dialerProc}
	// A FULL accept backlog drops the SYN (tcp_abort_on_overflow=0, the
	// default): the dialer retransmits and either a slot frees in time (the
	// send below lands) or its retries exhaust — connect fails ETIMEDOUT at
	// the retransmit horizon. Without the horizon a deadline-less dial into a
	// saturated listener hung forever, a sim-only permanent hang. This arms
	// for same-host dials too: a loopback connect into a full queue times out
	// in production just the same (the queue, not the link, is exhausted).
	var backlogHorizonC <-chan time.Time
	var backlogHorizonT *time.Timer
	if retransNs > 0 {
		backlogHorizonT = time.NewTimer(time.Duration(retransNs))
		backlogHorizonC = backlogHorizonT.C
	}
	stopBacklogHorizon := func() {
		if backlogHorizonT != nil {
			backlogHorizonT.Stop()
		}
	}
	select {
	case <-ctx.Done():
		stopBacklogHorizon()
		p1.Close()
		p2.Close()
		return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: mapErr(ctx.Err())}
	case <-backlogHorizonC:
		p1.Close()
		p2.Close()
		return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ETIMEDOUT}
	case l.accept <- server:
		stopBacklogHorizon()
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
				goto refused
			}
			// Register both ends once the conn is live (the dial is about to return
			// it). Registration intentionally trails the l.accept handoff: a Reset
			// racing a conn still in establishment benignly misses it — under-firing
			// a reset is sound (a real RST can miss a conn mid-handshake too), whereas
			// the failure paths above never register, so a reset never over-fires.
			dstConnRegister(dialer) // accepted before teardown: a live conn
			dstConnRegister(server)
		default:
			dstConnRegister(dialer) // queued: a live conn
			dstConnRegister(server)
		}
		// SYN-ACK: the acknowledgment travels back; the connect completes one full RTT
		// after it began. Committed at the handoff above, so this only delays the
		// dialer's return (not ctx-interruptible), like the wire delays that follow.
		dstConnectSYNACK(latency, jitter)
		return dialer, nil
	case <-l.done:
		stopBacklogHorizon()
		p1.Close()
		p2.Close()
		goto refused
	}
refused:
	// The listener closed under this in-flight dial. One decision point for
	// both handoff arms (backlog claim and pre-handoff close): a live kernel's
	// closed listener answers RST — ECONNREFUSED — but a listener that died
	// with its MACHINE emits nothing; the dial re-enters the blackhole wait
	// (redial) and times out or reaches the rebooted kernel.
	if dstHostDead(l.host) {
		goto redial
	}
	return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.ECONNREFUSED}
}
