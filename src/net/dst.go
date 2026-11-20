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
	isync "internal/sync"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe" // go:linkname + coarse-dependency identities
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
// gated on dstInSimBubble(); typed and packet public entry points fail fast
// instead of falling through to host sockets until those surfaces are modeled.
// dstNetEnabled is a constant false in a non -tags dst build, so this all
// compiles out there.
//
// This is the reliable, in-order base; network faults (partition/drop/reorder/
// latency) layer on top of the same registry+conns later.

//go:linkname dstActive runtime.dstActive
func dstActive() bool

//go:linkname dstInSimBubble runtime.dstInSimBubble
func dstInSimBubble() bool

//go:linkname dstNetEpoch runtime.dstNetEpoch
func dstNetEpoch() uint64

// Page-0 synthetic coarse dependency keys allocated to net (disjoint from os;
// see os/dst_fs.go): [1025, 2048] per-host network/partition/port state,
// [3585, 4095] virtual socket-option descriptors.
const (
	dstCoarseNetHostBase = 1025
	dstCoarseNetHostSpan = 1024
	dstCoarseSockFDBase  = 3585
	dstCoarseSockFDSpan  = 511
)

func dstCoarseNetHostID(host uint32) uintptr {
	return dstCoarseNetHostBase + uintptr(host)%dstCoarseNetHostSpan
}

func dstCoarseSockFDID(fd int) uintptr {
	return dstCoarseSockFDBase + uintptr(uint(fd))%dstCoarseSockFDSpan
}

// dstNet is the process-global simulated network: a per-run registry of listeners
// keyed by address. Keyed by the run epoch (dstNetEpoch) so a new run starts with
// an empty registry, with no explicit teardown hook.
var dstNet struct {
	mu              isync.KernelMutex
	epoch           uint64
	listeners       map[string]*dstListener
	closedListeners map[string]*uint64    // scoped listener key → last-close sync shadow
	nextPort        map[uint32]int        // host → deterministic ephemeral local port for dialers
	nextListenPort  map[uint32]int        // host → deterministic ephemeral port for listeners bound to :0
	pendingBinds    map[dstBindKey]uint32 // bind → owning process
}

