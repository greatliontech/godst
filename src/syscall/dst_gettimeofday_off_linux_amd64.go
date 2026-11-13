// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst && linux && amd64

package syscall

import _ "unsafe"

// Accessed via assembly in x/sys/unix.
//
//go:noescape
//go:linkname gettimeofday
func gettimeofday(tv *Timeval) (err Errno)
