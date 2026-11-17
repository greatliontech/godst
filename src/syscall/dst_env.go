// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && (unix || (js && wasm) || plan9 || wasip1)

package syscall

import (
	isync "internal/sync"
	"runtime"
	"unsafe"
)

//go:linkname dstEnvSlotTeardown
func dstEnvSlotTeardown(slot uint32) {
	if slot == 0 {
		return
	}
	ep := dstEnvRunEpoch()
	dstEnvMu.Lock()
	defer dstEnvMu.Unlock()
	if dstEnvBySlot == nil || ep != dstEnvEpoch {
		dstEnvBySlot = make(map[[2]uint32]*dstProcEnv)
		dstEnvEpoch = ep
		return
	}
	for key := range dstEnvBySlot {
		if key[1] == slot {
			delete(dstEnvBySlot, key)
		}
	}
}

//go:linkname dstEnvHostTeardown
func dstEnvHostTeardown(host uint32) {
	ep := dstEnvRunEpoch()
	dstEnvMu.Lock()
	defer dstEnvMu.Unlock()
	if dstEnvBySlot == nil || ep != dstEnvEpoch {
		dstEnvBySlot = make(map[[2]uint32]*dstProcEnv)
		dstEnvEpoch = ep
		return
	}
	for key := range dstEnvBySlot {
		if key[0] == host {
			delete(dstEnvBySlot, key)
		}
	}
}

// dstEnvEnabled is the compile-time gate for the per-process copy-on-write
// environment; a stock build folds it to false and DCEs the branch in each
// env_unix.go entry point.
const dstEnvEnabled = true

//go:linkname dstEnvDispatchLock runtime.dstEnvDispatchLock
func dstEnvDispatchLock()

//go:linkname dstEnvDispatchUnlock runtime.dstEnvDispatchUnlock
func dstEnvDispatchUnlock()

//go:linkname dstSimEnvKey runtime.dstSimEnvKey
func dstSimEnvKey() (host, slot uint32, ok bool)

//go:linkname dstEnvRunEpoch runtime.dstNetEpoch
func dstEnvRunEpoch() uint64

//go:linkname dstKernelSectionEnter runtime.dstKernelSectionEnter
func dstKernelSectionEnter()

//go:linkname dstKernelSectionExit runtime.dstKernelSectionExit
func dstKernelSectionExit()

//go:linkname dstCoarseDep runtime.dstCoarseDep
func dstCoarseDep(id uintptr, write bool)

//go:linkname dstInSimBubble runtime.dstInSimBubble
func dstInSimBubble() bool

// Per-(host, process-invocation) copy-on-write environment (design.md
// "Environment surface"). Under a run each simulated machine's root context
// (slot 0) and each Process invocation (slot >= 1) operates on its own copy of
// the host environment: a Setenv in one process or host is never visible to
// another process, another host, or the real host process, and non-bubble
// goroutines retain direct access to the real host environment.
var (
	dstEnvMu          isync.KernelMutex
	dstEnvEpoch       uint64
	dstEnvBySlot      map[[2]uint32]*dstProcEnv
	dstEnvRunSnapshot []string
	dstEnvSnapValid   bool
)

func dstCloneEnvString(s string) string {
	if len(s) == 0 {
		return ""
	}
	b := make([]byte, len(s))
	copy(b, s)
	return unsafe.String(&b[0], len(b))
}

// dstEnvRunActivate captures and byte-clones the host environment snapshot at
// simulation Run entry (design.md "Environment surface"). Called under
// dstEnvDispatchLock before dstActivate so mid-run non-bubble host Setenv /
// Unsetenv / Clearenv calls neither perturb simulated (host, slot) views nor
// race with lazy per-slot environment materialization.
//
//go:linkname dstEnvRunActivate
func dstEnvRunActivate() {
	dstKernelSectionEnter()
	defer dstKernelSectionExit()

	copyenv()

	envLock.RLock()
	snap := make([]string, len(envs))
	for i, s := range envs {
		snap[i] = dstCloneEnvString(s)
	}
	envLock.RUnlock()

	dstEnvMu.Lock()
	dstEnvRunSnapshot = snap
	dstEnvSnapValid = true
	dstEnvBySlot = nil
	dstEnvMu.Unlock()
}

// dstEnvRunDeactivate releases the run-entry host environment snapshot and
// per-slot views when a simulation Run exits.
//
//go:linkname dstEnvRunDeactivate
func dstEnvRunDeactivate() {
	dstEnvMu.Lock()
	dstEnvRunSnapshot = nil
	dstEnvSnapValid = false
	dstEnvBySlot = nil
	dstEnvMu.Unlock()
}

type dstProcEnv struct {
	envs []string       // "key=value"; "" means deleted or duplicate (as in the host)
	env  map[string]int // key -> first index in envs
}

// dstEnvCurrent returns the calling bubble goroutine's environment copy while
// a run's bubble environment is active, else nil. Must not be called while
// holding dstEnvMu.
func dstEnvCurrent() *dstProcEnv {
	host, slot, ok := dstSimEnvKey()
	if !ok {
		return nil
	}
	ep := dstEnvRunEpoch()
	key := [2]uint32{host, slot}

	dstEnvMu.Lock()
	if dstEnvBySlot == nil || ep != dstEnvEpoch {
		dstEnvBySlot = make(map[[2]uint32]*dstProcEnv)
		dstEnvEpoch = ep
	}
	if pe := dstEnvBySlot[key]; pe != nil {
		dstEnvMu.Unlock()
		return pe
	}
	var snap []string
	valid := dstEnvSnapValid
	if valid {
		snap = dstEnvRunSnapshot
	}
	dstEnvMu.Unlock()

	// Build the copy outside dstEnvMu (fallback path takes envLock when invoked
	// outside a full simulation.Run harness).
	var pe *dstProcEnv
	if valid {
		pe = dstNewProcEnvFromSnapshot(snap)
	} else {
		pe = dstNewProcEnvFromHost()
	}

	dstEnvMu.Lock()
	defer dstEnvMu.Unlock()
	if existing := dstEnvBySlot[key]; existing != nil {
		return existing
	}
	dstEnvBySlot[key] = pe
	return pe
}

