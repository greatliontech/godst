// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"time"
)

// The simulated socket-option layer.
//
// Every simulated socket — a dialing socket, a listener, and each end of an
// established connection — carries a dstSockOpts: the option state a real
// kernel keeps per socket, restricted to the options the base network model
// gives semantics to. It is reachable two ways, exactly as in production:
//
//   - The Go-level configuration surface (Dialer.KeepAlive/KeepAliveConfig,
//     ListenConfig.KeepAlive/KeepAliveConfig), applied with production's own
//     resolution rules (newTCPConn → SetKeepAliveConfig: zero fields take the
//     Go defaults, negative fields leave the socket state unchanged).
//   - Raw setsockopt/getsockopt on the socket's VIRTUAL descriptor — the fd a
//     Dialer.Control / ListenConfig.Control callback receives, and the one
//     SyscallConn hands out after establishment. The raw syscalls (the
//     golang.org/x/sys/unix path) are routed here by the syscall package's
//     raw-boundary dispatch; the numeric (level, option) mapping and errno
//     shapes live in dst_sockopt_linux.go.
//
// The modeled set is SO_KEEPALIVE, TCP_KEEPIDLE, TCP_KEEPINTVL, TCP_KEEPCNT,
// and TCP_USER_TIMEOUT — the options whose semantics the wire's
// retransmission/death machinery implements (dst_wire.go's keepalive prober
// and per-socket horizon override). An option outside the modeled set is
// REFUSED at the syscall with ENOPROTOOPT rather than stored-and-ignored:
// the fence philosophy at option granularity — nothing is silently accepted
// whose behavior the model does not provide.
//
// Defaults are the simulated kernel's and are deterministic constants: the
// Linux sysctl defaults (tcp_keepalive_time=7200s, tcp_keepalive_intvl=75s,
// tcp_keepalive_probes=9), so a socket enabled via bare SO_KEEPALIVE=1 (the
// grpc-go dialer's recipe) probes on the same schedule everywhere.
const (
	dstKeepIdleDefaultSec  = 7200
	dstKeepIntvlDefaultSec = 75
	dstKeepCntDefault      = 9

	// Kernel bounds for the keepalive parameters (linux tcp(7)):
	// MAX_TCP_KEEPIDLE/MAX_TCP_KEEPINTVL = 32767, MAX_TCP_KEEPCNT = 127.
	dstKeepIdleMaxSec  = 32767
	dstKeepIntvlMaxSec = 32767
	dstKeepCntMax      = 127
)

type dstSockOpts struct {
	mu            sync.Mutex
	keepAlive     bool   // SO_KEEPALIVE
	keepIdleSec   int32  // TCP_KEEPIDLE
	keepIntvlSec  int32  // TCP_KEEPINTVL
	keepCnt       int32  // TCP_KEEPCNT
	userTimeoutMs uint32 // TCP_USER_TIMEOUT (milliseconds; 0 = system default)

	// kick, when set (at connection establishment), re-arms the owning wire
	// end's keepalive prober after an option write — so enabling SO_KEEPALIVE
	// through a stashed RawConn starts probing, exactly as the kernel's
	// keepalive timer arms on the setsockopt. Called without the mutex held.
	kick func()
}

func newDstSockOpts() *dstSockOpts {
	return &dstSockOpts{
		keepIdleSec:  dstKeepIdleDefaultSec,
		keepIntvlSec: dstKeepIntvlDefaultSec,
		keepCnt:      dstKeepCntDefault,
	}
}

// cloneForChild snapshots the listener's option state for an accepted
// connection — accept(2) inheritance (tcp(7) documents it for
// TCP_USER_TIMEOUT; SOL_SOCKET and the keepalive parameters inherit the same
// way). The snapshot is taken at establishment; production clones at child
// creation (the handshake), an unobservable difference here since the
// ListenConfig fields are immutable after Listen.
func (o *dstSockOpts) cloneForChild() *dstSockOpts {
	o.mu.Lock()
	defer o.mu.Unlock()
	return &dstSockOpts{
		keepAlive:     o.keepAlive,
		keepIdleSec:   o.keepIdleSec,
		keepIntvlSec:  o.keepIntvlSec,
		keepCnt:       o.keepCnt,
		userTimeoutMs: o.userTimeoutMs,
	}
}

func (o *dstSockOpts) setKick(fn func()) {
	o.mu.Lock()
	o.kick = fn
	kicked := o.keepAlive
	o.mu.Unlock()
	if kicked && fn != nil {
		fn()
	}
}

