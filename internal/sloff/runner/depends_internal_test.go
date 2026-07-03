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

func TestTaskReadsPath_LiteralPatternMatchesCleanStatePath(t *testing.T) {
	set, joined := inputSurface("spec", []string{"a-out.txt"}, nil)
	info := &taskInfo{inputSet: set, joinedInputPatterns: joined}
	if !taskReadsPath(info, "spec/a-out.txt") {
		t.Error("expected literal pattern to match the produced path")
	}
}

func TestTaskReadsPath_GlobPatternMatches(t *testing.T) {
	set, joined := inputSurface("proto/svc", []string{"../../gen/**/*.pb.go"}, nil)
	info := &taskInfo{inputSet: set, joinedInputPatterns: joined}
	if !taskReadsPath(info, "gen/foo/bar.pb.go") {
		t.Error("expected glob pattern to match")
	}
	if taskReadsPath(info, "gen/foo/bar.txt") {
		t.Error("non-matching path must not match")
	}
}

func TestTaskReadsPath_ExpandedInputSetMatches(t *testing.T) {
	set, joined := inputSurface("spec", []string{"unrelated.txt"}, []string{"spec/extra-input.go"})
	info := &taskInfo{inputSet: set, joinedInputPatterns: joined}
	if !taskReadsPath(info, "spec/extra-input.go") {
		t.Error("expected expanded input set to match")
	}
}
