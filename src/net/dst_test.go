// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"context"
	"errors"
	"internal/nettrace"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

func isDSTUnsupportedNetAPI(err error) bool {
	var opErr *OpError
	return errors.As(err, &opErr) && strings.Contains(opErr.Err.Error(), "network API unsupported under deterministic simulation")
}

func isDSTUnsupportedNetOption(err error, option string) bool {
	var opErr *OpError
	return errors.As(err, &opErr) && strings.Contains(opErr.Err.Error(), option+" unsupported under deterministic simulation")
}

func isDSTUnsupportedDNSLookup(err error) bool {
	var dnsErr *DNSError
	return errors.As(err, &dnsErr) && dnsErr.Err == "DNS lookup unsupported under deterministic simulation"
}

func isDSTUnsupportedServiceLookup(err error) bool {
	var dnsErr *DNSError
	return errors.As(err, &dnsErr) && strings.Contains(dnsErr.Err, "service lookup unsupported under deterministic simulation")
}

func TestDSTResolverAPIsDoNotTouchHost(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	ctx := context.Background()
	r := &Resolver{PreferGo: true, Dial: func(context.Context, string, string) (Conn, error) {
		t.Fatal("resolver Dial called under DST")
		return nil, nil
	}}
	originalDefault := DefaultResolver
	DefaultResolver = r
	defer func() { DefaultResolver = originalDefault }()

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "LookupHost",
			call: func() error {
				_, err := LookupHost("example.com")
				return err
			},
		},
		{
			name: "Resolver.LookupHost",
			call: func() error {
				_, err := r.LookupHost(ctx, "example.com")
				return err
			},
		},
		{
			name: "LookupIP",
			call: func() error {
				_, err := LookupIP("example.com")
				return err
			},
		},
		{
			name: "Resolver.LookupIPAddr",
			call: func() error {
				_, err := r.LookupIPAddr(ctx, "example.com")
				return err
			},
		},
		{
			name: "Resolver.LookupIP",
			call: func() error {
				_, err := r.LookupIP(ctx, "ip", "example.com")
				return err
			},
		},
		{
			name: "Resolver.LookupNetIP",
			call: func() error {
				_, err := r.LookupNetIP(ctx, "ip", "example.com")
				return err
			},
		},
		{
			name: "LookupCNAME",
			call: func() error {
				_, err := LookupCNAME("example.com")
				return err
			},
		},
		{
			name: "LookupSRV",
			call: func() error {
				_, _, err := LookupSRV("xmpp-server", "tcp", "example.com")
				return err
			},
		},
		{
			name: "LookupMX",
			call: func() error {
				_, err := LookupMX("example.com")
				return err
			},
		},
		{
			name: "LookupNS",
			call: func() error {
				_, err := LookupNS("example.com")
				return err
			},
		},
		{
			name: "LookupTXT",
			call: func() error {
				_, err := LookupTXT("example.com")
				return err
			},
		},
		{
			name: "LookupAddr",
			call: func() error {
				_, err := LookupAddr("192.0.2.1")
				return err
			},
		},
	}

	simulation.Run(1, func() {
		for _, tt := range cases {
			if err := tt.call(); !isDSTUnsupportedDNSLookup(err) {
				t.Fatalf("%s under DST error = %v, want deterministic unsupported-DNS error", tt.name, err)
			}
		}

		if _, err := LookupPort("tcp", "http"); !isDSTUnsupportedServiceLookup(err) {
			t.Fatalf("LookupPort under DST error = %v, want deterministic unsupported-service error", err)
		}
	})
}

