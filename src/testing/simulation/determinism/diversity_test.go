// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package determinism

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/simulation"
	"time"
	_ "unsafe" // for go:linkname
)

// White-box readout of the runtime's seeded-decision trace (see
// runtime/dst.go, dstTraceState): a default-off, observation-only diagnostic
// recording, per seeded schedule-bearing decision, the site, candidate-set
// size, and chosen index. These tests quantify how much of the seed's entropy
// reaches each choice site — the "N seeds must explore N schedules" side of
// the DST contract — and pin that observing the schedule does not perturb it.

//go:linkname dstSchedTraceSetFP runtime.dstSchedTraceSetFP
func dstSchedTraceSetFP(on bool)

//go:linkname dstSchedTraceSummaryFP runtime.dstSchedTraceSummaryFP
func dstSchedTraceSummaryFP(site int) (hash, xorIdent, ndec, forced, multi uint64)

//go:linkname dstSchedTracePrefixFP runtime.dstSchedTracePrefixFP
func dstSchedTracePrefixFP(k int) uint64

//go:linkname dstSchedTraceCountFP runtime.dstSchedTraceCountFP
func dstSchedTraceCountFP(site, n, chosen int) uint64

//go:linkname xdstSchedStatsFP runtime.dstSchedStatsFP
func xdstSchedStatsFP() (decisions, sysScheds, rngDraws uint64)

// Site indices and histogram bounds; must match runtime/dst.go.
const (
	siteSched  = 0
	siteSelect = 1
	siteTimer  = 2
	numSites   = 3
	traceMaxN  = 8
	prefixK    = 21
)

var siteName = [numSites]string{"sched", "select", "timer"}

// diversitySeeds is the sweep width. The default is a bounded CI budget;
// DST_DIVERSITY_SEEDS=<n> widens it for measurement runs (read before any
// simulation starts, like the conformance harness's seed knob).
func diversitySeeds() int {
	if s := os.Getenv("DST_DIVERSITY_SEEDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 32
}

// traceRun captures one run's trace readout.
type traceRun struct {
	hash    [numSites]uint64
	xor     [numSites]uint64 // order-independent per-site draw fold
	ndec    [numSites]uint64
	forced  [numSites]uint64
	multi   [numSites]uint64
	prefix  [prefixK]uint64
	count   [numSites][traceMaxN + 1][traceMaxN]uint64
	decs    uint64 // scheduler decisions (system picks included)
	sys     uint64 // system (infrastructure) picks
	rngDraw uint64
}

func readTrace() (r traceRun) {
	for s := 0; s < numSites; s++ {
		r.hash[s], r.xor[s], r.ndec[s], r.forced[s], r.multi[s] = dstSchedTraceSummaryFP(s)
		for n := 0; n <= traceMaxN; n++ {
			for c := 0; c < traceMaxN; c++ {
				r.count[s][n][c] = dstSchedTraceCountFP(s, n, c)
			}
		}
	}
	for k := 0; k < prefixK; k++ {
		r.prefix[k] = dstSchedTracePrefixFP(k)
	}
	r.decs, r.sys, r.rngDraw = xdstSchedStatsFP()
	return r
}

// electionProgram is the seed-basin resonance shape from the consumer report
// (an election livelock reproducing only in a narrow seed basin): candidate
// nodes race randomized election timeouts, and whoever's timeout resolves
// first claims the term; losers observe the advance, back off, and retry.
// Almost all scheduling decisions in this shape are clock-forced — the seed's
// leverage is the timeout VALUES plus the rare same-instant wakeup — so its
// measured diversity characterizes exactly the class the consumer hit.
func electionProgram(seed uint64, opts simulation.Options) string {
	var b strings.Builder
	simulation.RunWith(seed, opts, func() {
		const nodes = 5
		const terms = 8
		var term atomic.Int64
		var mu sync.Mutex
		record := func(t int64, id int) {
			mu.Lock()
			fmt.Fprintf(&b, "t%d:n%d@%d ", t, id, time.Now().UnixNano())
			mu.Unlock()
		}
		var wg sync.WaitGroup
		for i := 0; i < nodes; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for {
					cur := term.Load()
					if cur >= terms {
						return
					}
					// Randomized election timeout on the virtual clock,
					// millisecond-granular like a production raft tick.
					time.Sleep(time.Duration(10+rand.Intn(20)) * time.Millisecond)
					if term.CompareAndSwap(cur, cur+1) {
						record(cur+1, id)
					}
				}
			}(i)
		}
		wg.Wait()
	})
	return b.String()
}

