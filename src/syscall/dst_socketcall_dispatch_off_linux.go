// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst && linux && (386 || s390x)

package syscall

// Stock build: the socketcall fence folds away (dstSimFenced is false); this
// stub exists only so the dead branch still type-checks.
func dstSocketcallSockopt(call int, a0, a1, a2, a3, a4 uintptr) (err Errno, handled bool) {
	return 0, false
}
