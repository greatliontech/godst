// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package testing

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"
)

// The testing framework's output stream (the -v chatty printer, and the
// benchmark b.Log stream) is framework-owned host plumbing, not code under
// test: it is constructed from the pre-run host stdout before any simulation
// starts, and under -v its writes (t.Log routing, status lines) execute on
// goroutines inside the simulation bubble. Under deterministic simulation the
// interception boundary would otherwise refuse those writes as SUT stdio
// escapes — losing every in-run diagnostic the -v stream exists to carry.
//
// The bubble path here writes the captured raw descriptor directly under the
// same scoped per-goroutine host-I/O grant that explicitly inherited file
// capabilities use: an outbound, schedule-ordered side effect that feeds no
// nondeterminism back into the run — the write syscall holds the P, so it
// serializes the bubble for its duration exactly like a capability write (see
// docs/dst/design.md, "Deterministic pipes and the stdio stance"). Two
// determinism constraints shape the path:
//
//   - It never blocks on state a HOST goroutine can hold across wall-clock
//     work: not the printer's lastNameMu (a t.Parallel host test logging
//     concurrently holds it across fmt formatting and a possibly-blocking
//     stdout write) and not the poll layer's fd mutex (held across the host's
//     own write syscalls). A bubble goroutine parked on either wakes at a
//     wall-clock-dependent instant, reordering the seeded schedule. The
//     bubble path holds no lock at all and writes with a raw, granted
//     syscall.
//
//   - It adds no host-coupled BRANCHES of its own. That is why the
//     "=== NAME" context header is UNCONDITIONAL, not last-name-tracked: the
//     stream's real "current test" context is shared with concurrent host
//     writers, so any header decision either reads host-coupled state (the
//     bytes written — and the allocations behind them, which feed the
//     deterministic GC trigger — would then vary with host activity) or
//     tracks only bubble writes and goes stale the moment a host line lands,
//     leaving a bubble line attributed to the wrong test. A constant
//     decision always attributes correctly; the cost is one redundant header
//     line per bubble write. (Recorded bound, not introduced here: fmt draws
//     its printers from a process-shared sync.Pool, so an in-bubble fmt
//     call's hit-or-miss allocation profile is in principle host-coupled — a
//     whole-of-fmt property affecting every in-bubble formatting call, SUT
//     included. The coupling is schedule-neutral: sync.Pools are cleared at
//     every gcStart, and in-bubble GCs are deterministic, so host-donated
//     pool warmth is bounded by deterministic clear points rather than
//     accumulating across a run. Enforced by same-seed transcript equality
//     under a host-parallel goroutine hammering the shared printer
//     (TestVerboseContendedSameSeedTranscript) and by the fmt-heavy,
//     GC-trigger-crossing testing/simulation/determinism sweep — the
//     standing pins if the coupling ever becomes schedule-reachable.)
//
// The grant is enacted only around these unexported writes, whose inputs the
// framework formatted before the grant opens; SUT code cannot reach them, so
// a SUT write to os.Stdout/os.Stderr stays fenced exactly as without -v.
// Everything is scoped to the writing goroutine and restored, so a grant can
// never leak past the framework write.

// dstFrameworkStreamEnabled is the compile-time gate, mirroring the runtime's
// dstBuild / syscall's dstSimFenced const-guard idiom: in a stock (non-dst)
// build the false guard folds the bubble path out of the printer entirely.
const dstFrameworkStreamEnabled = true

//go:linkname dstSetHostIO runtime.dstSetHostIO
func dstSetHostIO(active bool) (old bool)

//go:linkname dstInSimBubble runtime.dstInSimBubble
func dstInSimBubble() bool

// dstPipeBufChunk is the atomicity budget of one framework-stream write:
// POSIX guarantees a pipe write of at most PIPE_BUF (4096 on Linux) bytes is
// atomic against concurrent writers, and the test binary's stdout is a pipe
// under go test. Each emitted chunk stays within this budget and carries its
// own attribution header, so concurrent host lines can interleave only at
// chunk boundaries, where the next chunk re-identifies itself; only a single
// line longer than the budget can tear mid-line — output cosmetics only.
const dstPipeBufChunk = 4096

