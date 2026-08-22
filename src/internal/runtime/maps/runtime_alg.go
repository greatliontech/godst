// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package maps

import (
	"internal/byteorder"
	"internal/cpu"
	"internal/goarch"
	"unsafe"
)

// runtime variable to check if the processor we're running on
// actually supports the instructions used by the AES-based
// hash implementation.
var UseAeshash bool

const hashRandomBytes = goarch.PtrSize / 4 * 64

// used to seed the hash function
var aeskeysched [hashRandomBytes]byte

// used in hash{32,64}.go to seed the hash function
var hashkey [4]uintptr

func AlgInit() {
	// Always intialize hashkey.
	//
	// See #78073
	if dstBuild {
		// -tags dst: derive the key from a fixed constant,
		// position-independently, so map iteration order is identical
		// across builds and binary compositions (see initAlgAES; the
		// uintptr() mirrors the production truncation on 32-bit).
		var k [len(hashkey)]uint64
		dstFixedHashKey(k[:], dstHashKeySaltFallback)
		for i := range hashkey {
			hashkey[i] = uintptr(k[i])
		}
	} else {
		for i := range hashkey {
			hashkey[i] = uintptr(bootstrapRand())
		}
	}

	// Install AES hash algorithms if the instructions needed are present.
	if (goarch.GOARCH == "386" || goarch.GOARCH == "amd64") &&
		cpu.X86.HasAES && // AESENC
		cpu.X86.HasSSSE3 && // PSHUFB
		cpu.X86.HasSSE41 { // PINSR{D,Q}

		// In memHashAES we have global variables that should be properly aligned.
		//
		// See #12415
		if !checkMasksAndShiftsAlignment() {
			fatal("maps: global variables for AES hashing are not properly aligned!")
		}
		initAlgAES()

		if memHashUsesVAES {
			// We are using intrinsics hash implementation.
			// Override the UseAeshash in this case, since it uses VAES (AVX) instructions.
			// While assembly implementation used AES-NI instructions,
			// simd intrinsics only provide access to AVX ones.
			UseAeshash = cpu.X86.HasAVX
		}
		return
	}
	if goarch.GOARCH == "arm64" && cpu.ARM64.HasAES {
		initAlgAES()
		return
	}
}

func initAlgAES() {
	UseAeshash = true
	key := (*[hashRandomBytes / 8]uint64)(unsafe.Pointer(&aeskeysched))
	if dstBuild {
		// -tags dst fixes the global map hash key for determinism (the
		// runtime seeds its global RNG from a constant; see design.md "Map
		// hash key requires -tags dst"). But filling the key from
		// bootstrapRand draws from the global RNG at whatever *stream
		// position* AlgInit reaches, and that position depends on how many
		// startup draws preceded it — which varies with binary composition
		// and -race/-msan instrumentation. So a bootstrapRand-derived key is
		// only fixed *per build*: a different build shifts the key (measured:
		// -race shifts it one word), changing multi-group map iteration order
		// (keys land in different groups via hash & mask). Derive the key
		// from a fixed constant instead, so it is position-independent and
		// the key — and thus map order — is identical across builds and
		// compositions. Per-map seed variation (m.seed) is unaffected; only
		// this one process-global key is fixed.
		dstFixedHashKey(key[:], dstHashKeySaltAES)
		return
	}
	// Initialize with random data so hash collisions will be hard to engineer.
	for i := range key {
		key[i] = bootstrapRand()
	}
}

//go:nosplit
func add(p unsafe.Pointer, x uintptr) unsafe.Pointer {
	return unsafe.Pointer(uintptr(p) + x)
}

// Note: These routines perform the read with a native endianness.
func readUnaligned32(p unsafe.Pointer) uint32 {
	q := (*[4]byte)(p)
	if goarch.BigEndian {
		return byteorder.BEUint32(q[:])
	}
	return byteorder.LEUint32(q[:])
}

func readUnaligned64(p unsafe.Pointer) uint64 {
	q := (*[8]byte)(p)
	if goarch.BigEndian {
		return byteorder.BEUint64(q[:])
	}
	return byteorder.LEUint64(q[:])
}
