// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package conformance

import (
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Filesystem ops. Each constructor bakes every parameter (paths relative
// to the world root, payload bytes, slot indices) at generation time, so
// both legs execute identical sequences.

func statKind(fi os.FileInfo) string {
	m := fi.Mode()
	switch {
	case m.IsDir():
		return "d"
	case m&os.ModeCharDevice != 0:
		return "c"
	case m&os.ModeNamedPipe != 0:
		return "p"
	case m.IsRegular():
		return "f"
	default:
		return "?"
	}
}

func statState(fi os.FileInfo) string {
	// Directory sizes are filesystem-defined on the host (ext4 block
	// multiples, tmpfs entry-scaled, btrfs 0): an unspecified
	// observable, excluded from the compared projection like mtimes.
	if fi.IsDir() {
		return fmt.Sprintf("kind=%s perm=%04o size=*", statKind(fi), fi.Mode().Perm())
	}
	return fmt.Sprintf("kind=%s perm=%04o size=%d", statKind(fi), fi.Mode().Perm(), fi.Size())
}

func flagName(flag int) string {
	var parts []string
	switch flag & 0x3 {
	case os.O_RDONLY:
		parts = append(parts, "O_RDONLY")
	case os.O_WRONLY:
		parts = append(parts, "O_WRONLY")
	case os.O_RDWR:
		parts = append(parts, "O_RDWR")
	}
	for _, f := range []struct {
		bit  int
		name string
	}{
		{os.O_CREATE, "O_CREATE"}, {os.O_EXCL, "O_EXCL"}, {os.O_TRUNC, "O_TRUNC"},
		{os.O_APPEND, "O_APPEND"},
	} {
		if flag&f.bit != 0 {
			parts = append(parts, f.name)
		}
	}
	// Linux O_SYNC contains the O_DSYNC bit; report the full-sync flag
	// once and the data-only flag only when it stands alone.
	switch {
	case flag&os.O_SYNC == os.O_SYNC:
		parts = append(parts, "O_SYNC")
	case flag&syscall.O_DSYNC != 0:
		parts = append(parts, "O_DSYNC")
	}
	return strings.Join(parts, "|")
}

// fsOpen opens rel with flag/perm. track appends the result (nil on
// failure) to the slot table so indices stay leg-aligned; untracked
// unexpected successes are closed immediately.
func fsOpen(rel string, flag int, perm os.FileMode, track bool) op {
	return op{
		name: fmt.Sprintf("open(%q, %s, %04o) track=%v", rel, flagName(flag), perm, track),
		run: func(w *world) outcome {
			f, err := os.OpenFile(w.path(rel), flag, perm)
			if track {
				w.files = append(w.files, f)
			} else if f != nil {
				f.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

// fsOpenProbe opens and immediately closes: self-contained, so a
// one-leg-only success (the permission-enforcement gap) leaves no slot
// residue.
func fsOpenProbe(label, rel string, flag int) op {
	return op{
		name: fmt.Sprintf("%s(%q, %s)", label, rel, flagName(flag)),
		run: func(w *world) outcome {
			f, err := os.OpenFile(w.path(rel), flag, 0)
			if f != nil {
				f.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

func slotFile(w *world, slot int) *os.File {
	if slot < 0 || slot >= len(w.files) {
		return nil
	}
	return w.files[slot]
}

const nilSlot = "harness:nil-slot"

func fsClose(slot int) op {
	return op{
		name: fmt.Sprintf("close(slot %d)", slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(f.Close()), N: -1}
		},
	}
}

func fsWrite(slot int, payload []byte) op {
	return op{
		name: fmt.Sprintf("write(slot %d, %d bytes)", slot, len(payload)),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			n, err := f.Write(payload)
			return outcome{Err: errClass(err), N: n}
		},
	}
}

func fsRead(slot, n int) op {
	return op{
		name: fmt.Sprintf("read(slot %d, %d bytes)", slot, n),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			buf := make([]byte, n)
			rn, err := f.Read(buf)
			return outcome{Err: errClass(err), N: rn, State: contentHash(buf[:max(rn, 0)])}
		},
	}
}

// fsReadOnDir probes read(2) on a directory descriptor. The errno is
// host-variability: Linux reports EISDIR at position 0 (and always on
// tmpfs), but EINVAL on ext* once getdents64 has advanced the directory
// position (probed on 7.1.3-arch1-2) — so the op compares the reject
// CLASS {EISDIR, EINVAL}, of which the sim's stable EISDIR is a member.
func fsReadOnDir(slot int) op {
	return op{
		name: fmt.Sprintf("read-on-dir(slot %d)", slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			n, err := f.Read(make([]byte, 16))
			cls := errClass(err)
			if cls == "PathError(read)/errno:EISDIR" || cls == "PathError(read)/errno:EINVAL" {
				cls = "PathError(read)/errno:{EISDIR|EINVAL}"
			}
			return outcome{Err: cls, N: n}
		},
	}
}

func fsPread(slot, n int, off int64) op {
	return op{
		name: fmt.Sprintf("pread(slot %d, %d bytes, off %d)", slot, n, off),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			buf := make([]byte, n)
			rn, err := f.ReadAt(buf, off)
			return outcome{Err: errClass(err), N: rn, State: contentHash(buf[:max(rn, 0)])}
		},
	}
}

func fsPwrite(slot int, payload []byte, off int64) op {
	return op{
		name: fmt.Sprintf("pwrite(slot %d, %d bytes, off %d)", slot, len(payload), off),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			n, err := f.WriteAt(payload, off)
			return outcome{Err: errClass(err), N: n}
		},
	}
}

func fsSeek(slot int, off int64, whence int) op {
	return op{
		name: fmt.Sprintf("seek(slot %d, %d, %d)", slot, off, whence),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			pos, err := f.Seek(off, whence)
			return outcome{Err: errClass(err), N: int(pos)}
		},
	}
}

func fsMkdir(rel string, perm os.FileMode) op {
	return op{
		name: fmt.Sprintf("mkdir(%q, %04o)", rel, perm),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.Mkdir(w.path(rel), perm)), N: -1}
		},
	}
}

func fsMkdirAll(rel string, perm os.FileMode) op {
	return op{
		name: fmt.Sprintf("mkdirall(%q, %04o)", rel, perm),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.MkdirAll(w.path(rel), perm)), N: -1}
		},
	}
}

func fsRemove(rel string) op {
	return op{
		name: fmt.Sprintf("remove(%q)", rel),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.Remove(w.path(rel))), N: -1}
		},
	}
}

