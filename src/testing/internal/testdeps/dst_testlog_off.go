// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst || !linux

package testdeps

import "io"

const dstTestlogEnabled = false

func dstBubbleTestlogAdd(l *testLog, op, name string) bool { return false }

func dstHostTestlogAddLocked(l *testLog, op, name string) bool { return false }

func dstStartTestLogLocked(l *testLog, w io.Writer) {}

func dstStopTestLogLocked(l *testLog) error { return nil }
