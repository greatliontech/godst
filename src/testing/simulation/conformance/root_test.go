// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package conformance

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// os.Root ops. Every rooted method reports root-relative PathError.Path and
// LinkError.Old/New values, so outcome State records those error paths in
// addition to errClass's Op + sentinel/errno — catching component-cleaning or
// whole-path wrapping drift alongside errno divergences.

func slotRoot(w *world, rslot int) *os.Root {
	if rslot < 0 || rslot >= len(w.roots) {
		return nil
	}
	return w.roots[rslot]
}

func rootErrState(w *world, err error) string {
	if err == nil {
		return ""
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return "old=" + le.Old + " new=" + le.New
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return "path=" + strings.TrimPrefix(pe.Path, w.root+"/")
	}
	return ""
}

func rootOutcome(w *world, err error) outcome {
	return outcome{Err: errClass(err), N: -1, State: rootErrState(w, err)}
}

// rootOpenWorld opens w.path(rel) via os.OpenRoot.
func rootOpenWorld(rel string, track bool) op {
	return op{
		name: fmt.Sprintf("root-open-world(%q) trackRoot=%v", rel, track),
		run: func(w *world) outcome {
			r, err := os.OpenRoot(w.path(rel))
			if track {
				w.roots = append(w.roots, r)
			} else if r != nil {
				r.Close()
			}
			return rootOutcome(w, err)
		},
	}
}

func rootOpenInRoot(dirRel, name string) op {
	return op{
		name: fmt.Sprintf("root-openinroot(%q, %q)", dirRel, name),
		run: func(w *world) outcome {
			f, err := os.OpenInRoot(w.path(dirRel), name)
			if f != nil {
				f.Close()
			}
			return rootOutcome(w, err)
		},
	}
}

func rootOpenSub(rslot int, name string, track bool) op {
	return op{
		name: fmt.Sprintf("root-openroot(root %d, %q) trackRoot=%v", rslot, name, track),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			sub, err := r.OpenRoot(name)
			if track {
				w.roots = append(w.roots, sub)
			} else if sub != nil {
				sub.Close()
			}
			return rootOutcome(w, err)
		},
	}
}

func rootClose(rslot int) op {
	return op{
		name: fmt.Sprintf("root-close(root %d)", rslot),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(r.Close()), N: -1}
		},
	}
}

func rootOpenFile(rslot int, name string, flag int, perm os.FileMode, trackFile bool) op {
	return op{
		name: fmt.Sprintf("root-openfile(root %d, %q, %s, %04o) trackFile=%v", rslot, name, flagName(flag), perm, trackFile),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			f, err := r.OpenFile(name, flag, perm)
			if trackFile {
				w.files = append(w.files, f)
			} else if f != nil {
				f.Close()
			}
			return rootOutcome(w, err)
		},
	}
}

func rootOpenProbe(label string, rslot int, name string, flag int) op {
	return op{
		name: fmt.Sprintf("%s(root %d, %q, %s)", label, rslot, name, flagName(flag)),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			f, err := r.OpenFile(name, flag, 0)
			if f != nil {
				f.Close()
			}
			return rootOutcome(w, err)
		},
	}
}

func rootCreate(rslot int, name string, trackFile bool) op {
	return op{
		name: fmt.Sprintf("root-create(root %d, %q) trackFile=%v", rslot, name, trackFile),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			f, err := r.Create(name)
			if trackFile {
				w.files = append(w.files, f)
			} else if f != nil {
				f.Close()
			}
			return rootOutcome(w, err)
		},
	}
}

func rootWriteFile(rslot int, name string, payload []byte, perm os.FileMode) op {
	return op{
		name: fmt.Sprintf("root-writefile(root %d, %q, %d bytes, %04o)", rslot, name, len(payload), perm),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return rootOutcome(w, r.WriteFile(name, payload, perm))
		},
	}
}

func rootReadFile(rslot int, name string) op {
	return op{
		name: fmt.Sprintf("root-readfile(root %d, %q)", rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			data, err := r.ReadFile(name)
			if err != nil {
				return rootOutcome(w, err)
			}
			return outcome{N: len(data), State: contentHash(data)}
		},
	}
}

func rootMkdir(rslot int, name string, perm os.FileMode) op {
	return op{
		name: fmt.Sprintf("root-mkdir(root %d, %q, %04o)", rslot, name, perm),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return rootOutcome(w, r.Mkdir(name, perm))
		},
	}
}

func rootMkdirAll(rslot int, name string, perm os.FileMode) op {
	return op{
		name: fmt.Sprintf("root-mkdirall(root %d, %q, %04o)", rslot, name, perm),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return rootOutcome(w, r.MkdirAll(name, perm))
		},
	}
}