func TestDSTResolverAPIsKeepNoIOFastPaths(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	ctx := context.Background()

	simulation.Run(1, func() {
		addrs, err := LookupHost("192.0.2.1")
		if err != nil {
			t.Fatal(err)
		}
		if len(addrs) != 1 || addrs[0] != "192.0.2.1" {
			t.Fatalf("LookupHost literal = %v, want [192.0.2.1]", addrs)
		}

		ipAddrs, err := DefaultResolver.LookupIPAddr(ctx, "2001:db8::1")
		if err != nil {
			t.Fatal(err)
		}
		if len(ipAddrs) != 1 || !ipAddrs[0].IP.Equal(ParseIP("2001:db8::1")) {
			t.Fatalf("LookupIPAddr literal = %v, want 2001:db8::1", ipAddrs)
		}

		port, err := LookupPort("tcp", "443")
		if err != nil {
			t.Fatal(err)
		}
		if port != 443 {
			t.Fatalf("LookupPort numeric = %d, want 443", port)
		}

		if _, err := LookupAddr("not-an-ip"); err == nil || isDSTUnsupportedDNSLookup(err) {
			t.Fatalf("LookupAddr invalid address error = %v, want validation error", err)
		}

		invalidName := "!!!.###.bogus..domain."
		invalidNameCases := []struct {
			name    string
			wantErr string
			call    func() error
		}{
			{
				name:    "LookupSRV",
				wantErr: "_xmpp-server._tcp." + invalidName,
				call: func() error {
					_, _, err := LookupSRV("xmpp-server", "tcp", invalidName)
					return err
				},
			},
			{
				name:    "LookupMX",
				wantErr: invalidName,
				call: func() error {
					_, err := LookupMX(invalidName)
					return err
				},
			},
			{
				name:    "LookupNS",
				wantErr: invalidName,
				call: func() error {
					_, err := LookupNS(invalidName)
					return err
				},
			},
			{
				name:    "LookupTXT",
				wantErr: invalidName,
				call: func() error {
					_, err := LookupTXT(invalidName)
					return err
				},
			},
		}
		for _, tt := range invalidNameCases {
			var dnsErr *DNSError
			if err := tt.call(); !errors.As(err, &dnsErr) || dnsErr.Err != errNoSuchHost.Error() || dnsErr.Name != tt.wantErr {
				t.Fatalf("%s invalid name error = %v, want no-such-host validation error for %q", tt.name, err, tt.wantErr)
			}
		}
	})
}