func fsRemoveAll(rel string) op {
	return op{
		name: fmt.Sprintf("removeall(%q)", rel),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.RemoveAll(w.path(rel))), N: -1}
		},
	}
}

// fsRemoveDot exercises rmdir(".")'s reserved EINVAL. "." resolves
// against the process cwd on both legs; rmdir(".") mutates nothing.
func fsRemoveDot() op {
	return op{
		name: `remove(".")`,
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.Remove(".")), N: -1}
		},
	}
}

func fsRename(oldRel, newRel string) op {
	return op{
		name: fmt.Sprintf("rename(%q, %q)", oldRel, newRel),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.Rename(w.path(oldRel), w.path(newRel))), N: -1}
		},
	}
}

func fsTruncateName(rel string, size int64) op {
	return op{
		name: fmt.Sprintf("truncate(%q, %d)", rel, size),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.Truncate(w.path(rel), size)), N: -1}
		},
	}
}

func fsTruncateFd(slot int, size int64) op {
	return op{
		name: fmt.Sprintf("ftruncate(slot %d, %d)", slot, size),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(f.Truncate(size)), N: -1}
		},
	}
}

func fsSync(slot int) op {
	return op{
		name: fmt.Sprintf("fsync(slot %d)", slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(f.Sync()), N: -1}
		},
	}
}

// fsFdatasync goes through the raw fd (virtual under DST), the surface
// x/sys-style callers use.
func fsFdatasync(label string, slot int) op {
	return op{
		name: fmt.Sprintf("%s(slot %d)", label, slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(syscall.Fdatasync(int(f.Fd()))), N: -1}
		},
	}
}

func fsStat(rel string, fromCreate bool) op {
	return op{
		name:           fmt.Sprintf("stat(%q) fromCreate=%v", rel, fromCreate),
		permFromCreate: fromCreate,
		run: func(w *world) outcome {
			fi, err := os.Stat(w.path(rel))
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			return outcome{N: -1, State: statState(fi)}
		},
	}
}

func fsFstat(slot int, fromCreate bool) op {
	return op{
		name:           fmt.Sprintf("fstat(slot %d) fromCreate=%v", slot, fromCreate),
		permFromCreate: fromCreate,
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			fi, err := f.Stat()
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			return outcome{N: -1, State: statState(fi)}
		},
	}
}

// fsFstatNlink reads Nlink through the raw fstat (virtual fd under
// DST). Aimed at directories: the recorded per-subdirectory-Nlink gap.
func fsFstatNlink(slot int) op {
	return op{
		name: fmt.Sprintf("fstat-nlink(slot %d)", slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			var st syscall.Stat_t
			if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			return outcome{N: -1, State: fmt.Sprintf("nlink=%d", st.Nlink)}
		},
	}
}

// fsStatSys pins the FileInfo.Sys() shape (recorded: nil under DST).
func fsStatSys(rel string) op {
	return op{
		name: fmt.Sprintf("stat-sys(%q)", rel),
		run: func(w *world) outcome {
			fi, err := os.Stat(w.path(rel))
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			if _, ok := fi.Sys().(*syscall.Stat_t); ok {
				return outcome{N: -1, State: "sys=stat"}
			}
			if fi.Sys() == nil {
				return outcome{N: -1, State: "sys=nil"}
			}
			return outcome{N: -1, State: "sys=other"}
		},
	}
}

func fsChmod(rel string, perm os.FileMode) op {
	return op{
		name: fmt.Sprintf("chmod(%q, %04o)", rel, perm),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.Chmod(w.path(rel), perm)), N: -1}
		},
	}
}

// fsChownNoop calls Chown(-1, -1): a no-op on the host (no ownership
// change), fenced under DST (the recorded no-ownership-model gap).
func fsChownNoop(rel string) op {
	return op{
		name: fmt.Sprintf("chown(%q, -1, -1)", rel),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.Chown(w.path(rel), -1, -1)), N: -1}
		},
	}
}

var chtimesFixed = time.Unix(1234567890, 123456789)

func fsChtimes(rel string) op {
	return op{
		name: fmt.Sprintf("chtimes(%q, fixed)", rel),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.Chtimes(w.path(rel), chtimesFixed, chtimesFixed)), N: -1}
		},
	}
}

// fsChtimesOmitMissing probes the utimensat both-OMIT-on-missing-path
// quirk (zero time.Time values are UTIME_OMIT).
func fsChtimesOmitMissing(rel string) op {
	return op{
		name: fmt.Sprintf("chtimes-omit-missing(%q)", rel),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.Chtimes(w.path(rel), time.Time{}, time.Time{})), N: -1}
		},
	}
}

// fsStatMtime compares the mtime VALUE — only meaningful after an
// explicit fixed-time Chtimes (wall-domain mtimes are never compared).
func fsStatMtime(rel string) op {
	return op{
		name: fmt.Sprintf("stat-mtime(%q)", rel),
		run: func(w *world) outcome {
			fi, err := os.Stat(w.path(rel))
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			return outcome{N: -1, State: fmt.Sprintf("mtime=%d", fi.ModTime().UnixNano())}
		},
	}
}

func fsReadDir(rel string) op {
	return op{
		name: fmt.Sprintf("readdir(%q)", rel),
		run: func(w *world) outcome {
			ents, err := os.ReadDir(w.path(rel))
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			parts := make([]string, len(ents))
			for i, e := range ents {
				kind := "f"
				if e.IsDir() {
					kind = "d"
				}
				parts[i] = e.Name() + ":" + kind
			}
			return outcome{N: len(ents), State: strings.Join(parts, ",")}
		},
	}
}

// fsReaddirnamesChunks drains a directory handle in chunks of n. Host
// getdents order is filesystem-defined, so the union is compared as a
// sorted set and only the per-chunk COUNT sequence positionally; the
// sim's sorted order is trivially a member of the host-legal
// permutation set (sortedness itself is pinned by the fork's unit
// suite, which the host cannot pin).
func fsReaddirnamesChunks(slot, n int) op {
	return op{
		name: fmt.Sprintf("readdirnames-chunks(slot %d, %d)", slot, n),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			var counts []string
			var all []string
			for {
				names, err := f.Readdirnames(n)
				counts = append(counts, fmt.Sprint(len(names)))
				all = append(all, names...)
				if err == io.EOF {
					break
				}
				if err != nil {
					return outcome{Err: errClass(err), N: len(all)}
				}
				if len(names) == 0 {
					break
				}
			}
			sort.Strings(all)
			return outcome{N: len(all), State: "counts=" + strings.Join(counts, ",") + " names=" + strings.Join(all, ",")}
		},
	}
}

