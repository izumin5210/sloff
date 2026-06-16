package runner_test

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
)

const patternProducerYML = `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
commands:
  - name: gen-a
    cmd: ["sh", "-c", "true"]
    inputs: ["a.in"]
    outputs: ["a.out"]
    tools: [versioner]
  - name: gen-b
    cmd: ["sh", "-c", "true"]
    inputs: ["b.in"]
    outputs: ["b.out"]
    tools: [versioner]
  - name: other
    cmd: ["sh", "-c", "true"]
    inputs: ["c.in"]
    outputs: ["c.out"]
    tools: [versioner]
`

// dependsOfTask returns the resolved depends edges of one planned task.
func dependsOfTask(t *testing.T, tasks []depgraph.Task, specDir, name string) []depgraph.TaskRef {
	t.Helper()
	for _, task := range tasks {
		if task.SpecRelpath == specDir && task.Name == name {
			return task.DependsOn
		}
	}
	t.Fatalf("task %s/%s not in plan", specDir, name)
	return nil
}

// TestRunner_PatternDepends_ExpandsThroughPlan verifies a glob depends entry is
// expanded into literal edges by Plan (ADR-0016 D2): the pattern "gen-*" must
// resolve to gen-a and gen-b — and never the declaring command or the
// non-matching "other" — so from depgraph construction on, the consumer carries
// ordinary literal edges.
func TestRunner_PatternDepends_ExpandsThroughPlan(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"gen/sloff.yml": patternProducerYML,
		"gen/a.in":      "a",
		"gen/b.in":      "b",
		"gen/c.in":      "c",
		"svc/sloff.yml": `commands:
  - name: consume
    cmd: ["sh", "-c", "true"]
    inputs: ["x.in"]
    outputs: ["y.out"]
    tools: [versioner]
    depends:
      - {spec: ../gen, task: "gen-*"}
`,
		"svc/x.in": "x",
	})

	tasks, _, err := newProviderRunner(t, workdir, specs).Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := dependsOfTask(t, tasks, "svc", "consume")
	want := []depgraph.TaskRef{
		{SpecRelpath: "gen", Name: "gen-a"},
		{SpecRelpath: "gen", Name: "gen-b"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("expanded DependsOn mismatch (-want +got):\n%s", diff)
	}
}

// TestRunner_PatternDepends_ZeroMatchFailsPlan confirms a pattern that resolves
// to no task is a hard error at plan time (ADR-0016 D3), not a silent
// no-dependency.
func TestRunner_PatternDepends_ZeroMatchFailsPlan(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"gen/sloff.yml": patternProducerYML,
		"gen/a.in":      "a",
		"gen/b.in":      "b",
		"gen/c.in":      "c",
		"svc/sloff.yml": `commands:
  - name: consume
    cmd: ["sh", "-c", "true"]
    inputs: ["x.in"]
    outputs: ["y.out"]
    tools: [versioner]
    depends:
      - {spec: ../gen, task: "nope-*"}
`,
		"svc/x.in": "x",
	})

	_, _, err := newProviderRunner(t, workdir, specs).Plan(context.Background())
	if err == nil {
		t.Fatal("expected a zero-match error, got nil")
	}
}

// TestRunner_PatternDepends_E2E_MatchesGeneratedTasks is the ADR-0015 × ADR-0016
// synergy as a full run: a command_provider in gen/ emits copy-a / copy-b, a
// glob depends "copy-*" in svc/ matches them, orders bundle after the whole
// group, and reads their outputs (observed dependency, no warning). The golden
// pins the fingerprint records written for the generated tasks and the consumer.
func TestRunner_PatternDepends_E2E_MatchesGeneratedTasks(t *testing.T) {
	requireSh(t)
	runE2E(t, "pattern-depends-matches-generated", runStep())
}

// TestRunner_PatternDepends_E2E_UnobservedWarns pins ADR-0016 D4: a pattern that
// matches tasks whose outputs the consumer never reads emits exactly one
// aggregated warning for the pattern, while the run still succeeds and writes
// records.
func TestRunner_PatternDepends_E2E_UnobservedWarns(t *testing.T) {
	requireSh(t)
	runE2E(t, "pattern-depends-unobserved-warns", runStep(expectWarn(`pattern "copy-*"`)))
}
