package lister_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
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

	got, err := lister.NewGoPackages(root).List(context.Background(), "", "./cmd/tool/...")
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

// TestGoPackages_ResolvesAgainstNestedModule guards that packages.Load is
// invoked inside the spec's working directory, not at repo root. Without
// this, a sub-module's `go run ./cmd/tool` would fail to resolve because
// the loader would look at the (unrelated) repo-root module.
func TestGoPackages_ResolvesAgainstNestedModule(t *testing.T) {
	requireGo(t)
	root := t.TempDir()
	// Repo root has no go.mod; the only module sits under submodule/.
	mustWriteFile(t, root, "submodule/go.mod", "module example.test/sub\n\ngo 1.22\n")
	mustWriteFile(t, root, "submodule/cmd/tool/main.go", "package main\n\nfunc main() {}\n")

	got, err := lister.NewGoPackages(root).List(context.Background(), "submodule", "./cmd/tool/...")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := "submodule/cmd/tool/main.go"
	if !slices.Contains(got.InternalFiles, want) {
		t.Errorf("InternalFiles = %v, want to contain %q", got.InternalFiles, want)
	}
}

// TestGoPackages_IncludesIgnoredFilesForBuildTagSources guards that build-
// tagged sources (foo_linux.go, foo_darwin.go, files behind custom -tags)
// also contribute to the hash. Without this, the same generator would hash
// to different values depending on the host GOOS/GOARCH and break the
// resolver's OS-neutral cache contract.
func TestGoPackages_IncludesIgnoredFilesForBuildTagSources(t *testing.T) {
	requireGo(t)
	root := t.TempDir()
	mustWriteFile(t, root, "go.mod", "module example.test/buildtag\n\ngo 1.22\n")
	mustWriteFile(t, root, "cmd/tool/main.go", "package main\n\nfunc main() {}\n")
	mustWriteFile(t, root, "cmd/tool/main_linux.go", "//go:build linux\n\npackage main\n")
	mustWriteFile(t, root, "cmd/tool/main_darwin.go", "//go:build darwin\n\npackage main\n")

	got, err := lister.NewGoPackages(root).List(context.Background(), "", "./cmd/tool/...")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, want := range []string{
		"cmd/tool/main.go",
		"cmd/tool/main_linux.go",
		"cmd/tool/main_darwin.go",
	} {
		if !slices.Contains(got.InternalFiles, want) {
			t.Errorf("InternalFiles = %v, must include %q (build-tag sources are required for OS-neutral hashing)", got.InternalFiles, want)
		}
	}
}

// TestGoPackages_IncludesOtherFiles guards that non-Go source files reported
// by packages.Package.OtherFiles (.s / .c / .cc / .syso) are also folded into
// the hash. `go run` rebuilds when these files change, so omitting them would
// let edits to assembly or cgo sources pass without invalidating resolved_versions_hash.
func TestGoPackages_IncludesOtherFiles(t *testing.T) {
	requireGo(t)
	root := t.TempDir()
	mustWriteFile(t, root, "go.mod", "module example.test/other\n\ngo 1.22\n")
	mustWriteFile(t, root, "cmd/tool/main.go", "package main\n\nfunc add(a, b int) int\n\nfunc main() { _ = add(1, 2) }\n")
	// Minimal Go-syntax assembly stub so packages.Load reports it in OtherFiles.
	mustWriteFile(t, root, "cmd/tool/add_amd64.s", "TEXT ·add(SB), $0-24\n\tMOVQ a+0(FP), AX\n\tADDQ b+8(FP), AX\n\tMOVQ AX, ret+16(FP)\n\tRET\n")

	got, err := lister.NewGoPackages(root).List(context.Background(), "", "./cmd/tool/...")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := "cmd/tool/add_amd64.s"
	if !slices.Contains(got.InternalFiles, want) {
		t.Errorf("InternalFiles = %v, must include %q (non-Go sources change the built binary)", got.InternalFiles, want)
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

	got, err := lister.NewGoPackages(root).List(context.Background(), "", "./cmd/tool/...")
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

	if _, err := lister.NewGoPackages(root).List(context.Background(), "", "example.test/abs/cmd/tool"); err == nil {
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

	got, err := lister.NewGoPackages(root).List(context.Background(), "", "./cmd/tool/...")
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

	if _, err := lister.NewGoPackages(root).List(context.Background(), "", "./cmd/does-not-exist"); err == nil {
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
