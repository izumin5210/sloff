package runner_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/izumin5210/sloff/internal/sloff/benchgen"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

// Macro-benchmarks drive full in-process runs against a synthetic monorepo at
// the deployment scale the perf ADRs cite (~500 tasks, ~30k input files, deep
// chains; see benchgen). Generator commands are trivial `cat`s, so the numbers
// isolate sloff's own orchestration overhead — the quantity the perf PRs
// (#17/#47/#52/#54/#57, ADR-0014, ADR-0020) optimised. Scenarios:
//
//   - cold:             outputs on disk (git-managed model / fresh clone), no
//     fingerprint records — every task executes.
//   - warm-incremental: one chain-head input rewritten per run — the mutated
//     chain and the sink re-run, everything else hits.
//   - full-hit:         nothing changed — the minutes-to-seconds path of #17.
//
// warm-incremental and full-hit run twice: filehash=persist uses the ADR-0014
// cross-run digest cache; filehash=memory disables it (the
// SLOFF_NO_FILE_HASH_CACHE escape hatch wired through
// Options.FileHashCachePath=""). The persist-vs-memory gap is the standing
// sensitivity proof that the persistent cache still pays for itself.
//
// The racy-file guard of ADR-0014 refuses to persist digests of files whose
// mtime/ctime is within ~2s of run start, so persist-mode setup sleeps
// benchSettleDelay after the last write before warming the store; without it
// the store would silently stay empty and the benchmark would lie.
const benchSettleDelay = 2100 * time.Millisecond

// benchScale is what the macro-benchmarks run at: 501 tasks (400 wide + 20
// chains x depth 5 + sink), 30,060 source files.
func benchScale() benchgen.Params { return benchgen.DefaultParams() }

var benchRepoOnce = sync.OnceValues(func() (*benchgen.Repo, error) {
	root, err := os.MkdirTemp("", "sloff-bench-*")
	if err != nil {
		return nil, err
	}
	repo, err := benchgen.Generate(root, benchScale())
	if err != nil {
		return nil, err
	}
	// Bootstrap once from the clean tree so cross-task outputs exist and all
	// later runs (including "cold" ones) see the steady-state input sets —
	// sloff's model has generated outputs committed to git, so a real cold
	// run is "records absent, outputs present".
	if _, err := benchRunOnce(repo, "", noopTracerProvider()); err != nil {
		return nil, err
	}
	return repo, nil
})

func benchRepo(b *testing.B) *benchgen.Repo {
	b.Helper()
	repo, err := benchRepoOnce()
	if err != nil {
		b.Fatalf("generate bench repo: %v", err)
	}
	return repo
}

func noopTracerProvider() trace.TracerProvider { return nil }

// benchRunOnce performs one full production-shaped run: discovery included,
// script resolver only (the synthetic repo's single tool), local storage.
// It returns RUN/SKIP counts so scenarios can assert they measured the state
// they claim to measure.
func benchRunOnce(repo *benchgen.Repo, fileHashCachePath string, tp trace.TracerProvider) (runSkipCounts, error) {
	specs, err := spec.Discover(repo.Root, "**/sloff.yml")
	if err != nil {
		return runSkipCounts{}, fmt.Errorf("discover: %w", err)
	}
	reg := toolresolver.NewRegistry()
	reg.Register(script.New(repo.Root))
	logs := &benchCountLogger{}
	r := runner.New(runner.Options{
		RepoRoot:          repo.Root,
		Specs:             specs,
		Storage:           local.New(repo.Root),
		Resolvers:         reg,
		Preflight:         preflight.NewRegistry(),
		FileHashCachePath: fileHashCachePath,
		Logger:            logs,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		TracerProvider:    tp,
	})
	if err := r.Run(context.Background()); err != nil {
		return runSkipCounts{}, err
	}
	return logs.counts(), nil
}

type runSkipCounts struct{ runs, skips int }

type benchCountLogger struct {
	runs  atomic.Int64
	skips atomic.Int64
}

func (l *benchCountLogger) Infof(format string, _ ...any) {
	switch {
	case strings.HasPrefix(format, "RUN"):
		l.runs.Add(1)
	case strings.HasPrefix(format, "SKIP"):
		l.skips.Add(1)
	}
}

func (l *benchCountLogger) Warnf(string, ...any)  {}
func (l *benchCountLogger) Errorf(string, ...any) {}

