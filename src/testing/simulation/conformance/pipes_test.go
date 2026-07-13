// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package conformance

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"testing/simulation"
	"time"
)

// Host-probed Linux anonymous-pipe constants (the model's contract).
const (
	pipeCap = 65536
	pipeBuf = 4096
)

// Guard durations. A guard bounds a would-block op by setting the
// relevant deadline, executing, and clearing it: on the host it costs
// real time, in the bubble it is virtual. Guards exist so a
// single-goroutine sequence can include blocking arms without hanging.
const (
	guardBlock = 150 * time.Millisecond
	guardReady = 5 * time.Second // completes on any healthy host
)

// ---------------------------------------------------------------------------
// Pipe ops.

func pipeCreate() op {
	return op{
		name: "pipe() -> r,w slots",
		run: func(w *world) outcome {
			r, wr, err := os.Pipe()
			w.files = append(w.files, r, wr)
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

func pipeRead(slot, n int, guard time.Duration) op {
	return op{
		name: fmt.Sprintf("pipe-read(slot %d, %d bytes, guard %v)", slot, n, guard),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			if guard > 0 {
				f.SetReadDeadline(time.Now().Add(guard))
			}
			buf := make([]byte, n)
			rn, err := f.Read(buf)
			if guard > 0 {
				f.SetReadDeadline(time.Time{})
			}
			return outcome{Err: errClass(err), N: rn, State: contentHash(buf[:max(rn, 0)])}
		},
	}
}

func pipeWrite(slot int, payload []byte, guard time.Duration) op {
	return op{
		name:      fmt.Sprintf("pipe-write(slot %d, %d bytes, guard %v)", slot, len(payload), guard),
		writeSize: len(payload),
		guarded:   guard > 0,
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			if guard > 0 {
				f.SetWriteDeadline(time.Now().Add(guard))
			}
			n, err := f.Write(payload)
			if guard > 0 {
				f.SetWriteDeadline(time.Time{})
			}
			return outcome{Err: errClass(err), N: n}
		},
	}
}

// deadline kinds for the deliberate-deadline grammar arms. There is no
// armed-FUTURE kind: a wall-clock future deadline would make later host
// ops depend on real elapsed time, which the virtual clock cannot
// mirror — future-bounded blocking lives in the ops' own guards.
const (
	dlPast  = "past"
	dlClear = "clear"
)

func deadlineTime(kind string) time.Time {
	if kind == dlPast {
		return time.Now().Add(-time.Hour)
	}
	return time.Time{}
}

func pipeSetReadDeadline(slot int, kind string) op {
	return op{
		name: fmt.Sprintf("pipe-set-read-deadline(slot %d, %s)", slot, kind),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(f.SetReadDeadline(deadlineTime(kind))), N: -1}
		},
	}
}

func pipeSetWriteDeadline(slot int, kind string) op {
	return op{
		name: fmt.Sprintf("pipe-set-write-deadline(slot %d, %s)", slot, kind),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(f.SetWriteDeadline(deadlineTime(kind))), N: -1}
		},
	}
}

func pipeFchmod(slot int, perm os.FileMode) op {
	return op{
		name: fmt.Sprintf("pipe-fchmod(slot %d, %04o)", slot, perm),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(f.Chmod(perm)), N: -1}
		},
	}
}

func pipeSameFile(rSlot, wSlot int) op {
	return op{
		name: fmt.Sprintf("pipe-samefile(slots %d,%d)", rSlot, wSlot),
		run: func(w *world) outcome {
			r, wr := slotFile(w, rSlot), slotFile(w, wSlot)
			if r == nil || wr == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			rfi, err := r.Stat()
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			wfi, err := wr.Stat()
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			return outcome{N: -1, State: fmt.Sprintf("samefile=%v", os.SameFile(rfi, wfi))}
		},
	}
}

