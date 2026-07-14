// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"sort"
	"sync"
)

// Connection-reset targeting. The imperative API in testing/simulation
// (Reset / ResetProcess) drives this through runtime's passthrough into
// dstApplyNetFaultOp, exactly like partition. A reset enumerates the active
// connections matching the victim — a host-pair (all conns between hosts a and b)
// or a process (all conns that process owns either end of) — and tears each
// end down KERNEL-FAITHFULLY (faults.md "Connection reset"): a SURVIVING end
// receives the RST as a real kernel would deliver it — bytes already in its
// receive queue drain first, then the first failing op reports ECONNRESET
// (one-shot sk_err; later reads EOF, later writes EPIPE — consumeSkErr);
// writes fail at once; bytes still in flight toward it die (dstInjectReset).
// A DEAD end (its
// process or host crashed) is torn down outright via resetConn — its receive
// queue died with it and nothing will read it again. Matching is by the
// connection's host/process attribution (dstConn.localHost/remoteHost/
// localProc/remoteProc), so a reset touches exactly the victim's conns
// (DST-FAULT-VICTIM).

// dstConns is the per-run set of active simulated connections, registered at Dial
// and deregistered on Close/reset. Keyed off the run epoch (dstNetEpoch) so it
// resets each run. Both ends of a connection are registered, each as its own
// dstConn. resetConn tears down the END it is called on — that end's transport
// closes; the shared reset flag only maps the peer's eventual EOF to the reset
// identity, it does NOT close the peer's transport, so a peer whose own dstConn
// is not torn down still DRAINS queued bytes before seeing the error. That
// drain is the kernel-real shape for an RST's RECEIVER (tcp_recvmsg reports
// pending data before the socket error, host-probed): the close(2)
// conditional's RST arm (dstResetBothEnds) resets only the emitting end and
// merely deregisters the peer, and the FAULT matchers deliver an injected RST
// to each surviving end via dstInjectReset — delivered bytes drain, in-flight
// bytes die, writes fail (the host and process crash matchers reset the DEAD
// side's ends outright and spare a survivor's end whose victim end already
// closed — see dstResetHost). The listener-backlog teardown of a
// never-accepted server end, toward whose dialer nothing can yet be queued,
// is per-end. resetConn is idempotent when both ends are reset: an atomic
// flag store, a sync.Once close, a map delete.
var dstConns struct {
	mu    sync.Mutex
	epoch uint64
	set   map[*dstConn]struct{}
	seq   uint64 // per-run monotonic registration counter (see dstConn.regSeq)

	// timeWait holds the local 2-tuples of actively-FIN-closed conn ends
	// until their base-time expiry — production's TIME_WAIT, visible only to
	// the explicit-LocalAddr bind probe (dstTimeWaitHeld), never to listener
	// probes or the ephemeral allocator (SO_REUSEADDR binds over TIME_WAIT;
	// connect-time selection reuses held ports). Appended in Close order
	// (schedule-determined) and pruned in place, so iteration order is
	// deterministic. prunedLen is the slice length after the last prune; adds
	// prune again once the length doubles (amortized O(1), length-driven so
	// replay-exact). See design.md's bind paragraph for the modeled scope.
	timeWait  []dstTimeWait
	prunedLen int
}

// dstTimeWait is one held local 2-tuple: an actively-closed dialer end's
// local binding, refused to non-SO_REUSEADDR binds until expireAt.
type dstTimeWait struct {
	host     uint32
	ip       IP
	port     int
	family   string
	wildcard bool
	expireAt int64 // universe base-time ns (dstBaseNanos)
}

// dstTimeWaitNs is production's TIME_WAIT hold: Linux's TCP_TIMEWAIT_LEN, a
// fixed 60s — the kernel's 2·MSL. Counted in universe base time, the same
// basis wire delivery is gated on (a kernel TCP timer, not the host's
// possibly-drifted wall clock — DST-NET-LATENCY-DET's precedent).
const dstTimeWaitNs = 60_000_000_000

