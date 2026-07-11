// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package syscall_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDSTSocketcallEntriesPreservePointerArguments(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips cross-architecture objdump checks")
	}
	testenv.MustHaveGoBuild(t)

	dir := t.TempDir()
	mainSrc := []byte(`package main

import "syscall"

var result [2]int

func main() {
	result, _ = syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM, 0)
	_ = syscall.Bind(-1, &syscall.SockaddrInet4{Port: 1})
	exerciseExternalEntries()
}
`)

	for _, arch := range []string{"386", "s390x"} {
		t.Run(arch, func(t *testing.T) {
			archDir := filepath.Join(dir, arch)
			if err := os.Mkdir(archDir, 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(archDir, "main.go"), mainSrc, 0o666); err != nil {
				t.Fatal(err)
			}
			if arch == "386" {
				const externalGo = `package main

import "syscall"

func externalSocketcall(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err syscall.Errno)
func externalRawsocketcall(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err syscall.Errno)

func exerciseExternalEntries() {
	_, _ = externalSocketcall(1, uintptr(syscall.AF_INET), uintptr(syscall.SOCK_STREAM), 0, 0, 0, 0)
	_, _ = externalRawsocketcall(1, uintptr(syscall.AF_INET), uintptr(syscall.SOCK_STREAM), 0, 0, 0, 0)
}
`
				const externalAsm = `#include "textflag.h"

TEXT ·externalSocketcall(SB),NOSPLIT,$0-36
	JMP syscall·socketcall(SB)

TEXT ·externalRawsocketcall(SB),NOSPLIT,$0-36
	JMP syscall·rawsocketcall(SB)
`
				if err := os.WriteFile(filepath.Join(archDir, "external.go"), []byte(externalGo), 0o666); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(archDir, "external.s"), []byte(externalAsm), 0o666); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(archDir, "external.go"), []byte("package main\n\nfunc exerciseExternalEntries() {}\n"), 0o666); err != nil {
				t.Fatal(err)
			}

			exe := filepath.Join(archDir, "probe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-tags=dst", "-o", exe, ".")
			cmd.Dir = archDir
			cmd = testenv.CleanCmdEnv(cmd)
			cmd.Env = append(cmd.Env, "GO111MODULE=off", "GOFLAGS=", "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("building linux/%s probe: %v\n%s", arch, err, out)
			}

			cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", `^syscall\.(raw)?socketcall$`, exe)
			cmd = testenv.CleanCmdEnv(cmd)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("objdumping linux/%s socketcall entries: %v\n%s", arch, err, out)
			}
			text := string(out)
			for _, symbol := range []string{"syscall.socketcall", "syscall.rawsocketcall"} {
				if !strings.Contains(text, "TEXT "+symbol+"(SB)") {
					t.Errorf("linux/%s probe has no %s symbol; structural check is vacuous\n%s", arch, symbol, out)
				}
			}
			if strings.Contains(text, "runtime.morestack") {
				t.Errorf("linux/%s socketcall entry can grow and move pointer-derived uintptr arguments:\n%s", arch, out)
			}
		})
	}
}