// pipeFd probes Fd() on a pipe end: works on the host, fenced (panic)
// under DST — the recorded pipe virtual-fd gap. Placed on a dedicated
// pipe: host Fd() switches the descriptor to blocking mode, killing
// later deadline ops on that end.
func pipeFd(slot int) op {
	return op{
		name: fmt.Sprintf("pipe-fd(slot %d)", slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			if f.Fd() != 0 {
				return outcome{N: -1, State: "fd=ok"}
			}
			return outcome{N: -1, State: "fd=zero"}
		},
	}
}

func pipeChdir(slot int) op {
	return op{
		name: fmt.Sprintf("pipe-chdir(slot %d)", slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(f.Chdir()), N: -1}
		},
	}
}

// pipeDrainAll reads under repeated guards until the pipe is empty
// (deadline) or ended (EOF/closed), reporting the total and a running
// hash. Emitted after every potentially-partial blocked write, so the
// two legs' buffered-byte accounting is re-synchronized AND compared:
// if the host's page-structured pipe ring and the sim's byte-exact ring
// ever admit different byte counts, the drain total diverges loudly
// here instead of desynchronizing later ops.
func pipeDrainAll(slot int) op {
	return op{
		name: fmt.Sprintf("pipe-drain-all(slot %d)", slot),
		run: func(w *world) outcome {
			f := slotFile(w, slot)
			if f == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			total := 0
			var last error
			var hash string
			buf := make([]byte, 8192)
			for {
				f.SetReadDeadline(time.Now().Add(guardBlock))
				n, err := f.Read(buf)
				if n > 0 {
					total += n
					hash = contentHash([]byte(hash + contentHash(buf[:n])))
				}
				if err != nil {
					last = err
					break
				}
			}
			f.SetReadDeadline(time.Time{})
			return outcome{Err: errClass(last), N: total, State: hash}
		},
	}
}

// ---------------------------------------------------------------------------
// The pipe allowlist.

