// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import "sync"

// DSTFutexMuForTest exposes the futex bucket lock so the external test can
// choreograph the load-vs-enqueue window deterministically (the lost-wake
// pin below cannot be expressed through the raw syscall surface alone).
func DSTFutexMuForTest() *sync.Mutex { return &dstFutex.mu }
