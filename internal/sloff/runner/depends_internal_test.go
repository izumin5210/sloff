package runner

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
	"github.com/izumin5210/sloff/internal/sloff/spec"
)

func TestResolveDepends_JoinsRelativeSpecDirs(t *testing.T) {
	got := resolveDepends("proto/svc", []spec.Depend{
		{Task: "lint"},                               // same spec file
		{Spec: "../options", Task: "gen"},            // sibling dir
		{Spec: "../../tools/codegen", Task: "build"}, // deeper relative
	})
	want := []depgraph.TaskRef{
		{SpecRelpath: "proto/svc", Name: "lint"},
		{SpecRelpath: "proto/options", Name: "gen"},
		{SpecRelpath: "tools/codegen", Name: "build"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestResolveDepends_RootSpecDir(t *testing.T) {
	got := resolveDepends(".", []spec.Depend{{Task: "gen"}})
	want := []depgraph.TaskRef{{SpecRelpath: ".", Name: "gen"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestResolveDepends_EmptyReturnsNil(t *testing.T) {
	if got := resolveDepends("a", nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}
