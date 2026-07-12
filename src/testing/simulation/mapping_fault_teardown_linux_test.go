// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"errors"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
)

var mappingFaultTeardownSink byte

func TestDSTMappingFaultRunsCompleteProcessTeardown(t *testing.T) {
	t.Setenv("DST_MAPPING_FAULT_ENV", "host")
	const page = 4096
	var leakedRoot *os.Root
	var leakedFile *os.File
	var leakedConn net.Conn
	var leakedMapping []byte
	Test(t, 1, func(t *testing.T) {
		Host("h", HostConfig{}, func() {
			ready := make(chan int, 1)
			go Process("worker", func() {
				os.Mkdir("/old", 0o755)
				os.Chdir("/old")
				os.Setenv("DST_MAPPING_FAULT_ENV", "mutated")
				f, err := os.OpenFile("/db", os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				f.Truncate(page)
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					t.Fatal(err)
				}
				leakedFile = f
				m, err := syscall.Mmap(int(f.Fd()), 0, 2*page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Fatal(err)
				}
				leakedMapping = m
				leakedRoot, err = os.OpenRoot("/")
				if err != nil {
					t.Fatal(err)
				}
				ln, err := net.Listen("tcp", ":23001")
				if err != nil {
					t.Fatal(err)
				}
				connReady := make(chan net.Conn, 1)
				go func() {
					c, err := net.Dial("tcp", "127.0.0.1:23001")
					if err != nil {
						t.Error(err)
						return
					}
					connReady <- c
				}()
				server, err := ln.Accept()
				if err != nil {
					t.Fatal(err)
				}
				_ = server
				leakedConn = <-connReady
				ready <- os.Getpid()
				mappingFaultTeardownSink = m[page] // SIGBUS: mapped wholly beyond EOF.
			})
			pid := <-ready
			for syscall.Kill(pid, 0) == nil {
				runtime.Gosched()
			}
			if _, err := leakedFile.Write(nil); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("file after mapping fault = %v, want ErrClosed", err)
			}
			if _, err := leakedConn.Write([]byte("x")); err == nil {
				t.Fatal("connection remained usable after mapping fault")
			}
			if err := syscall.Munmap(leakedMapping); !errors.Is(err, syscall.EINVAL) {
				t.Fatalf("old mapping remained registered: Munmap = %v, want EINVAL", err)
			}
			if _, err := leakedRoot.Stat("."); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("Root after mapping fault = %v, want ErrClosed", err)
			}
			proc := lookupProc("worker")
			runs := mappingFaultTeardownRuns
			mappingFaultProcessTeardown(proc)
			if mappingFaultTeardownRuns != runs {
				t.Fatalf("duplicate mapping-fault teardown ran twice: %d -> %d", runs, mappingFaultTeardownRuns)
			}
			Process("worker", func() {
				if cwd, _ := os.Getwd(); cwd != "/" {
					t.Fatalf("restart cwd = %q", cwd)
				}
				if got := os.Getenv("DST_MAPPING_FAULT_ENV"); got != "host" {
					t.Fatalf("restart env = %q", got)
				}
				f, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatalf("restart flock: %v", err)
				}
				m, err := syscall.Mmap(int(f.Fd()), 0, page, syscall.PROT_READ, syscall.MAP_SHARED)
				if err != nil {
					t.Fatalf("restart mmap: %v", err)
				}
				syscall.Munmap(m)
				ln, err := net.Listen("tcp", ":23001")
				if err != nil {
					t.Fatalf("restart listen: %v", err)
				}
				ln.Close()
			})
		})
	})
}
