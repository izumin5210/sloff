package pnpmlocal_test

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/pnpmlocal"
)

func TestParsePackageJSON_BinAsString(t *testing.T) {
	got, err := pnpmlocal.ParsePackageJSON([]byte(`{
  "name": "@org/codegen",
  "bin": "dist/cli.js",
  "main": "dist/index.js"
}`))
	if err != nil {
		t.Fatalf("ParsePackageJSON: %v", err)
	}
	want := &pnpmlocal.PackageJSON{
		Name:        "@org/codegen",
		Bin:         []string{"dist/cli.js"},
		Main:        "dist/index.js",
		EntryPoints: []string{"dist/cli.js"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestParsePackageJSON_BinAsObject_SortedDeterministic(t *testing.T) {
	got, err := pnpmlocal.ParsePackageJSON([]byte(`{
  "name": "@org/codegen",
  "bin": {
    "z-tool": "dist/z.js",
    "a-tool": "dist/a.js"
  }
}`))
	if err != nil {
		t.Fatalf("ParsePackageJSON: %v", err)
	}
	if diff := cmp.Diff([]string{"dist/a.js", "dist/z.js"}, got.EntryPoints); diff != "" {
		t.Errorf("EntryPoints not sorted (-want +got):\n%s", diff)
	}
}

// TestParsePackageJSON_FallsBackToMainWhenNoBin guards the documented "ts-node /
// tsx で直接 source 実行" pattern: a workspace package with `main` but no `bin`
// must still produce an entry point so the lister has something to anchor on.
func TestParsePackageJSON_FallsBackToMainWhenNoBin(t *testing.T) {
	got, err := pnpmlocal.ParsePackageJSON([]byte(`{
  "name": "@org/codegen",
  "main": "src/index.ts"
}`))
	if err != nil {
		t.Fatalf("ParsePackageJSON: %v", err)
	}
	if diff := cmp.Diff([]string{"src/index.ts"}, got.EntryPoints); diff != "" {
		t.Errorf("EntryPoints (-want +got):\n%s", diff)
	}
}

// TestParsePackageJSON_NoEntryPoints surfaces the case where neither bin nor
// main is set. The resolver upstream needs to fail loudly in this case rather
// than hash an empty file set, so we expose it as zero-length EntryPoints
// without erroring at parse time.
func TestParsePackageJSON_NoEntryPoints(t *testing.T) {
	got, err := pnpmlocal.ParsePackageJSON([]byte(`{"name": "@org/codegen"}`))
	if err != nil {
		t.Fatalf("ParsePackageJSON: %v", err)
	}
	if len(got.EntryPoints) != 0 {
		t.Errorf("EntryPoints = %v, want empty", got.EntryPoints)
	}
}

func TestParsePackageJSON_InvalidBinShape(t *testing.T) {
	_, err := pnpmlocal.ParsePackageJSON([]byte(`{"name": "x", "bin": [1,2,3]}`))
	if err == nil {
		t.Fatal("expected error when bin is neither string nor object")
	}
}

func TestLoadPackageJSON(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "packages", "codegen", "package.json"), `{
  "name": "@org/codegen",
  "bin": "dist/cli.js"
}`)

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
