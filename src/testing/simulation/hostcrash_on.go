// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import _ "unsafe" // for go:linkname

//go:linkname dstRestoreHostDiskFor os.dstRestoreHostDiskFor
func dstRestoreHostDiskFor(host uint32)

//go:linkname dstCloseHostFilesFor os.dstCloseHostFilesFor
func dstCloseHostFilesFor(host uint32)

//go:linkname dstSetCrashTear os.dstSetCrashTear
func dstSetCrashTear(on bool)

// The os-side simulated filesystem exists only in a -tags dst build, so its
// symbols cannot be named from the untagged files of this package: CrashHost
// and run/TestWith compile in EVERY build (Run's missing-tag panic is what a
// stock binary is supposed to hit), and a direct linkname there makes an
// untagged program that merely calls them fail to LINK — a relocation error
// naming an internal symbol, instead of the documented panic. These shims keep
// the tag boundary at one place.

func restoreHostDisk(host uint32) { dstRestoreHostDiskFor(host) }
func closeHostFiles(host uint32)  { dstCloseHostFilesFor(host) }
func setCrashTear(on bool)        { dstSetCrashTear(on) }

// crashTearEnabled reports the run's crash-tear policy, so an exploration can
// record it in the Failure it reports (Replay restores it).
func crashTearEnabled() bool { return dstCrashTearEnabled() }

//go:linkname dstCrashTearEnabled os.dstCrashTearEnabled
func dstCrashTearEnabled() bool