func pipeAllowlist() []allowEntry {
	return []allowEntry{
		{
			key:  "pipe-ring-byte-exact",
			cite: `design.md §Deterministic pipes and the stdio stance: "The simulated buffer is a byte-exact 64 KiB ring; the kernel's is a ring of 16 page slots ... the partial count of a deadline- or close-interrupted oversize write may run HIGHER in simulation by that fragmentation slack, never lower"`,
			match: func(o op, host, sim outcome) bool {
				// One-directional: the simulation admits at least what
				// the kernel would. The slack is page-granular and
				// bounded only by the ring, so the direction is the
				// checked invariant.
				delta := sim.N - host.N
				if delta <= 0 || delta >= pipeCap || host.Err != sim.Err || host.Err == "" {
					return false
				}
				// The interrupted write itself — scoped to OVERSIZE
				// (>PIPE_BUF) guarded writes, the only shape the recorded
				// slack window covers: a ≤PIPE_BUF write is atomic on
				// both legs, so a partial count there is a regression the
				// entry must NOT absorb — and the drain that
				// re-synchronizes the two legs' accounting afterwards
				// (its totals — and thus content hashes — carry the same
				// one-directional slack).
				return strings.HasPrefix(o.name, "pipe-write(") && o.writeSize > pipeBuf && o.guarded ||
					strings.HasPrefix(o.name, "pipe-drain-all(")
			},
		},
		{
			key:  "pipe-fd-fenced",
			cite: `design.md §Deterministic pipes and the stdio stance: "Fd() on a simulated pipe still panics" (the virtual fd surface is deliberately file-tree-only until a pipe fd contract is settled)`,
			match: func(o op, host, sim outcome) bool {
				return strings.HasPrefix(o.name, "pipe-fd(") &&
					host.Err == "" && host.State == "fd=ok" &&
					sim.Err == "panic:dst-unsupported"
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Fixed coverage ladder: the spec's precedence order (closed > deadline
// > direction > EPIPE/EOF), the zero-length short-circuit points, the
// non-stream surface (ESPIPE/EINVAL/ENOTDIR), Stat/SameFile/Chmod, and
// the recorded Fd fence.

func pipeCoverageOps() []op {
	var ops []op
	add := func(o op) { ops = append(ops, o) }

	// pipe A: r=0 w=1 — transfer shapes, deadline precedence.
	add(pipeCreate())
	rA, wA := 0, 1
	add(pipeWrite(wA, pat(8, 20), 0))
	add(pipeRead(rA, 16, 0)) // partial buffer read: 8 bytes
	add(pipeRead(rA, 0, 0))  // zero read on empty pipe: (0, nil)
	add(pipeWrite(wA, nil, 0))
	add(pipeWrite(wA, pat(pipeBuf+904, 21), 0)) // >PIPE_BUF single-writer write
	add(pipeRead(rA, 3000, 0))                  // partial read
	add(pipeRead(rA, 3000, 0))                  // returns the 2000 available, not 3000
	// Expired deadline beats buffered data.
	add(pipeWrite(wA, pat(10, 22), 0))
	add(pipeSetReadDeadline(rA, dlPast))
	add(pipeRead(rA, 10, 0)) // ErrDeadlineExceeded despite data
	add(pipeRead(rA, 0, 0))  // zero read short-circuits ahead of the deadline
	add(pipeSetReadDeadline(rA, dlClear))
	add(pipeRead(rA, 10, 0)) // data readable after clear
	// Expired WRITE deadline on the READ end beats the direction check.
	add(pipeSetWriteDeadline(rA, dlPast))
	add(pipeWrite(rA, pat(3, 23), 0)) // ErrDeadlineExceeded, not EBADF
	add(pipeWrite(rA, nil, 0))        // zero write: deadline still beats it
	add(pipeSetWriteDeadline(rA, dlClear))
	add(pipeWrite(rA, pat(3, 24), 0)) // EBADF (wrong direction)
	add(pipeWrite(rA, nil, 0))        // EBADF: zero write checks direction first
	add(pipeRead(wA, 3, 0))           // EBADF (wrong direction)
	// Blocked-read guard shape.
	add(pipeRead(rA, 8, guardBlock)) // empty pipe: ErrDeadlineExceeded, n=0
	// Reader-less write: EPIPE; writer-less read: EOF.
	add(fsClose(rA))
	add(pipeWrite(wA, pat(4, 25), 0)) // EPIPE
	add(pipeWrite(wA, nil, 0))        // zero write with peer closed: (0, nil)
	add(fsClose(wA))
	add(pipeWrite(wA, pat(4, 26), 0))    // own end closed: ErrClosed (beats EPIPE)
	add(pipeRead(rA, 4, 0))              // own end closed: ErrClosed
	add(pipeSetReadDeadline(rA, dlPast)) // closed SetDeadline: bare ErrClosed
	add(fsClose(rA))                     // double close: ErrClosed

	// pipe B: r=2 w=3 — EOF drain, oversize block, non-stream surface.
	add(pipeCreate())
	rB, wB := 2, 3
	add(pipeWrite(wB, pat(100, 27), 0))
	add(fsClose(wB))
	add(pipeRead(rB, 60, 0))         // drains
	add(pipeRead(rB, 60, 0))         // rest
	add(pipeRead(rB, 60, 0))         // EOF
	add(pipeRead(rB, 0, 0))          // zero read with writer closed: (0, nil)
	add(fsSeek(rB, 0, io.SeekStart)) // ESPIPE
	add(fsPread(rB, 4, 0))           // ESPIPE
	add(fsPwrite(rB, pat(4, 28), 0)) // ESPIPE
	add(fsTruncateFd(rB, 0))         // EINVAL
	add(fsSync(rB))                  // EINVAL
	add(fsReaddirnamesChunks(rB, 2)) // ENOTDIR
	add(pipeChdir(rB))               // ENOTDIR
	add(fsFstat(rB, false))          // ModeNamedPipe|0600, size 0
	add(fsClose(rB))

	// pipe C: r=4 w=5 — Stat identity, Chmod, buffered-size pin, the
	// full-buffer blocked write (small writes are PIPE_BUF-atomic:
	// nothing transfers), an oversize blocked write (chunks: partial
	// transfer), and the Fd fence (last: host Fd() flips the end to
	// blocking mode).
	add(pipeCreate())
	rC, wC := 4, 5
	add(pipeSameFile(rC, wC))
	add(pipeWrite(wC, pat(9, 29), 0))
	add(fsFstat(rC, false)) // size stays 0 with bytes buffered
	add(pipeFchmod(rC, 0o640))
	add(fsFstat(rC, false)) // chmod shows in Stat
	add(pipeFchmod(rC, 0o600))
	// Fill to capacity, then block.
	add(pipeWrite(wC, pat(pipeCap-9, 30), 0))            // exactly fills
	add(pipeWrite(wC, pat(1, 31), guardBlock))           // full: deadline, n=0
	add(pipeRead(rC, 9, 0))                              // free 9 bytes
	add(pipeWrite(wC, pat(20, 32), guardBlock))          // atomic ≤PIPE_BUF: n=0, deadline
	add(pipeWrite(wC, pat(pipeBuf+100, 33), guardBlock)) // >PIPE_BUF: partial n, deadline
	add(pipeDrainAll(rC))
	add(pipeFd(wC)) // recorded fence divergence
	add(fsClose(rC))
	add(fsClose(wC))

	return ops
}

// pipeCoverageSlots is the number of file slots the ladder mints (3
// pipes × 2 ends).
const pipeCoverageSlots = 6

// ---------------------------------------------------------------------------
// Random grammar.

type pipeEndState struct {
	closed  bool
	readDL  string // dlPast/"" (none)
	writeDL string
}

type pipePair struct {
	r, w     int
	buffered int
	rSt, wSt pipeEndState
}

type pipeGen struct {
	rng    *rand.Rand
	ops    []op
	nSlots int
	pipes  []pipePair
}

func (g *pipeGen) add(o op) { g.ops = append(g.ops, o) }

func (g *pipeGen) newPipe() {
	g.add(pipeCreate())
	g.pipes = append(g.pipes, pipePair{r: g.nSlots, w: g.nSlots + 1})
	g.nSlots += 2
}

func (g *pipeGen) step() {
	if len(g.pipes) == 0 {
		g.newPipe()
		return
	}
	p := &g.pipes[g.rng.IntN(len(g.pipes))]
	r := g.rng.IntN(100)
	switch {
	case r < 30: // read on the read end
		n := 1 + g.rng.IntN(2048)
		if g.rng.IntN(12) == 0 {
			n = 0
		}
		switch {
		case p.rSt.closed || p.rSt.readDL == dlPast || n == 0:
			g.add(pipeRead(p.r, n, 0))
		case p.buffered > 0:
			g.add(pipeRead(p.r, n, 0))
			p.buffered -= min(n, p.buffered)
		case p.wSt.closed:
			g.add(pipeRead(p.r, n, 0)) // EOF
		default:
			g.add(pipeRead(p.r, n, guardBlock))
		}
	case r < 60: // write on the write end
		size := 1 + g.rng.IntN(2048)
		switch g.rng.IntN(10) {
		case 0:
			size = 0
		case 1:
			size = pipeBuf + 1 + g.rng.IntN(4096) // >PIPE_BUF
		case 2:
			size = pipeCap + 1 + g.rng.IntN(8192) // oversize: blocks
		}
		payload := pat(size, byte(g.rng.IntN(256)))
		switch {
		case p.wSt.closed || p.wSt.writeDL == dlPast || p.rSt.closed || size == 0:
			g.add(pipeWrite(p.w, payload, 0))
		case p.buffered+size <= pipeCap:
			g.add(pipeWrite(p.w, payload, 0))
			p.buffered += size
		default:
			// Would block. ≤PIPE_BUF writes are atomic (nothing
			// transfers before the deadline); larger writes transfer a
			// host-ring-dependent partial count, so a drain follows to
			// re-synchronize AND compare the two legs' accounting.
			g.add(pipeWrite(p.w, payload, guardBlock))
			if size > pipeBuf {
				// The drain arms and clears its own read deadline.
				g.add(pipeDrainAll(p.r))
				if !p.rSt.closed {
					p.buffered = 0
					p.rSt.readDL = ""
				}
			}
		}
	case r < 68: // wrong direction
		if g.rng.IntN(2) == 0 {
			g.add(pipeWrite(p.r, pat(1+g.rng.IntN(32), byte(g.rng.IntN(256))), 0))
		} else {
			g.add(pipeRead(p.w, 1+g.rng.IntN(32), 0))
		}
	case r < 82: // deadline arms (past/clear only: an armed wall-clock
		// FUTURE deadline would make later host ops depend on real
		// elapsed time — the virtual clock cannot mirror that, so the
		// future-bounded blocking shape lives in the guards instead)
		kind := []string{dlPast, dlClear}[g.rng.IntN(2)]
		endIsR := g.rng.IntN(2) == 0
		rw := g.rng.IntN(2) == 0
		var slot int
		var st *pipeEndState
		if endIsR {
			slot, st = p.r, &p.rSt
		} else {
			slot, st = p.w, &p.wSt
		}
		if rw {
			g.add(pipeSetReadDeadline(slot, kind))
			if !st.closed {
				st.readDL = kind
				if kind == dlClear {
					st.readDL = ""
				}
			}
		} else {
			g.add(pipeSetWriteDeadline(slot, kind))
			if !st.closed {
				st.writeDL = kind
				if kind == dlClear {
					st.writeDL = ""
				}
			}
		}
	case r < 90: // close an end (double closes included)
		if g.rng.IntN(2) == 0 {
			g.add(fsClose(p.r))
			p.rSt.closed = true
		} else {
			g.add(fsClose(p.w))
			p.wSt.closed = true
		}
		if p.rSt.closed && p.wSt.closed && len(g.pipes) < 6 {
			g.newPipe()
		}
	case r < 96: // stat-shaped
		if g.rng.IntN(2) == 0 {
			g.add(fsFstat(p.r, false))
		} else {
			g.add(pipeSameFile(p.r, p.w))
		}
	default: // non-stream surface
		switch g.rng.IntN(4) {
		case 0:
			g.add(fsSeek(p.r, int64(g.rng.IntN(64)), io.SeekStart))
		case 1:
			g.add(fsPread(p.r, 8, 0))
		case 2:
			g.add(fsTruncateFd(p.w, 0))
		case 3:
			g.add(fsSync(p.w))
		}
	}
}

func genPipeOps(seed uint64, n int) []op {
	g := &pipeGen{rng: rand.New(rand.NewPCG(seed, 0x91BE))}
	g.ops = pipeCoverageOps()
	g.nSlots = pipeCoverageSlots
	g.newPipe()
	for range n {
		g.step()
	}
	return g.ops
}

// ---------------------------------------------------------------------------
// Domain tests.

func TestDSTConformancePipes(t *testing.T) {
	allow := pipeAllowlist()
	fired := make(map[string]int)
	for _, seed := range sweepSeeds(t) {
		ops := genPipeOps(seed, 160)
		host := runOpsHost(t, ops)
		sim := runOpsSim(t, seed, ops)
		if d := diffOutcomes(ops, host, sim, allow, fired); d != nil {
			reportDivergence(t, "pipes", seed, ops, d)
			return
		}
	}
	checkAllowlistCoverage(t, allow, fired)
}

// ---------------------------------------------------------------------------
// Targeted two-goroutine cases: close-while-blocked. These compare
// outcome SETS (doc.go): the host observation and the deterministic sim
// outcome must each be a member of the host-legal set.

type blockedResult struct {
	n   int
	err string
}

// pipeBlockedScenario runs one close-while-blocked shape and reports
// the blocked op's outcome. kind selects which end blocks and which end
// a second goroutine closes after a settle delay.
func pipeBlockedScenario(kind string) blockedResult {
	r, w, err := os.Pipe()
	if err != nil {
		return blockedResult{-1, "pipe:" + errClass(err)}
	}
	defer r.Close()
	defer w.Close()
	var n int
	var opErr error
	switch kind {
	case "read-close-own":
		go func() { time.Sleep(50 * time.Millisecond); r.Close() }()
		n, opErr = r.Read(make([]byte, 8))
	case "read-close-peer":
		go func() { time.Sleep(50 * time.Millisecond); w.Close() }()
		n, opErr = r.Read(make([]byte, 8))
	case "write-close-own":
		go func() { time.Sleep(50 * time.Millisecond); w.Close() }()
		n, opErr = w.Write(pat(pipeCap+8192, 40))
	case "write-close-peer":
		go func() { time.Sleep(50 * time.Millisecond); r.Close() }()
		n, opErr = w.Write(pat(pipeCap+8192, 41))
	}
	return blockedResult{n, errClass(opErr)}
}

func TestDSTConformancePipeCloseWhileBlocked(t *testing.T) {
	const oversize = pipeCap + 8192
	cases := []struct {
		kind  string
		legal func(r blockedResult) bool
		want  string
	}{
		{"read-close-own", func(r blockedResult) bool {
			return r.n == 0 && r.err == "PathError(read)/ErrClosed:file"
		}, "n=0, read ErrClosed"},
		{"read-close-peer", func(r blockedResult) bool {
			return r.n == 0 && r.err == "EOF" // io.EOF stays bare at the os.File surface
		}, "n=0, EOF"},
		{"write-close-own", func(r blockedResult) bool {
			return r.n >= 0 && r.n <= oversize && r.err == "PathError(write)/ErrClosed:file"
		}, "partial n, write ErrClosed"},
		{"write-close-peer", func(r blockedResult) bool {
			return r.n >= 0 && r.n < oversize && r.err == "PathError(write)/errno:EPIPE"
		}, "partial n, EPIPE"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			hostRes := pipeBlockedScenario(tc.kind)
			if !tc.legal(hostRes) {
				t.Errorf("host outcome %+v outside the host-legal set (%s)", hostRes, tc.want)
			}
			var simRes blockedResult
			simulation.Run(1, func() {
				simRes = pipeBlockedScenario(tc.kind)
			})
			if !tc.legal(simRes) {
				t.Errorf("sim outcome %+v outside the host-legal set (%s); host observed %+v", simRes, tc.want, hostRes)
			}
		})
	}
}

// io.EOF must be the literal EOF at the os.File surface for pipes on
// both legs (Read contract); pin the identity once here since errClass
// folds wrapped and bare EOF together.
func TestDSTConformancePipeEOFIdentity(t *testing.T) {
	check := func(label string) error {
		r, w, err := os.Pipe()
		if err != nil {
			return fmt.Errorf("%s: pipe: %v", label, err)
		}
		defer r.Close()
		w.Close()
		_, rerr := r.Read(make([]byte, 1))
		if rerr != io.EOF && !errors.Is(rerr, io.EOF) {
			return fmt.Errorf("%s: read = %v, want io.EOF", label, rerr)
		}
		return nil
	}
	if err := check("host"); err != nil {
		t.Error(err)
	}
	var simErr error
	simulation.Run(1, func() { simErr = check("sim") })
	if simErr != nil {
		t.Error(simErr)
	}
}
