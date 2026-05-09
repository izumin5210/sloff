package toolresolver_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
)

type fakeResolver struct {
	name         string
	versions     []toolresolver.ToolVersion
	extraInputs  []string
	versionsErr  error
	inputsErr    error
	versionCalls int
	inputsCalls  int
	lastDeclared *toolresolver.DeclaredTool
}

func (f *fakeResolver) Name() string { return f.name }

func (f *fakeResolver) Inputs(_ context.Context, _ string, declared *toolresolver.DeclaredTool) ([]string, error) {
	f.inputsCalls++
	f.lastDeclared = declared
	return f.extraInputs, f.inputsErr
}

func (f *fakeResolver) Versions(_ context.Context, _ string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
	f.versionCalls++
	f.lastDeclared = declared
	return f.versions, f.versionsErr
}

func TestRegistry_VersionsRoutesToNamedResolver(t *testing.T) {
	a := &fakeResolver{name: "a", versions: []toolresolver.ToolVersion{{Name: "a", Version: "vA"}}}
	b := &fakeResolver{name: "b", versions: []toolresolver.ToolVersion{{Name: "b", Version: "vB"}}}

	reg := toolresolver.NewRegistry()
	reg.Register(a)
	reg.Register(b)

	declared := []toolresolver.DeclaredTool{{Resolver: "b", Exec: []string{"the-exec"}, Extract: "the-extract"}}
	got, err := reg.Versions(context.Background(), ".", declared)
	if err != nil {
		t.Fatal(err)
	}
	want := []toolresolver.ToolVersion{{Name: "b", Version: "vB"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
	if a.versionCalls != 0 {
		t.Errorf("only the declared resolver should run, a Versions called %d times", a.versionCalls)
	}
	if b.lastDeclared == nil {
		t.Fatal("declared not propagated to resolver")
	}
	if diff := cmp.Diff(&declared[0], b.lastDeclared); diff != "" {
		t.Errorf("declared not propagated correctly (-want +got):\n%s", diff)
	}
}

// TestRegistry_InputsDoesNotInvokeVersions locks the IZU-16 split contract:
// a graph-style consumer that only asks for Inputs must never trigger the
// Versions method (which is where script's `<bin> --version` subprocess
// would otherwise spawn). Without this guarantee, `sloff graph` would still
// fail when prebuilt binaries are missing.
func TestRegistry_InputsDoesNotInvokeVersions(t *testing.T) {
	a := &fakeResolver{name: "a", extraInputs: []string{"some/file.ts"}}
	reg := toolresolver.NewRegistry()
	reg.Register(a)

	if _, err := reg.Inputs(context.Background(), ".", []toolresolver.DeclaredTool{{Resolver: "a"}}); err != nil {
		t.Fatal(err)
	}
	if a.inputsCalls != 1 {
		t.Errorf("Inputs should run exactly once, got %d", a.inputsCalls)
	}
	if a.versionCalls != 0 {
		t.Errorf("Versions must not be called from Inputs path, got %d", a.versionCalls)
	}
}

func TestRegistry_MultipleDeclaredConcatenateVersionsInOrder(t *testing.T) {
	a := &fakeResolver{name: "a", versions: []toolresolver.ToolVersion{{Name: "a", Version: "vA"}}}
	b := &fakeResolver{name: "b", versions: []toolresolver.ToolVersion{{Name: "b", Version: "vB"}}}

	reg := toolresolver.NewRegistry()
	reg.Register(a)
	reg.Register(b)

	got, err := reg.Versions(context.Background(), ".",
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

// TestRegistry_AggregatesExtraInputs guards that input contributions from
// multiple resolvers concatenate in declared order. The runner relies on this
// so depgraph sees every workspace-tool input regardless of which channels a
// task pulls from.
func TestRegistry_AggregatesExtraInputs(t *testing.T) {
	a := &fakeResolver{name: "a", extraInputs: []string{"alpha/file.ts"}}
	b := &fakeResolver{name: "b", extraInputs: []string{"bravo/file.ts", "bravo/lib.ts"}}

	reg := toolresolver.NewRegistry()
	reg.Register(a)
	reg.Register(b)

	got, err := reg.Inputs(context.Background(), ".",
		[]toolresolver.DeclaredTool{{Resolver: "a"}, {Resolver: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha/file.ts", "bravo/file.ts", "bravo/lib.ts"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ExtraInputs mismatch (-want +got):\n%s", diff)
	}
}

func TestRegistry_UnknownDeclaredResolverErrors(t *testing.T) {
	reg := toolresolver.NewRegistry()
	reg.Register(&fakeResolver{name: "a"})

	_, err := reg.Versions(context.Background(), ".",
		[]toolresolver.DeclaredTool{{Resolver: "missing"}})
	if err == nil {
		t.Fatal("expected error for unknown resolver")
	}
}

// TestRegistry_EmptyDeclaredErrors guards the contract that all callers go through
// spec validation (ADR-0004 D1), which requires a non-empty tools[] list. Reaching
// Inputs/Versions with no declared tools indicates a programmer error elsewhere;
// per ADR-0005 the registry has no auto-dispatch fallback to silently fill the gap.
func TestRegistry_EmptyDeclaredErrors(t *testing.T) {
	reg := toolresolver.NewRegistry()
	reg.Register(&fakeResolver{name: "a"})

	if _, err := reg.Versions(context.Background(), ".", nil); err == nil {
		t.Error("Versions: expected error when declared tools[] is empty")
	}
	if _, err := reg.Inputs(context.Background(), ".", nil); err == nil {
		t.Error("Inputs: expected error when declared tools[] is empty")
	}
}

func TestRegistry_PropagatesResolverError(t *testing.T) {
	want := errors.New("boom")
	reg := toolresolver.NewRegistry()
	reg.Register(&fakeResolver{name: "a", versionsErr: want})

	_, err := reg.Versions(context.Background(), ".",
		[]toolresolver.DeclaredTool{{Resolver: "a"}})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}
