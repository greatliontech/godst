// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package os

const dstSimEnabled = false

func dstSimGetpid() (int, bool)         { return 0, false }
func dstSimGethostname() (string, bool) { return "", false }
func dstSimGetppid() (int, bool)        { return 0, false }
func dstSimGetuid() (int, bool)         { return 0, false }
func dstSimGetgid() (int, bool)         { return 0, false }
func dstSimGeteuid() (int, bool)        { return 0, false }
func dstSimGetegid() (int, bool)        { return 0, false }
func dstFenceActive() bool              { return false }
func dstHostIOActive() bool             { return false }

var errDSTUnsupported error
