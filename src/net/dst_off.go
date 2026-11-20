// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package net

import "context"

const dstNetEnabled = false

func dstActive() bool { return false }

func dstInSimBubble() bool { return false }

func dstIsLocalhost(host string) bool { return false }

func dstLookupLocalhostIPAddr(network, host string) ([]IPAddr, bool) { return nil, false }

func dstLookupProtocol(name string) (int, error) { return 0, nil }

func dstDial(ctx context.Context, d *Dialer, network, address string) (Conn, error) { return nil, nil }

func dstListen(lc *ListenConfig, network, address string) (Listener, error) { return nil, nil }

func dstListenPacket(ctx context.Context, lc *ListenConfig, network, address string) (PacketConn, error) {
	return nil, nil
}

func dstListenUDP(network string, laddr *UDPAddr) (*UDPConn, error) { return nil, nil }

func dstDialUDP(ctx context.Context, dialer *Dialer, network string, laddr, raddr *UDPAddr) (*UDPConn, error) {
	return nil, nil
}

func dstFDRawControl(fd *netFD, f func(uintptr)) (bool, error) { return false, nil }

func dstFDRawRead(fd *netFD, f func(uintptr) bool) (bool, error) { return false, nil }

func dstFDRawWrite(fd *netFD, f func(uintptr) bool) (bool, error) { return false, nil }

func dstUnsupportedNetAPI(op, network string, source, addr Addr) error { return nil }

func dstUnsupportedDNSLookup(name string) error { return nil }

func dstUnsupportedServiceLookup(network, service string) error { return nil }

func dstInterfaces() []Interface { return nil }

func dstZoneName(index int) string { return "" }

func dstZoneIndex(name string) int { return 0 }

func dstInterfaceAddrs(ifi *Interface) []Addr { return nil }
