// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package net

import (
	"context"
	"errors"
	"internal/nettrace"
	rtatomic "internal/runtime/atomic"
	isync "internal/sync"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
	"weak"
)

//go:linkname dstFaultProb runtime.dstFaultProb
func dstFaultProb(p float64) bool

//go:linkname dstCurrentSeed runtime.dstCurrentSeed
func dstCurrentSeed() uint64

const (
	dstCausalUDPBind    uint8 = 60
	dstCausalUDPClose   uint8 = 61
	dstCausalUDPRefused uint8 = 62
	dstCausalUDPDrop    uint8 = 63

	dstUDPDropPartition  uint8 = 1
	dstUDPDropPacketLoss uint8 = 2
	dstUDPDropRcvBuf     uint8 = 3

	dstUDPMaxIPv4Payload = 65535 - 20 - 8 // 65507 bytes
	dstUDPMaxIPv6Payload = 65535 - 40 - 8 // 65527 bytes
)

// Out-of-line per-netFD UDP state table (mirrors os/dst_filestate.go so netFD,
// UDPConn, conn, and rawConn retain exact stock field and method shapes under
// both untagged and -tags dst builds: INV-VANILLA + INV-TYPESHAPE).
const dstUDPStateChunk = 512

type dstUDPStateRow struct {
	obj  weak.Pointer[netFD]
	sock *dstUDPSock
}

type dstUDPStateChunkArr [dstUDPStateChunk]atomic.Pointer[dstUDPStateRow]

type dstUDPStateTable struct {
	mu    isync.KernelMutex
	spine atomic.Pointer[[]*dstUDPStateChunkArr]
	next  int
	free  []int
}

var dstUDPStates dstUDPStateTable

func (t *dstUDPStateTable) register(fd *netFD, sock *dstUDPSock) int {
	row := &dstUDPStateRow{obj: weak.Make(fd), sock: sock}
	t.mu.Lock()
	var idx int
	if n := len(t.free); n > 0 {
		idx = t.free[n-1]
		t.free = t.free[:n-1]
	} else {
		idx = t.next
		t.next++
		if spine := t.spine.Load(); spine == nil || idx/dstUDPStateChunk >= len(*spine) {
			var old []*dstUDPStateChunkArr
			if spine != nil {
				old = *spine
			}
			grown := make([]*dstUDPStateChunkArr, len(old)+1)
			copy(grown, old)
			grown[len(old)] = new(dstUDPStateChunkArr)
			t.spine.Store(&grown)
		}
	}
	(*t.spine.Load())[idx/dstUDPStateChunk][idx%dstUDPStateChunk].Store(row)
	t.mu.Unlock()
	return idx
}

func (t *dstUDPStateTable) get(idx int) *dstUDPSock {
	spine := t.spine.Load()
	if spine == nil || idx < 0 || idx/dstUDPStateChunk >= len(*spine) {
		return nil
	}
	row := (*spine)[idx/dstUDPStateChunk][idx%dstUDPStateChunk].Load()
	if row == nil {
		return nil
	}
	return row.sock
}

// sweep reclaims rows whose *netFD has been collected by GC. Called ONLY in
// host context at run teardown (dstNetRunTeardown) so weak.Pointer.Value never
// parks on mark termination inside a simulation bubble.
func (t *dstUDPStateTable) sweep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	spine := t.spine.Load()
	if spine == nil {
		return
	}
	for idx := 0; idx < t.next; idx++ {
		cell := &(*spine)[idx/dstUDPStateChunk][idx%dstUDPStateChunk]
		row := cell.Load()
		if row == nil || row.obj.Value() != nil {
			continue
		}
		cell.Store(nil)
		t.free = append(t.free, idx)
	}
}

// dstUDPStateStats exposes high-water index and free-list length for white-box
// teardown-sweep verification.
func dstUDPStateStats() (next, free int) {
	dstUDPStates.mu.Lock()
	defer dstUDPStates.mu.Unlock()
	return dstUDPStates.next, len(dstUDPStates.free)
}

func dstNetRunTeardown() {
	dstUDPStates.sweep()
}

func dstNetFDSysfd(fd *netFD) int {
	return int(rtatomic.Loaduintptr((*uintptr)(unsafe.Pointer(&fd.pfd.Sysfd))))
}

func dstAttachUDPSock(fd *netFD, sock *dstUDPSock) {
	slot := -2 - dstUDPStates.register(fd, sock)
	rtatomic.Storeuintptr((*uintptr)(unsafe.Pointer(&fd.pfd.Sysfd)), uintptr(slot))
}

func dstUDPSockOf(fd *netFD) *dstUDPSock {
	if fd == nil {
		return nil
	}
	sysfd := dstNetFDSysfd(fd)
	if sysfd > -2 {
		return nil
	}
	s := dstUDPStates.get(-2 - sysfd)
	runtime.KeepAlive(fd)
	return s
}

// Per-run global UDP state. Completely disjoint from dstNet's TCP listener and
// ephemeral port tables so TCP and UDP ports never collide. Never nested with
// any dstUDPNet.mu (both are leaf locks with respect to each other).
var dstUDP struct {
	mu             isync.KernelMutex
	epoch          uint64
	byHost         map[uint32]*dstUDPNet
	partDiagLogged map[uint64]bool
	lossDiagLogged map[uint64]bool
}

func dstUDPRollLocked() {
	if e := dstNetEpoch(); e != dstUDP.epoch || dstUDP.byHost == nil {
		dstUDP.epoch = e
		dstUDP.byHost = make(map[uint32]*dstUDPNet)
		dstUDP.partDiagLogged = make(map[uint64]bool)
		dstUDP.lossDiagLogged = make(map[uint64]bool)
	}
}

func dstUDPNetForHost(host uint32) *dstUDPNet {
	dstUDP.mu.Lock()
	dstUDPRollLocked()
	h := dstUDP.byHost[host]
	if h == nil {
		h = &dstUDPNet{
			host:              host,
			epoch:             dstUDP.epoch,
			byPort:            make(map[int][]*dstUDPSock),
			linkFreeAt:        make(map[uint32]int64),
			flowLastDeliverAt: make(map[dstUDPFlowKey]int64),
		}
		dstUDP.byHost[host] = h
	}
	dstUDP.mu.Unlock()
	return h
}

type dstUDPDropEvent struct {
	kind    uint8
	srcHost uint32
	srcPort uint32
	dstHost uint32
	dstPort uint32
}

func dstUDPRecordDropOnce(kind uint8, srcHost, srcPort, dstHost, dstPort uint32) {
	key := dstDirKey(srcHost, dstHost)
	emit := false
	dstUDP.mu.Lock()
	dstUDPRollLocked()
	switch kind {
	case dstUDPDropPartition:
		if !dstUDP.partDiagLogged[key] {
			dstUDP.partDiagLogged[key] = true
			emit = true
		}
	case dstUDPDropPacketLoss:
		if !dstUDP.lossDiagLogged[key] {
			dstUDP.lossDiagLogged[key] = true
			emit = true
		}
	}
	dstUDP.mu.Unlock()
	if emit {
		dstKernelSectionEnter()
		_, proc, pid := dstCurrentDiagIdentity()
		dstRecordCausalEvent(dstCausalUDPDrop, kind, srcHost, dstHost, proc, pid, int64(srcPort), int64(dstPort), 0, "")
		dstKernelSectionExit()
	}
}

func dstUDPRecordDrops(drops []dstUDPDropEvent) {
	for _, d := range drops {
		dstUDPRecordDropOnce(d.kind, d.srcHost, d.srcPort, d.dstHost, d.dstPort)
	}
}

func dstUDPResetLossDiag(op, a, b uint32) {
	dstUDP.mu.Lock()
	dstUDPRollLocked()
	delete(dstUDP.lossDiagLogged, dstDirKey(a, b))
	if op != dstLinkOpSetOneWay {
		delete(dstUDP.lossDiagLogged, dstDirKey(b, a))
	}
	dstUDP.mu.Unlock()
}

type dstUDPFlowKey struct {
	srcHost uint32
	srcPort uint16
	dstPort uint16
	srcIP   [16]byte
	dstIP   [16]byte
}

func makeDstUDPFlowKey(srcHost uint32, srcIP IP, srcPort int, dstIP IP, dstPort int) dstUDPFlowKey {
	var k dstUDPFlowKey
	k.srcHost = srcHost
	k.srcPort = uint16(srcPort)
	k.dstPort = uint16(dstPort)
	if ip16 := srcIP.To16(); ip16 != nil {
		copy(k.srcIP[:], ip16)
	}
	if ip16 := dstIP.To16(); ip16 != nil {
		copy(k.dstIP[:], ip16)
	}
	return k
}

// dstUDPPacket is one datagram either in flight in a target host's min-heap or
// sitting in a bound socket's bounded receive queue.
type dstUDPPacket struct {
	srcHost    uint32
	srcIP      IP
	srcPort    int
	dstIP      IP
	dstPort    int
	payload    []byte
	sendAt     int64
	deliverAt  int64
	seq        uint64
	senderSock *dstUDPSock // non-nil only when sent by a connected UDP socket
	raceSync   uint64
}

type dstUDPRefusal struct {
	sender    *dstUDPSock
	peerHost  uint32
	peerPort  int
	sendAt    int64
	deliverAt int64
	seq       uint64
	raceSync  uint64
}

type dstUDPRefusalHeap []dstUDPRefusal

func (h dstUDPRefusalHeap) less(i, j int) bool {
	if h[i].deliverAt != h[j].deliverAt {
		return h[i].deliverAt < h[j].deliverAt
	}
	return h[i].seq < h[j].seq
}

func (h *dstUDPRefusalHeap) push(ref dstUDPRefusal) {
	*h = append(*h, ref)
	i := len(*h) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if !h.less(i, parent) {
			break
		}
		(*h)[i], (*h)[parent] = (*h)[parent], (*h)[i]
		i = parent
	}
}