func dstNewProcEnvFromSnapshot(snap []string) *dstProcEnv {
	dstKernelSectionEnter()
	defer dstKernelSectionExit()

	e := make([]string, len(snap))
	copy(e, snap)
	return dstIndexProcEnv(e)
}

// dstNewProcEnvFromHost copies and byte-clones the host environment into a
// fresh per-process view when no run-entry snapshot is active.
func dstNewProcEnvFromHost() *dstProcEnv {
	dstKernelSectionEnter()
	defer dstKernelSectionExit()

	copyenv() // dedup the host envs and build the host env map (idempotent)

	envLock.RLock()
	e := make([]string, len(envs))
	for i, s := range envs {
		e[i] = dstCloneEnvString(s)
	}
	envLock.RUnlock()

	return dstIndexProcEnv(e)
}

func dstIndexProcEnv(e []string) *dstProcEnv {
	pe := &dstProcEnv{envs: e, env: make(map[string]int, len(e))}
	for i, s := range e {
		for j := 0; j < len(s); j++ {
			if s[j] == '=' {
				key := s[:j]
				if _, ok := pe.env[key]; !ok {
					pe.env[key] = i
				}
				break
			}
		}
	}
	return pe
}

func (pe *dstProcEnv) getenv(key string) (value string, found bool) {
	if len(key) == 0 {
		return "", false
	}
	dstEnvMu.Lock()
	defer dstEnvMu.Unlock()
	i, ok := pe.env[key]
	if !ok {
		return "", false
	}
	s := pe.envs[i]
	for j := 0; j < len(s); j++ {
		if s[j] == '=' {
			return s[j+1:], true
		}
	}
	return "", false
}

func (pe *dstProcEnv) setenv(key, value string) error {
	if len(key) == 0 {
		return EINVAL
	}
	for i := 0; i < len(key); i++ {
		if key[i] == '=' || key[i] == 0 {
			return EINVAL
		}
	}
	// On Plan 9, null is used as a separator, e.g. in $path.
	if runtime.GOOS != "plan9" {
		for i := 0; i < len(value); i++ {
			if value[i] == 0 {
				return EINVAL
			}
		}
	}

	dstEnvMu.Lock()
	defer dstEnvMu.Unlock()
	i, ok := pe.env[key]
	buf := make([]byte, len(key)+1+len(value))
	copy(buf, key)
	buf[len(key)] = '='
	copy(buf[len(key)+1:], value)
	kv := unsafe.String(&buf[0], len(buf))
	if ok {
		pe.envs[i] = kv
	} else {
		i = len(pe.envs)
		pe.envs = append(pe.envs, kv)
	}
	pe.env[kv[:len(key)]] = i
	// No runtimeSetenv: the host process environment is left untouched.
	return nil
}

func (pe *dstProcEnv) unsetenv(key string) error {
	dstEnvMu.Lock()
	defer dstEnvMu.Unlock()
	if i, ok := pe.env[key]; ok {
		pe.envs[i] = ""
		delete(pe.env, key)
	}
	return nil
}

func (pe *dstProcEnv) clearenv() {
	dstEnvMu.Lock()
	defer dstEnvMu.Unlock()
	pe.env = make(map[string]int)
	pe.envs = []string{}
}

func (pe *dstProcEnv) environ() []string {
	dstEnvMu.Lock()
	defer dstEnvMu.Unlock()
	a := make([]string, 0, len(pe.envs))
	for _, kv := range pe.envs {
		if kv != "" {
			a = append(a, kv)
		}
	}
	return a
}

// The dispatch functions env_unix.go calls. Each reports handled=false when no
// run env is active, so the caller falls through to the host implementation.

// dstEnvPrepare resolves the calling process's environment view and, for
// in-bubble simulation goroutines, drops the dispatch gate around the coarse
// per-process dependency announce so same-process env races yield to DPOR
// without holding dstEnvDispatchGate across a crashable schedule point.
func dstEnvPrepare(write bool) *dstProcEnv {
	pe := dstEnvCurrent()
	if pe == nil {
		return nil
	}
	if dstInSimBubble() {
		dstEnvDispatchUnlock()
		dstCoarseDep(uintptr(unsafe.Pointer(pe)), write)
		dstEnvDispatchLock()
	}
	return pe
}

func dstGetenv(key string) (value string, found, handled bool) {
	if pe := dstEnvPrepare(false); pe != nil {
		v, f := pe.getenv(key)
		return v, f, true
	}
	return "", false, false
}

func dstSetenv(key, value string) (err error, handled bool) {
	if pe := dstEnvPrepare(true); pe != nil {
		return pe.setenv(key, value), true
	}
	return nil, false
}

func dstUnsetenv(key string) (err error, handled bool) {
	if pe := dstEnvPrepare(true); pe != nil {
		return pe.unsetenv(key), true
	}
	return nil, false
}

func dstClearenv() (handled bool) {
	if pe := dstEnvPrepare(true); pe != nil {
		pe.clearenv()
		return true
	}
	return false
}

func dstEnviron() (env []string, handled bool) {
	if pe := dstEnvPrepare(false); pe != nil {
		return pe.environ(), true
	}
	return nil, false
}
