// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst || race

package types

// dstSafeFromRHS enables the state-guarded Named.fromRHS read in
// (*Checker).isComplete under -tags dst and -race (including gopls/go/packages
// under test:dst-race) while folding out of untagged non-race std builds so
// go/types.(*Checker).isComplete compiles identically to stock Go.
const dstSafeFromRHS = true
