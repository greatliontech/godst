// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestUntaggedTextIdenticalToStock is INV-VANILLA's enforcing gate
// (design.md, "Untagged footprint (contract)"): the probe corpus, built
// untagged by this toolchain and by the upstream base release toolchain,
// must produce binaries whose text symbols are instruction-identical modulo
// the allowlist below — every entry a deviation design.md records as
// deliberate, with its class checked mechanically where the class permits,
// and an entry that fires on no symbol failing as stale.
//
// The stock toolchain comes from DST_STOCK_GOROOT (the `test:inert-diff`
// Taskfile leg provides it); without it the test skips with that
// instruction — the leg, not this test, is the enforcement point.
func TestUntaggedTextIdenticalToStock(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the differential build")
	}
	if runtime.GOARCH != "amd64" {
		// The drift bound's store heuristics are amd64 register-model
		// specific; other architectures need their own tuning before this
		// gate can vouch for them.
		t.Skipf("gate is amd64-only for now (GOARCH=%s)", runtime.GOARCH)
	}
	stock := os.Getenv("DST_STOCK_GOROOT")
	if stock == "" {
		t.Skip("DST_STOCK_GOROOT not set; run via `task test:inert-diff`, which locates or installs the upstream base toolchain")
	}
	fork := runtime.GOROOT()
	if v, err := os.ReadFile(filepath.Join(fork, "VERSION")); err == nil {
		base := strings.TrimSpace(strings.Split(string(v), "\n")[0])
		if i := strings.Index(base, "-dst."); i >= 0 {
			base = base[:i]
		}
		out, err := exec.Command(filepath.Join(stock, "bin", "go"), "version").Output()
		if err != nil || !strings.Contains(string(out), base+" ") {
			t.Fatalf("DST_STOCK_GOROOT is not the upstream base %s: %q (%v)", base, out, err)
		}
	}
	corpus, err := filepath.Abs("testdata/vanillacorpus")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	forkBin := filepath.Join(dir, "corpus-fork")
	stockBin := filepath.Join(dir, "corpus-stock")
	build := func(goroot, out string) {
		t.Helper()
		cmd := exec.Command(filepath.Join(goroot, "bin", "go"), "build", "-trimpath", "-o", out, ".")
		cmd.Dir = corpus
		cmd.Env = append(os.Environ(),
			"GOROOT="+goroot, "GOTOOLCHAIN=local", "CGO_ENABLED=0",
			"GOFLAGS=", "GO111MODULE=off", "GOARCH=", "GOOS=", "GOEXPERIMENT=")
		if outp, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build with %s: %v\n%s", goroot, err, outp)
		}
	}
	build(fork, forkBin)
	build(stock, stockBin)

	coverageCheck(t, fork, corpus)

	forkSyms := disassemble(t, fork, forkBin)
	stockSyms := disassemble(t, fork, stockBin)

	if dbg := os.Getenv("DST_VANILLA_DEBUG"); dbg != "" {
		fb, sb := forkSyms[dbg], stockSyms[dbg]
		t.Logf("fork %s: %d lines, calls %v\n%s", dbg, len(fb.lines), fb.calls, strings.Join(fb.lines, "\n"))
		t.Logf("stock %s: %d lines, calls %v\n%s", dbg, len(sb.lines), sb.calls, strings.Join(sb.lines, "\n"))
	}
	fired := make(map[*allowEntry]bool)
	var failures []string
	seen := make(map[string]bool)
	for name, fb := range forkSyms {
		seen[name] = true
		sb, ok := stockSyms[name]
		if ok && (fb.masked == sb.masked || equalRelaxed(fb, sb)) {
			continue // identical modulo addresses/offsets, or within the recorded codegen-drift bound
		}
		e := admit(name)
		if e == nil {
			if !ok {
				failures = append(failures, fmt.Sprintf("fork-only symbol %s (%d instructions) admitted by no recorded class", name, len(fb.lines)))
			} else {
				d := multisetSymDiff(fb.lines, sb.lines)
				if len(d) > 8 {
					d = d[:8]
				}
				failures = append(failures, fmt.Sprintf("symbol %s differs from stock and is admitted by no recorded class; differing lines: %q", name, d))
			}
			continue
		}
		fired[e] = true
		if msg := e.check(name, fb, sb, ok, forkSyms, stockSyms); msg != "" {
			failures = append(failures, fmt.Sprintf("%s (class %s): %s", name, e.class, msg))
		}
	}
	for name, sb := range stockSyms {
		if seen[name] {
			continue
		}
		e := admit(name)
		if e == nil {
			failures = append(failures, fmt.Sprintf("stock-only symbol %s (%d instructions): the fork dropped or renamed it and no recorded class admits that", name, len(sb.lines)))
			continue
		}
		fired[e] = true
		if msg := e.check(name, symbol{}, sb, false, forkSyms, stockSyms); msg != "" {
			failures = append(failures, fmt.Sprintf("%s (class %s): %s", name, e.class, msg))
		}
	}
	for body := range splitBodies {
		if strings.HasSuffix(body, ".abi0") {
			continue // rides its base name
		}
		if _, ok := forkSyms[body]; !ok {
			if _, abi0 := forkSyms[body+".abi0"]; abi0 {
				continue // an assembly entry links through its ABI0 wrapper
			}
			failures = append(failures, fmt.Sprintf("split body %s is not linked by the corpus — its deviation is unverified; exercise it", body))
		}
	}
	for i := range allowlist {
		if !fired[&allowlist[i]] {
			failures = append(failures, fmt.Sprintf("stale allowlist entry %q (class %s): fired on no symbol — the deviation it admits no longer exists; remove it", allowlist[i].pattern, allowlist[i].class))
		}
	}
	// No dst-named residue anywhere in the untagged fork binary — checked on
	// the raw text captured before operand canonicalization, so a symbolized
	// data reference cannot hide behind the ADDR rewrite.
	for name, fb := range forkSyms {
		for _, l := range fb.dstRaw {
			failures = append(failures, fmt.Sprintf("untagged %s references a dst symbol: %s", name, strings.TrimSpace(l)))
		}
	}
	if len(failures) > 0 {
		t.Fatalf("untagged text diverges from the upstream base beyond the recorded deviations:\n  %s", strings.Join(failures, "\n  "))
	}
}

