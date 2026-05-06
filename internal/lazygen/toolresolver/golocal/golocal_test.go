package golocal_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/golocal"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

// fakeLister is a stub SourceLister that records calls and returns a fixed
// listing per entry. It lets golocal tests exercise hashing logic without
// depending on `go list` / packages.Load.
type fakeLister struct {
	gotEntry string
	calls    int
	listing  lister.Listing
	err      error
}

func (f *fakeLister) List(_ context.Context, entry string) (lister.Listing, error) {
	f.gotEntry = entry
	f.calls++
	if f.err != nil {
		return lister.Listing{}, f.err
	}
	return f.listing, nil
}

func TestResolver_Name(t *testing.T) {
	r := golocal.New(t.TempDir(), &fakeLister{})
	if r.Name() != "go-local" {
		t.Errorf("Name() = %q, want go-local", r.Name())
	}
}

func TestResolver_DeclaredEntryDrivesResolution(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"cmd/bar/main.go": "package main\nfunc main() {}\n",
	})
	stub := &fakeLister{listing: lister.Listing{InternalFiles: []string{"cmd/bar/main.go"}}}

	// Per ADR-0005 the resolver is declared-only; the cmd shape is irrelevant.
	versions, err := golocal.New(root, stub).Resolve(
		context.Background(), ".", []string{"bin/protoc-gen-bar"},
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/bar"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if stub.gotEntry != "./cmd/bar" {
		t.Errorf("lister received entry %q, want ./cmd/bar", stub.gotEntry)
	}
	if !strings.HasPrefix(versions[0].Version, "go-local:./cmd/bar@sha256:") {
		t.Errorf("Version = %q", versions[0].Version)
	}
}

func TestResolver_FailsWithoutDeclaration(t *testing.T) {
	r := golocal.New(t.TempDir(), &fakeLister{})

	_, err := r.Resolve(context.Background(), ".", []string{"go", "run", "./cmd/foo"}, nil)
	if err == nil {
		t.Fatal("expected error when called without a declared tool (ADR-0005: no auto-dispatch)")
	}
}

func TestResolver_FailsOnDeclarationWithoutEntry(t *testing.T) {
	r := golocal.New(t.TempDir(), &fakeLister{})

	_, err := r.Resolve(context.Background(), ".", []string{"bin/foo"},
		&toolresolver.DeclaredTool{Resolver: "go-local"})
	if err == nil {
		t.Fatal("expected error when declared.Entry is empty")
	}
}

func TestResolver_FailsOnDeclaredEntryWithoutLeadingDotSlash(t *testing.T) {
	r := golocal.New(t.TempDir(), &fakeLister{})

	_, err := r.Resolve(context.Background(), ".", []string{"bin/foo"},
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "cmd/foo"})
	if err == nil {
		t.Fatal("expected error when declared.Entry lacks ./ prefix")
	}
}

func TestResolver_RebasesEntryToRepoRootForLister(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"spec/cmd/tool/main.go": "package main\nfunc main() {}\n",
	})
	stub := &fakeLister{listing: lister.Listing{InternalFiles: []string{"spec/cmd/tool/main.go"}}}

	// The spec lives under spec/, so the declared spec-relative entry "./cmd/tool"
	// must reach the lister as "./spec/cmd/tool" — otherwise packages.Load (running
	// at repoRoot) would mis-resolve to <repoRoot>/cmd/tool (which doesn't exist).
	versions, err := golocal.New(root, stub).Resolve(
		context.Background(), "spec", nil,
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/tool"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if stub.gotEntry != "./spec/cmd/tool" {
		t.Errorf("lister received entry %q, want ./spec/cmd/tool (rebased to repo root)", stub.gotEntry)
	}
	// The version label, however, stays in spec-relative form so the same generator
	// referenced from different specs keeps a stable display string.
	if !strings.HasPrefix(versions[0].Version, "go-local:./cmd/tool@sha256:") {
		t.Errorf("Version = %q, want spec-relative label", versions[0].Version)
	}
}

