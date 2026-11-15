// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestUntaggedTextIdenticalToStock is INV-VANILLA's enforcing gate
// (design.md, "Untagged footprint (contract)"): the probe corpus, built
// untagged by this toolchain and by the upstream base release toolchain,
// must produce binaries with the exact same text symbol set, zero dst-symbol
// references, and instruction-identical bodies for every symbol modulo
// .rodata/.data link-address relocations (shifted solely by the -dst.N suffix
// in runtime.buildVersion). Zero allowlist exceptions or codegen-drift slack
// are admitted.
func TestUntaggedTextIdenticalToStock(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the differential build")
	}
	goarch := os.Getenv("DST_VANILLA_GOARCH")
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	if goarch != "amd64" && goarch != "arm64" {
		t.Skipf("relocation canonicalizer supports amd64 and arm64; got GOARCH=%s", goarch)
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
		cmd := exec.Command(filepath.Join(goroot, "bin", "go"), "test", "-c", "-trimpath", "-o", out, ".")
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

	forkSyms := disassemble(t, fork, forkBin, goarch)
	stockSyms := disassemble(t, fork, stockBin, goarch)

	if dbg := os.Getenv("DST_VANILLA_DEBUG"); dbg != "" {
		fb, sb := forkSyms[dbg], stockSyms[dbg]
		t.Logf("fork %s (%d lines):\n%s", dbg, len(fb.lines), strings.Join(fb.lines, "\n"))
		t.Logf("stock %s (%d lines):\n%s", dbg, len(sb.lines), strings.Join(sb.lines, "\n"))
	}

	var failures []string
	var names []string
	for name := range forkSyms {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fb := forkSyms[name]
		for _, l := range fb.dstRaw {
			failures = append(failures, fmt.Sprintf("untagged %s references a dst symbol: %s", name, strings.TrimSpace(l)))
		}
		sb, ok := stockSyms[name]
		if !ok {
			failures = append(failures, fmt.Sprintf("fork-only symbol %s (%d instructions)", name, len(fb.lines)))
			continue
		}
		if fb.canon != sb.canon {
			failures = append(failures, fmt.Sprintf("symbol %s differs from stock (%s)", name, firstDiff(fb.lines, sb.lines)))
		}
	}
	var stockNames []string
	for name := range stockSyms {
		if _, ok := forkSyms[name]; !ok {
			stockNames = append(stockNames, name)
		}
	}
	sort.Strings(stockNames)
	for _, name := range stockNames {
		failures = append(failures, fmt.Sprintf("stock-only symbol %s (%d instructions)", name, len(stockSyms[name].lines)))
	}
	if len(failures) > 0 {
		if len(failures) > 40 {
			failures = append(failures[:40], fmt.Sprintf("... and %d more", len(failures)-40))
		}
		t.Fatalf("untagged text diverges from stock:\n  %s", strings.Join(failures, "\n  "))
	}
}

func firstDiff(a, b []string) string {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("line %d: fork=%q vs stock=%q; len %d vs %d", i, a[i], b[i], len(a), len(b))
		}
	}
	return fmt.Sprintf("length %d vs %d", len(a), len(b))
}

