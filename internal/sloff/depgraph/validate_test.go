package depgraph_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
)

func ref(spec, name string) depgraph.TaskRef {
	return depgraph.TaskRef{SpecRelpath: spec, Name: name}
}

func taskD(spec, name string, in, out []string, deps ...depgraph.TaskRef) depgraph.Task {
	return depgraph.Task{SpecRelpath: spec, Name: name, Inputs: in, Outputs: out, DependsOn: deps}
}

func TestFindMissingDependencies_DetectsUndeclaredOverlap(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("proto/options", "gen", []string{"proto/options/x.proto"}, []string{"gen/a.pb.go", "gen/b.pb.go"}),
		taskD("proto/svc", "consume", []string{"gen/b.pb.go", "gen/a.pb.go", "proto/svc/y.proto"}, []string{"out/z.go"}),
	}
	got := depgraph.FindMissingDependencies(tasks)
	want := []depgraph.MissingDependency{
		{
			Producer: ref("proto/options", "gen"),
			Consumer: ref("proto/svc", "consume"),
			Files:    []string{"gen/a.pb.go", "gen/b.pb.go"},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestFindMissingDependencies_DeclaredEdgeSuppresses(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("a", "gen", []string{"a/x.in"}, []string{"shared.out"}),
		taskD("b", "consume", []string{"shared.out"}, []string{"b/y.out"}, ref("a", "gen")),
	}
	if got := depgraph.FindMissingDependencies(tasks); len(got) != 0 {
		t.Errorf("declared edge must suppress, got %v", got)
	}
}

func TestFindMissingDependencies_SelfOverlapIgnored(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("a", "iterative", []string{"a/out.go", "a/src.in"}, []string{"a/out.go"}),
	}
	if got := depgraph.FindMissingDependencies(tasks); len(got) != 0 {
		t.Errorf("self overlap must be ignored, got %v", got)
	}
}

func TestFindMissingDependencies_NoOverlapReturnsNil(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("a", "one", []string{"a/x.in"}, []string{"a/x.out"}),
		taskD("b", "two", []string{"b/y.in"}, []string{"b/y.out"}),
	}
	if got := depgraph.FindMissingDependencies(tasks); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestFormatMissing_SameDirSuggestsTaskOnly(t *testing.T) {
	m := depgraph.MissingDependency{
		Producer: ref("spec", "producer"),
		Consumer: ref("spec", "consumer"),
		Files:    []string{"spec/mid.txt"},
	}
	got := depgraph.FormatMissing(m)
	for _, want := range []string{
		"spec:consumer",
		"spec/mid.txt",
		"spec:producer",
		"`depends: [{task: producer}]`",
		"spec/sloff.yml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatMissing missing %q in: %s", want, got)
		}
	}
}

func TestFormatMissing_CrossDirSuggestsRelativeSpec(t *testing.T) {
	m := depgraph.MissingDependency{
		Producer: ref("proto/options", "gen"),
		Consumer: ref("proto/svc", "consume"),
		Files:    []string{"gen/a.pb.go", "gen/b.pb.go", "gen/c.pb.go"},
	}
	got := depgraph.FormatMissing(m)
	for _, want := range []string{
		"gen/a.pb.go (+2 more)",
		"`depends: [{spec: ../options, task: gen}]`",
		"proto/svc/sloff.yml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatMissing missing %q in: %s", want, got)
		}
	}
}

func TestTaskRefLabel_CollapsesRootQualifiers(t *testing.T) {
	if got := ref(".", "gen").Label(); got != "gen" {
		t.Errorf("dot spec must collapse, got %q", got)
	}
	if got := ref("", "gen").Label(); got != "gen" {
		t.Errorf("empty spec must collapse, got %q", got)
	}
	if got := ref("proto/svc", "gen").Label(); got != "proto/svc:gen" {
		t.Errorf("unexpected label %q", got)
	}
}