// dstTimeWaitAdd records an actively-closed conn end's local 2-tuple. Prunes
// expired entries once the slice has doubled since the last prune, so a run
// that never consults the probe still holds only a bounded window of churn.
func dstTimeWaitAdd(host uint32, ip IP, port int, family string, wildcard bool) {
	dstConns.mu.Lock()
	now := dstBaseNanos()
	dstConnsRoll()
	if len(dstConns.timeWait) >= 64 && len(dstConns.timeWait) >= 2*dstConns.prunedLen {
		dstTimeWaitPrune(now)
	}
	dstConns.timeWait = append(dstConns.timeWait, dstTimeWait{
		host: host, ip: ip, port: port, family: family, wildcard: wildcard, expireAt: now + dstTimeWaitNs,
	})
	dstConns.mu.Unlock()
}

// dstTimeWaitPrune drops expired holds in place, order-preserving so the
// slice stays deterministic. Caller holds dstConns.mu.
func dstTimeWaitPrune(now int64) {
	live := dstConns.timeWait[:0]
	for _, w := range dstConns.timeWait {
		if w.expireAt > now {
			live = append(live, w)
		}
	}
	dstConns.timeWait = live
	dstConns.prunedLen = len(live)
}

// dstTimeWaitDropHost drops every hold on host h — the host-crash arm:
// TIME_WAIT is kernel socket-table state, lost with power. Order-preserving
// in place; the prune watermark is clamped so the doubling trigger stays
// monotone against the shrunk slice.
func dstTimeWaitDropHost(h uint32) {
	dstConns.mu.Lock()
	dstConnsRoll()
	live := dstConns.timeWait[:0]
	for _, w := range dstConns.timeWait {
		if w.host != h {
			live = append(live, w)
		}
	}
	dstConns.timeWait = live
	if dstConns.prunedLen > len(live) {
		dstConns.prunedLen = len(live)
	}
	dstConns.mu.Unlock()
}

// dstTimeWaitHeld reports whether ip:port on host is inside a TIME_WAIT hold —
// the bind(2)-without-SO_REUSEADDR probe of the explicit-LocalAddr dial path —
// pruning expired entries as it scans. ip nil means any IP at the port,
// matching the live-conn probe's convention.
func dstTimeWaitHeld(host uint32, ip IP, port int) bool {
	return dstTimeWaitBindHeld(host, ip, port, "")
}

func dstTimeWaitBindHeld(host uint32, ip IP, port int, family string) bool {
	dstConns.mu.Lock()
	defer dstConns.mu.Unlock()
	dstConnsRoll()
	dstTimeWaitPrune(dstBaseNanos())
	held := false
	for _, w := range dstConns.timeWait {
		if w.host != host || w.port != port {
			continue
		}
		if family != "" && w.family != "" && w.family != family {
			continue
		}
		if ip != nil && !w.wildcard && !w.ip.Equal(ip) {
			continue
		}
		held = true
		break
	}
	return held
}

func dstConnsRoll() { // caller holds mu
	if e := dstNetEpoch(); e != dstConns.epoch || dstConns.set == nil {
		dstConns.epoch = e
		dstConns.set = make(map[*dstConn]struct{})
		dstConns.seq = 0
		dstConns.timeWait = nil
		dstConns.prunedLen = 0
	}
}

func dstConnRegisterPair(dialer, server *dstConn) {
	dstConns.mu.Lock()
	defer dstConns.mu.Unlock()
	dstConnsRoll()
	// Publish both ends in one critical section. Their consecutive sequence
	// numbers preserve deterministic teardown order without exposing a half-owned
	// pair to reset or lifecycle teardown.
	dstConns.seq++
	dialer.regSeq = dstConns.seq
	dstConns.set[dialer] = struct{}{}
	dstConns.seq++
	server.regSeq = dstConns.seq
	dstConns.set[server] = struct{}{}
}

