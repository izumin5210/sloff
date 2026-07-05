// Package main implements benchgate, the benchmark regression gate used by
// the CI bench job (see docs/adr/0021-benchmark-suite-and-regression-gate.md).
//
// It compares two `go test -bench` output files — the PR's merge-base and the
// PR head, produced interleaved on the same runner — and fails when a metric
// regresses beyond its class threshold:
//
//   - timing metrics (sec/op and the runner phase *-ms/op metrics) regress
//     only when the delta is BOTH statistically significant (Mann-Whitney U,
//     no distributional assumption) AND larger than -threshold. Shared-runner
//     noise produces one or the other, rarely both.
//   - deterministic metrics (makespan-ticks/op, batchloads/op, listloads/op,
//     enumcalls/op) are pure functions of the code, so ANY increase is a
//     regression — these guard the scheduling/batching optimisations that
//     wall-clock numbers cannot see.
//   - everything else (B/op, allocs/op, unknown units) is informational.
//
// Benchmarks present on only one side are reported but never fail the gate:
// the merge-base legitimately lacks benchmarks the PR adds.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/perf/benchfmt"
	"golang.org/x/perf/benchmath"
)

type ruleKind int

const (
	ruleInfo ruleKind = iota
	ruleTime
	ruleExact
)

// exactUnits are the deterministic guard metrics. Every value must be
// identical run-to-run; an increase means the guarded optimisation regressed
// (or its design changed intentionally — then the guard and ADR-0021 must be
// updated in the same PR, which re-baselines automatically since the gate
// compares against the merge-base).
var exactUnits = map[string]struct{}{
	"makespan-ticks/op": {},
	"batchloads/op":     {},
	"listloads/op":      {},
	"enumcalls/op":      {},
}

func classify(unit string) ruleKind {
	if _, ok := exactUnits[unit]; ok {
		return ruleExact
	}
	// benchfmt tidies ns/op to sec/op; accept both in case of raw input.
	if unit == "sec/op" || unit == "ns/op" || strings.HasSuffix(unit, "-ms/op") {
		return ruleTime
	}
	return ruleInfo
}

type metricKey struct {
	pkg  string // benchmarks in different packages may share a name (both resolver packages emit BenchmarkResolver)
	name string // full benchmark name, including sub-benchmark config and -GOMAXPROCS suffix
	unit string
}

func (k metricKey) String() string { return k.pkg + " " + k.name + " " + k.unit }