func fsSetDeadline(slot int) op {
	return op{
		name: fmt.Sprintf("setdeadline(slot %d)", slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(f.SetDeadline(time.Now().Add(time.Second))), N: -1}
		},
	}
}

func fsFlock(slot, how int, label string) op {
	return op{
		name: fmt.Sprintf("flock(slot %d, %s)", slot, label),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(syscall.Flock(int(f.Fd()), how)), N: -1}
		},
	}
}

// fsMmapProbe maps the slot's file MAP_SHARED read-only and hashes the
// mapped bytes: the page-cache identity probe.
func fsMmapProbe(slot, length int) op {
	return op{
		name: fmt.Sprintf("mmap-probe(slot %d, %d bytes)", slot, length),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			data, err := syscall.Mmap(int(f.Fd()), 0, length, syscall.PROT_READ, syscall.MAP_SHARED)
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			h := contentHash(data)
			if err := syscall.Munmap(data); err != nil {
				return outcome{Err: errClass(err), N: -1, State: h}
			}
			return outcome{N: -1, State: h}
		},
	}
}

// dnMmap probes mmap on the null device (host: ENODEV).
func dnMmap(slot int) op {
	return op{
		name: fmt.Sprintf("devnull-mmap(slot %d)", slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			data, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
			if err == nil {
				syscall.Munmap(data)
				return outcome{N: -1, State: "mapped"}
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

// dnFstat pins the raw null-device identity (S_IFCHR, rdev (1,3)).
func dnFstat(slot int) op {
	return op{
		name: fmt.Sprintf("devnull-fstat(slot %d)", slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			var st syscall.Stat_t
			if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			return outcome{N: -1, State: fmt.Sprintf("ifmt=%#o rdev=%#x size=%d", st.Mode&syscall.S_IFMT, st.Rdev, st.Size)}
		},
	}
}

// fsDevNullOpen opens the real device path (never the world root). Only
// non-mutating device ops are generated against it: a root-privileged
// harness must never chmod or unlink the host's /dev/null.
func fsDevNullOpen(flag int, track bool) op {
	return op{
		name: fmt.Sprintf("devnull-open(%s) track=%v", flagName(flag), track),
		run: func(w *world) outcome {
			f, err := os.OpenFile(os.DevNull, flag, 0o666)
			if track {
				w.files = append(w.files, f)
			} else if f != nil {
				f.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

func fsDevNullTruncateName(size int64) op {
	return op{
		name: fmt.Sprintf("devnull-truncate-name(%d)", size),
		run: func(w *world) outcome {
			return outcome{Err: errClass(os.Truncate(os.DevNull, size)), N: -1}
		},
	}
}

func fsDevNullStat() op {
	return op{
		name: "devnull-stat",
		run: func(w *world) outcome {
			fi, err := os.Stat(os.DevNull)
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			return outcome{N: -1, State: statState(fi)}
		},
	}
}

// ---------------------------------------------------------------------------
// The filesystem allowlist: exactly the divergences the spec records.

var chtimesOmitMissingHostSucceeds = sync.OnceValue(func() bool {
	dir, err := os.MkdirTemp("", "conformance-probe")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	return os.Chtimes(dir+"/missing", time.Time{}, time.Time{}) == nil
})

func fsAllowlist() []allowEntry {
	return []allowEntry{
		{
			key:        "fs-create-umask",
			cite:       `design.md §In-memory deterministic filesystem: "umask is not modeled (recorded stance): created files and directories store the requested mode verbatim"`,
			applicable: func() bool { return hostUmask&0o777 != 0 },
			match: func(o op, host, sim outcome) bool {
				if !o.permFromCreate || host.Err != "" || sim.Err != "" {
					return false
				}
				hp, hRest, hok := parseStatePerm(host.State)
				sp, sRest, sok := parseStatePerm(sim.State)
				return hok && sok && hRest == sRest && host.N == sim.N &&
					hp == sp&^hostUmask && hp != sp
			},
		},
		{
			key:  "fs-dir-nlink",
			cite: `design.md §In-memory deterministic filesystem: "directories report Nlink 2 (per-subdirectory increments are not modeled)"`,
			match: func(o op, host, sim outcome) bool {
				if !strings.HasPrefix(o.name, "fstat-nlink(") || host.Err != "" || sim.Err != "" {
					return false
				}
				hn, hRest, hok := parseStateInt(host.State, "nlink")
				sn, sRest, sok := parseStateInt(sim.State, "nlink")
				return hok && sok && hRest == sRest && sn == 2 && hn > 2
			},
		},
		{
			key:  "fs-sys-nil",
			cite: `design.md §Roadmap Landed (Disk): "Sys() is nil" (no host stat behind a simulated node)`,
			match: func(o op, host, sim outcome) bool {
				return strings.HasPrefix(o.name, "stat-sys(") &&
					host.Err == "" && sim.Err == "" &&
					host.State == "sys=stat" && sim.State == "sys=nil"
			},
		},
		{
			key:        "fs-perm-not-enforced",
			cite:       `design.md §In-memory deterministic filesystem: "Permission bits are stored and reported but not enforced in the base model (no simulated credential checks)"`,
			applicable: func() bool { return os.Geteuid() != 0 },
			match: func(o op, host, sim outcome) bool {
				return strings.HasPrefix(o.name, "open-eacces-probe(") &&
					host.Err == "PathError(open)/errno:EACCES" && sim.Err == ""
			},
		},
		{
			key:  "fs-chown-fenced",
			cite: `design.md §In-memory deterministic filesystem: "ownership is not represented at all — Chown/Lchown and File.Chown stay fenced"`,
			match: func(o op, host, sim outcome) bool {
				return strings.HasPrefix(o.name, "chown(") &&
					host.Err == "" && sim.Err == "PathError(chown)/dst-unsupported"
			},
		},
		{
			key:  "fs-removeall-op-name",
			cite: `design.md §In-memory deterministic filesystem: "RemoveAll under a run is a single atomic subtree unlink on the tree ... so its failures carry *PathError Op \"removeall\" where the host surfaces the failing step's name (unlinkat, openat, …); the wrapped errno identity is preserved"`,
			match: func(o op, host, sim outcome) bool {
				if !strings.HasPrefix(o.name, "removeall(") || host.N != sim.N {
					return false
				}
				hOp, hRest, hok := splitPathErrOp(host.Err)
				sOp, sRest, sok := splitPathErrOp(sim.Err)
				if !hok || !sok || hRest != sRest || sOp != "removeall" {
					return false
				}
				switch hOp {
				case "unlinkat", "openat", "open", "fdopendir", "remove":
					return true
				}
				return false
			},
		},
		{
			key:  "fs-rawfd-fence-loud",
			cite: `design.md §In-memory deterministic filesystem (the virtual-fd paragraph): "Fd() on a CLOSED simulated file reports -1 ... so a raw syscall naming that -1 meets the raw boundary's fence (the loud unsupported refusal) where the host kernel answers EBADF for a closed real descriptor: a deliberate loud-refusal divergence"`,
			match: func(o op, host, sim outcome) bool {
				return (strings.HasPrefix(o.name, "fdatasync-") || strings.HasPrefix(o.name, "flock(")) &&
					host.Err == "errno:EBADF" && sim.Err == "panic:dst-unsupported"
			},
		},
		{
			key:        "fs-chtimes-omit-missing",
			cite:       `design.md §In-memory deterministic filesystem: "Chtimes on a missing path is ENOENT even with both times zero — Linux's utimensat both-OMIT-succeeds quirk is not reproduced"`,
			applicable: func() bool { return chtimesOmitMissingHostSucceeds() },
			match: func(o op, host, sim outcome) bool {
				return strings.HasPrefix(o.name, "chtimes-omit-missing(") &&
					host.Err == "" && sim.Err == "PathError(chtimes)/errno:ENOENT"
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Fixed coverage ladder: the spec-pinned arms (path shapes, error
// identities, the /dev/null device ladder) plus one deliberate firing of
// every applicable allowlist entry, so stale-entry detection can be a
// hard failure.

func fsCoverageOps() []op {
	var ops []op
	add := func(o op) int { ops = append(ops, o); return -1 }
	slot := -1
	track := func(o op) int { slot++; ops = append(ops, o); return slot }

	// Base tree: dirs with subdirs (nlink arm), files with known modes.
	add(fsMkdir("d1", 0o777))    // umask arm for dirs
	add(fsMkdir("d1/s1", 0o755)) // nlink>2 for d1 on the host
	add(fsMkdir("d1/s2", 0o755))
	add(fsMkdirAll("deep/a/b", 0o755))
	f1 := track(fsOpen("f1", os.O_RDWR|os.O_CREATE, 0o666, true)) // umask arm for files
	add(fsWrite(f1, pat(100, 1)))
	add(fsFstat(f1, true))
	add(fsStat("f1", true))
	add(fsStat("d1", true))
	add(fsStatSys("f1"))

	// Directory handle arms: nlink, fdatasync-vs-fsync, chunked listing,
	// read/seek on a directory descriptor.
	d1 := track(fsOpen("d1", os.O_RDONLY, 0, true))
	add(fsFstatNlink(d1))
	add(fsFdatasync("fdatasync-dir", d1))
	add(fsSync(d1))
	add(fsReaddirnamesChunks(d1, 2))
	add(fsReadOnDir(d1))
	add(fsSeek(d1, 0, io.SeekStart))
	add(fsClose(d1))

	// Ownership/permission/chtimes recorded-gap arms.
	add(fsChownNoop("f1"))
	denied := track(fsOpen("denied", os.O_WRONLY|os.O_CREATE, 0o644, true))
	add(fsClose(denied))
	add(fsChmod("denied", 0))
	add(fsOpenProbe("open-eacces-probe", "denied", os.O_RDONLY))
	add(fsChmod("denied", 0o644))
	add(fsChtimesOmitMissing("missing-for-chtimes"))
	add(fsChtimes("f1"))
	add(fsStatMtime("f1"))

	// Path-shape arms: component-wise resolution, trailing slashes.
	add(fsOpenProbe("open-path", "missing/../f1", os.O_RDONLY))
	add(fsOpenProbe("open-path", "f1/sub", os.O_RDONLY))
	add(fsOpenProbe("open-path", "f1/", os.O_RDONLY))
	add(fsOpen("newfile/", os.O_WRONLY|os.O_CREATE, 0o644, false))
	add(fsMkdir("nd/", 0o755))
	add(fsStat("nd", false))
	add(fsOpenProbe("open-path", "d1/", os.O_RDONLY))

	// Open-mode error arms.
	add(fsOpen("f1", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644, false)) // EEXIST
	add(fsOpen("f1", os.O_RDWR|os.O_EXCL, 0, false))                 // O_EXCL sans O_CREATE: opens
	// An untracked O_CREATE|O_EXCL open of a MISSING node succeeds and
	// mints it; the stat pins the minted node's perm provenance (the
	// umask arm for EXCL creates).
	add(fsOpen("exclmint", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o664, false))
	add(fsStat("exclmint", true))
	add(fsRemove("exclmint"))
	add(fsOpen("d1", os.O_WRONLY, 0, false))             // EISDIR
	add(fsOpen("d1", os.O_RDONLY|os.O_TRUNC, 0, false))  // EISDIR (O_TRUNC on dir)
	add(fsOpenProbe("open-path", "absent", os.O_RDONLY)) // ENOENT

	// Namespace error arms.
	add(fsMkdir("d1", 0o755))            // EEXIST
	add(fsMkdirAll("f1/x", 0o755))       // ENOTDIR
	add(fsRemoveAll("f1/x"))             // ENOTDIR: host Op unlinkat, sim Op removeall (recorded)
	add(fsRemove("d1"))                  // ENOTEMPTY
	add(fsRemove("absent"))              // ENOENT
	add(fsRemoveDot())                   // EINVAL
	add(fsRename("absent", "elsewhere")) // ENOENT
	add(fsRename("f1", "missingdir/x"))  // ENOENT
	add(fsRename("f1", "d1"))            // EEXIST (os.Rename preamble: existing-directory newname)
	add(fsRename("d1", "f1"))            // ENOTDIR (dir over file)
	add(fsMkdir("d3", 0o755))
	add(fsRename("d3", "d1")) // EEXIST (preamble; raw rename(2) would say ENOTEMPTY)
	add(fsRename("f1", "f1")) // self-rename no-op
	add(fsRemove("d3"))
	// Trailing-slash rename arms: rename(2) checks trailing slashes
	// against the SOURCE's type, so a directory renames onto a
	// trailing-slash missing newpath while a file source is ENOTDIR.
	add(fsMkdir("dmv", 0o755))
	add(fsRename("dmv", "dmoved/")) // dir source: succeeds
	add(fsStat("dmoved", false))
	add(fsRename("f1", "fmiss/"))     // file source: ENOTDIR
	add(fsStat("fmiss", false))       // ENOENT: the refused rename minted nothing
	add(fsRename("absent", "fmiss/")) // ENOENT: old final precedes the slash rule
	add(fsRemove("dmoved"))

	// Unlinked-but-open node semantics.
	u := track(fsOpen("unlinkme", os.O_RDWR|os.O_CREATE, 0o644, true))
	add(fsWrite(u, pat(64, 2)))
	add(fsRemove("unlinkme"))
	add(fsStat("unlinkme", false)) // ENOENT
	add(fsPwrite(u, pat(16, 3), 8))
	add(fsPread(u, 64, 0))
	add(fsFstat(u, true))
	add(fsClose(u))

	// O_APPEND, and Linux's pwrite-on-O_APPEND append-anyway quirk.
	base := track(fsOpen("ap", os.O_RDWR|os.O_CREATE, 0o644, true))
	add(fsWrite(base, pat(10, 4)))
	apw := track(fsOpen("ap", os.O_WRONLY|os.O_APPEND, 0, true))
	add(fsSeek(apw, 0, io.SeekStart))
	add(fsWrite(apw, pat(4, 5))) // appends despite the seek
	add(fsPwrite(apw, pat(2, 6), 2))
	add(fsPread(base, 32, 0)) // read-back pins where those bytes landed
	add(fsClose(apw))

	// Sparse extension, truncate arms, and read-shape at EOF.
	add(fsPwrite(base, pat(8, 7), 100))
	add(fsPread(base, 120, 0))
	add(fsTruncateFd(base, 7))
	add(fsPread(base, 16, 0))
	add(fsPread(base, 8, 7))    // read exactly at EOF
	add(fsTruncateFd(base, -1)) // EINVAL
	add(fsTruncateName("ap", 3))
	add(fsStat("ap", false))
	add(fsTruncateName("absent", 3)) // ENOENT
	add(fsSetDeadline(base))         // ErrNoDeadline on regular files

	// Wrong-direction descriptor arms.
	ro := track(fsOpen("f1", os.O_RDONLY, 0, true))
	add(fsWrite(ro, pat(4, 8))) // EBADF
	add(fsSync(ro))             // fsync on O_RDONLY works
	add(fsClose(ro))
	add(fsClose(ro))   // double close: ErrClosed
	add(fsRead(ro, 4)) // use-after-close: ErrClosed
	wo := track(fsOpen("f1", os.O_WRONLY, 0, true))
	add(fsRead(wo, 4)) // EBADF
	add(fsClose(wo))

	// Zero-length and seek arms on a regular file.
	rw := track(fsOpen("f1", os.O_RDWR, 0, true))
	add(fsRead(rw, 0))
	add(fsWrite(rw, nil))
	add(fsSeek(rw, 10, io.SeekStart))
	add(fsSeek(rw, -5, io.SeekCurrent))
	add(fsSeek(rw, 0, io.SeekEnd))
	add(fsSeek(rw, -1, io.SeekStart)) // EINVAL
	add(fsRead(rw, 8))                // read at seek-end: EOF
	add(fsSeek(rw, 2, io.SeekStart))
	add(fsRead(rw, 8))

	// flock ladder: exclusivity, NB conflicts, conversion, release.
	fl2 := track(fsOpen("f1", os.O_RDWR, 0, true))
	add(fsFlock(rw, syscall.LOCK_EX, "LOCK_EX"))
	add(fsFlock(fl2, syscall.LOCK_EX|syscall.LOCK_NB, "LOCK_EX|LOCK_NB")) // EWOULDBLOCK
	add(fsFlock(rw, syscall.LOCK_SH, "LOCK_SH"))                          // conversion
	add(fsFlock(fl2, syscall.LOCK_SH|syscall.LOCK_NB, "LOCK_SH|LOCK_NB"))
	add(fsFlock(fl2, syscall.LOCK_UN, "LOCK_UN"))
	add(fsClose(rw)) // close releases rw's lock
	add(fsFlock(fl2, syscall.LOCK_EX|syscall.LOCK_NB, "LOCK_EX|LOCK_NB"))
	add(fsFlock(fl2, syscall.LOCK_UN, "LOCK_UN"))
	add(fsClose(fl2))

	// Page-cache/mmap identity probe.
	mm := track(fsOpen("mapped", os.O_RDWR|os.O_CREATE, 0o644, true))
	add(fsWrite(mm, pat(4096, 9)))
	add(fsMmapProbe(mm, 4096))
	add(fsClose(mm))

	// O_SYNC write path (commit semantics are unobservable without a
	// crash; the differential arm pins the call shape and counts).
	sy := track(fsOpen("synced", os.O_WRONLY|os.O_CREATE|os.O_SYNC, 0o644, true))
	add(fsWrite(sy, pat(32, 10)))
	add(fsWrite(sy, nil))
	add(fsClose(sy))
	// Raw op on a closed file's stale descriptor: host EBADF, sim
	// fence (the recorded loud-refusal divergence).
	add(fsFdatasync("fdatasync-file", sy))
	// O_DSYNC write path (the data-only per-write barrier, distinct from
	// full O_SYNC; same unobservable-commit caveat).
	dsy := track(fsOpen("dsynced", os.O_WRONLY|os.O_CREATE|syscall.O_DSYNC, 0o644, true))
	add(fsWrite(dsy, pat(24, 14)))
	add(fsWrite(dsy, nil))
	add(fsPwrite(dsy, pat(8, 15), 4))
	add(fsClose(dsy))

	// ReadDir identity.
	add(fsReadDir("d1"))
	add(fsReadDir("f1"))     // ENOTDIR
	add(fsReadDir("absent")) // ENOENT

	// The /dev/null device ladder (non-mutating ops only — see doc.go).
	add(fsDevNullOpen(os.O_RDONLY, false))
	add(fsDevNullOpen(os.O_WRONLY|os.O_TRUNC, false))
	add(fsDevNullOpen(os.O_RDWR|os.O_CREATE, false))
	add(fsDevNullOpen(os.O_RDWR|os.O_CREATE|os.O_EXCL, false)) // EEXIST
	dn := track(fsDevNullOpen(os.O_RDWR, true))
	add(fsRead(dn, 8))           // EOF
	add(fsPread(dn, 8, 5))       // EOF at every offset
	add(fsRead(dn, 0))           // (0, nil)
	add(fsWrite(dn, pat(5, 11))) // discards, full count
	add(fsWrite(dn, nil))        // (0, nil)
	add(fsPwrite(dn, pat(3, 12), 100))
	add(fsSeek(dn, 100, io.SeekStart)) // position pinned at 0
	add(fsSeek(dn, 7, io.SeekEnd))
	add(fsSeek(dn, 5, io.SeekCurrent))
	add(fsSeek(dn, -1, io.SeekStart)) // 0, nil — unlike a regular file
	add(fsRead(dn, 8))                // still EOF after seeks
	add(fsDevNullStat())
	add(dnFstat(dn))
	add(fsTruncateFd(dn, 0))                  // EINVAL
	add(fsDevNullTruncateName(5))             // EINVAL
	add(fsSync(dn))                           // EINVAL
	add(fsFdatasync("fdatasync-devnull", dn)) // EINVAL
	add(fsSetDeadline(dn))                    // ErrNoDeadline
	add(dnMmap(dn))                           // ENODEV
	add(fsClose(dn))
	ro2 := track(fsDevNullOpen(os.O_RDONLY, true))
	add(fsWrite(ro2, pat(1, 13))) // EBADF ahead of the device discard
	add(fsClose(ro2))
	wo2 := track(fsDevNullOpen(os.O_WRONLY, true))
	add(fsRead(wo2, 1)) // EBADF ahead of the device EOF
	add(fsClose(wo2))

	// Close the ladder's remaining open handles: the random grammar
	// inherits the ladder slots as closed placeholders.
	add(fsClose(f1))
	add(fsClose(base))

	return ops
}

// ---------------------------------------------------------------------------
// Random grammar. The generator tracks a model of the tree only to keep
// slot indices meaningful and pick interesting ops — expectations never
// leak into comparison, so a generator-model bug cannot mask a
// divergence (both legs still run the identical sequence).

type fsGen struct {
	rng    *rand.Rand
	ops    []op
	nSlots int
	// Parallel to fsPathPool: 0 missing, 'f' file, 'd' dir.
	kind []byte
	// permCreate[i]: the node's perm is still its create-time request.
	permCreate []bool
	slots      []fsGenSlot
}

type fsGenSlot struct {
	pathIdx  int // pool index the slot's node is linked at; -2 once unlinked/replaced
	readable bool
	writable bool
	isDir    bool
	closed   bool
	// permCreate is the slot's OWN provenance snapshot: the node's perm
	// is still its create-time request. Cleared when a chmod reaches the
	// node through its current path; survives renames and unlinks (the
	// node keeps its mode).
	permCreate bool
}

var fsPathPool = []string{"a", "b", "c", "f1", "f2", "g1", "g2/w", "d1", "d1/x", "d1/y", "d1/sub", "d1/sub/z", "g2", "nope/deep"}

// fsAncestorChain returns the pool indices of rel and every pool
// ancestor of rel, shortest path first.
func fsAncestorChain(idx int) []int {
	rel := fsPathPool[idx]
	var chain []int
	for j, p := range fsPathPool {
		if p == rel || strings.HasPrefix(rel, p+"/") {
			chain = append(chain, j)
		}
	}
	sort.Slice(chain, func(a, b int) bool {
		return len(fsPathPool[chain[a]]) < len(fsPathPool[chain[b]])
	})
	return chain
}

// fsParentIdx returns the pool index of rel's parent, or -1 for the root.
func fsParentIdx(rel string) int {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return -1
	}
	parent := rel[:i]
	for j, p := range fsPathPool {
		if p == parent {
			return j
		}
	}
	return -2 // parent not in pool (never resolvable)
}

func (g *fsGen) parentIsDir(idx int) bool {
	p := fsParentIdx(fsPathPool[idx])
	if p == -1 {
		return true
	}
	return p >= 0 && g.kind[p] == 'd'
}

func (g *fsGen) add(o op)  { g.ops = append(g.ops, o) }
func (g *fsGen) pick() int { return g.rng.IntN(len(fsPathPool)) }
func (g *fsGen) track(o op) int {
	g.ops = append(g.ops, o)
	s := g.nSlots
	g.nSlots++
	return s
}

var fsFilePerms = []os.FileMode{0o600, 0o640, 0o644, 0o664, 0o666, 0o755}
var fsDirPerms = []os.FileMode{0o700, 0o755, 0o777}

func (g *fsGen) step() {
	r := g.rng.IntN(100)
	switch {
	case r < 22: // open
		idx := g.pick()
		rel := fsPathPool[idx]
		perm := fsFilePerms[g.rng.IntN(len(fsFilePerms))]
		flags := []int{
			os.O_RDONLY,
			os.O_RDWR,
			os.O_WRONLY | os.O_CREATE,
			os.O_RDWR | os.O_CREATE,
			os.O_RDWR | os.O_CREATE | os.O_EXCL,
			os.O_WRONLY | os.O_CREATE | os.O_TRUNC,
			os.O_WRONLY | os.O_CREATE | os.O_APPEND,
			os.O_RDWR | os.O_TRUNC,
			os.O_WRONLY | os.O_CREATE | os.O_SYNC,
			os.O_WRONLY | os.O_CREATE | syscall.O_DSYNC,
		}
		flag := flags[g.rng.IntN(len(flags))]
		// Model: does this open succeed and yield a useful slot?
		ok := false
		if g.parentIsDir(idx) {
			switch g.kind[idx] {
			case 'f':
				ok = flag&os.O_EXCL == 0
			case 'd':
				ok = flag&0x3 == os.O_RDONLY && flag&(os.O_TRUNC|os.O_CREATE|os.O_EXCL) == 0
			case 0:
				ok = flag&os.O_CREATE != 0
			}
		}
		if ok && g.kind[idx] != 'd' && g.rng.IntN(4) > 0 {
			g.track(fsOpen(rel, flag, perm, true))
			if g.kind[idx] == 0 {
				g.kind[idx] = 'f'
				g.permCreate[idx] = true
			}
			acc := flag & 0x3
			g.slots = append(g.slots, fsGenSlot{
				pathIdx:    idx,
				readable:   acc == os.O_RDONLY || acc == os.O_RDWR,
				writable:   acc == os.O_WRONLY || acc == os.O_RDWR,
				permCreate: g.permCreate[idx],
			})
		} else if ok && g.kind[idx] == 'd' {
			g.track(fsOpen(rel, flag, perm, true))
			g.slots = append(g.slots, fsGenSlot{pathIdx: idx, readable: true, isDir: true, permCreate: g.permCreate[idx]})
		} else {
			g.add(fsOpen(rel, flag, perm, false))
			if g.kind[idx] == 0 && g.parentIsDir(idx) && flag&os.O_CREATE != 0 {
				// Untracked create still mints the node — O_EXCL included:
				// EXCL refuses only an EXISTING node, and this arm is
				// missing-only (kind 0). Excluding EXCL here mislabeled
				// the minted node's perm provenance, so a later stat's
				// legitimate umask divergence went unallowlisted
				// (surfaced by the widened sweep, fs seed 13).
				g.kind[idx] = 'f'
				g.permCreate[idx] = true
			}
		}
	case r < 40: // slot I/O
		if len(g.slots) == 0 {
			idx := g.pick()
			g.add(fsStat(fsPathPool[idx], g.kind[idx] != 0 && g.permCreate[idx]))
			return
		}
		s := g.rng.IntN(len(g.slots))
		sl := g.slots[s]
		if sl.isDir && !sl.closed {
			// Directory descriptors get the reject-class read probe
			// and position-0 seeks only: dir read errnos and nonzero
			// dir offsets are filesystem-defined on the host.
			switch g.rng.IntN(4) {
			case 0:
				g.add(fsReadOnDir(s))
			case 1:
				// Seek on a consumed directory handle would race the
				// os-level dirinfo cache against the sim cursor (and
				// nonzero dir offsets are fs-defined); dir handles get
				// fsync instead.
				g.add(fsSync(s))
			case 2:
				g.add(fsFstat(s, sl.permCreate))
			case 3:
				g.add(fsReaddirnamesChunks(s, 1+g.rng.IntN(3)))
			}
			return
		}
		switch g.rng.IntN(7) {
		case 0:
			g.add(fsWrite(s, pat(1+g.rng.IntN(2048), byte(g.rng.IntN(256)))))
		case 1:
			g.add(fsRead(s, 1+g.rng.IntN(2048)))
		case 2:
			g.add(fsPwrite(s, pat(1+g.rng.IntN(512), byte(g.rng.IntN(256))), int64(g.rng.IntN(4096))))
		case 3:
			g.add(fsPread(s, 1+g.rng.IntN(512), int64(g.rng.IntN(4096))))
		case 4:
			whences := []int{io.SeekStart, io.SeekCurrent, io.SeekEnd}
			g.add(fsSeek(s, int64(g.rng.IntN(2048)), whences[g.rng.IntN(3)]))
		case 5:
			g.add(fsFstat(s, !sl.closed && sl.permCreate))
		case 6:
			// Multi-append burst: several consecutive sequential writes,
			// sized to cross page boundaries — the growth shape the crash
			// tear's per-page-advanced size draw ranges over (the crash leg
			// itself is outside this harness; the burst keeps the
			// differential grammar's input surface in step with it).
			n := 3 + g.rng.IntN(4)
			for i := 0; i < n; i++ {
				g.add(fsWrite(s, pat(1024+g.rng.IntN(1536), byte(g.rng.IntN(256)))))
			}
		}
	case r < 46: // close (sometimes double)
		if len(g.slots) == 0 {
			return
		}
		s := g.rng.IntN(len(g.slots))
		g.add(fsClose(s))
		g.slots[s].closed = true
	case r < 54: // mkdir/mkdirall
		idx := g.pick()
		rel := fsPathPool[idx]
		perm := fsDirPerms[g.rng.IntN(len(fsDirPerms))]
		if g.rng.IntN(2) == 0 || fsParentIdx(rel) == -2 {
			// Plain Mkdir for paths whose parent is outside the pool
			// ("nope/deep"): MkdirAll would mint the non-pool parent
			// and destroy the pool's permanently-unresolvable arm.
			g.add(fsMkdir(rel, perm))
			if g.kind[idx] == 0 && g.parentIsDir(idx) {
				g.kind[idx] = 'd'
				g.permCreate[idx] = true
			}
		} else {
			g.add(fsMkdirAll(rel, perm))
			// MkdirAll mints the whole ancestor chain: mark every pool
			// prefix of rel (shortest first) that the walk can create.
			for _, j := range fsAncestorChain(idx) {
				if g.kind[j] == 0 && g.parentIsDir(j) {
					g.kind[j] = 'd'
					g.permCreate[j] = true
				}
			}
		}
	case r < 62: // remove / removeall
		idx := g.pick()
		rel := fsPathPool[idx]
		if g.kind[idx] == 'd' || g.rng.IntN(3) == 0 {
			g.add(fsRemoveAll(rel))
			for j := range fsPathPool {
				if j == idx || strings.HasPrefix(fsPathPool[j], rel+"/") {
					if g.kind[j] != 0 {
						g.unlinkSlotsAt(j)
					}
				}
			}
			g.clearSubtree(idx)
		} else {
			g.add(fsRemove(rel))
			if g.kind[idx] == 'f' {
				g.unlinkSlotsAt(idx)
				g.kind[idx] = 0
			}
		}
	case r < 70: // rename between pool entries (dir sources and trailing-slash variants included)
		src := g.pick()
		dst := g.pick()
		oldRel, newRel := fsPathPool[src], fsPathPool[dst]
		if g.rng.IntN(5) == 0 {
			oldRel += "/"
		}
		if g.rng.IntN(5) == 0 {
			newRel += "/"
		}
		slashOld := strings.HasSuffix(oldRel, "/")
		slashNew := strings.HasSuffix(newRel, "/")
		g.add(fsRename(oldRel, newRel))
		// Model the namespace effect. os.Rename succeeds when the source
		// exists, the destination parent walk succeeds, and:
		//  - file source: no trailing slash on either argument (the
		//    source-type slash rule) and the destination is not an
		//    existing directory (the preamble's EEXIST);
		//  - dir source: the destination is missing (an existing dir is
		//    the preamble's EEXIST, an existing file ENOTDIR) and not
		//    inside the source (EINVAL). Trailing slashes are legal.
		switch g.kind[src] {
		case 'f':
			if src != dst && !slashOld && !slashNew && g.kind[dst] != 'd' && g.parentIsDir(dst) {
				if g.kind[dst] != 0 {
					g.unlinkSlotsAt(dst) // the replaced node leaves the namespace
				}
				for s := range g.slots {
					if g.slots[s].pathIdx == src {
						g.slots[s].pathIdx = dst // open handles follow the node
					}
				}
				g.kind[dst] = 'f'
				g.permCreate[dst] = g.permCreate[src]
				g.kind[src] = 0
			}
		case 'd':
			dstInsideSrc := src == dst || strings.HasPrefix(fsPathPool[dst], fsPathPool[src]+"/")
			if g.kind[dst] == 0 && g.parentIsDir(dst) && !dstInsideSrc {
				g.moveSubtree(src, dst)
			}
		}
	case r < 78: // stat / readdir
		idx := g.pick()
		if g.rng.IntN(3) == 0 && g.kind[idx] == 'd' {
			g.add(fsReadDir(fsPathPool[idx]))
		} else {
			g.add(fsStat(fsPathPool[idx], g.kind[idx] != 0 && g.permCreate[idx]))
		}
	case r < 84: // chmod
		idx := g.pick()
		var perm os.FileMode
		if g.kind[idx] == 'd' {
			perm = fsDirPerms[g.rng.IntN(len(fsDirPerms))]
		} else {
			perm = fsFilePerms[g.rng.IntN(len(fsFilePerms))]
		}
		g.add(fsChmod(fsPathPool[idx], perm))
		if g.kind[idx] != 0 {
			g.permCreate[idx] = false
			// The chmod reached the node currently linked at this path:
			// clear the provenance snapshot of every handle on it.
			for s := range g.slots {
				if g.slots[s].pathIdx == idx {
					g.slots[s].permCreate = false
				}
			}
		}
	case r < 90: // truncate
		idx := g.pick()
		if g.rng.IntN(2) == 0 && len(g.slots) > 0 {
			s := g.rng.IntN(len(g.slots))
			g.add(fsTruncateFd(s, int64(g.rng.IntN(512))))
		} else {
			g.add(fsTruncateName(fsPathPool[idx], int64(g.rng.IntN(512))))
		}
	case r < 96: // sync family
		if len(g.slots) == 0 {
			return
		}
		s := g.rng.IntN(len(g.slots))
		if g.rng.IntN(2) == 0 {
			g.add(fsSync(s))
		} else if g.slots[s].isDir {
			// Directory fdatasync succeeds on both legs (it commits entry
			// durability exactly as directory fsync does — host-verified).
			g.add(fsFdatasync("fdatasync-dir", s))
		} else {
			g.add(fsFdatasync("fdatasync-file", s))
		}
	default: // chtimes + mtime read-back
		idx := g.pick()
		g.add(fsChtimes(fsPathPool[idx]))
		g.add(fsStatMtime(fsPathPool[idx]))
	}
}

// moveSubtree applies a successful directory rename to the generator's
// pool model: every pool path at or under the source maps to the
// corresponding pool path under the destination when the pool has one
// (pool paths are the only nodes the grammar ever creates); a node whose
// new location has no pool path becomes unreachable to path-addressed
// ops, so its handles freeze like unlinked ones.
func (g *fsGen) moveSubtree(src, dst int) {
	srcPath, dstPath := fsPathPool[src], fsPathPool[dst]
	type mv struct {
		from, to int // pool indices; to == -1 when the new path is off-pool
	}
	var moves []mv
	for j, p := range fsPathPool {
		if g.kind[j] == 0 || (p != srcPath && !strings.HasPrefix(p, srcPath+"/")) {
			continue
		}
		toPath := dstPath + p[len(srcPath):]
		to := -1
		for k, q := range fsPathPool {
			if q == toPath {
				to = k
				break
			}
		}
		moves = append(moves, mv{j, to})
	}
	// Snapshot the moving nodes, clear the sources, then land the
	// destinations (source and destination subtrees are disjoint — the
	// caller refused dst-inside-src, and a live source child implies a
	// live source, so src-inside-dst would contradict a missing dst).
	kinds := make(map[int]byte, len(moves))
	perms := make(map[int]bool, len(moves))
	for _, m := range moves {
		kinds[m.from] = g.kind[m.from]
		perms[m.from] = g.permCreate[m.from]
		g.kind[m.from] = 0
	}
	for _, m := range moves {
		for s := range g.slots {
			if g.slots[s].pathIdx == m.from {
				if m.to >= 0 {
					g.slots[s].pathIdx = m.to
				} else {
					g.slots[s].pathIdx = -2
				}
			}
		}
		if m.to >= 0 {
			g.kind[m.to] = kinds[m.from]
			g.permCreate[m.to] = perms[m.from]
		}
	}
}

// unlinkSlotsAt marks every open handle on the node linked at pool
// index idx as unlinked: path-addressed ops can no longer reach it, so
// its provenance snapshot freezes.
func (g *fsGen) unlinkSlotsAt(idx int) {
	for s := range g.slots {
		if g.slots[s].pathIdx == idx {
			g.slots[s].pathIdx = -2
		}
	}
}

func (g *fsGen) clearSubtree(idx int) {
	prefix := fsPathPool[idx] + "/"
	g.kind[idx] = 0
	for j, p := range fsPathPool {
		if strings.HasPrefix(p, prefix) {
			g.kind[j] = 0
		}
	}
}

// genFSOps generates the filesystem sequence for one seed: the fixed
// coverage ladder, then n random grammar steps.
func genFSOps(seed uint64, n int) []op {
	g := &fsGen{
		rng:        rand.New(rand.NewPCG(seed, 0xF5)),
		kind:       make([]byte, len(fsPathPool)),
		permCreate: make([]bool, len(fsPathPool)),
	}
	g.ops = fsCoverageOps()
	// The ladder's tracked slots occupy the low indices (all closed by
	// the ladder's end); random slots start after them.
	g.nSlots = fsCoverageSlotCount
	for i := 0; i < g.nSlots; i++ {
		g.slots = append(g.slots, fsGenSlot{pathIdx: -1, closed: true})
	}
	// Pool nodes the ladder leaves behind, with their perm provenance.
	for i, p := range fsPathPool {
		switch p {
		case "f1":
			g.kind[i], g.permCreate[i] = 'f', true // created 0666, never chmod'd
		case "d1":
			g.kind[i], g.permCreate[i] = 'd', true // created 0777, never chmod'd
		}
	}
	for range n {
		g.step()
	}
	return g.ops
}

// fsCoverageSlotCount is the number of tracked slots fsCoverageOps
// creates (pinned by TestDSTConformanceGeneratorDeterminism's ladder
// audit below).
var fsCoverageSlotCount = func() int {
	n := 0
	for _, o := range fsCoverageOps() {
		if strings.Contains(o.name, "track=true") {
			n++
		}
	}
	return n
}()

// ---------------------------------------------------------------------------
// The domain test.

func TestDSTConformanceFS(t *testing.T) {
	allow := fsAllowlist()
	fired := make(map[string]int)
	for _, seed := range sweepSeeds(t) {
		ops := genFSOps(seed, 300)
		host := runOpsHost(t, ops)
		sim := runOpsSim(t, seed, ops)
		if d := diffOutcomes(ops, host, sim, allow, fired); d != nil {
			reportDivergence(t, "fs", seed, ops, d)
			return
		}
	}
	checkAllowlistCoverage(t, allow, fired)
}
