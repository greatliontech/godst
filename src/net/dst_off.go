// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package net

import "context"

const dstNetEnabled = false

func dstActive() bool { return false }

func dstDial(ctx context.Context, d *Dialer, network, address string) (Conn, error) { return nil, nil }

func dstListen(lc *ListenConfig, network, address string) (Listener, error) { return nil, nil }

func dstUnsupportedNetAPI(op, network string, source, addr Addr) error { return nil }

func dstUnsupportedDNSLookup(name string) error { return nil }

func dstUnsupportedServiceLookup(network, service string) error { return nil }

func dstInterfaces() []Interface { return nil }

func dstInterfaceAddrs(ifi *Interface) []Addr { return nil }