func TestDSTNetTypedAndPacketAPIsDoNotTouchHost(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	ctx := context.Background()
	dialer := Dialer{}
	lc := ListenConfig{}
	loopback := IPv4(127, 0, 0, 1)
	addr := netip.AddrFrom4([4]byte{127, 0, 0, 1})
	ap := netip.AddrPortFrom(addr, 1)
	unixStream := &UnixAddr{Name: t.TempDir() + "/stream.sock", Net: "unix"}
	unixDatagram := &UnixAddr{Name: t.TempDir() + "/datagram.sock", Net: "unixgram"}

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "DialTCP",
			call: func() error {
				c, err := DialTCP("tcp", nil, &TCPAddr{IP: loopback, Port: 1})
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "Dialer.DialTCP",
			call: func() error {
				c, err := dialer.DialTCP(ctx, "tcp", netip.AddrPort{}, ap)
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "DialUDP",
			call: func() error {
				c, err := DialUDP("udp", nil, &UDPAddr{IP: loopback, Port: 1})
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "Dialer.DialUDP",
			call: func() error {
				c, err := dialer.DialUDP(ctx, "udp", netip.AddrPort{}, ap)
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "DialIP",
			call: func() error {
				c, err := DialIP("ip4:icmp", nil, &IPAddr{IP: loopback})
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "Dialer.DialIP",
			call: func() error {
				c, err := dialer.DialIP(ctx, "ip4:icmp", netip.Addr{}, addr)
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "DialUnix",
			call: func() error {
				c, err := DialUnix("unix", nil, unixStream)
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "Dialer.DialUnix",
			call: func() error {
				c, err := dialer.DialUnix(ctx, "unix", nil, unixStream)
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "ListenPacket",
			call: func() error {
				c, err := ListenPacket("udp", "127.0.0.1:0")
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "ListenConfig.ListenPacket",
			call: func() error {
				c, err := lc.ListenPacket(ctx, "udp", "127.0.0.1:0")
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "ListenTCP",
			call: func() error {
				l, err := ListenTCP("tcp", &TCPAddr{IP: loopback})
				if l != nil {
					l.Close()
				}
				return err
			},
		},
		{
			name: "ListenUDP",
			call: func() error {
				c, err := ListenUDP("udp", &UDPAddr{IP: loopback})
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "ListenMulticastUDP",
			call: func() error {
				c, err := ListenMulticastUDP("udp4", nil, &UDPAddr{IP: IPv4(224, 0, 0, 1), Port: 9999})
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "ListenIP",
			call: func() error {
				c, err := ListenIP("ip4:icmp", &IPAddr{IP: loopback})
				if c != nil {
					c.Close()
				}
				return err
			},
		},
		{
			name: "ListenUnix",
			call: func() error {
				l, err := ListenUnix("unix", unixStream)
				if l != nil {
					l.Close()
				}
				return err
			},
		},
		{
			name: "ListenUnixgram",
			call: func() error {
				c, err := ListenUnixgram("unixgram", unixDatagram)
				if c != nil {
					c.Close()
				}
				return err
			},
		},
	}

	simulation.Run(1, func() {
		for _, tt := range cases {
			err := tt.call()
			if !isDSTUnsupportedNetAPI(err) {
				t.Fatalf("%s under DST error = %v, want deterministic unsupported-network-API error", tt.name, err)
			}
		}
	})
}

func TestDSTNetDialerLocalAddr(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		// Listen on a host-owned address (loopback): under the per-host network a
		// host cannot bind an IP it does not own. Use a distinct address from its
		// host-owned loopback range to exercise explicit source reporting.
		ln, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		acceptErr := make(chan error, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			defer c.Close()
			remote := c.RemoteAddr().(*TCPAddr)
			if !remote.IP.Equal(IPv4(127, 0, 0, 2)) {
				acceptErr <- &AddrError{Err: "server saw wrong remote IP", Addr: remote.String()}
				return
			}
			if remote.Port != 23456 {
				acceptErr <- &AddrError{Err: "server saw wrong remote port", Addr: remote.String()}
				return
			}
			acceptErr <- nil
		}()

		d := Dialer{LocalAddr: &TCPAddr{IP: IPv4(127, 0, 0, 2), Port: 23456}}
		c, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		local := c.LocalAddr().(*TCPAddr)
		if !local.IP.Equal(IPv4(127, 0, 0, 2)) {
			t.Fatalf("Dialer.LocalAddr IP ignored: got %v, want 127.0.0.2", local.IP)
		}
		if local.Port != 23456 {
			t.Fatalf("Dialer.LocalAddr port ignored: got %v, want 23456", local.Port)
		}
		if err := <-acceptErr; err != nil {
			t.Fatal(err)
		}
	})
}

// TestDSTNetInterfaces verifies net.Interfaces and friends return the calling
// host's fixed synthetic set (lo + eth0 bearing the host's routable IP) rather than
// the real machine's NICs — deterministic per host, no real-interface leak
// (DST-IDENTITY-SOUND), and consistent with the host-owned address model.
func TestDSTNetInterfaces(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	type view struct {
		names, addrs            string
		eth0IP, eth0MAC, byIdx2 string
		byNameOK, byBadErr      bool
	}
	capture := func() view {
		var v view
		ifs, err := Interfaces()
		if err != nil {
			t.Errorf("Interfaces: %v", err)
		}
		var ns []string
		for i := range ifs {
			ifi := &ifs[i]
			ns = append(ns, ifi.Name)
			if ifi.Name == "eth0" {
				v.eth0MAC = ifi.HardwareAddr.String()
				as, _ := ifi.Addrs()
				for _, a := range as {
					if ipn, ok := a.(*IPNet); ok && ipn.IP.To4() != nil {
						v.eth0IP = ipn.String()
					}
				}
			}
		}
		v.names = strings.Join(ns, ",")
		if ifi, err := InterfaceByName("eth0"); err == nil && ifi.Name == "eth0" {
			v.byNameOK = true
		}
		if _, err := InterfaceByName("wlan0"); err != nil {
			v.byBadErr = true
		}
		if ifi, err := InterfaceByIndex(2); err == nil {
			v.byIdx2 = ifi.Name
		}
		all, _ := InterfaceAddrs()
		var ss []string
		for _, a := range all {
			ss = append(ss, a.String())
		}
		v.addrs = strings.Join(ss, ",")
		return v
	}

	var hA, hB view
	simulation.Run(1, func() {
		simulation.Host("hA", simulation.HostConfig{}, func() { hA = capture() }) // id 1 -> 10.0.0.1
		simulation.Host("hB", simulation.HostConfig{}, func() { hB = capture() }) // id 2 -> 10.0.0.2
	})

	for _, tc := range []struct {
		got, want, what string
	}{
		{hA.names, "lo,eth0", "hA synthetic interface set (real NICs must not leak)"},
		{hB.names, "lo,eth0", "hB synthetic interface set"},
		{hA.eth0IP, "10.0.0.1/24", "hA eth0 address"},
		{hB.eth0IP, "10.0.0.2/24", "hB eth0 address"},
		{hA.byIdx2, "eth0", "hA InterfaceByIndex(2)"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.what, tc.got, tc.want)
		}
	}
	if !hA.byNameOK || !hA.byBadErr {
		t.Errorf("hA lookups: InterfaceByName(eth0)ok=%v InterfaceByName(wlan0)err=%v", hA.byNameOK, hA.byBadErr)
	}
	if !strings.Contains(hA.addrs, "10.0.0.1/24") || !strings.Contains(hA.addrs, "127.0.0.1/8") || !strings.Contains(hA.addrs, "::1/128") {
		t.Errorf("hA InterfaceAddrs = %q, want loopback + routable", hA.addrs)
	}
	if hA.eth0MAC == hB.eth0MAC {
		t.Errorf("hA and hB share eth0 MAC %q, want per-host", hA.eth0MAC)
	}
}

// TestDSTNetHostIsolation exercises the per-host network: cross-host reach via a
// host's routable IP (simulation.HostIP), host-private loopback, per-host port
// space, and the can't-bind-a-foreign-IP rule.
func TestDSTNetHostIsolation(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var (
		serverGot, clientGot        string
		serverRemoteIP, hbIP        string
		loopbackErr, foreignBindErr error
		bothBound9090               bool
	)
	simulation.Run(1, func() {
		var port string
		ready := make(chan struct{})

		simulation.Host("hA", simulation.HostConfig{}, func() { // host id 1 -> routable 10.0.0.1
			ln, err := Listen("tcp", ":0") // wildcard: hA's loopback + 10.0.0.1
			if err != nil {
				panic(err)
			}
			_, port, _ = SplitHostPort(ln.Addr().String())
			// hA cannot bind a routable IP it does not own (10.0.0.2 is host 2's).
			if _, e := Listen("tcp", "10.0.0.2:0"); e != nil {
				foreignBindErr = e
			}
			if ln2, e := Listen("tcp", ":9090"); e == nil {
				defer ln2.Close()
			}
			go func() {
				close(ready)
				c, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 16)
				n, _ := c.Read(buf)
				serverGot = string(buf[:n])
				serverRemoteIP = c.RemoteAddr().(*TCPAddr).IP.String()
				c.Write([]byte("pong"))
				c.Close()
			}()
		})

		simulation.Host("hB", simulation.HostConfig{}, func() { // host id 2
			<-ready
			hbIP = simulation.HostIP("hB")
			// hB cannot reach hA's loopback (host-private).
			if _, e := Dial("tcp", "127.0.0.1:"+port); e != nil {
				loopbackErr = e
			}
			// hB reaches hA via hA's routable IP.
			c, err := Dial("tcp", simulation.HostIP("hA")+":"+port)
			if err != nil {
				panic(err)
			}
			c.Write([]byte("ping"))
			buf := make([]byte, 16)
			n, _ := c.Read(buf)
			clientGot = string(buf[:n])
			c.Close()
			// hB can bind :9090 even though hA holds it (per-host port space).
			if ln, e := Listen("tcp", ":9090"); e == nil {
				bothBound9090 = true
				ln.Close()
			}
		})
	})

	if serverGot != "ping" || clientGot != "pong" {
		t.Errorf("cross-host exchange: server got %q, client got %q; want ping/pong", serverGot, clientGot)
	}
	if !errors.Is(loopbackErr, syscall.ECONNREFUSED) {
		t.Errorf("hB dial to hA's loopback = %v, want ECONNREFUSED (host-private loopback breached)", loopbackErr)
	}
	if serverRemoteIP != hbIP {
		t.Errorf("server saw cross-host source IP %q, want hB's routable IP %q", serverRemoteIP, hbIP)
	}
	if !bothBound9090 {
		t.Errorf("hB could not bind :9090 while hA holds it (port space not per-host)")
	}
	if !errors.Is(foreignBindErr, syscall.EADDRNOTAVAIL) {
		t.Errorf("hA bind 10.0.0.2 (host 2's IP) = %v, want EADDRNOTAVAIL", foreignBindErr)
	}
}

func TestDSTNetDialerLocalAddrValidation(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	cases := []struct {
		name    string
		network string
		address string
		local   Addr
		want    string
	}{
		{
			name:    "wrong type",
			network: "tcp",
			address: "127.0.0.1:1",
			local:   &UDPAddr{IP: IPv4(127, 0, 0, 1), Port: 23456},
			want:    "mismatched local address type",
		},
		{
			name:    "invalid port",
			network: "tcp",
			address: "127.0.0.1:1",
			local:   &TCPAddr{IP: IPv4(127, 0, 0, 1), Port: 1 << 16},
			want:    "invalid port",
		},
		{
			name:    "tcp4 rejects ipv6 local",
			network: "tcp4",
			address: "127.0.0.1:1",
			local:   &TCPAddr{IP: IPv6loopback, Port: 23456},
			want:    errNoSuitableAddress.Error(),
		},
		{
			name:    "tcp6 rejects ipv4 local",
			network: "tcp6",
			address: "[::1]:1",
			local:   &TCPAddr{IP: IPv4(127, 0, 0, 1), Port: 23456},
			want:    errNoSuitableAddress.Error(),
		},
		{
			name:    "tcp rejects family mismatch",
			network: "tcp",
			address: "[::1]:1",
			local:   &TCPAddr{IP: IPv4(127, 0, 0, 1), Port: 23456},
			want:    errNoSuitableAddress.Error(),
		},
	}

	simulation.Run(1, func() {
		for _, tt := range cases {
			d := Dialer{LocalAddr: tt.local}
			c, err := d.DialContext(context.Background(), tt.network, tt.address)
			if c != nil {
				c.Close()
			}
			var opErr *OpError
			if !errors.As(err, &opErr) || !strings.Contains(opErr.Err.Error(), tt.want) {
				t.Fatalf("%s LocalAddr error = %v, want %q", tt.name, err, tt.want)
			}
		}
	})
}

func TestDSTNetExplicitMPTCPDisableUsesBaseTCP(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		var lc ListenConfig
		lc.SetMultipathTCP(false)
		ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		acceptErr := make(chan error, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			c.Close()
			acceptErr <- nil
		}()

		var d Dialer
		d.SetMultipathTCP(false)
		c, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		if err := <-acceptErr; err != nil {
			t.Fatal(err)
		}
	})
}

func TestDSTNetErrorIdentity(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		c, err := Dial("tcp", "127.0.0.1:1")
		if c != nil {
			c.Close()
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("refused Dial error = %v, want errors.Is ECONNREFUSED", err)
		}

		ln, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		dup, err := Listen("tcp", ln.Addr().String())
		if dup != nil {
			dup.Close()
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("duplicate Listen error = %v, want errors.Is EADDRINUSE", err)
		}
	})
}

func TestDSTNetRejectsUnsupportedOptions(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	ctx := context.Background()
	dialAddr := "127.0.0.1:1"
	listenAddr := "127.0.0.1:0"

	// Control/ControlContext and the keepalive configuration are MODELED
	// (the socket-option layer; see dst_keepalive_test.go for the modeled
	// behavior). Only an explicit MPTCP enable remains refused.
	var mptcpDialer Dialer
	mptcpDialer.SetMultipathTCP(true)
	var mptcpConfig ListenConfig
	mptcpConfig.SetMultipathTCP(true)

	simulation.Run(1, func() {
		c, err := mptcpDialer.DialContext(ctx, "tcp", dialAddr)
		if c != nil {
			c.Close()
		}
		if !isDSTUnsupportedNetOption(err, "Dialer.MultipathTCP") {
			t.Fatalf("Dialer MPTCP option error = %v, want unsupported Dialer.MultipathTCP", err)
		}
		l, err := mptcpConfig.Listen(ctx, "tcp", listenAddr)
		if l != nil {
			l.Close()
		}
		if !isDSTUnsupportedNetOption(err, "ListenConfig.MultipathTCP") {
			t.Fatalf("ListenConfig MPTCP option error = %v, want unsupported ListenConfig.MultipathTCP", err)
		}
	})
}

// TestDSTNetFileAPIsRejected verifies FileConn/FileListener/FilePacketConn are
// gated under simulation: an inherited fd is a host socket, and using it would
// escape the in-memory network entirely (the one conn/listener-producing
// surface the typed-API gates did not cover).
func TestDSTNetFileAPIsRejected(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		f := os.NewFile(0, "stdin")
		if _, err := FileConn(f); !isDSTUnsupportedNetAPI(err) {
			t.Errorf("FileConn under DST = %v, want unsupported-API error", err)
		}
		if _, err := FileListener(f); !isDSTUnsupportedNetAPI(err) {
			t.Errorf("FileListener under DST = %v, want unsupported-API error", err)
		}
		if _, err := FilePacketConn(f); !isDSTUnsupportedNetAPI(err) {
			t.Errorf("FilePacketConn under DST = %v, want unsupported-API error", err)
		}
	})
}

