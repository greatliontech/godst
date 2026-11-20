// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && !linux

package net

import "context"

func dstNetRunTeardown()                                {}
func dstUDPCloseProc(uint32)                            {}
func dstUDPCrashHost(uint32)                            {}
func dstUDPOnPartitionOp(uint32, uint32, uint32)        {}
func dstUDPResetLossDiag(uint32, uint32, uint32)        {}
func dstUDPHasProcResources(uint32) bool                { return false }
func dstListenStreamOnUDP(network, address string) error { return dstUnsupportedNetwork("listen", network) }

func dstListenPacket(_ context.Context, _ *ListenConfig, network, _ string) (PacketConn, error) {
	return nil, dstUnsupportedNetAPI("listen", network, nil, nil)
}

func dstListenUDP(network string, laddr *UDPAddr) (*UDPConn, error) {
	return nil, dstUnsupportedNetAPI("listen", network, nil, laddr.opAddr())
}

func dstDialUDP(_ context.Context, _ *Dialer, network string, laddr, raddr *UDPAddr) (*UDPConn, error) {
	return nil, dstUnsupportedNetAPI("dial", network, laddr.opAddr(), raddr.opAddr())
}

func dstDialUDPString(_ context.Context, _ *Dialer, network, _ string) (Conn, error) {
	return nil, dstUnsupportedNetAPI("dial", network, nil, nil)
}

func dstFDRawControl(*netFD, func(uintptr)) (bool, error)     { return false, nil }
func dstFDRawRead(*netFD, func(uintptr) bool) (bool, error)   { return false, nil }
func dstFDRawWrite(*netFD, func(uintptr) bool) (bool, error)  { return false, nil }
