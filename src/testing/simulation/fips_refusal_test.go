// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"fmt"
	"internal/testenv"
	"os"
	"strings"
	"testing"
)

// TestDSTFIPSModeRefused: a process whose GODEBUG latches fips140=on at
// startup has Run refused loudly (enterSimulation's fips140Mode latch). This
// refusal is load-bearing beyond FIPS itself: the FIPS jitter-entropy path is
// the only reachable caller of the runtime's bubble-bypassing monotonic-clock
// read (crypto/internal/fips140deps/time), so admitting FIPS mode would admit a
// real-host clock read into the seeded schedule. The check runs in a child
// process of this test binary so the GODEBUG is read at init.
func TestDSTFIPSModeRefused(t *testing.T) {
	if os.Getenv("DST_FIPS_REFUSAL_CHILD") == "1" {
		defer func() {
			r := recover()
			fmt.Printf("refused=%v\n", r != nil && strings.Contains(fmt.Sprint(r), "unsupported in FIPS 140 mode"))
			os.Exit(0)
		}()
		Run(1, func() {})
		fmt.Println("refused=false")
		os.Exit(0)
	}
	testenv.MustHaveExec(t)
	cmd := testenv.CleanCmdEnv(testenv.Command(t, os.Args[0], "-test.run=^TestDSTFIPSModeRefused$", "-test.v"))
	cmd.Env = append(cmd.Env, "DST_FIPS_REFUSAL_CHILD=1", "GODEBUG=fips140=on")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "refused=true") {
		t.Fatalf("fips140=on child did not refuse Run:\n%s", out)
	}
}
