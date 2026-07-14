// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package sync

// dstMutexVirtualStarvation is false outside a -tags dst build: lockSlow's
// in-bubble starvation branch is dead-code-eliminated and the mutex keeps
// upstream's exact wall-clock starvation measure. See dst_mutex_on.go.
const dstMutexVirtualStarvation = false

// dstStarvationWakeThreshold is unused when the gate above is false; it
// exists so the folded branch typechecks.
const dstStarvationWakeThreshold = 0