// TestDSTNetListenerCloseSemantics verifies production listener-close shape:
// connections still in the accept backlog are reset (the dialer's next op
// fails with ECONNRESET instead of blocking durably forever), Accept after
// Close fails with net.ErrClosed even with queued connections, and a second
// Close fails with net.ErrClosed.
func TestDSTNetListenerCloseSemantics(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		ln, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		c, err := Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if err := ln.Close(); err != nil {
			t.Fatalf("first Close = %v", err)
		}
		if _, err := ln.Accept(); !errors.Is(err, ErrClosed) {
			t.Errorf("Accept after Close = %v, want net.ErrClosed", err)
		}
		if err := ln.Close(); !errors.Is(err, ErrClosed) {
			t.Errorf("second Close = %v, want net.ErrClosed", err)
		}
		buf := make([]byte, 1)
		if _, err := c.Read(buf); !errors.Is(err, syscall.ECONNRESET) {
			t.Errorf("Read on backlog-reset conn = %v, want ECONNRESET", err)
		}
		// The read consumed the one-shot sk_err: the write carries the
		// CLOSED-socket EPIPE, as on the host.
		if _, err := c.Write([]byte("x")); !errors.Is(err, syscall.EPIPE) {
			t.Errorf("Write on backlog-reset conn after the consumed reset = %v, want EPIPE", err)
		}
	})
}