func rootRemove(rslot int, name string) op {
	return op{
		name: fmt.Sprintf("root-remove(root %d, %q)", rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return rootOutcome(w, r.Remove(name))
		},
	}
}

func rootRemoveAll(rslot int, name string) op {
	return op{
		name: fmt.Sprintf("root-removeall(root %d, %q)", rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return rootOutcome(w, r.RemoveAll(name))
		},
	}
}

func rootRename(rslot int, oldname, newname string) op {
	return op{
		name: fmt.Sprintf("root-rename(root %d, %q, %q)", rslot, oldname, newname),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return rootOutcome(w, r.Rename(oldname, newname))
		},
	}
}

func rootLink(rslot int, oldname, newname string) op {
	return op{
		name: fmt.Sprintf("root-link(root %d, %q, %q)", rslot, oldname, newname),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return rootOutcome(w, r.Link(oldname, newname))
		},
	}
}

func rootSameFile(rslot int, a, b string) op {
	return op{
		name: fmt.Sprintf("root-samefile(root %d, %q, %q)", rslot, a, b),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			fia, err := r.Stat(a)
			if err != nil {
				return rootOutcome(w, err)
			}
			fib, err := r.Stat(b)
			if err != nil {
				return rootOutcome(w, err)
			}
			return outcome{N: -1, State: fmt.Sprintf("same=%v", os.SameFile(fia, fib))}
		},
	}
}

func rootStat(rslot int, name string, lstat, fromCreate bool) op {
	fn := "root-stat"
	if lstat {
		fn = "root-lstat"
	}
	return op{
		name:           fmt.Sprintf("%s(root %d, %q) fromCreate=%v", fn, rslot, name, fromCreate),
		permFromCreate: fromCreate,
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			var (
				fi  os.FileInfo
				err error
			)
			if lstat {
				fi, err = r.Lstat(name)
			} else {
				fi, err = r.Stat(name)
			}
			if err != nil {
				return rootOutcome(w, err)
			}
			return outcome{N: -1, State: "name=" + fi.Name() + " " + statState(fi)}
		},
	}
}

func rootStatSys(rslot int, name string) op {
	return op{
		name: fmt.Sprintf("root-stat-sys(root %d, %q)", rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			fi, err := r.Stat(name)
			if err != nil {
				return rootOutcome(w, err)
			}
			if fi.Sys() == nil {
				return outcome{N: -1, State: "sys=nil"}
			}
			return outcome{N: -1, State: "sys=stat"}
		},
	}
}

