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

func barrier(spec, name string, deps ...depgraph.TaskRef) depgraph.Task {
	return depgraph.Task{SpecRelpath: spec, Name: name, Barrier: true, DependsOn: deps}
}

// TestBuild_BarrierOrdersMembersBeforeConsumer locks the ADR-0017 barrier
// shape: a consumer depending only on the barrier must still be emitted after
// every barrier member, with the barrier node itself sitting between them.
func TestBuild_BarrierOrdersMembersBeforeConsumer(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "consumer", []string{"seed.txt"}, []string{"c.out"}, ref("", "gen-all")),
		barrier("", "gen-all", ref("", "gen-a"), ref("", "gen-b")),
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

// TestBuild_CycleThroughBarrierErrors locks that barrier nodes participate in
// cycle detection like any other node (ADR-0017 D3).
func TestBuild_CycleThroughBarrierErrors(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "A", []string{"a.in"}, []string{"a.out"}, ref("", "gen-all")),
		barrier("", "gen-all", ref("", "A")),
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

// TestBuild_PrioritisesDeepChainOverShallowSibling locks ADR-0020: when two
// independent tasks are ready in the same wave, the one that gates a longer
// chain of dependents is emitted first — even when it sorts LATER by
// (SpecRelpath, Name) — so a slot-limited runner starts the long pole early
// instead of starving it behind the shallow sibling.
func TestBuild_PrioritisesDeepChainOverShallowSibling(t *testing.T) {
	tasks := []depgraph.Task{
		// "a-shallow" is a sink (height 1) and sorts FIRST lexically.
		taskD("", "a-shallow", []string{"seed"}, []string{"shallow.out"}),
		// "z-es" gates z-es -> z-plugins -> z-node (height 3) and sorts LAST.
		taskD("", "z-es", []string{"seed"}, []string{"es.out"}),
		taskD("", "z-plugins", []string{"es.out"}, []string{"plugins.out"}, ref("", "z-es")),
		taskD("", "z-node", []string{"plugins.out"}, []string{"node.out"}, ref("", "z-plugins")),
	}
	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	// z-es (height 3) is emitted before a-shallow (height 1) despite sorting
	// later; the chain then drains in dependency order.
	want := []string{"z-es", "z-plugins", "a-shallow", "z-node"}
	if diff := cmp.Diff(want, names(got)); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestBuild_WideFanDoesNotStarveDeepChain models the real regression ADR-0020
// targets: a wide fan of shallow tasks (the buf-pothos-* wave, each a direct
// input to `generate`) alongside the start of a deep chain
// (buf-protoc-plugins-es -> build-protoc-plugins -> buf-custom-node -> generate).
// The chain start must precede every shallow sibling even though it sorts last
// lexically, so the runner schedules the long pole first.
func TestBuild_WideFanDoesNotStarveDeepChain(t *testing.T) {
	shallow := []string{"aaa", "bbb", "ccc", "ddd", "eee"}
	var tasks []depgraph.Task
	sinkDeps := make([]depgraph.TaskRef, 0, len(shallow)+1)
	for _, s := range shallow {
		tasks = append(tasks, taskD("", s, []string{"seed"}, []string{s + ".out"}))
		sinkDeps = append(sinkDeps, ref("", s))
	}
	tasks = append(
		tasks,
		taskD("", "zes", []string{"seed"}, []string{"zes.out"}),
		taskD("", "zplugins", []string{"zes.out"}, []string{"zplugins.out"}, ref("", "zes")),
		taskD("", "znode", []string{"zplugins.out"}, []string{"znode.out"}, ref("", "zplugins")),
	)
	sinkDeps = append(sinkDeps, ref("", "znode"))
	tasks = append(tasks, taskD("", "sink", []string{"znode.out"}, []string{"sink.out"}, sinkDeps...))

	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	order := names(got)
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	for _, s := range shallow {
		if pos["zes"] > pos[s] {
			t.Errorf("deep-chain start zes (pos %d) should precede shallow %q (pos %d)\norder: %v",
				pos["zes"], s, pos[s], order)
		}
	}
	// Topological sanity: producers still precede consumers.
	for _, pair := range [][2]string{{"zes", "zplugins"}, {"zplugins", "znode"}, {"znode", "sink"}} {
		if pos[pair[0]] > pos[pair[1]] {
			t.Errorf("%s must precede %s\norder: %v", pair[0], pair[1], order)
		}
	}
}

// TestBuild_EqualHeightFallsBackToKeyOrder locks that equal-height ready tasks
// keep the deterministic (SpecRelpath, Name) order — the priority tie-break must
// not perturb the stable order the old scheduler guaranteed.
func TestBuild_EqualHeightFallsBackToKeyOrder(t *testing.T) {
	// All four are sinks (height 1); order must be pure (spec, name).
	tasks := []depgraph.Task{
		task("z", "t", []string{"a"}, []string{"z.out"}),
		task("a", "t", []string{"b"}, []string{"a.out"}),
		task("m", "b", []string{"c"}, []string{"m2.out"}),
		task("m", "a", []string{"d"}, []string{"m1.out"}),
	}
	got, err := depgraph.Build(tasks)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a:t", "m:a", "m:b", "z:t"}
	if diff := cmp.Diff(want, names(got)); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestBuild_PriorityDeterministic guards R2 (deterministic): the same task set
// yields byte-identical order across repeated builds regardless of the input
// slice order the priority sort receives.
func TestBuild_PriorityDeterministic(t *testing.T) {
	build := func() []string {
		tasks := []depgraph.Task{
			taskD("", "sink", []string{"n.out"}, []string{"s.out"}, ref("", "node"), ref("", "wide1"), ref("", "wide2")),
			taskD("", "wide2", []string{"seed"}, []string{"w2.out"}),
			taskD("", "wide1", []string{"seed"}, []string{"w1.out"}),
			taskD("", "node", []string{"e.out"}, []string{"n.out"}, ref("", "edge")),
			taskD("", "edge", []string{"seed"}, []string{"e.out"}),
		}
		got, err := depgraph.Build(tasks)
		if err != nil {
			t.Fatal(err)
		}
		return names(got)
	}
	first := build()
	for i := range 5 {
		if diff := cmp.Diff(first, build()); diff != "" {
			t.Fatalf("non-deterministic order on run %d (-first +got):\n%s", i, diff)
		}
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