// TestSchedTraceNeutrality pins observation neutrality: enabling the
// seeded-decision trace leaves the run's transcript byte-identical, under
// both the Random and PCT strategies. A diagnostic that perturbed the
// schedule would describe a run nobody executes.
func TestSchedTraceNeutrality(t *testing.T) {
	defer dstSchedTraceSetFP(false)
	for _, seed := range []uint64{1, 11, 20260605} {
		dstSchedTraceSetFP(false)
		off := sweepProgram(seed)
		dstSchedTraceSetFP(true)
		on := sweepProgram(seed)
		if off != on {
			t.Fatalf("sweep transcript differs with trace enabled at seed %d:\n--- off ---\n%s\n--- on ---\n%s", seed, off, on)
		}
		pct := simulation.Options{Strategy: simulation.PCT, Depth: 3}
		dstSchedTraceSetFP(false)
		off = electionProgram(seed, pct)
		dstSchedTraceSetFP(true)
		on = electionProgram(seed, pct)
		if off != on {
			t.Fatalf("PCT election transcript differs with trace enabled at seed %d:\n--- off ---\n%s\n--- on ---\n%s", seed, off, on)
		}
	}
}

// sweepStats aggregates a seed sweep of one traced program.
type sweepStats struct {
	runs      []traceRun
	distinct  map[uint64]int // scheduler-site full-run fingerprints
	programs  map[string]int // program-visible transcripts
	agg       [numSites][traceMaxN + 1][traceMaxN]uint64
	forcedSch uint64
	multiSch  uint64
	ndecSch   uint64
}

func sweepTraced(seeds int, program func(seed uint64) string) sweepStats {
	st := sweepStats{distinct: map[uint64]int{}, programs: map[string]int{}}
	for seed := 0; seed < seeds; seed++ {
		out := program(uint64(seed) + 1)
		r := readTrace()
		st.runs = append(st.runs, r)
		st.distinct[r.hash[siteSched]]++
		st.programs[out]++
		for s := 0; s < numSites; s++ {
			for n := 0; n <= traceMaxN; n++ {
				for c := 0; c < traceMaxN; c++ {
					st.agg[s][n][c] += r.count[s][n][c]
				}
			}
		}
		st.forcedSch += r.forced[siteSched]
		st.multiSch += r.multi[siteSched]
		st.ndecSch += r.ndec[siteSched]
	}
	return st
}

// entropyRow returns the Shannon entropy (bits) and max share of one
// aggregated chosen-index distribution, with its total.
func entropyRow(row [traceMaxN]uint64) (bits, maxShare float64, total uint64) {
	for _, c := range row {
		total += c
	}
	if total == 0 {
		return 0, 0, 0
	}
	for _, c := range row {
		if c == 0 {
			continue
		}
		p := float64(c) / float64(total)
		bits -= p * math.Log2(p)
		if p > maxShare {
			maxShare = p
		}
	}
	return bits, maxShare, total
}

// report logs the sweep's diversity numbers: schedule distinctness, prefix
// divergence, forced-decision share, and the per-site chosen-index entropy
// table, flagging degenerate hot spots (max share > 0.9 on a multi-candidate
// row with enough mass to mean it).
func (st *sweepStats) report(t *testing.T, label string) (hotSpots int) {
	seeds := len(st.runs)
	t.Logf("%s: %d seeds; distinct schedules %d; distinct program outcomes %d",
		label, seeds, len(st.distinct), len(st.programs))
	var meanDec, meanSys float64
	for _, r := range st.runs {
		meanDec += float64(r.decs)
		meanSys += float64(r.sys)
	}
	meanDec /= float64(seeds)
	meanSys /= float64(seeds)
	forcedShare := float64(st.forcedSch) / float64(max(st.ndecSch, 1))
	t.Logf("%s: mean decisions/run %.0f (infra picks %.0f = %.1f%%); sched-site decisions %d, forced (set size 1) %.1f%%, multi-candidate %.1f%%",
		label, meanDec, meanSys, 100*meanSys/meanDec, st.ndecSch, 100*forcedShare, 100*float64(st.multiSch)/float64(max(st.ndecSch, 1)))
	for k := 0; k < prefixK; k++ {
		pfx := map[uint64]int{}
		have := 0
		for _, r := range st.runs {
			if r.prefix[k] != 0 {
				pfx[r.prefix[k]]++
				have++
			}
		}
		if have == 0 {
			break
		}
		t.Logf("%s: distinct schedule prefixes after %d decisions: %d/%d", label, 1<<k, len(pfx), have)
	}
	for s := 0; s < numSites; s++ {
		for n := 2; n <= traceMaxN; n++ {
			bits, maxShare, total := entropyRow(st.agg[s][n])
			if total == 0 {
				continue
			}
			width := n
			if s == siteTimer {
				width = traceMaxN // key draw: fixed 8-bucket histogram
			}
			uniform := math.Log2(float64(width))
			flag := ""
			// The capped top bucket (sizes >= traceMaxN with the chosen index
			// capped too) legitimately concentrates mass in its last cell for
			// large candidate sets, so it is reported but exempt from the
			// degenerate flag; exact sizes 2..traceMaxN-1 and the timer
			// site's uncapped 8-bucket key histogram are flagged.
			capped := n == traceMaxN && s != siteTimer
			if maxShare > 0.9 && total >= 100 && !capped {
				hotSpots++
				flag = "  <-- DEGENERATE"
			}
			t.Logf("%s: site %-6s n=%d%s: draws %7d entropy %.3f/%.3f bits max-share %.3f%s",
				label, siteName[s], n, map[bool]string{true: "+", false: " "}[n == traceMaxN], total, bits, uniform, maxShare, flag)
		}
	}
	return hotSpots
}

