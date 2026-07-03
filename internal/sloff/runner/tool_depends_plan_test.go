package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
)

// Plan-level coverage for ADR-0019 D2 (edge injection). Resolution succeeds
// everywhere here (script tools), so these tests isolate the injection /
// rebasing / dedup rules from the deferred-resolution machinery.

// TestRunner_ToolDepends_InjectsEdgeIntoConsumers: a tool-level depends lands
// on every task referencing the tool — and only on those tasks.
func TestRunner_ToolDepends_InjectsEdgeIntoConsumers(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"gen/sloff.yml": `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  gen-tool:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
    depends:
      - task: gen-src
commands:
  - name: gen-src
    cmd: ["sh", "-c", "true"]
    inputs: ["src.in"]
    outputs: ["src.out"]
    tools: [versioner]
  - name: consume-a
    cmd: ["sh", "-c", "true"]
    inputs: ["a.in"]
    outputs: ["a.out"]
    tools: [gen-tool]
  - name: consume-b
    cmd: ["sh", "-c", "true"]
    inputs: ["b.in"]
    outputs: ["b.out"]
    tools: [gen-tool]
  - name: unrelated
    cmd: ["sh", "-c", "true"]
    inputs: ["c.in"]
    outputs: ["c.out"]
    tools: [versioner]
`,
		"gen/src.in": "s", "gen/a.in": "a", "gen/b.in": "b", "gen/c.in": "c",
	})

	tasks, _, err := newProviderRunner(t, workdir, specs).Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []depgraph.TaskRef{{SpecRelpath: "gen", Name: "gen-src"}}
	for _, consumer := range []string{"consume-a", "consume-b"} {
		if diff := cmp.Diff(want, dependsOfTask(t, tasks, "gen", consumer)); diff != "" {
			t.Errorf("%s DependsOn mismatch (-want +got):\n%s", consumer, diff)
		}
	}
	for _, unaffected := range []string{"gen-src", "unrelated"} {
		if got := dependsOfTask(t, tasks, "gen", unaffected); len(got) != 0 {
			t.Errorf("%s must not receive injected edges, got %v", unaffected, got)
		}
	}
}

// TestRunner_ToolDepends_CrossSpecPathRebasing: tool depends are declared
// relative to the tool-defining spec dir (ADR-0008 D3) and must resolve to
// the same target regardless of where the consumer lives — root spec tool →
// nested consumer and nested tool → root consumer both rebase correctly.
func TestRunner_ToolDepends_CrossSpecPathRebasing(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		// Root spec defines a tool whose producer lives in gen/.
		"sloff.yml": `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  root-tool:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
    depends:
      - {spec: gen, task: gen-src}
commands:
  - name: root-consume
    cmd: ["sh", "-c", "true"]
    inputs: ["root.in"]
    outputs: ["root.out"]
    tools: [deep-tool]
`,
		"root.in": "r",
		// Nested spec defines a tool whose producer is its own sibling task.
		"svc/deep/sloff.yml": `tools:
  deep-tool:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
    depends:
      - task: deep-src
commands:
  - name: deep-src
    cmd: ["sh", "-c", "true"]
    inputs: ["deep.in"]
    outputs: ["deep.out"]
    tools: [versioner]
  - name: deep-consume
    cmd: ["sh", "-c", "true"]
    inputs: ["dc.in"]
    outputs: ["dc.out"]
    tools: [root-tool]
`,
		"svc/deep/deep.in": "d", "svc/deep/dc.in": "x",
		"gen/sloff.yml": `commands:
  - name: gen-src
    cmd: ["sh", "-c", "true"]
    inputs: ["g.in"]
    outputs: ["g.out"]
    tools: [versioner]
`,
		"gen/g.in": "g",
	})

	tasks, _, err := newProviderRunner(t, workdir, specs).Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// Nested consumer of the root-defined tool: root's "gen" rebases to "../../gen".
	wantDeep := []depgraph.TaskRef{{SpecRelpath: "gen", Name: "gen-src"}}
	if diff := cmp.Diff(wantDeep, dependsOfTask(t, tasks, "svc/deep", "deep-consume")); diff != "" {
		t.Errorf("deep-consume DependsOn mismatch (-want +got):\n%s", diff)
	}
	// Root consumer of the nested tool: same-dir "deep-src" rebases to "svc/deep".
	wantRoot := []depgraph.TaskRef{{SpecRelpath: "svc/deep", Name: "deep-src"}}
	if diff := cmp.Diff(wantRoot, dependsOfTask(t, tasks, ".", "root-consume")); diff != "" {
		t.Errorf("root-consume DependsOn mismatch (-want +got):\n%s", diff)
	}
}

