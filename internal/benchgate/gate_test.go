package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBench(t *testing.T, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	content := "goos: linux\ngoarch: amd64\npkg: example.test/bench\n" + strings.Join(lines, "\n") + "\nPASS\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func benchLines(name string, count int, nsPerOp float64, extra string) []string {
	lines := make([]string, count)
	for i := range count {
		lines[i] = fmt.Sprintf("%s-4 \t     100\t      %.0f ns/op%s", name, nsPerOp, extra)
	}
	return lines
}

// gateCfg disables the required-suite presence check so unit tests can use
// minimal fixtures; the presence check has its own dedicated tests.
func gateCfg() gateConfig { return gateConfig{threshold: 0.30, alpha: 0.05, minCount: 4} }

func mustGate(t *testing.T, oldPath, newPath string) *gateResult {
	t.Helper()
	res, err := runGate(oldPath, newPath, gateCfg())
	if err != nil {
		t.Fatalf("runGate: %v", err)
	}
	return res
}

func findComparison(t *testing.T, res *gateResult, unit string) comparison {
	t.Helper()
	for _, c := range res.comparisons {
		if c.key.unit == unit {
			return c
		}
	}
	t.Fatalf("no comparison with unit %q (got %+v)", unit, res.comparisons)
	return comparison{}
}

func TestGate_TimingRegressionBeyondThresholdFails(t *testing.T) {
	// +100% with zero variance: significant and far beyond +30%.
	oldPath := writeBench(t, "old.txt", benchLines("BenchmarkX", 6, 1000, "")...)
	newPath := writeBench(t, "new.txt", benchLines("BenchmarkX", 6, 2000, "")...)
	res := mustGate(t, oldPath, newPath)
	if !res.failed() {
		t.Error("gate passed on a 2x timing regression")
	}
	c := findComparison(t, res, "sec/op")
	if !c.regression || c.kind != ruleTime {
		t.Errorf("comparison = %+v, want timing regression", c)
	}
}

func TestGate_SignificantButSmallDeltaPasses(t *testing.T) {
	// +10% is significant with zero variance but below the +30% threshold —
	// the gate must not chase noise-scale drift.
	oldPath := writeBench(t, "old.txt", benchLines("BenchmarkX", 6, 1000, "")...)
	newPath := writeBench(t, "new.txt", benchLines("BenchmarkX", 6, 1100, "")...)
	if res := mustGate(t, oldPath, newPath); res.failed() {
		t.Error("gate failed on a +10% delta below threshold")
	}
}

func TestGate_LargeButInsignificantDeltaPasses(t *testing.T) {
	// Medians differ hugely but the samples interleave, so Mann-Whitney
	// cannot call them different distributions — one noisy spike on a shared
	// runner must not fail the build.
	old := []string{}
	for _, v := range []int{1000, 5000, 1100, 4900, 1050, 4950} {
		old = append(old, fmt.Sprintf("BenchmarkX-4 \t 100\t %d ns/op", v))
	}
	newer := []string{}
	for _, v := range []int{4800, 1200, 5100, 1000, 5000, 1150} {
		newer = append(newer, fmt.Sprintf("BenchmarkX-4 \t 100\t %d ns/op", v))
	}
	oldPath := writeBench(t, "old.txt", old...)
	newPath := writeBench(t, "new.txt", newer...)
	if res := mustGate(t, oldPath, newPath); res.failed() {
		t.Errorf("gate failed on statistically indistinguishable samples: %+v", res.comparisons)
	}
}

func TestGate_ExactMetricIncreaseFails(t *testing.T) {
	// Deterministic guard metrics fail on any increase — no significance
	// test, because a single deterministic run is already exact.
	oldPath := writeBench(t, "old.txt", benchLines("BenchmarkScheduleMakespan", 6, 1000, "\t37.00 makespan-ticks/op")...)
	newPath := writeBench(t, "new.txt", benchLines("BenchmarkScheduleMakespan", 6, 1000, "\t38.00 makespan-ticks/op")...)
	res := mustGate(t, oldPath, newPath)
	if !res.failed() {
		t.Error("gate passed on makespan-ticks/op 37 -> 38")
	}
	c := findComparison(t, res, "makespan-ticks/op")
	if !c.regression || c.kind != ruleExact {
		t.Errorf("comparison = %+v, want exact regression", c)
	}
}

