package lister

import (
	"os"
	"path/filepath"
	"strings"
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

// TestExternalLabel_VersionedReplaceDistinguishesTarget guards that
// `replace example.com/a => example.com/b v1.2.3` and `=> example.com/c v1.2.3`
// hash to different labels. Without encoding the replacement path, both would
// collapse to the same example.com/a@v1.2.3 and let lazygen serve stale outputs
// after a replacement-target switch.
func TestExternalLabel_VersionedReplaceDistinguishesTarget(t *testing.T) {
	toB := &packages.Module{
		Path: "example.com/a",
		Replace: &packages.Module{
			Path:    "example.com/b",
			Version: "v1.2.3",
		},
	}
	toC := &packages.Module{
		Path: "example.com/a",
		Replace: &packages.Module{
			Path:    "example.com/c",
			Version: "v1.2.3",
		},
	}
	pB, vB, _, _ := externalLabel(toB)
	pC, vC, _, _ := externalLabel(toC)
	if pB+"@"+vB == pC+"@"+vC {
		t.Errorf("labels collapsed: both replacements hash to %q@%q", pB, vB)
	}
	if pB != "example.com/a" {
		t.Errorf("labelPath = %q, want example.com/a (must keep the original import path stable)", pB)
	}
	if !strings.Contains(vB, "example.com/b") || !strings.Contains(vB, "v1.2.3") {
		t.Errorf("labelVersion = %q, must encode replacement path and version", vB)
	}
}

// TestExternalLabel_VersionedReplaceLooksUpGoSumByReplacementPath guards that
// the go.sum lookup follows the replacement target. go.sum is keyed by the
// replaced-with module path, so looking up the original path would always
// miss and leave GoSumLine empty for versioned replaces.
func TestExternalLabel_VersionedReplaceLooksUpGoSumByReplacementPath(t *testing.T) {
	m := &packages.Module{
		Path: "example.com/a",
		Replace: &packages.Module{
			Path:    "example.com/b",
			Version: "v1.2.3",
		},
	}
	_, _, sumPath, sumVersion := externalLabel(m)
	if sumPath != "example.com/b" {
		t.Errorf("sumPath = %q, want example.com/b (go.sum is keyed by the replacement)", sumPath)
	}
	if sumVersion != "v1.2.3" {
		t.Errorf("sumVersion = %q, want v1.2.3", sumVersion)
	}
}

func TestExternalLabel_LocalReplaceHasNoGoSumLookup(t *testing.T) {
	m := &packages.Module{
		Path: "example.com/a",
		Replace: &packages.Module{
			Path: "../local-fork", // no Version → local-directory replace
		},
	}
	labelPath, labelVersion, sumPath, sumVersion := externalLabel(m)
	if labelPath != "example.com/a" {
		t.Errorf("labelPath = %q, want example.com/a", labelPath)
	}
	if !strings.Contains(labelVersion, "../local-fork") {
		t.Errorf("labelVersion = %q must encode the replacement path so directive changes flip the hash", labelVersion)
	}
	if sumPath != "" || sumVersion != "" {
		t.Errorf("local replace must skip go.sum lookup, got sumPath=%q sumVersion=%q", sumPath, sumVersion)
	}
}

func TestExternalLabel_RegularDependencyUsesItsOwnPathAndVersion(t *testing.T) {
	m := &packages.Module{
		Path:    "example.com/dep",
		Version: "v1.0.0",
	}
	labelPath, labelVersion, sumPath, sumVersion := externalLabel(m)
	if labelPath != "example.com/dep" || labelVersion != "v1.0.0" {
		t.Errorf("label = %q@%q, want example.com/dep@v1.0.0", labelPath, labelVersion)
	}
	if sumPath != "example.com/dep" || sumVersion != "v1.0.0" {
		t.Errorf("sum lookup = %q@%q, want example.com/dep@v1.0.0", sumPath, sumVersion)
	}
}
