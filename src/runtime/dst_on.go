// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime

// dstBuild reports whether the program was built with -tags dst. In a DST build
// the process-global map hash key is seeded from a fixed constant (see
// randinit) so that map iteration order is reproducible across runs; testing/simulation
// requires such a build. See docs/dst/design.md.
const dstBuild = true

// dstG holds the per-goroutine DST state embedded into runtime.g under -tags dst.
// In untagged builds (dst_off.go), dstG is an empty struct{} embedded at a
// non-trailing position, adding 0 bytes to runtime.g.
//
// Field ordering packs dstHost alongside dstProc, dstProcSlot alongside dstPid,
// dstGCInternal and dstInternalGC alongside dstAccWrite, and dstKernelDepth
// alongside the trailing bool flags, so dstG is 76 bytes on 32-bit
// architectures and 96 bytes on 64-bit architectures.
type dstG struct {
	dstrand        uint64        // deterministic per-g RNG state for DST (testing/simulation); unused when DST off
	dstPrio        int64         // DST PCT scheduling priority (higher runs first); unused when DST off or strategy != PCT
	dstSeq         uint64        // DST stable per-bubble goroutine creation index+1 (0 = unassigned); canonicalizes Random/PCT choices and scheduled replay
	dstHost        uint32        // DST host identity (0 = root/default host); set by testing/simulation.Host, inherited parent→child at newproc1
	dstProc        uint32        // DST logical process identity (0 = root/default process); set by testing/simulation.Process, inherited parent→child at newproc1
	dstHostScope   *dstHostScope // DST immutable entered-Host ancestry; inherited parent→child so CrashHost can find nested root-process threads after dstHost changes
	dstProcSlot    uint32        // DST per-invocation process resource/env slot (0 = root/default); monotonic per run and reset on run teardown
	dstPid         int32         // DST per-process simulated pid (os.Getpid); set per-invocation by testing/simulation.Process, inherited parent→child at newproc1
	dstAccAddr     uintptr       // DST pending memory-access address for the next transition (0 = none); set by dstAccessYield, recorded at the scheduling decision for DPOR
	dstAccSize     uintptr       // DST pending memory-access size in bytes; paired with dstAccAddr for interval-overlap dependencies
	dstAccWrite    bool          // DST pending access is a write (vs read); paired with dstAccAddr
	dstGCInternal  bool          // DST simulation member is executing inside a GC disassociation scope; scheduled exploration runs this state transparently
	dstInternalGC  bool          // DST internal forced-GC cycle (dstGCForce) bypassing foreign-caller refusal without a wrapper frame on runtime.GC
	dstAccPC       uintptr       // DST pending access hook call-site PC; paired with dstAccAddr
	dstAccCount    uint64        // DST per-bubble access ordinal for forced replay yield points; unused otherwise
	dstAccPend     bool          // DST pending transition came from a hook call; false means a synthetic coarse decision
	dstAccAuto     bool          // DST pending transition came from dst-race memory auto-instrumentation; stack addresses are private for this path
	dstHostIO      bool          // DST explicit inherited-file capability is performing one trusted host operation; scoped on the calling g and never inherited
	dstSimG        bool          // DST sticky membership in the ACTIVE simulation's bubble
	dstKernelDepth int32         // DST nesting depth of uninterruptible modeled kernel critical sections; suppresses scheduler yields and stack-shrink preemption while > 0
}

// dstP holds the per-P DST state embedded into runtime.p under -tags dst.
type dstP struct {
	dstRunqOvf          gQueue
	dstSpecialStamp     dstCallbackStamp
	dstSpecialStaged    bool
	dstCleanupSlabBlock *cleanupBlock
	dstCleanupSlab      *[len(cleanupBlock{}.cleanups)]dstCallbackStamp
}

// dstTimer holds the per-timer DST state embedded into runtime.timer under -tags dst.
type dstTimer struct {
	dstHost      uint32
	dstBase      bool
	dstReg       uint64
	dstFakeNext  *timer
	dstWhenShift int64
}

// dstBubble holds the per-bubble DST state embedded into runtime.synctestBubble under -tags dst.
type dstBubble struct {
	gcDrain     *g
	gcDrainExit bool
	gcDrainDied bool
}
