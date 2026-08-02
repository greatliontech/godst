// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package conformance

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/simulation"
)

// ---------------------------------------------------------------------------
// Outcomes and normalization.

// outcome is one op's normalized observable result. Fields hold only
// values comparable across the host/sim legs: error classes, counts,
// and op-defined state. Never wall-clock values, addresses, or
// descriptor/inode numbers.
type outcome struct {
	Err   string // normalized error class; "" for nil
	N     int    // returned count; -1 when the op has none
	State string // op-defined observable state ("" when none)
}

func (o outcome) String() string {
	return fmt.Sprintf("{err=%q n=%d state=%q}", o.Err, o.N, o.State)
}

// errnoNames is the curated errno sentinel list. An errno outside it
// normalizes to its number (linux/amd64 leg only, so numbers compare).
var errnoNames = map[syscall.Errno]string{
	syscall.ENOENT:        "ENOENT",
	syscall.EEXIST:        "EEXIST",
	syscall.ENOTDIR:       "ENOTDIR",
	syscall.EISDIR:        "EISDIR",
	syscall.ENOTEMPTY:     "ENOTEMPTY",
	syscall.EBADF:         "EBADF",
	syscall.EINVAL:        "EINVAL",
	syscall.ESPIPE:        "ESPIPE",
	syscall.EPIPE:         "EPIPE",
	syscall.EACCES:        "EACCES",
	syscall.EPERM:         "EPERM",
	syscall.ENOSPC:        "ENOSPC",
	syscall.ECONNRESET:    "ECONNRESET",
	syscall.ECONNREFUSED:  "ECONNREFUSED",
	syscall.ETIMEDOUT:     "ETIMEDOUT",
	syscall.EADDRINUSE:    "EADDRINUSE",
	syscall.EADDRNOTAVAIL: "EADDRNOTAVAIL",
	syscall.ENODEV:        "ENODEV",
	syscall.ENXIO:         "ENXIO",
	syscall.EAGAIN:        "EAGAIN",
	syscall.EFBIG:         "EFBIG",
	syscall.ELOOP:         "ELOOP",
	syscall.ENAMETOOLONG:  "ENAMETOOLONG",
	syscall.EXDEV:         "EXDEV",
	syscall.EROFS:         "EROFS",
	syscall.EBUSY:         "EBUSY",
	syscall.EOVERFLOW:     "EOVERFLOW",
}

// errClass normalizes an error to an identity class: the wrapping
// struct's Op field (PathError/LinkError/OpError) plus a fixed probe
// chain of sentinels and syscall.Errno extraction. String comparison of
// error messages appears exactly once, as the recorded exception for
// the DST unsupported-shape fences, which export no sentinel.
func errClass(err error) string {
	if err == nil {
		return ""
	}
	prefix := ""
	var oe *net.OpError
	var le *os.LinkError
	var pe *fs.PathError
	switch {
	case errors.As(err, &oe):
		prefix = "OpError(" + oe.Op + ")/"
	case errors.As(err, &le):
		prefix = "LinkError(" + le.Op + ")/"
	case errors.As(err, &pe):
		prefix = "PathError(" + pe.Op + ")/"
	}
	return prefix + baseErrClass(err)
}

func baseErrClass(err error) string {
	switch {
	case errors.Is(err, net.ErrClosed):
		return "ErrClosed:net"
	case errors.Is(err, os.ErrClosed):
		return "ErrClosed:file"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "ErrDeadlineExceeded"
	case errors.Is(err, os.ErrNoDeadline):
		return "ErrNoDeadline"
	case errors.Is(err, io.EOF):
		return "EOF"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "UnexpectedEOF"
	case errors.Is(err, context.Canceled):
		return "ContextCanceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "ContextDeadlineExceeded"
	case errors.Is(err, fs.ErrInvalid):
		return "ErrInvalid"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if name, ok := errnoNames[errno]; ok {
			return "errno:" + name
		}
		return "errno:#" + strconv.Itoa(int(errno))
	}
	// Recorded exception: the DST fence shapes carry no exported sentinel.
	if strings.Contains(err.Error(), "unsupported under deterministic simulation") {
		return "dst-unsupported"
	}
	// Outside the curated chain the message rides along, so two DISTINCT
	// unknown errors can never silently match. This class only fires for
	// errors the probe chain does not know, where a diff SHOULD surface —
	// host-variable text in it makes the divergence louder, not flaky.
	return "opaque:" + err.Error()
}