// dstBubbleFramework reports whether the calling goroutine's framework-stream
// write must take the granted bubble path, and panics — the interception
// boundary's loud unsupported shape; the printer's call sites carry no error
// channel — if that path is unavailable because the printer's stream is not
// a raw-descriptor-backed file. The panic is unreachable under any real
// `go test` wiring (cmd/go always hands the child an *os.File stdout, which
// newChattyPrinter captures); it exists so a future wiring change fails
// loudly here rather than silently falling back to the host-lock path, whose
// parking couples the seeded schedule to wall time.
func (p *chattyPrinter) dstBubbleFramework() bool {
	if !dstInSimBubble() {
		return false
	}
	if p.hostFD < 0 {
		panic("testing: framework output stream is not file-backed: unsupported under deterministic simulation")
	}
	return true
}

// dstBubbleUpdatef is Updatef's bubble-domain leg. It reports false when the
// caller is not an in-bubble goroutine of an active simulation, in which case
// the caller takes the ordinary host path. Updatef messages carry the test
// name themselves (the method's contract), so no attribution header is
// needed.
func (p *chattyPrinter) dstBubbleUpdatef(testName, format string, args ...any) bool {
	if !p.dstBubbleFramework() {
		return false
	}
	dstHostStreamWrite(p.hostFD, nil, fmt.Appendf(nil, p.prefix()+format, args...))
	return true
}

// dstBubblePrintf is Printf's bubble-domain leg: an UNCONDITIONAL "=== NAME"
// attribution header, emitted in the same atomic chunk as the payload it
// attributes (see the package comment for why the header is constant, and
// dstPipeBufChunk for the atomicity bound).
func (p *chattyPrinter) dstBubblePrintf(testName, format string, args ...any) bool {
	if !p.dstBubbleFramework() {
		return false
	}
	header := fmt.Appendf(nil, "%s=== NAME  %s\n", p.prefix(), testName)
	dstHostStreamWrite(p.hostFD, header, fmt.Appendf(nil, format, args...))
	return true
}

// dstBubbleBenchWrite is the benchmark output branch's bubble-domain leg
// (writeLine's bench arm prints straight to stdout, with no name-header
// machinery — mirrored here). Reports false outside a bubble, as above.
func (p *chattyPrinter) dstBubbleBenchWrite(indent string, b []byte) bool {
	if !p.dstBubbleFramework() {
		return false
	}
	dstHostStreamWrite(p.hostFD, nil, fmt.Appendf(nil, "%s%s", indent, b))
	return true
}