// coverageCheck pins the corpus property (design.md): the corpus's
// transitive import closure covers every upstream-present std package the
// dst delta modifies. The live delta path set is consulted through git when
// available (base tag from VERSION); without git the embedded list — kept in
// step by this very check wherever git IS available — is the fallback, and
// the skip of the live consultation is reported, not silent.
func coverageCheck(t *testing.T, fork, corpus string) {
	t.Helper()
	required := []string{
		"crypto/internal/sysrand", "internal/runtime/maps", "internal/sync",
		"net", "os", "os/signal", "os/user", "runtime", "sync", "syscall",
		"testing", "time",
	}
	cmd := exec.Command(filepath.Join(fork, "bin", "go"), "list", "-deps", ".")
	cmd.Dir = corpus
	cmd.Env = append(os.Environ(), "GOROOT="+fork, "GOTOOLCHAIN=local", "GO111MODULE=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := make(map[string]bool)
	for _, p := range strings.Fields(string(out)) {
		deps[p] = true
	}
	for _, p := range required {
		if !deps[p] {
			t.Errorf("corpus closure misses dst-modified package %s", p)
		}
	}
	// Live delta consultation: the embedded list must not lag the tree.
	base, err := os.ReadFile(filepath.Join(fork, "VERSION"))
	if err != nil {
		t.Logf("coverage: VERSION unreadable (%v); embedded list not cross-checked", err)
		return
	}
	tag := strings.TrimSpace(strings.Split(string(base), "\n")[0])
	if i := strings.Index(tag, "-dst."); i >= 0 {
		tag = tag[:i]
	}
	git := exec.Command("git", "-C", fork, "diff", "--name-only", tag, "HEAD", "--", "src")
	gout, err := git.Output()
	if err != nil {
		if os.Getenv("DST_REQUIRE_LIVE_DELTA") != "" {
			t.Errorf("coverage: live delta REQUIRED but unavailable (git diff %s: %v)", tag, err)
		} else {
			t.Logf("coverage: live delta unavailable (git diff %s: %v); embedded list not cross-checked", tag, err)
		}
		return
	}
	req := make(map[string]bool)
	for _, p := range required {
		req[p] = true
	}
	for _, f := range strings.Split(string(gout), "\n") {
		if !strings.HasPrefix(f, "src/") || strings.Contains(f, "_test.go") ||
			strings.Contains(f, "/testdata/") || strings.HasPrefix(f, "src/cmd/") {
			continue
		}
		if !strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, ".s") {
			continue
		}
		pkg := filepath.Dir(strings.TrimPrefix(f, "src/"))
		if pkg == "." || strings.HasPrefix(pkg, "testing/simulation") {
			continue
		}
		if _, err := os.Stat(filepath.Join(os.Getenv("DST_STOCK_GOROOT"), "src", pkg)); err != nil {
			continue // godst-only package: outside the corpus property by contract
		}
		if !req[pkg] && !deps[pkg] {
			t.Errorf("dst delta modifies %s but the corpus closure does not reach it (and the embedded list lags)", pkg)
		}
	}
}

