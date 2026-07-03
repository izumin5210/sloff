package runner_test

// Tests for ADR-0019 D5 follow-up: batch fingerprint reload for deferred-tool
// consumers after the tool resolves.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

// countingStorage wraps a fingerprint.Storage and counts Load and LoadMany
// calls so tests can assert on the number of round-trips to the backend.
type countingStorage struct {
	inner         fingerprint.Storage
	loadCalls     atomic.Int64
	loadManyCalls atomic.Int64
}

func (s *countingStorage) Name() string { return s.inner.Name() }

func (s *countingStorage) Load(ctx context.Context, key fingerprint.Key) (*fingerprintv1.Record, bool, error) {
	s.loadCalls.Add(1)
	return s.inner.Load(ctx, key)
}

func (s *countingStorage) Save(ctx context.Context, key fingerprint.Key, record *fingerprintv1.Record) error {
	return s.inner.Save(ctx, key, record)
}

func (s *countingStorage) Delete(ctx context.Context, key fingerprint.Key) error {
	return s.inner.Delete(ctx, key)
}

func (s *countingStorage) List(ctx context.Context, filter fingerprint.ListFilter) ([]fingerprint.Key, error) {
	return s.inner.List(ctx, filter)
}

func (s *countingStorage) CollapseDuplicates(ctx context.Context) (int, error) {
	return s.inner.CollapseDuplicates(ctx)
}

func (s *countingStorage) LoadMany(ctx context.Context, keys []fingerprint.Key) (map[fingerprint.Key]*fingerprintv1.Record, error) {
	s.loadManyCalls.Add(1)
	return s.inner.LoadMany(ctx, keys)
}

func (s *countingStorage) SaveMany(ctx context.Context, items []fingerprint.KeyRecord) error {
	return s.inner.SaveMany(ctx, items)
}

// deferThenSucceedResolver is a pnpm-local stub that fails the first (eager)
// Inputs call and succeeds on the retry (deferred). This simulates the
// cold-bootstrap scenario where the tool source is not yet generated at run
// start but exists by the time the consumer task executes.
type deferThenSucceedResolver struct {
	calls atomic.Int32
}

func (s *deferThenSucceedResolver) Name() string { return "pnpm-local" }

func (s *deferThenSucceedResolver) Inputs(_ context.Context, _ string, _ *toolresolver.DeclaredTool) ([]string, error) {
	n := s.calls.Add(1)
	if n == 1 {
		return nil, fmt.Errorf("tool source not generated yet (eager, call #%d)", n)
	}
	// Retry succeeds with no extra inputs — the tool contributes only its version.
	return nil, nil
}

func (s *deferThenSucceedResolver) Versions(_ context.Context, _ string, _ *toolresolver.DeclaredTool) ([]toolresolver.ResolvedVersion, error) {
	return []toolresolver.ResolvedVersion{{Name: "codegen", Source: "stub:codegen", Version: "stub:codegen@1.0.0"}}, nil
}

// TestRunner_ToolDepends_BatchReloadAfterResolve verifies ADR-0019 D5 follow-up:
// after a deferred tool resolves successfully, consumer tasks that were
// excluded from the initial prefetch are batch-loaded (exactly one extra
// LoadMany call) rather than falling back to individual Storage.Load calls.
//
// Scenario: one deferred pnpm-local tool with depends on a producer task;
// two consumer tasks. Records are pre-seeded for both consumers so a hit is
// possible. After the run:
//   - Both consumers must SKIP (marker file not written by their cmds).
//   - Storage.Load must have been called 0 times for the consumers (served by
//     the batch reload, not individual live-Loads).
//   - Exactly one extra LoadMany must have fired (the post-resolve batch).
func TestRunner_ToolDepends_BatchReloadAfterResolve(t *testing.T) {
	requireSh(t)

	workdir := t.TempDir()
	write := func(rel, contents string) {
		t.Helper()
		full := filepath.Join(workdir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("input.txt", "hello\n")
	// producer: generates src/gen.txt and is listed in the tool's depends.
	// consumer1 / consumer2: use the deferred tool; their markers prove SKIP.
	write("sloff.yml", `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  codegen:
    pnpm-local: "@org/codegen"
    depends:
      - task: producer

commands:
  - name: producer
    cmd: ["sh", "-c", "mkdir -p src && echo generated > src/gen.txt"]
    inputs: ["input.txt"]
    outputs: ["src/gen.txt"]
    tools: [versioner]
  - name: consumer1
    cmd: ["sh", "-c", "printf x >> marker1.txt; cp input.txt out1.txt"]
    inputs: ["input.txt"]
    outputs: ["out1.txt"]
    tools: [codegen]
  - name: consumer2
    cmd: ["sh", "-c", "printf x >> marker2.txt; cp input.txt out2.txt"]
    inputs: ["input.txt"]
    outputs: ["out2.txt"]
    tools: [codegen]
`)

	specs, err := spec.Discover(workdir, "**/sloff.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	inner := local.New(workdir, local.WithClock(func() time.Time { return fixedClock }))
	store := &countingStorage{inner: inner}

	stub := &deferThenSucceedResolver{}
	reg := toolresolver.NewRegistry()
	reg.Register(script.New(workdir))
	reg.Register(stub)

	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   store,
		Resolvers: reg,
		Preflight: preflight.NewRegistry(),
	})

	// Cold run: producer runs, deferred tool resolves, consumers run and write records.
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("cold Run: %v", err)
	}
	// Reset counters: we only want to measure the warm run below.
	store.loadCalls.Store(0)
	store.loadManyCalls.Store(0)
	stub.calls.Store(0) // reset so the stub fails again on eager (warm run w/ deferred)

	// On the warm run the deferred tool will be demoted again (stub fails eagerly),
	// but after resolving, the consumer records exist in the store. We want to see
	// that the batch reload (one extra LoadMany) serves both consumers without any
	// individual Load call for them.
	//
	// Create a fresh runner to simulate a new process (no in-memory state).
	r2 := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   store,
		Resolvers: reg,
		Preflight: preflight.NewRegistry(),
	})
	if err := r2.Run(context.Background()); err != nil {
		t.Fatalf("warm Run: %v", err)
	}

	// Both consumers must SKIP: marker files must not have been touched.
	for _, m := range []string{"marker1.txt", "marker2.txt"} {
		data, err := os.ReadFile(filepath.Join(workdir, m))
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		// Cold run wrote one 'x' each; warm run must not have re-run them.
		if string(data) != "x" {
			t.Errorf("%s = %q, want %q (consumer re-executed on warm run)", m, string(data), "x")
		}
	}

	// The batch reload (reloadDeferredConsumers) must have fired exactly once for
	// the two consumers. That means exactly one extra LoadMany call beyond the
	// initial prefetch LoadMany (which covers producer only). Total = 2.
	// Individual Load calls for consumer tasks must be 0.
	loadCalls := store.loadCalls.Load()
	loadManyCalls := store.loadManyCalls.Load()
	t.Logf("warm run: Load=%d LoadMany=%d", loadCalls, loadManyCalls)
	if loadCalls != 0 {
		t.Errorf("warm run: Storage.Load called %d times, want 0 (consumers should be served by batch reload)", loadCalls)
	}
	// LoadMany: 1 for initial prefetch (producer only) + 1 for the deferred reload.
	if loadManyCalls != 2 {
		t.Errorf("warm run: Storage.LoadMany called %d times, want 2 (initial + reload)", loadManyCalls)
	}
}
