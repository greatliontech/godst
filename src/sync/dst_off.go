// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(dst && race)

package sync

const dstHookEnabled = false

func dstSyncAcquireRWMutex(rw *RWMutex) {}

func dstSyncReleaseRWMutexUnlock(rw *RWMutex) {}

func dstSyncReleaseRWMutexRUnlock(rw *RWMutex) {}

func dstSyncAcquireHBRWMutexLock(rw *RWMutex) {}

func dstSyncAcquireHBRWMutexRLock(rw *RWMutex) {}

func dstSyncAcquireOnce(o *Once) {}

func dstSyncReleaseOnce(o *Once) {}

func dstSyncAcquireHBOnce(o *Once) {}

func dstSyncObserveWaitGroup(wg *WaitGroup) {}

func dstSyncAcquireWaitGroup(wg *WaitGroup) {}

func dstSyncReleaseWaitGroup(wg *WaitGroup) {}

func dstSyncAcquireHBWaitGroup(wg *WaitGroup) {}

func dstSyncAcquireCond(c *Cond) {}
