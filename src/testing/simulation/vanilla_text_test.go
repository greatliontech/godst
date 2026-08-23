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
	"sort"
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
//
// The comparison is arch-profiled: each supported GOARCH carries its own
// normalization and store-shape heuristics (registers, displacement forms,
// the global-reference and stack-guard shapes). DST_VANILLA_GOARCH cross-
// builds the corpus for another profiled architecture — the CI enforcement
// points run natively, the knob exists for profile derivation and
// debugging on a development host.
func TestUntaggedTextIdenticalToStock(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the differential build")
	}
	goarch := os.Getenv("DST_VANILLA_GOARCH")
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	prof = profiles[goarch]
	if prof == nil {
		// The drift-bound store heuristics are register-model specific;
		// an architecture needs its own tuned profile before this gate
		// can vouch for it.
		var have []string
		for a := range profiles {
			have = append(have, a)
		}
		t.Skipf("no normalization profile for GOARCH=%s; profiled: %v", goarch, have)
	}
	allowlist = buildAllowlist(prof)
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
			"GOFLAGS=", "GO111MODULE=off", "GOARCH="+goarch, "GOOS=linux", "GOEXPERIMENT=")
		if outp, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build with %s: %v\n%s", goroot, err, outp)
		}
	}
	build(fork, forkBin)
	build(stock, stockBin)

	coverageCheck(t, fork, corpus, goarch)

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
	for body := range prof.splitBodies {
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
func coverageCheck(t *testing.T, fork, corpus, goarch string) {
	t.Helper()
	required := []string{
		"crypto/internal/sysrand", "internal/runtime/maps", "internal/sync",
		"net", "os", "os/signal", "os/user", "runtime", "sync", "syscall",
		"testing", "time",
	}
	// The closure is computed for the arch under comparison — under the
	// DST_VANILLA_GOARCH cross knob the host's closure could differ from
	// the target's and silently satisfy the property.
	cmd := exec.Command(filepath.Join(fork, "bin", "go"), "list", "-deps", ".")
	cmd.Dir = corpus
	cmd.Env = append(os.Environ(), "GOROOT="+fork, "GOTOOLCHAIN=local", "GO111MODULE=off",
		"GOARCH="+goarch, "GOOS=linux")
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

// archProfile carries everything about the comparison that depends on the
// target's register model and objdump syntax. The comparison rules
// themselves — the universal drift bound, the class latitudes, the residue
// rejections — are shared; the profile only teaches them each
// architecture's shapes.
type archProfile struct {
	// mask normalizes one instruction's operands so layout-shifted
	// offsets, immediates, and branch displacements compare equal. On
	// amd64 objdump prints these hex; on arm64 decimal, and a zero
	// displacement prints as a bare (Rn) where a nonzero one prints
	// N(Rn) — the mask rewrites both to one form, so the large-offset
	// store sequence the widened layouts force through the assembler
	// temp (ADD $off, Rn, R27; store (R27)) pairs against stock's
	// direct N(Rn) store under register-rename cancelling.
	mask func(string) string
	// canonData canonicalizes non-branch PC-relative data operands to
	// ADDR: amd64's symbolization-dependent `sym(SB)`/`0x...(IP)`
	// forms, arm64's ADRP page offsets (which flip sign when a global
	// lands on the other side of the code).
	canonData func(string) string
	// postNorm, where non-nil, runs over a symbol's normalized lines.
	// arm64 folds each `ADRP ADDR, Rx` into the immediately following
	// access through `(Rx)`, rewriting that operand to ADDR — a global
	// access then carries the same ADDR marker as amd64's RIP-relative
	// form, so the unpaired-global rejections see arm64 globals too
	// (an unfolded ADRP — a global's address taken, not accessed — is
	// the LEA analogue and stays subject to addrArith).
	postNorm func([]string) []string
	// regToken masks the allocatable registers for rename cancelling.
	// Role registers stay distinct: on arm64, ZR (a ZR-source store is
	// the field-clear signature), R28 (g), R29 (FP), R30 (LR).
	regToken *regexp.Regexp
	// nonStackMem matches a memory access through a non-stack base —
	// as source or destination — the shape of field residue.
	nonStackMem *regexp.Regexp
	// immStore matches the store shape the compiler emits for a
	// hand-written field clear: an immediate store to a plain
	// displacement on amd64, a ZR-source store on arm64 (arm64 cannot
	// store an immediate; zero goes through ZR, any other constant
	// through a register and the copy-word rule).
	immStore *regexp.Regexp
	// regSrcStore matches a register-source store to a plain
	// displacement (subject to the copy-word feeding-load rule).
	regSrcStore *regexp.Regexp
	// zeroPairStore, where non-nil, matches the compiler's bulk
	// zeroing of ADJACENT widened-record words (arm64's STP (ZR, ZR));
	// admitted as class latitude. Recorded blind spot: a hook clearing
	// two adjacent fields merges into the same shape (the amd64
	// analogue is X15-based zeroing, recorded in design.md).
	zeroPairStore *regexp.Regexp
	// addrArith matches pure address arithmetic that never touches
	// memory — amd64's LEA, arm64's ADRP page computation — exempt in
	// the CLASS latitude. (The universal bound stays strict: there an
	// unpaired ADRP is rejected through its canonicalized ADDR
	// operand, while amd64's LEA stays exempt — LEA is the common way
	// spills shift, ADRP only appears for globals.)
	addrArith *regexp.Regexp
	// aggregateStores selects the class store rule. amd64 (false):
	// each unmatched register-source store needs an unmatched load
	// into exactly its source register (the copy-word rule). arm64
	// (true): store conservation in aggregate — fork-side unmatched
	// store words may not exceed stock-side unmatched store words plus
	// fork-side unmatched load words. arm64's record writes reshape
	// freely between STP pairs fed by live argument registers and
	// spill-slot round-trips, so the per-register feeding rule fails
	// legitimate reshapes; conservation still fails residue, which
	// ADDS stores, and the exact-delta pins back it up (recorded in
	// design.md with the class blind spot).
	aggregateStores bool
	// stackGuard matches the stack-growth prologue's g read.
	stackGuard *regexp.Regexp
	// memDest matches a memory write (for the equality-function
	// no-store rule).
	memDest *regexp.Regexp
	// splitBodies maps each fork-side split-body symbol to the stock
	// symbol whose text it must equal (design.md, "Fence-wrapper
	// splits"). Per-arch: the gettimeofday split is linux/amd64
	// assembly and does not exist on arm64. The split class's
	// allowlist patterns are derived from this map (splitPatterns), so
	// the body set, the wrapper set, and the patterns cannot drift.
	splitBodies map[string]string
	// exactDeltas pins the fork-vs-stock instruction-count delta of
	// every non-generic admitted runtime symbol (pairs measured as
	// caller minus the extraction call plus helper). The compiler is
	// deterministic, so these are stable per base and per arch; a port
	// re-measures them under review. The pin closes the remaining
	// mimicry gap: a residue store that copies the shape of a legit
	// widened-record store still adds a line.
	exactDeltas map[string]int
}

// prof is the active profile, set by the test before any comparison; the
// helpers below read it. Package-level because the comparison helpers are
// deliberately free functions mirroring the design.md rule names; exactly
// one gate run executes per process.
var prof *archProfile

var hexRe = regexp.MustCompile(`0x[0-9a-f]+`)
var decRe = regexp.MustCompile(`\b\d+\b`)
var bareBaseRe = regexp.MustCompile(`(^|[^)\w])\((R\d+|RSP)\)`)
var adrpOperand = regexp.MustCompile(`-?\d+\(PC\)`)

func maskHex(s string) string { return hexRe.ReplaceAllString(s, "HEX") }

// maskARM64 masks hex and decimal operands (arm64 objdump prints
// displacements, immediates, and branch offsets in decimal; digits inside
// register names are protected by the word boundaries) and rewrites a bare
// zero-displacement base `(Rn)` to `HEX(Rn)` so both displacement
// spellings compare as one form. Lines carrying a symbol operand — branch
// and call targets (CALL/JMP and the occasional conditional branch to a
// symbol), the only (SB) forms arm64 objdump emits — keep their digits:
// `net.map.init.0` masked to `net.map.init.HEX` would let two different
// call targets compare equal.
func maskARM64(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = hexRe.ReplaceAllString(l, "HEX")
		if !strings.Contains(l, "(SB)") {
			l = decRe.ReplaceAllString(l, "HEX")
			l = bareBaseRe.ReplaceAllString(l, "${1}HEX($2)")
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

var symOperand = regexp.MustCompile(`([\w./:*()\[\]<>@$-]+\(SB\)|0x[0-9a-f]+\(IP\))`)

var profiles = map[string]*archProfile{
	"amd64": {
		mask: maskHex,
		canonData: func(text string) string {
			// objdump symbolizes a PC-relative operand only when the
			// target happens to land on a symbol, so the same
			// instruction prints as `LEAQ 0x...(IP)` in one binary and
			// `LEAQ sym(SB)` in the other. Canonicalize every non-branch
			// memory operand; branch targets keep their (semantic)
			// symbol names.
			return symOperand.ReplaceAllString(text, "ADDR")
		},
		regToken:    regexp.MustCompile(`\b(AX|BX|CX|DX|SI|DI|BP|R8|R9|R10|R11|R12|R13|R14|R15|X[0-9]+)\b`),
		nonStackMem: regexp.MustCompile(`(HEX|0x[0-9a-f]+|-?[0-9]+)?\((AX|BX|CX|DX|SI|DI|R8|R9|R10|R11|R12|R13|R14|R15)\)`),
		immStore:    regexp.MustCompile(`^(MOV[A-Z]*|AND[A-Z]*|OR[A-Z]*|XOR[A-Z]*|BT[SRC][A-Z]*) \$-?(HEX|0x[0-9a-f]+|[0-9]+), (HEX|0x[0-9a-f]+|-?[0-9]+)?\((AX|BX|CX|DX|SI|DI|R8|R9|R10|R11|R12|R13|R14|R15)\)$`),
		regSrcStore: regexp.MustCompile(`^(MOV[A-Z]*|AND[A-Z]*|OR[A-Z]*|XOR[A-Z]*|BT[SRC][A-Z]*) (AX|BX|CX|DX|SI|DI|R8|R9|R10|R11|R12|R13|R14|R15), (HEX|0x[0-9a-f]+|-?[0-9]+)?\((AX|BX|CX|DX|SI|DI|R8|R9|R10|R11|R12|R13|R14|R15)\)$`),
		stackGuard: regexp.MustCompile(`^CMPQ SP, (0x10|HEX)\(R14\)$`),
		addrArith:  regexp.MustCompile(`^LEA`),
		memDest:     regexp.MustCompile(`^(?:LOCK )?(?:MOV|AND|OR|XOR|ADD|SUB|INC|DEC|BT|XCHG|CMPXCHG)[A-Z]*\b.*(?:\((?:AX|BX|CX|DX|SI|DI|BP|R8|R9|R10|R11|R12|R13|R14|R15)\)(?:\((?:AX|BX|CX|DX|SI|DI|BP|R8|R9|R10|R11|R12|R13|R14|R15)\*[0-9]+\))?|ADDR)$`),
		splitBodies: map[string]string{
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
		},
		exactDeltas: map[string]int{
			"runtime.addCleanup":              2,
			"runtime.(*cleanupQueue).enqueue": 17,
			"runtime.freeSpecial":             3,
			"runtime.gcinit":                  21,
			// pairs: caller minus the extraction call, plus helper, vs stock
			"runtime.runFinalizers":  7,
			"runtime.GC":             8,
			"runtime.runCleanups":    15,
			"runtime.queuefinalizer": 21,
		},
	},
	"arm64": {
		mask: maskARM64,
		canonData: func(text string) string {
			// arm64 objdump never symbolizes data references; a global
			// access is an ADRP page offset that flips sign when the
			// global lands on the other side of the code. Canonicalize
			// the page operand; conditional-branch (PC) targets stay
			// (they mask to one form anyway).
			if strings.HasPrefix(text, "ADRP ") {
				return adrpOperand.ReplaceAllString(text, "ADDR")
			}
			return text
		},
		// R0-R27 and the FP/SIMD registers are rename-maskable; ZR,
		// R28 (g), R29 (FP), R30 (LR), RSP stay distinct.
		regToken:    regexp.MustCompile(`\b(R[0-9]|R1[0-9]|R2[0-7]|F[0-9]+|V[0-9]+)\b`),
		nonStackMem: regexp.MustCompile(`(-?HEX|-?[0-9]+)?\((R[0-9]|R1[0-9]|R2[0-8])\)`),
		// arm64 cannot store an immediate: a hand-written field clear
		// (`gp.dstX = 0`) is a ZR-source store to a plain displacement
		// (pre/post-index .W/.P spellings included); any other constant
		// goes through a register and falls under store conservation.
		// regSrcStore is deliberately absent: the aggregate branch
		// classifies stores through memDest.
		immStore: regexp.MustCompile(`^MOV[DWBH]U?(\.[WP])? ZR, -?HEX\((R[0-9]|R1[0-9]|R2[0-8])\)$`),
		zeroPairStore: regexp.MustCompile(`^STP \(ZR, ZR\), -?HEX\((R[0-9]|R1[0-9]|R2[0-8])\)$`),
		addrArith:     regexp.MustCompile(`^ADRP `),
		// The raw-text (16|HEX) alternation mirrors amd64's: the
		// wrapper check reads unmasked lines, the class checks masked
		// ones.
		stackGuard: regexp.MustCompile(`^MOVD (16|HEX)\(R28\), R16$`),
		// RSP-based accesses are frame construction (the prologue's
		// MOVD.W R30, -16(RSP) push), not field writes. The FP/SIMD
		// store mnemonics (FMOV/FSTP/VST with a memory destination) are
		// stores like any other — leaving them out both undercounts
		// fork stores AND miscredits them as feeding loads.
		memDest: regexp.MustCompile(`^(MOV[DWBH]U?|FMOV[SDQ]?|FSTP[SDQ]?|VST[0-9A-Z]*|STP|STLR[A-Z]*|STXR[A-Z]*|SWP[A-Z]*|CAS[A-Z]*)[\w.]* .*\((R[0-9]|R1[0-9]|R2[0-8])\)(\(R[0-9]+(<<[0-9]+|<<HEX)?\))?$`),
		splitBodies: map[string]string{
			"syscall.closeFD":   "syscall.Close",
			"syscall.fstatFD":   "syscall.Fstat",
			"syscall.seekFD":    "syscall.Seek",
			"syscall.fsync":     "syscall.Fsync",
			"syscall.fdatasync": "syscall.Fdatasync",
			"syscall.flock":     "syscall.Flock",
			"syscall.kill":      "syscall.Kill",
			"syscall.madvise":   "syscall.Madvise",
			"syscall.mprotect":  "syscall.Mprotect",
		},
		postNorm:        foldADRPARM64,
		aggregateStores: true,
		exactDeltas: map[string]int{
			"runtime.addCleanup":              -1,
			"runtime.(*cleanupQueue).enqueue": 7,
			"runtime.freeSpecial":             2,
			"runtime.gcinit":                  21,
			// pairs: caller minus the extraction call, plus helper, vs stock
			"runtime.runFinalizers":  -6,
			"runtime.GC":             12,
			"runtime.runCleanups":    17,
			"runtime.queuefinalizer": 22,
		},
	},
}

// rawDstRef matches any reference to a dst-named symbol — data operand,
// call, or jump — on the RAW instruction text, before operand
// canonicalization can hide a symbolized data reference.
var rawDstRef = regexp.MustCompile(`([\w/]+\.|\(\*?[\w\[\]]+\)\.)dst\w*[+(]|runtime\.dst|\(\*?dst\w+\)`)

type symbol struct {
	lines  []string // normalized instructions (position/address/bytes stripped)
	masked string   // lines joined, numeric operands masked
	calls  map[string]bool
	dstRaw []string // raw lines referencing a dst-named symbol, captured before operand canonicalization
}

// disassemble dumps every text symbol of bin with tool's objdump and
// normalizes each instruction: the position, address, and byte columns are
// stripped; masked additionally replaces numeric operands so layout offsets
// and branch targets compare equal.
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
			if prof.postNorm != nil {
				cur.lines = prof.postNorm(cur.lines)
			}
			cur.masked = prof.mask(strings.Join(cur.lines, "\n"))
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
		if text == "?" {
			// An encoding objdump cannot decode. All-zero words are
			// end-of-function alignment padding — semantics-free, and they
			// shift with layout, so they are excluded like NOPs. A non-zero
			// undecodable instruction (arm64's LSE atomics) compares by its
			// encoding, so a real difference cannot hide behind the `?`.
			if strings.Trim(f[2], "0") == "" {
				continue
			}
			text = "? " + f[2]
		}
		if rawDstRef.MatchString(text) {
			cur.dstRaw = append(cur.dstRaw, text)
		}
		if strings.HasPrefix(text, "NOP") || strings.HasPrefix(text, "NOOP") {
			// Alignment/inline-marker nops shift freely with layout; they
			// carry no semantics and are excluded from every comparison.
			// (amd64 spells them NOPW/NOPL, arm64 NOOP — note NOOP does
			// NOT have the prefix NOP.)
			continue
		}
		if strings.HasPrefix(text, "JMP ") && strings.Contains(text, "(SB)") {
			// The morestack epilogue tail-jumps to the symbol itself; a split
			// body self-references its split name where stock names the stock
			// symbol — the same "modulo the call-target name" admission.
			target := strings.TrimSuffix(strings.Fields(text)[1], "(SB)")
			if stockName, isSplit := prof.splitBodies[target]; isSplit {
				text = strings.Replace(text, target+"(SB)", stockName+"(SB)", 1)
			}
		}
		if strings.HasPrefix(text, "CALL ") && strings.Contains(text, "(SB)") {
			target := strings.TrimSuffix(strings.Fields(text)[1], "(SB)")
			cur.calls[target] = true
			// A fork caller that inlined a split wrapper calls the split body
			// where stock calls the stock name — the class admits callers
			// "modulo the call-target name alone", so canonicalize the target.
			if stockName, isSplit := prof.splitBodies[target]; isSplit {
				text = strings.Replace(text, target+"(SB)", stockName+"(SB)", 1)
			}
		} else if !strings.HasPrefix(text, "JMP ") {
			text = prof.canonData(text)
		}
		cur.lines = append(cur.lines, text)
	}
	flush()
	return syms
}

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
		// arithmetic, register-allocation freedom. (arm64 address
		// arithmetic is ADD/SUB on registers, which the memory matcher
		// never matches.)
		if strings.HasPrefix(l, "LEA") {
			continue
		}
		// Any fork-side unmatched memory touch — register-base or a
		// canonicalized global (ADDR) operand — is potential residue: a
		// field load/store or a package-var guard. An unmatched
		// undecodable encoding (`? <bytes>`) may be an atomic RMW on
		// residue state and cannot be inspected — rejected the same way.
		if prof.nonStackMem.MatchString(l) || strings.Contains(l, "ADDR") || strings.HasPrefix(l, "? ") {
			return false
		}
	}
	return true
}

