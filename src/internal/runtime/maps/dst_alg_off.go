// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package maps

const dstBuild = false

// Never called in a non-dst build (the dstBuild const folds the call away);
// declared so AlgInit compiles in both modes.
func dstFixedHashKey(words []uint64, salt uint64) {}

const (
	dstHashKeySaltAES      = 0
	dstHashKeySaltFallback = 0
)