// TestSeededScheduleDiversity measures the plain seeded (Random-strategy)
// path's decision distribution over the composed sweep program and pins the
// diversity floor: distinct schedules across seeds, no degenerate hot spot,
// and seed entropy visibly reaching every seeded choice site. The full
// measurement (DST_DIVERSITY_SEEDS widens the sweep) is recorded in
// docs/dst/exploration.md, "Measured seeded-path diversity".
func TestSeededScheduleDiversity(t *testing.T) {
	seeds := diversitySeeds()
	dstSchedTraceSetFP(true)
	defer dstSchedTraceSetFP(false)
	st := sweepTraced(seeds, sweepProgram)
	if hot := st.report(t, "sweep/random"); hot > 0 {
		t.Errorf("degenerate choice hot spots on the seeded path: %d (site,size) rows concentrate >90%% on one pick", hot)
	}

	if len(st.distinct) < seeds/2 {
		t.Errorf("schedule diversity collapsed: %d distinct schedules over %d seeds", len(st.distinct), seeds)
	}
	// Seed entropy must reach every seeded schedule-bearing site. Two pins,
	// each catching what the other cannot; a failed site means N seeds
	// explore fewer than N schedules — the silent false-negative class:
	//  - CROSS-SEED variation: each site's order-independent draw fold (xor
	//    of idents) takes at least two values across the sweep. A site whose
	//    seeded stream is frozen (e.g. a per-g stream rooted independently
	//    of the seed) has an identical per-goroutine draw multiset at every
	//    seed, so its fold is identical too — even when residual schedule
	//    variation reorders the draws, which would still vary an
	//    order-DEPENDENT stream hash and mask the freeze. The fold is
	//    order-immune, not COUNT-immune: this detection relies on
	//    sweepProgram's per-goroutine draw counts being schedule-independent
	//    (each worker runs exactly one select and arms fixed timers). An
	//    edit giving sweepProgram schedule-dependent draw counts (a retry
	//    loop, a conditional select) silently weakens this pin — keep the
	//    fixed-count shape or re-pin. The frozen SCHEDULER stream, whose
	//    fold ident is schedule-fed (chosen goroutine identity), is pinned
	//    separately by runtime's TestDSTScheduleDiversity over
	//    select/timer-free scenarios whose interleaving derives from the
	//    scheduling RNG alone.
	//  - choice variation: some multi-candidate row hits at least two chosen
	//    buckets. A site degenerating to one pick is caught here even when
	//    its draws still vary (the fold sees draw values, not chosen
	//    ordinals).
	for s := 0; s < numSites; s++ {
		folds := map[uint64]bool{}
		active := 0
		for _, r := range st.runs {
			if r.ndec[s] > 0 {
				active++
				folds[r.xor[s]] = true
			}
		}
		if active > 1 && len(folds) < 2 {
			t.Errorf("site %s decision stream is seed-invariant: %d runs recorded decisions, all with the identical order-independent draw fold", siteName[s], active)
		}
		varied := false
		for n := 2; n <= traceMaxN && !varied; n++ {
			nonzero := 0
			for c := 0; c < traceMaxN; c++ {
				if st.agg[s][n][c] != 0 {
					nonzero++
				}
			}
			varied = nonzero >= 2
		}
		if !varied {
			t.Errorf("seed entropy does not reach site %s: no multi-candidate decision varied its pick across %d seeds", siteName[s], seeds)
		}
	}
}

