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

	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
)

// fakeEnumerator returns a fixed list per pkgDir key. Resolver tests use
// this to keep the resolver decoupled from a real git working tree. The
// resolver now batches all of a package's workspace dirs into one enumerator
// call, so enumerate records one calls entry per invocation (the comma-joined
// dirs) and returns the union of the per-dir fixtures.
type fakeEnumerator struct {
	calls []string
	files map[string][]string
	err   error
}

func (f *fakeEnumerator) enumerate(_ context.Context, _ string, pkgDirs ...string) ([]string, error) {
	f.calls = append(f.calls, strings.Join(pkgDirs, ","))
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for _, pkgDir := range pkgDirs {
		out = append(out, f.files[pkgDir]...)
	}
	return out, nil
}

func TestResolver_Name(t *testing.T) {
	root := setupWorkspace(t, nil, nil, "")
	r := mustNewResolver(t, root, &fakeEnumerator{})
	if r.Name() != "pnpm-local" {
		t.Errorf("Name() = %q, want pnpm-local", r.Name())
	}
}

func TestResolver_RejectsNilDeclaration(t *testing.T) {
	root := setupWorkspace(t, nil, nil, "")
	r := mustNewResolver(t, root, &fakeEnumerator{})
	if _, err := r.Inputs(context.Background(), ".", nil); err == nil {
		t.Error("Inputs: expected error when declared is nil (ADR-0005)")
	}
	if _, err := r.Versions(context.Background(), ".", nil); err == nil {
		t.Error("Versions: expected error when declared is nil (ADR-0005)")
	}
}

func TestResolver_RejectsEmptyPackageName(t *testing.T) {
	root := setupWorkspace(t, nil, nil, "")
	r := mustNewResolver(t, root, &fakeEnumerator{})
	declared := &toolresolver.DeclaredTool{Resolver: "pnpm-local"}
	if _, err := r.Inputs(context.Background(), ".", declared); err == nil {
		t.Error("Inputs: expected error when PackageName is empty")
	}
	if _, err := r.Versions(context.Background(), ".", declared); err == nil {
		t.Error("Versions: expected error when PackageName is empty")
	}
}

// TestResolver_RejectsNonWorkspacePackage guards the ADR-0007 boundary: a
// pnpm-local declaration referring to an external npm package must fail
// clearly so the user knows to switch to the script resolver instead of
// getting a silent wrong-version fingerprint.
func TestResolver_RejectsNonWorkspacePackage(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen"}`}},
		nil,
		"",
	)
	r := mustNewResolver(t, root, &fakeEnumerator{})

	_, err := r.Inputs(context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@external/foo"})
	if err == nil {
		t.Fatal("expected error for non-workspace package")
	}
	if !errors.Is(err, pnpmlocal.ErrNotWorkspacePackage) {
		t.Errorf("error %v should wrap ErrNotWorkspacePackage", err)
	}
}

// TestResolver_EnumeratesPackageDirAsExtraInputs is the happy path for the
// input-contributor side: every file the enumerator returns for the
// resolved workspace dir surfaces verbatim in Result.ExtraInputs so the
// runner folds them into files_hash.
func TestResolver_EnumeratesPackageDirAsExtraInputs(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen"}`}},
		nil,
		"",
	)
	stub := &fakeEnumerator{files: map[string][]string{
		filepath.Join("packages", "codegen"): {
			"packages/codegen/src/cli.ts",
			"packages/codegen/src/lib.ts",
			"packages/codegen/package.json",
		},
	}}
	r := mustNewResolver(t, root, stub)

	got, err := r.Inputs(context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	want := []string{
		"packages/codegen/package.json",
		"packages/codegen/src/cli.ts",
		"packages/codegen/src/lib.ts",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs (-want +got):\n%s", diff)
	}
}

