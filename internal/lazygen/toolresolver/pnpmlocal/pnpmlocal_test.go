package pnpmlocal_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/pnpmlocal"
)

// fakeLister returns a fixed Listing per "<specDir>\x00<entry>" key. Pnpm-
// local tests use it to keep the resolver decoupled from esbuild during unit
// testing.
type fakeLister struct {
	calls    []fakeListerCall
	listings map[string]lister.Listing
	err      error
}

type fakeListerCall struct {
	specDir string
	entry   string
}

func (f *fakeLister) List(_ context.Context, specDir, entry string) (lister.Listing, error) {
	f.calls = append(f.calls, fakeListerCall{specDir: specDir, entry: entry})
	if f.err != nil {
		return lister.Listing{}, f.err
	}
	if l, ok := f.listings[specDir+"\x00"+entry]; ok {
		return l, nil
	}
	return lister.Listing{}, nil
}

func TestResolver_Name(t *testing.T) {
	root := setupWorkspace(t, nil, nil, "")
	r := mustNewResolver(t, root, &fakeLister{})
	if r.Name() != "pnpm-local" {
		t.Errorf("Name() = %q, want pnpm-local", r.Name())
	}
}

func TestResolver_RejectsNilDeclaration(t *testing.T) {
	root := setupWorkspace(t, nil, nil, "")
	r := mustNewResolver(t, root, &fakeLister{})
	if _, err := r.Resolve(context.Background(), ".", nil, nil); err == nil {
		t.Fatal("expected error when declared is nil (ADR-0005)")
	}
}

func TestResolver_RejectsEmptyPackageName(t *testing.T) {
	root := setupWorkspace(t, nil, nil, "")
	r := mustNewResolver(t, root, &fakeLister{})
	_, err := r.Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local"})
	if err == nil {
		t.Fatal("expected error when PackageName is empty")
	}
}

// TestResolver_RejectsNonWorkspacePackage guards the ADR-0007 boundary: a
// pnpm-local declaration referring to an external npm package (not registered
// as a workspace member) must fail clearly so the user knows to switch to the
// script resolver instead of getting a silent wrong-version cache.
func TestResolver_RejectsNonWorkspacePackage(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`}},
		nil,
		"",
	)
	r := mustNewResolver(t, root, &fakeLister{})

	_, err := r.Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@external/foo"})
	if err == nil {
		t.Fatal("expected error for non-workspace package")
	}
	if !errors.Is(err, pnpmlocal.ErrNotWorkspacePackage) {
		t.Errorf("error %v should wrap ErrNotWorkspacePackage", err)
	}
}

// TestResolver_ContributesWorkspaceFilesAsExtraInputs is the happy path for
// the input-contributor side of the resolver: the lister's InternalFiles
// surface verbatim in Result.ExtraInputs so the runner can fold them into
// the task's input set and depgraph picks up the upstream build task.
func TestResolver_ContributesWorkspaceFilesAsExtraInputs(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`}},
		map[string]string{"packages/codegen/dist/cli.js": "console.log('x');\n"},
		"",
	)

	stub := &fakeLister{listings: map[string]lister.Listing{
		"\x00./packages/codegen/dist/cli.js": {
			InternalFiles: []string{"packages/codegen/dist/cli.js", "packages/codegen/dist/lib.js"},
		},
	}}
	r := mustNewResolver(t, root, stub)

	got, err := r.Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"packages/codegen/dist/cli.js", "packages/codegen/dist/lib.js"}
	if diff := cmp.Diff(want, got.ExtraInputs); diff != "" {
		t.Errorf("ExtraInputs mismatch (-want +got):\n%s", diff)
	}
}

// TestResolver_FallsBackToBinPathWhenMissing exercises the fresh-checkout
// branch: when the bin file doesn't yet exist on disk (typical when an
// upstream build task hasn't run), the resolver still surfaces the bin path
// as an ExtraInput so depgraph can wire the build task by output overlap.
// Skipping this would leave depgraph blind to the dependency on first run.
func TestResolver_FallsBackToBinPathWhenMissing(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`}},
		nil, // dist/cli.js intentionally absent
		"",
	)
	stub := &fakeLister{}
	r := mustNewResolver(t, root, stub)

	got, err := r.Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if diff := cmp.Diff([]string{"packages/codegen/dist/cli.js"}, got.ExtraInputs); diff != "" {
		t.Errorf("ExtraInputs (-want +got):\n%s", diff)
	}
	if len(stub.calls) != 0 {
		t.Errorf("lister should be skipped when bin is missing, got %d calls", len(stub.calls))
	}
}

// TestResolver_EmitsTransitiveExternalsAsToolVersions is the happy path for
// the tools_hash side: every external dep reachable from the workspace
// package via the lockfile graph surfaces as a `pnpm-deps:<pkg>@<ver>`
// version string. Without this, runtime-resolved npm bumps would not flip
// tools_hash and stale outputs could leak through the cache.
func TestResolver_EmitsTransitiveExternalsAsToolVersions(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`}},
		nil,
		`lockfileVersion: '9.0'
importers:
  packages/codegen:
    dependencies:
      lodash:
        specifier: ^4.17.0
        version: 4.17.21
snapshots:
  lodash@4.17.21:
    dependencies:
      some-helper: 1.2.3
  some-helper@1.2.3: {}
`,
	)
	stub := &fakeLister{listings: map[string]lister.Listing{
		"\x00./packages/codegen/dist/cli.js": {InternalFiles: []string{"packages/codegen/dist/cli.js"}},
	}}

	got, err := mustNewResolver(t, root, stub).Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	versionStrs := make([]string, len(got.Versions))
	for i, v := range got.Versions {
		versionStrs[i] = v.Version
	}
	sort.Strings(versionStrs)
	want := []string{
		"pnpm-deps:lodash@4.17.21",
		"pnpm-deps:some-helper@1.2.3",
	}
	if diff := cmp.Diff(want, versionStrs); diff != "" {
		t.Errorf("Versions (-want +got):\n%s", diff)
	}
}