// TestElectionScheduleDiversity measures the consumer-witnessed resonance
// shape (timeout-racing election churn) under Random and under PCT, reporting
// how many meaningfully-distinct outcomes a seed sweep buys on a program
// whose decisions are almost entirely clock-forced.
func TestElectionScheduleDiversity(t *testing.T) {
	seeds := diversitySeeds()
	dstSchedTraceSetFP(true)
	defer dstSchedTraceSetFP(false)

	// The program key is the winner SEQUENCE with virtual timestamps stripped:
	// timestamps differ whenever the drawn timeouts differ, which would count
	// near-identical outcomes as distinct and mask basin structure. Distinct
	// winner sequences are the "meaningfully-distinct outcome" count.
	winners := func(transcript string) string {
		var b strings.Builder
		for _, f := range strings.Fields(transcript) {
			if i := strings.IndexByte(f, '@'); i >= 0 {
				f = f[:i]
			}
			b.WriteString(f + " ")
		}
		return b.String()
	}

	random := sweepTraced(seeds, func(seed uint64) string {
		return winners(electionProgram(seed, simulation.Options{}))
	})
	// The hot-spot floor is enforced on the Random path only: PCT's
	// priority-directed picks are legitimately non-uniform and are reported
	// below for comparison, not gated.
	if hot := random.report(t, "election/random"); hot > 0 {
		t.Errorf("degenerate choice hot spots on the seeded path: %d (site,size) rows concentrate >90%% on one pick", hot)
	}
	if len(random.programs) < 2 {
		t.Errorf("election outcomes identical across %d seeds; the seed must vary the run", seeds)
	}

	for _, d := range []int{1, 3} {
		pct := sweepTraced(seeds, func(seed uint64) string {
			return winners(electionProgram(seed, simulation.Options{Strategy: simulation.PCT, Depth: d, Steps: 2000}))
		})
		pct.report(t, fmt.Sprintf("election/pct-d%d", d))
	}
}

// TestSelectTraceIdentFullWidth pins the select site's trace ident as the RAW
// 64-bit draw, not the bounded chosen index: with tiny candidate counts a
// bounded ident aliases in the order-independent xorIdent freeze-detection
// fold (a two-case select folds only 0/1 — two seeds with equal chosen-index
// multisets fold identically even when their draw streams differ), so a
// frozen select stream could hide behind equal folds. The program records
// only two-case selects at the select site; the raw-draw ident makes its fold
// full-width, while a bounded ident caps it at 1. Mutation: recording the
// chosen index as the ident drives the fold back into {0,1}.
func TestSelectTraceIdentFullWidth(t *testing.T) {
	dstSchedTraceSetFP(true)
	defer dstSchedTraceSetFP(false)
	simulation.Run(7, func() {
		a, b := make(chan int, 1), make(chan int, 1)
		for i := 0; i < 32; i++ {
			a <- i
			b <- i
			select {
			case <-a:
				<-b
			case <-b:
				<-a
			}
		}
	})
	_, xor, ndec, _, _ := dstSchedTraceSummaryFP(siteSelect)
	if ndec == 0 {
		t.Fatal("no select decisions recorded; the pin is vacuous")
	}
	if xor <= 1 {
		t.Fatalf("select-site xorIdent = %#x over %d two-case decisions: the fold is bounded-index-shaped; the ident must be the raw draw", xor, ndec)
	}
	if xor < 1<<32 {
		t.Fatalf("select-site xorIdent = %#x over %d decisions; a full-width raw-draw ident is expected", xor, ndec)
	}
}

//go:linkname dstWedgeResolvedBoundsFP runtime.dstWedgeResolvedBoundsFP
func dstWedgeResolvedBoundsFP() (decisions uint64, wallNs int64)

// TestWedgeBoundsResolution pins the wedge-detector knob resolution the
// Options docs promise: 0 selects the defaults (1<<26 decisions, 60s), a
// positive value is used exactly, and a NEGATIVE value disables the arm
// (resolved bound 0) rather than silently selecting the default — the arm a
// disabled-detector behavioral pin cannot cover without waiting out a wedge.
// Mutation: folding the negative case into the default makes the disabled
// legs read the default bounds.
func TestWedgeBoundsResolution(t *testing.T) {
	read := func(opts simulation.Options) (dec uint64, wall int64) {
		simulation.RunWith(1, opts, func() {
			dec, wall = dstWedgeResolvedBoundsFP()
		})
		return
	}
	if dec, wall := read(simulation.Options{}); dec != 1<<26 || wall != 60_000_000_000 {
		t.Errorf("default bounds = (%d, %d), want (1<<26, 60s)", dec, wall)
	}
	if dec, wall := read(simulation.Options{WedgeDecisionLimit: -1, WedgeWallLimit: -1}); dec != 0 || wall != 0 {
		t.Errorf("negative limits = (%d, %d), want both arms disabled (0, 0)", dec, wall)
	}
	if dec, wall := read(simulation.Options{WedgeDecisionLimit: 12345, WedgeWallLimit: 7 * time.Second}); dec != 12345 || wall != 7_000_000_000 {
		t.Errorf("explicit limits = (%d, %d), want (12345, 7s)", dec, wall)
	}
}