func dstConnDeregister(c *dstConn) {
	dstConns.mu.Lock()
	dstConnsRoll()
	delete(dstConns.set, c)
	dstConns.mu.Unlock()
}

// dstResetMatching delivers an injected RST to every registered conn end for
// which match reports true — the ALL-SURVIVORS teardown (Reset host-pair,
// ResetProcess): no process died, so every matched end is a live kernel
// receiving an RST and drains its delivered bytes before failing
// (dstInjectReset). The victims are collected under the lock and torn down
// outside it, so the teardown's own deregister does not re-enter the lock.
// They are processed in registration-SEQUENCE order — never the registry
// map's iteration order, which hashes run-varying pointer addresses (the
// fixed -tags dst hash key does not make addresses reproducible), so with
// several victims the wake order of their blocked readers, and thus the
// whole downstream schedule, would diverge across same-seed runs
// (DST-FAULT-REPLAY).
func dstResetMatching(match func(*dstConn) bool) {
	for _, c := range dstMatchedVictims(match) {
		dstInjectReset(c)
	}
}

// dstInjectReset delivers a fault-injected RST to conn end c, the
// kernel-faithful receiver teardown: already-delivered bytes drain to the
// one-shot ECONNRESET identity (shared reset flag maps the post-drain failure),
// in-flight bytes die, writes fail immediately (dstWireEnd.injectRST), and
// the registration is released as production releases the tuple when the RST
// moves the socket to CLOSED. A server end still QUEUED in the accept
// backlog takes the same survivor shape: the accept queue holds only
// ESTABLISHED children (the SYN queue is upstream of Dial's return), so the
// dialer may already have written into its receive queue, and the kernel
// does not unlink an RST-aborted child from the queue — accept(2) hands it
// out and its reads drain the delivered bytes before the one-shot ECONNRESET
// (host-probed, with and without pre-accept data). acceptState deliberately
// stays 0 so Accept's 0→1 claim succeeds — the handout IS the production
// shape; only the listener-close teardown claims 0→2 (a closed listener can
// hand nothing out). A dial still blocked mid-establishment is woken into a
// prompt ECONNREFUSED by its own end's rstKill (the backlog-send select) or
// the shared reset flag (dstConnectSYNACK) — the SYN_SENT abort, the
// connection-refused mapping. One
// collapse to the outright resetConn remains: a future non-wire Conn
// wrapper falls to the uniform hard teardown rather than a silent skip.
func dstInjectReset(c *dstConn) {
	e, ok := c.Conn.(*dstWireEnd)
	if !ok {
		c.resetConn()
		return
	}
	c.reset.Store(true)
	e.injectRST()
	dstConnDeregister(c)
}

// dstMatchedVictims collects the registered conns satisfying match and returns them
// in registration-SEQUENCE order (never the registry map's pointer-address iteration
// order — see dstResetMatching's note). Factored out so the ordering is directly
// testable without resetConn's side effects. Collect-before-teardown is
// load-bearing beyond ordering: a matcher's predicate may read state a sibling
// victim's resetConn mutates (dstResetHost's app-closed skip reads the peer
// end's done channel), so every match must run against the pre-fault snapshot,
// before the first teardown.
func dstMatchedVictims(match func(*dstConn) bool) []*dstConn {
	dstConns.mu.Lock()
	var victims []*dstConn
	dstConnsRoll()
	for c := range dstConns.set {
		if match(c) {
			victims = append(victims, c)
		}
	}
	dstConns.mu.Unlock()
	sort.Slice(victims, func(i, j int) bool { return victims[i].regSeq < victims[j].regSeq })
	return victims
}

