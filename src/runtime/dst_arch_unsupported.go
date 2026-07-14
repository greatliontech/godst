// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && (loong64 || mips || mipsle || mips64 || mips64le)

package runtime

// DST makes host isolation an enforced invariant, and these architectures
// carry raw-entry surfaces the fork's fences do not claim (loong64's direct
// statx Fstat path, MIPS o32's ninth-argument Syscall9 entry) — verifiable
// only on emulated targets. Rather than ship a dst build whose fence contract
// cannot be enforced, the build is refused here at compile time; untagged
// builds are unaffected. Restoring an architecture means restoring its arch
// arms (`git log --all -- src/runtime/dst_arch_unsupported.go` finds the
// removal) and verifying the interception fences on the target.
var _ int = "DST (-tags dst) is unsupported on this architecture; see docs/dst/design.md, the interception boundary's architecture scope"
