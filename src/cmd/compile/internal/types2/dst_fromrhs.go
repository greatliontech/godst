// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types2

// dstSafeFromRHS enables the state-guarded Named.fromRHS read in
// (*Checker).isComplete. cmd/compile/internal/types2 is compiler-internal (not
// part of the untagged std binary-text surface), so it keeps the race fix
// enabled unconditionally while go/types gates it on dst || race.
const dstSafeFromRHS = true