// dstHostStreamWrite performs the granted raw writes of one framework-stream
// payload, prefixing every emitted chunk with header (which may be empty):
// the per-goroutine host-I/O grant lets the write syscalls pass the
// interception boundary. The payload is chunked at line boundaries so each
// header-carrying chunk stays within the PIPE_BUF atomicity budget
// (dstPipeBufChunk); a single line longer than the budget goes out whole —
// it would tear in the kernel however it were split.
//
// The write is a RawSyscall, not os.File.Write and not syscall.Write, and
// each hop matters: the poll layer's fd mutex is a lock host goroutines hold
// across wall-clock work (the package comment's no-host-lock constraint),
// and syscall.Write's Syscall trampoline opens an entersyscall/exitsyscall
// window — the one P's _Psyscall state becomes scheduler-visible shared
// state that wall-timed host events race against (a host M's own
// exitsyscall fast path reclaiming the P it also entered syscalls through,
// or a pending stop-the-world claiming _Psyscall Ps), and losing that race
// sends the returning bubble goroutine through exitsyscall's slow path onto
// the run queue: a scheduling decision at a wall-clock-dependent instant.
// Demonstrated as same-seed transcript divergence under -race with
// host-parallel logging, and empirically isolated to the window: the
// trampoline form diverges, this raw form does not (and GC-off runs did
// not) — the contended transcript pin holds the line. A RawSyscall is
// scheduler-invisible, so the write is indistinguishable from ordinary
// bubble computation except in wall time — while blocked it briefly holds
// the P, the literal form of the spec's capability-write serialization
// contract (a wall-time delay that cannot reorder the schedule). The dual
// of that invisibility: a wall-blocked raw write also delays a pending host
// GC stop-the-world for its duration (the goroutine is unpreemptible
// mid-raw-syscall) — a wall-time-only cost, within the same stance.
//
// EINTR and short writes are retried, as the poll layer would. EAGAIN is
// retried too, rather than dropping bytes or panicking: the descriptor is in
// blocking mode as a construction-time invariant (newChattyPrinter's Fd()
// call switches a pollable stream back to blocking mode as a side effect,
// and stdio starts blocking), so EAGAIN is unreachable; if wiring drift ever
// made the stream nonblocking, retrying preserves the diagnostic stream's
// completeness — the property this seam exists for — and degrades to a
// busy-wait form of the blocking write the model already admits, where a
// panic would turn cosmetic wiring drift into a spurious run failure. Any
// other error is swallowed, exactly like the host leg's ignored fmt.Fprintf
// error: the framework's diagnostic stream has no better channel to report
// its own failure.
func dstHostStreamWrite(fd int, header, payload []byte) {
	old := dstSetHostIO(true)
	defer dstSetHostIO(old)
	writeFull := func(b []byte) {
		for len(b) > 0 {
			r1, _, errno := syscall.RawSyscall(syscall.SYS_WRITE, uintptr(fd), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
			n := int(r1)
			if errno != 0 {
				n = 0
			}
			if n > 0 {
				b = b[n:]
			}
			if errno == syscall.EINTR || errno == syscall.EAGAIN {
				continue
			}
			if errno != 0 {
				return
			}
		}
	}
	budget := dstPipeBufChunk - len(header)
	if budget <= 0 {
		budget = 1 // a pathologically long test name: still chunk line-wise
	}
	for {
		chunk := payload
		if len(chunk) > budget {
			// Cut at the last line boundary within budget; with none, take
			// the whole overlong line (up to and including its newline, or
			// to the end).
			cut := budget
			for cut > 0 && chunk[cut-1] != '\n' {
				cut--
			}
			if cut == 0 {
				cut = len(chunk)
				for i := budget; i < len(chunk); i++ {
					if chunk[i] == '\n' {
						cut = i + 1
						break
					}
				}
			}
			chunk = chunk[:cut]
		}
		if len(header) > 0 {
			buf := make([]byte, 0, len(header)+len(chunk))
			buf = append(buf, header...)
			buf = append(buf, chunk...)
			writeFull(buf)
		} else {
			writeFull(chunk)
		}
		payload = payload[len(chunk):]
		if len(payload) == 0 {
			return
		}
	}
}

// dstTestlogWriter routes the cmd/go test-action log (the
// -test.testlogfile writer, whose bufio buffer is fed by every
// os.Open/Getenv the test performs and can FLUSH on any goroutine —
// including a bubble goroutine, where the raw host write met the
// fence and crashed any open-heavy dst binary under go test's caching
// mode). The log is framework plumbing exactly like the -v stream: a
// host-owned descriptor captured before any run starts, outbound
// only, feeding no nondeterminism back. In-bubble flushes take the
// same granted raw-write path as the framework stream; host
// goroutines write normally. The residual log.mu schedule coupling (a
// bubble flush parking on the buffer lock a host goroutine holds
// across wall-clock work) is recorded in the issue index — it needs
// the printer-style lock-free treatment, a design of its own.
type dstTestlogWriter struct {
	f  *os.File
	fd int // captured pre-run; File.Fd from a bubble goroutine would fence
}

func dstWrapTestlogWriter(f *os.File) io.Writer {
	return &dstTestlogWriter{f: f, fd: int(f.Fd())}
}

func (w *dstTestlogWriter) Write(p []byte) (int, error) {
	if !dstInSimBubble() {
		return w.f.Write(p)
	}
	dstHostStreamWrite(w.fd, nil, p)
	return len(p), nil
}
