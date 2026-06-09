// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(dst && race)

package sync

// dstHookEnabled is false outside a -tags dst -race build, so Mutex.Lock's
// `if dstHookEnabled { dstSyncAcquire(m) }` is dead-code-eliminated and Lock is
// byte-identical to upstream (DST-L2-4). See dst_on.go for the active build.
const dstHookEnabled = false

// dstSyncAcquire is a no-op outside a -tags dst -race build: the DST Level-2
// sync-acquisition auto-hook compiles to nothing, so Mutex.Lock is byte-identical to
// upstream in a non-dst build, a plain -race build, and a -tags dst build without
// -race (DST-L2-4). See dst_on.go for the active form.
func dstSyncAcquire(m *Mutex) {}
