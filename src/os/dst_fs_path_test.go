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

func TestDSTFSTerminalDotErrorPrecedence(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/dir", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("/file", []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("/dir/source", []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			name string
			run  func() error
			want error
		}{
			{"remove-missing-dot", func() error { return os.Remove("/missing/.") }, syscall.ENOENT},
			{"remove-missing-dotdot", func() error { return os.Remove("/missing/..") }, syscall.ENOENT},
			{"remove-file-dot", func() error { return os.Remove("/file/.") }, syscall.ENOTDIR},
			{"remove-file-dotdot", func() error { return os.Remove("/file/..") }, syscall.ENOTDIR},
			{"removeall-missing-dot", func() error { return os.RemoveAll("/missing/.") }, nil},
			{"removeall-missing-dotdot", func() error { return os.RemoveAll("/missing/..") }, nil},
			{"removeall-file-dot", func() error { return os.RemoveAll("/file/.") }, syscall.ENOTDIR},
			{"removeall-file-dotdot", func() error { return os.RemoveAll("/file/..") }, syscall.ENOTDIR},
			{"rename-old-missing-dot", func() error { return os.Rename("/missing/.", "/target") }, syscall.ENOENT},
			{"rename-old-missing-dotdot", func() error { return os.Rename("/missing/..", "/target") }, syscall.ENOENT},
			{"rename-old-file-dot", func() error { return os.Rename("/file/.", "/target") }, syscall.ENOTDIR},
			{"rename-old-file-dotdot", func() error { return os.Rename("/file/..", "/target") }, syscall.ENOTDIR},
			{"rename-new-missing-dot", func() error { return os.Rename("/dir/source", "/missing/.") }, syscall.ENOENT},
			{"rename-new-missing-dotdot", func() error { return os.Rename("/dir/source", "/missing/..") }, syscall.ENOENT},
			{"rename-new-file-dot", func() error { return os.Rename("/dir/source", "/file/.") }, syscall.ENOTDIR},
			{"rename-new-file-dotdot", func() error { return os.Rename("/dir/source", "/file/..") }, syscall.ENOTDIR},
			// rename(2) resolves BOTH parent walks before the terminal-dot
			// EBUSY and old-final existence checks (host-probed: a new-path
			// walk error beats the old terminal's EBUSY, EBUSY beats the
			// missing old final, and the missing old final beats the
			// new-final trailing-slash assertion).
			{"rename-new-walk-precedes-old-terminal", func() error { return os.Rename("/dir/.", "/missing/..") }, syscall.ENOENT},
			{"rename-new-walk-notdir-precedes-old-terminal", func() error { return os.Rename("/dir/.", "/file/sub") }, syscall.ENOTDIR},
			{"rename-new-walk-precedes-old-missing", func() error { return os.Rename("/missing", "/file/sub") }, syscall.ENOTDIR},
			// os.Rename's portable preamble (file_unix.go rename) Lstats an
			// existing-directory newname and returns the OLDNAME error before
			// any rename reaches the tree — raw rename(2) would say EBUSY
			// here, but the Go surface says ENOENT on host and sim alike.
			{"rename-existing-dir-newname-reports-oldname-error", func() error { return os.Rename("/missing", "/dir/.") }, syscall.ENOENT},
			{"rename-old-missing-precedes-new-trailing-slash", func() error { return os.Rename("/missing", "/newname/") }, syscall.ENOENT},
			{"rename-old-terminal-when-both-walks-clean", func() error { return os.Rename("/dir/.", "/target") }, syscall.EBUSY},
			// Terminal-dot EBUSY precedes the trailing-slash source check:
			// a directory source would make "/newname2/" legal, but the
			// "." terminal refuses first (host-probed).
			{"rename-old-terminal-precedes-new-trailing-slash", func() error { return os.Rename("/dir/.", "/newname2/") }, syscall.EBUSY},
			// Old trailing slash on a file with an existing-dir newname:
			// the os.Rename preamble Lstats the newname (a directory) and
			// reports the OLDNAME error — raw rename(2) would say EBUSY.
			{"rename-preamble-reports-oldname-slash-error", func() error { return os.Rename("/file/", "/dir/.") }, syscall.ENOTDIR},
		}
		for _, tc := range cases {
			err := tc.run()
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: error = %v, want %v", tc.name, err, tc.want)
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

// TestDSTFSTrailingSlashCreateLegs pins the host-probed errno identities of
// the trailing-slash CREATE legs, where the positive-dentry / slash-assertion
// ordering differs per op and per surface:
//   - mkdir(2) and mkdirat(2) report a positive final dentry EEXIST before
//     the slash's directory assertion (filename_create looks the dentry up
//     first) — never ENOTDIR;
//   - the rooted create through a slash-asserted MISSING component is
//     ENOENT (openat2's resolver), unlike the plain open(2)'s EISDIR
//     (TestDSTFSCreateTrailingSlashEISDIR pins that arm).
func TestDSTFSTrailingSlashCreateLegs(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/f", []byte("x"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := os.Mkdir("/f/", 0o755); !errors.Is(err, syscall.EEXIST) {
			t.Fatalf(`Mkdir("/f/") = %v, want EEXIST (the positive dentry answers before the slash assertion)`, err)
		}
		root, err := os.OpenRoot("/")
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer root.Close()
		if err := root.Mkdir("f/", 0o755); !errors.Is(err, syscall.EEXIST) {
			t.Fatalf(`Root.Mkdir("f/") = %v, want EEXIST (mkdirat's positive dentry answers first)`, err)
		}
		if _, err := root.OpenFile("missing/", os.O_CREATE|os.O_WRONLY, 0o644); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf(`Root.OpenFile("missing/", O_CREATE|O_WRONLY) = %v, want ENOENT (openat2 rejects the slash-asserted missing component)`, err)
		}
		if _, statErr := os.Stat("/missing"); statErr == nil {
			t.Fatalf("a file was created despite the trailing slash")
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

// TestDSTFSRenameTrailingSlash pins rename(2)'s trailing-slash rule at the
// os.Rename surface: trailing slashes on either path's final component are
// checked against the SOURCE's type after the old final's existence check
// ("unless the source is a directory trailing slashes give -ENOTDIR"), so a
// DIRECTORY source renames onto a trailing-slash missing newpath while a
// file source is refused. Every row is host-probed (ext4 and tmpfs agree);
// rows an os.Rename preamble intercepts are marked.
func TestDSTFSRenameTrailingSlash(t *testing.T) {
	simulation.Run(1, func() {
		mk := func() {
			os.RemoveAll("/fx")
			if err := os.MkdirAll("/fx/d", 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			for _, f := range []string{"/fx/d/sub", "/fx/f", "/fx/existf"} {
				if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}
			if err := os.Mkdir("/fx/empty", 0o755); err != nil {
				t.Fatalf("Mkdir: %v", err)
			}
			if err := os.Mkdir("/fx/nonempty", 0o755); err != nil {
				t.Fatalf("Mkdir: %v", err)
			}
			if err := os.WriteFile("/fx/nonempty/kid", []byte("k"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
		}
		cases := []struct {
			name     string
			old, new string
			want     error // nil = must succeed
		}{
			{"dir-to-missing-slash", "/fx/d", "/fx/miss/", nil},
			{"dir-slash-to-missing-slash", "/fx/d/", "/fx/miss/", nil},
			{"dir-slash-to-missing", "/fx/d/", "/fx/miss", nil},
			{"file-to-missing-slash", "/fx/f", "/fx/miss/", syscall.ENOTDIR},
			{"file-slash-to-missing", "/fx/f/", "/fx/miss", syscall.ENOTDIR},
			{"missing-to-missing-slash", "/fx/miss", "/fx/x/", syscall.ENOENT},
			{"missing-slash-to-missing", "/fx/miss/", "/fx/x", syscall.ENOENT},
			{"dir-to-existing-file-slash", "/fx/d", "/fx/existf/", syscall.ENOTDIR},
			{"file-to-existing-file-slash", "/fx/f", "/fx/existf/", syscall.ENOTDIR},
			// Old-final ENOENT precedes the trailing-slash source check,
			// even when the slash names an existing regular file.
			{"missing-to-existing-file-slash", "/fx/miss", "/fx/existf/", syscall.ENOENT},
			// Self-renames: the slash rule still keys on the source type.
			{"file-to-self-slash", "/fx/f", "/fx/f/", syscall.ENOTDIR},
			{"dir-to-self-slash", "/fx/d", "/fx/d/", nil}, // preamble SameFile fall-through; no-op
			// EINVAL (new inside old) is reachable for a dir source: the
			// slash rule does not apply and the containment check fires.
			{"dir-into-own-subtree-slash", "/fx/d", "/fx/d/inner/", syscall.EINVAL},
			// Existing-DIRECTORY newnames never reach rename(2) at this
			// surface: the os.Rename preamble Lstats them and answers
			// EEXIST (raw rename(2) would replace an empty directory).
			{"dir-to-empty-dir-slash", "/fx/d", "/fx/empty/", syscall.EEXIST},
			{"file-to-empty-dir-slash", "/fx/f", "/fx/empty/", syscall.EEXIST},
			{"dir-to-nonempty-dir-slash", "/fx/d", "/fx/nonempty/", syscall.EEXIST},
		}
		for _, tc := range cases {
			mk()
			err := os.Rename(tc.old, tc.new)
			if tc.want == nil {
				if err != nil {
					t.Errorf("%s: Rename(%q, %q) = %v, want success", tc.name, tc.old, tc.new, err)
				}
				continue
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: Rename(%q, %q) = %v, want %v", tc.name, tc.old, tc.new, err, tc.want)
			}
		}
		// The dir-source success actually moved the tree, and the refused
		// file-source rename minted nothing.
		mk()
		if err := os.Rename("/fx/d", "/fx/moved/"); err != nil {
			t.Fatalf(`Rename("/fx/d", "/fx/moved/") = %v, want success (dir source)`, err)
		}
		if got, err := os.ReadFile("/fx/moved/sub"); err != nil || string(got) != "x" {
			t.Fatalf("moved dir content = %q, %v; want intact", got, err)
		}
		if _, err := os.Stat("/fx/d"); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("source dir still present after rename: %v", err)
		}
		if err := os.Rename("/fx/f", "/fx/new/"); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf(`Rename("/fx/f", "/fx/new/") = %v, want ENOTDIR (file source)`, err)
		}
		if _, err := os.Stat("/fx/new"); err == nil {
			t.Fatalf("a file was created despite the trailing slash")
		}
	})
}
