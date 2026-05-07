package pnpmlocal_test

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/pnpmlocal"
)

// TestCollectExternals_DirectAndTransitive guards the surgical-walk contract:
// starting from a workspace package, every external dep — direct OR transitive
// — must surface with its resolved version string. Without transitive walk,
// indirect dep bumps (e.g. lodash → some-helper@1.x → some-helper@2.x) would
// not invalidate the consuming task even though runtime behaviour shifts.
func TestCollectExternals_DirectAndTransitive(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  packages/codegen:
    dependencies:
      lodash:
        specifier: ^4.17.0
        version: 4.17.21
    devDependencies:
      typescript:
        specifier: ^5.0.0
        version: 5.0.0
snapshots:
  lodash@4.17.21:
    dependencies:
      some-helper: 1.2.3
  some-helper@1.2.3: {}
  typescript@5.0.0: {}
`)

	lf, err := pnpmlocal.LoadLockfile(root)
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}
	got, err := pnpmlocal.CollectExternals(lf, "packages/codegen")
	if err != nil {
		t.Fatalf("CollectExternals: %v", err)
	}
	want := []string{
		"lodash@4.17.21",
		"some-helper@1.2.3",
		"typescript@5.0.0",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("externals mismatch (-want +got):\n%s", diff)
	}
}

// TestCollectExternals_SkipsWorkspaceLinks ensures workspace:* (recorded as
// "link:..." or "file:..." in the lockfile) are NOT emitted as externals.
// Their hashing is the esbuild lister's job; double-counting them here would
// inflate tools_hash with workspace identifiers that already feed files_hash
// via extra inputs.
func TestCollectExternals_SkipsWorkspaceLinks(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  packages/codegen:
    dependencies:
      '@org/util':
        specifier: workspace:*
        version: link:../util
      lodash:
        specifier: ^4.17.0
        version: 4.17.21
snapshots:
  lodash@4.17.21: {}
`)

	lf, err := pnpmlocal.LoadLockfile(root)
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}
	got, err := pnpmlocal.CollectExternals(lf, "packages/codegen")
	if err != nil {
		t.Fatalf("CollectExternals: %v", err)
	}
	if diff := cmp.Diff([]string{"lodash@4.17.21"}, got); diff != "" {
		t.Errorf("externals mismatch (-want +got):\n%s", diff)
	}
}

// TestCollectExternals_HandlesPeerDepSuffix guards the peer-context form
// (`pkg@1.0.0(peer@2.0.0)`) that pnpm uses when peers vary across consumers.
// The full version string with the peer suffix must round-trip so peer
// changes invalidate the cache too.
func TestCollectExternals_HandlesPeerDepSuffix(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  packages/codegen:
    dependencies:
      react-dom:
        specifier: ^18.0.0
        version: 18.0.0(react@18.0.0)
snapshots:
  'react-dom@18.0.0(react@18.0.0)':
    dependencies:
      react: 18.0.0
  react@18.0.0: {}
`)

	lf, err := pnpmlocal.LoadLockfile(root)
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}
	got, err := pnpmlocal.CollectExternals(lf, "packages/codegen")
	if err != nil {
		t.Fatalf("CollectExternals: %v", err)
	}
	want := []string{
		"react-dom@18.0.0(react@18.0.0)",
		"react@18.0.0",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("externals mismatch (-want +got):\n%s", diff)
	}
}

// TestCollectExternals_DedupesAcrossPaths exercises diamond dependencies:
// two paths can reach the same external. The walk must emit each (name,
// version) exactly once so tools_hash stays deterministic.
func TestCollectExternals_DedupesAcrossPaths(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  packages/codegen:
    dependencies:
      a:
        specifier: ^1.0.0
        version: 1.0.0
      b:
        specifier: ^1.0.0
        version: 1.0.0
snapshots:
  a@1.0.0:
    dependencies:
      shared: 1.0.0
  b@1.0.0:
    dependencies:
      shared: 1.0.0
  shared@1.0.0: {}
`)

	lf, err := pnpmlocal.LoadLockfile(root)
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}
	got, err := pnpmlocal.CollectExternals(lf, "packages/codegen")
	if err != nil {
		t.Fatalf("CollectExternals: %v", err)
	}
	want := []string{
		"a@1.0.0",
		"b@1.0.0",
		"shared@1.0.0",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("externals mismatch (-want +got):\n%s", diff)
	}
}

// TestCollectExternals_HandlesCircularDeps catches the "two packages refer to
// each other transitively" case. Without visited bookkeeping the walk would
// loop forever.
func TestCollectExternals_HandlesCircularDeps(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  packages/codegen:
    dependencies:
      a:
        specifier: ^1.0.0
        version: 1.0.0
snapshots:
  a@1.0.0:
    dependencies:
      b: 1.0.0
  b@1.0.0:
    dependencies:
      a: 1.0.0
`)

	lf, err := pnpmlocal.LoadLockfile(root)
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}
	got, err := pnpmlocal.CollectExternals(lf, "packages/codegen")
	if err != nil {
		t.Fatalf("CollectExternals: %v", err)
	}
	want := []string{"a@1.0.0", "b@1.0.0"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("externals mismatch (-want +got):\n%s", diff)
	}
}

// TestCollectExternals_UnknownImporterFails surfaces user errors loudly:
// asking for externals of a workspace package that the lockfile doesn't list
// must error rather than silently return empty (which would produce a
// misleadingly-stable tools_hash).
func TestCollectExternals_UnknownImporterFails(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  packages/codegen: {}
`)
	lf, err := pnpmlocal.LoadLockfile(root)
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}
	if _, err := pnpmlocal.CollectExternals(lf, "packages/missing"); err == nil {
		t.Fatal("expected error for unknown importer path")
	}
}