func (h *dstUDPRefusalHeap) pop() dstUDPRefusal {
	old := *h
	n := len(old) - 1
	top := old[0]
	old[0] = old[n]
	old[n] = dstUDPRefusal{}
	*h = old[:n]
	i := 0
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		smallest := left
		if right := left + 1; right < n && h.less(right, left) {
			smallest = right
		}
		if !h.less(smallest, i) {
			break
		}
		(*h)[i], (*h)[smallest] = (*h)[smallest], (*h)[i]
		i = smallest
	}
	return top
}

type dstUDPPacketHeap []*dstUDPPacket

func (h dstUDPPacketHeap) less(i, j int) bool {
	if h[i].deliverAt != h[j].deliverAt {
		return h[i].deliverAt < h[j].deliverAt
	}
	return h[i].seq < h[j].seq
}

func (h *dstUDPPacketHeap) push(pkt *dstUDPPacket) {
	*h = append(*h, pkt)
	i := len(*h) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if !h.less(i, parent) {
			break
		}
		(*h)[i], (*h)[parent] = (*h)[parent], (*h)[i]
		i = parent
	}
}

func (h *dstUDPPacketHeap) pop() *dstUDPPacket {
	old := *h
	n := len(old) - 1
	top := old[0]
	old[0] = old[n]
	old[n] = nil
	*h = old[:n]
	i := 0
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		smallest := left
		if right := left + 1; right < n && h.less(right, left) {
			smallest = right
		}
		if !h.less(smallest, i) {
			break
		}
		(*h)[i], (*h)[smallest] = (*h)[smallest], (*h)[i]
		i = smallest
	}
	return top
}

// dstUDPNet owns all UDP port bindings, in-flight datagrams, and per-socket
// receive queues for a single simulated host.
type dstUDPNet struct {
	mu                isync.KernelMutex
	host              uint32
	epoch             uint64
	seq               uint64 // monotonic registration & packet tie-breaker counter
	nextListenPort    int
	nextDialPort      int
	byPort            map[int][]*dstUDPSock
	inflight          dstUDPPacketHeap
	linkFreeAt        map[uint32]int64 // srcHost -> serialization horizon into this host
	flowLastDeliverAt map[dstUDPFlowKey]int64
	wakeGen           chan struct{}
}

func (h *dstUDPNet) wakeChLocked() <-chan struct{} {
	if h.wakeGen == nil {
		h.wakeGen = make(chan struct{})
	}
	return h.wakeGen
}

func (h *dstUDPNet) bumpWakeLocked() {
	if h.wakeGen != nil {
		close(h.wakeGen)
		h.wakeGen = nil
	}
}

type dstUDPSock struct {
	hostNet   *dstUDPNet
	epoch     uint64
	regSeq    uint64
	host      uint32
	proc      uint32
	network   string
	family    int
	ipv6only  bool
	wildcard  bool
	bindIP    IP // canonical 4B or 16B when !wildcard; nil when wildcard
	port      int
	laddr     *UDPAddr
	laddrStr  string
	connected bool
	peerHost  uint32
	peerIP    IP
	peerPort  int
	raddr     *UDPAddr
	sockFD    int
	opts      *dstSockOpts

	// Wrapper-local post-run Close idempotence latch (never touches bubble locks):
	staleClosed atomic.Bool

	// Guarded by hostNet.mu:
	closed        bool
	closeSync     uint64
	ready         []*dstUDPPacket
	readyBytes    int
	rcvBufCap     int
	rcvDropLogged bool
	errInflight   dstUDPRefusalHeap
	skErr         syscall.Errno
	skErrPeerHost uint32
	skErrPeerPort int
	skErrSync     uint64
	rdTimer       *time.Timer
	rdExpired     bool
	wrTimer       *time.Timer
	wrExpired     bool
	wakeGen       chan struct{}
}

func (s *dstUDPSock) stale() bool {
	return !dstInSimBubble() || s.epoch != dstNetEpoch()
}

func (s *dstUDPSock) wakeChLocked() <-chan struct{} {
	if s.wakeGen == nil {
		s.wakeGen = make(chan struct{})
	}
	return s.wakeGen
}

func (s *dstUDPSock) bumpWakeLocked() {
	if s.wakeGen != nil {
		close(s.wakeGen)
		s.wakeGen = nil
	}
}

func dstUDPPacketCharge(payloadLen int) int {
	if payloadLen <= 0 {
		return 1
	}
	return payloadLen
}

func dstNormIP(ip IP) IP {
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		out := make(IP, IPv4len)
		copy(out, v4)
		return out
	}
	if v6 := ip.To16(); v6 != nil {
		out := make(IP, IPv6len)
		copy(out, v6)
		return out
	}
	return nil
}

func dstUDPCoversFamily(family int, ipv6only bool, targetIP IP) bool {
	if targetIP.To4() != nil {
		return family == syscall.AF_INET || !ipv6only
	}
	return family == syscall.AF_INET6
}

// dstUDPBindOverlap reports whether a candidate UDP bind on port conflicts with
// existing bound socket s on the same host and port (Linux udp_lib_lport_inuse2
// + inet_rcv_saddr_equal).
func dstUDPBindOverlap(
	candFamily int, candIPv6Only, candWildcard bool, candIP IP, candReuseAddr, candReusePort bool,
	s *dstUDPSock,
) bool {
	sReuseAddr := s.opts.isReuseAddr()
	sReusePort := s.opts.isReusePort()

	var addrOverlap bool
	switch {
	case !candWildcard && !s.wildcard:
		addrOverlap = candIP.Equal(s.bindIP)
	case candWildcard && s.wildcard:
		cV4 := candFamily == syscall.AF_INET || !candIPv6Only
		cV6 := candFamily == syscall.AF_INET6
		sV4 := s.family == syscall.AF_INET || !s.ipv6only
		sV6 := s.family == syscall.AF_INET6
		addrOverlap = (cV4 && sV4) || (cV6 && sV6)
	case candWildcard && !s.wildcard:
		if dstUDPCoversFamily(candFamily, candIPv6Only, s.bindIP) {
			addrOverlap = !(candReuseAddr && sReuseAddr)
		}
	case !candWildcard && s.wildcard:
		if dstUDPCoversFamily(s.family, s.ipv6only, candIP) {
			addrOverlap = !(candReuseAddr && sReuseAddr)
		}
	}
	if !addrOverlap {
		return false
	}
	if candReusePort && sReusePort {
		return false
	}
	return true
}

func (h *dstUDPNet) hasBindConflictLocked(
	family int, ipv6only, wildcard bool, bindIP IP, port int, reuseAddr, reusePort bool,
) bool {
	for _, s := range h.byPort[port] {
		if !s.closed && dstUDPBindOverlap(family, ipv6only, wildcard, bindIP, reuseAddr, reusePort, s) {
			return true
		}
	}
	return false
}

// dstUDPReusePortHash computes a stateless, deterministic 4-tuple hash salted
// by the run seed and target host, advancing zero RNG streams.
func dstUDPReusePortHash(seed uint64, host uint32, srcIP IP, srcPort int, dstIP IP, dstPort int) uint64 {
	mix := func(x uint64) uint64 {
		x += 0x9e3779b97f4a7c15
		x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
		x = (x ^ (x >> 27)) * 0x94d049bb133111eb
		return x ^ (x >> 31)
	}
	acc := mix(seed ^ (uint64(host) << 32) ^ uint64(uint16(srcPort))<<16 ^ uint64(uint16(dstPort)))
	if ip16 := srcIP.To16(); ip16 != nil {
		var w0, w1 uint64
		for i := 0; i < 8; i++ {
			w0 = (w0 << 8) | uint64(ip16[i])
			w1 = (w1 << 8) | uint64(ip16[8+i])
		}
		acc = mix(acc ^ mix(w0) ^ mix(w1))
	}
	if ip16 := dstIP.To16(); ip16 != nil {
		var w0, w1 uint64
		for i := 0; i < 8; i++ {
			w0 = (w0 << 8) | uint64(ip16[i])
			w1 = (w1 << 8) | uint64(ip16[8+i])
		}
		acc = mix(acc ^ mix(w0) ^ mix(w1))
	}
	return acc
}

// lookupLocked selects the winning bound UDP socket on host h for an arriving
// datagram (srcIP:srcPort -> dstIP:dstPort), matching Linux compute_score +
// SO_REUSEPORT 4-tuple selection. Caller holds h.mu.
func (h *dstUDPNet) lookupLocked(srcIP IP, srcPort int, dstIP IP, dstPort int) *dstUDPSock {
	socks := h.byPort[dstPort]
	if len(socks) == 0 {
		return nil
	}
	bestScore := 0
	var best []*dstUDPSock
	for _, s := range socks {
		if s.closed {
			continue
		}
		if !dstUDPCoversFamily(s.family, s.ipv6only, dstIP) {
			continue
		}
		score := 0
		if s.connected {
			if s.peerPort != srcPort || !s.peerIP.Equal(srcIP) {
				continue
			}
			if !s.wildcard {
				if !s.bindIP.Equal(dstIP) {
					continue
				}
				score = 4
			} else {
				score = 3
			}
		} else {
			if !s.wildcard {
				if !s.bindIP.Equal(dstIP) {
					continue
				}
				score = 2
			} else {
				score = 1
			}
		}
		if score > bestScore {
			bestScore = score
			best = best[:0]
			best = append(best, s)
		} else if score == bestScore {
			best = append(best, s)
		}
	}
	if len(best) == 0 {
		return nil
	}
	if len(best) == 1 {
		return best[0]
	}
	sort.Slice(best, func(i, j int) bool { return best[i].regSeq < best[j].regSeq })
	idx := dstUDPReusePortHash(dstCurrentSeed(), h.host, srcIP, srcPort, dstIP, dstPort) % uint64(len(best))
	return best[idx]
}

