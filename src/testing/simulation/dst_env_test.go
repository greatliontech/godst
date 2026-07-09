// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestDSTEnvPerProcessCOW checks the per-process copy-on-write environment
// (design.md "Environment surface"): each simulated process gets its own copy of
// the host environment, writes are isolated from other processes and from the
// host, and reads of unmodified variables return the host value.
func TestDSTEnvPerProcessCOW(t *testing.T) {
	const hostKey = "DST_ENV_HOST"
	const hostVal = "host-value"
	os.Setenv(hostKey, hostVal) // real host env (no run active)
	defer os.Unsetenv(hostKey)

	var (
		p1ReadHost   string
		p1ReadHostOK bool
		p1AfterSet   string
		p2SeesP1Set  bool
		p2ReadHost   string
	)

	Run(1, func() {
		Process("p1", func() {
			p1ReadHost, p1ReadHostOK = os.LookupEnv(hostKey) // unmodified -> host value
			os.Setenv("DST_ENV_P1", "p1-value")              // p1's own write
			p1AfterSet = os.Getenv("DST_ENV_P1")             // p1 sees its write
			os.Unsetenv(hostKey)                             // shadow the host var in p1's copy only
		})
		Process("p2", func() {
			_, p2SeesP1Set = os.LookupEnv("DST_ENV_P1") // must NOT see p1's write
			p2ReadHost, _ = os.LookupEnv(hostKey)       // p1's Unsetenv must not reach p2
		})
	})

	// Host isolation: a bubble's Setenv/Unsetenv never touched the host env.
	if v, ok := os.LookupEnv(hostKey); !ok || v != hostVal {
		t.Errorf("host %s after run = (%q, %v), want (%q, true): a bubble Setenv/Unsetenv leaked to the host", hostKey, v, ok, hostVal)
	}
	if _, ok := os.LookupEnv("DST_ENV_P1"); ok {
		t.Errorf("host has DST_ENV_P1 after run: a bubble Setenv leaked to the host")
	}

	if !p1ReadHostOK || p1ReadHost != hostVal {
		t.Errorf("p1 read of unmodified host var = (%q, %v), want (%q, true): the copy must initialize from host machine state", p1ReadHost, p1ReadHostOK, hostVal)
	}
	if p1AfterSet != "p1-value" {
		t.Errorf("p1 read of its own Setenv = %q, want p1-value", p1AfterSet)
	}
	if p2SeesP1Set {
		t.Errorf("p2 sees p1's Setenv (DST_ENV_P1): per-process env isolation is broken")
	}
	if p2ReadHost != hostVal {
		t.Errorf("p2 read of host var = %q, want %q: p1's Unsetenv must not affect p2", p2ReadHost, hostVal)
	}
}

// TestDSTEnvResetBetweenRuns checks that a process's environment view is
// re-initialized from the host each run: run 2 must not see run 1's writes.
func TestDSTEnvResetBetweenRuns(t *testing.T) {
	var run2SeesRun1 bool
	var run2ReadHostAfterClear string

	const hostKey = "DST_ENV_RESET_HOST"
	os.Setenv(hostKey, "present")
	defer os.Unsetenv(hostKey)

	Run(1, func() {
		Process("p", func() {
			os.Setenv("DST_ENV_RESET", "run1")
			os.Clearenv() // wipe p's whole view in run 1
		})
	})
	Run(1, func() {
		Process("p", func() {
			_, run2SeesRun1 = os.LookupEnv("DST_ENV_RESET") // run 1's write must be gone
			run2ReadHostAfterClear = os.Getenv(hostKey)     // run 1's Clearenv must not persist
		})
	})

	if run2SeesRun1 {
		t.Errorf("run 2 sees run 1's Setenv: per-run env reset is broken")
	}
	if run2ReadHostAfterClear != "present" {
		t.Errorf("run 2 read of host var = %q, want \"present\": run 1's Clearenv leaked across runs", run2ReadHostAfterClear)
	}
}

