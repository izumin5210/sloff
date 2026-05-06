package toolresolver_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
)

type fakeResolver struct {
	name         string
	versions     []toolresolver.ToolVersion
	err          error
	calls        int
	lastDeclared *toolresolver.DeclaredTool
}

func (f *fakeResolver) Name() string { return f.name }
func (f *fakeResolver) Resolve(_ context.Context, _ string, _ []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
	f.calls++
	f.lastDeclared = declared
	return f.versions, f.err
}

func TestRegistry_DeclaredCallsNamedResolver(t *testing.T) {
	a := &fakeResolver{name: "a", versions: []toolresolver.ToolVersion{{Name: "a", Version: "vA"}}}
	b := &fakeResolver{name: "b", versions: []toolresolver.ToolVersion{{Name: "b", Version: "vB"}}}

	reg := toolresolver.NewRegistry()
	reg.Register(a)
	reg.Register(b)

	declared := []toolresolver.DeclaredTool{{Resolver: "b", Exec: []string{"the-exec"}, Extract: "the-extract"}}
	got, err := reg.Resolve(context.Background(), ".", []string{"x"}, declared)
	if err != nil {
		t.Fatal(err)
	}
	want := []toolresolver.ToolVersion{{Name: "b", Version: "vB"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
	if a.calls != 0 {
		t.Errorf("only the declared resolver should run, a was called %d times", a.calls)
	}
	if b.lastDeclared == nil {
		t.Fatal("declared not propagated to resolver")
	}
	if diff := cmp.Diff(&declared[0], b.lastDeclared); diff != "" {
		t.Errorf("declared not propagated correctly (-want +got):\n%s", diff)
	}
}

func TestRegistry_MultipleDeclaredConcatenateVersionsInOrder(t *testing.T) {
	a := &fakeResolver{name: "a", versions: []toolresolver.ToolVersion{{Name: "a", Version: "vA"}}}
	b := &fakeResolver{name: "b", versions: []toolresolver.ToolVersion{{Name: "b", Version: "vB"}}}

	reg := toolresolver.NewRegistry()
	reg.Register(a)
	reg.Register(b)

	got, err := reg.Resolve(context.Background(), ".", []string{"x"},
		[]toolresolver.DeclaredTool{{Resolver: "b"}, {Resolver: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	// The result preserves the spec's tools[] order, not the registration order.
	want := []toolresolver.ToolVersion{
		{Name: "b", Version: "vB"},
		{Name: "a", Version: "vA"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestRegistry_UnknownDeclaredResolverErrors(t *testing.T) {
	reg := toolresolver.NewRegistry()
	reg.Register(&fakeResolver{name: "a"})

	_, err := reg.Resolve(context.Background(), ".", []string{"x"},
		[]toolresolver.DeclaredTool{{Resolver: "missing"}})
	if err == nil {
		t.Fatal("expected error for unknown resolver")
	}
}

// TestRegistry_EmptyDeclaredErrors guards the contract that all callers go through
// spec validation (ADR-0004 D1), which requires a non-empty tools[] list. Reaching
// Resolve with no declared tools indicates a programmer error elsewhere; per
// ADR-0005 the registry has no auto-dispatch fallback to silently fill the gap.
func TestRegistry_EmptyDeclaredErrors(t *testing.T) {
	reg := toolresolver.NewRegistry()
	reg.Register(&fakeResolver{name: "a"})

	_, err := reg.Resolve(context.Background(), ".", []string{"x"}, nil)
	if err == nil {
		t.Fatal("expected error when declared tools[] is empty")
	}
}

func TestRegistry_PropagatesResolverError(t *testing.T) {
	want := errors.New("boom")
	reg := toolresolver.NewRegistry()
	reg.Register(&fakeResolver{name: "a", err: want})

	_, err := reg.Resolve(context.Background(), ".", []string{"x"},
		[]toolresolver.DeclaredTool{{Resolver: "a"}})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}
