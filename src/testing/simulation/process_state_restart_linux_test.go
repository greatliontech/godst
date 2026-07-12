// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"os"
	"testing"
)

func TestDSTProcessRestartResetsCWDAndEnvironment(t *testing.T) {
	t.Setenv("DST_RESTART_BASE", "base")
	t.Setenv("DST_RESTART_REMOVE", "host")
	for _, crash := range []bool{false, true} {
		death := "exit"
		if crash {
			death = "crash"
		}
		for _, mutation := range []string{"set", "unset", "clear"} {
			t.Run(death+"/"+mutation, func(t *testing.T) {
				Test(t, 1, func(t *testing.T) {
					Host("h", HostConfig{}, func() {
						first := func() {
							os.Mkdir("/old", 0o755)
							if err := os.Chdir("/old"); err != nil {
								t.Fatal(err)
							}
							switch mutation {
							case "set":
								os.Setenv("DST_RESTART_BASE", "mutated")
							case "unset":
								os.Unsetenv("DST_RESTART_REMOVE")
							case "clear":
								os.Clearenv()
							}
						}
						if crash {
							ready := make(chan struct{})
							go Process("worker", func() {
								first()
								close(ready)
								select {}
							})
							<-ready
							crashProcess("worker")
						} else {
							Process("worker", first)
						}
						Process("worker", func() {
							if cwd, err := os.Getwd(); err != nil || cwd != "/" {
								t.Fatalf("restart cwd = %q, %v; want /", cwd, err)
							}
							if got := os.Getenv("DST_RESTART_BASE"); got != "base" {
								t.Fatalf("restart base env = %q, want base", got)
							}
							if got := os.Getenv("DST_RESTART_REMOVE"); got != "host" {
								t.Fatalf("restart removed env = %q, want host", got)
							}
							if err := os.WriteFile("restart-file", []byte("ok"), 0o644); err != nil {
								t.Fatal(err)
							}
							if data, err := os.ReadFile("/restart-file"); err != nil || string(data) != "ok" {
								t.Fatalf("restart relative file = %q, %v", data, err)
							}
						})
					})
				})
			})
		}
	}
}

func TestDSTHostCrashRestartResetsProcessState(t *testing.T) {
	t.Setenv("DST_RESTART_BASE", "base")
	t.Setenv("DST_RESTART_REMOVE", "host")
	for _, mutation := range []string{"set", "unset", "clear"} {
		t.Run(mutation, func(t *testing.T) {
			Test(t, 1, func(t *testing.T) {
				ready := make(chan struct{})
				Host("h", HostConfig{}, func() {
					go Process("worker", func() {
						os.Mkdir("/old", 0o755)
						os.Chdir("/old")
						switch mutation {
						case "set":
							os.Setenv("DST_RESTART_BASE", "mutated")
						case "unset":
							os.Unsetenv("DST_RESTART_REMOVE")
						case "clear":
							os.Clearenv()
						}
						close(ready)
						select {}
					})
					<-ready
				})
				CrashHost("h")
				Host("h", HostConfig{}, func() {
					Process("worker", func() {
						if cwd, _ := os.Getwd(); cwd != "/" {
							t.Fatalf("restart cwd = %q, want /", cwd)
						}
						if os.Getenv("DST_RESTART_BASE") != "base" || os.Getenv("DST_RESTART_REMOVE") != "host" {
							t.Fatalf("restart environment did not restore host baseline")
						}
					})
				})
			})
		})
	}
}
