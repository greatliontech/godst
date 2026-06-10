// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package net

import "context"

const dstNetEnabled = false

func dstActive() bool { return false }

func dstDial(ctx context.Context, network, address string) (Conn, error) { return nil, nil }

func dstListen(network, address string) (Listener, error) { return nil, nil }