// advanceLocked matures all in-flight datagrams on host h with deliverAt <= now
// in deterministic (deliverAt, seq) order, returning any cross-host ICMP Port
// Unreachable refusals and deferred partition-drop causal events (flushed after
// releasing h.mu so h.mu and dstUDP.mu never nest). Caller holds h.mu.
func (h *dstUDPNet) advanceLocked(now int64) ([]dstUDPRefusal, []dstUDPDropEvent) {
	var refusals []dstUDPRefusal
	var drops []dstUDPDropEvent
	for len(h.inflight) > 0 && h.inflight[0].deliverAt <= now {
		pkt := h.inflight.pop()
		if pkt.srcHost != h.host {
			// Check whether a directional blackhole cut started before this
			// packet reached the destination host.
			if cs, cut, bh := dstPartCutStartDir(pkt.srcHost, h.host); cut && bh && pkt.deliverAt > cs {
				drops = append(drops, dstUDPDropEvent{
					kind:    dstUDPDropPartition,
					srcHost: pkt.srcHost,
					srcPort: uint32(pkt.srcPort),
					dstHost: h.host,
					dstPort: uint32(pkt.dstPort),
				})
				continue
			}
		}
		target := h.lookupLocked(pkt.srcIP, pkt.srcPort, pkt.dstIP, pkt.dstPort)
		if target != nil {
			charge := dstUDPPacketCharge(len(pkt.payload))
			if target.readyBytes+charge <= target.rcvBufCap {
				target.ready = append(target.ready, pkt)
				target.readyBytes += charge
				target.bumpWakeLocked()
			} else if !target.rcvDropLogged {
				target.rcvDropLogged = true
				dstKernelSectionEnter()
				_, proc, pid := dstCurrentDiagIdentity()
				dstRecordCausalEvent(dstCausalUDPDrop, dstUDPDropRcvBuf, pkt.srcHost, h.host, proc, pid, int64(pkt.srcPort), int64(pkt.dstPort), 0, "")
				dstKernelSectionExit()
			}
			continue
		}
		// Unbound destination port: only a connected UDP socket talking to a
		// live, declared host receives ICMP Port Unreachable (ECONNREFUSED).
		if pkt.senderSock == nil {
			continue
		}
		if pkt.srcHost != h.host && (!dstHostDeclaredQ(h.host) || dstHostDead(h.host)) {
			continue
		}
		var retDelay int64
		if pkt.srcHost != h.host {
			if _, cut, bh := dstPartCutStartDir(h.host, pkt.srcHost); cut && bh {
				continue
			}
			retLat, retJit, _, retLoss, _, _ := dstUDPLinkParamsForDir(h.host, pkt.srcHost)
			if retLoss > 0 && dstFaultProb(retLoss) {
				continue
			}
			retDelay = dstLinkDelay(retLat, retJit)
		}
		ref := dstUDPRefusal{
			sender:    pkt.senderSock,
			peerHost:  h.host,
			peerPort:  pkt.dstPort,
			sendAt:    pkt.deliverAt,
			deliverAt: dstDelayAdd(pkt.deliverAt, retDelay),
		}
		dstKernelSectionEnter()
		dstKernelRaceReleaseMerge(unsafe.Pointer(&ref.raceSync))
		dstKernelSectionExit()
		refusals = append(refusals, ref)
	}
	return refusals, drops
}

func dstUDPPostRefusals(refusals []dstUDPRefusal) {
	for i := range refusals {
		ref := refusals[i]
		sh := ref.sender.hostNet
		sh.mu.Lock()
		if !ref.sender.closed {
			sh.seq++
			ref.seq = sh.seq
			ref.sender.errInflight.push(ref)
			ref.sender.bumpWakeLocked()
		}
		sh.mu.Unlock()
	}
}

func dstUDPAdvanceHost(host uint32, now int64) {
	h := dstUDPNetForHost(host)
	h.mu.Lock()
	refs, drops := h.advanceLocked(now)
	h.mu.Unlock()
	if len(drops) > 0 {
		dstUDPRecordDrops(drops)
	}
	if len(refs) > 0 {
		dstUDPPostRefusals(refs)
	}
}

// advanceRefusalsLocked pops all in-flight ICMP refusals on connected socket s
// with deliverAt <= now in min-heap order, discarding any whose return leg was
// blackhole-cut mid-flight and overwriting the single kernel sk_err slot
// (matching Linux __udp4_lib_err -> sk->sk_err). Caller holds s.hostNet.mu.
func (s *dstUDPSock) advanceRefusalsLocked(now int64) {
	for len(s.errInflight) > 0 && s.errInflight[0].deliverAt <= now {
		ref := s.errInflight.pop()
		if ref.peerHost != s.host {
			if cs, cut, bh := dstPartCutStartDir(ref.peerHost, s.host); cut && bh && ref.deliverAt > cs {
				continue
			}
		}
		dstKernelSectionEnter()
		dstKernelRaceAcquire(unsafe.Pointer(&ref.raceSync))
		dstKernelRaceReleaseMerge(unsafe.Pointer(&s.skErrSync))
		dstKernelSectionExit()
		s.skErr = syscall.ECONNREFUSED
		s.skErrPeerHost = ref.peerHost
		s.skErrPeerPort = ref.peerPort
		s.bumpWakeLocked()
	}
}

// consumeSkErrLocked atomically clears and returns the pending one-shot sk_err
// if any refusal has matured by now (Linux sock_error xchg(&sk->sk_err, 0)).
// Caller holds s.hostNet.mu.
func (s *dstUDPSock) consumeSkErrLocked(now int64) (syscall.Errno, bool) {
	s.advanceRefusalsLocked(now)
	if s.skErr == 0 {
		return 0, false
	}
	errno := s.skErr
	peerHost := s.skErrPeerHost
	peerPort := s.skErrPeerPort
	s.skErr = 0
	dstKernelSectionEnter()
	dstKernelRaceAcquire(unsafe.Pointer(&s.skErrSync))
	_, proc, pid := dstCurrentDiagIdentity()
	dstRecordCausalEvent(dstCausalUDPRefused, 0, s.host, peerHost, proc, pid, int64(s.port), int64(peerPort), 0, "")
	dstKernelSectionExit()
	return errno, true
}

func (s *dstUDPSock) nextErrAtLocked() int64 {
	if len(s.errInflight) == 0 {
		return 0
	}
	return s.errInflight[0].deliverAt
}

// dstUDPOnPartitionOp matures already-arrived datagrams and refusals and drops
// mid-flight datagrams and mid-flight ICMP refusals across newly blackholed
// directions, matching faults.md.
func dstUDPOnPartitionOp(op, a, b uint32) {
	dstUDP.mu.Lock()
	dstUDPRollLocked()
	var hosts []*dstUDPNet
	for _, h := range dstUDP.byHost {
		hosts = append(hosts, h)
	}
	switch op {
	case dstPartOpPartition, dstPartOpPartitionRefuse, dstPartOpHeal:
		delete(dstUDP.partDiagLogged, dstDirKey(a, b))
		delete(dstUDP.partDiagLogged, dstDirKey(b, a))
	case dstPartOpPartitionOneWay:
		delete(dstUDP.partDiagLogged, dstDirKey(a, b))
	case dstPartOpIsolate, dstPartOpHealHost, dstFaultOpHostDown, dstFaultOpHostUp:
		for k := range dstUDP.partDiagLogged {
			if uint32(k>>32) == a || uint32(k) == a {
				delete(dstUDP.partDiagLogged, k)
			}
		}
	}
	dstUDP.mu.Unlock()

	sort.Slice(hosts, func(i, j int) bool { return hosts[i].host < hosts[j].host })
	now := dstBaseNanos()
	for _, h := range hosts {
		h.mu.Lock()
		refs, drops := h.advanceLocked(now)
		// Prune cross-host in-flight packets whose direction into h is now
		// blackholed (deliverAt > now >= cutStart).
		if len(h.inflight) > 0 {
			var kept dstUDPPacketHeap
			for len(h.inflight) > 0 {
				pkt := h.inflight.pop()
				if pkt.srcHost != h.host {
					if _, cut, bh := dstPartCutStartDir(pkt.srcHost, h.host); cut && bh {
						drops = append(drops, dstUDPDropEvent{
							kind:    dstUDPDropPartition,
							srcHost: pkt.srcHost,
							srcPort: uint32(pkt.srcPort),
							dstHost: h.host,
							dstPort: uint32(pkt.dstPort),
						})
						continue
					}
				}
				kept.push(pkt)
			}
			h.inflight = kept
		}
		h.bumpWakeLocked()
		h.mu.Unlock()
		if len(drops) > 0 {
			dstUDPRecordDrops(drops)
		}
		if len(refs) > 0 {
			dstUDPPostRefusals(refs)
		}
	}
	// Mature pre-cut refusals and prune mid-flight refusals across blackholed
	// return legs so a Partition+HealLink flap never resurrects dropped ICMP.
	for _, h := range hosts {
		h.mu.Lock()
		for _, socks := range h.byPort {
			for _, s := range socks {
				if s.closed || len(s.errInflight) == 0 {
					continue
				}
				s.advanceRefusalsLocked(now)
				if len(s.errInflight) > 0 {
					var kept dstUDPRefusalHeap
					for len(s.errInflight) > 0 {
						ref := s.errInflight.pop()
						if ref.peerHost != s.host {
							if _, cut, bh := dstPartCutStartDir(ref.peerHost, s.host); cut && bh {
								continue
							}
						}
						kept.push(ref)
					}
					s.errInflight = kept
				}
			}
		}
		h.mu.Unlock()
	}
}

