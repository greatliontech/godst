// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestDSTInheritFileCapability(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	created := make(chan struct{})
	originalClosed := make(chan struct{})
	go func() {
		<-created
		w.Close()
		close(originalClosed)
	}()

	var rawPanic, namedPanic, fdPanic, leakedMarkerPanic any
	Run(1, func() {
		rawPanic = inheritFilePanic(func() {
			syscall.Syscall(syscall.SYS_WRITE, w.Fd(), 0, 0)
		})
		namedPanic = inheritFilePanic(func() {
			w.Write([]byte("ungranted"))
		})
		if _, err := w.SyscallConn(); err == nil {
			t.Error("ungranted host file SyscallConn returned nil error")
		}
		if err := w.SetWriteDeadline(time.Now().Add(time.Second)); err == nil {
			t.Error("ungranted host file SetWriteDeadline returned nil error")
		}

		capability, err := InheritFile(w)
		if err != nil {
			t.Fatalf("InheritFile: %v", err)
		}
		if _, err := InheritFile(capability); err == nil {
			t.Error("InheritFile accepted an already simulated capability")
		}
		fdPanic = inheritFilePanic(func() { capability.Fd() })
		if _, err := capability.SyscallConn(); err == nil {
			t.Error("capability.SyscallConn returned nil error")
		}

		close(created)
		<-originalClosed
		if n, err := capability.Write([]byte("granted")); n != len("granted") || err != nil {
			t.Fatalf("capability.Write = %d, %v", n, err)
		}
		leakedMarkerPanic = inheritFilePanic(func() {
			syscall.Syscall(syscall.SYS_WRITE, w.Fd(), 0, 0)
		})
		if err := capability.Close(); err != nil {
			t.Fatalf("capability.Close: %v", err)
		}
	})

	for name, value := range map[string]any{
		"raw numeric write": rawPanic,
		"named host write":  namedPanic,
		"capability Fd":     fdPanic,
		"post-capability":   leakedMarkerPanic,
	} {
		if value == nil || !strings.Contains(value.(string), "unsupported under deterministic simulation") {
			t.Errorf("%s panic = %v, want unsupported-under-simulation refusal", name, value)
		}
	}
	buf := make([]byte, len("granted"))
	if n, err := r.Read(buf); n != len(buf) || err != nil || string(buf) != "granted" {
		t.Fatalf("host pipe read = %d, %v, %q", n, err, buf)
	}
	if err := r.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read after capability close = %d, %v; hidden writer remained open", n, err)
	}
}

func TestDSTRawHostFDsRequireCapability(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fd := w.Fd()
	var stat syscall.Stat_t

	operations := []struct {
		name string
		call func()
	}{
		{name: "named Read", call: func() { syscall.Read(int(fd), nil) }},
		{name: "named Write", call: func() { syscall.Write(int(fd), nil) }},
		{name: "named Pread", call: func() { syscall.Pread(int(fd), nil, 0) }},
		{name: "named Pwrite", call: func() { syscall.Pwrite(int(fd), nil, 0) }},
		{name: "named Seek", call: func() { syscall.Seek(int(fd), 0, 0) }},
		{name: "named Fstat", call: func() { syscall.Fstat(int(fd), &stat) }},
		{name: "named Fsync", call: func() { syscall.Fsync(int(fd)) }},
		{name: "named Fdatasync", call: func() { syscall.Fdatasync(int(fd)) }},
		{name: "Syscall", call: func() { syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0) }},
		{name: "Syscall6", call: func() { syscall.Syscall6(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0, 0, 0, 0) }},
		{name: "RawSyscall", call: func() { syscall.RawSyscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0) }},
		{name: "RawSyscall6", call: func() { syscall.RawSyscall6(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0, 0, 0, 0) }},
		{name: "raw read", call: func() { syscall.Syscall(syscall.SYS_READ, fd, uintptr(unsafe.Pointer(&[]byte{0}[0])), 1) }},
	}

	Run(1, func() {
		for _, op := range operations {
			t.Run(op.name, func(t *testing.T) {
				if value := inheritFilePanic(op.call); value == nil {
					t.Error("numeric host-fd operation did not panic")
				}
			})
		}
	})
}

