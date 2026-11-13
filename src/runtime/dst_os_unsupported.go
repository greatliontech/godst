// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && !linux

package runtime

// DST makes host isolation an enforced invariant, and the syscall-floor fences,
// virtual file descriptors, memfd page cache, futexes, and InheritFile backing
// that guarantee exist only on Linux. Rather than ship a dst build whose
// isolation contract cannot be enforced, -tags dst is refused on non-Linux
// operating systems at compile time; untagged builds remain valid upstream
// ports across all supported operating systems.
var _ int = "DST (-tags dst) is unsupported on this operating system; see docs/dst/design.md, the interception boundary's platform scope"
