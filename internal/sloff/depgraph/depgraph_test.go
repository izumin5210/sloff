package depgraph_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
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

func TestBuild_DeclaredDependencyOrdersProducerFirst(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "B", []string{"shared.proto", "x.options.pb.go"}, []string{"x.pb.go"}, ref("", "A")),
		taskD("", "A", []string{"options.proto"}, []string{"x.options.pb.go"}),
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

// TestBuild_OverlapWithoutDependsDoesNotOrder locks ADR-0013 D2: file overlap
// alone no longer creates edges. Ordering falls back to the stable
// (SpecRelpath, Name) sort; the undeclared overlap is FindMissingDependencies'
// concern, not Build's.
func TestBuild_OverlapWithoutDependsDoesNotOrder(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "consumer", []string{"mid.txt"}, []string{"out.txt"}),
		taskD("", "producer", []string{"in.txt"}, []string{"mid.txt"}),
	}
	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"consumer", "producer"} // plain stable order, no edge
	if diff := cmp.Diff(want, names(got)); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestBuild_JoinWaitsForAllDeclaredDependencies exercises a node with
// in-degree 2: the sink must stay blocked until both declared upstreams have
// been emitted (the cross-edge inDegree decrement path in Kahn's loop).
func TestBuild_JoinWaitsForAllDeclaredDependencies(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "join", []string{"a.out", "b.out"}, []string{"j.out"}, ref("", "A"), ref("", "B")),
		taskD("", "B", []string{"b.in"}, []string{"b.out"}),
		taskD("", "A", []string{"a.in"}, []string{"a.out"}),
	}
	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "B", "join"}
	if diff := cmp.Diff(want, names(got)); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestBuild_DiamondRespectsTopologicalOrder(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "C", []string{"a.out"}, []string{"c.out"}, ref("", "A")),
		taskD("", "B", []string{"a.out"}, []string{"b.out"}, ref("", "A")),
		taskD("", "A", []string{"a.in"}, []string{"a.out"}),
	}
	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "B", "C"}
	if diff := cmp.Diff(want, names(got)); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func group(spec, name string, deps ...depgraph.TaskRef) depgraph.Task {
	return depgraph.Task{SpecRelpath: spec, Name: name, Group: true, DependsOn: deps}
}

// TestBuild_GroupOrdersMembersBeforeConsumer locks the ADR-0017 barrier
// shape: a consumer depending only on the group must still be emitted after
// every group member, with the group node itself sitting between them.
func TestBuild_GroupOrdersMembersBeforeConsumer(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "consumer", []string{"seed.txt"}, []string{"c.out"}, ref("", "gen-all")),
		group("", "gen-all", ref("", "gen-a"), ref("", "gen-b")),
		taskD("", "gen-b", []string{"seed.txt"}, []string{"b.out"}),
		taskD("", "gen-a", []string{"seed.txt"}, []string{"a.out"}),
	}
	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gen-a", "gen-b", "gen-all", "consumer"}
	if diff := cmp.Diff(want, names(got)); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestBuild_CycleThroughGroupErrors locks that group nodes participate in
// cycle detection like any other node (ADR-0017 D3).
func TestBuild_CycleThroughGroupErrors(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "A", []string{"a.in"}, []string{"a.out"}, ref("", "gen-all")),
		group("", "gen-all", ref("", "A")),
	}
	_, err := depgraph.Build(tasks)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle, got %v", err)
	}
}

func TestBuild_CycleErrors(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "A", []string{"b.out"}, []string{"a.out"}, ref("", "B")),
		taskD("", "B", []string{"a.out"}, []string{"b.out"}, ref("", "A")),
	}
	_, err := depgraph.Build(tasks)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle, got %v", err)
	}
}

func TestBuild_DependencyDeclaredAcrossSpecDirs(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("svcB", "consumer", []string{"shared/proto/x.pb.go"}, []string{"svcB/y.go"}, ref("svcA", "producer")),
		taskD("svcA", "producer", []string{"shared/proto/x.proto"}, []string{"shared/proto/x.pb.go"}),
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

func TestBuild_UnknownDependencyErrors(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "B", []string{"x.in"}, []string{"x.out"}, ref("", "ghost")),
	}
	_, err := depgraph.Build(tasks)
	if err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Errorf("expected unknown-task error, got %v", err)
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
