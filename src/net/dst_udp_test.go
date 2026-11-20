// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package net

import (
	"bytes"
	"context"
	"errors"
	"internal/nettrace"
	"io"
	"math"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

func setReusePortControl(t *testing.T) func(network, address string, c syscall.RawConn) error {
	t.Helper()
	return func(network, address string, c syscall.RawConn) error {
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, dstSO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return sockErr
	}
}

func getsockoptIntRaw(t *testing.T, sc syscall.Conn, level, opt int) int {
	t.Helper()
	rc, err := sc.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var out int
	var sockErr error
	if err := rc.Control(func(fd uintptr) {
		out, sockErr = syscall.GetsockoptInt(int(fd), level, opt)
	}); err != nil {
		t.Fatalf("RawConn.Control: %v", err)
	}
	if sockErr != nil {
		t.Fatalf("GetsockoptInt(%d, %d): %v", level, opt, sockErr)
	}
	return out
}

func TestDSTUDPEntryPointsFamiliesAndTCPCoexistence(t *testing.T) {
	simulation.Test(t, 101, func(t *testing.T) {
		// Stream Listen on UDP networks fails with host Go's exact AddrError.
		if _, err := Listen("udp", "127.0.0.1:0"); err == nil {
			t.Fatal("Listen(udp) unexpectedly succeeded")
		} else {
			var addrErr *AddrError
			if !errors.As(err, &addrErr) || addrErr.Err != "unexpected address type" {
				t.Fatalf("Listen(udp) err = %v, want AddrError{unexpected address type}", err)
			}
		}

		// Dual-stack wildcard ListenPacket("udp", ":0") reports [::]:port and
		// accepts both IPv4 and IPv6 datagrams, with IPv4 mapped in AddrPort.
		pc, err := ListenPacket("udp", ":0")
		if err != nil {
			t.Fatalf("ListenPacket(udp, :0): %v", err)
		}
		defer pc.Close()
		uc := pc.(*UDPConn)
		la := uc.LocalAddr().(*UDPAddr)
		if !la.IP.Equal(IPv6zero) || la.Port < dstListenEphemeralStart {
			t.Fatalf("dual-stack LocalAddr = %v, want [::]:>=10000", la)
		}

		// TCP can bind the exact same port on the same host without conflict.
		tcpLn, err := Listen("tcp", ":"+strconv.Itoa(la.Port))
		if err != nil {
			t.Fatalf("TCP Listen on same port %d failed: %v", la.Port, err)
		}
		defer tcpLn.Close()

		// IPv4 client via Dial("udp4", ...) + nettrace.
		var starts, dones []string
		trace := &nettrace.Trace{
			ConnectStart: func(net, addr string) { starts = append(starts, net+"/"+addr) },
			ConnectDone:  func(net, addr string, err error) { dones = append(dones, net+"/"+addr) },
		}
		ctx := context.WithValue(context.Background(), nettrace.TraceKey{}, trace)
		var d Dialer
		c4, err := d.DialContext(ctx, "udp4", "localhost:"+strconv.Itoa(la.Port))
		if err != nil {
			t.Fatalf("DialContext(udp4): %v", err)
		}
		defer c4.Close()
		wantTrace := "udp4/127.0.0.1:" + strconv.Itoa(la.Port)
		if len(starts) != 1 || starts[0] != wantTrace || len(dones) != 1 || dones[0] != wantTrace {
			t.Fatalf("UDP nettrace = %v / %v, want [%q]", starts, dones, wantTrace)
		}
		if _, err := c4.Write([]byte("v4-hello")); err != nil {
			t.Fatalf("c4.Write: %v", err)
		}
		buf := make([]byte, 32)
		n, ap, err := uc.ReadFromUDPAddrPort(buf)
		if err != nil || string(buf[:n]) != "v4-hello" {
			t.Fatalf("ReadFromUDPAddrPort v4 = %q, %v", buf[:n], err)
		}
		if !ap.Addr().Is4In6() {
			t.Fatalf("dual-stack v4 sender AddrPort = %v, want IPv4-mapped IPv6", ap)
		}
		if _, err := uc.WriteToUDPAddrPort([]byte("v4-reply"), ap); err != nil {
			t.Fatalf("WriteToUDPAddrPort v4: %v", err)
		}
		n, err = c4.Read(buf)
		if err != nil || string(buf[:n]) != "v4-reply" {
			t.Fatalf("c4.Read = %q, %v", buf[:n], err)
		}

		// IPv6 client via (*Dialer).DialUDP.
		v6ap := netip.AddrPortFrom(netip.MustParseAddr("::1"), uint16(la.Port))
		c6, err := d.DialUDP(context.Background(), "udp6", netip.AddrPort{}, v6ap)
		if err != nil {
			t.Fatalf("Dialer.DialUDP(udp6): %v", err)
		}
		defer c6.Close()
		if _, err := c6.Write([]byte("v6-hello")); err != nil {
			t.Fatalf("c6.Write: %v", err)
		}
		n, from6, err := uc.ReadFromUDP(buf)
		if err != nil || string(buf[:n]) != "v6-hello" || !from6.IP.Equal(IPv6loopback) {
			t.Fatalf("ReadFromUDP v6 = %q from %v, err=%v", buf[:n], from6, err)
		}

		// udp6 socket is IPV6_V6ONLY: sending or dialing IPv4 fails ENETUNREACH;
		// udp4 and udp6 single-family wildcards can share the same port.
		u4, err := ListenUDP("udp4", &UDPAddr{IP: IPv4zero, Port: 0})
		if err != nil {
			t.Fatalf("ListenUDP(udp4): %v", err)
		}
		defer u4.Close()
		pShared := u4.LocalAddr().(*UDPAddr).Port
		u6, err := ListenUDP("udp6", &UDPAddr{IP: IPv6zero, Port: pShared})
		if err != nil {
			t.Fatalf("ListenUDP(udp6) on udp4 port %d failed: %v", pShared, err)
		}
		defer u6.Close()
		if _, err := u6.WriteToUDP([]byte("x"), &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: pShared}); !errors.Is(err, syscall.ENETUNREACH) {
			t.Fatalf("udp6 WriteTo IPv4 = %v, want ENETUNREACH", err)
		}
		if _, err := DialUDP("udp6", nil, &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: pShared}); !errors.Is(err, syscall.ENETUNREACH) {
			t.Fatalf("DialUDP(udp6, v4) = %v, want ENETUNREACH", err)
		}

		// Unowned local IP fails EADDRNOTAVAIL.
		if _, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(10, 0, 0, 99), Port: 0}); !errors.Is(err, syscall.EADDRNOTAVAIL) {
			t.Fatalf("ListenUDP unowned IP = %v, want EADDRNOTAVAIL", err)
		}
	})
}

