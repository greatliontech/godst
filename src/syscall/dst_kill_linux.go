// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package syscall

func Kill(pid int, sig Signal) (err error) {
	if e1, handled := dstTryKill(pid, sig); handled {
		if e1 != 0 {
			err = errnoErr(e1)
		}
		return err
	}
	return kill(pid, sig)
}
