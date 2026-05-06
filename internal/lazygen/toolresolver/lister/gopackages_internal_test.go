package lister

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestReadGoSumForMainModule_ReadsFromLoadedModuleDir guards the fix for the
// nested-module go.sum bug: the lister must fingerprint external deps against
// the go.sum that sits next to the *loaded* go.mod, not against repoRoot/go.sum.
// Without this, bumping a dependency in submodule/ would leave tools_hash
// unchanged and let lazygen serve stale outputs.
func TestReadGoSumForMainModule_ReadsFromLoadedModuleDir(t *testing.T) {
	repoRoot := t.TempDir()
	subDir := filepath.Join(repoRoot, "submodule")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a distinctive go.sum next to the submodule's go.mod and an unrelated
	// one at the repo root so we can tell which file the lister picked.
	if err := os.WriteFile(filepath.Join(repoRoot, "go.sum"), []byte("ROOT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "go.sum"), []byte("SUBMODULE\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkg := &packages.Package{
		ID: "example.test/sub/cmd/tool",
		Module: &packages.Module{
			Main:  true,
			Path:  "example.test/sub",
			GoMod: filepath.Join(subDir, "go.mod"),
		},
	}

	got, err := readGoSumForMainModule([]*packages.Package{pkg})
	if err != nil {
		t.Fatalf("readGoSumForMainModule: %v", err)
	}
	if string(got) != "SUBMODULE\n" {
		t.Errorf("read %q, want %q (must follow Module.GoMod, not repoRoot/go.sum)", got, "SUBMODULE\n")
	}
}

func TestReadGoSumForMainModule_MissingGoSumIsNotAnError(t *testing.T) {
	repoRoot := t.TempDir()
	pkg := &packages.Package{
		ID: "example.test/x",
		Module: &packages.Module{
			Main:  true,
			Path:  "example.test/x",
			GoMod: filepath.Join(repoRoot, "go.mod"),
		},
	}
	got, err := readGoSumForMainModule([]*packages.Package{pkg})
	if err != nil {
		t.Fatalf("readGoSumForMainModule: %v", err)
	}
	if got != nil {
		t.Errorf("read %q, want nil (missing go.sum is legitimate)", got)
	}
}

func TestReadGoSumForMainModule_NoMainModuleReturnsEmpty(t *testing.T) {
	pkg := &packages.Package{
		ID:     "example.test/no-module",
		Module: nil, // stdlib-style package
	}
	got, err := readGoSumForMainModule([]*packages.Package{pkg})
	if err != nil {
		t.Fatalf("readGoSumForMainModule: %v", err)
	}
	if got != nil {
		t.Errorf("read %q, want nil (no main module visible)", got)
	}
}
