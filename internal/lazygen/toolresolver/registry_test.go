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
	canResolve   bool
	versions     []toolresolver.ToolVersion
	err          error
	calls        int
	lastDeclared *toolresolver.DeclaredTool
}

func (f *fakeResolver) Name() string                     { return f.name }
func (f *fakeResolver) CanResolve(string, []string) bool { return f.canResolve }
func (f *fakeResolver) Resolve(_ context.Context, _ string, _ []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
	f.calls++
	f.lastDeclared = declared
	return f.versions, f.err
}

func TestRegistry_AutoDispatchPicksFirstCanResolve(t *testing.T) {
	a := &fakeResolver{name: "a", canResolve: false}
	b := &fakeResolver{name: "b", canResolve: true, versions: []toolresolver.ToolVersion{{Name: "b", Version: "v1"}}}
	c := &fakeResolver{name: "c", canResolve: true, versions: []toolresolver.ToolVersion{{Name: "c", Version: "vC"}}}

	reg := toolresolver.NewRegistry()
	reg.Register(a)
	reg.Register(b)
	reg.Register(c)

	got, err := reg.Resolve(context.Background(), ".", []string{"x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []toolresolver.ToolVersion{{Name: "b", Version: "v1"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
	if c.calls != 0 {
		t.Errorf("dispatch should stop at the first match, c was called %d times", c.calls)
	}
}

func TestRegistry_DeclaredOverridesAutoDispatch(t *testing.T) {
	a := &fakeResolver{name: "a", canResolve: true, versions: []toolresolver.ToolVersion{{Name: "a", Version: "vA"}}}
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
		t.Errorf("declared should bypass auto-dispatch, a was called %d times", a.calls)
	}
	if b.lastDeclared == nil {
		t.Fatal("declared not propagated to resolver")
	}
	if diff := cmp.Diff(&declared[0], b.lastDeclared); diff != "" {
		t.Errorf("declared not propagated correctly (-want +got):\n%s", diff)
	}
}

func TestRegistry_MultipleDeclaredUnionsVersions(t *testing.T) {
	a := &fakeResolver{name: "a", versions: []toolresolver.ToolVersion{{Name: "a", Version: "vA"}}}
	b := &fakeResolver{name: "b", versions: []toolresolver.ToolVersion{{Name: "b", Version: "vB"}}}

	reg := toolresolver.NewRegistry()
	reg.Register(a)
	reg.Register(b)

	got, err := reg.Resolve(context.Background(), ".", []string{"x"},
		[]toolresolver.DeclaredTool{{Resolver: "a"}, {Resolver: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []toolresolver.ToolVersion{
		{Name: "a", Version: "vA"},
		{Name: "b", Version: "vB"},
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

func TestRegistry_NoResolverMatchedErrors(t *testing.T) {
	reg := toolresolver.NewRegistry()
	reg.Register(&fakeResolver{name: "a", canResolve: false})

	_, err := reg.Resolve(context.Background(), ".", []string{"unknown"}, nil)
	if err == nil {
		t.Fatal("expected error when no resolver matches and no tools were declared")
	}
}

func TestRegistry_PropagatesResolverError(t *testing.T) {
	want := errors.New("boom")
	reg := toolresolver.NewRegistry()
	reg.Register(&fakeResolver{name: "a", canResolve: true, err: want})

	_, err := reg.Resolve(context.Background(), ".", []string{"x"}, nil)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}
