// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import "io/fs"

// The exported-API gate (api/go1-godst.txt, cmd/api) walks untagged build
// contexts only. These compile-time assertions pin the TAGGED surface it
// cannot see: an export that exists solely under -tags dst (HostFS), and an
// export whose tagged implementation is a twin of an untagged one (HostIP) —
// the api file pins the untagged twin, and nothing else stops the tagged
// signature from drifting. A signature change or removal fails this file's
// compile. Add here: any NEW dst-only export, and any new tagged/untagged
// twin pair. The tagged-context gate gap is tracked in
// docs/issues/tagged-import-policy-gate.md.
var (
	_ func(name string) fs.FS  = HostFS
	_ func(name string) string = HostIP
)