type dstBindKey struct {
	host     uint32
	family   string
	ip       string
	port     int
	wildcard bool
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
	family := dstTCPAddrFamily(localIP)
	const span = dstDialEphemeralEnd - dstDialEphemeralStart + 1
	for tried := 0; tried < span; tried++ {
		p := dstNet.nextPort[host]
		if p == 0 {
			p = dstDialEphemeralStart
		}
		dstNet.nextPort[host] = p + 1
		if dstNet.nextPort[host] > dstDialEphemeralEnd {
			dstNet.nextPort[host] = dstDialEphemeralStart
		}
		if dstLocalBindInUse(host, localIP, p) {
			continue
		}
		if dstPendingBindInUse(dstBindKey{host: host, family: family, ip: localIP.String(), port: p}) {
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
		dstNet.closedListeners = make(map[string]*uint64)
		dstNet.nextPort = make(map[uint32]int)
		dstNet.nextListenPort = make(map[uint32]int)
		dstNet.pendingBinds = make(map[dstBindKey]uint32)
	}
}

//go:linkname dstNetCurrentNode runtime.dstCurrentSlot
func dstNetCurrentNode() (host, proc uint32)

//go:linkname dstRecordCausalEvent runtime.dstRecordCausalEvent
func dstRecordCausalEvent(kind, flags uint8, hostA, hostB, proc uint32, pid int32, arg0, arg1, arg2 int64, str string)

//go:linkname dstRecordCausalCaller runtime.dstRecordCausalCaller
func dstRecordCausalCaller(kind, flags uint8, hostB uint32, arg0, arg1, arg2 int64, str string)

//go:linkname dstCurrentDiagIdentity runtime.dstCurrentDiagIdentity
func dstCurrentDiagIdentity() (host, proc uint32, pid int32)

// Causal event kind codes — must match runtime's dstCausalNet* constants.
const (
	dstCausalNetListen            uint8 = 40
	dstCausalNetListenerClose     uint8 = 41
	dstCausalNetDialEstablished   uint8 = 42
	dstCausalNetDialRefused       uint8 = 43
	dstCausalNetDialTimeout       uint8 = 44
	dstCausalNetRetransmitTimeout uint8 = 45
	dstCausalNetKeepaliveTimeout  uint8 = 46
	dstCausalNetRSTDelivered      uint8 = 47
)

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

func dstZoneName(index int) string {
	switch index {
	case 1:
		return "lo"
	case 2:
		return "eth0"
	default:
		return strconv.Itoa(index)
	}
}

func dstZoneIndex(name string) int {
	switch name {
	case "lo":
		return 1
	case "eth0":
		return 2
	default:
		index, _, _ := dtoi(name)
		return index
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

func dstUDPNetwork(network string) bool {
	switch network {
	case "udp", "udp4", "udp6":
		return true
	}
	return false
}

// dstUnsupportedNetwork rejects a network Dial/Listen/ListenPacket cannot model.
// A KNOWN but unmodeled network (Unix, IP) gets the same "unsupported under
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
	case "unix", "unixgram", "unixpacket", "ip", "ip4", "ip6":
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

func dstLocalIPOwnedByHost(host uint32, ip IP) bool {
	if ip.IsLoopback() {
		return true
	}
	owner, ok := dstHostForRoutableIP(ip)
	return ok && owner == host
}

func dstExplicitBindKey(host uint32, network string, ip IP, port int, wildcard bool) dstBindKey {
	key := dstBindKey{host: host, family: dstAddrFamily(network, ip), port: port, wildcard: wildcard}
	if !wildcard {
		key.ip = ip.String()
	}
	return key
}

func dstPendingBindInUse(key dstBindKey) bool {
	for bind := range dstNet.pendingBinds {
		if bind.host == key.host && bind.family == key.family && bind.port == key.port &&
			(bind.wildcard || key.wildcard || bind.ip == key.ip) {
			return true
		}
	}
	return false
}

// dstDialOptionsError rejects the dial options the model does not provide.
// Control/ControlContext and the keepalive configuration are MODELED (the
// socket-option layer, dst_sockopt.go, and the wire's keepalive prober,
// dst_wire.go); only an explicit MPTCP enable remains refused — multipath
// requires path semantics the single-link wire does not have.
func dstDialOptionsError(d *Dialer, network string, source, addr Addr) error {
	if d.mptcpStatus == mptcpEnabledDial {
		return dstUnsupportedNetOption("dial", network, source, addr, "Dialer.MultipathTCP")
	}
	return nil
}

func dstListenOptionsError(lc *ListenConfig, network string, addr Addr) error {
	if lc.mptcpStatus == mptcpEnabledListen {
		return dstUnsupportedNetOption("listen", network, nil, addr, "ListenConfig.MultipathTCP")
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

func dstIsLocalhost(host string) bool {
	if len(host) > 0 && host[len(host)-1] == '.' {
		host = host[:len(host)-1]
	}
	if len(host) != len("localhost") {
		return false
	}
	for i := 0; i < len("localhost"); i++ {
		c := host[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != "localhost"[i] {
			return false
		}
	}
	return true
}

func dstLookupLocalhostIPAddr(network, host string) ([]IPAddr, bool) {
	if !dstIsLocalhost(host) {
		return nil, false
	}
	if len(network) > 0 && network[len(network)-1] == '4' {
		return []IPAddr{{IP: IP{127, 0, 0, 1}}}, true
	}
	if len(network) > 0 && network[len(network)-1] == '6' {
		return []IPAddr{{IP: append(IP(nil), IPv6loopback...)}}, true
	}
	return []IPAddr{
		{IP: IP{127, 0, 0, 1}},
		{IP: append(IP(nil), IPv6loopback...)},
	}, true
}

func dstLookupProtocol(name string) (int, error) {
	var lowerProtocol [maxProtoLength]byte
	n := copy(lowerProtocol[:], name)
	lowerASCIIBytes(lowerProtocol[:n])
	if n != len(name) {
		return 0, &AddrError{Err: "unknown IP protocol specified", Addr: name}
	}
	switch string(lowerProtocol[:n]) {
	case "icmp":
		return 1, nil
	case "igmp":
		return 2, nil
	case "tcp":
		return 6, nil
	case "udp":
		return 17, nil
	case "ipv6-icmp":
		return 58, nil
	}
	return 0, &AddrError{Err: "unknown IP protocol specified", Addr: name}
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
	}
	if dstIsLocalhost(host) {
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
func dstAllocateListenPort(scope, network string, ip IP, host uint32, wildcard, dual, reuseAddr bool) (port int, keys []string, err error) {
	const span = 65535 - dstListenEphemeralStart + 1
	for tried := 0; tried < span; tried++ {
		p := dstNet.nextListenPort[host]
		if p == 0 {
			p = dstListenEphemeralStart
		}
		dstNet.nextListenPort[host] = p + 1
		if dstNet.nextListenPort[host] > 65535 {
			dstNet.nextListenPort[host] = dstListenEphemeralStart
		}
		if dstListenConnConflict(host, network, ip, p, wildcard, dual, reuseAddr) {
			continue // a conflicting conn or TIME_WAIT hold occupies the port
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

// dstListenConnConflict reports whether a live conn (or TIME_WAIT hold when
// !reuseAddr) on host blocks a new listener at (ip, port). Under default
// SO_REUSEADDR=1, accepted server ends (and explicit SO_REUSEADDR dialers)
// carry SO_REUSEADDR and do not block re-binding, while non-SO_REUSEADDR
// dialers block; when a Control callback clears SO_REUSEADDR=0, any matching
// live conn or TIME_WAIT hold blocks. Caller holds dstNet.mu.
func dstListenConnConflict(host uint32, network string, ip IP, port int, wildcard, dual, reuseAddr bool) bool {
	family := dstAddrFamily(network, ip)
	for bind := range dstNet.pendingBinds {
		if bind.host != host || bind.port != port {
			continue
		}
		if dual || bind.family == family && (wildcard || bind.wildcard || bind.ip == ip.String()) {
			return true
		}
	}
	switch {
	case dual:
		return dstConnBindInUse(host, nil, port, "", reuseAddr) ||
			(!reuseAddr && dstTimeWaitBindHeld(host, nil, port, ""))
	case wildcard:
		return dstConnBindInUse(host, nil, port, family, reuseAddr) ||
			(!reuseAddr && dstTimeWaitBindHeld(host, nil, port, family))
	default:
		return dstConnBindInUse(host, ip, port, family, reuseAddr) ||
			(!reuseAddr && dstTimeWaitBindHeld(host, ip, port, family))
	}
}

// dstConn is a simulated connection: a net.Pipe endpoint (Read/Write/Close/
// deadlines on the bubble's fake clock) wrapped with the connection's real
// local/remote addresses and production-shaped error identity. The raw pipe
// errors (io.ErrClosedPipe, bare os.ErrDeadlineExceeded, OpError{Net:"pipe"})
// would break production-shaped code: after a local Close every op must
// satisfy errors.Is(err, net.ErrClosed); a connection reset (listener backlog
// teardown) and the first write to a closed peer carry syscall.ECONNRESET
// once — the kernel's one-shot sk_err — and later ops the CLOSED-socket
// identities (reads io.EOF, writes EPIPE; consumeSkErr); reads from a
// gracefully closed peer return io.EOF; deadline errors are *OpError
// wrapping os.ErrDeadlineExceeded with the connection's network and addresses.
type dstConn struct {
	Conn
	epoch         uint64
	network       string
	local, remote Addr
	closed        atomic.Bool  // this end was Closed by its user
	reset         *atomic.Bool // connection reset (shared by both ends)
	resetSync     *uint64      // happens-before word paired with reset (shared by both ends)

	// skErrConsumed is this end's one-shot pending-socket-error latch —
	// production's sk_err, which the kernel reports ONCE: the first op that
	// surfaces this end's teardown (a reset, or the write refusal after the
	// peer's close) consumes the identity; later reads return io.EOF and
	// later writes EPIPE, the CLOSED-socket identities (host-probed ladder;
	// see consumeSkErr). Per end, as sk_err is per socket.
	skErrConsumed atomic.Bool

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
	bindWildcard          bool
	bindFamily            string

	// regSeq is the per-run registration sequence, stamped at dstConnRegister, so a
	// multi-victim reset orders its victims deterministically (registration order =
	// Dial order, schedule-determined) rather than by pointer-map iteration order.
	regSeq uint64

	// sockFD is this end's virtual socket descriptor (dst_sockopt.go): the
	// number SyscallConn hands out, retiring with the connection (Close or a
	// reset teardown). The dialer end keeps the dialing socket's descriptor
	// — production's fd continuity from socket() through Control to the
	// established conn.
	sockFD   int
	sockOpts *dstSockOpts

	// acceptState tracks a server-end connection through the accept backlog:
	// 0 queued, 1 accepted, 2 reset/refused. The listener's Accept claims
	// 0→1; the LISTENER-CLOSE backlog teardown and the dialer's post-send
	// listener-closed check claim 0→2 — so a connection the server already
	// Accepted can never be retroactively reset by that teardown, and a conn
	// the listener's close reset is never handed out by Accept (production
	// cannot accept from a closed listener). A FAULT-injected RST
	// deliberately leaves a queued conn at 0: the kernel does not unlink an
	// RST-aborted established child from the accept queue — accept(2) hands
	// it out and its reads drain delivered bytes, then fail ECONNRESET
	// (host-probed; see dstInjectReset). Nil on the dialer end.
	acceptState       *atomic.Int32
	fallbackCloseSync uint64
}

func (c *dstConn) closeSyncPtr() *uint64 {
	if e, ok := c.Conn.(*dstWireEnd); ok {
		return &e.closeSync
	}
	return &c.fallbackCloseSync
}

func (c *dstConn) isClosedAcquire() bool {
	dstKernelSectionEnter()
	closed := c.closed.Load()
	if closed {
		dstKernelRaceAcquire(unsafe.Pointer(c.closeSyncPtr()))
	}
	dstKernelSectionExit()
	return closed
}

func (c *dstConn) markClosed() (already bool) {
	cs := unsafe.Pointer(c.closeSyncPtr())
	dstKernelSectionEnter()
	if c.closed.Load() {
		dstKernelRaceAcquire(cs)
		dstKernelSectionExit()
		return true
	}
	dstKernelRaceReleaseMerge(cs)
	already = c.closed.Swap(true)
	if already {
		dstKernelRaceAcquire(cs)
	}
	dstKernelSectionExit()
	return already
}

func dstLoadResetAcquire(reset *atomic.Bool, resetSync *uint64) bool {
	if reset == nil {
		return false
	}
	dstKernelSectionEnter()
	r := reset.Load()
	if r && resetSync != nil {
		dstKernelRaceAcquire(unsafe.Pointer(resetSync))
	}
	dstKernelSectionExit()
	return r
}

func (c *dstConn) markReset() {
	dstKernelSectionEnter()
	if c.reset != nil {
		if c.resetSync != nil {
			dstKernelRaceReleaseMerge(unsafe.Pointer(c.resetSync))
		}
		c.reset.Store(true)
	}
	dstKernelSectionExit()
}

func (c *dstConn) isResetKernel() bool {
	if c.reset == nil {
		return false
	}
	dstKernelSectionEnter()
	r := c.reset.Load()
	dstKernelSectionExit()
	return r
}

func (c *dstConn) isResetAcquire() bool {
	if !dstLoadResetAcquire(c.reset, c.resetSync) {
		return false
	}
	if e, ok := c.Conn.(*dstWireEnd); ok {
		e.in.acquireFreezeSync()
		e.out.acquireFreezeSync()
	}
	return true
}

func (c *dstConn) claimAcceptState(old, new int32) bool {
	if c.acceptState == nil {
		return false
	}
	dstKernelSectionEnter()
	ok := c.acceptState.CompareAndSwap(old, new)
	dstKernelSectionExit()
	return ok
}

type dstOwnedConn struct {
	conn *dstConn
}

func dstOwnConnPair(dialer, server *dstConn) *dstOwnedConn {
	dstConnRegisterPair(dialer, server)
	return &dstOwnedConn{conn: server}
}

func dstCloneAddr(addr Addr) Addr {
	if tcp, ok := addr.(*TCPAddr); ok {
		clone := *tcp
		clone.IP = append(IP(nil), tcp.IP...)
		return &clone
	}
	return addr
}

func (c *dstConn) LocalAddr() Addr  { return dstCloneAddr(c.local) }
func (c *dstConn) RemoteAddr() Addr { return dstCloneAddr(c.remote) }

func (c *dstConn) stale() bool { return !dstInSimBubble() || c.epoch != dstNetEpoch() }

func (c *dstConn) opError(op string, err error) error {
	return &OpError{Op: op, Net: c.network, Source: dstCloneAddr(c.local), Addr: dstCloneAddr(c.remote), Err: err}
}

// resetConn tears the connection down as a reset: the peer's FIRST failing
// read or write reports ECONNRESET (production's RST pending as sk_err), not
// EOF/closed-pipe; later ops carry the CLOSED-socket identities (consumeSkErr).
func (c *dstConn) resetConn() {
	c.markReset()
	if e, ok := c.Conn.(*dstWireEnd); ok {
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.closeSync)), true)
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.in.raceSync)), true)
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.out.raceSync)), true)
		dstCoarseRecord(uintptr(unsafe.Pointer(&e.out.eofSync)), true)
		e.closeAfterCoarse()
	} else {
		c.Conn.Close()
	}
	dstCoarseRecord(dstCoarseNetHostID(c.localHost), true)
	dstConnDeregister(c)
	dstSockFDFree(c.sockFD, c.sockOpts)
}

// consumeSkErr shapes the error for an op that found this end's connection
// torn down — a reset, or a write refused after the peer's close — following
// the kernel's one-shot sk_err semantics (host-probed ladder): the FIRST such
// op consumes the pending error and reports ECONNRESET; every later op gets
// the CLOSED-socket identity — io.EOF for reads, EPIPE for writes. A reset
// that arrived in CLOSE_WAIT (the peer's FIN already delivered) pends EPIPE,
// not ECONNRESET (tcp_reset's CLOSE_WAIT arm), and an EPIPE consumption is
// indistinguishable from the post-consumption identities, so that arm reads
// EOF and writes EPIPE throughout — no ECONNRESET is ever reported.
func (c *dstConn) consumeSkErr(op string) error {
	closeWait := false
	if e, ok := c.Conn.(*dstWireEnd); ok {
		closeWait = e.isRSTCloseWaitAcquire()
	}
	if closeWait {
		return c.consumePendingErr(op, nil)
	}
	return c.consumePendingErr(op, syscall.ECONNRESET)
}

// consumePendingErr is the one-shot sk_err latch shared by every pending
// socket error the kernel reports once — ECONNRESET (an RST) and ETIMEDOUT
// (retransmission exhaustion, tcp_write_err's sk_err; host-probed: the first
// failing op after a TCP_USER_TIMEOUT death reports ETIMEDOUT, later reads
// return EOF and later writes EPIPE, identically to the ECONNRESET ladder). A
// nil pending error means the identity was already spent or never pended (the
// CLOSE_WAIT arm): the op reports the CLOSED-socket identity directly.
func (c *dstConn) consumePendingErr(op string, pending error) error {
	if pending != nil {
		dstKernelSectionEnter()
		already := c.skErrConsumed.Swap(true)
		if !already && pending == syscall.ECONNRESET {
			if e, ok := c.Conn.(*dstWireEnd); ok {
				e.recordRSTDelivered()
			}
		}
		dstKernelSectionExit()
		if !already {
			return c.opError(op, pending)
		}
	}
	if op == "read" {
		return io.EOF
	}
	return c.opError(op, syscall.EPIPE)
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
		// The retransmit horizon: a write/read into a permanently undeliverable
		// conn (a cut outlasting the horizon). Production pends ETIMEDOUT as the
		// kernel's ONE-SHOT sk_err (tcp_write_err; host-probed, the same
		// mechanics as the RST's ECONNRESET): the first failing op consumes it,
		// later reads return io.EOF and later writes EPIPE — the CLOSED-socket
		// identities.
		return c.consumePendingErr(op, syscall.ETIMEDOUT)
	case c.isClosedAcquire():
		return c.opError(op, errClosed)
	case err == syscall.EPIPE:
		return c.opError(op, syscall.EPIPE)
	case err == io.EOF:
		return io.EOF // graceful peer close (a reset read is mapped in Read)
	case err == io.ErrClosedPipe:
		// Peer closed or connection reset while we operate: production
		// surfaces the pending socket error ONCE, then the CLOSED-socket
		// identities (consumeSkErr). This covers ops on a reset end itself
		// (a reset closes the underlying pipe, so its own ops land here) and
		// the write refused because the peer's FIN closed the stream — the
		// sim's instant first-write ECONNRESET there is the recorded
		// divergence (design.md: the succeed-then-RST round trip is the
		// FIN/RST follow-on's work); its follow-up writes carry the kernel's
		// post-reset EPIPE.
		return c.consumeSkErr(op)
	}
	// Unreachable today: the pipe wraps only deadline errors (caught above) in
	// its own OpError. Kept as a conservative wrap so a future pipe error
	// cannot leak pipe-shaped identity.
	return c.opError(op, err)
}

