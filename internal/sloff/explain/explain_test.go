package explain_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
	"github.com/izumin5210/sloff/internal/sloff/explain"
)

func task(spec, name string, in, out []string) depgraph.Task {
	return depgraph.Task{SpecRelpath: spec, Name: name, Inputs: in, Outputs: out}
}

func taskD(spec, name string, in, out []string, deps ...depgraph.TaskRef) depgraph.Task {
	return depgraph.Task{SpecRelpath: spec, Name: name, Inputs: in, Outputs: out, DependsOn: deps}
}

func dref(spec, name string) depgraph.TaskRef {
	return depgraph.TaskRef{SpecRelpath: spec, Name: name}
}

func TestEdges_NoTasksReturnsNil(t *testing.T) {
	if got := explain.Edges(nil); got != nil {
		t.Errorf("expected nil edges, got %v", got)
	}
}

func TestEdges_SingleEdgeCarriesIntersectionFile(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("svc", "consumer", []string{"shared.pb.go"}, []string{"out.go"}, dref("svc", "producer")),
		task("svc", "producer", []string{"x.proto"}, []string{"shared.pb.go"}),
	}
	got := explain.Edges(tasks)
	want := []explain.Edge{
		{
			From:  explain.TaskRef{SpecRelpath: "svc", Name: "producer"},
			To:    explain.TaskRef{SpecRelpath: "svc", Name: "consumer"},
			Files: []string{"shared.pb.go"},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestEdges_MultipleJustifyingFilesDeduplicateAndSort guards the edge
// projection against silently dropping evidence when two of A's outputs
// also appear in B's inputs. Files end up sorted ascending so renderers
// can pick a deterministic "sample".
func TestEdges_MultipleJustifyingFilesDeduplicateAndSort(t *testing.T) {
	tasks := []depgraph.Task{
		task("", "A", []string{"src.proto"}, []string{"b.pb.go", "a.pb.go"}),
		taskD("", "B", []string{"a.pb.go", "b.pb.go", "other.in"}, []string{"final.go"}, dref("", "A")),
	}
	got := explain.Edges(tasks)
	want := []explain.Edge{
		{
			From:  explain.TaskRef{Name: "A"},
			To:    explain.TaskRef{Name: "B"},
			Files: []string{"a.pb.go", "b.pb.go"},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestEdges_DiamondOrdersByToThenFrom locks the deterministic edge ordering
// the renderers rely on: edges are sorted by To then From, so a diamond
// where two producers feed one consumer always renders the producers in
// stable lexical order regardless of input slice order.
func TestEdges_DiamondOrdersByToThenFrom(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("", "C", []string{"b.out", "a.out"}, []string{"c.out"}, dref("", "B"), dref("", "A")),
		task("", "B", []string{"shared.in"}, []string{"b.out"}),
		task("", "A", []string{"shared.in"}, []string{"a.out"}),
	}
	got := explain.Edges(tasks)
	if len(got) != 2 {
		t.Fatalf("expected 2 edges, got %d: %#v", len(got), got)
	}
	if got[0].From.Name != "A" || got[0].To.Name != "C" {
		t.Errorf("first edge should be A->C, got %s->%s", got[0].From.Name, got[0].To.Name)
	}
	if got[1].From.Name != "B" || got[1].To.Name != "C" {
		t.Errorf("second edge should be B->C, got %s->%s", got[1].From.Name, got[1].To.Name)
	}
}

// TestEdges_DeclaredEdgeWithoutOverlapHasEmptyFiles is the clean-checkout
// projection: the edge is declared in the spec but no generated file exists
// yet, so the evidence list is empty and renderers fall back to the
// "(declared)" caption.
func TestEdges_DeclaredEdgeWithoutOverlapHasEmptyFiles(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("svc", "producer", []string{"svc/x.proto"}, nil),
		taskD("svc", "consumer", []string{"svc/y.in"}, []string{"svc/out.go"}, dref("svc", "producer")),
	}
	got := explain.Edges(tasks)
	want := []explain.Edge{
		{
			From: explain.TaskRef{SpecRelpath: "svc", Name: "producer"},
			To:   explain.TaskRef{SpecRelpath: "svc", Name: "consumer"},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestEdges_OverlapWithoutDeclaredEdgeProducesNoEdge locks the inverse: file
// overlap alone is validation territory (FindMissingDependencies), never a
// rendered edge.
func TestEdges_OverlapWithoutDeclaredEdgeProducesNoEdge(t *testing.T) {
	tasks := []depgraph.Task{
		task("svc", "consumer", []string{"shared.pb.go"}, []string{"out.go"}),
		task("svc", "producer", []string{"x.proto"}, []string{"shared.pb.go"}),
	}
	if got := explain.Edges(tasks); len(got) != 0 {
		t.Errorf("expected no edges, got %v", got)
	}
}

func TestEdge_LabelSampleEmptyFilesYieldsDeclared(t *testing.T) {
	e := explain.Edge{}
	if got := e.LabelSample(); got != "(declared)" {
		t.Errorf("expected (declared), got %q", got)
	}
}

func TestEdge_LabelSampleSingleFileNotAnnotated(t *testing.T) {
	e := explain.Edge{Files: []string{"a.pb.go"}}
	if got := e.LabelSample(); got != "a.pb.go" {
		t.Errorf("expected bare file, got %q", got)
	}
}

// TestEdge_LabelSampleMultipleFilesAnnotatedWithRemainder is the literal
// rendering rule announced to users: one filename plus "(+N more)" so wide
// graphs stay readable while still telling the user how many other files
// justify the edge.
func TestEdge_LabelSampleMultipleFilesAnnotatedWithRemainder(t *testing.T) {
	e := explain.Edge{Files: []string{"a.pb.go", "b.pb.go", "c.pb.go"}}
	got := e.LabelSample()
	if got != "a.pb.go (+2 more)" {
		t.Errorf("unexpected sample: %q", got)
	}
}

// TestRenderMermaid_NodeAndEdgeOrderDeterministic locks the byte-exact
// rendering used by the graph subcommand goldens. Slug-style node IDs come
// from each task label with non-alphanumerics collapsed to "_"; edges use a
// "first-file (+N more)" caption.
func TestRenderMermaid_NodeAndEdgeOrderDeterministic(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("svc", "consumer", []string{"shared.pb.go"}, []string{"out.go"}, dref("svc", "producer")),
		task("svc", "producer", []string{"x.proto"}, []string{"shared.pb.go"}),
	}
	edges := explain.Edges(tasks)
	got := explain.RenderMermaid(tasks, edges)
	want := strings.Join([]string{
		"flowchart TD",
		`    n_svc_consumer["svc:consumer"]`,
		`    n_svc_producer["svc:producer"]`,
		`    n_svc_producer -->|"shared.pb.go"| n_svc_consumer`,
		"",
	}, "\n")
	if got != want {
		t.Errorf("mermaid mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestRenderDOT_QuotedLabelsMatchMermaidOrdering keeps DOT in lockstep with
// the mermaid renderer's node/edge ordering, so users swapping --format see
// the same logical graph.
func TestRenderDOT_QuotedLabelsMatchMermaidOrdering(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("svc", "consumer", []string{"shared.pb.go"}, []string{"out.go"}, dref("svc", "producer")),
		task("svc", "producer", []string{"x.proto"}, []string{"shared.pb.go"}),
	}
	edges := explain.Edges(tasks)
	got := explain.RenderDOT(tasks, edges)
	want := strings.Join([]string{
		"digraph sloff {",
		"    rankdir=TB;",
		"    node [shape=box];",
		`    "svc:consumer";`,
		`    "svc:producer";`,
		`    "svc:producer" -> "svc:consumer" [label="shared.pb.go"];`,
		"}",
		"",
	}, "\n")
	if got != want {
		t.Errorf("dot mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestRenderMermaid_EmptyTaskListEmitsHeaderOnly: an empty repo or a graph
// requested before any spec is authored should still produce a parseable
// (if trivial) flowchart, not an error.
func TestRenderMermaid_EmptyTaskListEmitsHeaderOnly(t *testing.T) {
	got := explain.RenderMermaid(nil, nil)
	if got != "flowchart TD\n" {
		t.Errorf("unexpected: %q", got)
	}
}

func groupTask(spec, name string, deps ...depgraph.TaskRef) depgraph.Task {
	return depgraph.Task{SpecRelpath: spec, Name: name, Group: true, DependsOn: deps}
}

// TestRenderMermaid_GroupNodeRendersAsHexagon locks ADR-0016 D6: group nodes
// use the {{...}} hexagon shape so an aggregation point is visually distinct
// from an executing task, and its edges (no outputs, no inputs) render with
// the "(declared)" caption.
func TestRenderMermaid_GroupNodeRendersAsHexagon(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("svc", "consumer", []string{"seed.txt"}, []string{"out.go"}, dref("svc", "gen-all")),
		groupTask("svc", "gen-all", dref("svc", "producer")),
		task("svc", "producer", []string{"x.proto"}, []string{"shared.pb.go"}),
	}
	edges := explain.Edges(tasks)
	got := explain.RenderMermaid(tasks, edges)
	want := strings.Join([]string{
		"flowchart TD",
		`    n_svc_consumer["svc:consumer"]`,
		`    n_svc_gen_all{{"svc:gen-all"}}`,
		`    n_svc_producer["svc:producer"]`,
		`    n_svc_gen_all -->|"(declared)"| n_svc_consumer`,
		`    n_svc_producer -->|"(declared)"| n_svc_gen_all`,
		"",
	}, "\n")
	if got != want {
		t.Errorf("mermaid mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestRenderDOT_GroupNodeRendersAsHexagon is the DOT counterpart: the group
// node overrides the box default with shape=hexagon.
func TestRenderDOT_GroupNodeRendersAsHexagon(t *testing.T) {
	tasks := []depgraph.Task{
		taskD("svc", "consumer", []string{"seed.txt"}, []string{"out.go"}, dref("svc", "gen-all")),
		groupTask("svc", "gen-all", dref("svc", "producer")),
		task("svc", "producer", []string{"x.proto"}, []string{"shared.pb.go"}),
	}
	edges := explain.Edges(tasks)
	got := explain.RenderDOT(tasks, edges)
	want := strings.Join([]string{
		"digraph sloff {",
		"    rankdir=TB;",
		"    node [shape=box];",
		`    "svc:consumer";`,
		`    "svc:gen-all" [shape=hexagon];`,
		`    "svc:producer";`,
		`    "svc:gen-all" -> "svc:consumer" [label="(declared)"];`,
		`    "svc:producer" -> "svc:gen-all" [label="(declared)"];`,
		"}",
		"",
	}, "\n")
	if got != want {
		t.Errorf("dot mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}