// TestDSTNetConnErrorIdentity verifies established-connection error identity
// matches production shape: ops after a local Close satisfy
// errors.Is(err, net.ErrClosed); reads from a gracefully closed peer return
// io.EOF; the first write to a closed peer carries ECONNRESET and later
// writes the kernel's post-reset EPIPE (one-shot); a second Close fails with
// net.ErrClosed; deadline errors are *OpError carrying os.ErrDeadlineExceeded
// (a net.Error with Timeout() true) on the connection's network, driven by
// the bubble's virtual clock.
func TestDSTNetConnErrorIdentity(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		ln, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		client, err := Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		server, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}

		// Deadline on the virtual clock: a blocked read fails once virtual
		// time passes the deadline, with production error shape.
		if err := client.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1)
		_, err = client.Read(buf)
		var ne Error
		if !errors.As(err, &ne) || !ne.Timeout() || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("deadline Read = %v, want timeout net.Error wrapping os.ErrDeadlineExceeded", err)
		}
		var op *OpError
		if !errors.As(err, &op) || op.Net != "tcp" {
			t.Errorf("deadline Read OpError.Net = %v, want tcp OpError", err)
		}
		if err := client.SetReadDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}

		// Graceful peer close: in-flight data then EOF on read, ECONNRESET on
		// write. The pipe is a rendezvous, so the peer writes from its own
		// goroutine and closes after the write completes.
		peerDone := make(chan struct{})
		go func() {
			defer close(peerDone)
			if _, err := server.Write([]byte("ok")); err != nil {
				t.Errorf("server Write = %v", err)
			}
			if err := server.Close(); err != nil {
				t.Errorf("server Close = %v", err)
			}
		}()
		got := make([]byte, 2)
		if _, err := io.ReadFull(client, got); err != nil || string(got) != "ok" {
			t.Errorf("read in-flight data before peer close = %q, %v", got, err)
		}
		<-peerDone
		if _, err := client.Read(buf); err != io.EOF {
			t.Errorf("Read after peer close = %v, want io.EOF", err)
		}
		if _, err := client.Write([]byte("x")); !errors.Is(err, syscall.ECONNRESET) {
			t.Errorf("Write after peer close = %v, want ECONNRESET", err)
		}
		// The first write consumed the minted one-shot: follow-up writes
		// carry the kernel's post-reset EPIPE (host ladder from write #2 on),
		// and reads stay io.EOF.
		if _, err := client.Write([]byte("y")); !errors.Is(err, syscall.EPIPE) {
			t.Errorf("second Write after peer close = %v, want EPIPE", err)
		}
		if _, err := client.Read(buf); err != io.EOF {
			t.Errorf("Read after the failed writes = %v, want io.EOF (persistent)", err)
		}
		// A peer close does not invalidate the local endpoint: deadlines still
		// apply cleanly, as in production.
		if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Errorf("SetDeadline after peer close = %v, want nil", err)
		}

		// Local close: every op is net.ErrClosed, including a second Close.
		if err := client.Close(); err != nil {
			t.Fatalf("first Close = %v", err)
		}
		if _, err := client.Read(buf); !errors.Is(err, ErrClosed) {
			t.Errorf("Read after local close = %v, want net.ErrClosed", err)
		}
		if _, err := client.Write([]byte("x")); !errors.Is(err, ErrClosed) {
			t.Errorf("Write after local close = %v, want net.ErrClosed", err)
		}
		if err := client.Close(); !errors.Is(err, ErrClosed) {
			t.Errorf("second Close = %v, want net.ErrClosed", err)
		}
		if err := client.SetDeadline(time.Now()); !errors.Is(err, ErrClosed) {
			t.Errorf("SetDeadline after local close = %v, want net.ErrClosed", err)
		}
		if err := client.SetReadDeadline(time.Now()); !errors.Is(err, ErrClosed) {
			t.Errorf("SetReadDeadline after local close = %v, want net.ErrClosed", err)
		}
		if err := client.SetWriteDeadline(time.Now()); !errors.Is(err, ErrClosed) {
			t.Errorf("SetWriteDeadline after local close = %v, want net.ErrClosed", err)
		}
		if _, err := server.Read(buf); !errors.Is(err, ErrClosed) {
			t.Errorf("server Read after its close = %v, want net.ErrClosed", err)
		}
	})
}

