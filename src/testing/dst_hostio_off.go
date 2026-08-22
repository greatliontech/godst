// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst || !linux

package testing

import (
	"io"
	"os"
)

// Stubs for the framework-stream bubble path (see dst_hostio.go): without
// -tags dst there is no simulation fence to pass, and the false const guard
// dead-code-eliminates the bubble legs from the printer. The bubble path is
// Linux-only like the interception boundary it passes through (its raw write
// rides the Linux trampolines).

const dstFrameworkStreamEnabled = false

func (p *chattyPrinter) dstBubbleUpdatef(testName, format string, args ...any) bool { return false }

func (p *chattyPrinter) dstBubblePrintf(testName, format string, args ...any) bool { return false }

func (p *chattyPrinter) dstBubbleBenchWrite(strErrBegin, indent string, b []byte, strErrEnd string, c []byte) bool {
	return false
}

func dstWrapTestlogWriter(f *os.File) io.Writer { return f }
