// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && !linux

package syscall

import _ "unsafe" // for go:linkname

//go:linkname dstSetVirtualFDActive
func dstSetVirtualFDActive(fd uintptr, active bool) {}

//go:linkname dstClearVirtualFDs
func dstClearVirtualFDs() {}