func (o *dstSockOpts) kicked() {
	o.mu.Lock()
	fn := o.kick
	o.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// applyGoKeepAlive applies the Go-level keepalive configuration with
// production's exact resolution (newTCPConn + SetKeepAliveConfig +
// setKeepAliveIdle/Interval/Count): a non-Enable config with a nonnegative
// KeepAlive duration becomes {Enable, Idle: KeepAlive}; an Enable'd config
// sets SO_KEEPALIVE and then each parameter — a zero field takes the Go
// default (15s/15s/9), a negative field leaves the socket state (a Control
// callback's writes, or the kernel defaults) unchanged. A disabled resolution
// applies NOTHING — production never clears an option Control enabled.
// Durations round up to whole seconds, as the kernel's second-granular
// options do.
func (o *dstSockOpts) applyGoKeepAlive(idle time.Duration, cfg KeepAliveConfig) {
	if !cfg.Enable && idle >= 0 {
		cfg = KeepAliveConfig{Enable: true, Idle: idle}
	}
	if !cfg.Enable {
		return
	}
	o.mu.Lock()
	o.keepAlive = true
	if cfg.Idle == 0 {
		cfg.Idle = defaultTCPKeepAliveIdle
	}
	if cfg.Idle > 0 {
		o.keepIdleSec = dstKeepSecs(cfg.Idle, dstKeepIdleMaxSec)
	}
	if cfg.Interval == 0 {
		cfg.Interval = defaultTCPKeepAliveInterval
	}
	if cfg.Interval > 0 {
		o.keepIntvlSec = dstKeepSecs(cfg.Interval, dstKeepIntvlMaxSec)
	}
	if cfg.Count == 0 {
		cfg.Count = defaultTCPKeepAliveCount
	}
	if cfg.Count > 0 {
		n := cfg.Count
		if n > dstKeepCntMax {
			n = dstKeepCntMax
		}
		o.keepCnt = int32(n)
	}
	o.mu.Unlock()
	o.kicked()
}

// dstKeepSecs rounds a keepalive duration up to whole seconds and clamps to
// the kernel bound (production's setsockopt would fail EINVAL beyond it; the
// Go-level path never produces such values in practice, and clamping keeps
// this internal boundary total).
func dstKeepSecs(d time.Duration, maxSec int32) int32 {
	secs := int64(roundDurationUp(d, time.Second))
	if secs < 1 {
		secs = 1
	}
	if secs > int64(maxSec) {
		secs = int64(maxSec)
	}
	return int32(secs)
}

// kaParams reads the keepalive law's inputs, in nanoseconds, at the instant
// of a probe decision — live, so a post-establishment option write takes
// effect on the next probe, as the kernel's timer reads its socket fields.
func (o *dstSockOpts) kaParams() (enabled bool, idleNs, intvlNs, cnt, utoNs int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.keepAlive,
		int64(o.keepIdleSec) * int64(time.Second),
		int64(o.keepIntvlSec) * int64(time.Second),
		int64(o.keepCnt),
		int64(o.userTimeoutMs) * int64(time.Millisecond)
}

// userTimeoutNs reports the socket's TCP_USER_TIMEOUT in nanoseconds (0 =
// system default: the run's RetransmitTimeout governs).
func (o *dstSockOpts) userTimeoutNs() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return int64(o.userTimeoutMs) * int64(time.Millisecond)
}

// The virtual socket-descriptor registry: the numbers handed to Control
// callbacks and SyscallConn users, routing their raw sockopt syscalls back to
// the owning dstSockOpts. The range is reserved in the syscall package
// (disjoint from and above the virtual FILE range); the constants are
// declared in both packages, shared by construction. Freed numbers are
// REUSED (most-recently-freed first — deterministic, as the free order is
// schedule-determined): a real kernel reuses closed descriptors, so a
// long-lived churn of dials must never exhaust the space — a sim-only
// EMFILE would be a false failure. A stashed RawConn of a closed socket can
// therefore alias a later socket, exactly production's stale-fd hazard.
// Keyed by the run epoch like the listener registry, so a new run starts
// empty and stale descriptors answer as closed.
const (
	dstVirtualSockFDBase  = 1<<30 + 1<<20
	dstVirtualSockFDCount = 1 << 20
)

var dstSockFDs struct {
	mu    sync.Mutex
	epoch uint64
	next  int
	freed []int
	byFD  map[int]*dstSockOpts
}

func dstSockFDRoll() {
	if e := dstNetEpoch(); e != dstSockFDs.epoch || dstSockFDs.byFD == nil {
		dstSockFDs.epoch = e
		dstSockFDs.next = 0
		dstSockFDs.freed = nil
		dstSockFDs.byFD = make(map[int]*dstSockOpts)
	}
}

