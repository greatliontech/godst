// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"internal/poll"
	"internal/strconv"
	"internal/stringslite"
	"io"
	"sync"
	"syscall"
	"time"
	_ "unsafe" // for go:linkname
)

//go:linkname dstPidStarttime runtime.dstPidStarttime
func dstPidStarttime(pid int32) (start uint64, ok bool)

//go:linkname dstHostBootIdent runtime.dstHostBootIdent
func dstHostBootIdent() (hi, lo uint64, host uint32, boot uint64, ok bool)

const dstProcPIDNamespace = "pid:[1]"

// dstProcBootIDPath is the per-boot host identity leaf: a UUID constant within
// one boot of the calling goroutine's host, shared by its processes, and
// regenerated when the machine boots (first declaration, or Host re-declaration
// after CrashHost) — the cross-boot epoch discriminator. Derived
// deterministically from (run seed, host, boot count) in the runtime.
const dstProcBootIDPath = "/proc/sys/kernel/random/boot_id"

// dstProcOpenFile returns a generated procfs file for the small synthetic /proc
// surface DST supports. It is an overlay, not a disk node: procfs is kernel identity
// state derived from the runtime pid registry, not mutable filesystem content.
func dstProcOpenFile(name string, flag int) (*File, bool, error) {
	data, ident, base, handled, errno := dstProcStatData(name)
	if !handled {
		return nil, false, nil
	}
	if errno != nil {
		return nil, true, &PathError{Op: "open", Path: name, Err: errno}
	}
	if flag&(O_WRONLY|O_RDWR|O_CREATE|O_TRUNC) != 0 {
		return nil, true, &PathError{Op: "open", Path: name, Err: syscall.EACCES}
	}
	return dstNewFile(&dstProcFile{data: data, name: name, base: base, ident: ident}, name), true, nil
}

func dstProcStatName(op, name string) (FileInfo, bool, error) {
	data, ident, base, handled, errno := dstProcStatData(name)
	if !handled {
		return nil, false, nil
	}
	if errno != nil {
		return nil, true, &PathError{Op: op, Path: name, Err: errno}
	}
	_ = data // proc leaves report size 0, like Linux procfs.
	return &dstFileInfo{name: base, mode: 0o444, ident: ident}, true, nil
}

func dstProcReadlink(name string) (string, bool, error) {
	if !dstFSActive() {
		return "", false, nil
	}
	abs := dstProcAbs(name)
	if abs == "" {
		return "", false, nil
	}
	target, errno := dstProcReadlinkAbs(abs)
	if errno != nil {
		return "", true, &PathError{Op: "readlink", Path: name, Err: errno}
	}
	return target, true, nil
}

// dstProcReadlinkAbs answers readlink for a canonical /proc path, shared by
// the plain and Root resolvers so the two surfaces cannot diverge: the ns/pid
// symlink's target; for a MODELED regular leaf (a stat file, boot_id) — not a
// symlink — the host kernel's answer, EINVAL when the leaf exists and its
// lookup errno (ENOENT dead pid, ENOTDIR trailing slash on an existing leaf)
// when it does not; the deterministic unsupported-surface answer elsewhere.
// A trailing slash on the ns/pid symlink itself takes the lookup-errno route
// too — the slash forces the resolver past the link, exactly as on the host.
func dstProcReadlinkAbs(abs string) (string, error) {
	if abs == "/proc/self/ns/pid" {
		if _, ok := dstSimGetpid(); !ok {
			return "", syscall.ENOENT
		}
		return dstProcPIDNamespace, nil
	}
	trimmed := abs
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == '/' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	_, isStat := dstProcStatPID(trimmed)
	if isStat || trimmed == dstProcBootIDPath || trimmed == "/proc/self/ns/pid" {
		_, _, _, _, errno := dstProcStatDataAbs(abs)
		if errno == nil {
			errno = syscall.EINVAL
		}
		return "", errno
	}
	return "", dstErrUnsupportedFS
}

// dstProcLeafExists reports whether the slash-trimmed proc path names a leaf
// that currently exists: a live pid's stat file, the ns/pid symlink of a live
// simulated pid, or the boot_id leaf of an active run.
func dstProcLeafExists(trimmed string) bool {
	if pidText, ok := dstProcStatPID(trimmed); ok {
		pid, ok := dstProcResolvePID(pidText)
		if !ok {
			return false
		}
		_, ok = dstPidStarttime(int32(pid))
		return ok
	}
	if trimmed == "/proc/self/ns/pid" {
		_, ok := dstSimGetpid()
		return ok
	}
	if trimmed == dstProcBootIDPath {
		_, _, _, _, ok := dstHostBootIdent()
		return ok
	}
	return false
}

func dstProcReserved(name string) bool {
	return dstFSActive() && dstProcAbs(name) != ""
}

