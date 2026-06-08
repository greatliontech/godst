// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Random number generation

package runtime

import (
	"internal/byteorder"
	"internal/chacha8rand"
	"internal/goarch"
	"internal/runtime/math"
	"unsafe"
	_ "unsafe" // for go:linkname
)

// OS-specific startup can set startupRand if the OS passes
// random data to the process at startup time.
// For example Linux passes 16 bytes in the auxv vector.
var startupRand []byte

// globalRand holds the global random state.
// It is only used at startup and for creating new m's.
// Otherwise the per-m random state should be used
// by calling goodrand.
var globalRand struct {
	lock  mutex
	seed  [32]byte
	state chacha8rand.State
	init  bool
}

var readRandomFailed bool

// randinit initializes the global random state.
// It must be called before any use of grand.
func randinit() {
	lock(&globalRand.lock)
	if globalRand.init {
		fatal("randinit twice")
	}

	seed := &globalRand.seed
	switch {
	case dstBuild:
		// In a DST build (-tags dst), seed the global generator from a fixed
		// constant so the process-global map hash key (derived in alginit via
		// bootstrapRand) is identical every run. Map iteration order is still
		// seed-varied via the per-g m.seed; only this global key must be fixed,
		// and it cannot be re-seeded at runtime without corrupting maps created
		// before DST activation. See docs/dst/design.md.
		dstFixedSeed(seed)
	case len(startupRand) >= 16 &&
		// Check that at least the first two words of startupRand weren't
		// cleared by any libc initialization.
		!allZero(startupRand[:8]) && !allZero(startupRand[8:16]):
		for i, c := range startupRand {
			seed[i%len(seed)] ^= c
		}
	default:
		if readRandom(seed[:]) != len(seed) || allZero(seed[:]) {
			// readRandom should never fail, but if it does we'd rather
			// not make Go binaries completely unusable, so make up
			// some random data based on the current time.
			readRandomFailed = true
			readTimeRandom(seed[:])
		}
	}
	globalRand.state.Init(*seed)
	clear(seed[:])

	if startupRand != nil {
		if dstBuild {
			// Don't consume the global generator to scrub startupRand in a DST
			// build: keep the hash-key derivation at a fixed stream position.
			startupRand = nil
		} else {
			// Overwrite startupRand instead of clearing it, in case cgo programs
			// access it after we used it.
			for len(startupRand) > 0 {
				buf := make([]byte, 8)
				for {
					if x, ok := globalRand.state.Next(); ok {
						byteorder.BEPutUint64(buf, x)
						break
					}
					globalRand.state.Refill()
				}
				n := copy(startupRand, buf)
				startupRand = startupRand[n:]
			}
			startupRand = nil
		}
	}

	globalRand.init = true
	unlock(&globalRand.lock)
}

// readTimeRandom stretches any entropy in the current time
// into entropy the length of r and XORs it into r.
// This is a fallback for when readRandom does not read
// the full requested amount.
// Whatever entropy r already contained is preserved.
func readTimeRandom(r []byte) {
	// Inspired by wyrand.
	// An earlier version of this code used getg().m.procid as well,
	// but note that this is called so early in startup that procid
	// is not initialized yet.
	v := uint64(nanotime())
	for len(r) > 0 {
		v ^= 0xa0761d6478bd642f
		v *= 0xe7037ed1a0b428db
		size := 8
		if len(r) < 8 {
			size = len(r)
		}
		for i := 0; i < size; i++ {
			r[i] ^= byte(v >> (8 * i))
		}
		r = r[size:]
		v = v>>32 | v<<32
	}
}

func allZero(b []byte) bool {
	var acc byte
	for _, x := range b {
		acc |= x
	}
	return acc == 0
}

// dstFixedSeed fills the 32-byte global chacha8 key from a fixed constant via
// splitmix64, for DST builds (see randinit). The value is arbitrary but fixed,
// so the derived map hash key is the same every run; it is never all-zero.
func dstFixedSeed(seed *[32]byte) {
	x := uint64(0x6470736565643031) // "dpseed01"
	for i := 0; i < len(seed); i += 8 {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		byteorder.BEPutUint64(seed[i:i+8], z^(z>>31))
	}
}

// bootstrapRand returns a random uint64 from the global random generator.
func bootstrapRand() uint64 {
	lock(&globalRand.lock)
	if !globalRand.init {
		fatal("randinit missed")
	}
	for {
		if x, ok := globalRand.state.Next(); ok {
			unlock(&globalRand.lock)
			return x
		}
		globalRand.state.Refill()
	}
}

// bootstrapRandReseed reseeds the bootstrap random number generator,
// clearing from memory any trace of previously returned random numbers.
func bootstrapRandReseed() {
	lock(&globalRand.lock)
	if !globalRand.init {
		fatal("randinit missed")
	}
	globalRand.state.Reseed()
	unlock(&globalRand.lock)
}

// rand32 is uint32(rand()), called from compiler-generated code.
//
//go:nosplit
func rand32() uint32 {
	return uint32(rand())
}