// dstSockFDAlloc issues a virtual socket descriptor for opts, reusing a
// freed number when one exists. ok=false means 2^20 descriptors are LIVE at
// once (EMFILE at the caller, a process genuinely out of descriptors).
func dstSockFDAlloc(opts *dstSockOpts) (fd int, ok bool) {
	dstSockFDs.mu.Lock()
	defer dstSockFDs.mu.Unlock()
	dstSockFDRoll()
	if n := len(dstSockFDs.freed); n > 0 {
		fd = dstSockFDs.freed[n-1]
		dstSockFDs.freed = dstSockFDs.freed[:n-1]
		dstSockFDs.byFD[fd] = opts
		return fd, true
	}
	if dstSockFDs.next >= dstVirtualSockFDCount {
		return 0, false
	}
	fd = dstVirtualSockFDBase + dstSockFDs.next
	dstSockFDs.next++
	dstSockFDs.byFD[fd] = opts
	return fd, true
}

// dstSockFDFree retires a descriptor (socket closed), returning its number
// to the free list. Guarded on OWNERSHIP, not mere liveness: a socket's
// teardown can be reached twice (Close and a reset), and with reuse a
// liveness guard would let the second free — arriving after the number was
// reissued — unregister the NEW socket and double-list the number. The
// owner compare makes that unrepresentable: only the socket the registry
// currently maps the number to may free it.
func dstSockFDFree(fd int, owner *dstSockOpts) {
	dstSockFDs.mu.Lock()
	defer dstSockFDs.mu.Unlock()
	dstSockFDRoll()
	if owner == nil || dstSockFDs.byFD[fd] != owner {
		return
	}
	delete(dstSockFDs.byFD, fd)
	dstSockFDs.freed = append(dstSockFDs.freed, fd)
}

func dstSockFDLookup(fd int) *dstSockOpts {
	dstSockFDs.mu.Lock()
	defer dstSockFDs.mu.Unlock()
	dstSockFDRoll()
	return dstSockFDs.byFD[fd]
}

// dstRawSockConn is the syscall.RawConn over a simulated socket: Control
// hands the callback the socket's virtual descriptor, whose raw sockopt
// syscalls the interception boundary routes to the option layer. Read/Write
// — production's readiness loops over the raw fd — require byte-level socket
// access the model does not provide and are refused loudly, in the raw-*
// OpError shapes production uses for rawConn failures.
type dstRawSockConn struct {
	fd      int
	network string
	laddr   Addr // may be nil (a dialing socket before establishment)
	raddr   Addr
	closed  func() bool
}

func (c *dstRawSockConn) opError(op string, err error) error {
	return &OpError{Op: op, Net: c.network, Source: dstCloneAddr(c.laddr), Addr: dstCloneAddr(c.raddr), Err: err}
}

func (c *dstRawSockConn) Control(f func(fd uintptr)) error {
	if (c.closed != nil && c.closed()) || dstSockFDLookup(c.fd) == nil {
		return c.opError("raw-control", errClosed)
	}
	f(uintptr(c.fd))
	return nil
}

func (c *dstRawSockConn) Read(f func(fd uintptr) bool) error {
	return c.opError("raw-read", errors.New("raw socket read unsupported under deterministic simulation"))
}

func (c *dstRawSockConn) Write(f func(fd uintptr) bool) error {
	return c.opError("raw-write", errors.New("raw socket write unsupported under deterministic simulation"))
}

// SyscallConn returns a raw network connection over the simulated socket,
// implementing syscall.Conn as *TCPConn does. Control works (sockopts on the
// modeled set); Read/Write are refused (see dstRawSockConn).
func (c *dstConn) SyscallConn() (syscall.RawConn, error) {
	if c.closed.Load() || c.stale() {
		return nil, syscall.EINVAL
	}
	return &dstRawSockConn{
		fd:      c.sockFD,
		network: c.network,
		laddr:   c.local,
		raddr:   c.remote,
		closed:  func() bool { return c.closed.Load() || c.stale() },
	}, nil
}

// SyscallConn returns a raw network connection over the simulated listener's
// socket, implementing syscall.Conn as *TCPListener does.
func (l *dstListener) SyscallConn() (syscall.RawConn, error) {
	if l.closed.Load() || l.stale() {
		return nil, syscall.EINVAL
	}
	return &dstRawSockConn{
		fd:      l.sockFD,
		network: l.network,
		raddr:   l.addr,
		closed:  func() bool { return l.closed.Load() || l.stale() },
	}, nil
}

// dstRunControl invokes a dial/listen Control (or ControlContext) callback
// against the socket's virtual descriptor. The network and address strings
// are production's: the resolved family form and the target address. A
// callback error aborts the surrounding dial/listen, wrapped by the caller
// exactly as production wraps a ctrlFn failure (the caller's error path
// also frees the descriptor — the socket dies with the failed dial/listen).
func dstRunControl(ctx context.Context, ctrlCtx func(context.Context, string, string, syscall.RawConn) error, ctrl func(string, string, syscall.RawConn) error, network, address string, fd int) error {
	raw := &dstRawSockConn{fd: fd, network: network}
	if ctrlCtx != nil {
		return ctrlCtx(ctx, network, address, raw)
	}
	if ctrl != nil {
		return ctrl(network, address, raw)
	}
	return nil
}