func (c *dstConn) Read(b []byte) (int, error) {
	if c.stale() {
		return 0, c.opError("read", errClosed)
	}
	e, isWire := c.Conn.(*dstWireEnd)
	if isWire {
		e.coarseRead()
	} else {
		dstCoarseDep(uintptr(unsafe.Pointer(c.closeSyncPtr())), false)
	}
	if c.isClosedAcquire() {
		return 0, c.opError("read", errClosed)
	}
	var (
		n   int
		err error
	)
	if isWire {
		n, err = e.readAfterCoarse(b)
	} else {
		n, err = c.Conn.Read(b)
	}
	if err == io.EOF && c.isResetAcquire() {
		// The transport drained to EOF on a reset connection: the RST is the
		// terminal event, surfaced with the one-shot sk_err semantics — the
		// first post-drain read reports ECONNRESET, later reads io.EOF.
		return n, c.consumeSkErr("read")
	}
	return n, c.mapConnErr("read", err)
}

func (c *dstConn) Write(b []byte) (int, error) {
	if c.stale() {
		return 0, c.opError("write", errClosed)
	}
	e, isWire := c.Conn.(*dstWireEnd)
	if isWire {
		e.coarseWrite()
	} else {
		dstCoarseDep(uintptr(unsafe.Pointer(c.closeSyncPtr())), false)
	}
	if c.isClosedAcquire() {
		return 0, c.opError("write", errClosed)
	}
	var (
		n   int
		err error
	)
	if isWire {
		n, err = e.writeAfterCoarse(b)
	} else {
		n, err = c.Conn.Write(b)
	}
	return n, c.mapConnErr("write", err)
}

func (c *dstConn) CloseWrite() error {
	if c.stale() {
		return c.opError("close", errClosed)
	}
	e, isWire := c.Conn.(*dstWireEnd)
	if isWire {
		e.coarseCloseWrite()
	} else {
		dstCoarseDep(uintptr(unsafe.Pointer(c.closeSyncPtr())), true)
	}
	if c.isClosedAcquire() {
		return c.opError("close", errClosed)
	}
	if isWire {
		if c.isResetAcquire() && e.streamFinArrived(e.peerHost, e.localHost, e.in) {
			return c.opError("close", os.NewSyscallError("shutdown", syscall.ENOTCONN))
		}
		if err := e.closeWriteAfterCoarse(); err != nil {
			if err == syscall.ENOTCONN {
				return c.opError("close", os.NewSyscallError("shutdown", syscall.ENOTCONN))
			}
			return c.opError("close", errClosed)
		}
		return nil
	}
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *dstConn) CloseRead() error {
	if c.stale() {
		return c.opError("close", errClosed)
	}
	e, isWire := c.Conn.(*dstWireEnd)
	if isWire {
		e.coarseCloseRead()
	} else {
		dstCoarseDep(uintptr(unsafe.Pointer(c.closeSyncPtr())), true)
	}
	if c.isClosedAcquire() {
		return c.opError("close", errClosed)
	}
	if isWire {
		if c.isResetAcquire() && e.streamFinArrived(e.peerHost, e.localHost, e.in) {
			return c.opError("close", os.NewSyscallError("shutdown", syscall.ENOTCONN))
		}
		if err := e.closeReadAfterCoarse(); err != nil {
			if err == syscall.ENOTCONN {
				return c.opError("close", os.NewSyscallError("shutdown", syscall.ENOTCONN))
			}
			return c.opError("close", errClosed)
		}
		return nil
	}
	if cr, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return cr.CloseRead()
	}
	return nil
}

func (c *dstConn) Close() error {
	if !dstInSimBubble() && dstActive() && c.epoch == dstNetEpoch() {
		return c.opError("close", errClosed)
	}
	if c.stale() {
		if c.closed.Swap(true) {
			return c.opError("close", errClosed)
		}
		return nil
	}
	e, isWire := c.Conn.(*dstWireEnd)
	if isWire {
		e.coarseClose()
	} else {
		dstCoarseDep(uintptr(unsafe.Pointer(c.closeSyncPtr())), true)
	}
	dstCoarseRecord(dstCoarseNetHostID(c.localHost), true)
	if c.sockFD != 0 {
		dstCoarseRecord(dstCoarseSockFDID(c.sockFD), true)
	}
	if c.markClosed() {
		return c.opError("close", errClosed)
	}
	// The kernel's close(2) conditional: an end whose receive queue holds
	// unread data answers the peer with RST — the peer drains bytes already
	// delivered (and any this end wrote before closing, which travel ahead
	// of the RST on the in-order link), then fails ECONNRESET — otherwise
	// the close FINs and the peer drains buffered bytes to io.EOF. Bytes
	// still in flight TOWARD this end count as queued for the RST decision
	// (the recorded collapse: the sim RSTs immediately, one of the two
	// orderings the real close-vs-arrival race produces). The type assertion
	// is the same wire-backed transport contract dstCloseProcConns records.
	if isWire && e.unreadInbound() {
		if dstPartitionedDir(c.localHost, c.remoteHost) {
			dstCrashVictimConn(c)
		} else {
			dstResetBothEnds(c)
		}
		return nil
	}
	c.timeWaitHold()
	if isWire {
		e.closeAfterCoarse()
	} else {
		c.Conn.Close()
	}
	dstConnDeregister(c)
	dstSockFDFree(c.sockFD, c.sockOpts)
	return nil
}