func (s *dstUDPSock) closeInternal(emitCausal bool) error {
	h := s.hostNet
	h.mu.Lock()
	if s.closed {
		dstKernelSectionEnter()
		dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
		dstKernelSectionExit()
		h.mu.Unlock()
		return errClosed
	}
	dstKernelSectionEnter()
	dstKernelRaceReleaseMerge(unsafe.Pointer(&s.closeSync))
	s.staleClosed.Store(true)
	dstKernelSectionExit()
	s.closed = true
	s.ready = nil
	s.readyBytes = 0
	s.errInflight = nil
	s.skErr = 0
	if s.rdTimer != nil {
		s.rdTimer.Stop()
		s.rdTimer = nil
	}
	if s.wrTimer != nil {
		s.wrTimer.Stop()
		s.wrTimer = nil
	}
	socks := h.byPort[s.port]
	live := socks[:0]
	for _, cur := range socks {
		if cur != s && !cur.closed {
			live = append(live, cur)
		}
	}
	if len(live) == 0 {
		delete(h.byPort, s.port)
	} else {
		h.byPort[s.port] = live
	}
	s.bumpWakeLocked()
	h.bumpWakeLocked()
	if emitCausal {
		dstKernelSectionEnter()
		_, proc, pid := dstCurrentDiagIdentity()
		dstRecordCausalEvent(dstCausalUDPClose, 0, s.host, 0, proc, pid, int64(s.port), 0, 0, s.laddrStr)
		dstKernelSectionExit()
	}
	h.mu.Unlock()
	dstCoarseRecord(uintptr(unsafe.Pointer(&s.closeSync)), true)
	dstCoarseRecord(dstCoarseNetHostID(s.host), true)
	dstSockFDFree(s.sockFD, s.opts)
	return nil
}

// dstUDPCloseProc closes all UDP sockets owned by process proc in deterministic
// registration order (process exit and CrashProcess teardown).
func dstUDPCloseProc(proc uint32) {
	dstKernelSectionEnter()
	defer dstKernelSectionExit()
	dstUDP.mu.Lock()
	dstUDPRollLocked()
	var hosts []*dstUDPNet
	for _, h := range dstUDP.byHost {
		hosts = append(hosts, h)
	}
	dstUDP.mu.Unlock()
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].host < hosts[j].host })

	for _, h := range hosts {
		var victims []*dstUDPSock
		h.mu.Lock()
		for _, socks := range h.byPort {
			for _, s := range socks {
				if !s.closed && s.proc == proc {
					victims = append(victims, s)
				}
			}
		}
		h.mu.Unlock()
		sort.Slice(victims, func(i, j int) bool { return victims[i].regSeq < victims[j].regSeq })
		for _, s := range victims {
			_ = s.closeInternal(false)
		}
	}
}

// dstUDPCrashHost tears down all UDP sockets and in-flight datagrams on crashed
// host h (power loss destroys kernel socket state and receive buffers silently).
func dstUDPCrashHost(host uint32) {
	dstKernelSectionEnter()
	defer dstKernelSectionExit()
	dstUDP.mu.Lock()
	dstUDPRollLocked()
	h := dstUDP.byHost[host]
	dstUDP.mu.Unlock()
	if h == nil {
		return
	}
	var victims []*dstUDPSock
	h.mu.Lock()
	h.nextListenPort = 0
	h.nextDialPort = 0
	h.inflight = nil
	clear(h.linkFreeAt)
	clear(h.flowLastDeliverAt)
	for _, socks := range h.byPort {
		for _, s := range socks {
			if !s.closed {
				victims = append(victims, s)
			}
		}
	}
	h.mu.Unlock()
	sort.Slice(victims, func(i, j int) bool { return victims[i].regSeq < victims[j].regSeq })
	for _, s := range victims {
		_ = s.closeInternal(false)
	}
}

func dstUDPHasProcResources(proc uint32) bool {
	dstUDP.mu.Lock()
	dstUDPRollLocked()
	var hosts []*dstUDPNet
	for _, h := range dstUDP.byHost {
		hosts = append(hosts, h)
	}
	dstUDP.mu.Unlock()
	for _, h := range hosts {
		h.mu.Lock()
		for _, socks := range h.byPort {
			for _, s := range socks {
				if !s.closed && s.proc == proc {
					h.mu.Unlock()
					return true
				}
			}
		}
		h.mu.Unlock()
	}
	return false
}

func dstAllocUDPPort(h *dstUDPNet, dial bool, family int, ipv6only, wildcard bool, bindIP IP, reuseAddr, reusePort bool) (int, error) {
	start, end, exhaustedErr := dstListenEphemeralStart, 65535, syscall.EADDRINUSE
	if dial {
		start, end, exhaustedErr = dstDialEphemeralStart, dstDialEphemeralEnd, syscall.EADDRNOTAVAIL
	}
	span := end - start + 1
	h.mu.Lock()
	defer h.mu.Unlock()
	counter := &h.nextListenPort
	if dial {
		counter = &h.nextDialPort
	}
	for tried := 0; tried < span; tried++ {
		p := *counter
		if p == 0 {
			p = start
		}
		*counter = p + 1
		if *counter > end {
			*counter = start
		}
		if !h.hasBindConflictLocked(family, ipv6only, wildcard, bindIP, p, reuseAddr, reusePort) {
			return p, nil
		}
	}
	return 0, exhaustedErr
}

func dstResolveUDPStringAddr(op, network, address string, mode string) (*UDPAddr, error) {
	if address == "" {
		if mode == "dial" {
			return nil, &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: errMissingAddress}
		}
		return &UDPAddr{}, nil
	}
	host, portStr, err := SplitHostPort(address)
	if err != nil {
		return nil, &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: err}
	}
	portnum, err := dstParsePort(op, network, portStr)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return &UDPAddr{Port: portnum}, nil
	}
	hostNoZone, zone := splitHostZone(host)
	if dstIsLocalhost(hostNoZone) && zone == "" {
		if network == "udp6" {
			return &UDPAddr{IP: append(IP(nil), IPv6loopback...), Port: portnum}, nil
		}
		return &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: portnum}, nil
	}
	ip := ParseIP(hostNoZone)
	if ip == nil {
		return nil, &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: &AddrError{Err: "DNS lookup unsupported under deterministic simulation", Addr: host}}
	}
	switch network {
	case "udp4":
		if ip.To4() == nil {
			return nil, &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: &AddrError{Err: errNoSuitableAddress.Error(), Addr: host}}
		}
		ip = ip.To4()
	case "udp6":
		if ip.To4() != nil || ip.To16() == nil {
			return nil, &OpError{Op: op, Net: network, Source: nil, Addr: nil, Err: &AddrError{Err: errNoSuitableAddress.Error(), Addr: host}}
		}
	}
	return &UDPAddr{IP: ip, Port: portnum, Zone: zone}, nil
}

// dstListenStreamOnUDP reproduces host Go's Listen("udp*", address) rejection
// (*OpError wrapping *AddrError{Err: "unexpected address type"}).
func dstListenStreamOnUDP(network, address string) error {
	la, err := dstResolveUDPStringAddr("listen", network, address, "listen")
	if err != nil {
		return err
	}
	return &OpError{Op: "listen", Net: network, Source: nil, Addr: la, Err: &AddrError{Err: "unexpected address type", Addr: address}}
}

func dstListenPacket(ctx context.Context, lc *ListenConfig, network, address string) (PacketConn, error) {
	if !dstUDPNetwork(network) {
		return nil, dstUnsupportedNetwork("listen", network)
	}
	if err := ctx.Err(); err != nil {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: nil, Err: mapErr(err)}
	}
	la, err := dstResolveUDPStringAddr("listen", network, address, "listen")
	if err != nil {
		return nil, err
	}
	c, err := dstBindUDP(ctx, lc, network, la)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func dstListenUDP(network string, laddr *UDPAddr) (*UDPConn, error) {
	var lc ListenConfig
	return dstBindUDP(context.Background(), &lc, network, laddr)
}

