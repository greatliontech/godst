// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package simulation

import "unsafe"

func dstAccessYield(addr unsafe.Pointer, write bool)                   {}
func dstFilterForceConservativeFP(on bool)                             {}
func dstAccessYieldRange(addr unsafe.Pointer, size uintptr, write bool) {}
func dstAccessYieldFP() uint64                                         { return 0 }
func dstAccessYieldReset()                                             {}
func dstSetPostGoYield(enabled bool) bool                              { return false }
func dstRunningPanicDefersFP() uint32                                  { return 0 }
func dstCurrentSeqFP() uint64                                          { return 0 }
func dstSyncEventOverflowProbe() bool                                  { return false }
func dstAccPageCharge(size uintptr) uintptr                            { return 0 }