// timeWaitHold records this end's local 2-tuple as a TIME_WAIT hold when
// production would: the FIN arm of an ACTIVE close — the ends production
// sends to CLOSED directly hold nothing (the RST arm above, an already-reset
// conn, a retransmit-exhaustion death), and neither does the PASSIVE closer
// (the peer closed first). The peer's close INSTANT, not its FIN's delivery,
// decides active vs passive — the same collapse the RST arm records for the
// close-vs-arrival race. The first closer therefore always holds, and two
// Closes that interleave before either transport closes BOTH hold —
// production's simultaneous-close shape (each end sent its FIN before
// receiving the peer's, so both enter TIME_WAIT). Both dialer and accepted
// ends hold, as in production; the hold is visible only to the explicit-
// LocalAddr bind probe (dstTimeWaitHeld), so accepts, re-listens, and the
// ephemeral allocator never see it. Runs BEFORE c.Conn.Close so remoteDone
// still reflects only the peer's action.
func (c *dstConn) timeWaitHold() {
	if c.isResetKernel() {
		return
	}
	la, ok := c.local.(*TCPAddr)
	if !ok {
		return
	}
	e, ok := c.Conn.(*dstWireEnd)
	if !ok || e.isTimedOutKernel() || !e.stampActiveCloser() {
		return
	}
	dstTimeWaitAdd(c.localHost, la.IP, la.Port, c.bindFamily, c.bindWildcard)
}

// setDeadline applies a deadline with production error identity: after a
// local Close it fails with net.ErrClosed; after a peer close it still succeeds
// and applies: FIN may be delayed or partition-held,
// so the local endpoint remains live until terminal state arrives.
func (c *dstConn) setDeadline(coarse func(*dstWireEnd), setWire func(*dstWireEnd, time.Time) error, setFallback func(time.Time) error, t time.Time) error {
	// Production shape for a set-deadline failure: Source nil, Addr local.
	closedErr := &OpError{Op: "set", Net: c.network, Source: nil, Addr: dstCloneAddr(c.local), Err: errClosed}
	if c.stale() {
		return closedErr
	}
	e, isWire := c.Conn.(*dstWireEnd)
	if isWire {
		coarse(e)
	} else {
		dstCoarseDep(uintptr(unsafe.Pointer(c.closeSyncPtr())), false)
	}
	if c.isClosedAcquire() {
		return closedErr
	}
	var err error
	if isWire {
		err = setWire(e, t)
	} else {
		err = setFallback(t)
	}
	if err != nil && !c.isClosedAcquire() {
		// The pipe rejects deadlines once either side is done; only a LOCAL
		// close is an error in production.
		return nil
	} else if err != nil {
		return closedErr
	}
	return nil
}

func (c *dstConn) SetDeadline(t time.Time) error {
	return c.setDeadline((*dstWireEnd).coarseSetDeadline, (*dstWireEnd).setDeadlineAfterCoarse, c.Conn.SetDeadline, t)
}

func (c *dstConn) SetReadDeadline(t time.Time) error {
	return c.setDeadline((*dstWireEnd).coarseSetReadDeadline, (*dstWireEnd).setReadDeadlineAfterCoarse, c.Conn.SetReadDeadline, t)
}

func (c *dstConn) SetWriteDeadline(t time.Time) error {
	return c.setDeadline((*dstWireEnd).coarseSetWriteDeadline, (*dstWireEnd).setWriteDeadlineAfterCoarse, c.Conn.SetWriteDeadline, t)
}

// dstListener is a simulated Listener. Dial pushes the server end of a new
// connection onto accept; Accept receives it. A dual-stack wildcard listener
// (plain "tcp" on a wildcard host) registers under both family keys.
type dstListener struct {
	network    string
	epoch      uint64
	addr       *TCPAddr
	keys       []string
	backlogSem chan struct{}
	accept     chan *dstOwnedConn
	done       chan struct{}
	closed     atomic.Bool
	closeSync  uint64
	host       uint32 // the host that owns this listener (its network identity)
	proc       uint32 // the process slot that created this listener (owns its accepted conns)

	// sockFD/opts are the listening socket's virtual descriptor and option
	// state (a ListenConfig.Control callback's writes land here); accepted
	// connections inherit a clone of opts (accept(2) inheritance) and then
	// take the Go-level keepalive resolution kaIdle/kaCfg (production's
	// newTCPConn at accept).
	sockFD int
	opts   *dstSockOpts
	kaIdle time.Duration
	kaCfg  KeepAliveConfig
}

func (l *dstListener) opError(op string, err error) error {
	return &OpError{Op: op, Net: l.network, Source: nil, Addr: dstCloneAddr(l.addr), Err: err}
}

func (l *dstListener) stale() bool { return !dstInSimBubble() || l.epoch != dstNetEpoch() }

func (l *dstListener) acquireCloseSync() {
	dstKernelSectionEnter()
	dstKernelRaceAcquire(unsafe.Pointer(&l.closeSync))
	dstKernelSectionExit()
}

func (l *dstListener) isClosedAcquire() bool {
	dstKernelSectionEnter()
	closed := l.closed.Load()
	if closed {
		dstKernelRaceAcquire(unsafe.Pointer(&l.closeSync))
	}
	dstKernelSectionExit()
	return closed
}

func (l *dstListener) markClosed() (already bool) {
	cs := unsafe.Pointer(&l.closeSync)
	dstKernelSectionEnter()
	if l.closed.Load() {
		dstKernelRaceAcquire(cs)
		dstKernelSectionExit()
		return true
	}
	dstKernelRaceReleaseMerge(cs)
	already = l.closed.Swap(true)
	if already {
		dstKernelRaceAcquire(cs)
	}
	dstKernelSectionExit()
	return already
}

func (l *dstListener) Accept() (Conn, error) {
	if l.stale() {
		return nil, l.opError("accept", errClosed)
	}
	dstCoarseDep(uintptr(unsafe.Pointer(l)), true)
	if l.isClosedAcquire() {
		return nil, l.opError("accept", errClosed)
	}
	// Closed-first: production Accept after Close always fails with ErrClosed,
	// even if connections were still queued in the backlog (Close reset them).
	select {
	case <-l.done:
		l.acquireCloseSync()
		return nil, l.opError("accept", errClosed)
	default:
	}
	for {
		select {
		case owned := <-l.accept:
			<-l.backlogSem
			c := owned.conn
			if c.acceptState != nil && !c.claimAcceptState(0, 1) {
				// Listener-close reset (or dial-side refusal) while queued;
				// never hand it out. A FAULT-injected RST leaves the state 0
				// on purpose — that conn IS handed out (see acceptState).
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
				l.acquireCloseSync()
				if c.acceptState != nil {
					dstKernelSectionEnter()
					c.acceptState.Store(2)
					dstKernelSectionExit()
				}
				if dstHostDead(c.localHost) || dstPartitionedDir(c.localHost, c.remoteHost) {
					dstCrashVictimConn(c)
				} else {
					dstResetBacklogConn(c)
				}
				return nil, l.opError("accept", errClosed)
			default:
			}
			return c, nil
		case <-l.done:
			l.acquireCloseSync()
			return nil, l.opError("accept", errClosed)
		}
	}
}

func dstResetBacklogConn(c *dstConn) {
	dstKernelSectionEnter()
	if e, ok := c.Conn.(*dstWireEnd); ok {
		e.diagState.Or(dstDiagRSTLogged)
	}
	if peer := dstConnPeer(c); peer != nil {
		if peerEnd, ok := peer.Conn.(*dstWireEnd); ok {
			peerEnd.recordBacklogRSTDelivered()
		}
	}
	dstKernelSectionExit()
	c.resetConn()
}

func (l *dstListener) Close() error { return l.closeTearing(nil, true) }