func dstBindUDP(ctx context.Context, lc *ListenConfig, network string, laddr *UDPAddr) (retConn *UDPConn, retErr error) {
	if laddr == nil {
		laddr = &UDPAddr{}
	}
	if laddr.Port < 0 || laddr.Port > 65535 {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: laddr.opAddr(), Err: &AddrError{Err: "invalid port", Addr: strconv.Itoa(laddr.Port)}}
	}
	if laddr.IP != nil && laddr.IP.IsMulticast() {
		return nil, dstUnsupportedNetOption("listen", network, nil, laddr.opAddr(), "UDP multicast")
	}
	if len(laddr.IP) != 0 && laddr.IP.To4() == nil && laddr.IP.To16() == nil {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: laddr.opAddr(), Err: &AddrError{Err: "invalid IP address", Addr: laddr.IP.String()}}
	}

	var family int
	var ipv6only bool
	wildcard := laddr.isWildcard()
	switch network {
	case "udp4":
		if !wildcard && laddr.IP.To4() == nil {
			return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: laddr.opAddr(), Err: &AddrError{Err: "non-IPv4 address", Addr: laddr.IP.String()}}
		}
		family = syscall.AF_INET
		ipv6only = false
	case "udp6":
		if !wildcard && laddr.IP.To4() != nil {
			return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: laddr.opAddr(), Err: &AddrError{Err: "non-IPv6 address", Addr: laddr.IP.String()}}
		}
		family = syscall.AF_INET6
		ipv6only = true
	default: // "udp"
		if wildcard {
			family = syscall.AF_INET6
			ipv6only = false
		} else if laddr.IP.To4() != nil {
			family = syscall.AF_INET
			ipv6only = false
		} else {
			family = syscall.AF_INET6
			ipv6only = false
		}
	}

	host, proc := dstNetCurrentNode()
	dstCoarseDep(dstCoarseNetHostID(host), true)
	if !wildcard && !dstLocalIPOwnedByHost(host, laddr.IP) {
		// Still run Control before bind so callback order matches sock_posix.go,
		// then fail bind with EADDRNOTAVAIL.
	}

	var reportReqIP IP
	var bindIP IP
	if wildcard {
		if family == syscall.AF_INET {
			reportReqIP = IPv4zero
		} else {
			reportReqIP = IPv6zero
		}
	} else {
		bindIP = dstNormIP(laddr.IP)
		reportReqIP = bindIP
	}

	h := dstUDPNetForHost(host)
	sock := &dstUDPSock{
		hostNet:   h,
		epoch:     dstNetEpoch(),
		host:      host,
		proc:      proc,
		network:   network,
		family:    family,
		ipv6only:  ipv6only,
		wildcard:  wildcard,
		bindIP:    bindIP,
		rcvBufCap: dstUDPRcvBufDefault,
	}
	opts := newDstUDPSockOpts(func(capBytes int) {
		h.mu.Lock()
		sock.rcvBufCap = capBytes
		h.mu.Unlock()
	})
	sock.opts = opts
	vfd, okFD := dstSockFDAlloc(opts)
	if !okFD {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: laddr.opAddr(), Err: os.NewSyscallError("socket", syscall.EMFILE)}
	}
	sock.sockFD = vfd
	defer func() {
		if retErr != nil {
			dstSockFDFree(vfd, opts)
		}
	}()

	fd := newFD(-1, family, syscall.SOCK_DGRAM, network)
	dstAttachUDPSock(fd, sock)

	if lc != nil && lc.Control != nil {
		if err := dstRunControl(ctx, nil, lc.Control, fd.ctrlNetwork(), laddr.String(), vfd); err != nil {
			return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: laddr.opAddr(), Err: err}
		}
	}

	if !wildcard && !dstLocalIPOwnedByHost(host, laddr.IP) {
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: laddr.opAddr(), Err: os.NewSyscallError("bind", syscall.EADDRNOTAVAIL)}
	}

	reuseAddr := opts.isReuseAddr()
	reusePort := opts.isReusePort()
	port := laddr.Port
	if port == 0 {
		var err error
		port, err = dstAllocUDPPort(h, false, family, ipv6only, wildcard, bindIP, reuseAddr, reusePort)
		if err != nil {
			return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: laddr.opAddr(), Err: os.NewSyscallError("bind", err)}
		}
	}

	h.mu.Lock()
	if laddr.Port != 0 && h.hasBindConflictLocked(family, ipv6only, wildcard, bindIP, port, reuseAddr, reusePort) {
		h.mu.Unlock()
		return nil, &OpError{Op: "listen", Net: network, Source: nil, Addr: laddr.opAddr(), Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	}
	h.seq++
	sock.regSeq = h.seq
	sock.port = port
	boundAddr := &UDPAddr{IP: append(IP(nil), reportReqIP...), Port: port, Zone: laddr.Zone}
	sock.laddr = boundAddr
	sock.laddrStr = boundAddr.String()
	h.byPort[port] = append(h.byPort[port], sock)
	h.bumpWakeLocked()
	h.mu.Unlock()

	fd.laddr = boundAddr
	fd.raddr = nil
	dstRecordCausalCaller(dstCausalUDPBind, 0, 0, int64(port), 0, 0, sock.laddrStr)
	return newUDPConn(fd), nil
}

func dstDialUDPString(ctx context.Context, d *Dialer, network, address string) (Conn, error) {
	var localSource Addr
	if d != nil {
		localSource = d.LocalAddr
	}
	if err := ctx.Err(); err != nil {
		return nil, &OpError{Op: "dial", Net: network, Source: localSource, Addr: nil, Err: mapErr(err)}
	}
	ra, err := dstResolveUDPStringAddr("dial", network, address, "dial")
	if err != nil {
		return nil, err
	}
	var la *UDPAddr
	if d != nil && d.LocalAddr != nil {
		var ok bool
		la, ok = d.LocalAddr.(*UDPAddr)
		if !ok {
			return nil, &OpError{Op: "dial", Net: network, Source: d.LocalAddr, Addr: ra.opAddr(), Err: &AddrError{Err: "mismatched local address type", Addr: d.LocalAddr.String()}}
		}
	}
	if trace, _ := ctx.Value(nettrace.TraceKey{}).(*nettrace.Trace); trace != nil {
		raStr := ra.String()
		if trace.ConnectStart != nil {
			trace.ConnectStart(network, raStr)
		}
		if trace.ConnectDone != nil {
			c, err := dstDialUDP(ctx, d, network, la, ra)
			trace.ConnectDone(network, raStr, err)
			if err != nil {
				return nil, err
			}
			return c, nil
		}
	}
	c, err := dstDialUDP(ctx, d, network, la, ra)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func dstDialUDP(ctx context.Context, dialer *Dialer, network string, laddr, raddr *UDPAddr) (retConn *UDPConn, retErr error) {
	if raddr == nil {
		return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: nil, Err: errMissingAddress}
	}
	if err := ctx.Err(); err != nil {
		return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: mapErr(err)}
	}
	if raddr.Port < 0 || raddr.Port > 65535 {
		return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: &AddrError{Err: "invalid port", Addr: strconv.Itoa(raddr.Port)}}
	}
	if laddr != nil && (laddr.Port < 0 || laddr.Port > 65535) {
		return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: &AddrError{Err: "invalid port", Addr: strconv.Itoa(laddr.Port)}}
	}
	if len(raddr.IP) != 0 && raddr.IP.To4() == nil && raddr.IP.To16() == nil {
		return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: &AddrError{Err: "invalid IP address", Addr: raddr.IP.String()}}
	}
	if laddr != nil && len(laddr.IP) != 0 && laddr.IP.To4() == nil && laddr.IP.To16() == nil {
		return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: &AddrError{Err: "invalid IP address", Addr: laddr.IP.String()}}
	}
	if (raddr.IP != nil && (raddr.IP.IsMulticast() || raddr.IP.Equal(IPv4bcast))) ||
		(laddr != nil && laddr.IP != nil && laddr.IP.IsMulticast()) {
		return nil, dstUnsupportedNetOption("dial", network, laddr.opAddr(), raddr.opAddr(), "UDP multicast/broadcast")
	}

	family, ipv6only := favoriteAddrFamily(network, laddr, raddr, "dial")
	switch family {
	case syscall.AF_INET:
		if !raddr.isWildcard() && raddr.IP.To4() == nil {
			return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: &AddrError{Err: "non-IPv4 address", Addr: raddr.IP.String()}}
		}
		if laddr != nil && !laddr.isWildcard() && laddr.IP.To4() == nil {
			return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: &AddrError{Err: "non-IPv4 address", Addr: laddr.IP.String()}}
		}
	case syscall.AF_INET6:
		if ipv6only {
			if (!raddr.isWildcard() && raddr.IP.To4() != nil) || (raddr.IP != nil && raddr.IP.Equal(IPv4zero)) {
				return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}
			}
			if laddr != nil && !laddr.isWildcard() && laddr.IP.To4() != nil {
				return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: &AddrError{Err: "non-IPv6 address", Addr: laddr.IP.String()}}
			}
		}
	}

	var peerIP IP
	if raddr.isWildcard() {
		if family == syscall.AF_INET || (raddr.IP != nil && raddr.IP.Equal(IPv4zero)) {
			peerIP = IPv4(127, 0, 0, 1)
		} else {
			peerIP = dstNormIP(IPv6loopback)
		}
	} else {
		peerIP = dstNormIP(raddr.IP)
	}

	host, proc := dstNetCurrentNode()
	dstCoarseDep(dstCoarseNetHostID(host), true)
	peerHost := host
	if rh, ok := dstHostForRoutableIP(peerIP); ok {
		peerHost = rh
	}

	var localIP IP
	localZone := ""
	if laddr != nil {
		localZone = laddr.Zone
	}
	if laddr != nil && !laddr.isWildcard() {
		localIP = dstNormIP(laddr.IP)
	} else if peerIP.To4() != nil {
		if _, routable := dstHostForRoutableIP(peerIP); routable || !peerIP.IsLoopback() {
			localIP = dstNormIP(dstHostRoutableIP(host))
		} else {
			localIP = IPv4(127, 0, 0, 1)
		}
	} else {
		localIP = dstNormIP(IPv6loopback)
	}

	h := dstUDPNetForHost(host)
	sock := &dstUDPSock{
		hostNet:   h,
		epoch:     dstNetEpoch(),
		host:      host,
		proc:      proc,
		network:   network,
		family:    family,
		ipv6only:  ipv6only,
		wildcard:  false,
		bindIP:    localIP,
		connected: true,
		peerHost:  peerHost,
		peerIP:    peerIP,
		peerPort:  raddr.Port,
		rcvBufCap: dstUDPRcvBufDefault,
	}
	opts := newDstUDPSockOpts(func(capBytes int) {
		h.mu.Lock()
		sock.rcvBufCap = capBytes
		h.mu.Unlock()
	})
	sock.opts = opts
	vfd, okFD := dstSockFDAlloc(opts)
	if !okFD {
		return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: os.NewSyscallError("socket", syscall.EMFILE)}
	}
	sock.sockFD = vfd
	defer func() {
		if retErr != nil {
			dstSockFDFree(vfd, opts)
		}
	}()

	fd := newFD(-1, family, syscall.SOCK_DGRAM, network)
	dstAttachUDPSock(fd, sock)

	if dialer != nil && (dialer.ControlContext != nil || dialer.Control != nil) {
		if err := dstRunControl(ctx, dialer.ControlContext, dialer.Control, fd.ctrlNetwork(), raddr.String(), vfd); err != nil {
			return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: err}
		}
	}

	if laddr != nil && !laddr.isWildcard() {
		if !dstLocalIPOwnedByHost(host, laddr.IP) {
			return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: os.NewSyscallError("bind", syscall.EADDRNOTAVAIL)}
		}
		if (localIP.To4() != nil) != (peerIP.To4() != nil) {
			return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: os.NewSyscallError("connect", syscall.EINVAL)}
		}
		if localIP.IsLoopback() && peerHost != host {
			return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: os.NewSyscallError("connect", syscall.EINVAL)}
		}
	}

	reuseAddr := opts.isReuseAddr()
	reusePort := opts.isReusePort()
	localPort := 0
	wildcardCheck := false
	checkIP := localIP
	if laddr != nil && laddr.Port != 0 {
		localPort = laddr.Port
		if laddr.isWildcard() {
			wildcardCheck = true
			checkIP = nil
		}
	} else {
		var err error
		localPort, err = dstAllocUDPPort(h, true, family, ipv6only, false, localIP, reuseAddr, reusePort)
		if err != nil {
			return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: os.NewSyscallError("bind", err)}
		}
	}

	h.mu.Lock()
	if laddr != nil && laddr.Port != 0 && h.hasBindConflictLocked(family, ipv6only, wildcardCheck, checkIP, localPort, reuseAddr, reusePort) {
		h.mu.Unlock()
		return nil, &OpError{Op: "dial", Net: network, Source: laddr.opAddr(), Addr: raddr.opAddr(), Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	}
	h.seq++
	sock.regSeq = h.seq
	sock.port = localPort
	boundLocal := &UDPAddr{IP: append(IP(nil), localIP...), Port: localPort, Zone: localZone}
	boundRemote := &UDPAddr{IP: append(IP(nil), peerIP...), Port: raddr.Port, Zone: raddr.Zone}
	sock.laddr = boundLocal
	sock.laddrStr = boundLocal.String()
	sock.raddr = boundRemote
	h.byPort[localPort] = append(h.byPort[localPort], sock)
	h.bumpWakeLocked()
	h.mu.Unlock()

	fd.isConnected = true
	fd.laddr = boundLocal
	fd.raddr = boundRemote
	dstRecordCausalCaller(dstCausalUDPBind, 0, 0, int64(localPort), 0, 0, sock.laddrStr)
	return newUDPConn(fd), nil
}

