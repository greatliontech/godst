// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"sync/atomic"
	"unsafe"
)

func mmapWordPtr(data []byte) unsafe.Pointer { return unsafe.Pointer(&data[0]) }
func mmapWordUintptr(w *uint32) uintptr      { return uintptr(unsafe.Pointer(w)) }
func storeWord(w *uint32, v uint32)          { atomic.StoreUint32(w, v) }
