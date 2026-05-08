package pnpmlocal_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
)

func TestLoadLockfile_v9_ImporterPathsExposed(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  .:
    dependencies:
      '@org/codegen':
        specifier: workspace:*
        version: link:packages/codegen
  packages/codegen:
    dependencies:
      typescript:
        specifier: ^5.0.0
        version: 5.0.0
  packages/util:
    {}
`)

	lf, err := pnpmlocal.LoadLockfile(root)
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}

	got := lf.WorkspacePaths()
	want := []string{".", "packages/codegen", "packages/util"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("WorkspacePaths mismatch (-want +got):\n%s", diff)
	}
}

func TestLoadLockfile_MissingFile(t *testing.T) {
	root := t.TempDir()
	if _, err := pnpmlocal.LoadLockfile(root); err == nil {
		t.Fatal("expected error when pnpm-lock.yaml is missing")
	}
}

func TestLoadLockfile_InvalidYAML(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), "importers: [unbalanced")
	if _, err := pnpmlocal.LoadLockfile(root); err == nil {
		t.Fatal("expected error on invalid YAML")
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
