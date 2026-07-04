package golocal_test

// Regression guard for the PR #53 prewarm batching. The guarded quantity is
// NOT CPU throughput but the NUMBER of packages.Load invocations, observed as
// List / ListBatch calls on the underlying SourceLister: pre-#53 the runner
// paid one packages.Load per declared tool, while post-#53 Resolver.Prewarm
// collapses each spec dir's tools into a single ListBatch that primes
// lister.Memoized, so the per-tool Inputs/Versions calls that follow are
// cache hits. The benchmark reports the call counts as deterministic custom
// metrics (b.ReportMetric) for the CI gate, and TestPrewarmedResolveCallCounts
// asserts them exactly so the guard also fires in the normal -race test job.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/golocal"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
)

// countingBatchLister is a lister.BatchSourceLister that counts how many
// times each load path runs: one List call stands in for one packages.Load
// per entry (the pre-#53 cost model), one ListBatch call for one
// packages.Load per spec dir (the post-#53 cost model). Prewarm fans batches
// out concurrently (errgroup capped at NumCPU), so the counters are
// mutex-protected to keep the guard race-clean.
type countingBatchLister struct {
	mu         sync.Mutex
	listCalls  int
	batchCalls int
}

// benchListing is the fixed per-entry result: a couple of internal files and
// one external module — enough for Inputs and Versions to do real
// copy/encode work without the fixture dominating the measurement.
func benchListing() lister.Listing {
	return lister.Listing{
		InternalFiles: []string{
			"cmd/tool/main.go",
			"internal/shared/shared.go",
		},
		ExternalModules: []lister.ExternalModule{
			{Path: "example.com/dep", Version: "v1.2.3", GoSumLine: "example.com/dep v1.2.3 h1:abc"},
		},
	}
}

func (c *countingBatchLister) List(_ context.Context, _, _ string) (lister.Listing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listCalls++
	return benchListing(), nil
}

func (c *countingBatchLister) ListBatch(_ context.Context, _ string, entries []string) (map[string]lister.Listing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.batchCalls++
	out := make(map[string]lister.Listing, len(entries))
	for _, e := range entries {
		out[e] = benchListing()
	}
	return out, nil
}

func (c *countingBatchLister) counts() (batchCalls, listCalls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.batchCalls, c.listCalls
}

// benchSpecDirs x benchEntriesPerDir is the 64-tool fixture: a monorepo with
// 4 spec dirs, each declaring 16 go-local tools. Big enough that the pre-#53
// cost model (64 loads) is unmistakably different from the post-#53 one (4).
var benchSpecDirs = []string{"", "services/api", "services/web", "tools"}

const benchEntriesPerDir = 16

func prewarmBenchReqs() []toolresolver.PrewarmRequest {
	reqs := make([]toolresolver.PrewarmRequest, 0, len(benchSpecDirs)*benchEntriesPerDir)
	for _, specDir := range benchSpecDirs {
		for i := range benchEntriesPerDir {
			reqs = append(reqs, toolresolver.PrewarmRequest{
				SpecDir: specDir,
				Declared: &toolresolver.DeclaredTool{
					Resolver: golocal.Name,
					Entry:    fmt.Sprintf("./cmd/tool%02d", i),
				},
			})
		}
	}
	return reqs
}

// runPrewarmedResolve executes one runner-shaped resolve pass — Prewarm once,
// then Inputs+Versions per tool — against a FRESH memoised lister and returns
// the underlying load counts. Freshness matters: lister.Memoized caches for
// its lifetime, so reusing one resolver across benchmark iterations would
// measure map hits only and hide a batching regression entirely.
func runPrewarmedResolve(ctx context.Context, tb testing.TB, reqs []toolresolver.PrewarmRequest) (batchCalls, listCalls int) {
	tb.Helper()
	fake := &countingBatchLister{}
	r := golocal.New("/repo", lister.NewMemoized(fake))
	if err := r.Prewarm(ctx, reqs); err != nil {
		tb.Fatalf("Prewarm: %v", err)
	}
	for i := range reqs {
		if _, err := r.Inputs(ctx, reqs[i].SpecDir, reqs[i].Declared); err != nil {
			tb.Fatalf("Inputs(%q, %q): %v", reqs[i].SpecDir, reqs[i].Declared.Entry, err)
		}
		if _, err := r.Versions(ctx, reqs[i].SpecDir, reqs[i].Declared); err != nil {
			tb.Fatalf("Versions(%q, %q): %v", reqs[i].SpecDir, reqs[i].Declared.Entry, err)
		}
	}
	return fake.counts()
}

func BenchmarkResolver(b *testing.B) {
	b.Run("path=prewarmed", func(b *testing.B) {
		ctx := context.Background()
		reqs := prewarmBenchReqs()
		b.ReportAllocs()
		var batchCalls, listCalls int
		for b.Loop() {
			batchCalls, listCalls = runPrewarmedResolve(ctx, b, reqs)
		}
		// The counts are deterministic (identical every iteration), so the
		// last iteration's counters ARE the per-op values. The CI gate fails
		// on ANY increase: batchloads/op above 4 or listloads/op above 0
		// means the #53 prewarm batching regressed and packages.Load is
		// being paid more than once per spec dir again.
		b.ReportMetric(float64(batchCalls), "batchloads/op")
		b.ReportMetric(float64(listCalls), "listloads/op")
	})
}

// TestPrewarmedResolveCallCounts is the same guard as the benchmark metric,
// expressed as a plain test so it also fires in the ordinary -race test job
// (benchmarks don't run there).
func TestPrewarmedResolveCallCounts(t *testing.T) {
	batchCalls, listCalls := runPrewarmedResolve(context.Background(), t, prewarmBenchReqs())
	if batchCalls != 4 || listCalls != 0 {
		t.Errorf(
			"prewarmed resolve of 64 tools across 4 spec dirs issued ListBatch=%d List=%d, want ListBatch=4 (one per spec dir) List=0 (all primed).\n"+
				"Any other value means the PR #53 prewarm batching regressed: Prewarm no longer collapses each spec dir into one packages.Load, "+
				"or Inputs/Versions miss the memoised cache and pay a per-tool load again. If the batching design changed intentionally, "+
				"update this guard and the ADR-0021 bench docs.",
			batchCalls, listCalls,
		)
	}
}