func TestGate_ExactMetricEqualPasses(t *testing.T) {
	oldPath := writeBench(t, "old.txt", benchLines("BenchmarkR", 6, 1000, "\t4.000 batchloads/op\t0 listloads/op")...)
	newPath := writeBench(t, "new.txt", benchLines("BenchmarkR", 6, 1000, "\t4.000 batchloads/op\t0 listloads/op")...)
	if res := mustGate(t, oldPath, newPath); res.failed() {
		t.Error("gate failed on identical exact metrics")
	}
}

func TestGate_PhaseMetricGatedAsTiming(t *testing.T) {
	oldPath := writeBench(t, "old.txt", benchLines("BenchmarkRun/scenario=full-hit", 6, 1e9, "\t40.00 prefetch-ms/op")...)
	newPath := writeBench(t, "new.txt", benchLines("BenchmarkRun/scenario=full-hit", 6, 1e9, "\t400.0 prefetch-ms/op")...)
	res := mustGate(t, oldPath, newPath)
	c := findComparison(t, res, "prefetch-ms/op")
	if c.kind != ruleTime || !c.regression {
		t.Errorf("prefetch-ms/op comparison = %+v, want timing regression", c)
	}
}

func TestGate_MsFloorSuppressesSmallPhaseDrift(t *testing.T) {
	// Verification finding: small-denominator phases (resolve ≈ 4ms) can be
	// significant AND beyond +30% on ~1ms of drift plus timer quantisation.
	// The absolute floor must absorb exactly this class. Samples reproduce
	// the experiment from the calibration: {3,3,3,4,4,4} -> {4,4,5,5,5,5}.
	old := []string{}
	for _, v := range []int{3, 3, 3, 4, 4, 4} {
		old = append(old, fmt.Sprintf("BenchmarkRun/scenario=cold-4 \t 1\t 1000000000 ns/op\t %d.00 resolve-ms/op", v))
	}
	newer := []string{}
	for _, v := range []int{4, 4, 5, 5, 5, 5} {
		newer = append(newer, fmt.Sprintf("BenchmarkRun/scenario=cold-4 \t 1\t 1000000000 ns/op\t %d.00 resolve-ms/op", v))
	}
	oldPath := writeBench(t, "old.txt", old...)
	newPath := writeBench(t, "new.txt", newer...)
	res := mustGate(t, oldPath, newPath)
	if res.failed() {
		t.Errorf("gate failed on millisecond-scale phase drift below the absolute floor: %+v", res.comparisons)
	}
	// A real blowup of the same phase must still trip (covered by the
	// 40->400ms case above: +360ms is far beyond the floor).
}

func TestGate_RequireSuiteFailsWhenGuardsVanish(t *testing.T) {
	// The gate must fail closed: a renamed benchmark or a broken -bench
	// regex removes guard metrics from the head file while `go test` stays
	// green — that has to surface as an error, not a pass.
	lines := benchLines("BenchmarkX", 6, 1000, "")
	oldPath := writeBench(t, "old.txt", lines...)
	newPath := writeBench(t, "new.txt", lines...)
	res, err := runGate(oldPath, newPath, gateConfig{threshold: 0.30, alpha: 0.05, minCount: 4, requireSuite: true})
	if err != nil {
		t.Fatalf("runGate: %v", err)
	}
	if !res.failed() || len(res.errs) == 0 {
		t.Errorf("gate passed although every suite guard metric is missing from head: errs=%v", res.errs)
	}
}

func TestGate_RequireSuitePassesWhenGuardsPresent(t *testing.T) {
	var lines []string
	lines = append(lines, benchLines("BenchmarkScheduleMakespan", 6, 1000, "\t37.00 makespan-ticks/op")...)
	lines = append(lines, benchLines("BenchmarkResolver/path=prewarmed", 6, 1000, "\t4.000 batchloads/op\t0 listloads/op")...)
	lines = append(lines, benchLines("BenchmarkResolver/path=inputs", 6, 1000, "\t1.000 enumcalls/op")...)
	lines = append(lines, benchLines("BenchmarkRun/scenario=full-hit/filehash=persist", 6, 2e8, "")...)
	oldPath := writeBench(t, "old.txt", lines...)
	newPath := writeBench(t, "new.txt", lines...)
	res, err := runGate(oldPath, newPath, gateConfig{threshold: 0.30, alpha: 0.05, minCount: 4, requireSuite: true})
	if err != nil {
		t.Fatalf("runGate: %v", err)
	}
	if res.failed() {
		t.Errorf("gate failed although all suite guard metrics are present: errs=%v", res.errs)
	}
}

