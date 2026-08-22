// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package maps

// dstBuild is true under -tags dst: the process-global map hash key is
// derived from a fixed constant (see AlgInit) so map iteration order is
// identical across runs, builds, and binary compositions. The runtime's own
// dstBuild constant is not visible here; this mirrors it for this package.
const dstBuild = true

// Distinct (nonzero) salts so the AES key schedule and the non-AES fallback hash
// key are independent constants: AlgInit always fills hashkey and, on an AES
// machine, aeskeysched as well, so both are fixed and decorrelated.
const (
	dstHashKeySaltAES      = 0x6165736b65793031 // "aeskey01"
	dstHashKeySaltFallback = 0x66616c6c6b793031 // "fallky01"
)

// dstFixedHashKey fills words from a fixed constant via splitmix64, for -tags dst
// builds (see AlgInit). Mirrors runtime.dstFixedSeed: the value is arbitrary but
// fixed and salted per key, so the derived map hash key is identical every run
// AND across builds/compositions — unlike a bootstrapRand-derived key, whose
// value depends on the composition-varying startup stream position.
func dstFixedHashKey(words []uint64, salt uint64) {
	x := uint64(0x686b657964737401) ^ salt // "hkeydst\x01"
	for i := range words {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		words[i] = z ^ (z >> 31)
	}
}
