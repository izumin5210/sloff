package pnpmlocal_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	preflightpnpm "github.com/izumin5210/lazygen/internal/lazygen/preflight/pnpmlocal"
)

func TestChecker_Name(t *testing.T) {
	root := setupWorkspace(t, nil, nil)
	c := preflightpnpm.New(root)
	if c.Name() != "pnpm-local" {
		t.Errorf("Name() = %q, want pnpm-local", c.Name())
	}
}

// TestChecker_DistMissing reports the build-required-but-not-built case. The
// resolver would still hash the src/ inputs successfully (lister doesn't care
// about dist), but the cmd would fail at runtime with `cannot find module
// dist/cli.js`. Surfacing this as a preflight Issue lets lazygen fail fast
// with a fix-it message instead of producing a confusing runtime error.
func TestChecker_DistMissing(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{
			path:    "packages/codegen",
			pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`,
		}},
		map[string]fileSpec{
			"packages/codegen/src/index.ts": {contents: "console.log('a');\n"},
		},
	)
	res, err := preflightpnpm.New(root).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.OK {
		t.Fatal("expected NOT OK when dist/ is missing")
	}
	if len(res.Issues) != 1 {
		t.Fatalf("len(Issues) = %d, want 1: %+v", len(res.Issues), res.Issues)
	}
	if !strings.Contains(res.Issues[0].Detail, "@org/codegen") {
		t.Errorf("Issue Detail should mention package name, got %q", res.Issues[0].Detail)
	}
	if !strings.Contains(res.Issues[0].Suggestion, "@org/codegen") {
		t.Errorf("Issue Suggestion should reference package, got %q", res.Issues[0].Suggestion)
	}
}

// TestChecker_DistOlderThanSrc covers the everyday "edited src, forgot to
// rebuild" case. The mtime comparison must consider the newest src file vs.
// the newest dist file so partial rebuilds (e.g. dist updated for some files
// but not others) still trip the check.
func TestChecker_DistOlderThanSrc(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{
			path:    "packages/codegen",
			pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`,
		}},
		map[string]fileSpec{
			"packages/codegen/dist/cli.js":  {contents: "compiled('v1');\n", mtime: t1},
			"packages/codegen/src/index.ts": {contents: "code('v2');\n", mtime: t2},
		},
	)
	res, err := preflightpnpm.New(root).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.OK {
		t.Fatal("expected NOT OK when dist is older than src")
	}
}

// TestChecker_DistFresh is the green path: dist/ is newer than src/ → no
// issue. Without a positive test, regressions could turn the checker into
// "always reports an issue" without anyone noticing.
func TestChecker_DistFresh(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{
			path:    "packages/codegen",
			pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`,
		}},
		map[string]fileSpec{
			"packages/codegen/src/index.ts": {contents: "code('v1');\n", mtime: t1},
			"packages/codegen/dist/cli.js":  {contents: "compiled('v2');\n", mtime: t2},
		},
	)
	res, err := preflightpnpm.New(root).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.OK || len(res.Issues) != 0 {
		t.Errorf("expected OK and no issues, got %+v", res)
	}
}

// TestChecker_SkipsPackagesWithoutDist verifies that ts-node / tsx style
// packages (bin points directly at src/...) are not falsely flagged. They have
// no build step, so dist freshness is meaningless.
func TestChecker_SkipsPackagesWithoutDist(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{
			path:    "packages/codegen",
			pkgJSON: `{"name": "@org/codegen", "bin": "src/cli.ts"}`,
		}},
		map[string]fileSpec{
			"packages/codegen/src/cli.ts": {contents: "console.log('a');\n"},
		},
	)
	res, err := preflightpnpm.New(root).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.OK {
		t.Errorf("expected OK for ts-node-style package, got %+v", res)
	}
}

// TestChecker_NoLockfileIsNoOp guards repos that don't use pnpm at all
// (Go-only). The checker is registered unconditionally by the CLI; it must
// short-circuit cleanly when pnpm-lock.yaml is absent rather than failing
// every lazygen run.
func TestChecker_NoLockfileIsNoOp(t *testing.T) {
	root := t.TempDir()
	res, err := preflightpnpm.New(root).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check (no lockfile): %v", err)
	}
	if !res.OK || len(res.Issues) != 0 {
		t.Errorf("expected silent OK without pnpm-lock.yaml, got %+v", res)
	}
}

// TestChecker_AggregatesAcrossWorkspacePackages guards the multi-package case:
// each broken package contributes a separate issue, so users see the full
// fix-it list in one go rather than fixing one and re-running.
func TestChecker_AggregatesAcrossWorkspacePackages(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{
			{path: "packages/a", pkgJSON: `{"name": "@org/a", "bin": "dist/a.js"}`},
			{path: "packages/b", pkgJSON: `{"name": "@org/b", "bin": "dist/b.js"}`},
		},
		map[string]fileSpec{
			"packages/a/src/index.ts": {contents: "x", mtime: t1},
			// no dist for a
			"packages/b/src/index.ts": {contents: "y", mtime: t2},
			// no dist for b
		},
	)
	res, err := preflightpnpm.New(root).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.OK {
		t.Fatal("expected NOT OK")
	}
	if len(res.Issues) != 2 {
		t.Errorf("expected 2 issues, got %d: %+v", len(res.Issues), res.Issues)
	}
}

// fixed timestamps so dist-vs-src comparisons are deterministic.
var (
	t1 = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
)

type importerSpec struct {
	path    string
	pkgJSON string
}

type fileSpec struct {
	contents string
	mtime    time.Time
}

func setupWorkspace(t *testing.T, importers []importerSpec, files map[string]fileSpec) string {
	t.Helper()
	root := t.TempDir()

	var b strings.Builder
	b.WriteString("lockfileVersion: '9.0'\nimporters:\n")
	for _, imp := range importers {
		b.WriteString("  ")
		b.WriteString(imp.path)
		b.WriteString(": {}\n")
	}
	if len(importers) == 0 {
		b.WriteString("  .: {}\n")
	}
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), b.String(), time.Time{})

	for _, imp := range importers {
		mustWrite(t, filepath.Join(root, filepath.FromSlash(imp.path), "package.json"), imp.pkgJSON, time.Time{})
	}
	for rel, spec := range files {
		mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), spec.contents, spec.mtime)
	}
	return root
}

func mustWrite(t *testing.T, full, contents string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}