func TestGate_DistinguishesSameBenchmarkNameAcrossPackages(t *testing.T) {
	// golocal and pnpmlocal both emit BenchmarkResolver; samples must not
	// merge across packages.
	oldContent := "goos: linux\ngoarch: amd64\npkg: example.test/a\n" +
		strings.Join(benchLines("BenchmarkResolver", 6, 1000, ""), "\n") +
		"\npkg: example.test/b\n" +
		strings.Join(benchLines("BenchmarkResolver", 6, 9000, ""), "\n") + "\n"
	path := filepath.Join(t.TempDir(), "both.txt")
	if err := os.WriteFile(path, []byte(oldContent), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := readBenchFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pkgs []string
	for k := range m {
		if k.unit == "sec/op" {
			pkgs = append(pkgs, k.pkg)
		}
	}
	if len(pkgs) != 2 {
		t.Errorf("expected 2 distinct (pkg, name, sec/op) series, got %d (%v)", len(pkgs), pkgs)
	}
}

func TestGate_AllocMetricsInformationalOnly(t *testing.T) {
	oldPath := writeBench(t, "old.txt", benchLines("BenchmarkX", 6, 1000, "\t100 B/op\t10 allocs/op")...)
	newPath := writeBench(t, "new.txt", benchLines("BenchmarkX", 6, 1000, "\t900 B/op\t90 allocs/op")...)
	if res := mustGate(t, oldPath, newPath); res.failed() {
		t.Error("gate failed on alloc metrics, which are informational")
	}
}

func TestGate_BenchmarkOnlyInHeadIsNotGated(t *testing.T) {
	// The merge-base legitimately lacks benchmarks a PR introduces (including
	// the PR that introduces the whole suite).
	oldPath := writeBench(t, "old.txt", benchLines("BenchmarkX", 6, 1000, "")...)
	newPath := writeBench(t, "new.txt", append(
		benchLines("BenchmarkX", 6, 1000, ""),
		benchLines("BenchmarkNew", 6, 500, "")...,
	)...)
	res := mustGate(t, oldPath, newPath)
	if res.failed() {
		t.Error("gate failed because head has an extra benchmark")
	}
	if len(res.onlyNew) == 0 {
		t.Error("expected the head-only benchmark to be reported as a note")
	}
}

func TestGate_InsufficientSamplesIsAnError(t *testing.T) {
	// Too few samples would make the significance test powerless and the
	// gate silently green; that must surface as a hard error instead.
	oldPath := writeBench(t, "old.txt", benchLines("BenchmarkX", 2, 1000, "")...)
	newPath := writeBench(t, "new.txt", benchLines("BenchmarkX", 2, 5000, "")...)
	res := mustGate(t, oldPath, newPath)
	if !res.failed() {
		t.Error("gate passed with 2 samples per side")
	}
	if len(res.errs) == 0 {
		t.Error("expected an insufficient-samples error")
	}
}

func TestGate_ToleratesNonBenchmarkNoise(t *testing.T) {
	// Real `go test` output interleaves ok/PASS lines and arbitrary logs.
	noisy := []string{"ok  \texample.test/other\t0.5s", "some stray log line"}
	noisy = append(noisy, benchLines("BenchmarkX", 6, 1000, "")...)
	oldPath := writeBench(t, "old.txt", noisy...)
	newPath := writeBench(t, "new.txt", benchLines("BenchmarkX", 6, 1000, "")...)
	if res := mustGate(t, oldPath, newPath); res.failed() {
		t.Errorf("gate failed on noisy input: %+v", res.errs)
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]ruleKind{
		"sec/op":            ruleTime,
		"ns/op":             ruleTime,
		"prefetch-ms/op":    ruleTime,
		"tasksrun-ms/op":    ruleTime,
		"makespan-ticks/op": ruleExact,
		"batchloads/op":     ruleExact,
		"listloads/op":      ruleExact,
		"enumcalls/op":      ruleExact,
		"B/op":              ruleInfo,
		"allocs/op":         ruleInfo,
		"somethingelse/op":  ruleInfo,
	}
	for unit, want := range cases {
		if got := classify(unit); got != want {
			t.Errorf("classify(%q) = %v, want %v", unit, got, want)
		}
	}
}