// closeTearing closes the listener; each queued backlog connection whose
// claim succeeds (acceptState 0→2) is torn down by tear. nil tear is the
// kernel's own backlog teardown: production TCP resets connections still
// sitting in the accept backlog when the listener closes — so a dialer that
// already got a successful Dial observes ECONNRESET on its next op instead
// of blocking durably forever on a connection no one will ever accept —
// but the RST is a kernel-emitted segment: a blackhole cut of the
// listener→dialer direction swallows it, and that dialer sees the silent
// freeze instead (its probes meet the CLOSED socket's RST after heal). The
// crash paths pass their own tear (a powered-off machine emits nothing for
// ANY backlog conn — dstCloseHostListeners).
func (l *dstListener) closeTearing(tear func(*dstConn), recordClose bool) error {
	if !dstInSimBubble() && dstActive() && l.epoch == dstNetEpoch() {
		return l.opError("close", errClosed)
	}
	if l.stale() {
		if l.closed.Swap(true) {
			return l.opError("close", errClosed)
		}
		return nil
	}
	dstCoarseDep(dstCoarseNetHostID(l.host), true)
	dstCoarseRecord(uintptr(unsafe.Pointer(l)), true)
	if l.sockFD != 0 {
		dstCoarseRecord(dstCoarseSockFDID(l.sockFD), true)
	}
	if l.markClosed() {
		return l.opError("close", errClosed)
	}
	if tear == nil {
		tear = func(c *dstConn) {
			if dstPartitionedDir(c.localHost, c.remoteHost) {
				dstCrashVictimConn(c)
			} else {
				dstResetBacklogConn(c)
			}
		}
	}
	close(l.done)
	dstNet.mu.Lock()
	dstNetRoll()
	if recordClose {
		var addrStr string
		if len(l.keys) > 0 {
			k := l.keys[0]
			for i := 0; i < len(k); i++ {
				if k[i] == '/' {
					addrStr = k[i+1:]
					break
				}
			}
		}
		callerHost, callerProc, callerPid := dstCurrentDiagIdentity()
		var proc uint32
		var pid int32
		if callerHost == l.host {
			proc = callerProc
			pid = callerPid
		}
		dstRecordCausalEvent(dstCausalNetListenerClose, 0, l.host, 0, proc, pid, int64(l.addr.Port), 0, 0, addrStr)
	}
	for _, k := range l.keys {
		if dstNet.listeners[k] == l {
			delete(dstNet.listeners, k)
			cs := dstNet.closedListeners[k]
			if cs == nil {
				cs = new(uint64)
				dstNet.closedListeners[k] = cs
			}
			dstKernelRaceReleaseMerge(unsafe.Pointer(cs))
		}
	}
	dstNet.mu.Unlock()
	dstSockFDFree(l.sockFD, l.opts)
	for {
		select {
		case owned := <-l.accept:
			<-l.backlogSem
			c := owned.conn
			if c.acceptState == nil || c.claimAcceptState(0, 2) {
				tear(c)
			}
		default:
			return nil
		}
	}
}

func (l *dstListener) Addr() Addr { return dstCloneAddr(l.addr) }

// dstListen is net.Listen under DST: register a simulated listener.
func dstListen(lc *ListenConfig, network, address string) (retL Listener, retErr error) {
	if dstUDPNetwork(network) {
		return nil, dstListenStreamOnUDP(network, address)
	}
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

	// The listening socket: production creates the fd and runs
	// ListenConfig.Control on it BEFORE bind, so a Control error preempts
	// any bind conflict, and the callback's address is the REQUESTED form
	// (":0" before an ephemeral port is assigned).
	lOpts := newDstSockOpts()
	lOpts.setReuseAddr(true)
	lfd, ok := dstSockFDAlloc(lOpts)
	if !ok {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: &TCPAddr{IP: reportIP, Port: portnum}, Err: os.NewSyscallError("socket", syscall.EMFILE)}
	}
	defer func() {
		if retErr != nil {
			dstSockFDFree(lfd, lOpts)
		}
	}()
	if lc.Control != nil {
		// The callback's network string is the SOCKET's family form — for a
		// dual-stack wildcard listen production opens AF_INET6 and reports
		// "tcp6" — so it derives from reportIP, not the loopback the
		// simulation maps the wildcard to internally.
		if err := dstRunControl(context.Background(), nil, lc.Control, dstAddrFamily(network, reportIP), (&TCPAddr{IP: reportIP, Port: portnum}).String(), lfd); err != nil {
			return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: &TCPAddr{IP: reportIP, Port: portnum}, Err: err}
		}
	}

	reuseAddr := lOpts.isReuseAddr()
	kaIdle, kaCfg := lc.KeepAlive, lc.KeepAliveConfig

	dstCoarseDep(dstCoarseNetHostID(listeningHost), true)
	dstNet.mu.Lock()
	defer dstNet.mu.Unlock()
	dstNetRoll()
	var keys []string
	if portnum == 0 {
		portnum, keys, err = dstAllocateListenPort(scope, network, ip, listeningHost, wildcard, dual, reuseAddr)
		if err != nil {
			// Production wraps a bind failure with the REQUESTED address
			// (port 0), not a nil Addr.
			return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: &TCPAddr{IP: append(IP(nil), reportIP...), Port: 0}, Err: err}
		}
	} else if dual {
		keys = dstDualKeys(portnum)
	} else {
		keys = []string{dstListenerKey(network, ip, portnum, wildcard)}
	}
	addr := &TCPAddr{IP: append(IP(nil), reportIP...), Port: portnum}
	if dstAnyListenerConflict(scope, network, ip, keys, portnum, wildcard, dual) ||
		dstListenConnConflict(listeningHost, network, ip, portnum, wildcard, dual, reuseAddr) {
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
		network:    network,
		epoch:      dstNetEpoch(),
		addr:       addr,
		keys:       scoped,
		backlogSem: make(chan struct{}, 128),
		accept:     make(chan *dstOwnedConn, 128), // backlog
		done:       make(chan struct{}),
		host:       listeningHost,
		proc:       listeningProc,
		sockFD:     lfd,
		opts:       lOpts,
		kaIdle:     kaIdle,
		kaCfg:      kaCfg,
	}
	for _, k := range scoped {
		dstNet.listeners[k] = l
	}
	dstRecordCausalCaller(dstCausalNetListen, 0, 0, int64(portnum), 0, 0, address)
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
// The same recorded collapse covers fault RESETS: the wire pair registers only after
// this traversal returns, so a ResetLinkConns/ResetProcessConns landing inside the half-RTT matches
// no registered conn and the dial establishes untouched — the injection "missed", an
// execution ⊆-real (a real RST event can miss a handshake too), never a false failure.
func dstConnectSYN(ctx context.Context, latencyNs, jitterNs int64) error {
	d := dstLinkDelay(latencyNs, jitterNs)
	if d <= 0 {
		return nil
	}
	t := dstNewBaseTimer(time.Duration(d))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return mapErr(ctx.Err())
	}
}

// dstConnectSYNACK waits for the second handshake traversal while the connect
// context and both transport endpoints remain live. An abort surfaces as
// ECONNREFUSED, never ECONNRESET: the dialer's socket is still in SYN_SENT
// (connect(2) has not returned), and tcp_reset maps an RST received in
// SYN_SENT to ECONNREFUSED — that mapping IS the connection-refused
// mechanism (host-probed: a dial parked mid-establishment whose listener
// closes fails "connection refused"). ECONNRESET is the established-state
// identity only.
func dstHandshakeAbortedKernel(e *dstWireEnd, reset *atomic.Bool) bool {
	if reset != nil {
		dstKernelSectionEnter()
		r := reset.Load()
		dstKernelSectionExit()
		if r {
			return true
		}
	}
	return e.isRSTArrivedKernel() || dstIsClosedKernelChan(e.localDone) || dstIsClosedKernelChan(e.remoteDone)
}

func dstAcquireHandshakeAbort(e *dstWireEnd, reset *atomic.Bool, resetSync *uint64) {
	dstLoadResetAcquire(reset, resetSync)
	e.isRSTArrivedAcquire()
	if dstIsClosedKernelChan(e.localDone) {
		e.acquireLocalCloseSync()
	}
	if dstIsClosedKernelChan(e.remoteDone) {
		e.acquirePeerCloseSync()
	}
}

func dstConnectSYNACK(ctx context.Context, latencyNs, jitterNs int64, e *dstWireEnd, reset *atomic.Bool, lDone <-chan struct{}) (listenerClosed bool, err error) {
	d := dstLinkDelay(latencyNs, jitterNs)
	if d <= 0 {
		if err := ctx.Err(); err != nil {
			return false, mapErr(err)
		}
		if dstIsClosedKernelChan(lDone) && !dstPartitionedDir(e.peerHost, e.localHost) {
			return true, nil
		}
		if dstHandshakeAbortedKernel(e, reset) {
			return false, syscall.ECONNREFUSED
		}
		return false, nil
	}
	t := dstNewBaseTimer(time.Duration(d))
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	case <-lDone:
		if dstPartitionedDir(e.peerHost, e.localHost) {
			// A closed listener's RST cannot traverse a blackhole cut of
			// listener→dialer; wait out the remaining link delay and let the
			// SYN-ACK cut loop hold until heal or retransmit timeout.
			select {
			case <-t.C:
			case <-ctx.Done():
			case <-e.localDone:
			}
		}
	case <-e.rstKill:
	case <-e.localDone:
	case <-e.remoteDone:
	}
	if err := ctx.Err(); err != nil {
		return false, mapErr(err)
	}
	if dstIsClosedKernelChan(lDone) && !dstPartitionedDir(e.peerHost, e.localHost) {
		return true, nil
	}
	if dstHandshakeAbortedKernel(e, reset) {
		return false, syscall.ECONNREFUSED
	}
	return false, nil
}