func TestDSTUDPReusePortReuseAddrAndLookupPrecedence(t *testing.T) {
	simulation.Test(t, 202, func(t *testing.T) {
		// TCP refuses SO_REUSEPORT with ENOPROTOOPT.
		tcpLC := ListenConfig{Control: setReusePortControl(t)}
		if _, err := tcpLC.Listen(context.Background(), "tcp", "127.0.0.1:0"); !errors.Is(err, syscall.ENOPROTOOPT) {
			t.Fatalf("TCP Listen with SO_REUSEPORT = %v, want ENOPROTOOPT", err)
		}

		// Default UDP SO_REUSEADDR=0: wildcard + specific IP on same port conflicts.
		w0, err := ListenPacket("udp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		port0 := w0.LocalAddr().(*UDPAddr).Port
		if _, err := ListenPacket("udp4", "127.0.0.1:"+strconv.Itoa(port0)); !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("specific bind over non-reuseaddr wildcard = %v, want EADDRINUSE", err)
		}
		w0.Close()

		// With SO_REUSEADDR=1 on both, wildcard + specific IP coexist, and
		// specific IP beats wildcard on lookup; connected 4-tuple beats both.
		raLC := ListenConfig{Control: setReuseAddrControl(1)}
		wConn, err := raLC.ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		defer wConn.Close()
		sharedPort := wConn.LocalAddr().(*UDPAddr).Port
		spConn, err := raLC.ListenPacket(context.Background(), "udp4", "127.0.0.1:"+strconv.Itoa(sharedPort))
		if err != nil {
			t.Fatalf("SO_REUSEADDR specific+wildcard bind failed: %v", err)
		}
		defer spConn.Close()

		sender, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer sender.Close()
		if _, err := sender.WriteToUDP([]byte("to-specific"), &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: sharedPort}); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 32)
		n, _, err := spConn.(*UDPConn).ReadFromUDP(buf)
		if err != nil || string(buf[:n]) != "to-specific" {
			t.Fatalf("specific socket did not win over wildcard: %q, %v", buf[:n], err)
		}

		// SO_REUSEPORT multi-worker group on identical 127.0.0.1:port.
		rpLC := ListenConfig{Control: setReusePortControl(t)}
		const workers = 4
		socks := make([]*UDPConn, workers)
		var groupPort int
		for i := 0; i < workers; i++ {
			addr := "127.0.0.1:0"
			if i > 0 {
				addr = "127.0.0.1:" + strconv.Itoa(groupPort)
			}
			pc, err := rpLC.ListenPacket(context.Background(), "udp4", addr)
			if err != nil {
				t.Fatalf("SO_REUSEPORT worker %d bind: %v", i, err)
			}
			defer pc.Close()
			socks[i] = pc.(*UDPConn)
			if i == 0 {
				groupPort = socks[0].LocalAddr().(*UDPAddr).Port
			}
		}

		// Non-SO_REUSEPORT socket attempting to join the group fails EADDRINUSE.
		if _, err := ListenPacket("udp4", "127.0.0.1:"+strconv.Itoa(groupPort)); !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("non-SO_REUSEPORT join = %v, want EADDRINUSE", err)
		}

		// Connected socket with SO_REUSEPORT on the same local port beats
		// unconnected SO_REUSEPORT workers for its exact peer 4-tuple.
		specialClient, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer specialClient.Close()
		rpDialer := Dialer{
			LocalAddr: &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: groupPort},
			Control:   setReusePortControl(t),
		}
		connWorker, err := rpDialer.DialContext(context.Background(), "udp4", specialClient.LocalAddr().String())
		if err != nil {
			t.Fatalf("connected SO_REUSEPORT dial: %v", err)
		}
		defer connWorker.Close()
		if _, err := specialClient.WriteToUDP([]byte("for-connected"), &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: groupPort}); err != nil {
			t.Fatal(err)
		}
		n, err = connWorker.Read(buf)
		if err != nil || string(buf[:n]) != "for-connected" {
			t.Fatalf("connected 4-tuple worker did not win: %q, %v", buf[:n], err)
		}

		// 64 distinct unconnected clients distribute across all 4 workers and
		// each client hashes stickily to the same worker on repeat sends.
		hits := make([]int, workers)
		target := &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: groupPort}
		readOne := func() int {
			for idx, w := range socks {
				_ = w.SetReadDeadline(time.Now().Add(time.Millisecond))
				n, _, err := w.ReadFromUDP(buf)
				_ = w.SetReadDeadline(time.Time{})
				if err == nil && n > 0 {
					return idx
				}
			}
			t.Fatal("no SO_REUSEPORT worker received datagram")
			return -1
		}
		for c := 0; c < 64; c++ {
			cli, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 0})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cli.WriteToUDP([]byte("p1"), target); err != nil {
				t.Fatal(err)
			}
			firstIdx := readOne()
			if _, err := cli.WriteToUDP([]byte("p2"), target); err != nil {
				t.Fatal(err)
			}
			secondIdx := readOne()
			if firstIdx != secondIdx {
				t.Fatalf("client %d hashed to worker %d then %d; want sticky 4-tuple routing", c, firstIdx, secondIdx)
			}
			hits[firstIdx]++
			cli.Close()
		}
		for i, count := range hits {
			if count == 0 {
				t.Fatalf("SO_REUSEPORT worker %d received 0 of 64 flows (hits=%v)", i, hits)
			}
		}
	})
}

