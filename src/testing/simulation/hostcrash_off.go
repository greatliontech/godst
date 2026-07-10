// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package simulation

// Without -tags dst there is no simulated filesystem: no disk to restore, no
// files to close, no tear policy. A run cannot be active either (Run panics on
// the missing tag), so every caller of these is already a no-op. See
// hostcrash_on.go.

func restoreHostDisk(host uint32) {}
func closeHostFiles(host uint32)  {}
func setCrashTear(on bool)        {}

func crashTearEnabled() bool { return false }
