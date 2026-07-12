// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst || !linux

package os

import (
	"errors"
	_ "unsafe" // for go:linkname
)

//go:linkname dstInheritFile
func dstInheritFile(*File) (*File, error) {
	return nil, errors.New("simulation.InheritFile requires -tags dst on Linux")
}
