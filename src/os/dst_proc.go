// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"internal/poll"
	"io"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "unsafe" // for go:linkname
)

//go:linkname dstPidStarttime runtime.dstPidStarttime
func dstPidStarttime(pid int32) (start uint64, ok bool)

const dstProcPIDNamespace = "pid:[1]"

// dstProcOpenFile returns a generated procfs file for the small synthetic /proc
// surface DST supports. It is an overlay, not a disk node: procfs is kernel identity
// state derived from the runtime pid registry, not mutable filesystem content.
func dstProcOpenFile(name string, flag int) (*File, bool, error) {
	data, ident, handled, errno := dstProcStatData(name)
	if !handled {
		return nil, false, nil
	}
	if errno != nil {
		return nil, true, &PathError{Op: "open", Path: name, Err: errno}
	}
	if flag&(O_WRONLY|O_RDWR|O_CREATE|O_TRUNC) != 0 {
		return nil, true, &PathError{Op: "open", Path: name, Err: syscall.EACCES}
	}
	return dstNewFile(&dstProcFile{data: data, name: name, ident: ident}, name), true, nil
}

func dstProcStatName(op, name string) (FileInfo, bool, error) {
	data, ident, handled, errno := dstProcStatData(name)
	if !handled {
		return nil, false, nil
	}
	if errno != nil {
		return nil, true, &PathError{Op: op, Path: name, Err: errno}
	}
	_ = data // proc stat files report size 0, like Linux procfs.
	return &dstFileInfo{name: "stat", mode: 0o444, ident: ident}, true, nil
}

func dstProcReadlink(name string) (string, bool, error) {
	if !dstFSActive() {
		return "", false, nil
	}
	abs := dstProcAbs(name)
	if abs == "" {
		return "", false, nil
	}
	if abs != "/proc/self/ns/pid" {
		return "", true, &PathError{Op: "readlink", Path: name, Err: dstErrUnsupportedFS}
	}
	if _, ok := dstSimGetpid(); !ok {
		return "", true, &PathError{Op: "readlink", Path: name, Err: syscall.ENOENT}
	}
	return dstProcPIDNamespace, true, nil
}

func dstProcReserved(name string) bool {
	return dstFSActive() && dstProcAbs(name) != ""
}

func dstProcStatData(name string) (data []byte, ident string, handled bool, errno error) {
	abs := dstProcAbs(name)
	if abs == "" {
		return nil, "", false, nil
	}
	return dstProcStatDataAbs(abs)
}

func dstProcStatDataAbs(abs string) (data []byte, ident string, handled bool, errno error) {
	if strings.HasSuffix(abs, "/") {
		// A trailing slash asserts directory-ness; on a proc LEAF that exists the
		// host answers ENOTDIR (the filesystem section's trailing-slash clause),
		// and ENOENT elsewhere on the unsupported surface.
		trimmed := strings.TrimRight(abs, "/")
		if _, ok := dstProcStatPID(trimmed); ok || trimmed == "/proc/self/ns/pid" {
			return nil, abs, true, syscall.ENOTDIR
		}
		return nil, abs, true, syscall.ENOENT
	}
	pidText, ok := dstProcStatPID(abs)
	if !ok {
		return nil, abs, true, syscall.ENOENT
	}
	pid, ok := dstProcResolvePID(pidText)
	if !ok {
		return nil, abs, true, syscall.ENOENT
	}
	start, ok := dstPidStarttime(int32(pid))
	if !ok {
		return nil, abs, true, syscall.ENOENT
	}
	// The identity is the CANONICAL pid form, so /proc/self/stat and
	// /proc/<own-pid>/stat are one file to SameFile, as they are one inode on
	// the host.
	return []byte(dstProcStatContents(pid, start)), "/proc/" + strconv.Itoa(pid) + "/stat", true, nil
}

func dstProcStatPID(abs string) (string, bool) {
	const prefix = "/proc/"
	const suffix = "/stat"
	if !strings.HasPrefix(abs, prefix) || !strings.HasSuffix(abs, suffix) {
		return "", false
	}
	pid := abs[len(prefix) : len(abs)-len(suffix)]
	if pid == "" || strings.IndexByte(pid, '/') >= 0 {
		return "", false
	}
	return pid, true
}