// TestResolver_FollowsTransitiveWorkspaceLinks guards that link: deps are
// followed: editing @org/util's source must be visible to a consumer that
// uses @org/codegen, because codegen depends on util via workspace:*.
// Without this, transitive workspace edits would silently miss the fingerprint.
func TestResolver_FollowsTransitiveWorkspaceLinks(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{
			{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen"}`},
			{path: "packages/util", pkgJSON: `{"name": "@org/util"}`},
		},
		nil,
		`lockfileVersion: '9.0'
importers:
  packages/codegen:
    dependencies:
      '@org/util':
        specifier: workspace:*
        version: link:../util
  packages/util: {}
`,
	)
	stub := &fakeEnumerator{files: map[string][]string{
		filepath.Join("packages", "codegen"): {"packages/codegen/src/cli.ts"},
		filepath.Join("packages", "util"):    {"packages/util/src/lib.ts"},
	}}

	got, err := mustNewResolver(t, root, stub).Inputs(context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	want := []string{"packages/codegen/src/cli.ts", "packages/util/src/lib.ts"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs (-want +got):\n%s", diff)
	}
}

// TestResolver_EmitsTransitiveExternalsAsResolvedVersions covers the resolved_versions_hash
// side: every external dep reachable from the workspace package via the
// lockfile graph (including those reached only through workspace links)
// surfaces as `pnpm-deps:<pkg>@<ver>`.
func TestResolver_EmitsTransitiveExternalsAsResolvedVersions(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{
			{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen"}`},
			{path: "packages/util", pkgJSON: `{"name": "@org/util"}`},
		},
		nil,
		`lockfileVersion: '9.0'
importers:
  packages/codegen:
    dependencies:
      '@org/util':
        specifier: workspace:*
        version: link:../util
      lodash:
        specifier: ^4.17.0
        version: 4.17.21
  packages/util:
    dependencies:
      'some-helper':
        specifier: ^1.0.0
        version: 1.2.3
snapshots:
  lodash@4.17.21: {}
  some-helper@1.2.3: {}
`,
	)
	stub := &fakeEnumerator{files: map[string][]string{
		filepath.Join("packages", "codegen"): {"packages/codegen/src/cli.ts"},
		filepath.Join("packages", "util"):    {"packages/util/src/lib.ts"},
	}}

	got, err := mustNewResolver(t, root, stub).Versions(context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	versionStrs := make([]string, len(got))
	for i, v := range got {
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

// TestResolver_PassesThroughEnumeratorError surfaces enumeration failures
// rather than silently producing an empty hash that could mask a broken
// git environment.
func TestResolver_PassesThroughEnumeratorError(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen"}`}},
		nil,
		"",
	)
	stub := &fakeEnumerator{err: errors.New("git ls-files boom")}

	_, err := mustNewResolver(t, root, stub).Inputs(context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"})
	if err == nil || !strings.Contains(err.Error(), "git ls-files boom") {
		t.Errorf("err = %v, want wrap of enumerator error", err)
	}
}

// TestResolver_InputsAndVersionsShareWalk locks the IZU-16 caching contract:
// when both methods are called for the same declared package, the resolver
// only walks the lockfile and invokes the FileEnumerator once. Without the
// per-package memoisation, splitting the methods would double the discovery
// cost in production runs.
func TestResolver_InputsAndVersionsShareWalk(t *testing.T) {
	root := setupWorkspace(
		t,
		[]importerSpec{{path: "packages/codegen", pkgJSON: `{"name": "@org/codegen"}`}},
		nil,
		"",
	)
	stub := &fakeEnumerator{files: map[string][]string{
		filepath.Join("packages", "codegen"): {"packages/codegen/src/cli.ts"},
	}}
	r := mustNewResolver(t, root, stub)
	declared := &toolresolver.DeclaredTool{Resolver: "pnpm-local", PackageName: "@org/codegen"}

	if _, err := r.Inputs(context.Background(), ".", declared); err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	if _, err := r.Versions(context.Background(), ".", declared); err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(stub.calls) != 1 {
		t.Errorf("FileEnumerator should be invoked once across Inputs+Versions, got %d (calls=%v)", len(stub.calls), stub.calls)
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

func mustNewResolver(t *testing.T, root string, stub *fakeEnumerator) *pnpmlocal.Resolver {
	t.Helper()
	r, err := pnpmlocal.New(root, stub.enumerate)
	if err != nil {
		t.Fatalf("pnpmlocal.New: %v", err)
	}
	return r
}