func readBenchFile(path string) (map[metricKey][]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[metricKey][]float64{}
	r := benchfmt.NewReader(f, path)
	for r.Scan() {
		res, ok := r.Result().(*benchfmt.Result)
		if !ok {
			continue // SyntaxError records: tolerate stray non-benchmark output lines
		}
		name := string(res.Name.Full())
		pkg := res.GetConfig("pkg")
		for _, v := range res.Values {
			k := metricKey{pkg: pkg, name: name, unit: v.Unit}
			out[k] = append(out[k], v.Value)
		}
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type comparison struct {
	key        metricKey
	kind       ruleKind
	oldCenter  float64
	newCenter  float64
	p          float64 // NaN when not applicable (exact/info without test)
	delta      float64 // (new-old)/old; +Inf when old == 0 and new > 0
	regression bool
	note       string
}

type gateResult struct {
	comparisons []comparison
	onlyOld     []metricKey
	onlyNew     []metricKey
	errs        []string
}

func (g *gateResult) failed() bool {
	for _, c := range g.comparisons {
		if c.regression {
			return true
		}
	}
	return len(g.errs) > 0
}

type gateConfig struct {
	threshold float64 // e.g. 0.30 = fail timing metrics beyond +30%
	alpha     float64 // significance level for the Mann-Whitney test
	minCount  int     // minimum samples per side for timing metrics
	// requireSuite errors when the head file lacks the suite's core metrics.
	// Without it the gate fails open: renaming BenchmarkRun, moving a
	// package, or breaking the -bench regex silently removes every guard
	// while the job stays green.
	requireSuite bool
}

// msAbsFloor is the minimum absolute worsening (in ms) before a *-ms/op
// phase metric can fail the gate. Runner-noise calibration (LESSONS.md)
// showed the small-denominator phases (resolve ≈ 1–7ms, fpload ≈ 5–15ms)
// breach both the significance and the relative threshold on ~1ms drift and
// timer quantisation alone; a millisecond-scale floor removes that class of
// false positive while a real small-phase blowup (e.g. resolve 4ms → 200ms)
// still trips.
const msAbsFloor = 25.0

// requiredHeadUnits are the deterministic guard metrics every gate run must
// see on the head side; their absence means the corresponding benchmark
// vanished (rename / regex drift / package move), not that it passed.
var requiredHeadUnits = []string{
	"makespan-ticks/op",
	"batchloads/op",
	"listloads/op",
	"enumcalls/op",
}

// checkRequiredSuite appends an error per missing core metric. The macro
// check keys on the benchmark name because sec/op alone is emitted by every
// benchmark.
func checkRequiredSuite(newM map[metricKey][]float64, res *gateResult) {
	present := map[string]bool{}
	macroSeen := false
	for k := range newM {
		present[k.unit] = true
		if strings.Contains(k.name, "Run/scenario=") && k.unit == "sec/op" {
			macroSeen = true
		}
	}
	for _, u := range requiredHeadUnits {
		if !present[u] {
			res.errs = append(res.errs, fmt.Sprintf(
				"required metric %q missing from head: its guard benchmark vanished (rename/regex/package move?)", u,
			))
		}
	}
	if !macroSeen {
		res.errs = append(res.errs, "no BenchmarkRun/scenario=* sec/op in head: the macro suite vanished")
	}
}

func runGate(oldPath, newPath string, cfg gateConfig) (*gateResult, error) {
	oldM, err := readBenchFile(oldPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", oldPath, err)
	}
	newM, err := readBenchFile(newPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", newPath, err)
	}
	return compare(oldM, newM, cfg), nil
}

func compare(oldM, newM map[metricKey][]float64, cfg gateConfig) *gateResult {
	res := &gateResult{}
	keys := map[metricKey]struct{}{}
	for k := range oldM {
		keys[k] = struct{}{}
	}
	for k := range newM {
		keys[k] = struct{}{}
	}
	sorted := make([]metricKey, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })

	if cfg.requireSuite {
		checkRequiredSuite(newM, res)
	}

	for _, k := range sorted {
		oldVals, inOld := oldM[k]
		newVals, inNew := newM[k]
		switch {
		case !inOld:
			res.onlyNew = append(res.onlyNew, k)
			continue
		case !inNew:
			res.onlyOld = append(res.onlyOld, k)
			continue
		}
		res.comparisons = append(res.comparisons, compareOne(k, oldVals, newVals, cfg, res))
	}
	return res
}

func compareOne(k metricKey, oldVals, newVals []float64, cfg gateConfig, res *gateResult) comparison {
	kind := classify(k.unit)
	c := comparison{key: k, kind: kind}

	oldSample := benchmath.NewSample(oldVals, &benchmath.DefaultThresholds)
	newSample := benchmath.NewSample(newVals, &benchmath.DefaultThresholds)
	c.oldCenter = benchmath.AssumeNothing.Summary(oldSample, 0.95).Center
	c.newCenter = benchmath.AssumeNothing.Summary(newSample, 0.95).Center
	c.delta = deltaOf(c.oldCenter, c.newCenter)

	switch kind {
	case ruleExact:
		// Deterministic metric: tiny tolerance only for float re-encoding.
		if c.newCenter > c.oldCenter*1.001+1e-9 {
			c.regression = true
			c.note = "deterministic metric increased"
		}
	case ruleTime:
		if len(oldVals) < cfg.minCount || len(newVals) < cfg.minCount {
			// Too few samples means the significance test cannot reject
			// anything and the gate would silently wave regressions through.
			// That is a CI configuration error, not a pass.
			res.errs = append(res.errs, fmt.Sprintf(
				"%s: %d/%d samples, need >= %d per side for the timing gate", k, len(oldVals), len(newVals), cfg.minCount,
			))
			c.note = "insufficient samples"
			return c
		}
		cmp := benchmath.AssumeNothing.Compare(oldSample, newSample)
		c.p = cmp.P
		if cmp.P < cfg.alpha && c.delta > cfg.threshold {
			if strings.HasSuffix(k.unit, "-ms/op") && c.newCenter-c.oldCenter < msAbsFloor {
				c.note = fmt.Sprintf("below %gms absolute floor (small-denominator phase)", msAbsFloor)
				return c
			}
			c.regression = true
			c.note = fmt.Sprintf("significant (p=%.3f) and beyond +%.0f%%", cmp.P, cfg.threshold*100)
		}
	case ruleInfo:
		// reported, never gated
	}
	return c
}

func deltaOf(oldC, newC float64) float64 {
	if oldC == 0 {
		if newC == 0 {
			return 0
		}
		return 1e18 // effectively +Inf but table-printable
	}
	return (newC - oldC) / oldC
}

func (g *gateResult) render(w io.Writer) {
	kindLabel := map[ruleKind]string{ruleInfo: "info", ruleTime: "time", ruleExact: "exact"}
	fmt.Fprintf(w, "%-90s %-22s %-6s %12s %12s %9s %8s  %s\n",
		"benchmark", "unit", "class", "old", "new", "delta", "p", "verdict")
	for _, c := range g.comparisons {
		verdict := "ok"
		if c.regression {
			verdict = "REGRESSION: " + c.note
		} else if c.note != "" {
			verdict = c.note
		}
		p := "-"
		if c.kind == ruleTime && c.note != "insufficient samples" {
			p = fmt.Sprintf("%.3f", c.p)
		}
		fmt.Fprintf(w, "%-90s %-22s %-6s %12.5g %12.5g %+8.1f%% %8s  %s\n",
			c.key.name, c.key.unit, kindLabel[c.kind], c.oldCenter, c.newCenter, c.delta*100, p, verdict)
	}
	for _, k := range g.onlyNew {
		fmt.Fprintf(w, "note: %s only in head (new benchmark; not gated)\n", k)
	}
	for _, k := range g.onlyOld {
		fmt.Fprintf(w, "note: %s only in base (removed benchmark; not gated)\n", k)
	}
	for _, e := range g.errs {
		fmt.Fprintf(w, "error: %s\n", e)
	}
}