func (l *benchCountLogger) counts() runSkipCounts {
	return runSkipCounts{runs: int(l.runs.Load()), skips: int(l.skips.Load())}
}

// phaseCollector accumulates the same span durations SLOFF_DEBUG_TIMING
// (ADR-0018, internal/sloff/timing) renders, so the per-phase benchmark
// metrics stay comparable to the breakdown the maintainer reads in
// production. Setup phases are wall durations; per-task spans are summed
// across tasks.
type phaseCollector struct {
	mu     sync.Mutex
	totals map[string]time.Duration
}

func newPhaseCollector() *phaseCollector {
	return &phaseCollector{totals: map[string]time.Duration{}}
}

// benchPhaseMetrics maps span names to the benchmark metric units they are
// reported under. The units end in -ms/op so the CI gate classifies them as
// timing metrics.
var benchPhaseMetrics = map[string]string{
	"runner.resolve":              "resolve-ms/op",
	"runner.collect_tasks":        "collect-ms/op",
	"runner.fingerprint.prefetch": "prefetch-ms/op",
	"runner.tasks.run":            "tasksrun-ms/op",
	"runner.task.hash_inputs":     "hashinputs-ms/op",
	"runner.task.exec":            "taskexec-ms/op",
	"runner.fingerprint.load":     "fpload-ms/op",
}

func (c *phaseCollector) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (c *phaseCollector) OnEnd(s sdktrace.ReadOnlySpan) {
	if _, ok := benchPhaseMetrics[s.Name()]; !ok {
		return
	}
	d := s.EndTime().Sub(s.StartTime())
	c.mu.Lock()
	c.totals[s.Name()] += d
	c.mu.Unlock()
}

func (c *phaseCollector) Shutdown(context.Context) error   { return nil }
func (c *phaseCollector) ForceFlush(context.Context) error { return nil }

func (c *phaseCollector) report(b *testing.B, discoverTotal time.Duration) {
	b.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	perOp := func(d time.Duration) float64 { return float64(d.Milliseconds()) / float64(b.N) }
	b.ReportMetric(perOp(discoverTotal), "discover-ms/op")
	for span, unit := range benchPhaseMetrics {
		b.ReportMetric(perOp(c.totals[span]), unit)
	}
}

// benchMutationSeq makes every warm-incremental mutation unique within the
// process so each measured run really re-executes the chain instead of
// re-hitting a record from a previous iteration.
var benchMutationSeq atomic.Int64

func mutateChainHead(b *testing.B, repo *benchgen.Repo) {
	b.Helper()
	content := fmt.Sprintf("mutation %d\n", benchMutationSeq.Add(1))
	if err := os.WriteFile(filepath.Join(repo.Root, filepath.FromSlash(repo.MutableInput)), []byte(content), 0o644); err != nil {
		b.Fatalf("mutate %s: %v", repo.MutableInput, err)
	}
}

func wipeFingerprints(b *testing.B, repo *benchgen.Repo) {
	b.Helper()
	if err := os.RemoveAll(filepath.Join(repo.Root, ".sloff")); err != nil {
		b.Fatalf("wipe fingerprints: %v", err)
	}
}

// converge re-establishes the steady state (records present and keyed off the
// full input sets) regardless of what the previous scenario left behind.
func converge(b *testing.B, repo *benchgen.Repo) {
	b.Helper()
	if _, err := benchRunOnce(repo, "", noopTracerProvider()); err != nil {
		b.Fatalf("converge run: %v", err)
	}
}

