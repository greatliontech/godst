// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || windows

package net

import (
	"internal/poll"
	"runtime"
	"syscall"
	"time"
)

// Network file descriptor.
type netFD struct {
	pfd poll.FD

	// immutable until Close
	family      int
	sotype      int
	isConnected bool // handshake completed or use of association with peer
	net         string
	laddr       Addr
	raddr       Addr
}

func (fd *netFD) name() string {
	var ls, rs string
	if fd.laddr != nil {
		ls = fd.laddr.String()
	}
	if fd.raddr != nil {
		rs = fd.raddr.String()
	}
	return fd.net + ":" + ls + "->" + rs
}

func (fd *netFD) setAddr(laddr, raddr Addr) {
	fd.laddr = laddr
	fd.raddr = raddr
	// TODO Replace with runtime.AddCleanup.
	runtime.SetFinalizer(fd, (*netFD).Close)
}

func (fd *netFD) Close() error {
	if dstNetEnabled {
		if handled, err := dstFDClose(fd); handled {
			return err
		}
	}
	// TODO Replace with runtime.AddCleanup.
	runtime.SetFinalizer(fd, nil)
	return fd.pfd.Close()
}

func (fd *netFD) shutdown(how int) error {
	if dstNetEnabled {
		if handled, err := dstFDShutdown(fd, how); handled {
			return wrapSyscallError("shutdown", err)
		}
	}
	err := fd.pfd.Shutdown(how)
	runtime.KeepAlive(fd)
	return wrapSyscallError("shutdown", err)
}

func (fd *netFD) closeRead() error {
	return fd.shutdown(syscall.SHUT_RD)
}

func (fd *netFD) closeWrite() error {
	return fd.shutdown(syscall.SHUT_WR)
}

func (fd *netFD) Read(p []byte) (n int, err error) {
	if dstNetEnabled {
		if n, handled, err := dstFDRead(fd, p); handled {
			return n, wrapSyscallError(readSyscallName, err)
		}
	}
	n, err = fd.pfd.Read(p)
	runtime.KeepAlive(fd)
	return n, wrapSyscallError(readSyscallName, err)
}

func (fd *netFD) readFrom(p []byte) (n int, sa syscall.Sockaddr, err error) {
	if dstNetEnabled {
		if n, sa, handled, err := dstFDReadFrom(fd, p); handled {
			return n, sa, wrapSyscallError(readFromSyscallName, err)
		}
	}
	n, sa, err = fd.pfd.ReadFrom(p)
	runtime.KeepAlive(fd)
	return n, sa, wrapSyscallError(readFromSyscallName, err)
}
func (fd *netFD) readFromInet4(p []byte, from *syscall.SockaddrInet4) (n int, err error) {
	if dstNetEnabled {
		if n, handled, err := dstFDReadFromInet4(fd, p, from); handled {
			return n, wrapSyscallError(readFromSyscallName, err)
		}
	}
	n, err = fd.pfd.ReadFromInet4(p, from)
	runtime.KeepAlive(fd)
	return n, wrapSyscallError(readFromSyscallName, err)
}

func (fd *netFD) readFromInet6(p []byte, from *syscall.SockaddrInet6) (n int, err error) {
	if dstNetEnabled {
		if n, handled, err := dstFDReadFromInet6(fd, p, from); handled {
			return n, wrapSyscallError(readFromSyscallName, err)
		}
	}
	n, err = fd.pfd.ReadFromInet6(p, from)
	runtime.KeepAlive(fd)
	return n, wrapSyscallError(readFromSyscallName, err)
}

func (fd *netFD) readMsg(p []byte, oob []byte, flags int) (n, oobn, retflags int, sa syscall.Sockaddr, err error) {
	if dstNetEnabled {
		if n, oobn, retflags, sa, handled, err := dstFDReadMsg(fd, p, oob, flags); handled {
			return n, oobn, retflags, sa, wrapSyscallError(readMsgSyscallName, err)
		}
	}
	n, oobn, retflags, sa, err = fd.pfd.ReadMsg(p, oob, flags)
	runtime.KeepAlive(fd)
	return n, oobn, retflags, sa, wrapSyscallError(readMsgSyscallName, err)
}

func (fd *netFD) readMsgInet4(p []byte, oob []byte, flags int, sa *syscall.SockaddrInet4) (n, oobn, retflags int, err error) {
	if dstNetEnabled {
		if n, oobn, retflags, handled, err := dstFDReadMsgInet4(fd, p, oob, flags, sa); handled {
			return n, oobn, retflags, wrapSyscallError(readMsgSyscallName, err)
		}
	}
	n, oobn, retflags, err = fd.pfd.ReadMsgInet4(p, oob, flags, sa)
	runtime.KeepAlive(fd)
	return n, oobn, retflags, wrapSyscallError(readMsgSyscallName, err)
}