// rand returns a random uint64 from the per-m chacha8 state.
// This is called from compiler-generated code.
//
// Do not change signature: used via linkname from other packages.
//
//go:nosplit
//go:linkname rand
func rand() uint64 {
	gp := getg()
	if dstActive() {
		// Under deterministic scheduling, draw from the per-g DST stream. rand is
		// the source for the math/rand and math/rand/v2 globals (linkname'd to
		// runtime.rand), for map seeds/iteration (maps.rand), and for
		// compiler-emitted map seeds and NaN-key hashing — all application-
		// observable, and all drawn at counts that are a function of the
		// goroutine's own logical history, so routing them per-g is correct.
		// The one internal caller drawn at a load-dependent count on a user
		// goroutine's stack — mrandinit, when a new m is spawned — is exempted
		// there (it seeds from bootstrapRand under DST). randomizeScheduler's
		// randn (proc.go) is -race-only and outside DST scope.
		return dstrandUint64(gp)
	}
	// Note: We avoid acquirem here so that in the fast path
	// there is just a getg, an inlined c.Next, and a return.
	// The performance difference on a 16-core AMD is
	// 3.7ns/call this way versus 4.3ns/call with acquirem (+16%).
	mp := gp.m
	c := &mp.chacha8
	for {
		// Note: c.Next is marked nosplit,
		// so we don't need to use mp.locks
		// on the fast path, which is that the
		// first attempt succeeds.
		x, ok := c.Next()
		if ok {
			return x
		}
		mp.locks++ // hold m even though c.Refill may do stack split checks
		c.Refill()
		mp.locks--
	}
}

//go:linkname maps_rand internal/runtime/maps.rand
func maps_rand() uint64 {
	// rand() already draws map seeds and iteration offsets from the per-g DST
	// stream under DST, so map order is a function of the
	// creating and iterating goroutine's logical history, not of which m runs it.
	return rand()
}