func rootFSReadDir(rslot int, name string) op {
	return op{
		name: fmt.Sprintf("root-fs-readdir(root %d, %q)", rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			ents, err := fs.ReadDir(r.FS(), name)
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

func rootFSReadFile(rslot int, name string) op {
	return op{
		name: fmt.Sprintf("root-fs-readfile(root %d, %q)", rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			data, err := fs.ReadFile(r.FS(), name)
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			return outcome{N: len(data), State: contentHash(data)}
		},
	}
}

func rootChmod(rslot int, name string, perm os.FileMode) op {
	return op{
		name: fmt.Sprintf("root-chmod(root %d, %q, %04o)", rslot, name, perm),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return rootOutcome(w, r.Chmod(name, perm))
		},
	}
}

var fixedRootMtime = time.Unix(1_700_000_000, 123_456_000).UTC()

func rootChtimes(rslot int, name string) op {
	return op{
		name: fmt.Sprintf("root-chtimes(root %d, %q)", rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return rootOutcome(w, r.Chtimes(name, fixedRootMtime, fixedRootMtime))
		},
	}
}

func rootChtimesOmitMissing(rslot int, name string) op {
	return op{
		name: fmt.Sprintf("root-chtimes-omit-missing(root %d, %q)", rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return rootOutcome(w, r.Chtimes(name, time.Time{}, time.Time{}))
		},
	}
}

func rootStatMtime(rslot int, name string) op {
	return op{
		name: fmt.Sprintf("root-stat-mtime(root %d, %q)", rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			fi, err := r.Stat(name)
			if err != nil {
				return rootOutcome(w, err)
			}
			return outcome{N: -1, State: fmt.Sprintf("mtime=%d", fi.ModTime().UnixNano())}
		},
	}
}

func rootChown(rslot int, name string, lchown bool) op {
	fn := "root-chown"
	if lchown {
		fn = "root-lchown"
	}
	return op{
		name: fmt.Sprintf("%s(root %d, %q)", fn, rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			var err error
			if lchown {
				err = r.Lchown(name, -1, -1)
			} else {
				err = r.Chown(name, -1, -1)
			}
			return rootOutcome(w, err)
		},
	}
}

func rootSymlink(rslot int, oldname, newname string) op {
	return op{
		name: fmt.Sprintf("root-symlink(root %d, %q, %q)", rslot, oldname, newname),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			err := r.Symlink(oldname, newname)
			if err == nil {
				// Clean up the host-created symlink immediately so both legs'
				// trees remain identical after this self-contained probe.
				_ = r.Remove(newname)
			}
			return rootOutcome(w, err)
		},
	}
}

func rootReadlink(rslot int, name string) op {
	return op{
		name: fmt.Sprintf("root-readlink(root %d, %q)", rslot, name),
		run: func(w *world) outcome {
			r := slotRoot(w, rslot)
			if r == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			_, err := r.Readlink(name)
			return rootOutcome(w, err)
		},
	}
}

// ---------------------------------------------------------------------------
// The os.Root allowlist: exactly the divergences docs/dst/design.md records.

func rootAllowlist() []allowEntry {
	return []allowEntry{
		{
			key:        "root-create-umask",
			cite:       `design.md §In-memory deterministic filesystem: "umask is not modeled (recorded stance): created files and directories store the requested mode verbatim"`,
			applicable: func() bool { return hostUmask&0o777 != 0 },
			match: func(o op, host, sim outcome) bool {
				if !strings.HasPrefix(o.name, "root-") || !o.permFromCreate || host.Err != "" || sim.Err != "" {
					return false
				}
				hp, hRest, hok := parseStatePerm(host.State)
				sp, sRest, sok := parseStatePerm(sim.State)
				return hok && sok && hRest == sRest && host.N == sim.N &&
					hp == sp&^hostUmask && hp != sp
			},
		},
		{
			key:  "root-sys-nil",
			cite: `design.md §Roadmap Landed (Disk): "Sys() is nil" (no host stat behind a simulated node)`,
			match: func(o op, host, sim outcome) bool {
				return strings.HasPrefix(o.name, "root-stat-sys(") &&
					host.Err == "" && sim.Err == "" &&
					host.State == "sys=stat" && sim.State == "sys=nil"
			},
		},
		{
			key:        "root-perm-not-enforced",
			cite:       `design.md §In-memory deterministic filesystem: "Permission bits are stored and reported but not enforced in the base model (no simulated credential checks)"`,
			applicable: func() bool { return os.Geteuid() != 0 },
			match: func(o op, host, sim outcome) bool {
				return strings.HasPrefix(o.name, "root-open-eacces-probe(") &&
					host.Err == "PathError(openat)/errno:EACCES" && sim.Err == "" &&
					strings.HasPrefix(host.State, "path=") && sim.State == ""
			},
		},
		{
			key:  "root-chown-fenced",
			cite: `design.md §In-memory deterministic filesystem: "ownership is not represented at all — Chown/Lchown and File.Chown stay fenced"`,
			match: func(o op, host, sim outcome) bool {
				if strings.HasPrefix(o.name, "root-chown(") {
					return host.Err == "" && sim.Err == "PathError(chownat)/dst-unsupported" &&
						host.State == "" && strings.HasPrefix(sim.State, "path=")
				}
				if strings.HasPrefix(o.name, "root-lchown(") {
					return host.Err == "" && sim.Err == "PathError(lchownat)/dst-unsupported" &&
						host.State == "" && strings.HasPrefix(sim.State, "path=")
				}
				return false
			},
		},
		{
			key:  "root-symlink-fenced",
			cite: `design.md §In-memory deterministic filesystem: "Symbolic links ... are unmodeled and fenced"`,
			match: func(o op, host, sim outcome) bool {
				if strings.HasPrefix(o.name, "root-symlink(") {
					return host.Err == "" && sim.Err == "LinkError(symlinkat)/dst-unsupported" &&
						host.State == "" && strings.HasPrefix(sim.State, "old=")
				}
				if strings.HasPrefix(o.name, "root-readlink(") {
					return host.Err == "PathError(readlinkat)/errno:EINVAL" &&
						sim.Err == "PathError(readlinkat)/dst-unsupported" &&
						host.State == sim.State && strings.HasPrefix(sim.State, "path=")
				}
				return false
			},
		},
		{
			key:        "root-chtimes-omit-missing",
			cite:       `design.md §In-memory deterministic filesystem: "Chtimes on a missing path is ENOENT even with both times zero — Linux's utimensat both-OMIT-succeeds quirk is not reproduced"`,
			applicable: func() bool { return chtimesOmitMissingHostSucceeds() },
			match: func(o op, host, sim outcome) bool {
				return strings.HasPrefix(o.name, "root-chtimes-omit-missing(") &&
					host.Err == "" && sim.Err == "PathError(chtimesat)/errno:ENOENT" &&
					host.State == "" && strings.HasPrefix(sim.State, "path=")
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Fixed coverage ladder: path containment, Mkdir/MkdirAll component cleaning,
// Rename/Link/Remove/RemoveAll ladders, sub-roots + moved/unlinked roots,
// closed-root errors, and every allowlist entry.

func rootCoverageOps() []op {
	var ops []op
	add := func(o ...op) { ops = append(ops, o...) }

	// Seed a named directory "jail" inside w.root (Chmodded to 0o755 so "." and
	// "d1/.." Stat probes compare exact permissions alongside filepath.Base(name)),
	// plus a sibling "outside" file that escapes must never reach.
	add(
		fsMkdir("jail", 0o777),
		fsChmod("jail", 0o755),
		fsOpen("outside", os.O_CREATE|os.O_WRONLY, 0o644, true), // file slot 0
		fsWrite(0, []byte("secret")),
		fsClose(0),

		// OpenRoot errors and world root creation (root slot 0).
		rootOpenWorld("missing-jail", false),
		rootOpenWorld("outside", false),
		rootOpenWorld("jail", true), // root slot 0

		// Closed Root behavior (root slot 1).
		rootOpenWorld("jail", true), // root slot 1
		rootClose(1),
		rootClose(1),
		rootOpenFile(1, "x", os.O_RDONLY, 0, false),
		rootMkdir(1, "x", 0o755),
		rootMkdirAll(1, "x/y", 0o755),
		rootStat(1, ".", false, false),
		rootRemove(1, "x"),
		rootRemoveAll(1, "x"),
		rootRename(1, "a", "b"),
		rootLink(1, "a", "b"),
		rootChmod(1, "x", 0o644),
		rootChtimes(1, "x"),
		rootOpenSub(1, ".", false),

		// Populate baseline files and directories in root 0.
		rootWriteFile(0, "f1", pat(24, 0x11), 0o666),
		rootReadFile(0, "f1"),
		rootMkdir(0, "d1", 0o777),
		rootStat(0, ".", false, false),
		rootStat(0, "f1", false, true),  // fires root-create-umask
		rootStat(0, "d1", true, true),   // Lstat + umask
		rootStatSys(0, "f1"),            // fires root-sys-nil
		rootOpenInRoot("jail", "f1"),
		rootOpenInRoot("jail", "../outside"),
		rootOpenInRoot("missing-jail", "f1"),

		// Invalid permission bits (rejected before syscall on both legs).
		rootOpenFile(0, "badperm", os.O_CREATE|os.O_WRONLY, 0o1644, false),
		rootMkdir(0, "badperm", 0o1755),
		rootMkdirAll(0, "badperm/sub", 0o1755),

		// Path containment and dot/slash resolution shapes.
		rootOpenFile(0, "", os.O_RDONLY, 0, false),
		rootOpenFile(0, "/", os.O_RDONLY, 0, false),
		rootOpenFile(0, "/outside", os.O_RDONLY, 0, false),
		rootOpenFile(0, "..", os.O_RDONLY, 0, false),
		rootOpenFile(0, "../outside", os.O_RDONLY, 0, false),
		rootOpenFile(0, "d1/../../outside", os.O_RDONLY, 0, false),
		rootReadFile(0, "./f1"),
		rootReadFile(0, "d1/../f1"),
		rootStat(0, "d1/.", false, true),
		rootStat(0, "d1/..", false, false),
		rootOpenSub(0, "f1", false),
		rootOpenSub(0, "f1/", false),
		rootOpenFile(0, "f1/", os.O_RDONLY, 0, false),
		rootOpenFile(0, "missing/", os.O_CREATE|os.O_WRONLY, 0o644, false),
		rootOpenFile(0, "f1", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644, false),
		rootOpenFile(0, "d1", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644, false),
		rootOpenFile(0, "d1", os.O_WRONLY, 0, false),

		// Mkdir / MkdirAll component-cleaning and error-identity ladder.
		rootMkdir(0, ".", 0o755),
		rootMkdirAll(0, ".", 0o755),
		rootMkdir(0, "d1/", 0o755),
		rootMkdirAll(0, "d1/", 0o755),
		rootMkdir(0, "f1", 0o755),
		rootMkdir(0, "f1/", 0o755),
		rootMkdirAll(0, "f1", 0o755),
		rootMkdirAll(0, "f1/", 0o755),
		rootMkdirAll(0, "f1/sub", 0o755),
		rootMkdirAll(0, "./d1//deep/", 0o755),
		rootWriteFile(0, "d1/deep/f2", pat(8, 0x22), 0o644),
		rootMkdirAll(0, "d1/./deep//f2", 0o755),
		rootMkdirAll(0, "d1/./deep//f2/", 0o755),
		rootMkdirAll(0, "d1/./deep//f2/child", 0o755),
		rootMkdirAll(0, "d1/deep/../../f1", 0o755),
		rootMkdirAll(0, "../esc", 0o755),
		rootRemove(0, "d1/deep/f2"),
		rootRemove(0, "d1/deep"),

		// Rename ladder (rooted preamble + terminal dot/slash rules).
		rootMkdir(0, "rdir", 0o755),
		rootWriteFile(0, "rsrc", []byte("ren"), 0o644),
		rootRename(0, "rsrc/", "rdst"),
		rootRename(0, "rsrc", "rdst/"),
		rootRename(0, "rsrc", "rdir"),
		rootRename(0, "rdir", "rdir"),
		rootRename(0, "rdir/.", "rdir/."),
		rootRename(0, "rdir", "rdir/."),
		rootRename(0, "rdir/.", "rdir"),
		rootRename(0, "rdir", "rdir/sub"),
		rootRename(0, "rdir", "f1"),
		rootRename(0, "../outside", "stolen"),
		rootRename(0, "rsrc", "../escaped"),
		rootRename(0, "rsrc", "rdst"),
		rootReadFile(0, "rdst"),
		rootRemove(0, "rdst"),
		rootRemove(0, "rdir"),

		// Link ladder (hardlink identity + rooted slashed-file ENOTDIR).
		rootLink(0, "f1", "f1_link"),
		rootSameFile(0, "f1", "f1_link"),
		rootLink(0, "f1", "f1_link"),
		rootLink(0, "f1", "f1_link/"),
		rootLink(0, "f1", "missing_link/"),
		rootLink(0, "d1", "f1_link"),
		rootLink(0, "d1", "dir_link"),
		rootLink(0, "../outside", "esc_link"),
		rootLink(0, "no_src", "../outside"),
		rootLink(0, "no_src", "f1/sub"),
		rootRemove(0, "f1_link"),

		// Sub-root containment, directory-rename tracking, RemoveAll("d/.."),
		// and unlinked sub-root creation refusal.
		rootMkdirAll(0, "sub/inner", 0o755),
		rootWriteFile(0, "sub/inner/item", []byte("in-sub"), 0o644),
		rootOpenSub(0, "sub", true), // root slot 2
		rootOpenFile(2, "../f1", os.O_RDONLY, 0, false),
		rootReadFile(2, "inner/item"),
		rootRename(0, "sub", "sub_moved"),
		rootReadFile(2, "inner/item"),       // open Root follows moved directory
		rootRemoveAll(2, "inner/.."),        // empties sub_moved, returns EINVAL
		rootFSReadDir(2, "."),               // now empty
		rootRemove(2, "."),                  // EINVAL
		rootRemoveAll(2, "."),               // EINVAL
		rootRemoveAll(2, "inner/."),         // EINVAL
		rootRemove(0, "sub_moved"),          // unlinks the directory backing root 2
		rootCreate(2, "ghost", false),       // ENOENT in unlinked root
		rootMkdir(2, "ghost", 0o755),        // ENOENT in unlinked root
		rootMkdirAll(2, "ghost/sub", 0o755), // ENOENT in unlinked root
		rootClose(2),

		// Root.FS() view.
		rootFSReadDir(0, "."),
		rootFSReadFile(0, "f1"),

		// Deliberate firings of remaining allowlist entries.
		rootCreate(0, "perm0", false),
		rootChmod(0, "perm0", 0o000),
		rootOpenProbe("root-open-eacces-probe", 0, "perm0", os.O_RDONLY), // fires root-perm-not-enforced
		rootChmod(0, "perm0", 0o644),
		rootRemove(0, "perm0"),

		rootChown(0, "f1", false),                    // fires root-chown-fenced
		rootChown(0, "f1", true),                     // Lchown arm
		rootSymlink(0, "f1", "sym_probe"),            // fires root-symlink-fenced (Symlink)
		rootReadlink(0, "f1"),                        // fires root-symlink-fenced (Readlink)
		rootChtimesOmitMissing(0, "no-such-chtimes"), // fires root-chtimes-omit-missing

		// File-handle slot I/O through Root.OpenFile (file slot 1).
		rootOpenFile(0, "f1", os.O_RDWR, 0, true), // file slot 1
		fsPwrite(1, pat(10, 0x77), 4),
		fsPread(1, 16, 0),
		fsClose(1),
	)
	return ops
}

var rootCoverageFileSlots = func() int {
	n := 0
	for _, o := range rootCoverageOps() {
		if strings.Contains(o.name, "track=true") || strings.Contains(o.name, "trackFile=true") {
			n++
		}
	}
	return n
}()

// ---------------------------------------------------------------------------
// Seeded random grammar over root 0 ("jail").

var rootPathPool = []string{
	"f1", "f2", "f3",
	"d1", "d2",
	"d1/a", "d1/b", "d1/sub", "d1/sub/x",
	"d2/a", "d2/sub", "d2/sub/x",
}

var rootEscapePool = []string{
	"", "/", "/outside", "..", "../outside", "d1/../../outside", "d2/../../../x",
}

type rootGen struct {
	rng        *rand.Rand
	ops        []op
	kind       []byte // 0 missing, 'f' file, 'd' dir
	permCreate []bool
	files      []fsGenSlot
}

func (g *rootGen) add(o ...op) { g.ops = append(g.ops, o...) }

func (g *rootGen) pick() int { return g.rng.IntN(len(rootPathPool)) }

func (g *rootGen) parentIsDir(idx int) bool {
	p := rootPathPool[idx]
	slash := strings.LastIndexByte(p, '/')
	if slash < 0 {
		return true
	}
	parent := p[:slash]
	for i, q := range rootPathPool {
		if q == parent {
			return g.kind[i] == 'd'
		}
	}
	return false
}

// decoratePath occasionally wraps a pool path in "./", repeated "//", or an
// in-bounds "<existingDir>/../" hop to exercise dstRootWalkLocked's component
// normalizer under random combinations.
func (g *rootGen) decoratePath(rel string) string {
	switch g.rng.IntN(5) {
	case 0:
		return "./" + rel
	case 1:
		return strings.ReplaceAll(rel, "/", "//")
	case 2:
		for i, p := range rootPathPool {
			if g.kind[i] == 'd' && !strings.Contains(p, "/") {
				return p + "/../" + rel
			}
		}
	}
	return rel
}

func (g *rootGen) clearSubtree(idx int) {
	prefix := rootPathPool[idx] + "/"
	g.kind[idx] = 0
	for j, p := range rootPathPool {
		if strings.HasPrefix(p, prefix) {
			g.kind[j] = 0
		}
	}
}

func (g *rootGen) unlinkFilesAt(idx int) {
	for s := range g.files {
		if g.files[s].pathIdx == idx {
			g.files[s].pathIdx = -2
		}
	}
}

func (g *rootGen) moveSubtree(src, dst int) {
	srcPath, dstPath := rootPathPool[src], rootPathPool[dst]
	type mv struct{ from, to int }
	var moves []mv
	for j, p := range rootPathPool {
		if g.kind[j] == 0 || (p != srcPath && !strings.HasPrefix(p, srcPath+"/")) {
			continue
		}
		toPath := dstPath + p[len(srcPath):]
		to := -1
		for k, q := range rootPathPool {
			if q == toPath {
				to = k
				break
			}
		}
		moves = append(moves, mv{j, to})
	}
	kinds := make(map[int]byte, len(moves))
	perms := make(map[int]bool, len(moves))
	for _, m := range moves {
		kinds[m.from] = g.kind[m.from]
		perms[m.from] = g.permCreate[m.from]
		g.kind[m.from] = 0
	}
	for _, m := range moves {
		for s := range g.files {
			if g.files[s].pathIdx == m.from {
				if m.to >= 0 {
					g.files[s].pathIdx = m.to
				} else {
					g.files[s].pathIdx = -2
				}
			}
		}
		if m.to >= 0 {
			g.kind[m.to] = kinds[m.from]
			g.permCreate[m.to] = perms[m.from]
		}
	}
}

func (g *rootGen) step() {
	r := g.rng.IntN(100)
	switch {
	case r < 6: // escape / invalid path probe across random methods
		esc := rootEscapePool[g.rng.IntN(len(rootEscapePool))]
		switch g.rng.IntN(5) {
		case 0:
			g.add(rootOpenFile(0, esc, os.O_RDONLY, 0, false))
		case 1:
			g.add(rootMkdirAll(0, esc, 0o755))
		case 2:
			g.add(rootStat(0, esc, g.rng.IntN(2) == 0, false))
		case 3:
			g.add(rootRemoveAll(0, esc))
		default:
			g.add(rootRename(0, esc, "f1"))
		}

	case r < 22: // OpenFile / WriteFile / ReadFile
		idx := g.pick()
		rel := g.decoratePath(rootPathPool[idx])
		if g.rng.IntN(3) == 0 {
			if g.rng.IntN(2) == 0 {
				perm := fsFilePerms[g.rng.IntN(len(fsFilePerms))]
				g.add(rootWriteFile(0, rel, pat(g.rng.IntN(128), byte(g.rng.UintN(256))), perm))
				if g.kind[idx] == 0 && g.parentIsDir(idx) {
					g.kind[idx] = 'f'
					g.permCreate[idx] = true
				}
			} else {
				g.add(rootReadFile(0, rel))
			}
			return
		}
		acc := []int{os.O_RDONLY, os.O_WRONLY, os.O_RDWR}[g.rng.IntN(3)]
		flag := acc
		if g.rng.IntN(2) == 0 {
			flag |= os.O_CREATE
		}
		if flag&os.O_CREATE != 0 && g.rng.IntN(4) == 0 {
			flag |= os.O_EXCL
		}
		if acc != os.O_RDONLY && g.rng.IntN(3) == 0 {
			flag |= os.O_TRUNC
		}
		slashed := g.rng.IntN(6) == 0
		if slashed {
			rel += "/"
		}
		perm := fsFilePerms[g.rng.IntN(len(fsFilePerms))]
		g.add(rootOpenFile(0, rel, flag, perm, true))
		willSucceed := false
		created := false
		switch g.kind[idx] {
		case 0:
			if flag&os.O_CREATE != 0 && !slashed && g.parentIsDir(idx) {
				willSucceed = true
				created = true
				g.kind[idx] = 'f'
				g.permCreate[idx] = true
			}
		case 'f':
			if !slashed && flag&(os.O_CREATE|os.O_EXCL) != (os.O_CREATE|os.O_EXCL) {
				willSucceed = true
			}
		case 'd':
			if acc == os.O_RDONLY && flag&(os.O_CREATE|os.O_TRUNC) == 0 {
				willSucceed = true
			}
		}
		if willSucceed {
			pIdx := idx
			pCreate := g.permCreate[idx]
			if created {
				pCreate = true
			}
			g.files = append(g.files, fsGenSlot{
				isDir:      g.kind[idx] == 'd',
				writable:   acc != os.O_RDONLY,
				readable:   acc != os.O_WRONLY && g.kind[idx] != 'd',
				pathIdx:    pIdx,
				permCreate: pCreate,
			})
		} else {
			g.files = append(g.files, fsGenSlot{pathIdx: -1, closed: true})
		}

	case r < 34: // I/O or Close on rooted file handle
		if len(g.files) <= rootCoverageFileSlots {
			return
		}
		s := rootCoverageFileSlots + g.rng.IntN(len(g.files)-rootCoverageFileSlots)
		switch g.rng.IntN(4) {
		case 0:
			g.add(fsWrite(s, pat(1+g.rng.IntN(96), byte(g.rng.UintN(256)))))
		case 1:
			g.add(fsRead(s, 1+g.rng.IntN(96)))
		case 2:
			g.add(fsPread(s, 1+g.rng.IntN(64), int64(g.rng.IntN(64))))
		default:
			g.add(fsClose(s))
			g.files[s].closed = true
		}

	case r < 48: // Mkdir / MkdirAll
		idx := g.pick()
		rel := g.decoratePath(rootPathPool[idx])
		if g.rng.IntN(4) == 0 {
			rel += "/"
		}
		perm := fsDirPerms[g.rng.IntN(len(fsDirPerms))]
		if g.rng.IntN(2) == 0 {
			g.add(rootMkdir(0, rel, perm))
			if g.kind[idx] == 0 && g.parentIsDir(idx) {
				g.kind[idx] = 'd'
				g.permCreate[idx] = true
			}
		} else {
			g.add(rootMkdirAll(0, rel, perm))
			// Update pool state if every ancestor in rootPathPool[idx] is
			// missing or already a directory.
			parts := strings.Split(rootPathPool[idx], "/")
			ok := true
			for k := 1; k <= len(parts); k++ {
				sub := strings.Join(parts[:k], "/")
				for j, q := range rootPathPool {
					if q == sub && g.kind[j] == 'f' {
						ok = false
					}
				}
			}
			if ok {
				for k := 1; k <= len(parts); k++ {
					sub := strings.Join(parts[:k], "/")
					for j, q := range rootPathPool {
						if q == sub && g.kind[j] == 0 {
							g.kind[j] = 'd'
							g.permCreate[j] = true
						}
					}
				}
			}
		}

	case r < 60: // Remove / RemoveAll
		idx := g.pick()
		rel := g.decoratePath(rootPathPool[idx])
		// Match fsGen: directories may hold un-pooled children carried across an
		// earlier directory Rename, so use RemoveAll whenever g.kind[idx] == 'd'
		// (Root.Remove on empty and non-empty directories is pinned in
		// rootCoverageOps) so g.kind stays exact and Chmod never strips the
		// execute bit from a live host directory.
		if g.kind[idx] != 'd' && g.rng.IntN(2) == 0 {
			g.add(rootRemove(0, rel))
			if g.kind[idx] == 'f' {
				g.unlinkFilesAt(idx)
				g.kind[idx] = 0
			}
		} else {
			g.add(rootRemoveAll(0, rel))
			// RemoveAll fails without removing when an ancestor is a regular file.
			ancFile := false
			parts := strings.Split(rootPathPool[idx], "/")
			for k := 1; k < len(parts); k++ {
				sub := strings.Join(parts[:k], "/")
				for j, q := range rootPathPool {
					if q == sub && g.kind[j] == 'f' {
						ancFile = true
					}
				}
			}
			if !ancFile && g.kind[idx] != 0 {
				prefix := rootPathPool[idx] + "/"
				g.unlinkFilesAt(idx)
				for j, q := range rootPathPool {
					if strings.HasPrefix(q, prefix) {
						g.unlinkFilesAt(j)
					}
				}
				g.clearSubtree(idx)
			}
		}

	case r < 72: // Rename / Link
		src, dst := g.pick(), g.pick()
		if g.rng.IntN(3) == 0 {
			slashDst := g.rng.IntN(5) == 0
			dstRel := rootPathPool[dst]
			if slashDst {
				dstRel += "/"
			}
			g.add(rootLink(0, g.decoratePath(rootPathPool[src]), dstRel))
			if g.kind[src] == 'f' && g.kind[dst] == 0 && !slashDst && g.parentIsDir(dst) {
				g.kind[dst] = 'f'
				g.permCreate[dst] = g.permCreate[src]
			}
			return
		}
		oldRel, newRel := rootPathPool[src], rootPathPool[dst]
		slashOld := g.rng.IntN(5) == 0
		slashNew := g.rng.IntN(5) == 0
		if slashOld {
			oldRel += "/"
		}
		if slashNew {
			newRel += "/"
		}
		g.add(rootRename(0, oldRel, newRel))
		switch g.kind[src] {
		case 'f':
			if src != dst && !slashOld && !slashNew && g.kind[dst] != 'd' && g.parentIsDir(dst) {
				if g.kind[dst] != 0 {
					g.unlinkFilesAt(dst)
				}
				for s := range g.files {
					if g.files[s].pathIdx == src {
						g.files[s].pathIdx = dst
					}
				}
				g.kind[dst] = 'f'
				g.permCreate[dst] = g.permCreate[src]
				g.kind[src] = 0
			}
		case 'd':
			dstInsideSrc := src == dst || strings.HasPrefix(rootPathPool[dst], rootPathPool[src]+"/")
			if g.kind[dst] == 0 && g.parentIsDir(dst) && !dstInsideSrc {
				g.moveSubtree(src, dst)
			}
		}

	case r < 86: // Stat / Lstat / Root.FS() ReadDir & ReadFile
		idx := g.pick()
		switch g.rng.IntN(4) {
		case 0:
			if g.kind[idx] == 'd' {
				g.add(rootFSReadDir(0, rootPathPool[idx]))
			} else {
				g.add(rootFSReadFile(0, rootPathPool[idx]))
			}
		default:
			g.add(rootStat(0, g.decoratePath(rootPathPool[idx]), g.rng.IntN(2) == 0, g.kind[idx] != 0 && g.permCreate[idx]))
		}

	case r < 94: // Chmod
		idx := g.pick()
		var perm os.FileMode
		if g.kind[idx] == 'd' {
			perm = fsDirPerms[g.rng.IntN(len(fsDirPerms))]
		} else {
			perm = fsFilePerms[g.rng.IntN(len(fsFilePerms))]
		}
		g.add(rootChmod(0, g.decoratePath(rootPathPool[idx]), perm))
		if g.kind[idx] != 0 {
			g.permCreate[idx] = false
			for s := range g.files {
				if g.files[s].pathIdx == idx {
					g.files[s].permCreate = false
				}
			}
		}

	default: // Chtimes + Stat mtime or transient OpenRoot probe
		idx := g.pick()
		if g.rng.IntN(2) == 0 {
			g.add(rootOpenSub(0, g.decoratePath(rootPathPool[idx]), false))
		} else {
			g.add(rootChtimes(0, rootPathPool[idx]))
			g.add(rootStatMtime(0, rootPathPool[idx]))
		}
	}
}

func genRootOps(seed uint64, n int) []op {
	g := &rootGen{
		rng:        rand.New(rand.NewPCG(seed, 0xD57)),
		kind:       make([]byte, len(rootPathPool)),
		permCreate: make([]bool, len(rootPathPool)),
	}
	g.ops = rootCoverageOps()
	for i := 0; i < rootCoverageFileSlots; i++ {
		g.files = append(g.files, fsGenSlot{pathIdx: -1, closed: true})
	}
	for i, p := range rootPathPool {
		switch p {
		case "f1":
			g.kind[i], g.permCreate[i] = 'f', true
		case "d1":
			g.kind[i], g.permCreate[i] = 'd', true
		}
	}
	for range n {
		g.step()
	}
	return g.ops
}

func TestDSTConformanceRoot(t *testing.T) {
	allow := rootAllowlist()
	fired := make(map[string]int)
	for _, seed := range sweepSeeds(t) {
		ops := genRootOps(seed, 300)
		host := runOpsHost(t, ops)
		sim := runOpsSim(t, seed, ops)
		if d := diffOutcomes(ops, host, sim, allow, fired); d != nil {
			reportDivergence(t, "root", seed, ops, d)
			return
		}
	}
	checkAllowlistCoverage(t, allow, fired)
}
