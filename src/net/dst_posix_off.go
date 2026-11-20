// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst && (unix || windows)

package net

import (
	"os"
	"syscall"
	"time"
)

func dstFDClose(*netFD) (bool, error)                                       { return false, nil }
func dstFDShutdown(*netFD, int) (bool, error)                               { return false, nil }
func dstFDRead(*netFD, []byte) (int, bool, error)                           { return 0, false, nil }
func dstFDReadFrom(*netFD, []byte) (int, syscall.Sockaddr, bool, error)     { return 0, nil, false, nil }
func dstFDReadFromInet4(*netFD, []byte, *syscall.SockaddrInet4) (int, bool, error) {
	return 0, false, nil
}
func dstFDReadFromInet6(*netFD, []byte, *syscall.SockaddrInet6) (int, bool, error) {
	return 0, false, nil
}
func dstFDReadMsg(*netFD, []byte, []byte, int) (int, int, int, syscall.Sockaddr, bool, error) {
	return 0, 0, 0, nil, false, nil
}
func dstFDReadMsgInet4(*netFD, []byte, []byte, int, *syscall.SockaddrInet4) (int, int, int, bool, error) {
	return 0, 0, 0, false, nil
}
func dstFDReadMsgInet6(*netFD, []byte, []byte, int, *syscall.SockaddrInet6) (int, int, int, bool, error) {
	return 0, 0, 0, false, nil
}
func dstFDWrite(*netFD, []byte) (int, bool, error)                         { return 0, false, nil }
func dstFDWriteTo(*netFD, []byte, syscall.Sockaddr) (int, bool, error)     { return 0, false, nil }
func dstFDWriteToInet4(*netFD, []byte, *syscall.SockaddrInet4) (int, bool, error) {
	return 0, false, nil
}
func dstFDWriteToInet6(*netFD, []byte, *syscall.SockaddrInet6) (int, bool, error) {
	return 0, false, nil
}
func dstFDWriteMsg(*netFD, []byte, []byte, syscall.Sockaddr) (int, int, bool, error) {
	return 0, 0, false, nil
}
func dstFDWriteMsgInet4(*netFD, []byte, []byte, *syscall.SockaddrInet4) (int, int, bool, error) {
	return 0, 0, false, nil
}
func dstFDWriteMsgInet6(*netFD, []byte, []byte, *syscall.SockaddrInet6) (int, int, bool, error) {
	return 0, 0, false, nil
}
func dstFDSetDeadline(*netFD, time.Time) (bool, error)      { return false, nil }
func dstFDSetReadDeadline(*netFD, time.Time) (bool, error)  { return false, nil }
func dstFDSetWriteDeadline(*netFD, time.Time) (bool, error) { return false, nil }
func dstFDDup(*netFD) (*os.File, bool, error)               { return nil, false, nil }
func dstFDSetReadBuffer(*netFD, int) (bool, error)          { return false, nil }
func dstFDSetWriteBuffer(*netFD, int) (bool, error)         { return false, nil }