// dstDial is net.Dial under DST: find the matching listener and hand back the
// dialer end of a new in-memory connection.
func dstDial(ctx context.Context, d *Dialer, network, address string) (Conn, error) {
	if dstUDPNetwork(network) {
		return dstDialUDPString(ctx, d, network, address)
	}
	c, err := dstDialConnect(ctx, d, network, address)
	if err == nil {
		dstKernelSectionEnter()
		e := c.Conn.(*dstWireEnd)
		dstRecordCausalCaller(dstCausalNetDialEstablished, 0, c.remoteHost, int64(e.localPort), int64(e.peerPort), 0, address)
		dstKernelSectionExit()
		return c, nil
	}
	if opErr, ok := err.(*OpError); ok {
		dstKernelSectionEnter()
		callerHost, _, _ := dstCurrentDiagIdentity()
		targetHost := callerHost
		if tcpAddr, ok := opErr.Addr.(*TCPAddr); ok {
			if h, ok := dstHostForRoutableIP(tcpAddr.IP); ok {
				targetHost = h
			}
		}
		switch opErr.Err {
		case syscall.ECONNREFUSED:
			dstRecordCausalCaller(dstCausalNetDialRefused, 0, targetHost, 0, 0, 0, address)
		case syscall.ETIMEDOUT:
			dstRecordCausalCaller(dstCausalNetDialTimeout, 1, targetHost, 0, 0, 0, address)
		case errTimeout:
			dstRecordCausalCaller(dstCausalNetDialTimeout, 0, targetHost, 0, 0, 0, address)
		}
		dstKernelSectionExit()
	}
	return nil, err
}