func TestDSTUDPFramingTruncationBuffersAndFences(t *testing.T) {
	simulation.Test(t, 303, func(t *testing.T) {
		srv, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()
		srvAddr := srv.LocalAddr().(*UDPAddr)

		cli, err := DialUDP("udp4", nil, srvAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer cli.Close()

		// Exact framing + 0-byte datagram (never io.EOF).
		if _, err := cli.Write([]byte("abcd")); err != nil {
			t.Fatal(err)
		}
		if n, err := cli.Write(nil); err != nil || n != 0 {
			t.Fatalf("0-byte Write = (%d, %v), want (0, nil)", n, err)
		}
		if _, err := cli.Write([]byte("efghi")); err != nil {
			t.Fatal(err)
		}

		buf := make([]byte, 16)
		if n, _, err := srv.ReadFromUDP(buf); err != nil || string(buf[:n]) != "abcd" {
			t.Fatalf("frame 1 = (%q, %v), want abcd", buf[:n], err)
		}
		if n, _, err := srv.ReadFromUDP(buf); err != nil || n != 0 {
			t.Fatalf("0-byte frame 2 = (%d, %v), want (0, nil) — never EOF", n, err)
		}
		// Short-read truncation via ReadMsgUDP sets syscall.MSG_TRUNC.
		short := make([]byte, 3)
		n, oobn, flags, from, err := srv.ReadMsgUDP(short, nil)
		if err != nil || n != 3 || oobn != 0 || flags&syscall.MSG_TRUNC == 0 || string(short) != "efg" || from == nil {
			t.Fatalf("ReadMsgUDP short = (n=%d oobn=%d flags=%#x data=%q from=%v err=%v), want 3, MSG_TRUNC, efg",
				n, oobn, flags, short[:n], from, err)
		}

		// Max payload boundaries: 65507B IPv4 ok, 65508B EMSGSIZE; 65527B IPv6 ok, 65528B EMSGSIZE.
		maxV4 := bytes.Repeat([]byte{0x5a}, dstUDPMaxIPv4Payload)
		if n, err := cli.Write(maxV4); err != nil || n != len(maxV4) {
			t.Fatalf("max IPv4 UDP write = (%d, %v)", n, err)
		}
		bigBuf := make([]byte, 65536)
		if n, _, err := srv.ReadFromUDP(bigBuf); err != nil || n != len(maxV4) || !bytes.Equal(bigBuf[:n], maxV4) {
			t.Fatalf("max IPv4 UDP read = (%d, %v)", n, err)
		}
		if _, err := cli.Write(make([]byte, dstUDPMaxIPv4Payload+1)); !errors.Is(err, syscall.EMSGSIZE) {
			t.Fatalf("oversize IPv4 UDP write = %v, want EMSGSIZE", err)
		}

		srv6, err := ListenUDP("udp6", &UDPAddr{IP: IPv6loopback, Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer srv6.Close()
		cli6, err := DialUDP("udp6", nil, srv6.LocalAddr().(*UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		defer cli6.Close()
		if err := srv6.SetReadBuffer(128 << 10); err != nil {
			t.Fatal(err)
		}
		maxV6 := bytes.Repeat([]byte{0xa5}, dstUDPMaxIPv6Payload)
		if n, err := cli6.Write(maxV6); err != nil || n != len(maxV6) {
			t.Fatalf("max IPv6 UDP write = (%d, %v)", n, err)
		}
		if n, _, err := srv6.ReadFromUDP(bigBuf); err != nil || n != len(maxV6) {
			t.Fatalf("max IPv6 UDP read = (%d, %v)", n, err)
		}
		if _, err := cli6.Write(make([]byte, dstUDPMaxIPv6Payload+1)); !errors.Is(err, syscall.EMSGSIZE) {
			t.Fatalf("oversize IPv6 UDP write = %v, want EMSGSIZE", err)
		}

		// SO_RCVBUF / SO_SNDBUF Linux doubling + clamping and overflow tail-drop without sender backpressure.
		if err := srv.SetReadBuffer(4096); err != nil {
			t.Fatalf("SetReadBuffer(4096): %v", err)
		}
		if got := getsockoptIntRaw(t, srv, syscall.SOL_SOCKET, syscall.SO_RCVBUF); got != 8192 {
			t.Fatalf("SO_RCVBUF doubled = %d, want 8192", got)
		}
		if err := srv.SetReadBuffer(100); err != nil { // min(100,8M)*2 = 200 -> clamped to dstSockBufMin = 2304
			t.Fatalf("SetReadBuffer: %v", err)
		}
		if err := srv.SetWriteBuffer(100); err != nil {
			t.Fatalf("SetWriteBuffer: %v", err)
		}
		if got := getsockoptIntRaw(t, srv, syscall.SOL_SOCKET, syscall.SO_RCVBUF); got != dstSockBufMin {
			t.Fatalf("SO_RCVBUF clamped = %d, want %d", got, dstSockBufMin)
		}
		if got := getsockoptIntRaw(t, srv, syscall.SOL_SOCKET, syscall.SO_SNDBUF); got != dstSockBufMin {
			t.Fatalf("SO_SNDBUF clamped = %d, want %d", got, dstSockBufMin)
		}

		// IPv6 scope Zone ("lo" / "eth0") resolves statelessly without host netlink syscalls.
		if _, err := srv6.WriteToUDP([]byte("zoned"), &UDPAddr{IP: IPv6loopback, Port: srv6.LocalAddr().(*UDPAddr).Port, Zone: "lo"}); err != nil {
			t.Fatalf("WriteToUDP with IPv6 Zone=lo: %v", err)
		}
		if n, _, err := srv6.ReadFromUDP(buf); err != nil || string(buf[:n]) != "zoned" {
			t.Fatalf("ReadFromUDP zoned = (%q, %v)", buf[:n], err)
		}

		// Dialer with MultipathTCP enabled is ignored on UDP dials (matching stock Go),
		// and Control callbacks receive stock pre-bind address strings (":0", ":<port>").
		var listenCtrlAddr, dialCtrlAddr string
		ctrlLC := ListenConfig{Control: func(_, addr string, _ syscall.RawConn) error {
			listenCtrlAddr = addr
			return nil
		}}
		wpc, err := ctrlLC.ListenPacket(context.Background(), "udp4", ":0")
		if err != nil {
			t.Fatalf("ListenPacket(:0) with Control: %v", err)
		}
		wpcPort := wpc.LocalAddr().(*UDPAddr).Port
		wpc.Close()
		if listenCtrlAddr != ":0" {
			t.Fatalf("ListenPacket(:0) Control addr = %q, want \":0\"", listenCtrlAddr)
		}
		var mpDialer Dialer
		mpDialer.SetMultipathTCP(true)
		mpDialer.Control = func(_, addr string, _ syscall.RawConn) error {
			dialCtrlAddr = addr
			return nil
		}
		mpConn, err := mpDialer.DialContext(context.Background(), "udp4", ":"+strconv.Itoa(wpcPort))
		if err != nil {
			t.Fatalf("Dialer with MultipathTCP on udp4 failed: %v", err)
		}
		mpConn.Close()
		if dialCtrlAddr != ":"+strconv.Itoa(wpcPort) {
			t.Fatalf("Dial(:port) Control addr = %q, want %q", dialCtrlAddr, ":"+strconv.Itoa(wpcPort))
		}
		// 2304B fits two 1000B datagrams (2000B <= 2304B) and drops the third (3000B > 2304B).
		for i := 0; i < 4; i++ {
			pkt := bytes.Repeat([]byte{byte('A' + i)}, 1000)
			if n, err := cli.Write(pkt); err != nil || n != 1000 {
				t.Fatalf("overflow sender Write %d = (%d, %v), want non-blocking success", i, n, err)
			}
		}
		for i := 0; i < 2; i++ {
			n, _, err := srv.ReadFromUDP(bigBuf)
			if err != nil || n != 1000 || bigBuf[0] != byte('A'+i) {
				t.Fatalf("queued packet %d = (%d, %c, %v)", i, n, bigBuf[0], err)
			}
		}
		_ = srv.SetReadDeadline(time.Now().Add(time.Millisecond))
		if _, _, err := srv.ReadFromUDP(bigBuf); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("expected 3rd/4th packets dropped by SO_RCVBUF, got %v", err)
		}
		_ = srv.SetReadDeadline(time.Time{})
		// Draining freed buffer space: new datagrams deliver again.
		if _, err := cli.Write([]byte("recovered")); err != nil {
			t.Fatal(err)
		}
		if n, _, err := srv.ReadFromUDP(buf); err != nil || string(buf[:n]) != "recovered" {
			t.Fatalf("post-drain ReadFromUDP = (%q, %v)", buf[:n], err)
		}

		// Error & fence identities:
		if _, err := srv.Write([]byte("x")); !errors.Is(err, syscall.EDESTADDRREQ) {
			t.Fatalf("Write on unconnected UDP = %v, want EDESTADDRREQ", err)
		}
		if _, err := cli.WriteToUDP([]byte("x"), srvAddr); !errors.Is(err, ErrWriteToConnected) {
			t.Fatalf("WriteToUDP on connected UDP = %v, want ErrWriteToConnected", err)
		}
		if _, err := srv.WriteToUDP([]byte("x"), &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 0}); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("WriteToUDP port 0 = %v, want EINVAL", err)
		}
		if err := srv.fd.closeRead(); !errors.Is(err, syscall.ENOTCONN) {
			t.Fatalf("unconnected closeRead = %v, want ENOTCONN", err)
		}
		if err := cli.fd.closeWrite(); !errors.Is(err, syscall.EOPNOTSUPP) {
			t.Fatalf("connected closeWrite = %v, want EOPNOTSUPP", err)
		}
		if _, _, _, _, err := srv.ReadMsgUDP(buf, make([]byte, 16)); err == nil || !strings.Contains(err.Error(), "unsupported under deterministic simulation") {
			t.Fatalf("ReadMsgUDP with oob = %v, want unsupported fence", err)
		}
		if _, _, err := srv.WriteMsgUDP([]byte("x"), make([]byte, 16), srvAddr); err == nil || !strings.Contains(err.Error(), "unsupported under deterministic simulation") {
			t.Fatalf("WriteMsgUDP with oob = %v, want unsupported fence", err)
		}
		if _, err := srv.File(); err == nil || !strings.Contains(err.Error(), "unsupported under deterministic simulation") {
			t.Fatalf("UDPConn.File() = %v, want unsupported fence", err)
		}
		rc, err := srv.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		if err := rc.Read(func(uintptr) bool { return true }); err == nil || !strings.Contains(err.Error(), "unsupported under deterministic simulation") {
			t.Fatalf("RawConn.Read = %v, want unsupported fence", err)
		}
		if err := rc.Write(func(uintptr) bool { return true }); err == nil || !strings.Contains(err.Error(), "unsupported under deterministic simulation") {
			t.Fatalf("RawConn.Write = %v, want unsupported fence", err)
		}
	})
}

func TestDSTUDPDeadlinesRespectStepAndDrift(t *testing.T) {
	simulation.Test(t, 404, func(t *testing.T) {
		simulation.Host("drifted", func() {
			c, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 0})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()

			// StepClock shifts wall time only, not relative monotonic deadlines (DST-CLOCK-DURATION).
			start := time.Now()
			if err := c.SetReadDeadline(start.Add(40 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			go func() {
				time.Sleep(10 * time.Millisecond)
				simulation.StepClock("drifted", 5*time.Second)
			}()
			buf := make([]byte, 8)
			_, _, err = c.ReadFromUDP(buf)
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("ReadFromUDP deadline = %v, want ErrDeadlineExceeded", err)
			}

			// DriftClock at +100% rate (+1e9 ppb => 2x host clock): a 100ms host
			// deadline fires in 50ms of base time (100ms on the host's own clock).
			simulation.DriftClock("drifted", 1_000_000_000)
			t0 := time.Now()
			if err := c.SetReadDeadline(t0.Add(100 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			_, _, err = c.ReadFromUDP(buf)
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("drifted ReadFromUDP deadline = %v", err)
			}
			hostElapsed := time.Since(t0)
			if hostElapsed < 100*time.Millisecond || hostElapsed > 105*time.Millisecond {
				t.Fatalf("drifted host elapsed = %v, want ~100ms", hostElapsed)
			}
		})
	})
}

func TestDSTUDPCrossHostFaultsAndTCPImmunity(t *testing.T) {
	// Validate LinkConfig bounds panic state-neutrally.
	for _, bad := range []simulation.LinkConfig{
		{PacketLoss: -0.01},
		{PacketLoss: 1.01},
		{PacketLoss: math.NaN()},
		{PacketDuplicate: -0.1},
		{PacketDuplicate: 1.5},
		{PacketDuplicate: math.NaN()},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for invalid LinkConfig %+v", bad)
				}
			}()
			simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHost: bad}}, func() {})
		}()
	}

	simulation.Test(t, 505, func(t *testing.T) {
		var srvIP string
		simulation.Host("srv", func() { srvIP = simulation.HostIP("srv") })
		simulation.Host("cli", func() {})

		// 1. PacketReorder=false clamps per-flow delivery monotonically under jitter;
		//    PacketReorder=true permits smaller-jitter packets to overtake earlier ones.
		runOrderProbe := func(reorder bool) bool {
			var outOfOrder bool
			done := make(chan struct{})
			portCh := make(chan int, 1)
			simulation.ConfigureLink("cli", "srv", simulation.LinkConfig{
				Latency:       5 * time.Millisecond,
				Jitter:        40 * time.Millisecond,
				PacketReorder: reorder,
			})
			go simulation.Host("srv", func() {
				defer close(done)
				u, err := ListenUDP("udp4", &UDPAddr{IP: ParseIP(srvIP), Port: 0})
				if err != nil {
					t.Error(err)
					return
				}
				defer u.Close()
				portCh <- u.LocalAddr().(*UDPAddr).Port
				buf := make([]byte, 8)
				prev := -1
				for i := 0; i < 40; i++ {
					n, _, err := u.ReadFromUDP(buf)
					if err != nil {
						t.Error(err)
						return
					}
					seq, _ := strconv.Atoi(string(buf[:n]))
					if seq < prev {
						outOfOrder = true
					}
					prev = seq
				}
			})
			p := <-portCh
			simulation.Host("cli", func() {
				c, err := DialUDP("udp4", nil, &UDPAddr{IP: ParseIP(srvIP), Port: p})
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				for i := 0; i < 40; i++ {
					if _, err := c.Write([]byte(strconv.Itoa(i))); err != nil {
						t.Fatal(err)
					}
				}
			})
			<-done
			return outOfOrder
		}
		if runOrderProbe(false) {
			t.Fatal("PacketReorder=false reordered cross-host UDP datagrams")
		}
		if !runOrderProbe(true) {
			t.Fatal("PacketReorder=true never reordered across 40 jittered cross-host datagrams")
		}

		// 2. PacketLoss, PacketDuplicate, and TCP stream immunity on the exact same link!
		simulation.ConfigureLink("cli", "srv", simulation.LinkConfig{
			Latency:         2 * time.Millisecond,
			Jitter:          10 * time.Millisecond,
			PacketLoss:      0.35,
			PacketDuplicate: 0.50,
			PacketReorder:   true,
		})

		tcpDone := make(chan []byte, 1)
		tcpAddrCh := make(chan string, 1)
		udpPortCh := make(chan int, 1)
		udpDone := make(chan [2]int, 1) // [totalReceived, duplicates]

		go simulation.Host("srv", func() {
			ln, err := Listen("tcp", srvIP+":0")
			if err != nil {
				t.Error(err)
				return
			}
			defer ln.Close()
			tcpAddrCh <- ln.Addr().String()
			go func() {
				c, err := ln.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close()
				data, err := io.ReadAll(c)
				if err != nil {
					t.Error(err)
				}
				tcpDone <- data
			}()

			u, err := ListenUDP("udp4", &UDPAddr{IP: ParseIP(srvIP), Port: 0})
			if err != nil {
				t.Error(err)
				return
			}
			defer u.Close()
			udpPortCh <- u.LocalAddr().(*UDPAddr).Port
			seen := make(map[int]int)
			total, dups := 0, 0
			buf := make([]byte, 16)
			for {
				_ = u.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				n, _, err := u.ReadFromUDP(buf)
				if err != nil {
					break
				}
				id, _ := strconv.Atoi(string(buf[:n]))
				total++
				seen[id]++
				if seen[id] > 1 {
					dups++
				}
			}
			udpDone <- [2]int{total, dups}
		})

		tcpAddr := <-tcpAddrCh
		udpPort := <-udpPortCh
		var wantTCP bytes.Buffer
		simulation.Host("cli", func() {
			tc, err := Dial("tcp", tcpAddr)
			if err != nil {
				t.Fatal(err)
			}
			uc, err := DialUDP("udp4", nil, &UDPAddr{IP: ParseIP(srvIP), Port: udpPort})
			if err != nil {
				t.Fatal(err)
			}
			defer uc.Close()
			for i := 0; i < 80; i++ {
				chunk := []byte("chunk-" + strconv.Itoa(i) + ";")
				wantTCP.Write(chunk)
				if _, err := tc.Write(chunk); err != nil {
					t.Fatal(err)
				}
				if _, err := uc.Write([]byte(strconv.Itoa(i))); err != nil {
					t.Fatal(err)
				}
			}
			tc.Close()
		})

		gotTCP := <-tcpDone
		if !bytes.Equal(gotTCP, wantTCP.Bytes()) {
			t.Fatalf("TCP stream corrupted/reordered/dropped by UDP datagram LinkConfig! got %dB, want %dB", len(gotTCP), wantTCP.Len())
		}
		stats := <-udpDone
		if stats[0] == 0 || stats[1] == 0 || stats[0]-stats[1] >= 80 {
			t.Fatalf("UDP loss+dup stats = total=%d dups=%d unique=%d; expected both losses (<80 unique) and duplicates (>0)",
				stats[0], stats[1], stats[0]-stats[1])
		}
		simulation.UnconfigureLink("cli", "srv")
	})
}

