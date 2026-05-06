package lister

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestReadGoSumForMainModules_ReadsFromLoadedModuleDir guards the fix for the
// nested-module go.sum bug: the lister must fingerprint external deps against
// the go.sum that sits next to the *loaded* go.mod, not against repoRoot/go.sum.
// Without this, bumping a dependency in submodule/ would leave tools_hash
// unchanged and let lazygen serve stale outputs.
func TestReadGoSumForMainModules_ReadsFromLoadedModuleDir(t *testing.T) {
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

	got, err := readGoSumForMainModules([]*packages.Package{pkg})
	if err != nil {
		t.Fatalf("readGoSumForMainModules: %v", err)
	}
	if string(got) != "SUBMODULE\n" {
		t.Errorf("read %q, want %q (must follow Module.GoMod, not repoRoot/go.sum)", got, "SUBMODULE\n")
	}
}

func TestReadGoSumForMainModules_MissingGoSumIsNotAnError(t *testing.T) {
	repoRoot := t.TempDir()
	pkg := &packages.Package{
		ID: "example.test/x",
		Module: &packages.Module{
			Main:  true,
			Path:  "example.test/x",
			GoMod: filepath.Join(repoRoot, "go.mod"),
		},
	}
	got, err := readGoSumForMainModules([]*packages.Package{pkg})
	if err != nil {
		t.Fatalf("readGoSumForMainModules: %v", err)
	}
	if got != nil {
		t.Errorf("read %q, want nil (missing go.sum is legitimate)", got)
	}
}

func TestReadGoSumForMainModules_NoMainModuleReturnsEmpty(t *testing.T) {
	pkg := &packages.Package{
		ID:     "example.test/no-module",
		Module: nil, // stdlib-style package
	}
	got, err := readGoSumForMainModules([]*packages.Package{pkg})
	if err != nil {
		t.Fatalf("readGoSumForMainModules: %v", err)
	}
	if got != nil {
		t.Errorf("read %q, want nil (no main module visible)", got)
	}
}

// TestReadGoSumForMainModules_ConcatenatesGoWorkSiblings guards multi-module
// go.work support: when packages.Load surfaces two repo-local mains, both
// modules' go.sum lines must end up in the result so that bumping a dep in
// either module flips tools_hash.
func TestReadGoSumForMainModules_ConcatenatesGoWorkSiblings(t *testing.T) {
	repoRoot := t.TempDir()
	dirA := filepath.Join(repoRoot, "a")
	dirB := filepath.Join(repoRoot, "b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "go.sum"), []byte("MOD-A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "go.sum"), []byte("MOD-B\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &packages.Package{
		ID: "example.test/a/cmd/tool",
		Module: &packages.Module{
			Main:  true,
			Path:  "example.test/a",
			GoMod: filepath.Join(dirA, "go.mod"),
		},
	}
	b := &packages.Package{
		ID: "example.test/b/pkg/util",
		Module: &packages.Module{
			Main:  true,
			Path:  "example.test/b",
			GoMod: filepath.Join(dirB, "go.mod"),
		},
	}
	a.Imports = map[string]*packages.Package{b.ID: b}

	got, err := readGoSumForMainModules([]*packages.Package{a})
	if err != nil {
		t.Fatalf("readGoSumForMainModules: %v", err)
	}
	if !strings.Contains(string(got), "MOD-A") || !strings.Contains(string(got), "MOD-B") {
		t.Errorf("got %q, must contain go.sum lines from both workspace mains", got)
	}
}

// TestReadGoSumForMainModules_TolerateMissingSiblingGoSum: if one main module
// has a go.sum and the other doesn't, the result is the union of what exists.
// Missing files must not abort the whole load.
func TestReadGoSumForMainModules_TolerateMissingSiblingGoSum(t *testing.T) {
	repoRoot := t.TempDir()
	dirA := filepath.Join(repoRoot, "a")
	dirB := filepath.Join(repoRoot, "b")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "go.sum"), []byte("ONLY-A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// dirB has no go.sum on purpose.

	a := &packages.Package{
		ID:     "example.test/a",
		Module: &packages.Module{Main: true, Path: "example.test/a", GoMod: filepath.Join(dirA, "go.mod")},
	}
	b := &packages.Package{
		ID:     "example.test/b",
		Module: &packages.Module{Main: true, Path: "example.test/b", GoMod: filepath.Join(dirB, "go.mod")},
	}
	a.Imports = map[string]*packages.Package{b.ID: b}

	got, err := readGoSumForMainModules([]*packages.Package{a})
	if err != nil {
		t.Fatalf("readGoSumForMainModules: %v", err)
	}
	if string(got) != "ONLY-A\n" {
		t.Errorf("got %q, want ONLY-A\\n", got)
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

// TestWalk_LocalReplaceHashesReplacedSourcesAsInternal guards that local
// replace directives (`replace example.com/a => ../local`) are folded into
// the internal-file set rather than treated as external label-only entries.
// Without this, edits to the replaced module would leave tools_hash unchanged
// even though `go run` rebuilds against the new code.
func TestWalk_LocalReplaceHashesReplacedSourcesAsInternal(t *testing.T) {
	repoRoot := t.TempDir()
	replacedDir := filepath.Join(repoRoot, "vendor-fork", "pkg")
	if err := os.MkdirAll(replacedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(replacedDir, "lib.go")
	if err := os.WriteFile(srcPath, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkg := &packages.Package{
		ID:         "example.com/upstream/pkg",
		GoFiles:    []string{srcPath},
		EmbedFiles: nil,
		Module: &packages.Module{
			Path:    "example.com/upstream",
			Version: "v0.0.0",
			Replace: &packages.Module{
				Path: "../vendor-fork", // no Version → local-directory replace
				Dir:  filepath.Join(repoRoot, "vendor-fork"),
			},
		},
	}
	l := &goPackagesLister{repoRoot: repoRoot}
	listing, err := l.walk([]*packages.Package{pkg}, nil)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	want := "vendor-fork/pkg/lib.go"
	if !slices.Contains(listing.InternalFiles, want) {
		t.Errorf("InternalFiles = %v, must include %q", listing.InternalFiles, want)
	}
	if len(listing.ExternalModules) != 0 {
		t.Errorf("ExternalModules = %v, must be empty for local replace (handled as internal)", listing.ExternalModules)
	}
}

// TestWalk_LocalReplaceRejectsOutsideRepoRoot guards the boundary: if a local
// replace points outside repoRoot (absolute path or `../sibling-repo`), the
// resulting file paths would vary per developer machine and break OS-neutral
// cache sharing. Refuse to fingerprint instead of recording per-machine paths.
func TestWalk_LocalReplaceRejectsOutsideRepoRoot(t *testing.T) {
	repoRoot := t.TempDir()
	outside := t.TempDir() // a separate tempdir, definitely outside repoRoot
	srcPath := filepath.Join(outside, "lib.go")
	if err := os.WriteFile(srcPath, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkg := &packages.Package{
		ID:      "example.com/upstream/pkg",
		GoFiles: []string{srcPath},
		Module: &packages.Module{
			Path:    "example.com/upstream",
			Version: "v0.0.0",
			Replace: &packages.Module{
				Path: outside,
				Dir:  outside,
			},
		},
	}
	l := &goPackagesLister{repoRoot: repoRoot}
	_, err := l.walk([]*packages.Package{pkg}, nil)
	if err == nil {
		t.Fatal("expected error when local replace target escapes repo root")
	}
	if !strings.Contains(err.Error(), "local replace") || !strings.Contains(err.Error(), "escapes repo root") {
		t.Errorf("error %q must explain the cause", err)
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