func (s *dstUDPSock) armDeadlineLocked(isRead bool, isZero bool, dur time.Duration) {
	var old **time.Timer
	var exp *bool
	if isRead {
		old = &s.rdTimer
		exp = &s.rdExpired
	} else {
		old = &s.wrTimer
		exp = &s.wrExpired
	}
	if *old != nil {
		(*old).Stop()
		*old = nil
	}
	if isZero {
		*exp = false
		return
	}
	if dur <= 0 {
		*exp = true
		return
	}
	*exp = false
	dstKernelSectionEnter()
	prevHost, prevProc := dstSetNode(s.host, 0)
	tm := time.NewTimer(dur)
	dstSetNode(prevHost, prevProc)
	dstKernelSectionExit()
	*old = tm
}

func (s *dstUDPSock) checkDeadlineLocked(isRead bool) bool {
	var tm **time.Timer
	var exp *bool
	if isRead {
		tm = &s.rdTimer
		exp = &s.rdExpired
	} else {
		tm = &s.wrTimer
		exp = &s.wrExpired
	}
	if *exp {
		return true
	}
	if *tm == nil {
		return false
	}
	fired := false
	dstKernelSectionEnter()
	select {
	case <-(*tm).C:
		fired = true
	default:
	}
	dstKernelSectionExit()
	if fired {
		*tm = nil
		*exp = true
		s.bumpWakeLocked()
		return true
	}
	return false
}

func (s *dstUDPSock) recvDatagram(p []byte) (n int, truncated bool, srcIP IP, srcPort int, err error) {
	if s.stale() {
		return 0, false, nil, 0, errClosed
	}
	dstCoarseDep(uintptr(unsafe.Pointer(&s.closeSync)), false)
	dstCoarseDep(dstCoarseNetHostID(s.host), true)
	h := s.hostNet

	for {
		now := dstBaseNanos()
		if s.connected && s.peerHost != s.host {
			dstUDPAdvanceHost(s.peerHost, now)
		}
		h.mu.Lock()
		if s.closed {
			dstKernelSectionEnter()
			dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
			dstKernelSectionExit()
			h.mu.Unlock()
			return 0, false, nil, 0, errClosed
		}
		refs, drops := h.advanceLocked(now)
		if len(drops) > 0 || len(refs) > 0 {
			h.mu.Unlock()
			if len(drops) > 0 {
				dstUDPRecordDrops(drops)
			}
			if len(refs) > 0 {
				dstUDPPostRefusals(refs)
			}
			continue
		}
		if s.connected {
			if errno, ok := s.consumeSkErrLocked(now); ok {
				h.mu.Unlock()
				return 0, false, nil, 0, errno
			}
		}
		if len(s.ready) > 0 {
			pkt := s.ready[0]
			s.ready[0] = nil
			s.ready = s.ready[1:]
			s.readyBytes -= dstUDPPacketCharge(len(pkt.payload))
			s.rcvDropLogged = false
			dstKernelSectionEnter()
			dstKernelRaceAcquire(unsafe.Pointer(&pkt.raceSync))
			dstKernelSectionExit()
			h.mu.Unlock()
			n = copy(p, pkt.payload)
			return n, len(pkt.payload) > len(p), pkt.srcIP, pkt.srcPort, nil
		}
		if s.checkDeadlineLocked(true) {
			h.mu.Unlock()
			return 0, false, nil, 0, os.ErrDeadlineExceeded
		}

		nextWake := int64(math.MaxInt64)
		if len(h.inflight) > 0 && h.inflight[0].deliverAt < nextWake {
			nextWake = h.inflight[0].deliverAt
		}
		if errAt := s.nextErrAtLocked(); errAt > 0 && errAt < nextWake {
			nextWake = errAt
		}
		var rdTimer *time.Timer
		var rdTimerC <-chan time.Time
		if s.rdTimer != nil {
			rdTimer = s.rdTimer
			rdTimerC = rdTimer.C
		}
		sockWake := s.wakeChLocked()
		hostWake := h.wakeChLocked()
		h.mu.Unlock()

		var peerWake <-chan struct{}
		if s.connected && s.peerHost != s.host {
			ph := dstUDPNetForHost(s.peerHost)
			ph.mu.Lock()
			if len(ph.inflight) > 0 && ph.inflight[0].deliverAt < nextWake {
				nextWake = ph.inflight[0].deliverAt
			}
			peerWake = ph.wakeChLocked()
			ph.mu.Unlock()
		}
		partWake := dstPartWakeCh()

		firedDeadline := false
		if nextWake < math.MaxInt64 {
			waitNs := nextWake - now
			if waitNs <= 0 {
				continue
			}
			timer := dstNewBaseTimer(time.Duration(waitNs))
			select {
			case <-timer.C:
			case <-rdTimerC:
				timer.Stop()
				firedDeadline = true
			case <-sockWake:
				timer.Stop()
			case <-hostWake:
				timer.Stop()
			case <-peerWake:
				timer.Stop()
			case <-partWake:
				timer.Stop()
			}
		} else {
			select {
			case <-rdTimerC:
				firedDeadline = true
			case <-sockWake:
			case <-hostWake:
			case <-peerWake:
			case <-partWake:
			}
		}
		if firedDeadline {
			h.mu.Lock()
			if s.rdTimer == rdTimer {
				s.rdTimer = nil
				s.rdExpired = true
				s.bumpWakeLocked()
			}
			h.mu.Unlock()
		}
	}
}

