// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package syscall

// Stock build: the interception-boundary fences fold away. dstSimFenced is a
// constant false, so every `if dstSimFenced && …` branch is dead-code-
// eliminated and the identifiers below exist only to satisfy the type checker.
const dstSimFenced = false

func dstFenceActive() bool { return false }

var dstErrUnsupported error
