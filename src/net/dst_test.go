// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"context"
	"errors"
	"net/netip"
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
		ln, err := Listen("tcp", "10.0.0.1:0")
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
			if !remote.IP.Equal(IPv4(10, 0, 0, 2)) {
				acceptErr <- &AddrError{Err: "server saw wrong remote IP", Addr: remote.String()}
				return
			}
			if remote.Port != 23456 {
				acceptErr <- &AddrError{Err: "server saw wrong remote port", Addr: remote.String()}
				return
			}
			acceptErr <- nil
		}()

		d := Dialer{LocalAddr: &TCPAddr{IP: IPv4(10, 0, 0, 2), Port: 23456}}
		c, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		local := c.LocalAddr().(*TCPAddr)
		if !local.IP.Equal(IPv4(10, 0, 0, 2)) {
			t.Fatalf("Dialer.LocalAddr IP ignored: got %v, want 10.0.0.2", local.IP)
		}
		if local.Port != 23456 {
			t.Fatalf("Dialer.LocalAddr port ignored: got %v, want 23456", local.Port)
		}
		if err := <-acceptErr; err != nil {
			t.Fatal(err)
		}
	})
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

func TestDSTNetRejectsUnsupportedOptions(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	ctx := context.Background()
	dialAddr := "127.0.0.1:1"
	listenAddr := "127.0.0.1:0"

	dialCases := []struct {
		name   string
		dialer Dialer
		option string
	}{
		{
			name: "ControlContext",
			dialer: Dialer{ControlContext: func(context.Context, string, string, syscall.RawConn) error {
				t.Fatal("Dialer.ControlContext called under DST")
				return nil
			}},
			option: "Dialer.ControlContext",
		},
		{
			name: "Control",
			dialer: Dialer{Control: func(string, string, syscall.RawConn) error {
				t.Fatal("Dialer.Control called under DST")
				return nil
			}},
			option: "Dialer.Control",
		},
		{
			name:   "MPTCP",
			option: "Dialer.MultipathTCP",
		},
		{
			name:   "KeepAlive",
			dialer: Dialer{KeepAlive: time.Second},
			option: "Dialer.KeepAlive",
		},
		{
			name:   "KeepAliveConfig",
			dialer: Dialer{KeepAliveConfig: KeepAliveConfig{Enable: true}},
			option: "Dialer.KeepAlive",
		},
	}
	dialCases[2].dialer.SetMultipathTCP(true)

	listenCases := []struct {
		name   string
		config ListenConfig
		option string
	}{
		{
			name: "Control",
			config: ListenConfig{Control: func(string, string, syscall.RawConn) error {
				t.Fatal("ListenConfig.Control called under DST")
				return nil
			}},
			option: "ListenConfig.Control",
		},
		{
			name:   "MPTCP",
			option: "ListenConfig.MultipathTCP",
		},
		{
			name:   "KeepAlive",
			config: ListenConfig{KeepAlive: time.Second},
			option: "ListenConfig.KeepAlive",
		},
		{
			name:   "KeepAliveConfig",
			config: ListenConfig{KeepAliveConfig: KeepAliveConfig{Enable: true}},
			option: "ListenConfig.KeepAlive",
		},
	}
	listenCases[1].config.SetMultipathTCP(true)

	simulation.Run(1, func() {
		for _, tt := range dialCases {
			c, err := tt.dialer.DialContext(ctx, "tcp", dialAddr)
			if c != nil {
				c.Close()
			}
			if !isDSTUnsupportedNetOption(err, tt.option) {
				t.Fatalf("Dialer %s option error = %v, want unsupported %s", tt.name, err, tt.option)
			}
		}
		for _, tt := range listenCases {
			l, err := tt.config.Listen(ctx, "tcp", listenAddr)
			if l != nil {
				l.Close()
			}
			if !isDSTUnsupportedNetOption(err, tt.option) {
				t.Fatalf("ListenConfig %s option error = %v, want unsupported %s", tt.name, err, tt.option)
			}
		}
	})
}