func dstDialConnect(ctx context.Context, d *Dialer, network, address string) (retConn *dstConn, retErr error) {
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
	// The dialing socket: production creates the fd and runs
	// Dialer.ControlContext/Control on it after resolution and before
	// connect (sock_posix.go's socket()); the callback's raw sockopts land
	// on the socket's option state, carried into the established connection.
	// A callback error aborts the dial exactly as production's ctrlFn
	// failure does. The descriptor is freed on any dial failure (the socket
	// dies with it) and otherwise lives as the conn's identity.
	dialOpts := newDstSockOpts()
	dialFD, okFD := dstSockFDAlloc(dialOpts)
	if !okFD {
		return nil, &OpError{Op: "dial", Net: network, Source: localTCPAddr.opAddr(), Addr: serverAddr, Err: os.NewSyscallError("socket", syscall.EMFILE)}
	}
	defer func() {
		if retErr != nil {
			dstSockFDFree(dialFD, dialOpts)
		}
	}()
	if d.ControlContext != nil || d.Control != nil {
		if err := dstRunControl(ctx, d.ControlContext, d.Control, dstAddrFamily(network, ip), serverAddr.String(), dialFD); err != nil {
			return nil, &OpError{Op: "dial", Net: network, Source: localTCPAddr.opAddr(), Addr: serverAddr, Err: err}
		}
	}
	dialOpts.applyGoKeepAlive(d.KeepAlive, d.KeepAliveConfig)
	reuseAddr := dialOpts.isReuseAddr()
	dialerHost, dialerProc := dstNetCurrentNode()
	targetHost := dialerHost
	if h, ok := dstHostForRoutableIP(ip); ok {
		targetHost = h
	}
	localIP := IPv4(127, 0, 0, 1)
	if _, routable := dstHostForRoutableIP(ip); routable {
		localIP = dstHostRoutableIP(dialerHost)
	} else if dstAddrFamily(network, ip) == "tcp6" {
		localIP = IPv6loopback
	}
	if localTCPAddr != nil && localTCPAddr.IP != nil && !localTCPAddr.IP.IsUnspecified() {
		if !dstLocalIPOwnedByHost(dialerHost, localTCPAddr.IP) {
			return nil, &OpError{Op: "dial", Net: network, Source: d.LocalAddr, Addr: serverAddr, Err: syscall.EADDRNOTAVAIL}
		}
		localIP = localTCPAddr.IP
	}
	explicitPort := 0
	if localTCPAddr != nil {
		explicitPort = localTCPAddr.Port
	}
	wildcardBind := explicitPort != 0 && (localTCPAddr.IP == nil || localTCPAddr.IP.IsUnspecified())
	var reserved *dstBindKey
	if explicitPort != 0 {
		key := dstExplicitBindKey(dialerHost, network, localIP, explicitPort, wildcardBind)
		bindIP := localIP
		if wildcardBind {
			bindIP = nil
		}
		family := dstAddrFamily(network, localIP)
		dstCoarseDep(dstCoarseNetHostID(dialerHost), true)
		dstNet.mu.Lock()
		dstNetRoll()
		dialerScope := dstNetScope(dialerHost)
		conflict := dstConnBindInUse(dialerHost, bindIP, explicitPort, family, reuseAddr) ||
			(!reuseAddr && dstTimeWaitBindHeld(dialerHost, bindIP, explicitPort, family)) ||
			dstListenerConflict(dialerScope, network, localIP, dstListenerKey(network, localIP, explicitPort, wildcardBind), explicitPort, wildcardBind)
		pending := dstPendingBindInUse(key)
		if conflict || pending {
			dstNet.mu.Unlock()
			src := &TCPAddr{IP: append(IP(nil), localTCPAddr.IP...), Port: explicitPort, Zone: localTCPAddr.Zone}
			return nil, &OpError{Op: "dial", Net: network, Source: src, Addr: serverAddr, Err: syscall.EADDRINUSE}
		}
		if reuseAddr && dstFourTupleInUse(dialerHost, localIP, explicitPort, ip, portnum) {
			dstNet.mu.Unlock()
			src := &TCPAddr{IP: append(IP(nil), localTCPAddr.IP...), Port: explicitPort, Zone: localTCPAddr.Zone}
			return nil, &OpError{Op: "dial", Net: network, Source: src, Addr: serverAddr, Err: syscall.EADDRNOTAVAIL}
		}
		dstNet.pendingBinds[key] = dialerProc
		dstNet.mu.Unlock()
		reserved = &key
		defer func() {
			dstNet.mu.Lock()
			dstNetRoll()
			delete(dstNet.pendingBinds, *reserved)
			dstNet.mu.Unlock()
		}()
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

	// Partition on connect: a Dial across a cut link either REFUSES (the peer answers
	// RST → ECONNREFUSED, fast) or BLACKHOLES (the SYN is dropped → the dial blocks
	// until the link heals, the context/deadline expires, or the retransmit horizon
	// fires ETIMEDOUT — a real kernel's exhausted SYN retries, so a deadline-less dial
	// into a permanent blackhole fails in bounded virtual time rather than hanging).
	// The mode is selectable per fault (Partition vs PartitionRefuse); the cut is
	// checked in BOTH handshake directions (SYN dialer→target, SYN-ACK target→dialer)
	// so a one-directional partition of either also fails the connect. The target host
	// is the routable IP's owner; a loopback/own-host target is never partitioned.
	retransNs := dstNetRetransmitTimeoutNs()
	if uto := dialOpts.userTimeoutNs(); uto > 0 {
		// TCP_USER_TIMEOUT set on the dialing socket (a Control callback)
		// bounds the connect's retransmissions too (tcp(7): the option
		// applies in SYN_SENT), overriding the run's horizon for this dial.
		retransNs = uto
	}
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
	dstCoarseDep(dstCoarseNetHostID(dialerHost), true)
	if targetHost != dialerHost {
		dstCoarseRecord(dstCoarseNetHostID(targetHost), false)
	}
	blockStart := int64(-1) // base-time the dial first blocked; -1 = not yet blocked
	for {
		wake := dstPartWakeCh()
		cut, refuse := dstDialCut(dialerHost, targetHost)
		if !cut {
			break
		}
		if refuse {
			dstAcquirePartFaultSync(dialerHost, targetHost)
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
				dstAcquirePartFaultSync(dialerHost, targetHost)
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
			dstAcquirePartFaultSync(dialerHost, targetHost)
			return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.ETIMEDOUT}
		}
	}
	if blockStart >= 0 {
		dstCoarseRecord(dstCoarseNetHostID(dialerHost), true)
		if targetHost != dialerHost {
			dstCoarseRecord(dstCoarseNetHostID(targetHost), false)
		}
	}
	scope := dstDialScope(ip, dialerHost)
	kExact := scope + dstListenerKey(network, ip, portnum, false)
	kWild := scope + dstListenerKey(network, ip, portnum, true)
	dstNet.mu.Lock()
	dstNetRoll()
	l := dstNet.listeners[kExact]
	if l == nil {
		l = dstNet.listeners[kWild] // a wildcard listener on this port/family
	}
	var closedExactSync, closedWildSync *uint64
	if l == nil {
		closedExactSync = dstNet.closedListeners[kExact]
		closedWildSync = dstNet.closedListeners[kWild]
	}
	localPort := explicitPort
	if localPort != 0 {
		dialerScope := dstNetScope(dialerHost)
		bindIP := localIP
		if wildcardBind {
			bindIP = nil
		}
		family := dstAddrFamily(network, localIP)
		if dstConnBindInUse(dialerHost, bindIP, localPort, family, reuseAddr) ||
			(!reuseAddr && dstTimeWaitBindHeld(dialerHost, bindIP, localPort, family)) ||
			dstListenerConflict(dialerScope, network, localIP, dstListenerKey(network, localIP, localPort, wildcardBind), localPort, wildcardBind) {
			dstNet.mu.Unlock()
			src := &TCPAddr{IP: append(IP(nil), localTCPAddr.IP...), Port: localPort, Zone: localTCPAddr.Zone}
			return nil, &OpError{Op: "dial", Net: network, Source: src, Addr: serverAddr, Err: syscall.EADDRINUSE}
		}
		if reuseAddr && dstFourTupleInUse(dialerHost, localIP, localPort, ip, portnum) {
			dstNet.mu.Unlock()
			src := &TCPAddr{IP: append(IP(nil), localTCPAddr.IP...), Port: localPort, Zone: localTCPAddr.Zone}
			return nil, &OpError{Op: "dial", Net: network, Source: src, Addr: serverAddr, Err: syscall.EADDRNOTAVAIL}
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
	if l != nil {
		dstCoarseRecord(uintptr(unsafe.Pointer(l)), true)
	} else {
		if dstHostDead(targetHost) || (targetHost != dialerHost && !dstHostDeclaredQ(targetHost)) {
			// Powered off (or never declared) between the clear-path check
			// and the lookup — re-enter the front-door blackhole loop.
			goto redial
		}
		if cut, _ := dstDialCut(dialerHost, targetHost); cut {
			// A partition landed between the front-door cut check and the
			// listener table lookup (reachable under Level-2 Explore yields);
			// re-enter redial so blackhole waits and refuse acquires fault sync.
			goto redial
		}
		dstKernelSectionEnter()
		if closedExactSync != nil {
			dstKernelRaceAcquire(unsafe.Pointer(closedExactSync))
		}
		if closedWildSync != nil {
			dstKernelRaceAcquire(unsafe.Pointer(closedWildSync))
		}
		dstKernelSectionExit()
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
	capacity, retrans := dstNetSendBufferBytes(), dstNetRetransmitTimeoutNs()
	// SYN: the first half of the connect round trip travels dialerHost→l.host. A
	// connect deadline shorter than this traversal fails now, before anything is
	// established — a zero-RTT connect would instead let a SUT's connect timeout
	// pass under simulation on a link where it fails in production (unsound).
	synLat, synJit, _ := dstLinkParamsForDir(dialerHost, l.host)
	if err := dstConnectSYN(ctx, synLat, synJit); err != nil {
		return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: err}
	}
	// The established connection's socket-option states: the dialer end
	// carries the dialing socket's (Control's writes plus the Go-level
	// keepalive resolution applied above). The server end inherits a clone of
	// the listener's options (accept(2) inheritance) and takes the
	// ListenConfig resolution the same way.
	var (
		dialer, server *dstConn
		ownedServer    *dstOwnedConn
		p1, p2         Conn
		reset          *atomic.Bool
		resetSync      *uint64
	)
	func() {
		dstKernelSectionEnter()
		defer dstKernelSectionExit()
		localClone := &TCPAddr{IP: append(IP(nil), localAddr.IP...), Port: localAddr.Port, Zone: localAddr.Zone}
		serverClone := &TCPAddr{IP: append(IP(nil), serverAddr.IP...), Port: serverAddr.Port, Zone: serverAddr.Zone}
		serverOpts := l.opts.cloneForChild()
		serverOpts.applyGoKeepAlive(l.kaIdle, l.kaCfg)
		serverFD, okServerFD := dstSockFDAlloc(serverOpts)
		if !okServerFD {
			return
		}
		p1, p2 = dstWirePair(capacity, retrans, dialerHost, l.host, dialOpts, serverOpts)
		p1.(*dstWireEnd).localPort = uint16(localClone.Port)
		p1.(*dstWireEnd).peerPort = uint16(serverClone.Port)
		p2.(*dstWireEnd).localPort = uint16(serverClone.Port)
		p2.(*dstWireEnd).peerPort = uint16(localClone.Port)
		reset = new(atomic.Bool)
		resetSync = new(uint64)
		epoch := dstNetEpoch()
		dialer = &dstConn{Conn: p1, epoch: epoch, network: network, local: localClone, remote: serverClone, reset: reset, resetSync: resetSync, localHost: dialerHost, remoteHost: l.host, localProc: dialerProc, remoteProc: l.proc, bindWildcard: wildcardBind, bindFamily: dstAddrFamily(network, localIP), sockFD: dialFD, sockOpts: dialOpts}
		server = &dstConn{Conn: p2, epoch: epoch, network: network, local: serverClone, remote: localClone, reset: reset, resetSync: resetSync, acceptState: new(atomic.Int32), localHost: l.host, remoteHost: dialerHost, localProc: l.proc, remoteProc: dialerProc, bindFamily: dstAddrFamily(network, serverClone.IP), sockFD: serverFD, sockOpts: serverOpts}
		// Publish lifecycle ownership before the server endpoint can enter the
		// backlog or become visible to Accept. Failed establishment removes both
		// registrations through the idempotent cleanup below.
		ownedServer = dstOwnConnPair(dialer, server)
	}()
	if ownedServer == nil {
		return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: os.NewSyscallError("socket", syscall.EMFILE)}
	}
	slotHeld := false
	cleanup := func() {
		if slotHeld {
			slotHeld = false
			<-l.backlogSem
		}
		server.claimAcceptState(0, 2)
		dialOpts.setKick(nil)
		dialer.sockFD = 0
		dialer.resetConn()
		server.resetConn()
	}
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
	// The backlog park models the SYN's undeliverability across a partition:
	// while the handshake path is cut, the retransmitted SYN cannot reach the
	// listener (nor could the child's handshake complete), so the parked send
	// must not land — an Accept freeing a slot during a cut completes OTHER
	// dials, never this one. The park re-checks the cut table on every
	// partition change (the same clear-path wait the front-door blackhole
	// loop performs), bounded by the already-armed retransmit horizon. The
	// cut-wait deliberately does NOT wake on l.done: no RST can traverse a
	// blackhole cut, so a listener closed during the cut is observed only
	// after heal, when the re-offered SYN meets the closed port. A cut
	// landing while the send is ALREADY parked commits the dial to the
	// partition wake (the fault op runs on a scheduled goroutine, so its
	// wake resolves the select before any Accept can run); an Accept that
	// raced ahead of the cut is the SYN-arrived-at-the-cut-instant ordering,
	// one of the two the real race produces.
	for {
		partWake := dstPartWakeCh()
		if cut, refuse := dstDialCut(dialerHost, l.host); cut {
			if refuse {
				// A refuse-mode cut answers the retransmitted SYN with RST:
				// ECONNREFUSED, the SYN_SENT identity (immediate, the same
				// recorded timing simplification as the front-door refusal).
				stopBacklogHorizon()
				dstAcquirePartFaultSync(dialerHost, l.host)
				cleanup()
				return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
			}
			select {
			case <-ctx.Done():
				stopBacklogHorizon()
				cleanup()
				return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: mapErr(ctx.Err())}
			case <-backlogHorizonC:
				dstAcquirePartFaultSync(dialerHost, l.host)
				cleanup()
				return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ETIMEDOUT}
			case <-partWake:
				// Partition state changed: re-check the cut.
			case <-p1.(*dstWireEnd).localDone:
				stopBacklogHorizon()
				p1.(*dstWireEnd).acquireLocalCloseSync()
				cleanup()
				return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
			case <-p1.(*dstWireEnd).remoteDone:
				stopBacklogHorizon()
				if dstHostDead(l.host) {
					cleanup()
					goto redial
				}
				p1.(*dstWireEnd).acquirePeerCloseSync()
				cleanup()
				return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
			case <-p1.(*dstWireEnd).rstKill:
				stopBacklogHorizon()
				if dstHostDead(l.host) {
					cleanup()
					goto redial
				}
				p1.(*dstWireEnd).acquireRSTSync()
				dstLoadResetAcquire(reset, resetSync)
				cleanup()
				return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
			}
			continue
		}
		select {
		case <-ctx.Done():
			stopBacklogHorizon()
			cleanup()
			return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: mapErr(ctx.Err())}
		case <-backlogHorizonC:
			dstAcquirePartFaultSync(dialerHost, l.host)
			cleanup()
			return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ETIMEDOUT}
		case <-partWake:
			// A partition landed while the send was parked: re-check the cut
			// before offering the SYN again.
			continue
		// Every abort of a still-parked dial carries ECONNREFUSED, never
		// ECONNRESET: the dialer's socket is in SYN_SENT (connect(2) has not
		// returned), and tcp_reset maps an RST received in SYN_SENT to
		// ECONNREFUSED — the same identity the closed-listener path below
		// surfaces (host-probed; see dstConnectSYNACK).
		case <-p1.(*dstWireEnd).localDone:
			stopBacklogHorizon()
			p1.(*dstWireEnd).acquireLocalCloseSync()
			cleanup()
			return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
		case <-p1.(*dstWireEnd).remoteDone:
			stopBacklogHorizon()
			if dstHostDead(l.host) {
				cleanup()
				goto redial
			}
			p1.(*dstWireEnd).acquirePeerCloseSync()
			cleanup()
			return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
		case <-p1.(*dstWireEnd).rstKill:
			// A fault-injected RST during the SYN retransmit window (backlog
			// full, or a reset racing establishment) aborts the connect promptly
			// — the drain-then-reset teardown of the queued/registered ends does
			// not close the transports, so the done channels above stay silent.
			// The dead-host check mirrors the remoteDone case: a crashed server
			// host emits no RST, so that path redials into the blackhole and
			// times out as production does.
			stopBacklogHorizon()
			if dstHostDead(l.host) {
				cleanup()
				goto redial
			}
			p1.(*dstWireEnd).acquireRSTSync()
			dstLoadResetAcquire(reset, resetSync)
			cleanup()
			return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
		case l.backlogSem <- struct{}{}:
			slotHeld = true
			stopBacklogHorizon()
			select {
			case <-l.done:
				if !dstPartitionedDir(l.host, dialerHost) {
					cleanup()
					goto refused
				}
			default:
			}
			// SYN-ACK: the acknowledgment travels back l.host→dialerHost before the
			// server child enters ESTABLISHED and becomes visible to Accept. Context
			// expiry, listener close, or endpoint teardown before its arrival aborts
			// the connect and tears down both ends without ever waking Accept.
			ackLat, ackJit, _ := dstLinkParamsForDir(l.host, dialerHost)
			lClosed, err := dstConnectSYNACK(ctx, ackLat, ackJit, p1.(*dstWireEnd), reset, l.done)
			if lClosed && !dstPartitionedDir(l.host, dialerHost) {
				cleanup()
				goto refused
			}
			if err != nil {
				if dstHostDead(l.host) {
					cleanup()
					goto redial
				}
				if err == syscall.ECONNREFUSED {
					dstAcquireHandshakeAbort(p1.(*dstWireEnd), reset, resetSync)
				}
				cleanup()
				return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: err}
			}
			// SYN-ACK across a cut: the acknowledgment travels the RETURNING
			// direction (listener→dialer); a cut active there when it would
			// complete holds it like any undeliverable segment — the server's
			// SYN-ACK retransmits into the void — so connect(2) completes only
			// on heal, or fails ETIMEDOUT at the retransmit horizon anchored
			// at the first cut observation on this leg (never earlier than a
			// kernel's own retransmit clock — the sound, errs-later anchor). A
			// pure refuse-mode cut answers the handshake with RST instead:
			// ECONNREFUSED, the SYN_SENT identity.
			synackBlock := int64(-1)
			for {
				ackWake := dstPartWakeCh()
				_, ackCut, ackBlackhole := dstPartCutStartDir(l.host, dialerHost)
				if !ackCut {
					if dstHandshakeAbortedKernel(p1.(*dstWireEnd), reset) {
						if dstHostDead(l.host) {
							cleanup()
							goto redial
						}
						dstAcquireHandshakeAbort(p1.(*dstWireEnd), reset, resetSync)
						cleanup()
						return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
					}
					select {
					case <-l.done:
						if dstPartitionedDir(l.host, dialerHost) {
							continue
						}
						cleanup()
						goto refused
					default:
					}
					// Handshake reached ESTABLISHED: transfer the backlog slot
					// token into l.accept (guaranteed non-blocking since every
					// item in l.accept holds one backlogSem slot).
					slotHeld = false
					dstKernelSectionEnter()
					p1.(*dstWireEnd).diagState.Or(dstDiagEstablished)
					p2.(*dstWireEnd).diagState.Or(dstDiagEstablished)
					dstKernelSectionExit()
					l.accept <- ownedServer
					select {
					case <-l.done:
						for {
							select {
							case owned := <-l.accept:
								<-l.backlogSem
								c := owned.conn
								if c.acceptState == nil || c.claimAcceptState(0, 2) {
									if dstHostDead(l.host) || dstPartitionedDir(c.localHost, c.remoteHost) {
										dstCrashVictimConn(c)
									} else {
										dstResetBacklogConn(c)
									}
								}
							default:
								goto drainedPostEnqueue
							}
						}
					drainedPostEnqueue:
						dstKernelSectionEnter()
						rejected := server.acceptState.Load() == 2
						dstKernelSectionExit()
						if rejected && server.isResetKernel() {
							dstKernelSectionEnter()
							p1.(*dstWireEnd).diagState.And(^uint32(dstDiagEstablished))
							p2.(*dstWireEnd).diagState.And(^uint32(dstDiagEstablished))
							dstKernelSectionExit()
							dialOpts.setKick(nil)
							dialer.sockFD = 0
							dialer.resetConn()
							server.resetConn()
							goto refused
						}
					default:
					}
					dstKernelSectionEnter()
					p1.(*dstWireEnd).diagState.Or(dstDiagDialReturned)
					dstKernelSectionExit()
					return dialer, nil
				}
				if !ackBlackhole {
					dstAcquirePartFaultSync(l.host, dialerHost)
					cleanup()
					return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
				}
				if synackBlock < 0 {
					synackBlock = dstBaseNanos()
				}
				var ackHorizonC <-chan time.Time
				var ackHorizonT *time.Timer
				if retransNs > 0 {
					remaining := retransNs - (dstBaseNanos() - synackBlock)
					if remaining <= 0 {
						dstAcquirePartFaultSync(l.host, dialerHost)
						cleanup()
						return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ETIMEDOUT}
					}
					ackHorizonT = time.NewTimer(time.Duration(remaining))
					ackHorizonC = ackHorizonT.C
				}
				select {
				case <-ctx.Done():
					if ackHorizonT != nil {
						ackHorizonT.Stop()
					}
					cleanup()
					return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: mapErr(ctx.Err())}
				case <-ackWake:
					if ackHorizonT != nil {
						ackHorizonT.Stop()
					}
				case <-ackHorizonC:
					dstAcquirePartFaultSync(l.host, dialerHost)
					cleanup()
					return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ETIMEDOUT}
				case <-p1.(*dstWireEnd).localDone:
					if ackHorizonT != nil {
						ackHorizonT.Stop()
					}
					p1.(*dstWireEnd).acquireLocalCloseSync()
					cleanup()
					return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
				case <-p1.(*dstWireEnd).remoteDone:
					if ackHorizonT != nil {
						ackHorizonT.Stop()
					}
					if dstHostDead(l.host) {
						cleanup()
						goto redial
					}
					p1.(*dstWireEnd).acquirePeerCloseSync()
					cleanup()
					return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
				case <-p1.(*dstWireEnd).rstKill:
					if ackHorizonT != nil {
						ackHorizonT.Stop()
					}
					if dstHostDead(l.host) {
						cleanup()
						goto redial
					}
					p1.(*dstWireEnd).acquireRSTSync()
					dstLoadResetAcquire(reset, resetSync)
					cleanup()
					return nil, &OpError{Op: "dial", Net: network, Source: localAddr, Addr: serverAddr, Err: syscall.ECONNREFUSED}
				}
			}
		case <-l.done:
			if dstPartitionedDir(l.host, dialerHost) {
				continue
			}
			stopBacklogHorizon()
			cleanup()
			goto refused
		}
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
	if cut, _ := dstDialCut(dialerHost, l.host); cut {
		goto redial
	}
	l.acquireCloseSync()
	return nil, &OpError{Op: "dial", Net: network, Source: nil, Addr: serverAddr, Err: syscall.ECONNREFUSED}
}
