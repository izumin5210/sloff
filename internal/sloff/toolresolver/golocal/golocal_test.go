package golocal_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/golocal"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
)

// fakeLister stubs SourceLister with a fixed Listing per call. Tests use it
// to keep the resolver decoupled from packages.Load while still exercising
// both contribution channels (ExtraInputs from internal files, Versions
// from external modules).
type fakeLister struct {
	gotSpecDir string
	gotEntry   string
	calls      int
	listing    lister.Listing
	err        error
}

func (f *fakeLister) List(_ context.Context, specDir, entry string) (lister.Listing, error) {
	f.gotSpecDir = specDir
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

// TestResolver_PassesEntryToLister exercises the declared-only dispatch
// (ADR-0005): the cmd shape is irrelevant; the resolver hands declared.Entry
// straight to the lister.
func TestResolver_PassesEntryToLister(t *testing.T) {
	stub := &fakeLister{listing: lister.Listing{InternalFiles: []string{"cmd/bar/main.go"}}}

	if _, err := golocal.New(t.TempDir(), stub).Inputs(
		context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/bar"},
	); err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	if stub.gotEntry != "./cmd/bar" {
		t.Errorf("lister received entry %q, want ./cmd/bar", stub.gotEntry)
	}
}

func TestResolver_FailsWithoutDeclaration(t *testing.T) {
	r := golocal.New(t.TempDir(), &fakeLister{})

	if _, err := r.Inputs(context.Background(), ".", nil); err == nil {
		t.Error("Inputs: expected error when called without a declared tool (ADR-0005: no auto-dispatch)")
	}
	if _, err := r.Versions(context.Background(), ".", nil); err == nil {
		t.Error("Versions: expected error when called without a declared tool (ADR-0005: no auto-dispatch)")
	}
}

func TestResolver_FailsOnDeclarationWithoutEntry(t *testing.T) {
	r := golocal.New(t.TempDir(), &fakeLister{})
	declared := &toolresolver.DeclaredTool{Resolver: "go-local"}

	if _, err := r.Inputs(context.Background(), ".", declared); err == nil {
		t.Error("Inputs: expected error when declared.Entry is empty")
	}
	if _, err := r.Versions(context.Background(), ".", declared); err == nil {
		t.Error("Versions: expected error when declared.Entry is empty")
	}
}

func TestResolver_FailsOnDeclaredEntryWithoutLeadingDotSlash(t *testing.T) {
	r := golocal.New(t.TempDir(), &fakeLister{})
	declared := &toolresolver.DeclaredTool{Resolver: "go-local", Entry: "cmd/foo"}

	if _, err := r.Inputs(context.Background(), ".", declared); err == nil {
		t.Error("Inputs: expected error when declared.Entry lacks ./ prefix")
	}
	if _, err := r.Versions(context.Background(), ".", declared); err == nil {
		t.Error("Versions: expected error when declared.Entry lacks ./ prefix")
	}
}

// TestResolver_AcceptsDotEntry guards `go-local: .` (a generator whose main
// package is the spec directory itself, invoked as `go run .`). Without this
// fixture, that common pattern would be silently unrepresentable.
func TestResolver_AcceptsDotEntry(t *testing.T) {
	stub := &fakeLister{listing: lister.Listing{InternalFiles: []string{"main.go"}}}

	got, err := golocal.New(t.TempDir(), stub).Inputs(
		context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "."},
	)
	if err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	if stub.gotEntry != "." {
		t.Errorf("lister received entry %q, want %q", stub.gotEntry, ".")
	}
	if diff := cmp.Diff([]string{"main.go"}, got); diff != "" {
		t.Errorf("Inputs (-want +got):\n%s", diff)
	}
}

// TestResolver_AcceptsParentRelativeEntry guards `tools: [{go-local: ../cmd/gen}]`
// for nested specs that invoke their generator from a parent directory. The
// entry passes through to the lister verbatim so the lister anchors at the
// spec dir.
func TestResolver_AcceptsParentRelativeEntry(t *testing.T) {
	stub := &fakeLister{listing: lister.Listing{InternalFiles: []string{"cmd/gen/main.go"}}}

	if _, err := golocal.New(t.TempDir(), stub).Inputs(
		context.Background(), filepath.Join("specs", "sub"),
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "../../cmd/gen"},
	); err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	if stub.gotSpecDir != filepath.Join("specs", "sub") {
		t.Errorf("lister received specDir %q, want %q", stub.gotSpecDir, filepath.Join("specs", "sub"))
	}
	if stub.gotEntry != "../../cmd/gen" {
		t.Errorf("lister received entry %q, want ../../cmd/gen (no rewriting)", stub.gotEntry)
	}
}

// TestResolver_PropagatesNestedSpecDir guards that the spec directory reaches
// the lister verbatim so the lister can run packages.Load inside the spec's
// working module (which is what makes nested-module monorepos work).
func TestResolver_PropagatesNestedSpecDir(t *testing.T) {
	stub := &fakeLister{listing: lister.Listing{InternalFiles: []string{"a/b/cmd/tool/main.go"}}}

	if _, err := golocal.New(t.TempDir(), stub).Inputs(
		context.Background(), filepath.Join("a", "b"),
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/tool/..."},
	); err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	if stub.gotSpecDir != filepath.Join("a", "b") {
		t.Errorf("lister received specDir %q, want %q", stub.gotSpecDir, filepath.Join("a", "b"))
	}
	if stub.gotEntry != "./cmd/tool/..." {
		t.Errorf("lister received entry %q, want ./cmd/tool/...", stub.gotEntry)
	}
}

