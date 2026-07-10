// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (386 || arm || mips || mipsle)

package runtime

// A 32-bit address space cannot hold the region; the page cache refuses these
// hosts at first use (dstPageCacheCheckHost). Zero geometry so the file
// compiles.
// The mapping region's geometry and flags. Per-arch: the canonical base must
// fit the architecture's smallest common user address-space (ppc64's hash MMU
// is 46-bit, mips64 is commonly 40, riscv64's sv39 is 39), and MAP_NORESERVE
// is one of the mman flags whose value predates the asm-generic layout on
// mips (0x400) and power (0x40). Only amd64 is empirically verified; on the
// others a wrong guess fails loudly at reservation (MAP_FIXED_NOREPLACE
// refuses, it never relocates), which is the contract: refuse, don't diverge.
const (
	dstMapRegionBase = 0
	dstMapRegionSize = 0
	_dstMapNoReserve = 0
)
