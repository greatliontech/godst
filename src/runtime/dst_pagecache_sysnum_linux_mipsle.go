// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && mipsle

package runtime

// Syscall numbers for the simulated page cache. They live in a fork-owned file
// rather than internal/runtime/syscall/linux/defs_linux_*.go so that porting
// the fork to a new Go release never conflicts on an upstream file.
const (
	dstSysMemfdCreate = 4354
	dstSysFtruncate   = 4212 // ftruncate64: off_t is 32 bits here
)
