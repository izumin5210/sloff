package aqua_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/aqua"
)

func TestParseConfig_BothFormsAccepted(t *testing.T) {
	in := []byte(`packages:
  - name: bufbuild/buf@v1.30.0
  - name: kyleconroy/sqlc
    version: v1.27.0
`)
	cfg, err := aqua.ParseConfig(in)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	want := []aqua.Package{
		{Name: "bufbuild/buf", Version: "v1.30.0"},
		{Name: "kyleconroy/sqlc", Version: "v1.27.0"},
	}
	if diff := cmp.Diff(want, cfg.Packages); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestParseConfig_RejectsMissingVersion(t *testing.T) {
	if _, err := aqua.ParseConfig([]byte("packages:\n  - name: foo/bar\n")); err == nil {
		t.Fatal("expected error when version is absent")
	}
}

func TestParseConfig_RejectsConflictingVersion(t *testing.T) {
	in := []byte(`packages:
  - name: foo/bar@v1
    version: v2
`)
	if _, err := aqua.ParseConfig(in); err == nil {
		t.Fatal("expected error when both inline and field versions are given")
	}
}

func TestResolver_CanResolveAndResolve_AutoDispatch(t *testing.T) {
	r := aqua.NewFromConfig(&aqua.Config{Packages: []aqua.Package{
		{Name: "bufbuild/buf", Version: "v1.30.0"},
	}})
	if r.Name() != "aqua" {
		t.Errorf("Name = %q", r.Name())
	}
	if !r.CanResolve(".", []string{"buf", "generate"}) {
		t.Error("CanResolve must be true for buf")
	}
	if r.CanResolve(".", []string{"unknown"}) {
		t.Error("CanResolve must be false for unknown")
	}

	got, err := r.Resolve(context.Background(), ".", []string{"buf", "generate"}, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []toolresolver.ToolVersion{
		{Name: "bufbuild/buf", Source: "aqua.yaml", Version: "aqua:bufbuild/buf@v1.30.0"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestResolver_Resolve_DeclaredKey(t *testing.T) {
	r := aqua.NewFromConfig(&aqua.Config{Packages: []aqua.Package{
		{Name: "bufbuild/buf", Version: "v1.30.0"},
		{Name: "kyleconroy/sqlc", Version: "v1.27.0"},
	}})

	got, err := r.Resolve(context.Background(), ".", []string{"some-script"}, "kyleconroy/sqlc")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []toolresolver.ToolVersion{
		{Name: "kyleconroy/sqlc", Source: "aqua.yaml", Version: "aqua:kyleconroy/sqlc@v1.27.0"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestResolver_Resolve_UnknownDeclaredKeyErrors(t *testing.T) {
	r := aqua.NewFromConfig(&aqua.Config{Packages: []aqua.Package{
		{Name: "bufbuild/buf", Version: "v1.30.0"},
	}})
	if _, err := r.Resolve(context.Background(), ".", []string{"x"}, "unknown/pkg"); err == nil {
		t.Fatal("expected error for unknown declared key")
	}
}

func TestResolver_Resolve_NoMatchInAutoDispatch(t *testing.T) {
	r := aqua.NewFromConfig(&aqua.Config{Packages: []aqua.Package{
		{Name: "bufbuild/buf", Version: "v1.30.0"},
	}})
	_, err := r.Resolve(context.Background(), ".", []string{"unknown"}, "")
	if err == nil {
		t.Fatal("expected error when no auto-dispatch match")
	}
}

func TestNew_LoadsAquaYAML(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "aqua.yaml"), `packages:
  - name: bufbuild/buf@v1.30.0
`)
	r, err := aqua.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !r.CanResolve(".", []string{"buf"}) {
		t.Error("expected loaded resolver to know buf")
	}
}

func TestNew_MissingAquaYAMLErrors(t *testing.T) {
	root := t.TempDir()
	_, err := aqua.New(root)
	if err == nil {
		t.Fatal("expected error when aqua.yaml is absent")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want wrap of os.ErrNotExist", err)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
