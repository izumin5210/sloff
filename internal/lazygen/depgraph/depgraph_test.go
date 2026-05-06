package depgraph_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/depgraph"
)

func task(spec, name string, in, out []string) depgraph.Task {
	return depgraph.Task{SpecRelpath: spec, Name: name, Inputs: in, Outputs: out}
}

func names(ts []depgraph.Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		if t.SpecRelpath == "" {
			out = append(out, t.Name)
		} else {
			out = append(out, t.SpecRelpath+":"+t.Name)
		}
	}
	return out
}

func TestBuild_EmptyReturnsEmpty(t *testing.T) {
	got, err := depgraph.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", names(got))
	}
}

func TestBuild_NoDependenciesPreservesStableOrder(t *testing.T) {
	tasks := []depgraph.Task{
		task("z", "alpha", []string{"a.in"}, []string{"a.out"}),
		task("a", "beta", []string{"b.in"}, []string{"b.out"}),
		task("m", "gamma", []string{"c.in"}, []string{"c.out"}),
	}
	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a:beta", "m:gamma", "z:alpha"} // sorted by (spec, name)
	if diff := cmp.Diff(want, names(got)); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestBuild_BDependsOnAPlacesABeforeB(t *testing.T) {
	tasks := []depgraph.Task{
		task("", "B", []string{"shared.proto", "x.options.pb.go"}, []string{"x.pb.go"}),
		task("", "A", []string{"options.proto"}, []string{"x.options.pb.go"}),
	}
	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "B"}
	if diff := cmp.Diff(want, names(got)); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestBuild_DiamondRespectsTopologicalOrder(t *testing.T) {
	// A produces a.out; B and C consume a.out.
	tasks := []depgraph.Task{
		task("", "C", []string{"a.out"}, []string{"c.out"}),
		task("", "B", []string{"a.out"}, []string{"b.out"}),
		task("", "A", []string{"a.in"}, []string{"a.out"}),
	}
	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	got_names := names(got)
	if got_names[0] != "A" {
		t.Errorf("A must come first, got %v", got_names)
	}
	// B and C come after A in stable order
	want := []string{"A", "B", "C"}
	if diff := cmp.Diff(want, got_names); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestBuild_CycleErrors(t *testing.T) {
	tasks := []depgraph.Task{
		task("", "A", []string{"b.out"}, []string{"a.out"}),
		task("", "B", []string{"a.out"}, []string{"b.out"}),
	}
	_, err := depgraph.Build(tasks)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle, got %v", err)
	}
}

func TestBuild_DuplicateOutputProducersErrors(t *testing.T) {
	tasks := []depgraph.Task{
		task("svcA", "first", []string{"a.in"}, []string{"shared.out", "a.out"}),
		task("svcB", "second", []string{"b.in"}, []string{"shared.out"}),
		task("svcC", "third", []string{"c.in"}, []string{"shared.out", "other.out"}),
	}
	_, err := depgraph.Build(tasks)
	if err == nil {
		t.Fatal("expected error for duplicate output producers")
	}
	msg := err.Error()
	for _, want := range []string{"shared.out", "svcA:first", "svcB:second", "svcC:third"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestBuild_DependencyDetectedAcrossSpecDirs(t *testing.T) {
	tasks := []depgraph.Task{
		task("svcB", "consumer", []string{"shared/proto/x.pb.go"}, []string{"svcB/y.go"}),
		task("svcA", "producer", []string{"shared/proto/x.proto"}, []string{"shared/proto/x.pb.go"}),
	}
	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"svcA:producer", "svcB:consumer"}
	if diff := cmp.Diff(want, names(got)); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}
