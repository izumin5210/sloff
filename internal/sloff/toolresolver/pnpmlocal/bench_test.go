package pnpmlocal_test

// Regression guard for the PR #49 enumerator batching. The guarded quantity
// is NOT CPU throughput but the NUMBER of FileEnumerator invocations:
// collectFiles must pass ALL of a tool's transitively-linked workspace dirs
// to ONE variadic enumerator call, so the production GitLsFiles spawns a
// single `git ls-files -- d1 d2 ...` subprocess regardless of workspace
// fan-out. Pre-#49 it spawned one subprocess per dir. The benchmark reports
// the invocation count as a deterministic custom metric (b.ReportMetric) for
// the CI gate, and TestBatchedEnumeratorCallCount asserts it exactly so the
// guard also fires in the normal -race test job.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
)

// benchLinkedLibCount is the workspace fan-out of the fixture tool: enough
// linked packages that the pre-#49 cost model (1 + 12 = 13 subprocess spawns)
// is unmistakably different from the post-#49 one (1).
const benchLinkedLibCount = 12

// benchWorkspace materialises the fixture ONCE per process: a repo whose
// pnpm-lock.yaml declares the tool package packages/codegen star-linking
// benchLinkedLibCount library packages via workspace:* / link: deps, so
// WalkDeps discovers 13 workspace dirs. Built once because the fixture is
// immutable and per-iteration disk writes would swamp the resolve cost the
// benchmark measures. The YAML mirrors setupWorkspace's shape.
var benchWorkspace = sync.OnceValues(func() (string, error) {
	root, err := os.MkdirTemp("", "sloff-pnpmlocal-bench-*")
	if err != nil {
		return "", err
	}

	var lock strings.Builder
	lock.WriteString("lockfileVersion: '9.0'\nimporters:\n  packages/codegen:\n    dependencies:\n")
	for i := range benchLinkedLibCount {
		fmt.Fprintf(&lock, "      '@org/lib%02d':\n        specifier: workspace:*\n        version: link:../lib%02d\n", i, i)
	}
	for i := range benchLinkedLibCount {
		fmt.Fprintf(&lock, "  packages/lib%02d: {}\n", i)
	}

	files := map[string]string{
		"pnpm-lock.yaml":                lock.String(),
		"packages/codegen/package.json": `{"name": "@org/codegen"}`,
	}
	for i := range benchLinkedLibCount {
		files[fmt.Sprintf("packages/lib%02d/package.json", i)] = fmt.Sprintf(`{"name": "@org/lib%02d"}`, i)
	}
	for rel, contents := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			return "", err
		}
	}
	return root, nil
})

// benchWorkspaceDirs returns the workspace-dir set WalkDeps discovers for
// @org/codegen — the tool's own dir plus every linked lib — in the OS-native
// form collectFiles hands to the enumerator.
func benchWorkspaceDirs() []string {
	dirs := []string{filepath.FromSlash("packages/codegen")}
	for i := range benchLinkedLibCount {
		dirs = append(dirs, filepath.FromSlash(fmt.Sprintf("packages/lib%02d", i)))
	}
	return dirs
}

// countingEnumerator is a FileEnumerator that records each invocation's
// pkgDirs, standing in for the production GitLsFiles where every invocation
// is one `git ls-files` subprocess spawn — the expensive unit this guard
// counts. Mutex-protected so the guard stays valid (and race-clean) even if
// the resolver ever enumerates concurrently.
type countingEnumerator struct {
	mu    sync.Mutex
	calls [][]string
}

func (c *countingEnumerator) enumerate(_ context.Context, _ string, pkgDirs ...string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, append([]string(nil), pkgDirs...))
	out := make([]string, 0, len(pkgDirs)*2)
	for _, d := range pkgDirs {
		rel := filepath.ToSlash(d)
		out = append(out, rel+"/package.json", rel+"/src/index.ts")
	}
	return out, nil
}

func (c *countingEnumerator) snapshot() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]string, len(c.calls))
	copy(out, c.calls)
	return out
}

func BenchmarkResolver(b *testing.B) {
	b.Run("path=inputs", func(b *testing.B) {
		root, err := benchWorkspace()
		if err != nil {
			b.Fatalf("build workspace fixture: %v", err)
		}
		ctx := context.Background()
		declared := &toolresolver.DeclaredTool{Resolver: pnpmlocal.Name, PackageName: "@org/codegen"}
		b.ReportAllocs()
		var enumCalls int
		for b.Loop() {
			// Fresh resolver per iteration: the resolver memoises per
			// PackageName, so reuse would measure cache hits and hide an
			// enumerator regression. Freshness also keeps the real
			// per-resolve work — lockfile parse + workspace walk — inside
			// the measurement, which is pnpm-local's actual per-run overhead.
			enum := &countingEnumerator{}
			r, err := pnpmlocal.New(root, enum.enumerate)
			if err != nil {
				b.Fatalf("New: %v", err)
			}
			if _, err := r.Inputs(ctx, "", declared); err != nil {
				b.Fatalf("Inputs: %v", err)
			}
			enumCalls = len(enum.snapshot())
		}
		// The count is deterministic (identical every iteration), so the last
		// iteration's counter IS the per-op value. The CI gate fails on ANY
		// increase: enumcalls/op above 1 means collectFiles no longer batches
		// all workspace dirs into one enumerator call (#49) and production
		// would spawn one `git ls-files` per dir again.
		b.ReportMetric(float64(enumCalls), "enumcalls/op")
	})
}

// TestBatchedEnumeratorCallCount is the same guard as the benchmark metric,
// expressed as a plain test so it also fires in the ordinary -race test job
// (benchmarks don't run there).
func TestBatchedEnumeratorCallCount(t *testing.T) {
	root, err := benchWorkspace()
	if err != nil {
		t.Fatalf("build workspace fixture: %v", err)
	}
	enum := &countingEnumerator{}
	r, err := pnpmlocal.New(root, enum.enumerate)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Inputs(context.Background(), "",
		&toolresolver.DeclaredTool{Resolver: pnpmlocal.Name, PackageName: "@org/codegen"}); err != nil {
		t.Fatalf("Inputs: %v", err)
	}

	calls := enum.snapshot()
	if len(calls) != 1 {
		dirCounts := make([]int, len(calls))
		for i, c := range calls {
			dirCounts[i] = len(c)
		}
		t.Fatalf(
			"FileEnumerator invoked %d times (per-call dir counts %v), want exactly 1.\n"+
				"PR #49 batches all of a tool's transitively-linked workspace dirs into ONE variadic enumerator call, "+
				"so production spawns a single `git ls-files -- d1 d2 ...` instead of one subprocess per dir; more calls "+
				"means that batching regressed. If the design changed intentionally, update this guard and the ADR-0021 bench docs.",
			len(calls), dirCounts,
		)
	}

	// The single call must carry every dir (tool dir + all linked libs):
	// a call that batches but silently drops dirs would pass the count check
	// while shrinking the fingerprint's input set.
	got := append([]string(nil), calls[0]...)
	sort.Strings(got)
	want := benchWorkspaceDirs()
	sort.Strings(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("single enumerator call must receive all %d workspace dirs (-want +got):\n%s", len(want), diff)
	}
}
