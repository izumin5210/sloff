package depgraph_test

import (
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
)

// Benchmark graph fixed at the scale of the ADR-0020 pathology (~500 tasks:
// 400 wide + 20 chains of depth 5 + sink) with the slot count it was measured
// under, so the reported numbers stay comparable across runs.
const (
	benchWide   = 400
	benchChains = 20
	benchDepth  = 5
	benchSlots  = 14
)

// BenchmarkBuild tracks the pure CPU/alloc cost of depgraph.Build (Kahn +
// downstream-height priority) on the production-scale graph. Note this metric
// alone cannot detect an ADR-0020 scheduling regression — the tie-break costs
// roughly the same either way; BenchmarkScheduleMakespan carries that signal.
func BenchmarkBuild(b *testing.B) {
	tasks := starvationGraph(benchWide, benchChains, benchDepth)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := depgraph.Build(tasks); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScheduleMakespan is the ADR-0020 regression gate. makespan-ticks/op
// is the simulated wall-clock (unit ticks) of Build's CURRENT emit order on a
// slot-limited runner where waiters hold their slot; it is a deterministic
// function of the emitted order, so if the scheduling priority regresses (e.g.
// the height tie-break is dropped) the tick count jumps immediately and
// reproducibly, independent of machine speed.
func BenchmarkScheduleMakespan(b *testing.B) {
	tasks := starvationGraph(benchWide, benchChains, benchDepth)
	var lastMakespan int
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ordered, err := depgraph.Build(tasks)
		if err != nil {
			b.Fatal(err)
		}
		lastMakespan = simulateMakespan(ordered, benchSlots, unitDur)
	}
	b.ReportMetric(float64(lastMakespan), "makespan-ticks/op")
}