func TestResolver_PropagatesListerError(t *testing.T) {
	wantErr := errors.New("transitive load failed")
	stub := &fakeLister{err: wantErr}

	if _, err := golocal.New(t.TempDir(), stub).Inputs(
		context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/foo"},
	); err == nil || !strings.Contains(err.Error(), "transitive load failed") {
		t.Errorf("expected lister error to propagate via Inputs, got: %v", err)
	}
	if _, err := golocal.New(t.TempDir(), stub).Versions(
		context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/foo"},
	); err == nil || !strings.Contains(err.Error(), "transitive load failed") {
		t.Errorf("expected lister error to propagate via Versions, got: %v", err)
	}
}

// TestResolver_InternalFilesBecomeExtraInputs is the contract test for the
// input-contributor side: lister.Listing.InternalFiles surfaces verbatim in
// Inputs so the runner folds them into files_hash and depgraph can wire
// upstream codegen tasks via output overlap.
func TestResolver_InternalFilesBecomeExtraInputs(t *testing.T) {
	stub := &fakeLister{listing: lister.Listing{
		InternalFiles: []string{"cmd/foo/main.go", "pkg/util/util.go"},
	}}

	r := golocal.New(t.TempDir(), stub)
	got, err := r.Inputs(
		context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/foo"},
	)
	if err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	want := []string{"cmd/foo/main.go", "pkg/util/util.go"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Inputs (-want +got):\n%s", diff)
	}
	versions, err := r.Versions(
		context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/foo"},
	)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("listing without ExternalModules must yield no Versions, got %+v", versions)
	}
}

// TestResolver_ExternalModulesBecomeGoDepsVersions is the contract test for
// the resolved_versions_hash side: each external module surfaces as one ResolvedVersion of
// the canonical form `go-deps:<path>@<version>+sum:<digest>`. Without the
// per-module emission, dep bumps would not invalidate resolved_versions_hash and stale
// runs could leak through.
func TestResolver_ExternalModulesBecomeGoDepsVersions(t *testing.T) {
	stub := &fakeLister{listing: lister.Listing{
		ExternalModules: []lister.ExternalModule{
			{Path: "example.com/dep", Version: "v1.0.0", GoSumLine: "example.com/dep v1.0.0 h1:aaa"},
			{Path: "example.com/other", Version: "v2.0.0", GoSumLine: "example.com/other v2.0.0 h1:bbb"},
		},
	}}

	got, err := golocal.New(t.TempDir(), stub).Versions(
		context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/foo"},
	)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(Versions) = %d, want 2: %+v", len(got), got)
	}
	for _, v := range got {
		if !strings.HasPrefix(v.Version, "go-deps:example.com/") {
			t.Errorf("Version = %q, want go-deps:example.com/... prefix", v.Version)
		}
		if !strings.Contains(v.Version, "+sum:") {
			t.Errorf("Version = %q must include go.sum digest suffix", v.Version)
		}
	}
}

// TestResolver_GoSumDriftFlipsDepsVersion guards the cryptographic-anchor
// invariant: same path@version with a different go.sum line must produce a
// different ResolvedVersion string (otherwise replaced/republished modules would
// silently reuse the cache).
func TestResolver_GoSumDriftFlipsDepsVersion(t *testing.T) {
	listing := func(sum string) lister.Listing {
		return lister.Listing{
			ExternalModules: []lister.ExternalModule{
				{Path: "example.com/dep", Version: "v1.0.0", GoSumLine: sum},
			},
		}
	}

	v1 := mustVersion(t, &fakeLister{listing: listing("example.com/dep v1.0.0 h1:aaa")})
	v2 := mustVersion(t, &fakeLister{listing: listing("example.com/dep v1.0.0 h1:bbb")})
	if v1 == v2 {
		t.Errorf("go.sum drift must change version, both returned %q", v1)
	}
}

// TestResolver_InputsAndVersionsShareListing locks the IZU-16 caching
// contract: when the resolver is wrapped in lister.NewMemoized (the
// production wiring), calling Inputs and Versions for the same declared
// tool causes packages.Load (the lister) to run exactly once. Without
// memoisation, splitting the methods would double the cost.
func TestResolver_InputsAndVersionsShareListing(t *testing.T) {
	stub := &fakeLister{listing: lister.Listing{
		InternalFiles:   []string{"cmd/foo/main.go"},
		ExternalModules: []lister.ExternalModule{{Path: "example.com/x", Version: "v1.0.0"}},
	}}
	memo := lister.NewMemoized(stub)
	r := golocal.New(t.TempDir(), memo)
	declared := &toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/foo"}

	if _, err := r.Inputs(context.Background(), ".", declared); err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	if _, err := r.Versions(context.Background(), ".", declared); err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("memoised lister should be called once across Inputs+Versions, got %d", stub.calls)
	}
}

func mustVersion(t *testing.T, stub *fakeLister) string {
	t.Helper()
	got, err := golocal.New(t.TempDir(), stub).Versions(
		context.Background(), ".",
		&toolresolver.DeclaredTool{Resolver: "go-local", Entry: "./cmd/foo"},
	)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(Versions) = %d, want 1", len(got))
	}
	return got[0].Version
}