func dstProcStatData(name string) (data []byte, ident, base string, handled bool, errno error) {
	abs := dstProcAbs(name)
	if abs == "" {
		return nil, "", "", false, nil
	}
	return dstProcStatDataAbs(abs)
}

func dstProcStatDataAbs(abs string) (data []byte, ident, base string, handled bool, errno error) {
	if stringslite.HasSuffix(abs, "/") {
		// A trailing slash asserts directory-ness; on a proc LEAF that EXISTS
		// the host answers ENOTDIR (the filesystem section's trailing-slash
		// clause), and ENOENT elsewhere — including a leaf-shaped name whose
		// pid is dead: the kernel resolves the missing entry before the
		// trailing slash matters.
		trimmed := abs
		for len(trimmed) > 0 && trimmed[len(trimmed)-1] == '/' {
			trimmed = trimmed[:len(trimmed)-1]
		}
		if dstProcLeafExists(trimmed) {
			return nil, abs, "", true, syscall.ENOTDIR
		}
		return nil, abs, "", true, syscall.ENOENT
	}
	if abs == dstProcBootIDPath {
		hi, lo, host, boot, ok := dstHostBootIdent()
		if !ok {
			return nil, abs, "", true, syscall.ENOENT
		}
		data, ident := dstProcBootIDContents(hi, lo, host, boot)
		return data, ident, "boot_id", true, nil
	}
	pidText, ok := dstProcStatPID(abs)
	if !ok {
		return nil, abs, "", true, syscall.ENOENT
	}
	pid, ok := dstProcResolvePID(pidText)
	if !ok {
		return nil, abs, "", true, syscall.ENOENT
	}
	start, ok := dstPidStarttime(int32(pid))
	if !ok {
		return nil, abs, "", true, syscall.ENOENT
	}
	// The identity is the CANONICAL pid form, so /proc/self/stat and
	// /proc/<own-pid>/stat are one file to SameFile, as they are one inode on
	// the host.
	return []byte(dstProcStatContents(pid, start)), "/proc/" + strconv.Itoa(pid) + "/stat", "stat", true, nil
}

// dstProcBootIDContents renders the boot_id leaf: the canonical lowercase
// 8-4-4-4-12 UUID text plus newline, exactly the host kernel's shape, with RFC
// 4122 version-4 and variant bits set over the deterministic 128-bit material.
// The SameFile identity is per (host, boot): each boot mounts a fresh procfs
// instance, and two machines never share one.
func dstProcBootIDContents(hi, lo uint64, host uint32, boot uint64) (data []byte, ident string) {
	var b [16]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(hi >> (56 - 8*i))
		b[8+i] = byte(lo >> (56 - 8*i))
	}
	b[6] = 0x40 | b[6]&0x0f // version 4
	b[8] = 0x80 | b[8]&0x3f // variant 10
	const hexdig = "0123456789abcdef"
	out := make([]byte, 0, 37)
	for i, c := range b {
		switch i {
		case 4, 6, 8, 10:
			out = append(out, '-')
		}
		out = append(out, hexdig[c>>4], hexdig[c&0xf])
	}
	out = append(out, '\n')
	return out, dstProcBootIDPath + "#h" + strconv.Itoa(int(host)) + "#b" + strconv.FormatUint(boot, 10)
}

func dstProcStatPID(abs string) (string, bool) {
	const prefix = "/proc/"
	const suffix = "/stat"
	if !stringslite.HasPrefix(abs, prefix) || !stringslite.HasSuffix(abs, suffix) {
		return "", false
	}
	pid := abs[len(prefix) : len(abs)-len(suffix)]
	if pid == "" || stringslite.IndexByte(pid, '/') >= 0 {
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
	// []byte append rather than strings.Builder: os sits below the strings
	// package in the dependency policy (go/build's TestDependencies scans
	// tag-excluded files too, so dst files conform like os's own code).
	b := make([]byte, 0, len(name)+1)
	for _, c := range stack {
		b = append(b, '/')
		b = append(b, c...)
	}
	if trailing {
		b = append(b, '/')
	}
	return string(b)
}

func dstProcStackIsLeaf(stack []string) bool {
	if len(stack) == 3 && stack[0] == "proc" && stack[2] == "stat" {
		return true
	}
	if len(stack) == 5 && stack[0] == "proc" && stack[1] == "sys" && stack[2] == "kernel" && stack[3] == "random" && stack[4] == "boot_id" {
		return true
	}
	return len(stack) == 4 && stack[0] == "proc" && stack[1] == "self" && stack[2] == "ns" && stack[3] == "pid"
}

type dstProcFile struct {
	mu     sync.Mutex
	data   []byte
	name   string
	base   string // leaf base name ("stat", "boot_id") — the FileInfo.Name
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
	return &dstFileInfo{name: f.base, mode: 0o444, ident: f.ident}, nil
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
