package preflight_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/preflight"
)

type fakeChecker struct {
	name   string
	issues []preflight.Issue
	err    error
	calls  int
}

func (f *fakeChecker) Name() string { return f.name }
func (f *fakeChecker) Check(_ context.Context, _ string) (preflight.Result, error) {
	f.calls++
	return preflight.Result{OK: len(f.issues) == 0, Issues: f.issues}, f.err
}

func TestRegistry_RunsOnlyNamedCheckers(t *testing.T) {
	a := &fakeChecker{name: "a"}
	b := &fakeChecker{name: "b"}
	reg := preflight.NewRegistry()
	reg.Register(a)
	reg.Register(b)

	if _, err := reg.Run(context.Background(), ".", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 {
		t.Errorf("a.calls = %d, want 1", a.calls)
	}
	if b.calls != 0 {
		t.Errorf("b.calls = %d, want 0", b.calls)
	}
}

func TestRegistry_AggregatesIssuesAcrossCheckers(t *testing.T) {
	a := &fakeChecker{name: "a", issues: []preflight.Issue{{Channel: "a", Detail: "stale"}}}
	b := &fakeChecker{name: "b", issues: []preflight.Issue{{Channel: "b", Detail: "missing"}}}
	reg := preflight.NewRegistry()
	reg.Register(a)
	reg.Register(b)

	got, err := reg.Run(context.Background(), ".", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if got.OK {
		t.Error("expected OK=false")
	}
	want := []preflight.Issue{
		{Channel: "a", Detail: "stale"},
		{Channel: "b", Detail: "missing"},
	}
	if diff := cmp.Diff(want, got.Issues); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestRegistry_AllOK(t *testing.T) {
	reg := preflight.NewRegistry()
	reg.Register(&fakeChecker{name: "a"})
	reg.Register(&fakeChecker{name: "b"})

	got, err := reg.Run(context.Background(), ".", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.OK {
		t.Errorf("expected OK=true, got %+v", got)
	}
}

func TestRegistry_UnknownNameErrors(t *testing.T) {
	reg := preflight.NewRegistry()
	reg.Register(&fakeChecker{name: "a"})
	if _, err := reg.Run(context.Background(), ".", []string{"missing"}); err == nil {
		t.Fatal("expected error for unknown checker")
	}
}

func TestRegistry_HardErrorAborts(t *testing.T) {
	want := errors.New("io error")
	reg := preflight.NewRegistry()
	reg.Register(&fakeChecker{name: "a", err: want})
	reg.Register(&fakeChecker{name: "b"})

	_, err := reg.Run(context.Background(), ".", []string{"a", "b"})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

func TestRegistry_DedupesNames(t *testing.T) {
	a := &fakeChecker{name: "a"}
	reg := preflight.NewRegistry()
	reg.Register(a)

	if _, err := reg.Run(context.Background(), ".", []string{"a", "a"}); err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 {
		t.Errorf("a.calls = %d, want 1 (dedup expected)", a.calls)
	}
}
