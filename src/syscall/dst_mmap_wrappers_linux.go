// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package syscall

func Madvise(b []byte, advice int) (err error) {
	if e1, handled := dstTryMadvise(b, advice); handled {
		if e1 != 0 {
			err = errnoErr(e1)
		}
		return err
	}
	return madvise(b, advice)
}

func Mprotect(b []byte, prot int) (err error) {
	if e1, handled := dstTryMprotect(b, prot); handled {
		if e1 != 0 {
			err = errnoErr(e1)
		}
		return err
	}
	return mprotect(b, prot)
}
