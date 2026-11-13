// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && loong64

package syscall

func fstatFD(fd int, stat *Stat_t) (err error) {
	return fstatat(fd, "", stat, _AT_EMPTY_PATH)
}
