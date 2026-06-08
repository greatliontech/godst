// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package runtime

// dstBuild is false in a normal build: the map hash key is seeded from OS
// entropy (hash-flooding protection) and testing/simulation.Run refuses to run. See
// dst_on.go and docs/dst/design.md.
const dstBuild = false