// panicClass normalizes a recovered panic value. The DST fences panic
// with the unsupported shape; anything else is opaque (and must match
// across legs to pass, which an unexpected panic never will).
func panicClass(r any) string {
	msg := fmt.Sprint(r)
	if strings.Contains(msg, "unsupported under deterministic simulation") {
		return "panic:dst-unsupported"
	}
	return "panic:opaque"
}

// contentHash is the deterministic digest used for read-back payloads
// in outcome state.
func contentHash(b []byte) string {
	h := fnv.New64a()
	h.Write(b)
	return fmt.Sprintf("fnv:%016x", h.Sum64())
}

// ---------------------------------------------------------------------------
// Ops and worlds.

// op is one step of a differential sequence: pure data (generated once,
// so both legs execute identical sequences by construction) plus an
// execution against a world.
type op struct {
	name string // reproducer line; stable across legs
	// permFromCreate marks stat-shaped ops whose State perm field is
	// still the creating open/mkdir's requested mode (never chmod'd
	// since): the scope of the umask allowlist entry.
	permFromCreate bool
	// writeSize and guarded carry a write op's payload size and
	// deadline-bounded-ness structurally, so allowlist matchers scope on
	// them instead of parsing op names.
	writeSize int
	guarded   bool
	run       func(w *world) outcome
}

// world is the per-leg execution context. Ops address resources through
// slot indices, which stay leg-consistent because both legs run the
// identical sequence (the differ stops at the first divergence, before
// slot tables can drift).
type world struct {
	root  string
	sim   bool // the simulated leg (fd-free settle; the host leg polls real fds)
	files []*os.File
	lns   []net.Listener
	conns []net.Conn
}

func (w *world) path(rel string) string {
	if rel == "" {
		return w.root
	}
	return w.root + "/" + rel
}

func (w *world) close() {
	for _, f := range w.files {
		if f != nil {
			f.Close()
		}
	}
	for _, c := range w.conns {
		if c != nil {
			c.Close()
		}
	}
	for _, l := range w.lns {
		if l != nil {
			l.Close()
		}
	}
}

func runOne(o op, w *world) (out outcome) {
	defer func() {
		if r := recover(); r != nil {
			out = outcome{Err: panicClass(r), N: -1}
		}
	}()
	return o.run(w)
}

func runOps(w *world, ops []op) []outcome {
	outs := make([]outcome, 0, len(ops))
	for _, o := range ops {
		outs = append(outs, runOne(o, w))
	}
	return outs
}

// runOpsHost executes the sequence against the real host (outside any
// run: build-mode inertness makes every primitive the real Linux one).
func runOpsHost(t *testing.T, ops []op) []outcome {
	t.Helper()
	w := &world{root: t.TempDir()}
	defer w.close()
	return runOps(w, ops)
}

// runOpsSim executes the same sequence inside a simulation run.
func runOpsSim(t *testing.T, seed uint64, ops []op) []outcome {
	t.Helper()
	var outs []outcome
	simulation.Run(seed, func() {
		const simRoot = "/tmp/conformance"
		if err := os.MkdirAll(simRoot, 0o777); err != nil {
			panic("conformance: sim root: " + err.Error())
		}
		w := &world{root: simRoot, sim: true}
		defer w.close()
		outs = runOps(w, ops)
	})
	return outs
}

// ---------------------------------------------------------------------------
// The allowlist: machine-checked recorded divergences.

// allowEntry encodes ONE model-vs-host divergence docs/dst/design.md
// records as deliberate. match must accept exactly the recorded shape —
// a permissive matcher would silently allowlist new defects.
type allowEntry struct {
	key        string
	cite       string      // one-line spec citation
	applicable func() bool // nil = always; false skips stale detection
	match      func(o op, host, sim outcome) bool
}

// divergence is a differ finding: the first unallowlisted host/sim
// mismatch of a sequence.
type divergence struct {
	index     int
	op        string
	host, sim outcome
}

