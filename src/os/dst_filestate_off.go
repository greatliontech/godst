// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package os

// Without the simulated backing (untagged builds) the constant nil folds the
// dstSimEnabled-guarded gates to dead code. A free function, deliberately not
// a method — see dst_filestate.go (design.md, INV-TYPESHAPE).
func dstBackendOf(f *file) dstFileBackend {
	return nil
}

func dstSysfd(f *file) int {
	return -1
}
