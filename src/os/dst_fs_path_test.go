// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os_test

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// TestDSTFSPhysicalPathWalk is the M10 regression: path resolution walks
// component-wise like the kernel, so `..` is evaluated against the tree (never erased
// lexically first) and every intermediate must exist and be a directory. A lexical
// path.Clean would turn these path bugs into sim-only successes a real kernel rejects.
func TestDSTFSPhysicalPathWalk(t *testing.T) {
	simulation.Run(1, func() {
		// Set up: /dir (a directory), /file (a regular file).
		if err := os.Mkdir("/dir", 0o755); err != nil {
			t.Fatalf("Mkdir /dir: %v", err)
		}
		if err := os.WriteFile("/file", []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile /file: %v", err)
		}
		if err := os.WriteFile("/dir/inner", []byte("y"), 0o644); err != nil {
			t.Fatalf("WriteFile /dir/inner: %v", err)
		}

		cases := []struct {
			name string
			path string
			want error // nil = must succeed
		}{
			// `..` through a MISSING intermediate: ENOENT (the walk reaches "missing"
			// first), not a lexical collapse to /file.
			{"dotdot-through-missing", "/missing/../file", syscall.ENOENT},
			// `..` through a FILE intermediate: ENOTDIR.
			{"dotdot-through-file", "/file/../dir", syscall.ENOTDIR},
			// A regular file as an intermediate component: ENOTDIR.
			{"file-as-dir", "/file/inner", syscall.ENOTDIR},
			// Trailing slash on a regular file: ENOTDIR.
			{"trailing-slash-file", "/file/", syscall.ENOTDIR},
			// `..` resolved physically that DOES exist: succeeds (/dir/../file → /file).
			{"dotdot-valid", "/dir/../file", nil},
			// `..` at the root stays at the root: /../file → /file.
			{"dotdot-at-root", "/../file", nil},
			// Trailing slash on a directory is fine.
			{"trailing-slash-dir", "/dir/", nil},
		}
		for _, tc := range cases {
			_, err := os.Open(tc.path)
			if tc.want == nil {
				if err != nil {
					t.Errorf("%s: Open(%q) = %v, want success", tc.name, tc.path, err)
				}
				continue
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: Open(%q) err = %v, want %v (lexical path.Clean would hide this)", tc.name, tc.path, err, tc.want)
			}
		}
	})
}

// TestDSTFSOpenTruncDirEISDIR is the M11 regression: O_TRUNC on a directory is EISDIR
// regardless of access mode, and must not mutate the directory (not even its mtime) —
// an open real Linux rejects before any state change.
func TestDSTFSOpenTruncDirEISDIR(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/d", 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		before, err := os.Stat("/d")
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		// Advance the fake clock so a buggy truncate that bumped the dir mtime would
		// stamp a DIFFERENT value than the Mkdir did — otherwise bubble.now is frozen
		// between the two Stats and the mtime check is vacuous.
		time.Sleep(time.Second)
		// O_RDONLY|O_TRUNC on a directory: EISDIR (the access-mode-independent leg).
		_, err = os.OpenFile("/d", os.O_RDONLY|os.O_TRUNC, 0)
		if !errors.Is(err, syscall.EISDIR) {
			t.Fatalf("OpenFile(dir, O_RDONLY|O_TRUNC) = %v, want EISDIR", err)
		}
		after, err := os.Stat("/d")
		if err != nil {
			t.Fatalf("Stat after: %v", err)
		}
		if !after.ModTime().Equal(before.ModTime()) {
			t.Errorf("rejected O_TRUNC bumped the directory mtime: %v -> %v (must mutate nothing)", before.ModTime(), after.ModTime())
		}
	})
}

// TestDSTFSRemoveNonEmptyENOTEMPTY: os.Remove of a non-empty directory surfaces
// rmdir's ENOTEMPTY (not EINVAL, which is reserved for ".").
func TestDSTFSRemoveNonEmptyENOTEMPTY(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/full", 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := os.WriteFile("/full/child", []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Remove("/full"); !errors.Is(err, syscall.ENOTEMPTY) {
			t.Fatalf("Remove non-empty dir = %v, want ENOTEMPTY", err)
		}
	})
}

// TestDSTFSCreateTrailingSlashEISDIR: O_CREAT through a trailing slash cannot mint a
// regular file — real Linux returns EISDIR (a trailing slash asserts a directory).
func TestDSTFSCreateTrailingSlashEISDIR(t *testing.T) {
	simulation.Run(1, func() {
		_, err := os.OpenFile("/newthing/", os.O_CREATE|os.O_WRONLY, 0o644)
		if !errors.Is(err, syscall.EISDIR) {
			t.Fatalf("OpenFile(%q, O_CREATE) = %v, want EISDIR", "/newthing/", err)
		}
		if _, statErr := os.Stat("/newthing"); statErr == nil {
			t.Fatalf("a file was created despite the trailing slash")
		}
	})
}