// dstLocalBindInUse reports whether a live conn on host already occupies the local
// binding ip:port — a 2-tuple, which is what the real path checks: Go binds an
// explicit LocalAddr without SO_REUSEADDR, so bind(2) fails EADDRINUSE on a local
// addr:port collision even when the destinations differ, and an ephemeral allocator
// must skip a still-live port. Scans the per-run conn registry by the dialer end's
// attribution (localHost + local address). LIVE conns only: the explicit-LocalAddr
// dial path consults the TIME_WAIT holds separately (dstTimeWaitHeld — the bind(2)
// surface), while the ephemeral allocator deliberately does not (production's
// connect-time selection is 4-tuple-aware with tcp_tw_reuse, so churn never
// exhausts the range there — see design.md's bind paragraph). Deterministic (a
// set-membership test, no iteration order observed).
func dstLocalBindInUse(host uint32, ip IP, port int) bool {
	return dstConnBindInUse(host, ip, port, "", false)
}

// dstConnBindInUse is the general conn-side bind probe: does a live conn end on
// host occupy local port? ip nil means any IP at the port (the wildcard-listen
// probe); family "" means either family, otherwise "tcp4"/"tcp6" narrows it.
// dialerEndsOnly counts only dialer-side ends: an ACCEPTED server end inherits
// the listener's SO_REUSEADDR, so it never blocks a new listener — a server
// restarted while its old connections drain must be able to re-bind its port —
// while a dialer's socket carries no SO_REUSEADDR and blocks everything.
// Deterministic (a set-membership test, no iteration order observed).
func dstConnBindInUse(host uint32, ip IP, port int, family string, dialerEndsOnly bool) bool {
	dstConns.mu.Lock()
	defer dstConns.mu.Unlock()
	dstConnsRoll()
	for c := range dstConns.set {
		if c.localHost != host {
			continue
		}
		if dialerEndsOnly && c.acceptState != nil {
			continue
		}
		la, ok := c.local.(*TCPAddr)
		if !ok || la.Port != port {
			continue
		}
		connFamily := c.bindFamily
		if connFamily == "" {
			connFamily = dstAddrFamily("", la.IP)
		}
		if family != "" && connFamily != family {
			continue
		}
		if ip != nil && !c.bindWildcard && !la.IP.Equal(ip) {
			continue
		}
		return true
	}
	return false
}

// dstResetHost tears down every conn an end of which lives on host h — the
// machine lost power. The VICTIM's own ends (localHost == h) reset outright:
// their receive queues died with the kernel and nothing will read them
// again. Each SURVIVING peer end receives the injected RST kernel-faithfully
// (dstInjectReset): bytes already delivered to the survivor's receive queue
// drain first — an incoming RST cannot destroy what the survivor's kernel
// already holds — then the first read fails ECONNRESET (one-shot sk_err);
// bytes still in flight died
// with the crashed sender. Tearing the survivor's end down outright instead
// would destroy delivered bytes (an execution no real kernel produces), and
// tearing down ONLY the victim's end would present at the peer as a graceful
// write-close, draining even the in-flight bytes the power loss destroyed. A
// conn between two other hosts has neither end on h and is untouched
// (DST-FAULT-VICTIM).
func dstResetHost(h uint32) {
	// The machine lost power, so its kernel's TIME_WAIT table is gone too: a
	// hold surviving the crash would refuse a bind the rebooted kernel
	// allows. A process crash, by contrast, leaves the kernel (and its
	// holds) alive — only this host-crash path purges.
	dstTimeWaitDropHost(h)
	victimEnd := func(c *dstConn) bool { return c.localHost == h }
	survivorEnd := func(c *dstConn) bool {
		if c.localHost == h || c.remoteHost != h {
			return false
		}
		// The surviving peer's end of a victim conn. Skip it when the victim's
		// own end was already closed before the power loss (its Close
		// deregistered it): the data and FIN are on the wire, and a powered-off
		// machine emits no packet that could destroy bytes its peer already
		// holds — the peer drains and reads EOF (or the ECONNRESET an earlier
		// exit-reset recorded), exactly as the pre-crash teardown left it. No
		// real RST exists for an app-closed end at power loss
		// (DST-FAULT-SOUND). Every simulated connection is wire-backed (the
		// transport contract); a future non-wire Conn wrapper falls to the
		// RST arm — the uniform crash teardown — never to a silent skip.
		// The predicate is evaluated against the PRE-CRASH snapshot:
		// dstMatchedVictims runs every match before any teardown, so a
		// victim-side reset in the same crash (which closes exactly the
		// channel tested here) cannot flip a survivor entry to a spurious
		// skip. An interleaved match-and-teardown loop would misclassify
		// whenever the victim end carries the lower regSeq (victim dialed) —
		// see TestDSTCrashHostDropsInFlightBytesVictimDialer.
		if e, ok := c.Conn.(*dstWireEnd); ok && isClosedChan(e.remoteDone) {
			return false
		}
		return true
	}
	dstCrashTeardown(victimEnd, survivorEnd)
}