func TestDSTInheritFileOperations(t *testing.T) {
	host, err := os.CreateTemp("", "dst-inherit-file")
	if err != nil {
		t.Fatal(err)
	}
	name := host.Name()
	defer os.Remove(name)
	defer host.Close()
	if _, err := host.WriteString("abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Seek(0, 0); err != nil {
		t.Fatal(err)
	}

	Run(1, func() {
		capability, err := InheritFile(host)
		if err != nil {
			t.Fatalf("InheritFile: %v", err)
		}
		defer capability.Close()
		buf := make([]byte, 3)
		if n, err := capability.Read(buf); n != 3 || err != nil || string(buf) != "abc" {
			t.Fatalf("Read = %d, %v, %q", n, err, buf)
		}
		if _, err := capability.Seek(0, 0); err != nil {
			t.Fatalf("Seek: %v", err)
		}
		if n, err := capability.WriteAt([]byte("Z"), 1); n != 1 || err != nil {
			t.Fatalf("WriteAt = %d, %v", n, err)
		}
		if err := capability.Truncate(4); err != nil {
			t.Fatalf("Truncate: %v", err)
		}
		if err := capability.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if info, err := capability.Stat(); err != nil || info.Size() != 4 {
			t.Fatalf("Stat = %v, %v", info, err)
		}
	})
	if got, err := os.ReadFile(name); err != nil || string(got) != "aZc\x00" {
		t.Fatalf("host content = %q, %v", got, err)
	}
}

func TestDSTInheritFileUsesCurrentAppendMode(t *testing.T) {
	for _, tc := range []struct {
		name          string
		openAppend    bool
		currentAppend bool
		wantContent   string
	}{
		{name: "set after open", currentAppend: true},
		{name: "clear after open", openAppend: true, wantContent: "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, err := os.CreateTemp("", "dst-inherit-append")
			if err != nil {
				t.Fatal(err)
			}
			name := host.Name()
			host.Close()
			defer os.Remove(name)
			flag := os.O_RDWR
			if tc.openAppend {
				flag |= os.O_APPEND
			}
			host, err = os.OpenFile(name, flag, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			setHostAppend(t, host, tc.currentAppend)

			Run(1, func() {
				capability, err := InheritFile(host)
				if err != nil {
					t.Fatalf("InheritFile: %v", err)
				}
				defer capability.Close()
				_, err = capability.WriteAt([]byte("ok"), 0)
				if tc.currentAppend {
					if err == nil || !strings.Contains(err.Error(), "invalid use of WriteAt on file opened with O_APPEND") {
						t.Fatalf("WriteAt on append capability = %v, want append-mode refusal", err)
					}
				} else if err != nil {
					t.Fatalf("WriteAt after clearing append mode: %v", err)
				}
			})
			if got, err := os.ReadFile(name); err != nil || string(got) != tc.wantContent {
				t.Fatalf("host content = %q, %v; want %q", got, err, tc.wantContent)
			}
		})
	}
}

func setHostAppend(t *testing.T, file *os.File, enabled bool) {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), syscall.F_GETFL, 0)
	if errno != 0 {
		t.Fatalf("F_GETFL: %v", errno)
	}
	if enabled {
		flags |= syscall.O_APPEND
	} else {
		flags &^= syscall.O_APPEND
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), syscall.F_SETFL, flags); errno != 0 {
		t.Fatalf("F_SETFL: %v", errno)
	}
}

func TestDSTInheritFileRefusesChdir(t *testing.T) {
	hostCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	Run(1, func() {
		capability, err := InheritFile(dir)
		if err != nil {
			t.Fatalf("InheritFile: %v", err)
		}
		defer capability.Close()
		if err := capability.Chdir(); err == nil {
			t.Error("inherited capability Chdir returned nil error")
		}
	})
	if got, err := os.Getwd(); err != nil || got != hostCwd {
		t.Fatalf("host cwd after refused Chdir = %q, %v; want %q", got, err, hostCwd)
	}
}

func TestDSTInheritFileRejectsForeignCaller(t *testing.T) {
	host, err := os.CreateTemp("", "dst-inherit-foreign")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(host.Name())
	defer host.Close()

	start := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		<-start
		capability, err := InheritFile(host)
		if capability != nil {
			capability.Close()
		}
		result <- err
	}()
	Run(1, func() {
		close(start)
		if err := <-result; err == nil {
			t.Error("foreign InheritFile returned nil error during an active run")
		}
	})
}

func TestDSTInheritFileOutsideRun(t *testing.T) {
	if _, err := InheritFile(os.Stdout); err == nil {
		t.Error("InheritFile outside a simulation returned nil error")
	}
}

func TestDSTUngrantedHostFileCloseRefused(t *testing.T) {
	host, err := os.CreateTemp("", "dst-ungranted-close")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(host.Name())
	defer host.Close()
	fd := host.Fd()

	Run(1, func() {
		if err := host.Close(); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("ungranted host Close = %v, want EBADF", err)
		}
	})
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0); errno != 0 {
		t.Fatalf("F_GETFD after refused Close = %v", errno)
	}
	if _, err := host.WriteString("still-owned"); err != nil {
		t.Fatalf("File unusable after refused Close: %v", err)
	}
}