func TestResolver_RebasesNestedSpecDirEntry(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"a/b/cmd/tool/main.go": "package main\nfunc main() {}\n",
	})
	stub := &fakeLister{listing: lister.Listing{InternalFiles: []string{"a/b/cmd/tool/main.go"}}}

	if _, err := golocal.New(root, stub).Resolve(
		context.Background(), filepath.Join("a", "b"),
		nil, &toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/tool/..."},
	); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if stub.gotEntry != "./a/b/cmd/tool/..." {
		t.Errorf("lister received entry %q, want ./a/b/cmd/tool/...", stub.gotEntry)
	}
}

func TestResolver_PropagatesListerError(t *testing.T) {
	wantErr := errors.New("transitive load failed")
	stub := &fakeLister{err: wantErr}

	_, err := golocal.New(t.TempDir(), stub).Resolve(
		context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/foo"},
	)
	if err == nil {
		t.Fatal("expected lister error to propagate")
	}
	if !strings.Contains(err.Error(), "transitive load failed") {
		t.Errorf("error %q should wrap lister error", err)
	}
}

func TestResolver_HashChangesOnInternalFileEdit(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"cmd/foo/main.go": "package main\nfunc main() {}\n",
	})
	listing := lister.Listing{InternalFiles: []string{"cmd/foo/main.go"}}
	stub := &fakeLister{listing: listing}

	v1 := mustResolveVersion(t, root, stub, "./cmd/foo")

	if err := os.WriteFile(filepath.Join(root, "cmd", "foo", "main.go"),
		[]byte("package main\nfunc main() { _ = 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v2 := mustResolveVersion(t, root, stub, "./cmd/foo")

	if v1 == v2 {
		t.Errorf("internal file edit must change version, both runs returned %q", v1)
	}
}

func TestResolver_HashChangesOnExternalModuleVersionBump(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"cmd/foo/main.go": "package main\nfunc main() {}\n",
	})

	listingV1 := lister.Listing{
		InternalFiles: []string{"cmd/foo/main.go"},
		ExternalModules: []lister.ExternalModule{
			{Path: "example.com/dep", Version: "v1.0.0", GoSumLine: "example.com/dep v1.0.0 h1:aaa"},
		},
	}
	listingV2 := lister.Listing{
		InternalFiles: []string{"cmd/foo/main.go"},
		ExternalModules: []lister.ExternalModule{
			{Path: "example.com/dep", Version: "v1.0.1", GoSumLine: "example.com/dep v1.0.1 h1:bbb"},
		},
	}

	stub1 := &fakeLister{listing: listingV1}
	stub2 := &fakeLister{listing: listingV2}

	v1 := mustResolveVersion(t, root, stub1, "./cmd/foo")
	v2 := mustResolveVersion(t, root, stub2, "./cmd/foo")
	if v1 == v2 {
		t.Errorf("external module version bump must change version, both runs returned %q", v1)
	}
}

func TestResolver_HashIsDeterministic(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"cmd/foo/main.go":    "package main\n",
		"pkg/util/util.go":   "package util\n",
		"pkg/other/other.go": "package other\n",
	})

	stub := &fakeLister{listing: lister.Listing{
		InternalFiles: []string{
			"cmd/foo/main.go",
			"pkg/util/util.go",
			"pkg/other/other.go",
		},
		ExternalModules: []lister.ExternalModule{
			{Path: "example.com/b", Version: "v1.0.0", GoSumLine: "b-line"},
			{Path: "example.com/a", Version: "v2.0.0", GoSumLine: "a-line"},
		},
	}}

	v1 := mustResolveVersion(t, root, stub, "./cmd/foo")
	v2 := mustResolveVersion(t, root, stub, "./cmd/foo")
	if v1 != v2 {
		t.Errorf("expected deterministic version, got %q vs %q", v1, v2)
	}
}

// setupRepo writes the given relative path → contents mapping under a fresh
// temp directory and returns the directory path.
func setupRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, contents := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mustResolveVersion(t *testing.T, root string, l lister.SourceLister, entry string) string {
	t.Helper()
	versions, err := golocal.New(root, l).Resolve(
		context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: entry},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
	return versions[0].Version
}