func dstProcResolvePID(pidText string) (int, bool) {
	if pidText == "self" {
		pid, ok := dstSimGetpid()
		return pid, ok && pid > 0
	}
	// Linux procfs's name_to_int rejects a leading zero ("0424242" is not a pid
	// entry), so a zero-padded alias never names a live pid here either.
	if pidText[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(pidText); i++ {
		if pidText[i] < '0' || pidText[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(pidText, 10, 32)
	if err != nil || n <= 0 {
		return 0, false
	}
	return int(n), true
}

func dstProcStatContents(pid int, start uint64) string {
	pidText := strconv.Itoa(pid)
	startText := strconv.FormatUint(start, 10)
	ppid := "1"
	if p, ok := dstSimGetppid(); ok {
		ppid = strconv.Itoa(p)
	}
	// Field 22 is starttime. The preceding fields are deterministic placeholders
	// for the Linux proc_pid_stat shape gmdb parses for liveness recovery.
	return pidText + " (dst) R " + ppid + " 0 0 0 0 0 0 0 0 0 0 0 0 0 20 0 1 0 " + startText + "\n"
}

func dstProcAbs(name string) string {
	if name == "" {
		return ""
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	comps, trailing := dstFSComponents(name)
	root := dstFSDiskHere().root
	nodeStack := []*dstFSNode{root}
	stack := make([]string, 0, len(comps))
	invalidProc := false
	for _, c := range comps {
		if invalidProc {
			stack = append(stack, c)
			continue
		}
		switch c {
		case ".":
			if dstProcStackIsLeaf(stack) {
				invalidProc = true
				stack = append(stack, c)
			}
			continue
		case "..":
			if len(stack) > 1 && stack[0] == "proc" {
				invalidProc = true
				stack = append(stack, c)
				continue
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
				if len(nodeStack) > 1 {
					nodeStack = nodeStack[:len(nodeStack)-1]
				}
			}
		default:
			if dstProcStackIsLeaf(stack) {
				invalidProc = true
				stack = append(stack, c)
				continue
			}
			if len(stack) == 0 && c == "proc" {
				stack = append(stack, c)
				continue
			}
			if len(stack) == 0 || stack[0] != "proc" {
				cur := nodeStack[len(nodeStack)-1]
				next := cur.entries[c]
				if next == nil || !next.isDir {
					return ""
				}
				nodeStack = append(nodeStack, next)
			}
			stack = append(stack, c)
		}
	}
	if len(stack) == 0 || stack[0] != "proc" {
		return ""
	}
	var b strings.Builder
	for _, c := range stack {
		b.WriteByte('/')
		b.WriteString(c)
	}
	if trailing {
		b.WriteByte('/')
	}
	return b.String()
}

func dstProcStackIsLeaf(stack []string) bool {
	if len(stack) == 3 && stack[0] == "proc" && stack[2] == "stat" {
		return true
	}
	return len(stack) == 4 && stack[0] == "proc" && stack[1] == "self" && stack[2] == "ns" && stack[3] == "pid"
}

type dstProcFile struct {
	mu     sync.Mutex
	data   []byte
	name   string
	ident  string
	off    int64
	closed bool
}

func (f *dstProcFile) read(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, poll.ErrFileClosing
	}
	if len(b) == 0 {
		return 0, nil
	}
	if f.off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(b, f.data[f.off:])
	f.off += int64(n)
	return n, nil
}

func (f *dstProcFile) pread(b []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, poll.ErrFileClosing
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	return copy(b, f.data[off:]), nil
}

func (f *dstProcFile) write([]byte) (int, error) {
	if err := f.closedError(); err != nil {
		return 0, err
	}
	return 0, syscall.EBADF
}

func (f *dstProcFile) pwrite([]byte, int64) (int, error) {
	if err := f.closedError(); err != nil {
		return 0, err
	}
	return 0, syscall.EBADF
}

func (f *dstProcFile) truncate(int64) error {
	if err := f.closedError(); err != nil {
		return err
	}
	return syscall.EINVAL
}

func (f *dstProcFile) sync() error {
	if err := f.closedError(); err != nil {
		return err
	}
	return syscall.EINVAL
}

func (f *dstProcFile) readdir(int) ([]string, []FileInfo, error) {
	if err := f.closedError(); err != nil {
		return nil, nil, err
	}
	return nil, nil, syscall.ENOTDIR
}

func (f *dstProcFile) chdirHandle() error {
	if err := f.closedError(); err != nil {
		return err
	}
	return syscall.ENOTDIR
}

func (f *dstProcFile) chmodHandle(FileMode) error {
	if err := f.closedError(); err != nil {
		return err
	}
	return syscall.EPERM
}

func (f *dstProcFile) closedError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return poll.ErrFileClosing
	}
	return nil
}

func (f *dstProcFile) seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, poll.ErrFileClosing
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.off
	case io.SeekEnd:
		base = int64(len(f.data))
	default:
		return 0, syscall.EINVAL
	}
	pos := base + offset
	if pos < 0 {
		return 0, syscall.EINVAL
	}
	f.off = pos
	return pos, nil
}

func (f *dstProcFile) stat() (FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, poll.ErrFileClosing
	}
	return &dstFileInfo{name: "stat", mode: 0o444, ident: f.ident}, nil
}

func (f *dstProcFile) closeFile() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return poll.ErrFileClosing
	}
	f.closed = true
	return nil
}

func (f *dstProcFile) setDeadline(rd, wd bool, t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return poll.ErrFileClosing
	}
	return poll.ErrNoDeadline
}