func (fd *netFD) readMsgInet6(p []byte, oob []byte, flags int, sa *syscall.SockaddrInet6) (n, oobn, retflags int, err error) {
	if dstNetEnabled {
		if n, oobn, retflags, handled, err := dstFDReadMsgInet6(fd, p, oob, flags, sa); handled {
			return n, oobn, retflags, wrapSyscallError(readMsgSyscallName, err)
		}
	}
	n, oobn, retflags, err = fd.pfd.ReadMsgInet6(p, oob, flags, sa)
	runtime.KeepAlive(fd)
	return n, oobn, retflags, wrapSyscallError(readMsgSyscallName, err)
}

func (fd *netFD) Write(p []byte) (nn int, err error) {
	if dstNetEnabled {
		if nn, handled, err := dstFDWrite(fd, p); handled {
			return nn, wrapSyscallError(writeSyscallName, err)
		}
	}
	nn, err = fd.pfd.Write(p)
	runtime.KeepAlive(fd)
	return nn, wrapSyscallError(writeSyscallName, err)
}

func (fd *netFD) writeTo(p []byte, sa syscall.Sockaddr) (n int, err error) {
	if dstNetEnabled {
		if n, handled, err := dstFDWriteTo(fd, p, sa); handled {
			return n, wrapSyscallError(writeToSyscallName, err)
		}
	}
	n, err = fd.pfd.WriteTo(p, sa)
	runtime.KeepAlive(fd)
	return n, wrapSyscallError(writeToSyscallName, err)
}

func (fd *netFD) writeToInet4(p []byte, sa *syscall.SockaddrInet4) (n int, err error) {
	if dstNetEnabled {
		if n, handled, err := dstFDWriteToInet4(fd, p, sa); handled {
			return n, wrapSyscallError(writeToSyscallName, err)
		}
	}
	n, err = fd.pfd.WriteToInet4(p, sa)
	runtime.KeepAlive(fd)
	return n, wrapSyscallError(writeToSyscallName, err)
}

func (fd *netFD) writeToInet6(p []byte, sa *syscall.SockaddrInet6) (n int, err error) {
	if dstNetEnabled {
		if n, handled, err := dstFDWriteToInet6(fd, p, sa); handled {
			return n, wrapSyscallError(writeToSyscallName, err)
		}
	}
	n, err = fd.pfd.WriteToInet6(p, sa)
	runtime.KeepAlive(fd)
	return n, wrapSyscallError(writeToSyscallName, err)
}

func (fd *netFD) writeMsg(p []byte, oob []byte, sa syscall.Sockaddr) (n int, oobn int, err error) {
	if dstNetEnabled {
		if n, oobn, handled, err := dstFDWriteMsg(fd, p, oob, sa); handled {
			return n, oobn, wrapSyscallError(writeMsgSyscallName, err)
		}
	}
	n, oobn, err = fd.pfd.WriteMsg(p, oob, sa)
	runtime.KeepAlive(fd)
	return n, oobn, wrapSyscallError(writeMsgSyscallName, err)
}

func (fd *netFD) writeMsgInet4(p []byte, oob []byte, sa *syscall.SockaddrInet4) (n int, oobn int, err error) {
	if dstNetEnabled {
		if n, oobn, handled, err := dstFDWriteMsgInet4(fd, p, oob, sa); handled {
			return n, oobn, wrapSyscallError(writeMsgSyscallName, err)
		}
	}
	n, oobn, err = fd.pfd.WriteMsgInet4(p, oob, sa)
	runtime.KeepAlive(fd)
	return n, oobn, wrapSyscallError(writeMsgSyscallName, err)
}

func (fd *netFD) writeMsgInet6(p []byte, oob []byte, sa *syscall.SockaddrInet6) (n int, oobn int, err error) {
	if dstNetEnabled {
		if n, oobn, handled, err := dstFDWriteMsgInet6(fd, p, oob, sa); handled {
			return n, oobn, wrapSyscallError(writeMsgSyscallName, err)
		}
	}
	n, oobn, err = fd.pfd.WriteMsgInet6(p, oob, sa)
	runtime.KeepAlive(fd)
	return n, oobn, wrapSyscallError(writeMsgSyscallName, err)
}

func (fd *netFD) SetDeadline(t time.Time) error {
	if dstNetEnabled {
		if handled, err := dstFDSetDeadline(fd, t); handled {
			return err
		}
	}
	return fd.pfd.SetDeadline(t)
}

func (fd *netFD) SetReadDeadline(t time.Time) error {
	if dstNetEnabled {
		if handled, err := dstFDSetReadDeadline(fd, t); handled {
			return err
		}
	}
	return fd.pfd.SetReadDeadline(t)
}

func (fd *netFD) SetWriteDeadline(t time.Time) error {
	if dstNetEnabled {
		if handled, err := dstFDSetWriteDeadline(fd, t); handled {
			return err
		}
	}
	return fd.pfd.SetWriteDeadline(t)
}
