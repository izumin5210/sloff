package pnpmlocal_test

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/pnpmlocal"
)

func TestLoadWorkspace_LooksUpByName(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  .:
    dependencies:
      '@org/codegen':
        specifier: workspace:*
        version: link:packages/codegen
  packages/codegen:
    {}
  packages/util:
    {}
`)
	mustWrite(t, filepath.Join(root, "package.json"), `{"name": "monorepo-root"}`)
	mustWrite(t, filepath.Join(root, "packages", "codegen", "package.json"), `{
  "name": "@org/codegen",
  "bin": "dist/cli.js"
}`)
	mustWrite(t, filepath.Join(root, "packages", "util", "package.json"), `{
  "name": "@org/util",
  "main": "dist/index.js"
}`)

	ws, err := pnpmlocal.LoadWorkspace(root)
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}

	got, ok := ws.Lookup("@org/codegen")
	if !ok {
		t.Fatalf("Lookup(@org/codegen): not found")
	}
	want := pnpmlocal.WorkspacePackage{
		Name: "@org/codegen",
		Dir:  filepath.Join("packages", "codegen"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Lookup(@org/codegen) mismatch (-want +got):\n%s", diff)
	}
}

// TestLoadWorkspace_RejectsDuplicateName guards against silently picking one
// of two workspace packages that share a name. pnpm itself rejects this state,
// but if it slips through (manual edits, broken merge), we still want a clear
// error rather than a non-deterministic Lookup result.
func TestLoadWorkspace_RejectsDuplicateName(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  packages/a: {}
  packages/b: {}
`)
	mustWrite(t, filepath.Join(root, "packages", "a", "package.json"), `{"name": "@org/dup"}`)
	mustWrite(t, filepath.Join(root, "packages", "b", "package.json"), `{"name": "@org/dup"}`)

	if _, err := pnpmlocal.LoadWorkspace(root); err == nil {
		t.Fatal("expected error on duplicate workspace package name")
	}
}

// TestLoadWorkspace_SkipsImportersWithoutPackageJSON guards stale lockfile
// drift: pnpm-lock.yaml routinely carries entries for renamed/removed
// workspace members until the user reruns `pnpm install`. A missing manifest
// for one such entry must not break resolution for unrelated packages whose
// package.json is intact — pnpm-local is supposed to tolerate benign drift
// the same way the downstream WalkDeps walker does.
func TestLoadWorkspace_SkipsImportersWithoutPackageJSON(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  .: {}
  packages/codegen: {}
  packages/stale: {}
`)
	// Root importer + stale importer have no package.json on disk.
	// Only @org/codegen is present.
	mustWrite(t, filepath.Join(root, "packages", "codegen", "package.json"),
		`{"name": "@org/codegen", "bin": "dist/cli.js"}`)

	ws, err := pnpmlocal.LoadWorkspace(root)
	if err != nil {
		t.Fatalf("LoadWorkspace must tolerate missing package.json: %v", err)
	}
	if _, ok := ws.Lookup("@org/codegen"); !ok {
		t.Errorf("@org/codegen lookup failed despite its manifest being intact")
	}
}

// TestLoadWorkspace_StillFailsOnCorruptPackageJSON guards the boundary of
// the fs.ErrNotExist tolerance: an existing-but-malformed package.json
// should still fail loudly. Otherwise corrupt manifests would be silently
// dropped from the index and the user would only notice when downstream
// lookups returned ErrNotWorkspacePackage with no clue why.
func TestLoadWorkspace_StillFailsOnCorruptPackageJSON(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  packages/broken: {}
`)
	mustWrite(t, filepath.Join(root, "packages", "broken", "package.json"), `{not valid json`)

	if _, err := pnpmlocal.LoadWorkspace(root); err == nil {
		t.Fatal("expected error for corrupt package.json, not silent skip")
	}
}

// TestLoadWorkspace_RootImporterWithoutNameIsIgnored guards the common case
// where the monorepo root package.json has no `name` (private workspace
// container). The lockfile lists `.` as an importer, but it should not be
// surfaced as a workspace package.
func TestLoadWorkspace_RootImporterWithoutNameIsIgnored(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  .: {}
  packages/codegen: {}
`)
	mustWrite(t, filepath.Join(root, "package.json"), `{"private": true}`)
	mustWrite(t, filepath.Join(root, "packages", "codegen", "package.json"), `{"name": "@org/codegen", "bin": "dist/cli.js"}`)

	ws, err := pnpmlocal.LoadWorkspace(root)
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}
	if _, ok := ws.Lookup(""); ok {
		t.Errorf("empty name must never be looked up")
	}
	if _, ok := ws.Lookup("@org/codegen"); !ok {
		t.Errorf("@org/codegen lookup failed")
	}
}

// TestLoadWorkspace_AllReturnsSortedPackages exposes the full set so the
// preflight checker can iterate every workspace package without re-reading
// the lockfile, in deterministic order.
func TestLoadWorkspace_AllReturnsSortedPackages(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  packages/z: {}
  packages/a: {}
`)
	mustWrite(t, filepath.Join(root, "packages", "z", "package.json"), `{"name": "@org/z", "bin": "dist/z.js"}`)
	mustWrite(t, filepath.Join(root, "packages", "a", "package.json"), `{"name": "@org/a", "bin": "dist/a.js"}`)

	ws, err := pnpmlocal.LoadWorkspace(root)
	if err != nil {
		t.Fatalf("LoadWorkspace: %v", err)
	}
	all := ws.All()
	if len(all) != 2 || all[0].Name != "@org/a" || all[1].Name != "@org/z" {
		t.Errorf("All() = %+v, want sorted [@org/a, @org/z]", all)
	}
}
