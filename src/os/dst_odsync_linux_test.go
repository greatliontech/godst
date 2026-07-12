// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

func TestDSTFODSyncCommitsDataWithoutMetadata(t *testing.T) {
	var durableMtime time.Time
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("p", func() {
				f, err := os.OpenFile("/f", os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString("old"); err != nil {
					t.Fatal(err)
				}
				if err := f.Sync(); err != nil {
					t.Fatal(err)
				}
				f.Close()
				root, _ := os.Open("/")
				if err := root.Sync(); err != nil {
					t.Fatal(err)
				}
				root.Close()
				fi, _ := os.Stat("/f")
				durableMtime = fi.ModTime()
				time.Sleep(time.Second)
				f, err = os.OpenFile("/f", os.O_WRONLY|syscall.O_DSYNC, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString("new-data"); err != nil {
					t.Fatal(err)
				}
				f.Close()
				fi, _ = os.Stat("/f")
				if !fi.ModTime().After(durableMtime) {
					t.Fatal("O_DSYNC write did not update current mtime")
				}
			})
		})
		simulation.CrashHost("h")
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("p", func() {
				data, err := os.ReadFile("/f")
				if err != nil || string(data) != "new-data" {
					t.Fatalf("recovered data = %q, %v", data, err)
				}
				fi, _ := os.Stat("/f")
				if !fi.ModTime().Equal(durableMtime) {
					t.Fatalf("recovered mtime = %v, want %v", fi.ModTime(), durableMtime)
				}
			})
		})
	})
}

func TestDSTFODSyncWriteAtAndRootedHandle(t *testing.T) {
	paths := map[string]string{"/at": "write-at", "/rooted": "rooted-data"}
	mtimes := map[string]time.Time{}
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("p", func() {
				for path := range paths {
					f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
					if err != nil {
						t.Fatal(err)
					}
					f.WriteString("old")
					f.Sync()
					f.Close()
					fi, _ := os.Stat(path)
					mtimes[path] = fi.ModTime()
				}
				rootDir, _ := os.Open("/")
				rootDir.Sync()
				rootDir.Close()
				time.Sleep(time.Second)
				f, _ := os.OpenFile("/at", os.O_WRONLY|syscall.O_DSYNC, 0)
				if _, err := f.WriteAt([]byte(paths["/at"]), 0); err != nil {
					t.Fatal(err)
				}
				f.Close()
				root, _ := os.OpenRoot("/")
				f, err := root.OpenFile("rooted", os.O_WRONLY|syscall.O_DSYNC, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString(paths["/rooted"]); err != nil {
					t.Fatal(err)
				}
				f.Close()
				root.Close()
			})
		})
		simulation.CrashHost("h")
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("p", func() {
				for path, want := range paths {
					data, err := os.ReadFile(path)
					if err != nil || string(data) != want {
						t.Fatalf("%s data = %q, %v", path, data, err)
					}
					fi, _ := os.Stat(path)
					if !fi.ModTime().Equal(mtimes[path]) {
						t.Fatalf("%s mtime = %v, want %v", path, fi.ModTime(), mtimes[path])
					}
				}
			})
		})
	})
}

func TestDSTFODSyncWriteCommitThreshold(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("p", func() {
				f, _ := os.OpenFile("/zero", os.O_CREATE|os.O_RDWR, 0o644)
				f.Sync()
				f.WriteString("pending")
				f.Close()
				f, _ = os.OpenFile("/zero", os.O_WRONLY|syscall.O_DSYNC, 0)
				if n, err := f.Write(nil); n != 0 || err != nil {
					t.Fatalf("zero write = %d, %v", n, err)
				}
				f.Close()
				_, synced, _, _, _, _, _ := os.DSTFSNodeState("/zero")
				if synced != "" {
					t.Fatalf("zero write durable data = %q", synced)
				}
				if err := os.Remove("/zero"); err != nil {
					t.Fatal(err)
				}
				f, _ = os.OpenFile("/refused", os.O_CREATE|os.O_RDWR, 0o644)
				f.Sync()
				f.WriteString("pending")
				f.Close()

				simulation.LimitDisk("h", 10)
				f, _ = os.OpenFile("/partial", os.O_CREATE|os.O_RDWR|syscall.O_DSYNC, 0o644)
				n, err := f.Write([]byte("abcde"))
				if n != 3 || !errors.Is(err, syscall.ENOSPC) {
					t.Fatalf("partial write = %d, %v", n, err)
				}
				f.Close()
				_, synced, _, _, _, _, _ = os.DSTFSNodeState("/partial")
				if synced != "abc" {
					t.Fatalf("partial durable data = %q", synced)
				}
				f, _ = os.OpenFile("/refused", os.O_WRONLY|os.O_APPEND|syscall.O_DSYNC, 0)
				n, err = f.Write([]byte("x"))
				if n != 0 || !errors.Is(err, syscall.ENOSPC) {
					t.Fatalf("refused write = %d, %v", n, err)
				}
				f.Close()
				_, synced, _, _, _, _, _ = os.DSTFSNodeState("/refused")
				if synced != "" {
					t.Fatalf("refused durable data = %q", synced)
				}
			})
		})
	})
}
