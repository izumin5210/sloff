package lister_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

// TestEsbuildLister_SingleEntryNoImports guards the simplest path: an entry
// with no imports yields exactly that one file in InternalFiles. Without this
// the resolver would silently produce an empty listing for trivial generators.
func TestEsbuildLister_SingleEntryNoImports(t *testing.T) {
	root := setupTSRepo(t, map[string]string{
		"packages/gen/dist/cli.js": "console.log('hi');\n",
	})

	got, err := lister.NewEsbuild(root).List(context.Background(), "", "./packages/gen/dist/cli.js")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if diff := cmp.Diff([]string{"packages/gen/dist/cli.js"}, got.InternalFiles); diff != "" {
		t.Errorf("InternalFiles (-want +got):\n%s", diff)
	}
	if len(got.ExternalModules) != 0 {
		t.Errorf("ExternalModules must be empty for esbuild lister, got %v", got.ExternalModules)
	}
}

// TestEsbuildLister_RelativeImportIncluded ensures the lister walks transitive
// imports via esbuild's bundler, not just the root entry. Hash determinism for
// pnpm-local hinges on this.
func TestEsbuildLister_RelativeImportIncluded(t *testing.T) {
	root := setupTSRepo(t, map[string]string{
		"packages/gen/dist/cli.js":    "import { run } from './lib.js';\nrun();\n",
		"packages/gen/dist/lib.js":    "export function run() { console.log('x'); }\n",
		"packages/gen/dist/unused.js": "export const ignored = 1;\n",
	})

	got, err := lister.NewEsbuild(root).List(context.Background(), "", "./packages/gen/dist/cli.js")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Contains(got.InternalFiles, "packages/gen/dist/cli.js") {
		t.Errorf("entry not in InternalFiles: %v", got.InternalFiles)
	}
	if !slices.Contains(got.InternalFiles, "packages/gen/dist/lib.js") {
		t.Errorf("relative import not in InternalFiles: %v", got.InternalFiles)
	}
	if slices.Contains(got.InternalFiles, "packages/gen/dist/unused.js") {
		t.Errorf("unused file leaked into InternalFiles: %v", got.InternalFiles)
	}
}

// TestEsbuildLister_NodeModulesExcluded guards the ADR-0007 boundary: external
// npm packages reachable through node_modules must not contribute to the
// pnpm-local hash. Their version is the script resolver's responsibility, and
// hashing them here would double-count and re-introduce $GOMODCACHE-style
// portability problems (per-developer/per-CI install paths).
func TestEsbuildLister_NodeModulesExcluded(t *testing.T) {
	root := setupTSRepo(t, map[string]string{
		"packages/gen/dist/cli.js":           "import 'some-dep';\nconsole.log('x');\n",
		"node_modules/some-dep/package.json": `{"name":"some-dep","main":"index.js"}`,
		"node_modules/some-dep/index.js":     "module.exports = 1;\n",
	})

	got, err := lister.NewEsbuild(root).List(context.Background(), "", "./packages/gen/dist/cli.js")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, f := range got.InternalFiles {
		if strings.Contains(filepath.ToSlash(f), "node_modules/") {
			t.Errorf("node_modules path leaked into InternalFiles: %s", f)
		}
	}
}

// TestEsbuildLister_DeterministicSort guards that two invocations yield the
// same sorted listing — without this, hash output flips between runs on the
// same filesystem and cache hits become unreliable.
func TestEsbuildLister_DeterministicSort(t *testing.T) {
	root := setupTSRepo(t, map[string]string{
		"packages/gen/dist/cli.js": "import './c.js';\nimport './a.js';\nimport './b.js';\n",
		"packages/gen/dist/a.js":   "export const a = 1;\n",
		"packages/gen/dist/b.js":   "export const b = 1;\n",
		"packages/gen/dist/c.js":   "export const c = 1;\n",
	})

	l := lister.NewEsbuild(root)
	first, err := l.List(context.Background(), "", "./packages/gen/dist/cli.js")
	if err != nil {
		t.Fatalf("List #1: %v", err)
	}
	second, err := l.List(context.Background(), "", "./packages/gen/dist/cli.js")
	if err != nil {
		t.Fatalf("List #2: %v", err)
	}
	if diff := cmp.Diff(first.InternalFiles, second.InternalFiles); diff != "" {
		t.Errorf("non-deterministic InternalFiles (-first +second):\n%s", diff)
	}
	if !slices.IsSorted(first.InternalFiles) {
		t.Errorf("InternalFiles must be sorted ascending, got %v", first.InternalFiles)
	}
}

// TestEsbuildLister_FailsOnMissingEntry ensures parse / resolution errors are
// surfaced as Go errors rather than being swallowed into an empty listing
// (which would silently break the cache).
func TestEsbuildLister_FailsOnMissingEntry(t *testing.T) {
	root := t.TempDir()
	_, err := lister.NewEsbuild(root).List(context.Background(), "", "./packages/gen/dist/missing.js")
	if err == nil {
		t.Fatal("expected error when entry file does not exist")
	}
}

func setupTSRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, contents := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