// TestDSTNetDualStackWildcard verifies the plain-"tcp" wildcard listener is
// dual-stack as in production: it reports the IPv6 wildcard address and
// accepts dials of both families, and it conflicts with single-family
// listeners on the same port.
func TestDSTNetDualStackWildcard(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		ln, err := Listen("tcp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		ta := ln.Addr().(*TCPAddr)
		if !ta.IP.Equal(IPv6zero) {
			t.Errorf("dual-stack wildcard Addr = %v, want [::]:port", ta)
		}
		port := strconv.Itoa(ta.Port)
		c4, err := Dial("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Fatalf("IPv4 dial to dual-stack wildcard: %v", err)
		}
		defer c4.Close()
		if _, err := ln.Accept(); err != nil {
			t.Fatal(err)
		}
		c6, err := Dial("tcp", "[::1]:"+port)
		if err != nil {
			t.Fatalf("IPv6 dial to dual-stack wildcard: %v", err)
		}
		defer c6.Close()
		if _, err := ln.Accept(); err != nil {
			t.Fatal(err)
		}
		if _, err := Listen("tcp4", "127.0.0.1:"+port); !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("tcp4 listen on dual-stack port = %v, want EADDRINUSE", err)
		}
		if _, err := Listen("tcp6", "[::1]:"+port); !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("tcp6 listen on dual-stack port = %v, want EADDRINUSE", err)
		}
	})
}