// TestDSTEnvironIsolated checks os.Environ under a run: it reflects the process's
// own copy (Setenv visible, Unsetenv removed), and the host Environ is unchanged
// afterward.
func TestDSTEnvironIsolated(t *testing.T) {
	const hostKey = "DST_ENVIRON_HOST"
	os.Setenv(hostKey, "h")
	defer os.Unsetenv(hostKey)

	hostBefore := os.Environ()

	var hasSet, hasHost bool
	Run(1, func() {
		Process("p", func() {
			os.Setenv("DST_ENVIRON_SET", "s")
			for _, kv := range os.Environ() {
				switch kv {
				case "DST_ENVIRON_SET=s":
					hasSet = true
				case hostKey + "=h":
					hasHost = true
				}
			}
		})
	})

	if !hasSet {
		t.Errorf("os.Environ in bubble missing the process's own DST_ENVIRON_SET")
	}
	if !hasHost {
		t.Errorf("os.Environ in bubble missing the inherited host var %s", hostKey)
	}
	// Host Environ unchanged: the bubble's Setenv is not in it.
	hostAfter := os.Environ()
	if slices.Contains(hostAfter, "DST_ENVIRON_SET=s") {
		t.Errorf("host Environ contains the bubble's DST_ENVIRON_SET: leaked to host")
	}
	if !slices.Equal(hostBefore, hostAfter) {
		t.Errorf("host Environ changed across a run:\n before=%v\n after=%v", hostBefore, hostAfter)
	}
}

// TestDSTEnvironOrderDeterministic checks that os.Environ order under a run is a
// deterministic function of the seed and host env — the same across two same-seed
// runs. A regression using map iteration for Environ output would flake this.
func TestDSTEnvironOrderDeterministic(t *testing.T) {
	os.Setenv("DST_ORDER_HOST", "h")
	defer os.Unsetenv("DST_ORDER_HOST")

	capture := func() []string {
		var env []string
		Run(7, func() {
			Process("p", func() {
				os.Setenv("DST_ORDER_A", "1")
				os.Setenv("DST_ORDER_B", "2")
				os.Setenv("DST_ORDER_HOST", "override")
				env = os.Environ()
			})
		})
		return env
	}
	if a, b := capture(), capture(); !slices.Equal(a, b) {
		t.Errorf("os.Environ order not deterministic across same-seed runs:\n run1=%v\n run2=%v", a, b)
	}
}

// TestDSTEnvOverrideInPlace checks that Setenv of an existing host key overrides
// in place (like real env_unix.go) — the key appears once in Environ, not
// duplicated/appended.
func TestDSTEnvOverrideInPlace(t *testing.T) {
	os.Setenv("DST_OVR", "orig")
	defer os.Unsetenv("DST_OVR")

	var val string
	var count int
	Run(1, func() {
		Process("p", func() {
			os.Setenv("DST_OVR", "new")
			val = os.Getenv("DST_OVR")
			for _, kv := range os.Environ() {
				if strings.HasPrefix(kv, "DST_OVR=") {
					count++
				}
			}
		})
	})

	if val != "new" {
		t.Errorf("override read = %q, want new", val)
	}
	if count != 1 {
		t.Errorf("DST_OVR appears %d times in Environ, want 1 (override is in place, not appended)", count)
	}
}

// TestDSTEnvClearThenRead checks a within-run read after Clearenv: the process's
// whole view (including inherited host vars) is wiped, and a subsequent Setenv is
// visible.
func TestDSTEnvClearThenRead(t *testing.T) {
	os.Setenv("DST_CLR", "present")
	defer os.Unsetenv("DST_CLR")

	var afterClear string
	var afterClearOK bool
	var newVisible string
	Run(1, func() {
		Process("p", func() {
			os.Clearenv()
			afterClear, afterClearOK = os.LookupEnv("DST_CLR") // host var now shadowed
			os.Setenv("DST_CLR_NEW", "n")
			newVisible = os.Getenv("DST_CLR_NEW")
		})
	})

	if afterClearOK {
		t.Errorf("host var visible after Clearenv = %q, want absent (Clearenv wipes the process view)", afterClear)
	}
	if newVisible != "n" {
		t.Errorf("Setenv after Clearenv = %q, want n", newVisible)
	}
}