// rawDstRef matches any reference to a dst-named symbol — data operand,
// call, or jump — on the RAW instruction text, before operand
// canonicalization can hide a symbolized data reference.
var rawDstRef = regexp.MustCompile(`([\w/]+\.|\(\*?[\w\[\]]+\)\.)dst\w*[+(]|runtime\.dst|\(\*?dst\w+\)`)

type symbol struct {
	lines  []string // normalized instructions (position/address/bytes stripped)
	masked string   // lines joined, hex operands masked
	calls  map[string]bool
	dstRaw []string // raw lines referencing a dst-named symbol, captured before operand canonicalization
}

// disassemble dumps every text symbol of bin with tool's objdump and
// normalizes each instruction: the position, address, and byte columns are
// stripped; masked additionally replaces hex operands so layout offsets and
// branch targets compare equal.
func disassemble(t *testing.T, goroot, bin string) map[string]symbol {
	t.Helper()
	cmd := exec.Command(filepath.Join(goroot, "bin", "go"), "tool", "objdump", bin)
	cmd.Env = append(os.Environ(), "GOROOT="+goroot, "GOTOOLCHAIN=local")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("objdump %s: %v", bin, err)
	}
	syms := make(map[string]symbol)
	var name string
	var cur symbol
	flush := func() {
		if name != "" {
			cur.masked = maskHex(strings.Join(cur.lines, "\n"))
			syms[name] = cur
		}
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "TEXT ") {
			flush()
			// Generic shape names contain spaces; the name is everything
			// between "TEXT " and the final "(SB)".
			name = line[len("TEXT "):]
			if i := strings.LastIndex(name, "(SB)"); i >= 0 {
				name = name[:i]
			}
			cur = symbol{calls: make(map[string]bool)}
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || name == "" {
			continue
		}
		text := strings.Join(f[3:], " ")
		if rawDstRef.MatchString(text) {
			cur.dstRaw = append(cur.dstRaw, text)
		}
		if strings.HasPrefix(text, "NOP") {
			// Alignment/inline-marker nops shift freely with layout; they
			// carry no semantics and are excluded from every comparison.
			continue
		}
		if strings.HasPrefix(text, "JMP ") && strings.Contains(text, "(SB)") {
			// The morestack epilogue tail-jumps to the symbol itself; a split
			// body self-references its split name where stock names the stock
			// symbol — the same "modulo the call-target name" admission.
			target := strings.TrimSuffix(strings.Fields(text)[1], "(SB)")
			if stockName, isSplit := splitBodies[target]; isSplit {
				text = strings.Replace(text, target+"(SB)", stockName+"(SB)", 1)
			}
		}
		if strings.HasPrefix(text, "CALL ") && strings.Contains(text, "(SB)") {
			target := strings.TrimSuffix(strings.Fields(text)[1], "(SB)")
			cur.calls[target] = true
			// A fork caller that inlined a split wrapper calls the split body
			// where stock calls the stock name — the class admits callers
			// "modulo the call-target name alone", so canonicalize the target.
			if stockName, isSplit := splitBodies[target]; isSplit {
				text = strings.Replace(text, target+"(SB)", stockName+"(SB)", 1)
			}
		} else if !strings.HasPrefix(text, "JMP ") {
			// objdump symbolizes a PC-relative operand only when the target
			// happens to land on a symbol, so the same instruction prints as
			// `LEAQ 0x...(IP)` in one binary and `LEAQ sym(SB)` in the
			// other. Canonicalize every non-branch memory operand; branch
			// targets keep their (semantic) symbol names.
			text = symOperand.ReplaceAllString(text, "ADDR")
		}
		cur.lines = append(cur.lines, text)
	}
	flush()
	return syms
}

var hexRe = regexp.MustCompile(`0x[0-9a-f]+`)

var symOperand = regexp.MustCompile(`([\w./:*()\[\]<>@$-]+\(SB\)|0x[0-9a-f]+\(IP\))`)

func maskHex(s string) string { return hexRe.ReplaceAllString(s, "HEX") }