// TestDSTNetDialConnectTrace verifies the nettrace connect callbacks fire
// around a simulated dial, so httptrace-instrumented clients observe
// ConnectStart/ConnectDone as in production.
func TestDSTNetDialConnectTrace(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		ln, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		var starts, dones []string
		trace := &nettrace.Trace{
			ConnectStart: func(network, addr string) { starts = append(starts, network+"/"+addr) },
			ConnectDone:  func(network, addr string, err error) { dones = append(dones, network+"/"+addr) },
		}
		ctx := context.WithValue(context.Background(), nettrace.TraceKey{}, trace)
		var d Dialer
		port := strconv.Itoa(ln.Addr().(*TCPAddr).Port)
		// Dial by name: the callbacks must report the RESOLVED address, as
		// production fires them per connect attempt after resolution.
		c, err := d.DialContext(ctx, "tcp", "localhost:"+port)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		want := "tcp/127.0.0.1:" + port
		if len(starts) != 1 || starts[0] != want || len(dones) != 1 || dones[0] != want {
			t.Errorf("connect trace = starts %v dones %v, want one %q each", starts, dones, want)
		}
		// A dial that fails address validation fires no connect callbacks.
		starts, dones = nil, nil
		if _, err := d.DialContext(ctx, "tcp", "%%bad%%"); err == nil {
			t.Error("dial of invalid address succeeded")
		}
		if len(starts) != 0 || len(dones) != 0 {
			t.Errorf("invalid-address dial fired connect trace: starts %v dones %v", starts, dones)
		}
	})
}

// TestDSTNetUnsupportedNetworkText verifies a known-but-unmodeled network is
// rejected with the simulation-boundary error shape (not "unknown network"),
// while a genuinely unknown network keeps UnknownNetworkError identity.
func TestDSTNetUnsupportedNetworkText(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		_, err := Dial("udp", "127.0.0.1:53")
		var op *OpError
		if !errors.As(err, &op) || !strings.Contains(op.Err.Error(), "unsupported under deterministic simulation") {
			t.Errorf("Dial udp = %v, want unsupported-under-simulation error", err)
		}
		var unk UnknownNetworkError
		if errors.As(err, &unk) {
			t.Errorf("Dial udp classified as unknown network: %v", err)
		}
		_, err = Dial("bogusnet", "127.0.0.1:1")
		if !errors.As(err, &unk) {
			t.Errorf("Dial bogusnet = %v, want UnknownNetworkError", err)
		}
	})
}

