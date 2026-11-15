// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package sync

const dstSemaEnabled = false

func dstRWMutexWakeReaders(rw *RWMutex, r int32) {}

func dstWaitGroupWake(sema *uint32, w uint32) {}