func (s *dstUDPSock) sendDatagram(p []byte, hasExplicitDst bool, explicitIP IP, explicitPort int) (int, error) {
	if s.stale() {
		return 0, errClosed
	}
	dstCoarseDep(uintptr(unsafe.Pointer(&s.closeSync)), false)
	dstCoarseDep(dstCoarseNetHostID(s.host), false)
	now := dstBaseNanos()
	if s.connected && s.peerHost != s.host {
		dstUDPAdvanceHost(s.peerHost, now)
	}

	h := s.hostNet
	h.mu.Lock()
	if s.closed {
		dstKernelSectionEnter()
		dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
		dstKernelSectionExit()
		h.mu.Unlock()
		return 0, errClosed
	}
	if refs, drops := h.advanceLocked(now); len(drops) > 0 || len(refs) > 0 {
		h.mu.Unlock()
		if len(drops) > 0 {
			dstUDPRecordDrops(drops)
		}
		if len(refs) > 0 {
			dstUDPPostRefusals(refs)
		}
		h.mu.Lock()
		if s.closed {
			dstKernelSectionEnter()
			dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
			dstKernelSectionExit()
			h.mu.Unlock()
			return 0, errClosed
		}
	}
	if s.connected {
		if errno, ok := s.consumeSkErrLocked(now); ok {
			h.mu.Unlock()
			return 0, errno
		}
	}
	if s.checkDeadlineLocked(false) {
		h.mu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	h.mu.Unlock()

	var dstIP IP
	var dstPort int
	var senderRef *dstUDPSock
	if s.connected {
		if hasExplicitDst {
			return 0, ErrWriteToConnected
		}
		dstIP = s.peerIP
		dstPort = s.peerPort
		senderRef = s
	} else {
		if !hasExplicitDst {
			return 0, syscall.EDESTADDRREQ
		}
		if explicitPort <= 0 || explicitPort > 65535 {
			return 0, syscall.EINVAL
		}
		if explicitIP == nil || explicitIP.IsUnspecified() {
			if s.family == syscall.AF_INET || (explicitIP != nil && explicitIP.Equal(IPv4zero)) {
				dstIP = IPv4(127, 0, 0, 1)
			} else {
				dstIP = dstNormIP(IPv6loopback)
			}
		} else {
			dstIP = dstNormIP(explicitIP)
		}
		dstPort = explicitPort
	}

	if dstIP == nil {
		return 0, syscall.EINVAL
	}
	if dstIP.IsMulticast() || dstIP.Equal(IPv4bcast) {
		return 0, errors.New("UDP multicast/broadcast unsupported under deterministic simulation")
	}
	if s.ipv6only && dstIP.To4() != nil {
		return 0, syscall.ENETUNREACH
	}
	if s.family == syscall.AF_INET && dstIP.To4() == nil {
		return 0, syscall.EAFNOSUPPORT
	}
	maxPayload := dstUDPMaxIPv6Payload
	if dstIP.To4() != nil {
		maxPayload = dstUDPMaxIPv4Payload
	}
	if len(p) > maxPayload {
		return 0, syscall.EMSGSIZE
	}

	targetHost := s.host
	targetRoutable := false
	if rh, ok := dstHostForRoutableIP(dstIP); ok {
		targetHost = rh
		targetRoutable = true
	}

	var srcIP IP
	if !s.wildcard {
		if (s.bindIP.To4() != nil) != (dstIP.To4() != nil) {
			return 0, syscall.EINVAL
		}
		if s.bindIP.IsLoopback() && targetHost != s.host {
			return 0, syscall.EINVAL
		}
		srcIP = s.bindIP
	} else if dstIP.To4() != nil {
		if targetRoutable || !dstIP.IsLoopback() {
			srcIP = dstNormIP(dstHostRoutableIP(s.host))
		} else {
			srcIP = IPv4(127, 0, 0, 1)
		}
	} else {
		srcIP = dstNormIP(IPv6loopback)
	}

	dstCoarseDep(dstCoarseNetHostID(s.host), false)
	dstCoarseDep(dstCoarseNetHostID(targetHost), true)

	// Unowned non-loopback non-10.x addresses (e.g. RFC 5737 TEST-NET literals)
	// have no simulated kernel: the send succeeds locally and drops silently.
	if !dstIP.IsLoopback() && !targetRoutable {
		return len(p), nil
	}

	// Undeclared or powered-off peer hosts drop outbound UDP datagrams silently
	// (no live kernel exists to answer ICMP Port Unreachable).
	if targetHost != s.host && (!dstHostDeclaredQ(targetHost) || dstHostDead(targetHost)) {
		return len(p), nil
	}

	var latencyNs, jitterNs, bandwidthBps int64
	var lossProb, dupProb float64
	var reorder bool
	if targetHost != s.host {
		_, cut, bh := dstPartCutStartDir(s.host, targetHost)
		if cut {
			if bh {
				dstUDPRecordDropOnce(dstUDPDropPartition, s.host, uint32(s.port), targetHost, uint32(dstPort))
				return len(p), nil
			}
			// Pure PartitionRefuse: active refusal on the link pends ECONNREFUSED
			// on connected UDP sockets if the return leg is not blackholed.
			if senderRef != nil {
				if _, rCut, rBH := dstPartCutStartDir(targetHost, s.host); !(rCut && rBH) {
					fLat, fJit, _, fLoss, _, _ := dstUDPLinkParamsForDir(s.host, targetHost)
					if !(fLoss > 0 && dstFaultProb(fLoss)) {
						rLat, rJit, _, rLoss, _, _ := dstUDPLinkParamsForDir(targetHost, s.host)
						if !(rLoss > 0 && dstFaultProb(rLoss)) {
							rtt := dstDelayAdd(dstLinkDelay(fLat, fJit), dstLinkDelay(rLat, rJit))
							ref := dstUDPRefusal{
								sender:    senderRef,
								peerHost:  targetHost,
								peerPort:  dstPort,
								sendAt:    now,
								deliverAt: dstDelayAdd(now, rtt),
							}
							dstKernelSectionEnter()
							dstKernelRaceReleaseMerge(unsafe.Pointer(&ref.raceSync))
							dstKernelSectionExit()
							dstUDPPostRefusals([]dstUDPRefusal{ref})
						}
					}
				}
			}
			return len(p), nil
		}
		latencyNs, jitterNs, bandwidthBps, lossProb, dupProb, reorder = dstUDPLinkParamsForDir(s.host, targetHost)
		if lossProb > 0 && dstFaultProb(lossProb) {
			dstUDPRecordDropOnce(dstUDPDropPacketLoss, s.host, uint32(s.port), targetHost, uint32(dstPort))
			return len(p), nil
		}
	}

	copies := 1
	if targetHost != s.host && dupProb > 0 && dstFaultProb(dupProb) {
		copies = 2
	}

	th := dstUDPNetForHost(targetHost)
	flowKey := makeDstUDPFlowKey(s.host, srcIP, s.port, dstIP, dstPort)
	th.mu.Lock()
	transmitEnd := now
	if targetHost != s.host {
		if freeAt := th.linkFreeAt[s.host]; freeAt > transmitEnd {
			transmitEnd = freeAt
		}
		if bandwidthBps > 0 {
			transmitEnd = dstDelayAdd(transmitEnd, dstTransmitNanos(int64(len(p)), bandwidthBps))
			th.linkFreeAt[s.host] = transmitEnd
		}
	}
	for c := 0; c < copies; c++ {
		at := transmitEnd
		if targetHost != s.host {
			at = dstDelayAdd(dstDelayAdd(transmitEnd, latencyNs), dstFaultRandN(jitterNs))
			if !reorder {
				if prev := th.flowLastDeliverAt[flowKey]; at < prev {
					at = prev
				}
				th.flowLastDeliverAt[flowKey] = at
			} else if at > th.flowLastDeliverAt[flowKey] {
				th.flowLastDeliverAt[flowKey] = at
			}
		}
		th.seq++
		pkt := &dstUDPPacket{
			srcHost:    s.host,
			srcIP:      srcIP,
			srcPort:    s.port,
			dstIP:      dstIP,
			dstPort:    dstPort,
			payload:    append([]byte(nil), p...),
			sendAt:     now,
			deliverAt:  at,
			seq:        th.seq,
			senderSock: senderRef,
		}
		dstKernelSectionEnter()
		dstKernelRaceReleaseMerge(unsafe.Pointer(&pkt.raceSync))
		dstKernelSectionExit()
		th.inflight.push(pkt)
	}
	refs, drops := th.advanceLocked(now)
	th.bumpWakeLocked()
	th.mu.Unlock()
	if len(drops) > 0 {
		dstUDPRecordDrops(drops)
	}
	if len(refs) > 0 {
		dstUDPPostRefusals(refs)
	}
	return len(p), nil
}

// ---------------------------------------------------------------------------
// netFD, sockopt_posix, fd_unix, and rawconn interception hooks.

func dstFDClose(fd *netFD) (bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return false, nil
	}
	if !dstInSimBubble() && dstActive() && s.epoch == dstNetEpoch() {
		return true, errClosed
	}
	if s.stale() {
		if s.staleClosed.Swap(true) {
			return true, errClosed
		}
		return true, nil
	}
	dstCoarseDep(uintptr(unsafe.Pointer(&s.closeSync)), true)
	dstCoarseRecord(dstCoarseNetHostID(s.host), true)
	if s.sockFD != 0 {
		dstCoarseRecord(dstCoarseSockFDID(s.sockFD), true)
	}
	return true, s.closeInternal(true)
}

func dstFDShutdown(fd *netFD, how int) (bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return false, nil
	}
	if s.stale() {
		return true, errClosed
	}
	dstCoarseDep(uintptr(unsafe.Pointer(&s.closeSync)), false)
	s.hostNet.mu.Lock()
	closed := s.closed
	if closed {
		dstKernelSectionEnter()
		dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
		dstKernelSectionExit()
	}
	connected := s.connected
	s.hostNet.mu.Unlock()
	if closed {
		return true, errClosed
	}
	if !connected {
		return true, syscall.ENOTCONN
	}
	return true, syscall.EOPNOTSUPP
}

func dstFDRead(fd *netFD, p []byte) (int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, false, nil
	}
	n, _, _, _, err := s.recvDatagram(p)
	return n, true, err
}

func dstFillSockaddrInet4(sa *syscall.SockaddrInet4, ip IP, port int) {
	if sa == nil {
		return
	}
	*sa = syscall.SockaddrInet4{Port: port}
	if v4 := ip.To4(); v4 != nil {
		copy(sa.Addr[:], v4)
	}
}

func dstFillSockaddrInet6(sa *syscall.SockaddrInet6, ip IP, port int) {
	if sa == nil {
		return
	}
	*sa = syscall.SockaddrInet6{Port: port}
	if v6 := ip.To16(); v6 != nil {
		copy(sa.Addr[:], v6)
	}
}

func dstMakeSockaddr(family int, ip IP, port int) syscall.Sockaddr {
	if family == syscall.AF_INET {
		var sa syscall.SockaddrInet4
		dstFillSockaddrInet4(&sa, ip, port)
		return &sa
	}
	var sa syscall.SockaddrInet6
	dstFillSockaddrInet6(&sa, ip, port)
	return &sa
}

func dstFDReadFrom(fd *netFD, p []byte) (int, syscall.Sockaddr, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, nil, false, nil
	}
	n, _, srcIP, srcPort, err := s.recvDatagram(p)
	if err != nil {
		return 0, nil, true, err
	}
	return n, dstMakeSockaddr(s.family, srcIP, srcPort), true, nil
}

func dstFDReadFromInet4(fd *netFD, p []byte, from *syscall.SockaddrInet4) (int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, false, nil
	}
	n, _, srcIP, srcPort, err := s.recvDatagram(p)
	if err == nil {
		dstFillSockaddrInet4(from, srcIP, srcPort)
	}
	return n, true, err
}

func dstFDReadFromInet6(fd *netFD, p []byte, from *syscall.SockaddrInet6) (int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, false, nil
	}
	n, _, srcIP, srcPort, err := s.recvDatagram(p)
	if err == nil {
		dstFillSockaddrInet6(from, srcIP, srcPort)
	}
	return n, true, err
}

var errDSTOOBUnsupported = errors.New("UDP ancillary control messages unsupported under deterministic simulation")

func dstFDReadMsg(fd *netFD, p, oob []byte, flags int) (int, int, int, syscall.Sockaddr, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, 0, 0, nil, false, nil
	}
	if len(oob) > 0 || flags != 0 {
		return 0, 0, 0, nil, true, errDSTOOBUnsupported
	}
	n, trunc, srcIP, srcPort, err := s.recvDatagram(p)
	if err != nil {
		return 0, 0, 0, nil, true, err
	}
	retFlags := 0
	if trunc {
		retFlags = syscall.MSG_TRUNC
	}
	return n, 0, retFlags, dstMakeSockaddr(s.family, srcIP, srcPort), true, nil
}

