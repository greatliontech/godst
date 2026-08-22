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
)

// TestDSTRootMkdirConformsToHost runs one table of Root.Mkdir/Root.MkdirAll
// calls against the Linux host (outside any simulation) and against the
// simulated filesystem, and requires the same errno class for every row: the
// host is the oracle, nothing is hardcoded. The rows cover the terminal
// component's shapes — trailing slashes, dots, an existing directory, an
// existing regular file, a deep create, escapes — because that is where
// mkdirat (the final component) and openat (its ancestors) answer differently:
// an existing non-directory is EEXIST as the target and ENOTDIR as an
// ancestor, trailing slash or not — and the PathError names that component
// (Op openat or mkdirat, Path its cleaned root-relative path), which the
// comparison includes.
// Symlinks are outside the modeled filesystem and absent from the table.
func TestDSTRootMkdirConformsToHost(t *testing.T) {
	type call struct {
		all  bool
		name string
	}
	table := []call{
		{false, ""}, {true, ""}, {false, "."}, {true, "."},
		{false, "dir/"}, {true, "dir/"}, {false, "file/"}, {true, "file/"},
		{false, "file"}, {true, "file"}, {false, "dir"}, {true, "dir"},
		{true, "new1/"}, {true, "new2/a/b/"}, {true, "file/a/b"}, {false, "file/a"},
		{false, "/"}, {true, "/"}, {true, "dir/../new3/"}, {true, "../esc"}, {false, "../esc"},
		{false, "new4/."}, {true, "new5/./"}, {true, "dir/./sub/"}, {false, "dir/sub2/"},
		// Depth ≥ 2 and normalization: the error names the failing
		// component's CLEANED root-relative path, not its basename or the
		// name as given.
		{true, "dir/deep/file2"}, {true, "dir/deep/file2/"}, {true, "dir/deep/file2/x"},
		{true, "dir/./deep//file2"}, {true, "dir/new6/../deep/file2"}, {true, "dir/deep/../../file"},
		{true, "./dir/deep/file2/x/y"}, {false, "dir/deep/file2/x"},
	}
	// errClass names what the differential compares: the PathError's Op and
	// Path and the errno (or sentinel) behind it — the whole observable
	// shape short of the formatted text.
	errClass := func(err error) string {
		if err == nil {
			return "nil"
		}
		class := err.Error()
		var errno syscall.Errno
		if errors.As(err, &errno) {
			class = errno.Error()
		} else if errors.Is(err, os.ErrPathEscapes) {
			class = "escapes"
		}
		var pe *os.PathError
		if errors.As(err, &pe) {
			return pe.Op + " " + pe.Path + ": " + class
		}
		return class
	}
	// Fixture failures panic rather than t.Fatal: inside simulation.Run a
	// Goexit would unwind the bubble and leave the sim column unassigned.
	run := func() []string {
		base, err := os.MkdirTemp("", "rootmkdir")
		if err != nil {
			panic(err)
		}
		defer os.RemoveAll(base)
		if err := os.WriteFile(base+"/file", []byte("x"), 0o644); err != nil {
			panic(err)
		}
		if err := os.MkdirAll(base+"/dir/deep", 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(base+"/dir/deep/file2", []byte("y"), 0o644); err != nil {
			panic(err)
		}
		r, err := os.OpenRoot(base)
		if err != nil {
			panic(err)
		}
		defer r.Close()
		got := make([]string, len(table))
		for i, c := range table {
			var err error
			if c.all {
				err = r.MkdirAll(c.name, 0o755)
			} else {
				err = r.Mkdir(c.name, 0o755)
			}
			got[i] = errClass(err)
		}
		return got
	}
	host := run()
	var sim []string
	simulation.Run(1, func() { sim = run() })
	for i, c := range table {
		op := "Mkdir"
		if c.all {
			op = "MkdirAll"
		}
		if host[i] != sim[i] {
			t.Errorf("Root.%s(%q): host %s, sim %s", op, c.name, host[i], sim[i])
		}
	}
}
