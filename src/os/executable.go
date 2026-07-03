// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package os

// Executable returns the path name for the executable that started
// the current process. There is no guarantee that the path is still
// pointing to the correct executable. If a symlink was used to start
// the process, depending on the operating system, the result might
// be the symlink or the path it pointed to. If a stable result is
// needed, [path/filepath.EvalSymlinks] might help.
//
// Executable returns an absolute path unless an error occurred.
//
// The main use case is finding resources located relative to an
// executable.
func Executable() (string, error) {
	// Interception-boundary fence: from a bubble goroutine this reads a host
	// path (e.g. /proc/self/exe) that names nothing in the simulated namespace
	// and varies per machine — refused with the standard unsupported shape. A
	// non-bubble goroutine keeps host access. See design.md "The interception
	// boundary". Folds away in stock builds.
	if dstSimEnabled && dstFenceActive() {
		return "", errDSTUnsupported
	}
	return executable()
}