func dstFDReadMsgInet4(fd *netFD, p, oob []byte, flags int, sa *syscall.SockaddrInet4) (int, int, int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, 0, 0, false, nil
	}
	if len(oob) > 0 || flags != 0 {
		return 0, 0, 0, true, errDSTOOBUnsupported
	}
	n, trunc, srcIP, srcPort, err := s.recvDatagram(p)
	if err == nil {
		dstFillSockaddrInet4(sa, srcIP, srcPort)
	}
	retFlags := 0
	if trunc && err == nil {
		retFlags = syscall.MSG_TRUNC
	}
	return n, 0, retFlags, true, err
}

func dstFDReadMsgInet6(fd *netFD, p, oob []byte, flags int, sa *syscall.SockaddrInet6) (int, int, int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, 0, 0, false, nil
	}
	if len(oob) > 0 || flags != 0 {
		return 0, 0, 0, true, errDSTOOBUnsupported
	}
	n, trunc, srcIP, srcPort, err := s.recvDatagram(p)
	if err == nil {
		dstFillSockaddrInet6(sa, srcIP, srcPort)
	}
	retFlags := 0
	if trunc && err == nil {
		retFlags = syscall.MSG_TRUNC
	}
	return n, 0, retFlags, true, err
}

func dstFDWrite(fd *netFD, p []byte) (int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, false, nil
	}
	n, err := s.sendDatagram(p, false, nil, 0)
	return n, true, err
}

func dstParseSockaddr(sa syscall.Sockaddr) (ip IP, port int, has bool, err error) {
	if sa == nil {
		return nil, 0, false, nil
	}
	switch a := sa.(type) {
	case *syscall.SockaddrInet4:
		ip = make(IP, IPv4len)
		copy(ip, a.Addr[:])
		return ip, a.Port, true, nil
	case *syscall.SockaddrInet6:
		ip = make(IP, IPv6len)
		copy(ip, a.Addr[:])
		return ip, a.Port, true, nil
	default:
		return nil, 0, true, syscall.EAFNOSUPPORT
	}
}

func dstFDWriteTo(fd *netFD, p []byte, sa syscall.Sockaddr) (int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, false, nil
	}
	ip, port, has, err := dstParseSockaddr(sa)
	if err != nil {
		return 0, true, err
	}
	n, err := s.sendDatagram(p, has, ip, port)
	return n, true, err
}

func dstFDWriteToInet4(fd *netFD, p []byte, sa *syscall.SockaddrInet4) (int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, false, nil
	}
	if sa == nil {
		n, err := s.sendDatagram(p, false, nil, 0)
		return n, true, err
	}
	ip := make(IP, IPv4len)
	copy(ip, sa.Addr[:])
	n, err := s.sendDatagram(p, true, ip, sa.Port)
	return n, true, err
}

func dstFDWriteToInet6(fd *netFD, p []byte, sa *syscall.SockaddrInet6) (int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, false, nil
	}
	if sa == nil {
		n, err := s.sendDatagram(p, false, nil, 0)
		return n, true, err
	}
	ip := make(IP, IPv6len)
	copy(ip, sa.Addr[:])
	n, err := s.sendDatagram(p, true, ip, sa.Port)
	return n, true, err
}

func dstFDWriteMsg(fd *netFD, p, oob []byte, sa syscall.Sockaddr) (int, int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, 0, false, nil
	}
	if len(oob) > 0 {
		return 0, 0, true, errDSTOOBUnsupported
	}
	ip, port, has, err := dstParseSockaddr(sa)
	if err != nil {
		return 0, 0, true, err
	}
	n, err := s.sendDatagram(p, has, ip, port)
	return n, 0, true, err
}

func dstFDWriteMsgInet4(fd *netFD, p, oob []byte, sa *syscall.SockaddrInet4) (int, int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, 0, false, nil
	}
	if len(oob) > 0 {
		return 0, 0, true, errDSTOOBUnsupported
	}
	if sa == nil {
		n, err := s.sendDatagram(p, false, nil, 0)
		return n, 0, true, err
	}
	ip := make(IP, IPv4len)
	copy(ip, sa.Addr[:])
	n, err := s.sendDatagram(p, true, ip, sa.Port)
	return n, 0, true, err
}

func dstFDWriteMsgInet6(fd *netFD, p, oob []byte, sa *syscall.SockaddrInet6) (int, int, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return 0, 0, false, nil
	}
	if len(oob) > 0 {
		return 0, 0, true, errDSTOOBUnsupported
	}
	if sa == nil {
		n, err := s.sendDatagram(p, false, nil, 0)
		return n, 0, true, err
	}
	ip := make(IP, IPv6len)
	copy(ip, sa.Addr[:])
	n, err := s.sendDatagram(p, true, ip, sa.Port)
	return n, 0, true, err
}

func dstFDSetDeadline(fd *netFD, t time.Time) (bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return false, nil
	}
	if s.stale() {
		return true, errClosed
	}
	dstCoarseDep(uintptr(unsafe.Pointer(&s.closeSync)), true)
	isZero := t.IsZero()
	var dur time.Duration
	if !isZero {
		dur = time.Until(t)
	}
	h := s.hostNet
	h.mu.Lock()
	if s.closed {
		dstKernelSectionEnter()
		dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
		dstKernelSectionExit()
		h.mu.Unlock()
		return true, errClosed
	}
	s.armDeadlineLocked(true, isZero, dur)
	s.armDeadlineLocked(false, isZero, dur)
	s.bumpWakeLocked()
	h.mu.Unlock()
	return true, nil
}

func dstFDSetReadDeadline(fd *netFD, t time.Time) (bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return false, nil
	}
	if s.stale() {
		return true, errClosed
	}
	dstCoarseDep(uintptr(unsafe.Pointer(&s.closeSync)), true)
	isZero := t.IsZero()
	var dur time.Duration
	if !isZero {
		dur = time.Until(t)
	}
	h := s.hostNet
	h.mu.Lock()
	if s.closed {
		dstKernelSectionEnter()
		dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
		dstKernelSectionExit()
		h.mu.Unlock()
		return true, errClosed
	}
	s.armDeadlineLocked(true, isZero, dur)
	s.bumpWakeLocked()
	h.mu.Unlock()
	return true, nil
}

func dstFDSetWriteDeadline(fd *netFD, t time.Time) (bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return false, nil
	}
	if s.stale() {
		return true, errClosed
	}
	dstCoarseDep(uintptr(unsafe.Pointer(&s.closeSync)), true)
	isZero := t.IsZero()
	var dur time.Duration
	if !isZero {
		dur = time.Until(t)
	}
	h := s.hostNet
	h.mu.Lock()
	if s.closed {
		dstKernelSectionEnter()
		dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
		dstKernelSectionExit()
		h.mu.Unlock()
		return true, errClosed
	}
	s.armDeadlineLocked(false, isZero, dur)
	s.bumpWakeLocked()
	h.mu.Unlock()
	return true, nil
}

func dstFDDup(fd *netFD) (*os.File, bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return nil, false, nil
	}
	return nil, true, errors.New("socket File duplication unsupported under deterministic simulation")
}

func dstFDSetReadBuffer(fd *netFD, bytes int) (bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return false, nil
	}
	if s.stale() {
		return true, errClosed
	}
	dstCoarseDep(uintptr(unsafe.Pointer(&s.closeSync)), true)
	if s.sockFD != 0 {
		dstCoarseRecord(dstCoarseSockFDID(s.sockFD), true)
	}
	s.hostNet.mu.Lock()
	closed := s.closed
	if closed {
		dstKernelSectionEnter()
		dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
		dstKernelSectionExit()
	}
	s.hostNet.mu.Unlock()
	if closed {
		return true, errClosed
	}
	s.opts.setRcvBuf(int32(bytes))
	return true, nil
}

func dstFDSetWriteBuffer(fd *netFD, bytes int) (bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return false, nil
	}
	if s.stale() {
		return true, errClosed
	}
	dstCoarseDep(uintptr(unsafe.Pointer(&s.closeSync)), true)
	if s.sockFD != 0 {
		dstCoarseRecord(dstCoarseSockFDID(s.sockFD), true)
	}
	s.hostNet.mu.Lock()
	closed := s.closed
	if closed {
		dstKernelSectionEnter()
		dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
		dstKernelSectionExit()
	}
	s.hostNet.mu.Unlock()
	if closed {
		return true, errClosed
	}
	s.opts.setSndBuf(int32(bytes))
	return true, nil
}

func dstFDRawControl(fd *netFD, f func(uintptr)) (bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return false, nil
	}
	if s.stale() {
		return true, errClosed
	}
	dstCoarseDep(dstCoarseSockFDID(s.sockFD), false)
	dstCoarseRecord(uintptr(unsafe.Pointer(&s.closeSync)), false)
	s.hostNet.mu.Lock()
	closed := s.closed
	if closed {
		dstKernelSectionEnter()
		dstKernelRaceAcquire(unsafe.Pointer(&s.closeSync))
		dstKernelSectionExit()
	}
	s.hostNet.mu.Unlock()
	if closed || dstSockFDLookup(s.sockFD) == nil {
		return true, errClosed
	}
	f(uintptr(s.sockFD))
	return true, nil
}

func dstFDRawRead(fd *netFD, _ func(uintptr) bool) (bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return false, nil
	}
	return true, errors.New("raw socket read unsupported under deterministic simulation")
}

func dstFDRawWrite(fd *netFD, _ func(uintptr) bool) (bool, error) {
	s := dstUDPSockOf(fd)
	if s == nil {
		return false, nil
	}
	return true, errors.New("raw socket write unsupported under deterministic simulation")
}
