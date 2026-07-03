package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/golocal"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

// These E2E cases cover ADR-0019 (tool bootstrap depends + deferred
// resolution). The fixtures under testdata/e2e/runner/tooldepends-*/ share
// one shape: a go-local tool whose import closure contains a generated
// package (gen/gen.go), so the tool cannot be resolved on a clean tree until
// its producer task has run.

// TestRunner_ToolDepends_ColdBootstrap is the ADR-0019 motivating scenario
// end to end. On a clean tree (gen/gen.go absent) the go-local tool fails
// eager resolution, is deferred (declared depends), and resolves after the
// injected edge has run gen-source — one `sloff run`, no external bootstrap.
// The second run must be all-SKIP: the cold run's records were computed from
// the post-resolution full input set, so they hit the warm keys (D7). The
// marker file (appended on every consume execution) pins "ran exactly once"
// in the golden.
func TestRunner_ToolDepends_ColdBootstrap(t *testing.T) {
	requireSh(t)
	runE2E(
		t, "tooldepends-cold-bootstrap",
		runStep(
			expectWarn("deferred until its declared depends complete"),
			expectInfo(`resolved tool "gen-tool" after its declared depends completed`),
		),
		runStep(expectNoWarns(), expectNoInfoContaining("RUN ")),
	)
}

// TestRunner_ToolDepends_UndeclaredToolStillFatal is the regression guard for
// D3's gate: the same broken-closure tool without a depends declaration must
// keep failing at run start (typo and environment errors stay loud), naming
// the tool, before any task executes — the golden is the untouched initial
// tree.
func TestRunner_ToolDepends_UndeclaredToolStillFatal(t *testing.T) {
	requireSh(t)
	runE2E(
		t, "tooldepends-undeclared-fatal",
		runStep(expectError(`resolve inputs for tool "gen-tool"`)),
	)
}

// TestRunner_ToolDepends_DeferredFailureAttribution: the tool's depends names
// a task that does not produce its missing sources, so the deferred retry
// fails too. The failure surfaces when the consumer task starts, attributed
// to the tool with both causes and the likely spec fix (D4). The producer it
// did declare (unrelated) has already run — its output and record are in the
// golden — while consume never executed (no output.txt, no marker, no record).
func TestRunner_ToolDepends_DeferredFailureAttribution(t *testing.T) {
	requireSh(t)
	runE2E(
		t, "tooldepends-deferred-failure",
		runStep(
			expectWarn("deferred until its declared depends complete"),
			expectError(`tool "gen-tool" (defined in sloff.yml) could not be resolved: at run start:`),
		),
	)
}

// TestRunner_ToolDepends_DedupWithHandWrittenEdge: a consumer that already
// declares the producer edge by hand (the pre-ADR-0019 idiom) keeps working
// unchanged when the tool also declares it — no duplicate-depends error, the
// cold bootstrap still succeeds, and the second run is all-SKIP.
func TestRunner_ToolDepends_DedupWithHandWrittenEdge(t *testing.T) {
	requireSh(t)
	runE2E(
		t, "tooldepends-dedup-handwritten",
		runStep(expectWarn("deferred until its declared depends complete")),
		runStep(expectNoWarns(), expectNoInfoContaining("RUN ")),
	)
}

// TestRunner_ToolDepends_SelfEdgeError pins the D2 structural check and its
// message: the tool's bootstrap producer itself uses the tool, so the run
// fails at injection time (before preflight or any resolver call) and the
// golden is the untouched initial tree.
func TestRunner_ToolDepends_SelfEdgeError(t *testing.T) {
	requireSh(t)
	runE2E(
		t, "tooldepends-self-edge-error",
		runStep(expectError(`tool "gen-tool" declares depends on gen:gen-src, but that task itself uses the tool`)),
	)
}

// stubExtraInputsResolver impersonates the pnpm-local channel and returns a
// fixed ExtraInputs list without touching the filesystem — the analogue of
// pnpm-local's git-based enumeration listing a tracked file that the
// worktree no longer holds (ADR-0019 D6's motivating case).
type stubExtraInputsResolver struct {
	inputs []string
}

func (s *stubExtraInputsResolver) Name() string { return "pnpm-local" }

func (s *stubExtraInputsResolver) Inputs(context.Context, string, *toolresolver.DeclaredTool) ([]string, error) {
	return append([]string(nil), s.inputs...), nil
}

func (s *stubExtraInputsResolver) Versions(context.Context, string, *toolresolver.DeclaredTool) ([]toolresolver.ResolvedVersion, error) {
	return []toolresolver.ResolvedVersion{{Name: "codegen", Source: "stub:codegen", Version: "stub:codegen@1.0.0"}}, nil
}

