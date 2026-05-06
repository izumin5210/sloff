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

func TestResolver_NameAndCanResolve(t *testing.T) {
	r := golocal.New(t.TempDir(), &fakeLister{})

	if r.Name() != "go-local" {
		t.Errorf("Name() = %q, want go-local", r.Name())
	}

	cases := []struct {
		cmd  []string
		want bool
	}{
		{[]string{"go", "run", "./cmd/foo/..."}, true},
		{[]string{"go", "run", "./cmd/foo"}, true},
		{[]string{"go", "run", "-tags", "dev", "./cmd/foo"}, true},
		{[]string{"go", "build", "./cmd/foo"}, false},
		{[]string{"buf", "generate"}, false},
		{[]string{"go", "run"}, false},
		{[]string{"go", "run", "example.com/foo"}, false}, // module path, not relative
	}
	for _, c := range cases {
		if got := r.CanResolve("", c.cmd); got != c.want {
			t.Errorf("CanResolve(%v) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestResolver_AutoDispatchExtractsEntryFromGoRun(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"cmd/foo/main.go": "package main\nfunc main() {}\n",
	})
	stub := &fakeLister{listing: lister.Listing{InternalFiles: []string{"cmd/foo/main.go"}}}

	versions, err := golocal.New(root, stub).Resolve(
		context.Background(), ".", []string{"go", "run", "./cmd/foo/..."}, nil,
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if stub.gotEntry != "./cmd/foo/..." {
		t.Errorf("lister received entry %q, want ./cmd/foo/...", stub.gotEntry)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
	v := versions[0]
	if v.Name != "./cmd/foo/..." {
		t.Errorf("Name = %q, want ./cmd/foo/...", v.Name)
	}
	if v.Source != "go-local:./cmd/foo/..." {
		t.Errorf("Source = %q", v.Source)
	}
	if !strings.HasPrefix(v.Version, "go-local:./cmd/foo/...@sha256:") {
		t.Errorf("Version = %q", v.Version)
	}
}

func TestResolver_DeclaredEntryOverridesCmd(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"cmd/bar/main.go": "package main\nfunc main() {}\n",
	})
	stub := &fakeLister{listing: lister.Listing{InternalFiles: []string{"cmd/bar/main.go"}}}

	// cmd is a prebuilt binary invocation; declared entry is what wins.
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

func TestResolver_FailsWhenAutoDispatchedAgainstNonGoRunCmd(t *testing.T) {
	r := golocal.New(t.TempDir(), &fakeLister{})

	_, err := r.Resolve(context.Background(), ".", []string{"buf", "generate"}, nil)
	if err == nil {
		t.Fatal("expected error when cmd is not `go run ./...` and no declaration was supplied")
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

func TestResolver_PropagatesListerError(t *testing.T) {
	wantErr := errors.New("transitive load failed")
	stub := &fakeLister{err: wantErr}

	_, err := golocal.New(t.TempDir(), stub).Resolve(
		context.Background(), ".", []string{"go", "run", "./cmd/foo"}, nil,
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