// warmPersistentStore seeds the ADR-0014 store after the racy-guard window
// has passed, then sanity-checks the store actually holds entries — a
// silently-empty store would turn the persist scenarios into memory-mode
// measurements without failing anything.
func warmPersistentStore(b *testing.B, repo *benchgen.Repo, path string) {
	b.Helper()
	time.Sleep(benchSettleDelay)
	if _, err := benchRunOnce(repo, path, noopTracerProvider()); err != nil {
		b.Fatalf("warm persistent store: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		b.Fatalf("persistent store missing after warm run: %v", err)
	}
	// 30k entries encode to megabytes; anything this small means the racy
	// guard dropped the tree and the benchmark would lie.
	if st.Size() < 100*1024 {
		b.Fatalf("persistent store suspiciously small (%d bytes); racy-guard settle failed?", st.Size())
	}
}

func BenchmarkRun(b *testing.B) {
	repo := benchRepo(b)
	p := benchScale()
	incrementalRuns := p.ChainDepth + 1 // mutated chain + sink

	scenario := func(prepare func(b *testing.B) string, perIter func(b *testing.B), want func(runSkipCounts) error) func(*testing.B) {
		return func(b *testing.B) {
			fhPath := prepare(b)
			collector := newPhaseCollector()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(collector))
			defer func() { _ = tp.Shutdown(context.Background()) }()
			var discoverTotal time.Duration

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if perIter != nil {
					b.StopTimer()
					perIter(b)
					b.StartTimer()
				}
				start := time.Now()
				specs, err := spec.Discover(repo.Root, "**/sloff.yml")
				discoverTotal += time.Since(start)
				if err != nil {
					b.Fatalf("discover: %v", err)
				}
				_ = specs
				counts, err := benchRunOnce2(repo, fhPath, tp, specs)
				if err != nil {
					b.Fatalf("run: %v", err)
				}
				b.StopTimer()
				if err := want(counts); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.StopTimer()
			collector.report(b, discoverTotal)
		}
	}

	wantAllRun := func(c runSkipCounts) error {
		if c.runs != repo.TaskCount || c.skips != 0 {
			return fmt.Errorf("cold iteration: RUN=%d SKIP=%d, want RUN=%d SKIP=0", c.runs, c.skips, repo.TaskCount)
		}
		return nil
	}
	wantAllSkip := func(c runSkipCounts) error {
		if c.skips != repo.TaskCount || c.runs != 0 {
			return fmt.Errorf("full-hit iteration: RUN=%d SKIP=%d, want RUN=0 SKIP=%d", c.runs, c.skips, repo.TaskCount)
		}
		return nil
	}
	wantIncremental := func(c runSkipCounts) error {
		if c.runs != incrementalRuns || c.skips != repo.TaskCount-incrementalRuns {
			return fmt.Errorf("warm-incremental iteration: RUN=%d SKIP=%d, want RUN=%d SKIP=%d",
				c.runs, c.skips, incrementalRuns, repo.TaskCount-incrementalRuns)
		}
		return nil
	}

	b.Run("scenario=cold", scenario(
		func(b *testing.B) string { converge(b, repo); return "" },
		func(b *testing.B) { wipeFingerprints(b, repo) },
		wantAllRun,
	))

	b.Run("scenario=warm-incremental/filehash=memory", scenario(
		func(b *testing.B) string { converge(b, repo); mutateChainHead(b, repo); return "" },
		func(b *testing.B) { mutateChainHead(b, repo) },
		wantIncremental,
	))

	b.Run("scenario=warm-incremental/filehash=persist", scenario(
		func(b *testing.B) string {
			converge(b, repo)
			path := filepath.Join(b.TempDir(), "filehashes.pb")
			warmPersistentStore(b, repo, path)
			mutateChainHead(b, repo)
			return path
		},
		func(b *testing.B) { mutateChainHead(b, repo) },
		wantIncremental,
	))

	b.Run("scenario=full-hit/filehash=memory", scenario(
		func(b *testing.B) string { converge(b, repo); return "" },
		nil,
		wantAllSkip,
	))

	b.Run("scenario=full-hit/filehash=persist", scenario(
		func(b *testing.B) string {
			converge(b, repo)
			path := filepath.Join(b.TempDir(), "filehashes.pb")
			warmPersistentStore(b, repo, path)
			return path
		},
		nil,
		wantAllSkip,
	))
}

// benchRunOnce2 is benchRunOnce with discovery hoisted out (the caller times
// it separately so the discover phase is reported on its own).
func benchRunOnce2(repo *benchgen.Repo, fileHashCachePath string, tp trace.TracerProvider, specs []spec.Spec) (runSkipCounts, error) {
	reg := toolresolver.NewRegistry()
	reg.Register(script.New(repo.Root))
	logs := &benchCountLogger{}
	r := runner.New(runner.Options{
		RepoRoot:          repo.Root,
		Specs:             specs,
		Storage:           local.New(repo.Root),
		Resolvers:         reg,
		Preflight:         preflight.NewRegistry(),
		FileHashCachePath: fileHashCachePath,
		Logger:            logs,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		TracerProvider:    tp,
	})
	if err := r.Run(context.Background()); err != nil {
		return runSkipCounts{}, err
	}
	return logs.counts(), nil
}