// equalRelaxed is the recorded codegen-drift bound (design.md admission
// rule): the recorded data layouts shift register allocation, instruction
// scheduling, block order, and size-class selection in functions that touch
// the widened structs. Within this bound a difference is a layout
// consequence, not residue: the callee sets must be equal (modulo the
// barrier and size-class-specialized-malloc families), the fork side must
// reference no dst symbol, the instruction counts may differ by at most 3,
// and the mnemonic multisets by at most 6 entries.
func equalRelaxed(fb, sb symbol) bool {
	if len(calleeSetDiff(fb, sb, nil)) > 0 || len(calleeSetDiff(sb, fb, nil)) > 0 {
		return false
	}
	if len(fb.dstRaw) > 0 {
		return false
	}
	d := len(fb.lines) - len(sb.lines)
	if d < -3 || d > 3 {
		return false
	}
	if mnemonicSymDiff(fb.lines, sb.lines) > 6 {
		return false
	}
	// The lines that differ may shuffle registers and stack spills. Pair
	// fork-extra and stock-extra lines that are equal once register names are
	// masked — pure register renames. An UNPAIRED memory store to a
	// non-stack base is the signature of field residue (a hook clearing
	// per-object DST words), not of register allocation — reject it.
	forkExtra, stockExtra := splitSymDiff(fb.lines, sb.lines)
	forkLeft, _ := cancelRegRenames(forkExtra, stockExtra)
	for _, l := range forkLeft {
		// LEA computes an address without touching memory — pure register
		// arithmetic, register-allocation freedom.
		if strings.HasPrefix(l, "LEA") {
			continue
		}
		// Any fork-side unmatched memory touch — register-base or a
		// canonicalized global (ADDR) operand — is potential residue: a
		// field load/store or a package-var guard.
		if nonStackMem.MatchString(l) || strings.Contains(l, "ADDR") {
			return false
		}
	}
	return true
}

var regToken = regexp.MustCompile(`\b(AX|BX|CX|DX|SI|DI|BP|R8|R9|R10|R11|R12|R13|R14|R15|X[0-9]+)\b`)

// cancelRegRenames pairs fork-extra against stock-extra lines that are
// equal once register names are masked, returning what remains UNPAIRED on
// each side. Residue can only live on the fork side; stock-side leftovers
// are stock's own text and never rejected on their own.
func cancelRegRenames(a, b []string) (aLeft, bLeft []string) {
	bm := make(map[string]int)
	for _, l := range b {
		bm[regToken.ReplaceAllString(l, "R")]++
	}
	for _, l := range a {
		k := regToken.ReplaceAllString(l, "R")
		if bm[k] > 0 {
			bm[k]--
			continue
		}
		aLeft = append(aLeft, l)
	}
	for _, l := range b {
		k := regToken.ReplaceAllString(l, "R")
		if bm[k] > 0 {
			bm[k]--
			bLeft = append(bLeft, l)
		}
	}
	return aLeft, bLeft
}

func splitSymDiff(a, b []string) (aExtra, bExtra []string) {
	count := make(map[string]int)
	for _, l := range a {
		count[maskHex(l)]++
	}
	for _, l := range b {
		count[maskHex(l)]--
	}
	for l, n := range count {
		for ; n > 0; n-- {
			aExtra = append(aExtra, l)
		}
		for ; n < 0; n++ {
			bExtra = append(bExtra, l)
		}
	}
	return
}

// nonStackMem matches an instruction touching memory through a non-stack
// base register (SP and the BP frame pointer are register-allocation
// freedom), as source or destination. An UNPAIRED such line in a drift diff
// is the signature of field residue — a hook loading or clearing per-object
// DST words — not of register allocation.
var nonStackMem = regexp.MustCompile(`(HEX|0x[0-9a-f]+|-?[0-9]+)?\((AX|BX|CX|DX|SI|DI|R8|R9|R10|R11|R12|R13|R14|R15)\)`)

