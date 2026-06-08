// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime

// dstBuild reports whether the program was built with -tags dst. In a DST build
// the process-global map hash key is seeded from a fixed constant (see
// randinit) so that map iteration order is reproducible across runs; runtime/dst
// requires such a build. See docs/dst/design.md.
const dstBuild = true