// dstCrashTeardown is the shared crash-fault conn teardown: one snapshot of
// the registry, victim (dead) ends reset outright, surviving peer ends given
// the injected drain-then-reset RST — in registration-sequence order across
// the union (DST-FAULT-REPLAY), with every predicate evaluated against the
// pre-fault snapshot before any teardown runs.
func dstCrashTeardown(victimEnd, survivorEnd func(*dstConn) bool) {
	victims := dstMatchedVictims(func(c *dstConn) bool { return victimEnd(c) || survivorEnd(c) })
	// Classify against the snapshot BEFORE any teardown mutates conn state.
	dead := make(map[*dstConn]bool, len(victims))
	for _, c := range victims {
		dead[c] = victimEnd(c)
	}
	for _, c := range victims {
		if dead[c] {
			c.resetConn()
		} else {
			dstInjectReset(c)
		}
	}
}

// dstCloseHostListeners closes every listener on host h — the whole port space
// dies with the machine, whichever of its processes bound it.
func dstCloseHostListeners(h uint32) {
	dstReleasePendingBinds(func(key dstBindKey, _ uint32) bool { return key.host == h })
	dstCloseListeners(func(l *dstListener) bool { return l.host == h })
}

// dstResetPair resets every conn between hosts a and b (either direction).
func dstResetPair(a, b uint32) {
	key := dstPartKey(a, b)
	dstResetMatching(func(c *dstConn) bool {
		return dstPartKey(c.localHost, c.remoteHost) == key
	})
}

// dstResetProc injects an RST on every conn that process p owns an end of (as
// dialer or as the listening/accepting process) — the ResetProcess fault: p is
// ALIVE (only its connections are reset), so every end, p's own included, is a
// survivor and drains its delivered bytes before failing (dstResetMatching's
// all-survivors teardown). The crash faults use dstCrashProcConns instead.
func dstResetProc(p uint32) {
	dstResetMatching(func(c *dstConn) bool {
		return c.localProc == p || c.remoteProc == p
	})
}

// dstCrashProcConns tears down every conn process p owns an end of for a
// process CRASH (kill -9 / OOM): p is dead. Its own ends reset outright —
// their receive queues died with the process. Each surviving peer end
// receives the injected RST kernel-faithfully (drain delivered bytes, then
// ECONNRESET), except a peer end whose victim-side end the application
// already closed before the crash: its data and FIN are on the wire and the
// kernel (which survives a process crash) has no socket left to answer RST
// for — the peer drains and reads EOF exactly as the pre-crash teardown left
// it, the same boundary the host-crash matcher applies (DST-FAULT-SOUND).
func dstCrashProcConns(p uint32) {
	victimEnd := func(c *dstConn) bool { return c.localProc == p }
	survivorEnd := func(c *dstConn) bool {
		if c.localProc == p || c.remoteProc != p {
			return false
		}
		if e, ok := c.Conn.(*dstWireEnd); ok && isClosedChan(e.remoteDone) {
			return false // app-closed victim end: FIN already on the wire
		}
		return true
	}
	dstCrashTeardown(victimEnd, survivorEnd)
}