// plainDispStore matches a store whose destination is a plain displacement
// off a non-stack base — the shape of a struct-field write. Mask/array
// construction writes are index-scaled (`(R)(R*n)`) and recognizably
// different, so the admitted layout classes can keep their loops while a
// field-clearing hook still fails.
// The mnemonic prefix keeps this to WRITES (CMP/TEST/UCOMI read their
// memory operand). Only IMMEDIATE-source stores are rejected: a hand-written
// hook clears fields with constants (`gp.dstX = 0` → `MOVQ $0x0, off(REG)`),
// while a widened record's extra copy words move through registers (each
// unpaired store has a matching unpaired load) and its zeroing goes through
// the X15 zero register — both layout consequences.
// immStore matches an immediate store to a PLAIN displacement off a
// non-stack base — the only shape the compiler emits for a hand-written
// field clear (`gp.dstX = 0`): struct fields are fixed displacements from a
// base pointer. An INDEXED immediate store (`$v, off(R)(R*n)`) is an
// array-element write inside a loop — the extracted finalizer/cleanup loops
// clear their entries that way — and is class latitude, not residue.
var immStore = regexp.MustCompile(`^(MOV[A-Z]*|AND[A-Z]*|OR[A-Z]*|XOR[A-Z]*|BT[SRC][A-Z]*) \$-?(HEX|0x[0-9a-f]+|[0-9]+), (HEX|0x[0-9a-f]+|-?[0-9]+)?\((AX|BX|CX|DX|SI|DI|R8|R9|R10|R11|R12|R13|R14|R15)\)$`)

var stackGuardCheck = regexp.MustCompile(`^CMPQ SP, (0x10|HEX)\(R14\)$`)

// memDest: the last operand is a memory reference (register-base, indexed,
// or canonicalized global) on a non-compare mnemonic — a memory write.
var memDest = regexp.MustCompile(`^(?:LOCK )?(?:MOV|AND|OR|XOR|ADD|SUB|INC|DEC|BT|XCHG|CMPXCHG)[A-Z]*\b.*(?:\((?:AX|BX|CX|DX|SI|DI|BP|R8|R9|R10|R11|R12|R13|R14|R15)\)(?:\((?:AX|BX|CX|DX|SI|DI|BP|R8|R9|R10|R11|R12|R13|R14|R15)\*[0-9]+\))?|ADDR)$`)

var regSrcStore = regexp.MustCompile(`^(MOV[A-Z]*|AND[A-Z]*|OR[A-Z]*|XOR[A-Z]*|BT[SRC][A-Z]*) (AX|BX|CX|DX|SI|DI|R8|R9|R10|R11|R12|R13|R14|R15), (HEX|0x[0-9a-f]+|-?[0-9]+)?\((AX|BX|CX|DX|SI|DI|R8|R9|R10|R11|R12|R13|R14|R15)\)$`)

// classRelaxed is the admitted-class text bound (design.md admission rule):
// wider numeric drift than the universal bound — widened-record spills and
// mask loops are large — but an unpaired plain-displacement store to a
// non-stack base still fails, in every class. Unpaired plain loads and
// indexed stores are the classes' recorded latitude.
func classRelaxed(fLines, sLines []string, fDst []string, maxD, maxM int) string {
	if len(fDst) > 0 {
		return fmt.Sprintf("references a dst symbol: %s", fDst[0])
	}
	d := len(fLines) - len(sLines)
	if d < -maxD || d > maxD {
		return fmt.Sprintf("instruction count drifts by %d (bound %d)", d, maxD)
	}
	if m := mnemonicSymDiff(fLines, sLines); m > maxM {
		return fmt.Sprintf("mnemonic drift %d exceeds bound %d", m, maxM)
	}
	fe, se := splitSymDiff(fLines, sLines)
	forkLeft, _ := cancelRegRenames(fe, se)
	loadsInto := make(map[string]int)
	var stores []string
	for _, l := range forkLeft {
		if strings.HasPrefix(l, "LEA") {
			continue
		}
		switch {
		case immStore.MatchString(l):
			// A hand-written field clear or flag set.
			return fmt.Sprintf("unpaired field store in the diff: %q", l)
		case strings.Contains(l, "ADDR") && !strings.HasPrefix(l, "CALL") && !strings.HasPrefix(l, "JMP"):
			// A canonicalized global operand — a package-var guard or store.
			return fmt.Sprintf("unpaired global access in the diff: %q", l)
		case regSrcStore.MatchString(l):
			stores = append(stores, l)
		case nonStackMem.MatchString(l):
			// A widened-record copy word being read; note its destination
			// register.
			f := strings.Fields(l)
			loadsInto[f[len(f)-1]]++
		}
	}
	for _, l := range stores {
		// The classes' latitude is copy words: a copy loads and stores the
		// SAME register, so every unmatched register-source store must have
		// an unmatched load into exactly that register. A store whose source
		// register has no feeding load is a hook writing computed state (the
		// save-restore residue shape).
		f := strings.Fields(l)
		src := strings.TrimSuffix(f[1], ",")
		if loadsInto[src] == 0 {
			return fmt.Sprintf("unpaired register-source field store with no feeding load: %q", l)
		}
		loadsInto[src]--
	}
	return ""
}

