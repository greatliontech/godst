// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst || !linux

package main

// The runtime page-cache registry exists only under dst && linux; the probe
// that uses this (DSTMemfdFDIsolation) runs only under the tag, so the stub
// just keeps the untagged binary linking.
func dstPageCacheFDReservedFP(fd uintptr) bool { return false }
