// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(dst && race)

package sync

// dstHookEnabled is false outside a -tags dst -race build, so Mutex methods'
// `if dstHookEnabled { dstSyncAcquire(m) }` calls are dead-code-eliminated and are
// byte-identical to upstream (DST-L2-4). See dst_on.go for the active build.
const dstHookEnabled = false

// dstSyncAcquire is a no-op outside a -tags dst -race build: the DST Level-2
// sync-decision auto-hook compiles to nothing, so Mutex methods are byte-identical to
// upstream in a non-dst build, a plain -race build, and a -tags dst build without
// -race (DST-L2-4). See dst_on.go for the active form.
func dstSyncAcquire(m *Mutex) {}