func TestDSTUDPMidFlightPartitionAndConnectedRefused(t *testing.T) {
	simulation.Test(t, 606, func(t *testing.T) {
		var h1IP, h2IP string
		simulation.Host("h1", func() { h1IP = simulation.HostIP("h1") })
		simulation.Host("h2", func() { h2IP = simulation.HostIP("h2") })
		_ = h1IP
		simulation.ConfigureLink("h1", "h2", simulation.LinkConfig{Latency: 20 * time.Millisecond})

		// 1. Arrived-before-cut survives; mid-flight datagram is permanently dropped
		//    (not delivered even after HealLink, because UDP never retransmits).
		portCh := make(chan int, 1)
		srvRes := make(chan []string, 1)
		go simulation.Host("h2", func() {
			u, err := ListenUDP("udp4", &UDPAddr{IP: ParseIP(h2IP), Port: 0})
			if err != nil {
				t.Error(err)
				return
			}
			defer u.Close()
			portCh <- u.LocalAddr().(*UDPAddr).Port
			time.Sleep(100 * time.Millisecond) // read after cut + heal
			var got []string
			buf := make([]byte, 32)
			for {
				_ = u.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				n, _, err := u.ReadFromUDP(buf)
				if err != nil {
					break
				}
				got = append(got, string(buf[:n]))
			}
			srvRes <- got
		})
		p := <-portCh
		simulation.Host("h1", func() {
			c, err := DialUDP("udp4", nil, &UDPAddr{IP: ParseIP(h2IP), Port: p})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, _ = c.Write([]byte("before-cut")) // sent t=0, arrives t=20ms
			time.Sleep(25 * time.Millisecond)    // t=25ms: "before-cut" already in h2's rcvbuf
			_, _ = c.Write([]byte("mid-flight")) // sent t=25ms, would arrive t=45ms
			time.Sleep(5 * time.Millisecond)     // t=30ms: cut while "mid-flight" is on the wire!
			simulation.Partition("h1", "h2")
			_, _ = c.Write([]byte("during-cut"))
			time.Sleep(30 * time.Millisecond) // t=60ms: heal
			simulation.HealLink("h1", "h2")
			_, _ = c.Write([]byte("after-heal")) // arrives t=80ms
		})
		got := <-srvRes
		if len(got) != 2 || got[0] != "before-cut" || got[1] != "after-heal" {
			t.Fatalf("mid-flight partition received %v, want [before-cut after-heal]", got)
		}

		// 2. Connected UDP to unbound port on live host: Write succeeds, Read after
		//    1-RTT (40ms) returns ECONNREFUSED once, socket remains open and works
		//    once h2 binds the port!
		const unboundPort = 39123
		simulation.Host("h1", func() {
			c, err := DialUDP("udp4", nil, &UDPAddr{IP: ParseIP(h2IP), Port: unboundPort})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()

			t0 := time.Now()
			if n, err := c.Write([]byte("probe")); err != nil || n != 5 {
				t.Fatalf("connected Write to unbound port = (%d, %v), want (5, nil)", n, err)
			}
			buf := make([]byte, 32)
			_, err = c.Read(buf)
			rtt := time.Since(t0)
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("connected Read after unbound Write = %v, want ECONNREFUSED", err)
			}
			if rtt < 40*time.Millisecond {
				t.Fatalf("connected cross-host ECONNREFUSED arrived in %v, want >= 40ms (1 RTT)", rtt)
			}
			// Second Read does NOT repeat stale ECONNREFUSED or return EOF; it times out!
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			if _, err := c.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("second Read after ECONNREFUSED = %v, want ErrDeadlineExceeded (socket stays open)", err)
			}
			_ = c.SetReadDeadline(time.Time{})

			// Undeclared host (10.0.0.77) and blackhole-cut host emit NO ECONNREFUSED.
			cUnowned, err := DialUDP("udp4", nil, &UDPAddr{IP: IPv4(10, 0, 0, 77), Port: unboundPort})
			if err != nil {
				t.Fatal(err)
			}
			defer cUnowned.Close()
			_, _ = cUnowned.Write([]byte("into-void"))
			_ = cUnowned.SetReadDeadline(time.Now().Add(60 * time.Millisecond))
			if _, err := cUnowned.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("undeclared host Read = %v, want silence (ErrDeadlineExceeded)", err)
			}

			// Boot a listener on h2:unboundPort and verify the SAME socket c works!
			ready := make(chan struct{})
			go simulation.Host("h2", func() {
				srv, err := ListenUDP("udp4", &UDPAddr{IP: ParseIP(h2IP), Port: unboundPort})
				if err != nil {
					t.Error(err)
					return
				}
				defer srv.Close()
				close(ready)
				b := make([]byte, 32)
				n, from, err := srv.ReadFromUDP(b)
				if err == nil {
					_, _ = srv.WriteToUDP([]byte("echo:"+string(b[:n])), from)
				}
			})
			<-ready
			if _, err := c.Write([]byte("alive")); err != nil {
				t.Fatalf("Write on reused connected UDP socket: %v", err)
			}
			n, err := c.Read(buf)
			if err != nil || string(buf[:n]) != "echo:alive" {
				t.Fatalf("Read on reused connected UDP socket = (%q, %v), want echo:alive", buf[:n], err)
			}
		})
	})
}

