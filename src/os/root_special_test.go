// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package os_test

import (
	"os"
	"testing"
)

// rootRefusal asserts err is the exact refusal shape Root's create
// operations document: a *PathError with the given Op and Path wrapping
// "unsupported file mode".
func rootRefusal(t *testing.T, what string, err error, op, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s succeeded, want refusal", what)
	}
	pe, ok := err.(*os.PathError)
	if !ok || pe.Op != op || pe.Path != path || pe.Err.Error() != "unsupported file mode" {
		t.Fatalf("%s = %v, want &PathError{Op: %q, Path: %q, Err: \"unsupported file mode\"}", what, err, op, path)
	}
}

// Every rooted create surface documents that perm may contain only the nine
// least-significant bits: any other mode bit — setuid/setgid/sticky
// included — is refused before dispatch, even though the named creators
// (os.OpenFile, os.Mkdir) accept and preserve those bits. The refusal
// happens in portable validation, so this exact shape is the contract on
// every platform.
func TestRootCreateRejectsSpecialModeBits(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, bit := range []os.FileMode{os.ModeSetuid, os.ModeSetgid, os.ModeSticky} {
		f, err := root.OpenFile("f", os.O_CREATE|os.O_RDWR, 0o600|bit)
		if err == nil {
			f.Close()
		}
		rootRefusal(t, "OpenFile with "+bit.String(), err, "openat", "f")
		rootRefusal(t, "WriteFile with "+bit.String(), root.WriteFile("w", []byte("x"), 0o600|bit), "openat", "w")
		rootRefusal(t, "Mkdir with "+bit.String(), root.Mkdir("d", 0o700|bit), "mkdirat", "d")
		rootRefusal(t, "MkdirAll with "+bit.String(), root.MkdirAll("d/e", 0o700|bit), "mkdirat", "d/e")
	}
}
