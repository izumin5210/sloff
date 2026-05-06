package lister_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

// requireGo skips the test when the `go` binary is not on PATH. packages.Load
// invokes `go list` internally, so the lister cannot operate without it.
func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not available; skipping packages.Load-based tests")
	}
}

// scaffoldModule writes a minimal Go module rooted at root and returns root.
// The module has one main package importing one internal subpackage, plus one
// _test.go to verify it is excluded.
func scaffoldModule(t *testing.T, modPath string) string {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, root, "go.mod", "module "+modPath+"\n\ngo 1.22\n")
	mustWriteFile(t, root, "cmd/tool/main.go", `package main

import "`+modPath+`/pkg/util"

func main() { util.Greet() }
`)
	mustWriteFile(t, root, "pkg/util/util.go", `package util

func Greet() string { return "hi" }
`)
	mustWriteFile(t, root, "pkg/util/util_test.go", `package util

import "testing"

func TestGreet(t *testing.T) { _ = Greet() }
`)
	return root
}

func TestGoPackages_ListsMainModuleFilesAndExcludesTests(t *testing.T) {
	requireGo(t)
	root := scaffoldModule(t, "example.test/scaffold")

	got, err := lister.NewGoPackages(root).List(context.Background(), "./cmd/tool/...")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	wantFiles := []string{
		"cmd/tool/main.go",
		"pkg/util/util.go",
	}
	sort.Strings(got.InternalFiles)
	if diff := cmpStringSlices(wantFiles, got.InternalFiles); diff != "" {
		t.Errorf("InternalFiles mismatch:\n%s", diff)
	}
	for _, f := range got.InternalFiles {
		if strings.HasSuffix(f, "_test.go") {
			t.Errorf("test file leaked into InternalFiles: %s", f)
		}
	}
}

func TestGoPackages_StdlibIsNotIncluded(t *testing.T) {
	requireGo(t)
	root := scaffoldModule(t, "example.test/stdlib")
	// main.go imports stdlib via util.go's "fmt"-free body, but the test
	// scaffolding's util.go body already pulls only project-local imports.
	// Add a stdlib import explicitly.
	mustWriteFile(t, root, "pkg/util/util.go", `package util

import "fmt"

func Greet() string { return fmt.Sprint("hi") }
`)

	got, err := lister.NewGoPackages(root).List(context.Background(), "./cmd/tool/...")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, f := range got.InternalFiles {
		// stdlib paths would appear under $GOROOT, never as relative repo paths.
		if filepath.IsAbs(f) || strings.HasPrefix(f, "..") {
			t.Errorf("absolute or escaping path leaked: %s", f)
		}
	}
	if len(got.ExternalModules) != 0 {
		t.Errorf("ExternalModules = %v, want empty (only stdlib was imported)", got.ExternalModules)
	}
}

func TestGoPackages_RejectsNonRelativeEntry(t *testing.T) {
	requireGo(t)
	root := scaffoldModule(t, "example.test/abs")

	if _, err := lister.NewGoPackages(root).List(context.Background(), "example.test/abs/cmd/tool"); err == nil {
		t.Error("expected error when entry lacks ./ prefix")
	}
}

func TestGoPackages_IncludesEmbedFiles(t *testing.T) {
	requireGo(t)
	root := t.TempDir()
	mustWriteFile(t, root, "go.mod", "module example.test/embed\n\ngo 1.22\n")
	mustWriteFile(t, root, "cmd/tool/main.go", `package main

import (
	_ "embed"
	"fmt"
)

//go:embed asset.txt
var asset string

func main() { fmt.Print(asset) }
`)
	mustWriteFile(t, root, "cmd/tool/asset.txt", "v1\n")

	got, err := lister.NewGoPackages(root).List(context.Background(), "./cmd/tool/...")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	wantAsset := "cmd/tool/asset.txt"
	if !slices.Contains(got.InternalFiles, wantAsset) {
		t.Errorf("InternalFiles must include the //go:embed asset %q, got %v", wantAsset, got.InternalFiles)
	}
}

func TestGoPackages_FailsOnMissingPackage(t *testing.T) {
	requireGo(t)
	root := scaffoldModule(t, "example.test/missing")

	if _, err := lister.NewGoPackages(root).List(context.Background(), "./cmd/does-not-exist"); err == nil {
		t.Error("expected error when entry does not match any package")
	}
}

func cmpStringSlices(want, got []string) string {
	if len(want) != len(got) {
		return diffSlices(want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			return diffSlices(want, got)
		}
	}
	return ""
}

func diffSlices(want, got []string) string {
	return "want=" + strings.Join(want, ",") + "\n got=" + strings.Join(got, ",")
}
