// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst && linux && (mips64 || mips64le)

package syscall

func Fstat(fd int, s *Stat_t) (err error) {
	st := &stat_t{}
	err = fstat(fd, st)
	fillStat_t(s, st)
	return
}