// diffOutcomes compares two legs' transcripts op by op. A mismatch is
// checked against the allowlist (recording which entries fired); the
// first unallowlisted mismatch is returned. Comparison stops there:
// past a divergence the legs' resource tables may drift, so later
// mismatches would be noise.
func diffOutcomes(ops []op, host, sim []outcome, allow []allowEntry, fired map[string]int) *divergence {
	for i := range ops {
		h, s := host[i], sim[i]
		if h == s {
			continue
		}
		matched := false
		for _, e := range allow {
			if e.applicable != nil && !e.applicable() {
				continue
			}
			if e.match(ops[i], h, s) {
				if fired != nil {
					fired[e.key]++
				}
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		return &divergence{index: i, op: ops[i].name, host: h, sim: s}
	}
	return nil
}

// reportDivergence fails the test with the minimal reproducer: the op
// sequence up to and including the diverging op, plus the seed.
func reportDivergence(t *testing.T, domain string, seed uint64, ops []op, d *divergence) {
	t.Helper()
	var b strings.Builder
	for i := 0; i <= d.index; i++ {
		marker := "  "
		if i == d.index {
			marker = "=>"
		}
		fmt.Fprintf(&b, "%s [%3d] %s\n", marker, i, ops[i].name)
	}
	t.Errorf("%s seed=%d: unallowlisted host/sim divergence at op %d %q\n  host: %v\n  sim:  %v\nreproducer (op sequence to the divergence):\n%s",
		domain, seed, d.index, d.op, d.host, d.sim, b.String())
}

// staleAllowlistEntries returns the applicable entries that never
// fired across a sweep: the spec (or the model) has outrun the list.
func staleAllowlistEntries(allow []allowEntry, fired map[string]int) []string {
	var stale []string
	for _, e := range allow {
		if e.applicable != nil && !e.applicable() {
			continue
		}
		if fired[e.key] == 0 {
			stale = append(stale, e.key)
		}
	}
	return stale
}

// checkAllowlistCoverage fails the test for every stale entry.
func checkAllowlistCoverage(t *testing.T, allow []allowEntry, fired map[string]int) {
	t.Helper()
	for _, e := range allow {
		if e.applicable != nil && !e.applicable() {
			t.Logf("allowlist entry %q not applicable on this host; skipped (cite: %s)", e.key, e.cite)
		}
	}
	for _, key := range staleAllowlistEntries(allow, fired) {
		t.Errorf("stale allowlist entry %q never fired in the sweep — the spec may have outrun the list", key)
	}
}

// ---------------------------------------------------------------------------
// Sweep configuration and shared helpers.

// sweepSeeds returns the seed set. Default is a bounded sweep; the
// DST_CONFORMANCE_SEEDS environment knob widens it (documented in
// Taskfile.yml). Read on the host leg only, before any run.
func sweepSeeds(t *testing.T) []uint64 {
	n := 4
	if v := os.Getenv("DST_CONFORMANCE_SEEDS"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 {
			t.Fatalf("DST_CONFORMANCE_SEEDS=%q: want a positive integer", v)
		}
		n = p
	}
	seeds := make([]uint64, n)
	for i := range seeds {
		seeds[i] = uint64(i + 1)
	}
	return seeds
}

// pat returns a deterministic payload, fixed at generation time so both
// legs write identical bytes.
func pat(n int, tag byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)*7 + tag
	}
	return b
}

// opNames extracts the reproducer lines (generator-determinism pin).
func opNames(ops []op) []string {
	names := make([]string, len(ops))
	for i, o := range ops {
		names[i] = o.name
	}
	return names
}

// hostUmask is the process umask, read once: the umask allowlist entry
// validates host-created modes against it.
var hostUmask = func() os.FileMode {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return os.FileMode(m)
}()

// parseStatePerm extracts a "perm=NNNN" field from an op State string.
func parseStatePerm(state string) (os.FileMode, string, bool) {
	i := strings.Index(state, "perm=")
	if i < 0 {
		return 0, "", false
	}
	rest := state[i+len("perm="):]
	j := strings.IndexByte(rest, ' ')
	field := rest
	if j >= 0 {
		field = rest[:j]
	}
	v, err := strconv.ParseUint(field, 8, 32)
	if err != nil {
		return 0, "", false
	}
	// Return the state with the perm field blanked, for rest-equality.
	blanked := state[:i] + "perm=*" + rest[len(field):]
	return os.FileMode(v), blanked, true
}

// splitPathErrOp splits a normalized "PathError(<op>)/<rest>" class
// into its op and rest.
func splitPathErrOp(cls string) (opName, rest string, ok bool) {
	const prefix = "PathError("
	if !strings.HasPrefix(cls, prefix) {
		return "", "", false
	}
	i := strings.Index(cls, ")/")
	if i < 0 {
		return "", "", false
	}
	return cls[len(prefix):i], cls[i+2:], true
}

// parseStateInt extracts an integer "key=N" field, returning the state
// with the field blanked.
func parseStateInt(state, key string) (int64, string, bool) {
	i := strings.Index(state, key+"=")
	if i < 0 {
		return 0, "", false
	}
	rest := state[i+len(key)+1:]
	j := strings.IndexByte(rest, ' ')
	field := rest
	if j >= 0 {
		field = rest[:j]
	}
	v, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		return 0, "", false
	}
	blanked := state[:i] + key + "=*" + rest[len(field):]
	return v, blanked, true
}
