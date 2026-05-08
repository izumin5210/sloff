package pnpmlocal_test

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
)

// TestParsePackageJSON_ExtractsName guards the parser's only remaining job:
// surface the package name. Bin / main / other fields are intentionally
// dropped from PackageJSON since ADR-0008 D7 moved build/run into the cmd,
// so we no longer need to derive entry points from package.json.
func TestParsePackageJSON_ExtractsName(t *testing.T) {
	got, err := pnpmlocal.ParsePackageJSON([]byte(`{
  "name": "@org/codegen",
  "bin": "dist/cli.js",
  "main": "dist/index.js"
}`))
	if err != nil {
		t.Fatalf("ParsePackageJSON: %v", err)
	}
	want := &pnpmlocal.PackageJSON{Name: "@org/codegen"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestParsePackageJSON_NoName surfaces the no-name case (typical for the
// monorepo root package.json with `private: true`). The parser succeeds
// quietly; LoadWorkspace filters such entries out of the lookup index.
func TestParsePackageJSON_NoName(t *testing.T) {
	got, err := pnpmlocal.ParsePackageJSON([]byte(`{"private": true}`))
	if err != nil {
		t.Fatalf("ParsePackageJSON: %v", err)
	}
	if got.Name != "" {
		t.Errorf("Name = %q, want empty for package.json without name field", got.Name)
	}
}

// TestParsePackageJSON_InvalidJSONFails guards the boundary: malformed JSON
// must fail loudly rather than silently produce an empty PackageJSON that
// then drops the workspace member from the index.
func TestParsePackageJSON_InvalidJSONFails(t *testing.T) {
	if _, err := pnpmlocal.ParsePackageJSON([]byte(`{not valid`)); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}

func TestLoadPackageJSON(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "packages", "codegen", "package.json"),
		`{"name": "@org/codegen"}`)

	got, err := pnpmlocal.LoadPackageJSON(root, filepath.Join("packages", "codegen"))
	if err != nil {
		t.Fatalf("LoadPackageJSON: %v", err)
	}
	if got.Name != "@org/codegen" {
		t.Errorf("Name = %q, want @org/codegen", got.Name)
	}
}

func TestLoadPackageJSON_MissingFile(t *testing.T) {
	root := t.TempDir()
	if _, err := pnpmlocal.LoadPackageJSON(root, "packages/missing"); err == nil {
		t.Fatal("expected error when package.json is missing")
	}
}