func TestDSTUDPLifecycleStateSweepDeterminismAndExplore(t *testing.T) {
	// 1. Process exit, CrashProcess, CrashHost, and post-run stale handle fence.
	var crashed, leaked *UDPConn
	simulation.Test(t, 707, func(t *testing.T) {
		const port = 41234
		simulation.Host("node", func() {
			// Normal process exit closes owned UDP sockets.
			simulation.Process("ephemeral-proc", func() {
				u, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: port})
				if err != nil {
					t.Fatal(err)
				}
				_ = u // intentionally unclosed; process exit must reclaim port
			})
			// Port must be immediately re-bindable after process exit.
			started := make(chan struct{})
			go simulation.Process("victim-proc", func() {
				u, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: port})
				if err != nil {
					t.Errorf("re-bind after process exit failed: %v", err)
					return
				}
				close(started)
				buf := make([]byte, 8)
				_, _, _ = u.ReadFromUDP(buf)
			})
			<-started
			simulation.CrashProcess("node", "victim-proc")

			var err error
			crashed, err = ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: port})
			if err != nil {
				t.Fatalf("re-bind after CrashProcess failed: %v", err)
			}
		})
		simulation.CrashHost("node")
		simulation.Host("node", func() {
			var err error
			leaked, err = ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: port})
			if err != nil {
				t.Fatalf("re-bind after CrashHost reboot failed: %v", err)
			}
		})
	})
	if err := crashed.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-CrashHost Close() = %v, want net.ErrClosed", err)
	}
	crashed = nil
	if _, err := leaked.WriteToUDP([]byte("x"), &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("leaked UDPConn WriteToUDP outside run = %v, want net.ErrClosed", err)
	}
	// First post-run Close() on an unclosed leaked *UDPConn returns nil (wrapper-local
	// idempotent cleanup); subsequent Close() returns net.ErrClosed.
	if err := leaked.Close(); err != nil {
		t.Fatalf("first post-run Close() = %v, want nil", err)
	}
	if err := leaked.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second post-run Close() = %v, want net.ErrClosed", err)
	}
	leaked = nil

	// Non-bubble caller during an active simulation run is fenced cleanly with ErrClosed
	// without touching bubble channels, virtual clocks, or RNG streams.
	foreignReady := make(chan *UDPConn, 1)
	foreignChecked := make(chan error, 4)
	var foreignWG sync.WaitGroup
	foreignWG.Add(1)
	go func() {
		defer foreignWG.Done()
		u := <-foreignReady
		_, _, err1 := u.ReadFromUDP(make([]byte, 8))
		_, err2 := u.WriteToUDP([]byte("x"), &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 9})
		err3 := u.SetReadDeadline(time.Now().Add(time.Second))
		err4 := u.Close()
		for _, err := range []error{err1, err2, err3, err4} {
			if !errors.Is(err, ErrClosed) {
				foreignChecked <- err
				return
			}
		}
		foreignChecked <- nil
	}()
	simulation.Run(710, func() {
		u, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatal(err)
		}
		defer u.Close()
		// Exercise via host-side sync outside bubble scheduling:
		foreignReady <- u
		foreignWG.Wait()
	})
	if err := <-foreignChecked; err != nil {
		t.Fatalf("foreign non-bubble caller got %v, want net.ErrClosed", err)
	}

	// Connected UDP jittered multi-write sk_err overwrite + mid-flight refusal partition drop.
	simulation.Test(t, 711, func(t *testing.T) {
		var h2IP string
		simulation.Host("j1", func() {})
		simulation.Host("j2", func() { h2IP = simulation.HostIP("j2") })
		simulation.ConfigureLink("j1", "j2", simulation.LinkConfig{
			Latency:         10 * time.Millisecond,
			Jitter:          40 * time.Millisecond,
			PacketDuplicate: 1.0,
			PacketReorder:   true,
		})
		simulation.Host("j1", func() {
			c, err := DialUDP("udp4", nil, &UDPAddr{IP: ParseIP(h2IP), Port: 48888})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			// Multiple writes (each duplicated) with return-leg jitter must never busy-spin
			// and once matured must collapse into a single sk_err = ECONNREFUSED.
			for i := 0; i < 4; i++ {
				_, _ = c.Write([]byte("p"))
			}
			time.Sleep(150 * time.Millisecond) // all refusals matured into single sk_err
			buf := make([]byte, 8)
			if _, err := c.Read(buf); !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("first Read after matured refusals = %v, want ECONNREFUSED", err)
			}
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			if _, err := c.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("second Read after sk_err consumed = %v, want ErrDeadlineExceeded (single sk_err slot)", err)
			}
			_ = c.SetReadDeadline(time.Time{})

			// Mid-flight return-leg partition+heal must drop in-flight refusal rather than resurrecting it.
			simulation.ConfigureLink("j1", "j2", simulation.LinkConfig{Latency: 30 * time.Millisecond})
			_, _ = c.Write([]byte("probe"))     // arrives at j2 at t+30ms; refusal returns at t+60ms
			time.Sleep(40 * time.Millisecond)   // refusal is mid-flight j2 -> j1!
			simulation.Partition("j1", "j2")    // drops mid-flight refusal
			simulation.HealLink("j1", "j2")     // heals before t+60ms
			time.Sleep(40 * time.Millisecond)   // past t+60ms
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			if _, err := c.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("mid-flight partitioned refusal resurrected after heal: %v", err)
			}
		})
	})

	// 2. Weak-pointer out-of-line table sweep at run teardown.
	simulation.Run(708, func() {
		for i := 0; i < 32; i++ {
			c, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 0})
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
		}
	})
	runtime.GC()
	runtime.GC()
	simulation.Run(709, func() {}) // teardown sweeps collected netFDs
	_, freeCount := dstUDPStateStats()
	if freeCount == 0 {
		t.Fatal("dstUDPStates.sweep() reclaimed 0 rows after GC + run teardown")
	}

	// 3. Same-seed determinism under cross-host jitter + loss + dup + reorder + SO_REUSEPORT.
	runTranscript := func(seed uint64) string {
		var out strings.Builder
		simulation.RunWith(seed, simulation.Options{
			Network: simulation.NetworkConfig{
				CrossHost: simulation.LinkConfig{
					Latency:         3 * time.Millisecond,
					Jitter:          12 * time.Millisecond,
					PacketLoss:      0.25,
					PacketDuplicate: 0.25,
					PacketReorder:   true,
				},
			},
		}, func() {
			var srvIP string
			simulation.Host("s", func() { srvIP = simulation.HostIP("s") })
			portCh := make(chan int, 1)
			done := make(chan [2]string, 1)
			go simulation.Host("s", func() {
				lc := ListenConfig{Control: setReusePortControl(t)}
				w0, err := lc.ListenPacket(context.Background(), "udp4", srvIP+":0")
				if err != nil {
					t.Error(err)
					return
				}
				defer w0.Close()
				p := w0.LocalAddr().(*UDPAddr).Port
				w1, err := lc.ListenPacket(context.Background(), "udp4", srvIP+":"+strconv.Itoa(p))
				if err != nil {
					t.Error(err)
					return
				}
				defer w1.Close()
				portCh <- p
				var res [2]string
				var wg sync.WaitGroup
				for idx, pc := range []PacketConn{w0, w1} {
					wg.Add(1)
					go func(i int, conn PacketConn) {
						defer wg.Done()
						var sb strings.Builder
						buf := make([]byte, 16)
						for {
							_ = conn.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
							n, _, err := conn.ReadFrom(buf)
							if err != nil {
								break
							}
							sb.Write(buf[:n])
							sb.WriteByte(',')
						}
						res[i] = sb.String()
					}(idx, pc)
				}
				wg.Wait()
				done <- res
			})
			p := <-portCh
			simulation.Host("c", func() {
				for k := 0; k < 4; k++ {
					cli, err := DialUDP("udp4", nil, &UDPAddr{IP: ParseIP(srvIP), Port: p})
					if err != nil {
						t.Fatal(err)
					}
					for m := 0; m < 6; m++ {
						_, _ = cli.Write([]byte(strconv.Itoa(k*10 + m)))
					}
					cli.Close()
				}
			})
			r := <-done
			out.WriteString(r[0])
			out.WriteString("|")
			out.WriteString(r[1])
		})
		return out.String()
	}
	t1 := runTranscript(808)
	t2 := runTranscript(808)
	if t1 != t2 || t1 == "|" {
		t.Fatalf("same-seed UDP transcript mismatch:\n  run1=%q\n  run2=%q", t1, t2)
	}

	// 4. DPOR ExploreTest + TSan per-packet happens-before causality + deadline/close races.
	var sharedPayload int
	var deliveries atomic.Int64
	simulation.ExploreTest(t, 909, simulation.ExploreOptions{Mode: simulation.DPOR, MaxSchedules: 12}, func() error {
		sharedPayload = 0
		srv, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			return err
		}
		defer srv.Close()
		srvAddr := srv.LocalAddr().(*UDPAddr)

		errCh := make(chan error, 4)
		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 16)
			for i := 0; i < 2; i++ {
				n, from, err := srv.ReadFromUDP(buf)
				if err != nil {
					errCh <- err
					return
				}
				if string(buf[:n]) == "publish" && sharedPayload != 42 {
					errCh <- errors.New("per-packet happens-before broken: sharedPayload != 42")
					return
				}
				_, _ = srv.WriteToUDP(buf[:n], from)
			}
		}()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c, err := DialUDP("udp4", nil, srvAddr)
			if err != nil {
				errCh <- err
				return
			}
			defer c.Close()
			sharedPayload = 42
			_, _ = c.Write([]byte("publish"))
			b := make([]byte, 16)
			if n, err := c.Read(b); err == nil && string(b[:n]) == "publish" {
				deliveries.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			c, err := DialUDP("udp4", nil, srvAddr)
			if err != nil {
				errCh <- err
				return
			}
			defer c.Close()
			_, _ = c.Write([]byte("other"))
			b := make([]byte, 16)
			_, _ = c.Read(b)
		}()
		wg.Wait()
		<-done
		select {
		case err := <-errCh:
			return err
		default:
			return nil
		}
	})
	if deliveries.Load() == 0 {
		t.Fatal("ExploreTest executed 0 schedules")
	}

	// Verify Level-2 DPOR explores multiple schedules when SetReadDeadline / Close race ReadFromUDP.
	res := simulation.ExploreWith(910, simulation.ExploreOptions{Mode: simulation.DPOR, MaxSchedules: 8}, func() error {
		u, err := ListenUDP("udp4", &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			return err
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			var b [4]byte
			_, _, _ = u.ReadFromUDP(b[:])
		}()
		_ = u.SetReadDeadline(time.Now().Add(-time.Second))
		_ = u.Close()
		<-done
		return nil
	})
	if res.Schedules < 2 {
		t.Fatalf("DPOR explored %d schedule(s) on UDP ReadFrom/SetReadDeadline/Close race, want >= 2", res.Schedules)
	}
}