// dstrandUint64 returns the next value of g's deterministic DST RNG stream,
// advancing it. It is used only under DST, to make
// per-goroutine randomness — select poll order, map seed and iteration order,
// and child-goroutine seeds — a function of the goroutine's own logical history,
// independent of which m runs it and of runtime-internal RNG draws (work
// stealing, goroutine tracking) that share the per-m streams. The stream is a
// splitmix64 sequence keyed by gp.dstrand, seeded as a deterministic tree: the
// root from the DST seed (dstActivate, or a synctest bubble re-root via
// dstBubbleRoot) and each child from its parent at newproc1.
//
//go:nosplit
func dstrandUint64(gp *g) uint64 {
	gp.dstrand += 0x9e3779b97f4a7c15
	z := gp.dstrand
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// dstrandn returns a deterministic value in [0,n) from g's DST RNG stream.
//
//go:nosplit
func dstrandn(gp *g, n uint32) uint32 {
	return uint32((uint64(uint32(dstrandUint64(gp))) * uint64(n)) >> 32)
}

// dstReadRandom fills b from the active run's deterministic per-g RNG stream and
// reports true, or returns false (filling nothing) when no run is active. It is
// the crypto/rand entropy seam: crypto/internal/sysrand.Read calls it first, so
// inside a Run all crypto/rand output — UUIDs, TLS nonces, tokens, key material —
// is a reproducible function of the seed, drawn from the same per-g stream as
// math/rand. Production crypto is untouched: dstActive() is false outside a run
// (dstSeed is never set in production — only simulation.Run, which requires
// -tags dst, sets it), so the OS-entropy path is taken there. The gate is the
// same cheap atomic load rand() already does on its hot path. Bytes are drawn
// big-endian per 8-byte word, matching dstFixedSeed.
//
//go:linkname dstReadRandom
func dstReadRandom(b []byte) bool {
	if !dstActive() {
		return false
	}
	gp := getg()
	for len(b) >= 8 {
		byteorder.BEPutUint64(b, dstrandUint64(gp))
		b = b[8:]
	}
	if len(b) > 0 {
		var tmp [8]byte
		byteorder.BEPutUint64(tmp[:], dstrandUint64(gp))
		copy(b, tmp[:])
	}
	return true
}

// dstBubbleRoot derives a synctest bubble's per-g tree root from the process DST
// seed, independent of the bubble's position in the global goroutine tree. This
// re-roots the tree per bubble so a bubble's randomness is reproducible
// regardless of what ran before it in the same process — i.e. a test reproduces
// identically in isolation. It is a splitmix64 step so the bubble root differs
// from g0's raw root (schedinit). A future per-bubble seed (the public DST
// control API) would override this.
func dstBubbleRoot(seed uint64) uint64 {
	seed += 0x9e3779b97f4a7c15
	seed = (seed ^ (seed >> 30)) * 0xbf58476d1ce4e5b9
	seed = (seed ^ (seed >> 27)) * 0x94d049bb133111eb
	return seed ^ (seed >> 31)
}

// mrandinit initializes the random state of an m.
func mrandinit(mp *m) {
	var seed [4]uint64
	for i := range seed {
		seed[i] = bootstrapRand()
	}
	bootstrapRandReseed() // erase key we just extracted
	mp.chacha8.Init64(seed)
	if dstActive() {
		// Don't seed from rand() under DST: rand() routes through the caller
		// goroutine's per-g stream, and mrandinit can run on a user goroutine's
		// stack (ready -> wakep -> newm -> allocm -> mcommoninit -> mrandinit, no
		// systemstack switch). Drawing here would advance that goroutine's per-g
		// stream by a load-dependent amount (new-m creation timing) and so
		// reintroduce the nondeterminism the per-g tree removes. Seed from the
		// global bootstrap generator instead; the new m's cheaprand is not
		// application-observable under DST (select/map/math-rand use the per-g
		// stream).
		mp.cheaprand = bootstrapRand()
	} else {
		mp.cheaprand = rand()
	}
}

// randn is like rand() % n but faster.
// Do not change signature: used via linkname from other packages.
//
//go:nosplit
//go:linkname randn
func randn(n uint32) uint32 {
	// See https://lemire.me/blog/2016/06/27/a-fast-alternative-to-the-modulo-reduction/
	return uint32((uint64(uint32(rand())) * uint64(n)) >> 32)
}

// cheaprand is a non-cryptographic-quality 32-bit random generator
// suitable for calling at very high frequency (such as during scheduling decisions)
// and at sensitive moments in the runtime (such as during stack unwinding).
// it is "cheap" in the sense of both expense and quality.
//
// cheaprand must not be exported to other packages:
// the rule is that other packages using runtime-provided
// randomness must always use rand.
//
// cheaprand should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/gopkg
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname cheaprand
//go:nosplit
func cheaprand() uint32 {
	mp := getg().m
	// Implement wyrand: https://github.com/wangyi-fudan/wyhash
	// Only the platform that math.Mul64 can be lowered
	// by the compiler should be in this list.
	if goarch.IsAmd64|goarch.IsArm64|goarch.IsPpc64|
		goarch.IsPpc64le|goarch.IsMips64|goarch.IsMips64le|
		goarch.IsS390x|goarch.IsRiscv64|goarch.IsLoong64 == 1 {
		mp.cheaprand += 0xa0761d6478bd642f
		hi, lo := math.Mul64(mp.cheaprand, mp.cheaprand^0xe7037ed1a0b428db)
		return uint32(hi ^ lo)
	}

	// Implement xorshift64+: 2 32-bit xorshift sequences added together.
	// Shift triplet [17,7,16] was calculated as indicated in Marsaglia's
	// Xorshift paper: https://www.jstatsoft.org/article/view/v008i14/xorshift.pdf
	// This generator passes the SmallCrush suite, part of TestU01 framework:
	// http://simul.iro.umontreal.ca/testu01/tu01.html
	t := (*[2]uint32)(unsafe.Pointer(&mp.cheaprand))
	s1, s0 := t[0], t[1]
	s1 ^= s1 << 17
	s1 = s1 ^ s0 ^ s1>>7 ^ s0>>16
	t[0], t[1] = s0, s1
	return s0 + s1
}

// cheaprand64 is a non-cryptographic-quality 63-bit random generator
// suitable for calling at very high frequency (such as during sampling decisions).
// it is "cheap" in the sense of both expense and quality.
//
// cheaprand64 must not be exported to other packages:
// the rule is that other packages using runtime-provided
// randomness must always use rand.
//
// cheaprand64 should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/zhangyunhao116/fastrand
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname cheaprand64
//go:nosplit
func cheaprand64() int64 {
	return int64(cheaprand())<<31 ^ int64(cheaprand())
}

// cheaprandn is like cheaprand() % n but faster.
//
// cheaprandn must not be exported to other packages:
// the rule is that other packages using runtime-provided
// randomness must always use randn.
//
// cheaprandn should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/phuslu/log
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname cheaprandn
//go:nosplit
func cheaprandn(n uint32) uint32 {
	// See https://lemire.me/blog/2016/06/27/a-fast-alternative-to-the-modulo-reduction/
	return uint32((uint64(cheaprand()) * uint64(n)) >> 32)
}

// Too much legacy code has go:linkname references
// to runtime.fastrand and friends, so keep these around for now.
// Code should migrate to math/rand/v2.Uint64,
// which is just as fast, but that's only available in Go 1.22+.
// It would be reasonable to remove these in Go 1.24.
// Do not call these from package runtime.

//go:linkname legacy_fastrand runtime.fastrand
func legacy_fastrand() uint32 {
	return uint32(rand())
}

//go:linkname legacy_fastrandn runtime.fastrandn
func legacy_fastrandn(n uint32) uint32 {
	return randn(n)
}

//go:linkname legacy_fastrand64 runtime.fastrand64
func legacy_fastrand64() uint64 {
	return rand()
}
