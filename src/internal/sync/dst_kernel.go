// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package sync

import (
	rtatomic "internal/runtime/atomic"
	_ "unsafe"
)

//go:linkname runtime_dstKernelSectionEnter
func runtime_dstKernelSectionEnter()

//go:linkname runtime_dstKernelSectionExit
func runtime_dstKernelSectionExit()

//go:linkname runtime_SemacquireKernel
func runtime_SemacquireKernel(s *uint32)

const (
	kernelLocked      = 1
	kernelWaiterShift = 1
)

// KernelMutex is an uninterruptible critical-section lock for modeled OS,
// network, environment, and simulation-topology tables under -tags dst.
//
// Unlike user sync.Mutex, KernelMutex enters the runtime kernel section
// (dstKernelDepth > 0 and per-goroutine TSan sync+access suppression) before
// touching lock state and exits only after releasing it. It therefore:
//   - never yields at Level-2 access/atomic hooks, post-go yields, heap-limit
//     GC triggers, or stack-shrink preemptions while held;
//   - never emits universe-global DPOR conflicts or happens-before edges;
//   - never publishes cross-goroutine TSan happens-before edges across
//     simulated processes or unrelated goroutines; and
//   - never trips uninstrumented user-mutex detection when contended.
//
// Callers of KernelMutex must never block on channels, timers, or user locks
// while holding it. Caller byte-slice copies run outside KernelMutex so user
// buffer accesses remain race- and DPOR-visible, while intentional user-level
// synchronization edges are recorded atomically inside KernelMutex via the
// runtime's kernel-sync helpers.
type KernelMutex struct {
	state rtatomic.Int32
	sema  uint32
}

func (m *KernelMutex) Lock() {
	runtime_dstKernelSectionEnter()
	if m.state.CompareAndSwap(0, kernelLocked) {
		return
	}
	m.lockSlow()
}

func (m *KernelMutex) lockSlow() {
	for {
		old := m.state.Load()
		if old&kernelLocked == 0 {
			if m.state.CompareAndSwap(old, old|kernelLocked) {
				return
			}
			continue
		}
		if m.state.CompareAndSwap(old, old+1<<kernelWaiterShift) {
			runtime_SemacquireKernel(&m.sema)
		}
	}
}

func (m *KernelMutex) Unlock() {
	new := m.state.Add(-kernelLocked)
	if new != 0 {
		m.unlockSlow(new)
	}
	runtime_dstKernelSectionExit()
}

func (m *KernelMutex) unlockSlow(new int32) {
	if (new+kernelLocked)&kernelLocked == 0 {
		fatal("sync: unlock of unlocked KernelMutex")
	}
	for {
		old := m.state.Load()
		waiters := old >> kernelWaiterShift
		if waiters == 0 || old&kernelLocked != 0 {
			return
		}
		if m.state.CompareAndSwap(old, old-(waiters<<kernelWaiterShift)) {
			for rem := waiters; rem > 0; {
				woke, crashed, _ := runtime_dstSemrelease(&m.sema, false, 0)
				var consumed int32 = 1
				if woke {
					consumed = 1 + crashed
				} else if crashed > 0 {
					consumed = crashed
				}
				if consumed <= rem {
					rem -= consumed
				} else {
					m.state.Add(-((consumed - rem) << kernelWaiterShift))
					rem = 0
				}
			}
			return
		}
	}
}

// KernelOnce is the modeled-kernel counterpart of sync.Once, backed by
// KernelMutex so a process crash or exit cannot strand its internal lock mid-Do
// and its state transitions stay invisible to user DPOR and TSan histories.
type KernelOnce struct {
	m    KernelMutex
	done bool
}

func (o *KernelOnce) Do(f func()) {
	o.m.Lock()
	defer o.m.Unlock()
	if !o.done {
		o.done = true
		f()
	}
}
