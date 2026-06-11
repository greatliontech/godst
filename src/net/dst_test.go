// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"testing/simulation"
)

func isDSTUnsupportedNetAPI(err error) bool {
	var opErr *OpError
	return errors.As(err, &opErr) && strings.Contains(opErr.Err.Error(), "network API unsupported under deterministic simulation")
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
