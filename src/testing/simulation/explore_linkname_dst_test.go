// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import "unsafe"

//go:linkname dstAccessYield runtime.dstAccessYield
func dstAccessYield(addr unsafe.Pointer, write bool)

//go:linkname dstFilterForceConservativeFP runtime.dstFilterForceConservativeFP
func dstFilterForceConservativeFP(on bool)

//go:linkname dstAccessYieldRange runtime.dstAccessYieldRange
func dstAccessYieldRange(addr unsafe.Pointer, size uintptr, write bool)

//go:linkname dstAccessYieldFP runtime.dstAccessYieldFP
func dstAccessYieldFP() uint64

//go:linkname dstAccessYieldReset runtime.dstAccessYieldReset
func dstAccessYieldReset()

//go:linkname dstSetPostGoYield runtime.dstSetPostGoYield
func dstSetPostGoYield(enabled bool) bool

//go:linkname dstRunningPanicDefersFP runtime.dstRunningPanicDefersFP
func dstRunningPanicDefersFP() uint32

//go:linkname dstCurrentSeqFP runtime.dstCurrentSeqFP
func dstCurrentSeqFP() uint64

//go:linkname dstSyncEventOverflowProbe runtime.dstSyncEventOverflowFP
func dstSyncEventOverflowProbe() bool

//go:linkname dstAccPageCharge runtime.dstAccPageCharge
func dstAccPageCharge(size uintptr) uintptr