// TestResolver_MultipleBinEntriesUnioned guards the multi-entry case: when
// package.json declares two bins, both contribute files as ExtraInputs.
// Without this, edits to one bin's transitive deps would not invalidate the
// cache via the inputs path.
func TestResolver_MultipleBinEntriesUnioned(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{
			path:    "packages/codegen",
			pkgJSON: `{"name": "@org/codegen", "bin": {"a": "dist/a.js", "b": "dist/b.js"}}`,
		}},
		map[string]string{
			"packages/codegen/dist/a.js": "1\n",
			"packages/codegen/dist/b.js": "1\n",
		},
		"",
	)
	stub := &fakeLister{listings: map[string]lister.Listing{
		"\x00./packages/codegen/dist/a.js": {InternalFiles: []string{"packages/codegen/dist/a.js"}},
		"\x00./packages/codegen/dist/b.js": {InternalFiles: []string{"packages/codegen/dist/b.js"}},
	}}

	got, err := mustNewResolver(t, root, stub).Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(stub.calls) != 2 {
		t.Errorf("expected 2 lister calls (one per bin), got %d: %+v", len(stub.calls), stub.calls)
	}
	want := []string{"packages/codegen/dist/a.js", "packages/codegen/dist/b.js"}
	if diff := cmp.Diff(want, got.ExtraInputs); diff != "" {
		t.Errorf("ExtraInputs (-want +got):\n%s", diff)
	}
}

// TestResolver_FailsOnPackageWithoutEntryPoints catches misconfigured workspace
// packages that declare neither bin nor main. Without the fail, the resolver
// would silently emit an empty contribution and downstream invalidation
// signals would be lost.
func TestResolver_FailsOnPackageWithoutEntryPoints(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen"}`}},
		nil,
		"",
	)
	r := mustNewResolver(t, root, &fakeLister{})

	_, err := r.Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err == nil {
		t.Fatal("expected error for package without bin/main")
	}
}

// TestResolver_PassesThroughListerError surfaces lister failures rather than
// silently producing an empty hash that could mask a broken esbuild call.
func TestResolver_PassesThroughListerError(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`}},
		map[string]string{"packages/codegen/dist/cli.js": "x"},
		"",
	)
	stub := &fakeLister{err: errors.New("esbuild boom")}

	_, err := mustNewResolver(t, root, stub).Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err == nil || !strings.Contains(err.Error(), "esbuild boom") {
		t.Errorf("err = %v, want wrap of lister error", err)
	}
}

// importerSpec describes a workspace package fixture: where it lives and what
// its package.json should contain.
type importerSpec struct {
	path    string
	pkgJSON string
}

// setupWorkspace materialises a temp repo with a pnpm-lock.yaml and the
// corresponding package.json files. lockYaml, when non-empty, replaces the
// auto-generated importer-only lockfile (used by tests that need snapshots).
func setupWorkspace(t *testing.T, importers []importerSpec, extra map[string]string, lockYaml string) string {
	t.Helper()
	root := t.TempDir()

	if lockYaml == "" {
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
		lockYaml = b.String()
	}
	mustWriteFile(t, filepath.Join(root, "pnpm-lock.yaml"), lockYaml)

	for _, imp := range importers {
		mustWriteFile(t, filepath.Join(root, filepath.FromSlash(imp.path), "package.json"), imp.pkgJSON)
	}
	for rel, contents := range extra {
		mustWriteFile(t, filepath.Join(root, filepath.FromSlash(rel)), contents)
	}
	return root
}

func mustWriteFile(t *testing.T, full, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustNewResolver(t *testing.T, root string, l lister.SourceLister) *pnpmlocal.Resolver {
	t.Helper()
	r, err := pnpmlocal.New(root, l)
	if err != nil {
		t.Fatalf("pnpmlocal.New: %v", err)
	}
	return r
}