// cancelRegRenames pairs fork-extra against stock-extra lines that are
// equal once register names are masked, returning what remains UNPAIRED on
// each side. Residue can only live on the fork side; stock-side leftovers
// are stock's own text and never rejected on their own.
func cancelRegRenames(a, b []string) (aLeft, bLeft []string) {
	bm := make(map[string]int)
	for _, l := range b {
		bm[prof.regToken.ReplaceAllString(l, "R")]++
	}
	for _, l := range a {
		k := prof.regToken.ReplaceAllString(l, "R")
		if bm[k] > 0 {
			bm[k]--
			continue
		}
		aLeft = append(aLeft, l)
	}
	for _, l := range b {
		k := prof.regToken.ReplaceAllString(l, "R")
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
		count[prof.mask(l)]++
	}
	for _, l := range b {
		count[prof.mask(l)]--
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

// classReject applies the rejections and exemptions shared by both class
// store rules to one unpaired fork-side line. handled=true means the line
// is fully dispositioned: a non-empty msg is a hard rejection; an empty one
// is an exemption — pure address arithmetic (amd64's LEA, arm64's ADRP),
// or arm64's STP (ZR, ZR) bulk-zeroing latitude (blind spot recorded in
// design.md: a hook clearing two ADJACENT fields merges into that shape).
func classReject(l string) (msg string, handled bool) {
	if prof.addrArith.MatchString(l) {
		return "", true
	}
	if prof.immStore.MatchString(l) {
		// A hand-written field clear or flag set.
		return fmt.Sprintf("unpaired field store in the diff: %q", l), true
	}
	if prof.zeroPairStore != nil && prof.zeroPairStore.MatchString(l) {
		return "", true
	}
	if strings.Contains(l, "ADDR") && !strings.HasPrefix(l, "CALL") && !strings.HasPrefix(l, "JMP") {
		// A canonicalized global operand — a package-var guard or store.
		return fmt.Sprintf("unpaired global access in the diff: %q", l), true
	}
	return "", false
}

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
	forkLeft, stockLeft := cancelRegRenames(fe, se)
	if prof.aggregateStores {
		// Store conservation (the arm64 class rule; see the profile
		// field's comment): reject the hand-written store shapes
		// outright, then require fork-side unmatched store words not to
		// exceed stock-side unmatched store words plus fork-side
		// unmatched load words.
		words := func(l string) int {
			for _, p := range []string{"STP", "LDP", "FSTP", "FLDP"} {
				if strings.HasPrefix(l, p) {
					return 2
				}
			}
			return 1
		}
		forkStores, stockStores, loadCredits := 0, 0, 0
		forkUndec, stockUndec := 0, 0
		for _, l := range forkLeft {
			if msg, handled := classReject(l); handled {
				if msg != "" {
					return msg
				}
				continue
			}
			switch {
			case strings.HasPrefix(l, "? "):
				// An undecodable encoding (the LSE atomics) may be an
				// atomic RMW store; it cannot be inspected. Fork-side
				// excess over the stock side's is charged as stores
				// below — never as load credits.
				forkUndec += words(l)
			case prof.memDest.MatchString(l) && prof.nonStackMem.MatchString(l):
				forkStores += words(l)
			case strings.HasPrefix(l, "ST") && prof.nonStackMem.MatchString(l):
				return fmt.Sprintf("unpaired store of unrecognized shape in the diff: %q", l)
			case prof.nonStackMem.MatchString(l):
				loadCredits += words(l)
			}
		}
		for _, l := range stockLeft {
			if strings.HasPrefix(l, "? ") {
				stockUndec += words(l)
				continue
			}
			if prof.memDest.MatchString(l) && prof.nonStackMem.MatchString(l) &&
				!(prof.zeroPairStore != nil && prof.zeroPairStore.MatchString(l)) {
				stockStores += words(l)
			}
		}
		// Undecodables cancel pairwise across the sides; the fork-side
		// excess is charged as stores, and a stock-side excess grants
		// NOTHING (stock atomics the fork lacks must not buy fork-side
		// residue budget).
		if forkUndec > stockUndec {
			forkStores += forkUndec - stockUndec
		}
		if forkStores > stockStores+loadCredits {
			return fmt.Sprintf("fork-side unmatched stores (%d words) exceed stock-side unmatched stores (%d) plus fork-side unmatched loads (%d) — added stores are the residue shape (unpaired fork-side context: %q)",
				forkStores, stockStores, loadCredits, forkLeft)
		}
		return ""
	}
	loadsInto := make(map[string]int)
	var stores []string
	for _, l := range forkLeft {
		if msg, handled := classReject(l); handled {
			if msg != "" {
				return msg
			}
			continue
		}
		switch {
		case strings.HasPrefix(l, "? "):
			// An unmatched undecodable encoding cannot be inspected and
			// can have no feeding load — rejected outright under the
			// per-register rule.
			return fmt.Sprintf("unpaired undecodable instruction in the diff: %q", l)
		case prof.regSrcStore.MatchString(l):
			stores = append(stores, l)
		case prof.nonStackMem.MatchString(l):
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

var adrpTarget = regexp.MustCompile(`^ADRP ADDR, (R[0-9]+)$`)
var adrpAdd = regexp.MustCompile(`^ADD \$-?[0-9]+, (R[0-9]+), (R[0-9]+)$`)
var dispBaseRe = regexp.MustCompile(`(-?[0-9]+)?\((R[0-9]+)\)`)

// foldADRPARM64 folds each `ADRP ADDR, Rx` (optionally followed by an
// `ADD $lo, Rx, Rx` low-bits step) into the immediately following memory
// access through `(Rx)`, rewriting that operand to ADDR and dropping the
// address-forming lines. The immediate follower is the only sound fold
// target: ADRP just clobbered Rx, so a next-line `(Rx)` base can only be
// the global. An ADRP whose follower does not access through Rx (address
// taken, not accessed) is kept as-is.
func foldADRPARM64(lines []string) []string {
	out := lines[:0]
	for i := 0; i < len(lines); i++ {
		m := adrpTarget.FindStringSubmatch(lines[i])
		if m == nil {
			out = append(out, lines[i])
			continue
		}
		reg, j := m[1], i+1
		if j < len(lines) {
			if a := adrpAdd.FindStringSubmatch(lines[j]); a != nil && a[1] == reg && a[2] == reg {
				j++ // low-bits ADD folds with its ADRP
			}
		}
		if j < len(lines) && !strings.HasPrefix(lines[j], "ADRP") {
			folded := false
			rewritten := dispBaseRe.ReplaceAllStringFunc(lines[j], func(m string) string {
				if sub := dispBaseRe.FindStringSubmatch(m); sub[2] == reg {
					folded = true
					return "ADDR"
				}
				return m
			})
			if folded {
				out = append(out, rewritten)
				i = j
				continue
			}
		}
		out = append(out, lines[i])
	}
	return out
}

func multisetSymDiff(a, b []string) []string {
	count := make(map[string]int)
	for _, l := range a {
		count[prof.mask(l)]++
	}
	for _, l := range b {
		count[prof.mask(l)]--
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
			if stockName, isSplit := prof.splitBodies[c]; isSplit {
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

// allowlist is built per profile at test start (the split-class patterns
// are per-arch).
var allowlist []allowEntry

func admit(name string) *allowEntry {
	for i := range allowlist {
		if allowlist[i].re.MatchString(name) {
			return &allowlist[i]
		}
	}
	return nil
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
	if want, pinned := prof.exactDeltas[name]; pinned {
		if d := len(pairSym.lines) - len(sb.lines); d != want {
			return fmt.Sprintf("pair instruction-count delta %d, pinned %d — re-measure only with review", d, want)
		}
	}
	return classRelaxed(pairSym.lines, sb.lines, pairSym.dstRaw, 48, 64)
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

// splitPatterns derives the split class's allowlist patterns from the
// profile's splitBodies map — the map is the single source of the body and
// wrapper sets.
func splitPatterns(bodies map[string]string) (bodyPat, wrapperPat string) {
	var bs, ws []string
	seen := make(map[string]bool)
	for b, w := range bodies {
		bs = append(bs, regexp.QuoteMeta(b))
		if !seen[w] {
			seen[w] = true
			ws = append(ws, regexp.QuoteMeta(w))
		}
	}
	sort.Strings(bs)
	sort.Strings(ws)
	return "^(" + strings.Join(bs, "|") + ")$", "^(" + strings.Join(ws, "|") + ")$"
}

func buildAllowlist(p *archProfile) []allowEntry {
	bodyPat, wrapperPat := splitPatterns(p.splitBodies)
	list := []allowEntry{
		// Shared-helper extractions: the caller differs by the one call, the
		// helper is fork-only (its body-vs-stock-loop identity is carried by the
		// caller's callee-set check plus the recorded clause).
		{pattern: `^runtime\.(runFinalizers|runCleanups|queuefinalizer|GC)$`, class: "extraction-caller", check: checkExtractionCaller},
		{pattern: `^runtime\.(runFinqBlocks|runCleanupBlock|finAllocBlockLocked|gcForce)$`, class: "extraction-helper", check: checkForkOnly},
		// Fence-wrapper splits: the split body must be instruction-identical to
		// the stock symbol it replaces; the stock-named wrapper (where it
		// survives inlining) may only call the split body.
		{pattern: bodyPat, class: "split-body",
			check: func(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string {
				if ok {
					return "expected fork-only"
				}
				counterpart, exists := stock[p.splitBodies[name]]
				if !exists {
					return fmt.Sprintf("stock has no %s to compare the split body against", p.splitBodies[name])
				}
				if fb.masked != counterpart.masked {
					return fmt.Sprintf("split body is not instruction-identical to stock's %s", p.splitBodies[name])
				}
				return ""
			}},
		{pattern: wrapperPat, class: "split-wrapper",
			check: func(name string, fb, sb symbol, ok bool, fork, stock map[string]symbol) string {
				if len(fb.lines) == 0 {
					return "" // wrapper fully inlined away: the split body carries the text
				}
				for c := range fb.calls {
					if p.splitBodies[c] != name && !barrierRe.MatchString(c) && c != "runtime.morestack_noctxt.abi0" {
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
					if p.stackGuard.MatchString(l) {
						continue // the canonical stack-growth prologue reads g's stackguard
					}
					if p.nonStackMem.MatchString(l) || strings.Contains(l, "ADDR") {
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
				if want, pinned := prof.exactDeltas[name]; pinned {
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
					if !strings.HasPrefix(l, "LEA") && p.memDest.MatchString(l) {
						return fmt.Sprintf("memory write inside an equality function: %q", l)
					}
				}
				return ""
			}},
	}
	for i := range list {
		list[i].re = regexp.MustCompile(list[i].pattern)
	}
	return list
}
