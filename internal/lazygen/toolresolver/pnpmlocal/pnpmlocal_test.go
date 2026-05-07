package pnpmlocal_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/pnpmlocal"
)

// fakeLister is a stub SourceLister that records the list calls and returns a
// pre-baked listing per (specDir, entry) key. Pnpm-local tests use it to keep
// the resolver decoupled from esbuild during unit testing.
type fakeLister struct {
	calls    []fakeListerCall
	listings map[string]lister.Listing // key = "<specDir>\x00<entry>"
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
	root := setupWorkspace(t, nil, nil)
	r := mustNewResolver(t, root, &fakeLister{})
	if r.Name() != "pnpm-local" {
		t.Errorf("Name() = %q, want pnpm-local", r.Name())
	}
}

func TestResolver_RejectsNilDeclaration(t *testing.T) {
	root := setupWorkspace(t, nil, nil)
	r := mustNewResolver(t, root, &fakeLister{})
	if _, err := r.Resolve(context.Background(), ".", nil, nil); err == nil {
		t.Fatal("expected error when declared is nil (ADR-0005)")
	}
}

func TestResolver_RejectsEmptyPackageName(t *testing.T) {
	root := setupWorkspace(t, nil, nil)
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
	root := setupWorkspace(t, []importerSpec{
		{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`},
	}, nil)
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

// TestResolver_HashesEntryPointTransitiveSources is the happy path: the
// declared workspace package's bin entry is forwarded to the lister, the
// returned files are hashed, and the resulting Version is the OS-neutral
// "pnpm-local:<name>@sha256:<hex>" form.
func TestResolver_HashesEntryPointTransitiveSources(t *testing.T) {
	root := setupWorkspace(t, []importerSpec{
		{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`},
	}, map[string]string{
		"packages/codegen/dist/cli.js": "console.log('a');\n",
		"packages/codegen/dist/lib.js": "console.log('b');\n",
	})

	stub := &fakeLister{listings: map[string]lister.Listing{
		"\x00./packages/codegen/dist/cli.js": {
			InternalFiles: []string{"packages/codegen/dist/cli.js", "packages/codegen/dist/lib.js"},
		},
	}}
	r := mustNewResolver(t, root, stub)

	versions, err := r.Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
	got := versions[0]
	if got.Name != "@org/codegen" {
		t.Errorf("Name = %q, want @org/codegen", got.Name)
	}
	if !strings.HasPrefix(got.Version, "pnpm-local:@org/codegen@sha256:") {
		t.Errorf("Version = %q", got.Version)
	}
	if got.Source != "pnpm-local:@org/codegen" {
		t.Errorf("Source = %q, want pnpm-local:@org/codegen", got.Source)
	}
}

func TestResolver_HashChangesOnSourceEdit(t *testing.T) {
	root := setupWorkspace(t, []importerSpec{
		{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`},
	}, map[string]string{
		"packages/codegen/dist/cli.js": "console.log('v1');\n",
	})
	stub := &fakeLister{listings: map[string]lister.Listing{
		"\x00./packages/codegen/dist/cli.js": {
			InternalFiles: []string{"packages/codegen/dist/cli.js"},
		},
	}}

	v1 := mustResolveVersion(t, root, stub, "@org/codegen")

	if err := os.WriteFile(filepath.Join(root, "packages", "codegen", "dist", "cli.js"),
		[]byte("console.log('v2');\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v2 := mustResolveVersion(t, root, stub, "@org/codegen")

	if v1 == v2 {
		t.Errorf("source edit must change Version, got %q twice", v1)
	}
}

// TestResolver_MultipleBinEntriesUnioned guards the multi-entry case: when
// package.json declares two bins, both contribute files to the hash. Without
// this, edits to one bin's transitive deps would not invalidate the cache.
func TestResolver_MultipleBinEntriesUnioned(t *testing.T) {
	root := setupWorkspace(t, []importerSpec{{
		path:    "packages/codegen",
		pkgJSON: `{"name": "@org/codegen", "bin": {"a": "dist/a.js", "b": "dist/b.js"}}`,
	}}, map[string]string{
		"packages/codegen/dist/a.js": "console.log('a');\n",
		"packages/codegen/dist/b.js": "console.log('b');\n",
	})

	stub := &fakeLister{listings: map[string]lister.Listing{
		"\x00./packages/codegen/dist/a.js": {InternalFiles: []string{"packages/codegen/dist/a.js"}},
		"\x00./packages/codegen/dist/b.js": {InternalFiles: []string{"packages/codegen/dist/b.js"}},
	}}

	versions, err := mustNewResolver(t, root, stub).Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}

	if len(stub.calls) != 2 {
		t.Errorf("expected 2 lister calls (one per bin), got %d: %+v", len(stub.calls), stub.calls)
	}
}

// TestResolver_FailsOnPackageWithoutEntryPoints catches misconfigured workspace
// packages that declare neither bin nor main. Without the fail, the resolver
// would hash an empty file set and every cache lookup would falsely hit.
func TestResolver_FailsOnPackageWithoutEntryPoints(t *testing.T) {
	root := setupWorkspace(t, []importerSpec{
		{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen"}`},
	}, nil)
	r := mustNewResolver(t, root, &fakeLister{})

	_, err := r.Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err == nil {
		t.Fatal("expected error for package without bin/main")
	}
}

// TestResolver_DeterministicAcrossRuns guards that the same workspace state
// produces byte-identical Version strings across runs — the cornerstone of
// shared cache integrity.
func TestResolver_DeterministicAcrossRuns(t *testing.T) {
	root := setupWorkspace(t, []importerSpec{{
		path:    "packages/codegen",
		pkgJSON: `{"name": "@org/codegen", "bin": "dist/cli.js"}`,
	}}, map[string]string{
		"packages/codegen/dist/cli.js": "console.log('hi');\n",
		"packages/codegen/dist/lib.js": "module.exports = 1;\n",
	})

	stub := &fakeLister{listings: map[string]lister.Listing{
		"\x00./packages/codegen/dist/cli.js": {
			InternalFiles: []string{"packages/codegen/dist/cli.js", "packages/codegen/dist/lib.js"},
		},
	}}

	v1 := mustResolveVersion(t, root, stub, "@org/codegen")
	v2 := mustResolveVersion(t, root, stub, "@org/codegen")
	if v1 != v2 {
		t.Errorf("non-deterministic Version: %q vs %q", v1, v2)
	}
}

// importerSpec describes a workspace package fixture: where it lives and what
// its package.json should contain.
type importerSpec struct {
	path    string
	pkgJSON string
}

// setupWorkspace materialises a temp repo with a pnpm-lock.yaml that lists
// every importer and the corresponding package.json files plus extra source
// files.
func setupWorkspace(t *testing.T, importers []importerSpec, extra map[string]string) string {
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
	mustWriteFile(t, filepath.Join(root, "pnpm-lock.yaml"), b.String())

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

func mustResolveVersion(t *testing.T, root string, l lister.SourceLister, pkg string) string {
	t.Helper()
	r := mustNewResolver(t, root, l)
	versions, err := r.Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: pkg})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
	return versions[0].Version
}