func multisetSymDiff(a, b []string) []string {
	count := make(map[string]int)
	for _, l := range a {
		count[maskHex(l)]++
	}
	for _, l := range b {
		count[maskHex(l)]--
	}
	var out []string
	for l, n := range count {
		if n != 0 {
			out = append(out, l)
		}
	}
	return out
}

func mnemonics(lines []string) map[string]int {
	m := make(map[string]int)
	for _, l := range lines {
		m[strings.Fields(l)[0]]++
	}
	return m
}

func mnemonicSymDiff(a, b []string) int {
	ma, mb := mnemonics(a), mnemonics(b)
	n := 0
	for k, v := range ma {
		if d := v - mb[k]; d > 0 {
			n += d
		}
	}
	for k, v := range mb {
		if d := v - ma[k]; d > 0 {
			n += d
		}
	}
	return n
}

// barrier forms are a recorded layout consequence (a widened record selects
// a different write-barrier helper), so callee-set comparisons treat them as
// one equivalence class.
var barrierRe = regexp.MustCompile(`^(runtime\.)?(gcWriteBarrier[0-9]*|wbZero|wbMove)$`)

// sizeClassRe: the size-class-specialized malloc entry points — a widened
// struct selects a different one, a pure layout consequence.
var sizeClassRe = regexp.MustCompile(`^runtime\.mallocgc(SmallScanNoHeader|SmallNoscan|Tiny)?SC[0-9]+$`)

func calleeSetDiff(fb, sb symbol, drop map[string]bool) []string {
	var extra []string
	norm := func(m map[string]bool) map[string]bool {
		out := make(map[string]bool)
		for c := range m {
			if stockName, isSplit := splitBodies[c]; isSplit {
				c = stockName // split call targets compare under the stock name
			}
			if barrierRe.MatchString(c) {
				c = "WB"
			} else if sizeClassRe.MatchString(c) {
				c = "MALLOC_SC"
			}
			out[c] = true
		}
		return out
	}
	fc, sc := norm(fb.calls), norm(sb.calls)
	for c := range fc {
		if !sc[c] && !drop[c] {
			extra = append(extra, c)
		}
	}
	return extra
}

