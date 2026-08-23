// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package testing

import (
	"bytes"
	"os"
)

// A chatty printer over a writer that is not an *os.File must carry the -1
// no-stream sentinel: fd 0 is a valid descriptor (stdin), and the bubble's
// raw framework-stream writes route through hostFD whenever it is >= 0
// (dst_hostio.go), so a zero value would aim them at stdin.
func TestDSTChattyPrinterNonFileWriterHasNoHostFD(tt *T) {
	var buf bytes.Buffer
	if p := newChattyPrinter(&buf); p.hostFD != -1 {
		tt.Fatalf("chattyPrinter over a non-File writer: hostFD = %d, want -1", p.hostFD)
	}
	if p := newChattyPrinter(os.Stdout); p.hostFD != 1 {
		tt.Fatalf("chattyPrinter over os.Stdout: hostFD = %d, want 1", p.hostFD)
	}
}
