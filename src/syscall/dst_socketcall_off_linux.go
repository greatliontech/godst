// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst && linux && (386 || s390x)

package syscall

import _ "unsafe"

// socketcall and rawsocketcall are accessed via assembly in x/sys/unix and x/net.
//
//go:linkname socketcall
func socketcall(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err Errno)

//go:linkname rawsocketcall
func rawsocketcall(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err Errno)
