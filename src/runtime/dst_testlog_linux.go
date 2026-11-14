// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package runtime

import "unsafe"

// dstWriteTestlogLine formats "<op> <name>\n" and writes it to fd via runtime
// write1 without allocating on the Go heap or entering the syscall-package
// interception boundary. Returns 0 on success or a positive Linux errno on
// failure.
//
// Lines up to 512 bytes use a non-escaping stack buffer; longer lines map a
// page-aligned anonymous scratch buffer directly from the OS and unmap it on
// return so neither memstats heap counters nor dstHeapAlloc/dstProcAlloc are
// perturbed by -test.testlogfile.
//
//go:linkname dstWriteTestlogLine
func dstWriteTestlogLine(fd int, op, name string) int32 {
	total := len(op) + 1 + len(name) + 1
	if total <= 512 {
		var stackBuf [512]byte
		n := copy(stackBuf[:], op)
		stackBuf[n] = ' '
		n++
		n += copy(stackBuf[n:], name)
		stackBuf[n] = '\n'
		n++
		return dstWriteTestlogAll(uintptr(fd), noescape(unsafe.Pointer(&stackBuf[0])), n)
	}
	alloc := alignUp(uintptr(total), physPageSize)
	if alloc == 0 || alloc < uintptr(total) {
		return _ENOMEM
	}
	p := sysAllocOS(alloc, "dst testlog line")
	if p == nil {
		return _ENOMEM
	}
	buf := unsafe.Slice((*byte)(p), total)
	n := copy(buf, op)
	buf[n] = ' '
	n++
	n += copy(buf[n:], name)
	buf[n] = '\n'
	errno := dstWriteTestlogAll(uintptr(fd), p, total)
	sysFreeOS(p, alloc)
	return errno
}

func dstWriteTestlogAll(fd uintptr, p unsafe.Pointer, rem int) int32 {
	const errEIO = 5
	for rem > 0 {
		chunk := rem
		if chunk > 0x7fffffff {
			chunk = 0x7fffffff
		}
		r := write1(fd, p, int32(chunk))
		if r > 0 {
			p = add(p, uintptr(r))
			rem -= int(r)
			continue
		}
		errno := -r
		if errno == _EINTR || errno == _EAGAIN {
			continue
		}
		if errno <= 0 {
			return errEIO
		}
		return errno
	}
	return 0
}