// TestRunner_ToolDepends_PrefetchSkipsMissingExtraInput covers D6: tool
// resolution succeeds but one of its ExtraInputs does not exist on disk yet
// (its producer — declared in the tool's depends — has not run). The
// consumer's optimistic-key computation hits fs.ErrNotExist; instead of
// failing the run at prefetch, the task is excluded from the batch and its
// exec-time lookup takes the live-Load path. Both the cold run and the warm
// rerun must succeed, and the warm rerun must be a SKIP (record continuity).
func TestRunner_ToolDepends_PrefetchSkipsMissingExtraInput(t *testing.T) {
	requireSh(t)
	workdir := t.TempDir()
	write := func(rel, contents string) {
		t.Helper()
		full := filepath.Join(workdir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lib.txt.tmpl", "libcontent\n")
	write("input.txt", "hello\n")
	write("sloff.yml", `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  codegen:
    pnpm-local: "@org/codegen"
    depends:
      - task: make-lib

commands:
  - name: make-lib
    cmd: ["sh", "-c", "mkdir -p lib && cp lib.txt.tmpl lib/generated.txt"]
    inputs: ["lib.txt.tmpl"]
    outputs: ["lib/generated.txt"]
    tools: [versioner]
  - name: consume
    cmd: ["sh", "-c", "cat input.txt lib/generated.txt > out.txt; printf x >> marker.txt"]
    inputs: ["input.txt"]
    outputs: ["out.txt"]
    tools: [codegen]
`)

	specs, err := spec.Discover(workdir, "**/sloff.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	store := local.New(workdir, local.WithClock(func() time.Time { return fixedClock }))
	newRunner := func() *runner.Runner {
		reg := toolresolver.NewRegistry()
		reg.Register(script.New(workdir))
		// The stub lists lib/generated.txt as an ExtraInput unconditionally,
		// so on the first run the consumer's prefetch stats a missing file.
		reg.Register(&stubExtraInputsResolver{inputs: []string{"lib/generated.txt"}})
		return runner.New(runner.Options{
			RepoRoot:  workdir,
			Specs:     specs,
			Storage:   store,
			Resolvers: reg,
			Preflight: preflight.NewRegistry(),
		})
	}

	// Cold run: without the D6 skip this fails at prefetch with ENOENT.
	if err := newRunner().Run(context.Background()); err != nil {
		t.Fatalf("cold Run: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(workdir, "out.txt"))
	if err != nil {
		t.Fatalf("consume did not produce out.txt: %v", err)
	}
	if got, want := string(out), "hello\nlibcontent\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}

	// Warm rerun: every file exists, the prefetch covers everything again,
	// and both tasks must SKIP — the marker not advancing proves consume's
	// cold record was written under the full (post-producer) input set.
	if err := newRunner().Run(context.Background()); err != nil {
		t.Fatalf("warm Run: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(workdir, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != "x" {
		t.Errorf("consume re-executed on the warm run: marker = %q, want %q", marker, "x")
	}
}

// TestRunner_ToolDepends_PlanColdSucceeds covers the Plan/graph side of
// ADR-0019 (D2/D3): on a clean tree the go-local tool defers instead of
// failing Plan, and the returned DAG carries the injected edge — Run and
// Plan agree on the graph shape regardless of tree state.
func TestRunner_ToolDepends_PlanColdSucceeds(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"go.mod": "module example.test/bootstrapfixture\n\ngo 1.22\n",
		"cmd/tool/main.go": `package main

import (
	"os"

	"example.test/bootstrapfixture/gen"
)

func main() {
	if err := os.WriteFile("output.txt", []byte(gen.Suffix), 0o644); err != nil {
		panic(err)
	}
}
`,
		"gen.go.txt": "package gen\n\nconst Suffix = \"generated\"\n",
		"input.txt":  "hello\n",
		"sloff.yml": `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  gen-tool:
    go-local: ./cmd/tool
    depends:
      - task: gen-source

commands:
  - name: gen-source
    cmd: ["sh", "-c", "mkdir -p gen && cp gen.go.txt gen/gen.go"]
    inputs: ["gen.go.txt"]
    outputs: ["gen/gen.go"]
    tools: [versioner]
  - name: consume
    cmd: ["sh", "-c", "go run ./cmd/tool"]
    inputs: ["input.txt"]
    outputs: ["output.txt"]
    tools: [gen-tool]
`,
	})

	reg := toolresolver.NewRegistry()
	reg.Register(script.New(workdir))
	reg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: reg,
		Preflight: preflight.NewRegistry(),
	})

	tasks, missing, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan on a cold tree must not fail for a depends-declaring tool: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("expected no missing dependencies, got %v", missing)
	}
	want := []depgraph.TaskRef{{SpecRelpath: ".", Name: "gen-source"}}
	if diff := cmp.Diff(want, dependsOfTask(t, tasks, ".", "consume")); diff != "" {
		t.Errorf("injected edge missing from cold Plan (-want +got):\n%s", diff)
	}
}

// TestRunner_ToolDepends_InjectedEdgeNoUnobservedWarn covers ADR-0019 D2:
// injected edges must not trigger the "none of the files it produced match"
// warning. A script tool with zero input contributions is the clearest case:
// the producer's outputs (the tool binary) never appear in the consumer's
// inputs, but the edge exists for scheduling, not data-flow, so warning would
// be wrong-by-construction. Both cold and warm runs must emit no warning.
func TestRunner_ToolDepends_InjectedEdgeNoUnobservedWarn(t *testing.T) {
	requireSh(t)
	runE2E(
		t, "zz-verify-script-tooldep",
		runStep(expectNoWarns()),
		runStep(expectNoWarns(), expectNoInfoContaining("RUN ")),
	)
}

// TestRunner_ToolDepends_IndirectCycleError covers ADR-0019 D2: when an
// injected edge forms an indirect cycle (tool X depends on P; P depends on C;
// C uses tool X), the cycle error must mention both "cycle detected" and a
// note attributing the closing edge to the tool.
func TestRunner_ToolDepends_IndirectCycleError(t *testing.T) {
	requireSh(t)
	runE2E(
		t, "tooldepends-indirect-cycle-error",
		runStep(
			expectError("cycle detected"),
			expectError(`was injected from tool "gen-tool"`),
		),
	)
}