// coverageCheck pins the corpus property (design.md): the corpus's
// transitive import closure covers every upstream-present std package the
// dst delta modifies.
func coverageCheck(t *testing.T, fork, corpus, goarch string) {
	t.Helper()
	required := []string{
		"crypto/internal/sysrand", "go/types", "internal/poll",
		"internal/runtime/maps", "internal/sync", "net", "os", "os/signal",
		"os/user", "runtime", "sync", "syscall", "testing",
		"testing/internal/testdeps", "time",
	}
	cmd := exec.Command(filepath.Join(fork, "bin", "go"), "list", "-test", "-deps", ".")
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
	base, err := os.ReadFile(filepath.Join(fork, "VERSION"))
	if err != nil {
		t.Logf("coverage: VERSION unreadable (%v); embedded list not cross-checked", err)
		return
	}
	tag := strings.TrimSpace(strings.Split(string(base), "\n")[0])
	if i := strings.Index(tag, "-dst."); i >= 0 {
		tag = tag[:i]
	}
	gout, err := liveSourceDelta(fork, tag)
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
	for _, f := range gout {
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

// liveSourceDelta includes staged, unstaged and untracked changes because the
// compiler and source inventory read the working tree, not the last commit.
func liveSourceDelta(fork, tag string) ([]string, error) {
	changed, err := exec.Command("git", "-C", fork, "diff", "--name-only", "-z", tag, "--", "src").Output()
	if err != nil {
		return nil, err
	}
	untracked, err := exec.Command("git", "-C", fork, "ls-files", "--others", "-z", "--", "src").Output()
	if err != nil {
		return nil, err
	}
	paths := func(data []byte) []string {
		if len(data) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	}
	result := paths(changed)
	for _, name := range paths(untracked) {
		switch name {
		case "", "src/cmd/cgo/zdefaultcc.go", "src/cmd/go/internal/cfg/zdefaultcc.go",
			"src/cmd/internal/objabi/zbootstrap.go", "src/internal/buildcfg/zbootstrap.go",
			"src/internal/runtime/sys/zversion.go", "src/time/tzdata/zzipdata.go":
			continue
		}
		result = append(result, name)
	}
	return result, nil
}

func TestLiveSourceDeltaIncludesUncommittedChanges(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.Mkdir(filepath.Join(dir, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, "src", name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "src", name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("staged.go", "package original\n")
	write("unstaged.go", "package original\n")
	git("add", "src")
	git("commit", "-qm", "base")
	git("tag", "base")
	write("committed.go", "package added\n")
	git("add", "src/committed.go")
	git("commit", "-qm", "fork addition")
	write("staged.go", "package changed\n")
	git("add", "src/staged.go")
	write("unstaged.go", "package changed\n")
	write("untracked.go", "package added\n")
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("src/ignored.go\n"), 0600); err != nil {
		t.Fatal(err)
	}
	write("ignored.go", "package added\n")
	write("é.go", "package added\n")
	write("quote\"tab\tline\n.go", "package added\n")
	write("time/tzdata/zzipdata.go", "package generated\n")
	write("time/tzdata/zzipdata_extra.go", "package added\n")
	want := []string{"src/committed.go", "src/ignored.go", "src/staged.go", "src/unstaged.go", "src/untracked.go", "src/é.go", "src/quote\"tab\tline\n.go", "src/time/tzdata/zzipdata_extra.go"}
	check := func() {
		t.Helper()
		out, err := liveSourceDelta(dir, "base")
		sort.Strings(out)
		sort.Strings(want)
		if err != nil || !slices.Equal(out, want) {
			t.Fatalf("live source delta = %q, %v; want %q", out, err, want)
		}
	}
	check()
	git("add", "-f", "src/time/tzdata/zzipdata.go")
	want = append(want, "src/time/tzdata/zzipdata.go")
	check()
}

// rawDstRef matches any reference to a dst-named symbol on raw instruction
// text, before relocation canonicalization can rewrite data operands.
var rawDstRef = regexp.MustCompile(`([\w/]+\.|\(\*?[\w\[\]]+\)\.)dst\w*[+(]|runtime\.dst|\(\*?dst\w+\)`)

var (
	amd64IPRel      = regexp.MustCompile(`-?0x[0-9a-f]+\(IP\)`)
	amd64SymImm     = regexp.MustCompile(`\$([^\s,+()]+)(?:\+(\d+))?\(SB\)`)
	amd64HexBranch  = regexp.MustCompile(`^(J[A-Z0-9]+)\s+(0x[0-9a-f]+)$`)
	arm64ADRP       = regexp.MustCompile(`^(ADRP?)\s+-?\d+\(PC\),\s*(R\d+)$`)
	arm64AddImm     = regexp.MustCompile(`^(ADD\s+)\$\d+(,\s*R\d+,\s*(R\d+|RSP))$`)
	arm64PCRelLabel = regexp.MustCompile(`(-?\d+)\(PC\)$`)
	arm64DstPair    = regexp.MustCompile(`\(\s*(R\d+|ZR)\s*,\s*(R\d+|ZR)\s*\)$`)
	arm64BaseReg    = regexp.MustCompile(`\((R\d+)\)$`)
)

var arm64BranchOps = map[string]bool{
	"CALL": true, "JMP": true, "RET": true,
	"CBZ": true, "CBNZ": true, "CBZW": true, "CBNZW": true,
	"TBZ": true, "TBNZ": true,
	"BEQ": true, "BNE": true, "BCS": true, "BHS": true,
	"BCC": true, "BLO": true, "BMI": true, "BPL": true,
	"BVS": true, "BVC": true, "BHI": true, "BLS": true,
	"BGE": true, "BLT": true, "BGT": true, "BLE": true,
	"BAL": true, "BNV": true,
}

type symbol struct {
	name       string
	lines      []string
	canon      string
	dstRaw     []string
	overlongVZ []uint64
}

type elfSym struct {
	name   string
	addr   uint64
	size   uint64
	isText bool
	isBSS  bool
}

type elfSymTable struct {
	sorted      []elfSym
	byName      map[string]elfSym
	textVAddr   uint64
	textFileOff uint64
	hasText     bool
}

func loadELFSyms(t *testing.T, bin string) *elfSymTable {
	t.Helper()
	f, err := elf.Open(bin)
	if err != nil {
		t.Fatalf("elf open %s: %v", bin, err)
	}
	defer f.Close()
	syms, err := f.Symbols()
	if err != nil {
		t.Fatalf("elf symbols %s: %v", bin, err)
	}
	tab := &elfSymTable{byName: make(map[string]elfSym, len(syms))}
	if textSec := f.Section(".text"); textSec != nil {
		tab.textVAddr = textSec.Addr
		tab.textFileOff = textSec.Offset
		tab.hasText = true
	}
	for _, s := range syms {
		if s.Size == 0 || s.Value == 0 || elf.ST_TYPE(s.Info) == elf.STT_SECTION {
			continue
		}
		var isText, isBSS bool
		if int(s.Section) >= 0 && int(s.Section) < len(f.Sections) {
			sec := f.Sections[s.Section]
			isText = sec.Flags&elf.SHF_EXECINSTR != 0
			isBSS = sec.Type == elf.SHT_NOBITS
		}
		es := elfSym{
			name:   s.Name,
			addr:   s.Value,
			size:   s.Size,
			isText: isText,
			isBSS:  isBSS,
		}
		tab.sorted = append(tab.sorted, es)
		tab.byName[s.Name] = es
	}
	sort.Slice(tab.sorted, func(i, j int) bool {
		if tab.sorted[i].addr != tab.sorted[j].addr {
			return tab.sorted[i].addr < tab.sorted[j].addr
		}
		return tab.sorted[i].name < tab.sorted[j].name
	})
	return tab
}

func (tab *elfSymTable) lookup(addr uint64) (string, uint64, bool) {
	if tab == nil || len(tab.sorted) == 0 {
		return "", 0, false
	}
	i := sort.Search(len(tab.sorted), func(i int) bool { return tab.sorted[i].addr > addr })
	if i == 0 {
		return "", 0, false
	}
	base := tab.sorted[i-1].addr
	best := -1
	for j := i - 1; j >= 0 && tab.sorted[j].addr == base; j-- {
		if addr < tab.sorted[j].addr+tab.sorted[j].size {
			best = j
		}
	}
	if best >= 0 {
		return tab.sorted[best].name, tab.sorted[best].addr, true
	}
	return "", 0, false
}

var arm64MemImmMatchers = func() map[string]*regexp.Regexp {
	m := make(map[string]*regexp.Regexp, 31)
	for i := 0; i <= 30; i++ {
		reg := fmt.Sprintf("R%d", i)
		m[reg] = regexp.MustCompile(`(\s)(?:\d+)?\(` + reg + `\)(,|$)`)
	}
	return m
}()

func disassemble(t *testing.T, goroot, bin, goarch string) map[string]symbol {
	t.Helper()
	elfTab := loadELFSyms(t, bin)
	cmd := exec.Command(filepath.Join(goroot, "bin", "go"), "tool", "objdump", bin)
	cmd.Env = append(os.Environ(), "GOROOT="+goroot, "GOTOOLCHAIN=local")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("objdump %s: %v", bin, err)
	}
	syms := parseDisassembly(string(out), goarch, elfTab, nil)
	if goarch != "amd64" || !elfTab.hasText {
		return syms
	}
	// x86asm in cmd/objdump consumes a spurious ModRM byte after 3-byte
	// VZEROUPPER (c5 f8 77), shifting subsequent instructions out of phase.
	// Re-disassemble only affected symbols from a scratch copy with those
	// exact 3-byte VZEROUPPER encodings replaced by 3 single-byte NOPs,
	// restoring the VZEROUPPER instruction at its original PC.
	vzFixed := make(map[uint64]bool)
	var binBytes []byte
	for {
		var affected []string
		newPCs := 0
		for name, sym := range syms {
			if len(sym.overlongVZ) == 0 {
				continue
			}
			affected = append(affected, regexp.QuoteMeta(name))
			for _, pc := range sym.overlongVZ {
				if !vzFixed[pc] {
					vzFixed[pc] = true
					newPCs++
				}
			}
		}
		if newPCs == 0 {
			break
		}
		sort.Strings(affected)
		if binBytes == nil {
			b, err := os.ReadFile(bin)
			if err != nil {
				t.Fatalf("read %s: %v", bin, err)
			}
			binBytes = b
		}
		patched := slices.Clone(binBytes)
		for pc := range vzFixed {
			off := elfTab.textFileOff + (pc - elfTab.textVAddr)
			if off+3 > uint64(len(patched)) || patched[off] != 0xc5 || patched[off+1] != 0xf8 || patched[off+2] != 0x77 {
				t.Fatalf("unexpected bytes at VZEROUPPER PC 0x%x in %s", pc, bin)
			}
			patched[off], patched[off+1], patched[off+2] = 0x90, 0x90, 0x90
		}
		tmp := filepath.Join(t.TempDir(), "vz-resync")
		if err := os.WriteFile(tmp, patched, 0o600); err != nil {
			t.Fatalf("write %s: %v", tmp, err)
		}
		rcmd := exec.Command(filepath.Join(goroot, "bin", "go"), "tool", "objdump", "-s", "^("+strings.Join(affected, "|")+")$", tmp)
		rcmd.Env = append(os.Environ(), "GOROOT="+goroot, "GOTOOLCHAIN=local")
		rout, err := rcmd.Output()
		if err != nil {
			t.Fatalf("objdump resync %s: %v", tmp, err)
		}
		for name, rsym := range parseDisassembly(string(rout), goarch, elfTab, vzFixed) {
			syms[name] = rsym
		}
	}
	return syms
}

type rawIns struct {
	pc     uint64
	encLen int
	hasP   bool
	isNOP  bool
	text   string
}

func parseDisassembly(out, goarch string, elfTab *elfSymTable, vzFixed map[uint64]bool) map[string]symbol {
	syms := make(map[string]symbol)
	var name string
	var cur symbol
	var raw []rawIns
	flush := func() {
		if name == "" {
			return
		}
		cur.lines, cur.dstRaw = canonicalizeSymbol(raw, goarch, elfTab, cur.dstRaw)
		cur.canon = strings.Join(cur.lines, "\n")
		syms[name] = cur
		raw = raw[:0]
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "TEXT ") {
			flush()
			name = line[len("TEXT "):]
			if i := strings.LastIndex(name, "(SB)"); i >= 0 {
				name = name[:i]
			}
			cur = symbol{name: name}
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || name == "" {
			continue
		}
		enc := f[2]
		text := strings.Join(f[3:], " ")
		if text == "?" {
			if strings.Trim(enc, "0") == "" {
				continue
			}
			text = "? " + enc
		}
		pc, err := strconv.ParseUint(strings.TrimPrefix(f[1], "0x"), 16, 64)
		hasP := err == nil
		if hasP && vzFixed != nil {
			if vzFixed[pc] {
				raw = append(raw, rawIns{pc: pc, encLen: 3, hasP: true, text: "VZEROUPPER"})
				continue
			}
			if vzFixed[pc-1] || vzFixed[pc-2] {
				continue
			}
		}
		if goarch == "amd64" && hasP && text == "VZEROUPPER" && len(enc) > 6 && strings.HasPrefix(enc, "c5f877") {
			cur.overlongVZ = append(cur.overlongVZ, pc)
		}
		if rawDstRef.MatchString(text) {
			cur.dstRaw = append(cur.dstRaw, text)
		}
		isNOP := strings.HasPrefix(text, "NOP") || strings.HasPrefix(text, "NOOP")
		raw = append(raw, rawIns{pc: pc, encLen: len(enc) / 2, hasP: hasP, isNOP: isNOP, text: text})
	}
	flush()
	return syms
}

func canonicalizeSymbol(raw []rawIns, goarch string, elfTab *elfSymTable, dstRaw []string) ([]string, []string) {
	lines := make([]string, 0, len(raw))
	switch goarch {
	case "amd64":
		for _, ins := range raw {
			if ins.isNOP {
				continue
			}
			text := ins.text
			if m := amd64HexBranch.FindStringSubmatch(text); m != nil && ins.hasP {
				if dst, err := strconv.ParseUint(strings.TrimPrefix(m[2], "0x"), 16, 64); err == nil {
					text = fmt.Sprintf("%s PC%+d", m[1], int64(dst-ins.pc))
				}
			} else if !strings.HasPrefix(text, "CALL ") && !strings.HasPrefix(text, "JMP ") {
				if elfTab != nil {
					text = amd64SymImm.ReplaceAllStringFunc(text, func(tok string) string {
						m := amd64SymImm.FindStringSubmatch(tok)
						if m == nil {
							return tok
						}
						es, ok := elfTab.byName[m[1]]
						if !ok || es.isText {
							return tok
						}
						var off uint64
						if m[2] != "" {
							v, err := strconv.ParseUint(m[2], 10, 64)
							if err != nil {
								return tok
							}
							off = v
						}
						if off > 0 || es.isBSS {
							return fmt.Sprintf("$0x%x", es.addr+off)
						}
						return tok
					})
				}
				text = amd64IPRel.ReplaceAllStringFunc(text, func(tok string) string {
					if elfTab != nil && ins.hasP && ins.encLen > 0 {
						dispStr := strings.TrimSuffix(tok, "(IP)")
						if disp, err := strconv.ParseInt(dispStr, 0, 64); err == nil {
							target := uint64(int64(ins.pc) + int64(ins.encLen) + disp)
							if symName, base, ok := elfTab.lookup(target); ok {
								var resolved string
								if target == base {
									resolved = symName + "(SB)"
								} else {
									resolved = fmt.Sprintf("%s+%d(SB)", symName, target-base)
								}
								if rawDstRef.MatchString(resolved) {
									dstRaw = append(dstRaw, resolved)
								}
								return resolved
							}
						}
					}
					return "ADDR(IP)"
				})
			}
			lines = append(lines, text)
		}
	case "arm64":
		joinPCs := make(map[uint64]bool)
		for _, ins := range raw {
			if ins.isNOP || !ins.hasP || !isARM64Branch(ins.text) {
				continue
			}
			if m := arm64PCRelLabel.FindStringSubmatch(ins.text); m != nil {
				if d, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					joinPCs[uint64(int64(ins.pc)+4*d)] = true
				}
			}
		}
		adrpRegs := make(map[string]bool)
		for _, ins := range raw {
			if ins.hasP && joinPCs[ins.pc] {
				clear(adrpRegs)
			}
			if ins.isNOP {
				continue
			}
			text := ins.text
			if m := arm64ADRP.FindStringSubmatch(text); m != nil {
				text = fmt.Sprintf("%s ADDR(PC), %s", m[1], m[2])
				if m[1] == "ADRP" {
					adrpRegs[m[2]] = true
				} else {
					delete(adrpRegs, m[2])
				}
			} else {
				text = canonARM64PageOff(text, adrpRegs)
			}
			lines = append(lines, text)
		}
	default:
		for _, ins := range raw {
			if ins.isNOP {
				continue
			}
			lines = append(lines, ins.text)
		}
	}
	return lines, dstRaw
}

func isARM64Branch(text string) bool {
	op := text
	if i := strings.IndexByte(text, ' '); i >= 0 {
		op = text[:i]
	}
	return arm64BranchOps[op]
}

func canonARM64PageOff(text string, adrpRegs map[string]bool) string {
	if isARM64Branch(text) {
		clear(adrpRegs)
		return text
	}
	if len(adrpRegs) == 0 {
		return text
	}
	op := text
	if i := strings.IndexByte(text, ' '); i >= 0 {
		op = text[:i]
	}
	if m := arm64AddImm.FindStringSubmatch(text); m != nil {
		parts := strings.Split(m[2], ",")
		src := strings.TrimSpace(parts[1])
		dst := strings.TrimSpace(parts[2])
		if adrpRegs[src] {
			text = m[1] + "$PAGEOFF" + m[2]
			delete(adrpRegs, dst)
			return text
		}
	}
	if !strings.Contains(op, ".") {
		for reg := range adrpRegs {
			if re, ok := arm64MemImmMatchers[reg]; ok && re.MatchString(text) {
				text = re.ReplaceAllString(text, "${1}PAGEOFF("+reg+")${2}")
			}
		}
	} else {
		// Pre-index (.W) and post-index (.P) addressing modes mutate their base register.
		for _, tok := range strings.Split(text, ",") {
			if m := arm64BaseReg.FindStringSubmatch(strings.TrimSpace(tok)); m != nil {
				delete(adrpRegs, m[1])
			}
		}
	}
	// Retire destination register(s) overwritten by this instruction.
	if m := arm64DstPair.FindStringSubmatch(text); m != nil {
		delete(adrpRegs, m[1])
		delete(adrpRegs, m[2])
	} else if i := strings.LastIndex(text, ","); i >= 0 {
		dst := strings.TrimSpace(text[i+1:])
		delete(adrpRegs, dst)
	}
	return text
}

func TestCanonicalizerRejectsRealMutations(t *testing.T) {
	wrapWithELF := func(arch string, tab *elfSymTable, lines ...string) symbol {
		var b strings.Builder
		b.WriteString("TEXT pkg.fn(SB) /src/pkg/fn.go\n")
		for i, l := range lines {
			fmt.Fprintf(&b, "  fn.go:%d 0x%x 00000000 %s\n", i+1, 0x1000+i*4, l)
		}
		return parseDisassembly(b.String(), arch, tab, nil)["pkg.fn"]
	}
	wrap := func(arch string, lines ...string) symbol {
		return wrapWithELF(arch, nil, lines...)
	}

	// Legitimate .rodata link-address relocation shifts must match, including
	// zero-offset page boundaries and non-branch B* instructions (BIC/BFI/BFM).
	if a, b := wrap("amd64", "LEAQ 0x1eefbb(IP), AX", "JEQ 0x1008", "RET"),
		wrap("amd64", "LEAQ 0x1eeb73(IP), AX", "JEQ 0x1008", "RET"); a.canon != b.canon {
		t.Fatalf("amd64 IP-rel relocation shift rejected: %q vs %q", a.canon, b.canon)
	}
	if a, b := wrap("arm64", "ADRP 3059712(PC), R1", "ADD $160, R1, R1", "ADRP 8192(PC), R2", "BIC $7, R9, R9", "MOVD (R2), R3"),
		wrap("arm64", "ADRP 3055616(PC), R1", "ADD $2808, R1, R1", "ADRP 4096(PC), R2", "BIC $7, R9, R9", "MOVD 64(R2), R3"); a.canon != b.canon {
		t.Fatalf("arm64 ADRP+pageoff relocation shift rejected: %q vs %q", a.canon, b.canon)
	}

	// Plain 32-bit integer immediates symbolized by x86asm against a shifted BSS
	// range reconstruct their exact literal value while still rejecting real
	// immediate mutations.
	forkTab := &elfSymTable{
		sorted: []elfSym{{name: "os.Stdin", addr: 0x901320, size: 8, isBSS: true}, {name: "pkg.bigbss", addr: 0x93a600, size: 0x2000000, isBSS: true}},
		byName: map[string]elfSym{
			"os.Stdin":   {name: "os.Stdin", addr: 0x901320, size: 8, isBSS: true},
			"pkg.bigbss": {name: "pkg.bigbss", addr: 0x93a600, size: 0x2000000, isBSS: true},
		},
	}
	stockTab := &elfSymTable{
		sorted: []elfSym{{name: "os.Stdin", addr: 0x901300, size: 8, isBSS: true}, {name: "pkg.bigbss", addr: 0x93a5e0, size: 0x2000000, isBSS: true}},
		byName: map[string]elfSym{
			"os.Stdin":   {name: "os.Stdin", addr: 0x901300, size: 8, isBSS: true},
			"pkg.bigbss": {name: "pkg.bigbss", addr: 0x93a5e0, size: 0x2000000, isBSS: true},
		},
	}
	if a, b := wrapWithELF("amd64", forkTab, "MOVL $pkg.bigbss+7101331(SB), DX"),
		wrapWithELF("amd64", stockTab, "MOVL $pkg.bigbss+7101363(SB), DX"); a.canon != b.canon {
		t.Fatalf("amd64 BSS-interior immediate constant shift rejected: %q vs %q", a.canon, b.canon)
	}
	if a, b := wrapWithELF("amd64", forkTab, "MOVL $pkg.bigbss+7101332(SB), DX"),
		wrapWithELF("amd64", stockTab, "MOVL $pkg.bigbss+7101363(SB), DX"); a.canon == b.canon {
		t.Fatalf("amd64 BSS-interior immediate mutation masked: %q == %q", a.canon, b.canon)
	}
	// IP-relative access at a zero-size section marker (pc=0x1000, encLen=4,
	// disp=0x90031c -> target=0x901320 = os.Stdin) resolves to os.Stdin(SB)
	// rather than masking named globals as ADDR(IP).
	if a, b := wrapWithELF("amd64", forkTab, "MOVQ 0x90031c(IP), DX"),
		wrapWithELF("amd64", stockTab, "MOVQ os.Stdin(SB), DX"); a.canon != b.canon {
		t.Fatalf("amd64 zero-size marker global resolution failed: %q vs %q", a.canon, b.canon)
	}
	if a, b := wrapWithELF("amd64", forkTab, "MOVQ 0x90031c(IP), DX"),
		wrapWithELF("amd64", stockTab, "MOVQ os.Stdout(SB), DX"); a.canon == b.canon {
		t.Fatalf("amd64 zero-size marker global swap masked: %q == %q", a.canon, b.canon)
	}

	// Real codegen deviations (named globals, struct/stack offsets, immediates,
	// registers, call targets, join-block fallthrough, LDP clobbers, indexed loads)
	// must NOT be masked.
	rejects := []struct {
		name string
		arch string
		a, b []string
	}{
		{"amd64-named-global", "amd64", []string{"MOVQ pkg.globA(SB), AX"}, []string{"MOVQ pkg.globB(SB), AX"}},
		{"amd64-typeassert-num", "amd64", []string{"LEAQ os..typeAssert.1(SB), AX"}, []string{"LEAQ os..typeAssert.2(SB), AX"}},
		{"amd64-stack-offset", "amd64", []string{"MOVQ 16(SP), AX"}, []string{"MOVQ 24(SP), AX"}},
		{"amd64-struct-offset", "amd64", []string{"MOVQ 32(R14), AX"}, []string{"MOVQ 40(R14), AX"}},
		{"amd64-immediate", "amd64", []string{"MOVQ $1, AX"}, []string{"MOVQ $2, AX"}},
		{"amd64-register", "amd64", []string{"MOVQ AX, BX"}, []string{"MOVQ AX, CX"}},
		{"amd64-call-target", "amd64", []string{"CALL pkg.foo(SB)"}, []string{"CALL pkg.bar(SB)"}},
		{"arm64-struct-offset", "arm64", []string{"MOVD 32(R28), R0"}, []string{"MOVD 40(R28), R0"}},
		{"arm64-non-adrp-add", "arm64", []string{"ADD $8, R1, R1"}, []string{"ADD $16, R1, R1"}},
		{"arm64-retired-adrp-add", "arm64", []string{"ADRP 4096(PC), R1", "ADD $16, R1, R1", "ADD $8, R1, R1"}, []string{"ADRP 4096(PC), R1", "ADD $16, R1, R1", "ADD $24, R1, R1"}},
		{"arm64-join-block-fallthrough", "arm64", []string{"CBZ R0, 2(PC)", "ADRP 4096(PC), R1", "MOVD 32(R1), R2"}, []string{"CBZ R0, 2(PC)", "ADRP 4096(PC), R1", "MOVD 40(R1), R2"}},
		{"arm64-join-block-noop-header", "arm64", []string{"CBZ R0, 2(PC)", "ADRP 4096(PC), R1", "NOOP", "MOVD 32(R1), R2"}, []string{"CBZ R0, 2(PC)", "ADRP 4096(PC), R1", "NOOP", "MOVD 40(R1), R2"}},
		{"arm64-ldp-clobber", "arm64", []string{"ADRP 4096(PC), R1", "LDP 16(RSP), (R1, R2)", "MOVD 8(R1), R3"}, []string{"ADRP 4096(PC), R1", "LDP 16(RSP), (R1, R2)", "MOVD 16(R1), R3"}},
		{"arm64-reg-index-not-pageoff", "arm64", []string{"ADRP 4096(PC), R1", "MOVD (R1)(R2), R3"}, []string{"ADRP 4096(PC), R1", "MOVD 8(R1), R3"}},
		{"arm64-call-target", "arm64", []string{"CALL pkg.foo(SB)"}, []string{"CALL pkg.bar(SB)"}},
	}
	for _, tc := range rejects {
		sa, sb := wrap(tc.arch, tc.a...), wrap(tc.arch, tc.b...)
		if sa.canon == sb.canon {
			t.Errorf("%s: canonicalizer masked real mutation (%q == %q)", tc.name, sa.canon, sb.canon)
		}
	}
	if s := wrap("amd64", "CALL runtime.dstPollBlock(SB)"); len(s.dstRaw) == 0 {
		t.Error("rawDstRef missed runtime.dstPollBlock(SB)")
	}
}