// TestRunner_ToolDepends_DedupsHandWrittenEdge: a consumer that already
// declares the tool's edge by hand keeps working — the injection is skipped,
// no duplicate-depends error, and the edge appears exactly once.
func TestRunner_ToolDepends_DedupsHandWrittenEdge(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"gen/sloff.yml": `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  gen-tool:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
    depends:
      - task: gen-src
commands:
  - name: gen-src
    cmd: ["sh", "-c", "true"]
    inputs: ["src.in"]
    outputs: ["src.out"]
    tools: [versioner]
  - name: consume
    cmd: ["sh", "-c", "true"]
    inputs: ["a.in"]
    outputs: ["a.out"]
    tools: [gen-tool]
    depends:
      - task: gen-src
`,
		"gen/src.in": "s", "gen/a.in": "a",
	})

	tasks, _, err := newProviderRunner(t, workdir, specs).Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []depgraph.TaskRef{{SpecRelpath: "gen", Name: "gen-src"}}
	if diff := cmp.Diff(want, dependsOfTask(t, tasks, "gen", "consume")); diff != "" {
		t.Errorf("consume DependsOn mismatch (-want +got):\n%s", diff)
	}
}

// TestRunner_ToolDepends_SelfEdgeErrors locks ADR-0019 D2's structural-
// contradiction check: a tool whose depends points at a task that itself
// uses the tool cannot bootstrap; the error is attributed to the tool.
func TestRunner_ToolDepends_SelfEdgeErrors(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"gen/sloff.yml": `tools:
  gen-tool:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
    depends:
      - task: gen-src
commands:
  - name: gen-src
    cmd: ["sh", "-c", "true"]
    inputs: ["src.in"]
    outputs: ["src.out"]
    tools: [gen-tool]
`,
		"gen/src.in": "s",
	})

	_, _, err := newProviderRunner(t, workdir, specs).Plan(context.Background())
	if err == nil {
		t.Fatal("expected self-edge error, got nil")
	}
	for _, want := range []string{`tool "gen-tool"`, "gen:gen-src", "cannot reference the tool"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
}

// TestRunner_ToolDepends_DanglingTargetErrors: a referenced tool whose
// depends names a task that exists nowhere fails at injection time with the
// tool as the subject (the consumer never wrote this edge).
func TestRunner_ToolDepends_DanglingTargetErrors(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"gen/sloff.yml": `tools:
  gen-tool:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
    depends:
      - task: no-such-task
commands:
  - name: consume
    cmd: ["sh", "-c", "true"]
    inputs: ["a.in"]
    outputs: ["a.out"]
    tools: [gen-tool]
`,
		"gen/a.in": "a",
	})

	_, _, err := newProviderRunner(t, workdir, specs).Plan(context.Background())
	if err == nil {
		t.Fatal("expected dangling-target error, got nil")
	}
	for _, want := range []string{`tool "gen-tool"`, "gen/sloff.yml", `"no-such-task"`, "not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
}

// TestRunner_ToolDepends_UnreferencedToolBrokenDependsHarmless: a catalog
// tool nobody references is neither resolved nor validated (ADR-0008), and
// its dangling depends must not fail the plan either.
func TestRunner_ToolDepends_UnreferencedToolBrokenDependsHarmless(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"gen/sloff.yml": `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  unreferenced:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
    depends:
      - task: no-such-task
commands:
  - name: gen
    cmd: ["sh", "-c", "true"]
    inputs: ["a.in"]
    outputs: ["a.out"]
    tools: [versioner]
`,
		"gen/a.in": "a",
	})

	if _, _, err := newProviderRunner(t, workdir, specs).Plan(context.Background()); err != nil {
		t.Errorf("unreferenced tool's broken depends must be inert, got %v", err)
	}
}

// TestRunner_ToolDepends_BarrierReceivesNoInjection: barriers cannot declare
// tools (ADR-0017 D1), so injection never touches them — their depends stay
// exactly as declared even when sibling tasks receive injected edges.
func TestRunner_ToolDepends_BarrierReceivesNoInjection(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"gen/sloff.yml": `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  gen-tool:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
    depends:
      - task: gen-src
commands:
  - name: gen-src
    cmd: ["sh", "-c", "true"]
    inputs: ["src.in"]
    outputs: ["src.out"]
    tools: [versioner]
  - name: consume
    cmd: ["sh", "-c", "true"]
    inputs: ["a.in"]
    outputs: ["a.out"]
    tools: [gen-tool]
  - name: all
    barrier: true
    depends:
      - task: consume
`,
		"gen/src.in": "s", "gen/a.in": "a",
	})

	tasks, _, err := newProviderRunner(t, workdir, specs).Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []depgraph.TaskRef{{SpecRelpath: "gen", Name: "consume"}}
	if diff := cmp.Diff(want, dependsOfTask(t, tasks, "gen", "all")); diff != "" {
		t.Errorf("barrier DependsOn mismatch (-want +got):\n%s", diff)
	}
}