// TestDSTNetListenerCloseFullBacklog verifies teardown with a full backlog and
// a dial blocked mid-handshake: every queued connection is reset, the blocked
// dial is refused (or its connection reset), and Accept after Close fails with
// net.ErrClosed rather than returning a torn-down connection that landed in
// the backlog after the drain.
func TestDSTNetListenerCloseFullBacklog(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		ln, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		var conns []Conn
		for i := 0; i < 128; i++ { // fill the backlog
			c, err := Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial %d: %v", i, err)
			}
			conns = append(conns, c)
		}
		blocked := make(chan error, 1)
		go func() {
			c, err := Dial("tcp", addr) // blocks: backlog full
			if c != nil {
				buf := make([]byte, 1)
				_, err = c.Read(buf)
			}
			blocked <- err
		}()
		time.Sleep(time.Millisecond) // let the dial block
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := ln.Accept(); !errors.Is(err, ErrClosed) {
			t.Errorf("Accept after Close = %v, want net.ErrClosed", err)
		}
		// The blocked dial either was refused outright or its connection was
		// reset; either way the dialer observes a connection-level error.
		err = <-blocked
		if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ECONNRESET) {
			t.Errorf("blocked dial after Close = %v, want ECONNREFUSED or ECONNRESET", err)
		}
		buf := make([]byte, 1)
		for i, c := range conns {
			if _, err := c.Read(buf); !errors.Is(err, syscall.ECONNRESET) {
				t.Errorf("conn %d Read after listener Close = %v, want ECONNRESET", i, err)
			}
		}
	})
}

// TestDSTNetParkedAcceptLosesToClose pins the Accept/Close overlap
// linearization: an Accept already blocked in the listener's select
// when Close runs returns net.ErrClosed — never a connection — and the
// connection it would have won is reset like the rest of the backlog, exactly
// as production unblocks every pending accept on close. The sequencing is
// deterministic under the simulation: the acceptor parks at a quiescence
// point, the dial hands it the connection, and Close runs before the acceptor
// is resumed.
func TestDSTNetParkedAcceptLosesToClose(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		ln, err := Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		type acceptResult struct {
			c   Conn
			err error
		}
		res := make(chan acceptResult, 1)
		go func() {
			c, err := ln.Accept()
			res <- acceptResult{c, err}
		}()
		time.Sleep(time.Millisecond) // quiescence: the acceptor is parked in its select
		c, err := Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if err := ln.Close(); err != nil { // before the acceptor is resumed
			t.Fatalf("Close = %v", err)
		}
		r := <-res
		if r.err == nil || !errors.Is(r.err, ErrClosed) {
			t.Errorf("Accept overlapping Close = (%v, %v), want net.ErrClosed", r.c, r.err)
		}
		buf := make([]byte, 1)
		if _, err := c.Read(buf); !errors.Is(err, syscall.ECONNRESET) {
			t.Errorf("Read on the reset connection = %v, want ECONNRESET", err)
		}
	})
}

// TestDSTNetFamilyWildcardAddr pins the reported address of single-family
// wildcard listens: production reports the family wildcard form
// (0.0.0.0:p / [::]:p), not the loopback the simulation maps it to
// internally — and dialing the reported form still reaches the listener.
func TestDSTNetFamilyWildcardAddr(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		ln4, err := Listen("tcp4", ":0")
		if err != nil {
			t.Fatal(err)
		}
		ta4 := ln4.Addr().(*TCPAddr)
		if !ta4.IP.Equal(IPv4zero) {
			t.Errorf("tcp4 wildcard Addr = %v, want 0.0.0.0:port", ta4)
		}
		ln6, err := Listen("tcp6", ":0")
		if err != nil {
			t.Fatal(err)
		}
		if ta6 := ln6.Addr().(*TCPAddr); !ta6.IP.Equal(IPv6zero) {
			t.Errorf("tcp6 wildcard Addr = %v, want [::]:port", ta6)
		}
		ln40, err := Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		if ta40 := ln40.Addr().(*TCPAddr); !ta40.IP.Equal(IPv4zero) {
			t.Errorf(`Listen("tcp", "0.0.0.0:0") Addr = %v, want 0.0.0.0:port`, ta40)
		}
		// The reported wildcard form is dialable and reaches the listener.
		done := make(chan struct{})
		go func() {
			defer close(done)
			c, err := ln4.Accept()
			if err != nil {
				t.Errorf("Accept on tcp4 wildcard: %v", err)
				return
			}
			c.Close()
		}()
		c, err := Dial("tcp4", ln4.Addr().String())
		if err != nil {
			t.Fatalf("Dial(%q) = %v", ln4.Addr().String(), err)
		}
		<-done
		c.Close()
	})
}