// dstCloseProcConns closes the connection ENDS process p owns — the exit-time
// close (the kernel closes a dying process's sockets on normal exit). The
// kernel's own conditional applies per end: a socket whose receive queue holds
// unread data answers the peer with RST (ECONNRESET), otherwise the close FINs
// and the peer drains buffered bytes then reads io.EOF. (Crash resets
// unconditionally — a different fault, see faults.md.) Each end of a
// connection registers its own dstConn whose localProc is that end's owner, so
// matching localProc alone covers every end p owns; the victims close in
// registration-sequence order (DST-FAULT-REPLAY, as dstResetMatching).
func dstCloseProcConns(p uint32) {
	for _, c := range dstMatchedVictims(func(c *dstConn) bool { return c.localProc == p }) {
		// Every simulated connection is wire-backed (the transport contract),
		// so the assertion always holds today; a future non-wire Conn wrapper
		// must extend the unread-inbound probe or it silently degrades the RST
		// arm to a graceful close.
		if e, ok := c.Conn.(*dstWireEnd); ok && e.unreadInbound() {
			dstResetBothEnds(c)
			continue
		}
		c.Close()
	}
}

// dstConnPeer finds the OTHER registered end of c's connection — the two ends
// share one reset flag, the only cross-end link — or nil if the peer already
// closed (its Close deregistered it). Deterministic: at most one non-c entry
// can share the pointer.
func dstConnPeer(c *dstConn) *dstConn {
	dstConns.mu.Lock()
	defer dstConns.mu.Unlock()
	dstConnsRoll()
	for c2 := range dstConns.set {
		if c2 != c && c2.reset == c.reset {
			return c2
		}
	}
	return nil
}

// dstResetBothEnds is the close(2) conditional's RST teardown: the EMITTING
// end resets (transport closed, deregistered); the RST-RECEIVING peer keeps
// its transport and only loses its registration. An incoming RST cannot
// destroy bytes the receiver's kernel already queued — tcp_recvmsg drains
// pending data before reporting the socket error (host-probed; the same rule
// the retransmit-horizon death follows) — and bytes the emitter wrote before
// closing travel ahead of its RST on the in-order link, so they drain too.
// The emitter's transport close ends the byte stream (the peer drains, sees
// EOF, and the SHARED reset flag stored by resetConn maps that EOF — and its
// failed writes — to the one-shot ECONNRESET identity); deregistering the peer
// now mirrors production releasing the tuple when the RST moves the socket to
// CLOSED, before any close(2). The fault-injection matchers follow the same
// tcp_recvmsg rule through dstInjectReset — a survivor always drains its
// delivered bytes — differing only in that an INJECTED RST also destroys the
// in-flight bytes this conditional lets travel ahead of it. A peer that
// already closed needs nothing (dstConnPeer returns nil).
func dstResetBothEnds(c *dstConn) {
	peer := dstConnPeer(c)
	c.resetConn()
	if peer != nil {
		dstConnDeregister(peer)
	}
}

func dstCloseProcListeners(p uint32) {
	dstReleasePendingBinds(func(_ dstBindKey, proc uint32) bool { return proc == p })
	dstCloseListeners(func(l *dstListener) bool { return l.proc == p })
}

func dstReleasePendingBinds(match func(dstBindKey, uint32) bool) {
	dstNet.mu.Lock()
	defer dstNet.mu.Unlock()
	dstNetRoll()
	for key, proc := range dstNet.pendingBinds {
		if match(key, proc) {
			delete(dstNet.pendingBinds, key)
		}
	}
}

func dstCloseListeners(match func(*dstListener) bool) {
	type victim struct {
		key string
		l   *dstListener
	}
	dstNet.mu.Lock()
	dstNetRoll()
	var victims []victim
	for key, l := range dstNet.listeners {
		if match(l) {
			victims = append(victims, victim{key: key, l: l})
		}
	}
	dstNet.mu.Unlock()
	sort.Slice(victims, func(i, j int) bool { return victims[i].key < victims[j].key })
	seen := make(map[*dstListener]bool)
	for _, v := range victims {
		if seen[v.l] {
			continue
		}
		seen[v.l] = true
		v.l.Close()
	}
}
