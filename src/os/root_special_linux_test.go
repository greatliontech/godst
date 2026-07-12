// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package os_test

import (
	"os"
	"testing"
)

func TestRootOpenFileSpecialModeBits(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	const special = os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	f, err := root.OpenFile("special", os.O_CREATE|os.O_RDWR, 0o600|special)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, err := root.Stat("special")
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode() & special; got != special {
		t.Fatalf("special mode bits = %v, want %v", got, special)
	}
}