type allowEntry struct {
	pattern string
	re      *regexp.Regexp
	class   string
	// check returns "" when the observed difference is of the recorded
	// class; ok reports whether the symbol exists on both sides.
	check func(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string
}

func init() {
	for i := range allowlist {
		allowlist[i].re = regexp.MustCompile(allowlist[i].pattern)
	}
}

func admit(name string) *allowEntry {
	for i := range allowlist {
		if allowlist[i].re.MatchString(name) {
			return &allowlist[i]
		}
	}
	return nil
}

// splitBodies maps each fork-side split-body symbol to the stock symbol
// whose text it must equal (design.md, "Fence-wrapper splits").
var splitBodies = map[string]string{
	"syscall.closeFD":              "syscall.Close",
	"syscall.fstatFD":              "syscall.Fstat",
	"syscall.seekFD":               "syscall.Seek",
	"syscall.fsync":                "syscall.Fsync",
	"syscall.fdatasync":            "syscall.Fdatasync",
	"syscall.flock":                "syscall.Flock",
	"syscall.kill":                 "syscall.Kill",
	"syscall.madvise":              "syscall.Madvise",
	"syscall.mprotect":             "syscall.Mprotect",
	"syscall.rawGettimeofday":      "syscall.gettimeofday",
	"syscall.rawGettimeofday.abi0": "syscall.gettimeofday.abi0",
}

// extractions maps each shared-helper extraction's caller to its extracted
// helper (design.md, "Shared-helper extractions").
var extractions = map[string]string{
	"runtime.runFinalizers":  "runtime.runFinqBlocks",
	"runtime.runCleanups":    "runtime.runCleanupBlock",
	"runtime.queuefinalizer": "runtime.finAllocBlockLocked",
	"runtime.GC":             "runtime.gcForce",
}

func checkExtractionCaller(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string {
	if !ok {
		return "expected on both sides"
	}
	helperName := extractions[name]
	// Pairwise admission: caller (minus the extraction call) plus helper must
	// be the stock body modulo the class latitude, and the PAIR's callee set
	// must match stock's — calls the extraction moved into the helper are not
	// "dropped".
	helper, hok := fork[helperName]
	if !hok {
		return fmt.Sprintf("extracted helper %s missing from the fork binary", helperName)
	}
	pairSym := symbol{calls: make(map[string]bool), dstRaw: append(append([]string{}, fb.dstRaw...), helper.dstRaw...)}
	for _, l := range fb.lines {
		if strings.Contains(l, helperName+"(SB)") {
			continue
		}
		pairSym.lines = append(pairSym.lines, l)
	}
	pairSym.lines = append(pairSym.lines, helper.lines...)
	for c := range fb.calls {
		if c != helperName {
			pairSym.calls[c] = true
		}
	}
	for c := range helper.calls {
		pairSym.calls[c] = true
	}
	if extra := calleeSetDiff(pairSym, sb, nil); len(extra) > 0 {
		return fmt.Sprintf("pair makes calls stock never makes: %v", extra)
	}
	if extra := calleeSetDiff(sb, pairSym, abiMicro); len(extra) > 0 {
		return fmt.Sprintf("pair dropped stock calls: %v", extra)
	}
	if os.Getenv("DST_VANILLA_DELTAS") != "" {
		fmt.Printf("DELTA pair %s: %d\n", name, len(pairSym.lines)-len(sb.lines))
	}
	if want, pinned := exactDeltas[name]; pinned {
		if d := len(pairSym.lines) - len(sb.lines); d != want {
			return fmt.Sprintf("pair instruction-count delta %d, pinned %d — re-measure only with review", d, want)
		}
	}
	return classRelaxed(pairSym.lines, sb.lines, pairSym.dstRaw, 48, 64)
}

// exactDeltas pins the fork-vs-stock instruction-count delta of every
// non-generic admitted runtime symbol (pairs measured as caller minus the
// extraction call plus helper). The compiler is deterministic, so these are
// stable per base; a port re-measures them under review. The pin closes the
// remaining mimicry gap: a residue store that copies the shape of a legit
// widened-record store still adds a line. Generic instantiations churn with
// the corpus and keep only the bounded class latitude — that residual blind
// spot is recorded in design.md.
var exactDeltas = map[string]int{
	"runtime.addCleanup":              2,
	"runtime.(*cleanupQueue).enqueue": 17,
	"runtime.freeSpecial":             3,
	"runtime.gcinit":                  21,
	// pairs: caller minus the extraction call, plus helper, vs stock
	"runtime.runFinalizers":  7,
	"runtime.GC":             8,
	"runtime.runCleanups":    15,
	"runtime.queuefinalizer": 21,
}

// abiMicro: the availability class lets any admitted symbol inline these
// micro-bodies away where stock calls them.
var abiMicro = map[string]bool{"internal/abi.TypeOf": true, "internal/abi.NoEscape": true}

func checkForkOnly(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string {
	if ok || len(fb.lines) == 0 {
		return "expected fork-only"
	}
	// A helper's text bound rides its caller's pairwise check, so the caller
	// must be in the binary too.
	for caller, helper := range extractions {
		if helper == name {
			if _, present := fork[caller]; !present {
				return fmt.Sprintf("extracted helper without its caller %s in the binary — the pairwise check cannot run", caller)
			}
			return ""
		}
	}
	return "helper not named in the extractions map"
}

var allowlist = []allowEntry{
	// Shared-helper extractions: the caller differs by the one call, the
	// helper is fork-only (its body-vs-stock-loop identity is carried by the
	// caller's callee-set check plus the recorded clause).
	{pattern: `^runtime\.(runFinalizers|runCleanups|queuefinalizer|GC)$`, class: "extraction-caller", check: checkExtractionCaller},
	{pattern: `^runtime\.(runFinqBlocks|runCleanupBlock|finAllocBlockLocked|gcForce)$`, class: "extraction-helper", check: checkForkOnly},
	// Fence-wrapper splits: the split body must be instruction-identical to
	// the stock symbol it replaces; the stock-named wrapper (where it
	// survives inlining) may only call the split body.
	{pattern: `^syscall\.(closeFD|fstatFD|seekFD|fsync|fdatasync|flock|kill|madvise|mprotect|rawGettimeofday(\.abi0)?)$`, class: "split-body",
		check: func(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string {
			if ok {
				return "expected fork-only"
			}
			counterpart, exists := stock[splitBodies[name]]
			if !exists {
				return fmt.Sprintf("stock has no %s to compare the split body against", splitBodies[name])
			}
			if fb.masked != counterpart.masked {
				return fmt.Sprintf("split body is not instruction-identical to stock's %s", splitBodies[name])
			}
			return ""
		}},
	{pattern: `^syscall\.(Close|Fstat|Seek|Fsync|Fdatasync|Flock|Kill|Madvise|Mprotect|gettimeofday(\.abi0)?)$`, class: "split-wrapper",
		check: func(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string {
			if len(fb.lines) == 0 {
				return "" // wrapper fully inlined away: the split body carries the text
			}
			for c := range fb.calls {
				if splitBodies[c] != name && !barrierRe.MatchString(c) && c != "runtime.morestack_noctxt.abi0" {
					return fmt.Sprintf("wrapper calls %s, not its own split body", c)
				}
			}
			if len(fb.dstRaw) > 0 {
				return fmt.Sprintf("wrapper references a dst symbol: %s", fb.dstRaw[0])
			}
			// A wrapper is prologue + one call + epilogue: it may touch
			// registers and its own stack frame, nothing else. Any other
			// memory access — a global, a field — is fence residue in the
			// likeliest hand-written site.
			for _, l := range fb.lines {
				if strings.HasPrefix(l, "CALL") || strings.HasPrefix(l, "JMP") || strings.HasPrefix(l, "LEA") {
					continue
				}
				if stackGuardCheck.MatchString(l) {
					continue // the canonical stack-growth prologue reads g's stackguard
				}
				if nonStackMem.MatchString(l) || strings.Contains(l, "ADDR") {
					return fmt.Sprintf("wrapper touches non-stack memory: %q", l)
				}
			}
			return ""
		}},
	// Layout consequences: by-value cleanupFn widening, callback-record
	// constants, and GC mask construction. Callee sets must match modulo the
	// barrier-form equivalence class.
	{pattern: `^runtime\.(addCleanup|AddCleanup\[.*|\(\*cleanupQueue\)\.enqueue|freeSpecial|gcinit)$`, class: "layout",
		check: func(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string {
			if !ok {
				return "expected on both sides"
			}
			if extra := calleeSetDiff(fb, sb, nil); len(extra) > 0 {
				return fmt.Sprintf("calls stock never makes: %v", extra)
			}
			if extra := calleeSetDiff(sb, fb, abiMicro); len(extra) > 0 {
				return fmt.Sprintf("dropped stock calls: %v", extra)
			}
			if os.Getenv("DST_VANILLA_DELTAS") != "" {
				fmt.Printf("DELTA %s: %d\n", name, len(fb.lines)-len(sb.lines))
			}
			if want, pinned := exactDeltas[name]; pinned {
				if d := len(fb.lines) - len(sb.lines); d != want {
					return fmt.Sprintf("instruction-count delta %d, pinned %d — re-measure only with review", d, want)
				}
			}
			return classRelaxed(fb.lines, sb.lines, fb.dstRaw, 48, 64)
		}},
	// Export-data inline-body availability: stock leaves abi micro-calls;
	// the fork inlines them. Fork callees must be a subset of stock's with
	// only those two missing; the abi symbols themselves go one-sided.
	{pattern: `^internal/abi\.(TypeOf|NoEscape)$`, class: "availability-symbol",
		check: func(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string {
			if ok || len(sb.lines) == 0 {
				return "expected stock-only"
			}
			return ""
		}},
	{pattern: `^(internal/)?sync\.`, class: "availability-substitution",
		check: func(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string {
			if !ok {
				return "expected on both sides"
			}
			if extra := calleeSetDiff(fb, sb, nil); len(extra) > 0 {
				return fmt.Sprintf("calls stock never makes: %v", extra)
			}
			for c := range sb.calls {
				if !fb.calls[c] && c != "internal/abi.TypeOf" && c != "internal/abi.NoEscape" && !barrierRe.MatchString(c) {
					return fmt.Sprintf("dropped stock call %s (only the abi micro-bodies may be inlined away)", c)
				}
			}
			return classRelaxed(fb.lines, sb.lines, fb.dstRaw, 12, 16)
		}},
	// Autogenerated equality functions renamed by a changed layout's hash.
	// Their bodies are compiler-generated comparisons: loads and CMPs only,
	// so any store or dst reference inside one is residue, not generation.
	{pattern: `^type:\.eq\.`, class: "eq-rename",
		check: func(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string {
			if len(fb.dstRaw) > 0 {
				return fmt.Sprintf("references a dst symbol: %s", fb.dstRaw[0])
			}
			for _, l := range fb.lines {
				// Generation emits loads and CMPs only: any non-LEA line
				// whose destination operand is memory is residue.
				if !strings.HasPrefix(l, "LEA") && memDest.MatchString(l) {
					return fmt.Sprintf("memory write inside an equality function: %q", l)
				}
			}
			return ""
		}},
}
