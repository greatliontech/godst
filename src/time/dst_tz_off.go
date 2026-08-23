// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package time

// dstTZBuild is the bare-constant guard for the zoneinfo hooks: a constant
// the inliner skips, so open/read stay inlinable in the stock build.
const dstTZBuild = false

func dstTZFenceActive() bool { return false }