func TestDSTInheritFileRejectsForeignUse(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	type foreignResult struct {
		writeErr error
		closeErr error
	}
	capabilities := make(chan *os.File)
	results := make(chan foreignResult)
	go func() {
		capability := <-capabilities
		_, writeErr := capability.Write([]byte("foreign"))
		results <- foreignResult{writeErr: writeErr, closeErr: capability.Close()}
	}()
	Run(1, func() {
		capability, err := InheritFile(w)
		if err != nil {
			t.Fatalf("InheritFile: %v", err)
		}
		defer capability.Close()
		capabilities <- capability
		result := <-results
		if result.writeErr == nil {
			t.Error("foreign capability write returned nil error")
		}
		if result.closeErr == nil {
			t.Error("foreign capability close returned nil error")
		}
		if n, err := capability.Write([]byte("bubble")); n != len("bubble") || err != nil {
			t.Fatalf("bubble capability write after foreign refusal = %d, %v", n, err)
		}
	})
	buf := make([]byte, len("bubble"))
	if n, err := r.Read(buf); n != len(buf) || err != nil || string(buf) != "bubble" {
		t.Fatalf("host pipe read = %d, %v, %q", n, err, buf)
	}
}

// TestDSTInheritFilePreservesNonblockingPollability pins that InheritFile
// preserves the source fd's O_NONBLOCK, so a nonblocking source stays a
// poller-registered capability with working deadlines. The pollable arm is
// supported with an explicit determinism boundary (design.md, "Determinism
// scope of the pollable arm"): same-seed transcript equality is not
// guaranteed while a pollable capability deadline is armed — the wake rides
// host readiness, not the seeded schedule. This pin uses an already-expired
// deadline, which fails without ever parking on the poller.
func TestDSTInheritFilePreservesNonblockingPollability(t *testing.T) {
	var fds [2]int
	if err := syscall.Pipe2(fds[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	r := os.NewFile(uintptr(fds[0]), "nonblocking-reader")
	w := os.NewFile(uintptr(fds[1]), "nonblocking-writer")
	defer r.Close()
	defer w.Close()

	Run(1, func() {
		capability, err := InheritFile(r)
		if err != nil {
			t.Fatalf("InheritFile: %v", err)
		}
		defer capability.Close()
		if err := capability.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		if _, err := capability.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read with expired deadline = %v, want os.ErrDeadlineExceeded", err)
		}
	})
}

func TestDSTInheritFileRunOwnership(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	var capability *os.File
	Run(1, func() {
		capability, err = InheritFile(w)
		if err != nil {
			t.Fatalf("InheritFile: %v", err)
		}
		Process("other", func() {
			// Cross-node use is the node-scoped refusal: a distinguishable
			// error naming the node scoping and the relay pattern — never
			// the closed shape (the capability is open; a closed-file error
			// misdirects diagnosis toward close bugs), and never host I/O.
			_, err := capability.Write(nil)
			if err == nil || errors.Is(err, os.ErrClosed) {
				t.Errorf("cross-process capability write = %v, want the node-scoped refusal", err)
			}
			if err == nil || !strings.Contains(err.Error(), "node-scoped to the root simulation body") {
				t.Errorf("cross-process capability write error = %v, want it to name the node scoping", err)
			}
			var pe *fs.PathError
			if !errors.As(err, &pe) {
				t.Errorf("cross-process capability write error = %T, want *fs.PathError", err)
			}
		})
		Host("elsewhere", HostConfig{}, func() {
			// The Host-body (proc 0 on another host) leg of the same scope.
			if _, err := capability.Write(nil); err == nil || !strings.Contains(err.Error(), "node-scoped to the root simulation body") {
				t.Errorf("cross-host capability write error = %v, want the node-scoped refusal", err)
			}
		})
	})
	if _, err := capability.Write(nil); !errors.Is(err, os.ErrClosed) {
		t.Errorf("post-run capability write = %v, want os.ErrClosed", err)
	}
	if err := capability.Close(); err != nil {
		t.Errorf("post-run capability close: %v", err)
	}
}

func TestDSTInheritFileRejectsProcessAdmission(t *testing.T) {
	host, err := os.CreateTemp("", "dst-inherit-process")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(host.Name())
	defer host.Close()
	Run(1, func() {
		Host("node", HostConfig{}, func() {
			if capability, err := InheritFile(host); err == nil || capability != nil {
				t.Errorf("host InheritFile = %v, %v; want nil, error", capability, err)
			}
		})
		Process("worker", func() {
			if capability, err := InheritFile(host); err == nil || capability != nil {
				t.Fatalf("process InheritFile = %v, %v; want nil, error", capability, err)
			}
		})
	})
}

func inheritFilePanic(f func()) (value any) {
	defer func() { value = recover() }()
	f()
	return nil
}
