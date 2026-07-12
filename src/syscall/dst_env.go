// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && (unix || (js && wasm) || plan9 || wasip1)

package syscall

import (
	"runtime"
	"sync"
	_ "unsafe" // for go:linkname
)

//go:linkname dstEnvProcessTeardown
func dstEnvProcessTeardown(proc uint32) {
	ep := dstEnvRunEpoch()
	dstEnvMu.Lock()
	defer dstEnvMu.Unlock()
	if dstEnvByProc == nil || ep != dstEnvEpoch {
		dstEnvByProc = make(map[uint32]*dstProcEnv)
		dstEnvEpoch = ep
		return
	}
	delete(dstEnvByProc, proc)
}

// dstEnvEnabled is the compile-time gate for the per-process copy-on-write
// environment; a stock build folds it to false and DCEs the branch in each
// env_unix.go entry point.
const dstEnvEnabled = true

//go:linkname dstEnvDispatchLock runtime.dstEnvDispatchLock
func dstEnvDispatchLock()

//go:linkname dstEnvDispatchUnlock runtime.dstEnvDispatchUnlock
func dstEnvDispatchUnlock()

//go:linkname dstSimEnvProc runtime.dstSimEnvProc
func dstSimEnvProc() (proc uint32, ok bool)

//go:linkname dstEnvRunEpoch runtime.dstNetEpoch
func dstEnvRunEpoch() uint64

// Per-process copy-on-write environment (design.md "Environment surface").
// Under a run each simulated process operates on its own copy of the host
// environment: a Setenv in one process is never visible to another process or
// the host, and reads of unmodified variables return the host value (machine
// state, like the inherited stdio handles). The copies are keyed by process id
// (g.dstProc), reset per run (the run epoch advances), and gated on the sim-env
// flag — process-global while set, like identity — so a non-bubble goroutine
// reading env mid-run sees the root process (proc 0). Isolation is the
// environment leg of DST-NODE-ISOLATION.
//
// A copy holds the same (envs, env) representation as the host globals, and the
// methods below are the env_unix.go algorithms applied to it, so semantics —
// Environ order, duplicate handling, EINVAL rules — are identical to the real
// environment, per process. The one difference from the host path is deliberate:
// no runtimeSetenv/Unsetenv/Clearenv, so the host process environment is never
// mutated (that is the isolation).
var (
	dstEnvMu     sync.Mutex
	dstEnvEpoch  uint64
	dstEnvByProc map[uint32]*dstProcEnv
)

type dstProcEnv struct {
	envs []string       // "key=value"; "" means deleted or duplicate (as in the host)
	env  map[string]int // key -> first index in envs
}

// dstEnvCurrent returns the calling process's environment copy while a run's env
// is active, else nil. It resets every copy when the run epoch advances, so a new
// run re-initializes each process's view from the host. Must not be called while
// holding dstEnvMu.
func dstEnvCurrent() *dstProcEnv {
	proc, ok := dstSimEnvProc()
	if !ok {
		return nil
	}
	// The epoch source (dstNetEpoch, gated on dstActive) and the env gate
	// (dstSimEnvSet) disagree in the brief set/clear windows around a run, where
	// dstSimEnvSet is already true but dstActive is not yet, so ep reads 0. That
	// is deliberately benign: the reset below is conservative (0 mismatches any
	// real epoch), and every in-run read — the only reads determinism depends on
	// — sees the true epoch N>0 and resets exactly once. No bubble read occurs in
	// those windows (the bubble spans only the active run).
	ep := dstEnvRunEpoch()

	dstEnvMu.Lock()
	if dstEnvByProc == nil || ep != dstEnvEpoch {
		dstEnvByProc = make(map[uint32]*dstProcEnv)
		dstEnvEpoch = ep
	}
	if pe := dstEnvByProc[proc]; pe != nil {
		dstEnvMu.Unlock()
		return pe
	}
	dstEnvMu.Unlock()

	// Build the copy outside dstEnvMu (dstNewProcEnvFromHost takes envLock).
	pe := dstNewProcEnvFromHost()

	dstEnvMu.Lock()
	defer dstEnvMu.Unlock()
	// The epoch is stable within a run (it advances only at run activation, on
	// the root goroutine, and runs do not overlap), so dstEnvByProc was not reset
	// between the two locks; only a concurrent goroutine of this same process
	// could have installed the copy first.
	if existing := dstEnvByProc[proc]; existing != nil {
		return existing
	}
	dstEnvByProc[proc] = pe
	return pe
}

// dstNewProcEnvFromHost copies the host environment (deduplicated) into a fresh
// per-process view and builds its key index, mirroring copyenv.
func dstNewProcEnvFromHost() *dstProcEnv {
	copyenv() // dedup the host envs and build the host env map (idempotent)

	envLock.RLock()
	e := make([]string, len(envs))
	copy(e, envs)
	envLock.RUnlock()

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
	kv := key + "=" + value
	if ok {
		pe.envs[i] = kv
	} else {
		i = len(pe.envs)
		pe.envs = append(pe.envs, kv)
	}
	pe.env[key] = i
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

func dstGetenv(key string) (value string, found, handled bool) {
	if pe := dstEnvCurrent(); pe != nil {
		v, f := pe.getenv(key)
		return v, f, true
	}
	return "", false, false
}

func dstSetenv(key, value string) (err error, handled bool) {
	if pe := dstEnvCurrent(); pe != nil {
		return pe.setenv(key, value), true
	}
	return nil, false
}

func dstUnsetenv(key string) (err error, handled bool) {
	if pe := dstEnvCurrent(); pe != nil {
		return pe.unsetenv(key), true
	}
	return nil, false
}

func dstClearenv() (handled bool) {
	if pe := dstEnvCurrent(); pe != nil {
		pe.clearenv()
		return true
	}
	return false
}

func dstEnviron() (env []string, handled bool) {
	if pe := dstEnvCurrent(); pe != nil {
		return pe.environ(), true
	}
	return nil, false
}
