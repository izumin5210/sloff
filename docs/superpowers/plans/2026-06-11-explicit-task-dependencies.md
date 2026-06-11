# Explicit Task Dependencies (ADR-0013) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Switch task execution ordering from inputs/outputs file-overlap derivation to a spec-declared `depends` field, keeping overlap computation as validation (missing-depends → error, unobserved-depends → warning), per ADR-0013.

**Architecture:** `spec` parses/validates the new `depends` struct entries; `depgraph` builds the DAG from declared edges only and exposes overlap validation as `FindMissingDependencies`; `runner` resolves depends into `depgraph.TaskRef`s, enforces plan-time and run-time validation, and emits the inputs-omission warning; `explain`/`graph` render declared edges with overlap evidence when observable.

**Tech Stack:** Go, goccy/go-yaml, bmatcuk/doublestar/v4, golden-based E2E harness (`testdata/e2e/{runner,graph}`).

**Spec:** `docs/adr/0013-explicit-task-dependencies.md` (D1–D5). Design context: `docs/design/architecture.md` §タスク間依存.

**Conventions for every task below:**
- Run tests from the repo root.
- Commit messages are English Conventional Commits, and every commit ends with the trailer:
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`
- The suite must be green at every commit boundary (`go test ./...`).

---

## File Structure

| File | Change |
|---|---|
| `internal/sloff/spec/spec.go` | Add `Depend` type, `Command.Depends`, file-level validation, `ValidateDependReferences` |
| `internal/sloff/spec/spec_test.go` (or new `depends_test.go`) | Unit tests for the above |
| `internal/sloff/depgraph/depgraph.go` | Add `TaskRef`, `Task.DependsOn`; switch `Build` edges to declared deps; add `FindMissingDependencies`, `FormatMissing` |
| `internal/sloff/depgraph/depgraph_test.go` | Rewrite edge-derivation tests; add validation tests |
| `internal/sloff/runner/runner.go` | `resolveDepends`, depends validation call, declared-edge predecessors, plan-time error, run-time validation, warning, `producedBy` refactor, `Plan` signature |
| `internal/sloff/runner/depends_internal_test.go` | New: unit tests for `resolveDepends` / `taskReadsPath` |
| `internal/sloff/runner/runner_test.go` | Harness options (`expectError`, `withReadOnly`, `expectWarn`), fix 2 inline chain tests, new E2E tests |
| `internal/sloff/explain/explain.go` | `Edges` from declared deps; `LabelSample` → `"(declared)"` for empty evidence |
| `internal/sloff/explain/explain_test.go` | Update tests for declared edges |
| `cmd/sloff/graph.go` | `Plan` new signature, stderr warnings, help-text updates |
| `cmd/sloff/graph_test.go` | Update docstrings; new golden tests |
| `testdata/e2e/graph/*/initial/**/sloff.yml` | Add `depends` to 4 existing fixtures; 2 new fixtures |
| `testdata/e2e/runner/depends-*` | 5 new fixtures |

---

### Task 1: spec — `Depend` type, parsing, file-level validation

**Files:**
- Modify: `internal/sloff/spec/spec.go`
- Test: `internal/sloff/spec/depends_test.go` (new file)

- [ ] **Step 1: Write the failing tests**

Create `internal/sloff/spec/depends_test.go`:

```go
package spec_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/spec"
)

const dependsSpecYAML = `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]

commands:
  - name: producer
    cmd: ["sh", "-c", "true"]
    inputs: ["in.txt"]
    outputs: ["mid.txt"]
    tools: [versioner]
  - name: consumer
    cmd: ["sh", "-c", "true"]
    inputs: ["mid.txt"]
    outputs: ["out.txt"]
    tools: [versioner]
    depends:
      - task: producer
      - spec: ../other
        task: gen
`

func TestParse_DependsEntries(t *testing.T) {
	f, err := spec.Parse([]byte(dependsSpecYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := f.Commands[1].Depends
	want := []spec.Depend{
		{Task: "producer"},
		{Spec: "../other", Task: "gen"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("depends mismatch (-want +got):\n%s", diff)
	}
}

func TestParse_DependsOmittedIsNil(t *testing.T) {
	f, err := spec.Parse([]byte(dependsSpecYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Commands[0].Depends != nil {
		t.Errorf("expected nil depends, got %v", f.Commands[0].Depends)
	}
}

func TestParse_DependsTaskRequired(t *testing.T) {
	yml := strings.Replace(dependsSpecYAML, "      - task: producer", "      - spec: ../other", 1)
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "task is required") {
		t.Errorf("expected 'task is required' error, got %v", err)
	}
}

func TestParse_DependsSpecMustBeRelative(t *testing.T) {
	yml := strings.Replace(dependsSpecYAML, "spec: ../other", "spec: /abs/path", 1)
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "must be a relative path") {
		t.Errorf("expected relative-path error, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/sloff/spec/... -run TestParse_Depends -v`
Expected: FAIL (`f.Commands[1].Depends` undefined / unknown field `Depend`)

- [ ] **Step 3: Implement**

In `internal/sloff/spec/spec.go`, add the `Depend` type directly above the `Command` type:

```go
// Depend is one entry of commands[*].depends — a reference to another task
// that must complete before this command runs (ADR-0013). Spec is the
// dependency's spec dir relative to the sloff.yml that declares the
// reference; empty means the same file. Depends affects scheduling only and
// never feeds the fingerprint input_hash (ADR-0013 D4).
type Depend struct {
	Spec string `yaml:"spec,omitempty"`
	Task string `yaml:"task"`
}
```

Add the field to `Command` (keep the existing fields as-is):

```go
type Command struct {
	Cmd     CmdLine  `yaml:"cmd"`
	Depends []Depend `yaml:"depends,omitempty"`
	Inputs  []string `yaml:"inputs"`
	Name    string   `yaml:"name"`
	Outputs []string `yaml:"outputs"`
	Tools   []string `yaml:"tools,omitempty"`
}
```

In `validateCommands`, after the existing `for j, name := range c.Tools` loop, add:

```go
		for j, d := range c.Depends {
			if d.Task == "" {
				return fmt.Errorf("commands[%d] (%s): depends[%d]: task is required", i, c.Name, j)
			}
			if strings.HasPrefix(d.Spec, "/") {
				return fmt.Errorf("commands[%d] (%s): depends[%d]: spec must be a relative path, got %q", i, c.Name, j, d.Spec)
			}
		}
```

(`strings` is already imported.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/sloff/spec/... -v`
Expected: PASS (all, including pre-existing tests)

- [ ] **Step 5: Commit**

```bash
git add internal/sloff/spec/
git commit -m "feat(spec): parse depends entries on commands"
```

---

### Task 2: spec — cross-file `ValidateDependReferences`

Reference-existence / self-reference / duplicate / repo-root-escape checks need the full discovered spec set, mirroring `ValidateToolReferences`.

**Files:**
- Modify: `internal/sloff/spec/spec.go`
- Test: `internal/sloff/spec/depends_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/sloff/spec/depends_test.go`:

```go
// buildSpecs assembles a discovered-spec set without touching the
// filesystem: dirs are OS-native (as spec.Discover produces them).
func buildSpecs(t *testing.T, byDir map[string]string) []spec.Spec {
	t.Helper()
	out := make([]spec.Spec, 0, len(byDir))
	for dir, yml := range byDir {
		f, err := spec.Parse([]byte(yml))
		if err != nil {
			t.Fatalf("Parse %s: %v", dir, err)
		}
		out = append(out, spec.Spec{Dir: dir, Path: dir + "/sloff.yml", File: f})
	}
	return out
}

const producerYAML = `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
commands:
  - name: gen
    cmd: ["sh", "-c", "true"]
    inputs: ["in.txt"]
    outputs: ["out.txt"]
    tools: [versioner]
`

func consumerYAML(dependsBlock string) string {
	return `commands:
  - name: consume
    cmd: ["sh", "-c", "true"]
    inputs: ["x.txt"]
    outputs: ["y.txt"]
    tools: [versioner]
` + dependsBlock
}

func TestValidateDependReferences_OK(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/options": producerYAML,
		"proto/svc": consumerYAML(`    depends:
      - spec: ../options
        task: gen
`),
	})
	if err := spec.ValidateDependReferences(specs); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

func TestValidateDependReferences_UnknownSpecDirErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/svc": consumerYAML(`    depends:
      - spec: ../nowhere
        task: gen
`),
	})
	err := spec.ValidateDependReferences(specs)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestValidateDependReferences_UnknownTaskErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/options": producerYAML,
		"proto/svc": consumerYAML(`    depends:
      - spec: ../options
        task: missing-task
`),
	})
	err := spec.ValidateDependReferences(specs)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestValidateDependReferences_SelfReferenceErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/svc": consumerYAML(`    depends:
      - task: consume
`),
	})
	err := spec.ValidateDependReferences(specs)
	if err == nil || !strings.Contains(err.Error(), "itself") {
		t.Errorf("expected self-reference error, got %v", err)
	}
}

func TestValidateDependReferences_DuplicateEdgeErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/options": producerYAML,
		"proto/svc": consumerYAML(`    depends:
      - spec: ../options
        task: gen
      - spec: ../../proto/options
        task: gen
`),
	})
	err := spec.ValidateDependReferences(specs)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got %v", err)
	}
}

func TestValidateDependReferences_RepoRootEscapeErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/svc": consumerYAML(`    depends:
      - spec: ../../../outside
        task: gen
`),
	})
	err := spec.ValidateDependReferences(specs)
	if err == nil || !strings.Contains(err.Error(), "escapes repo root") {
		t.Errorf("expected escape error, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/sloff/spec/... -run TestValidateDependReferences -v`
Expected: FAIL (`spec.ValidateDependReferences` undefined)

- [ ] **Step 3: Implement**

Append to `internal/sloff/spec/spec.go`:

```go
// ValidateDependReferences checks every commands[*].depends entry across the
// discovered spec set (ADR-0013 D1): the referenced spec dir must stay inside
// the repo, the referenced (spec dir, task) must exist, self-references are
// rejected, and the same edge declared twice in one command is rejected.
// Like ValidateToolReferences, this is a cross-file pass run on the full set
// after Discover; per-file structural checks live in validate.
func ValidateDependReferences(specs []Spec) error {
	type taskKey struct{ dir, name string }
	defined := map[taskKey]struct{}{}
	for _, sp := range specs {
		dir := filepath.ToSlash(sp.Dir)
		for _, c := range sp.File.Commands {
			defined[taskKey{dir, c.Name}] = struct{}{}
		}
	}
	for _, sp := range specs {
		dir := filepath.ToSlash(sp.Dir)
		for _, c := range sp.File.Commands {
			seen := map[taskKey]struct{}{}
			for i, d := range c.Depends {
				// path.Join cleans, so "../options" resolves against the
				// declaring file's dir the same way inputs/outputs globs do.
				target := path.Join(dir, d.Spec)
				if target == ".." || strings.HasPrefix(target, "../") {
					return fmt.Errorf("%s/%s: depends[%d]: spec %q escapes repo root", sp.Dir, c.Name, i, d.Spec)
				}
				key := taskKey{target, d.Task}
				if target == dir && d.Task == c.Name {
					return fmt.Errorf("%s/%s: depends[%d]: task depends on itself", sp.Dir, c.Name, i)
				}
				if _, ok := defined[key]; !ok {
					return fmt.Errorf("%s/%s: depends[%d]: task %q not found in spec dir %q", sp.Dir, c.Name, i, d.Task, target)
				}
				if _, dup := seen[key]; dup {
					return fmt.Errorf("%s/%s: depends[%d]: duplicate depends entry %s:%s", sp.Dir, c.Name, i, target, d.Task)
				}
				seen[key] = struct{}{}
			}
		}
	}
	return nil
}
```

(`path` and `filepath` are already imported in spec.go.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/sloff/spec/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/sloff/spec/
git commit -m "feat(spec): validate depends references across the spec set"
```

---

### Task 3: depgraph — `TaskRef`, `DependsOn` carrier, overlap-validation helpers (additive)

`Build` stays overlap-based in this task so the suite remains green; the switch happens in Task 5.

**Files:**
- Modify: `internal/sloff/depgraph/depgraph.go`
- Test: `internal/sloff/depgraph/validate_test.go` (new file)

- [ ] **Step 1: Write the failing tests**

Create `internal/sloff/depgraph/validate_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/sloff/depgraph/... -run 'TestFindMissing|TestFormatMissing|TestTaskRefLabel' -v`
Expected: FAIL (undefined: `depgraph.TaskRef`, `DependsOn`, ...)

- [ ] **Step 3: Implement**

In `internal/sloff/depgraph/depgraph.go`:

Add `"path/filepath"` to imports.

Add below the package imports (above `Task`):

```go
// TaskRef identifies a task by its (SpecRelpath, Name) key — the same pair
// that uniquely keys Task nodes throughout the orchestrator.
type TaskRef struct {
	SpecRelpath string
	Name        string
}

// Label renders the canonical human-readable identifier used in errors and
// graph output. SpecRelpath "" (unit tests) and "." (a sloff.yml at the repo
// root) both mean "no qualifier needed".
func (r TaskRef) Label() string {
	if r.SpecRelpath == "" || r.SpecRelpath == "." {
		return r.Name
	}
	return r.SpecRelpath + ":" + r.Name
}
```

Extend `Task` with the declared-dependency carrier and a `Ref` helper:

```go
// Task is one DAG node. SpecRelpath/Name together form the unique key.
type Task struct {
	SpecRelpath string
	Name        string
	Inputs      []string // expanded paths, repo-root relative
	Outputs     []string
	// DependsOn carries the spec-declared dependencies (ADR-0013). Build uses
	// only these for ordering edges; Inputs/Outputs remain for duplicate-
	// producer detection and overlap validation.
	DependsOn []TaskRef
}

// Ref returns the task's identity key.
func (t Task) Ref() TaskRef { return TaskRef{SpecRelpath: t.SpecRelpath, Name: t.Name} }
```

Append the validation helpers at the end of the file:

```go
// MissingDependency is one undeclared edge surfaced by overlap validation
// (ADR-0013 D3): the consumer's expanded inputs intersect the producer's
// expanded outputs, but the consumer does not declare the producer in
// depends. Files carries the intersection as evidence, sorted ascending.
type MissingDependency struct {
	Producer TaskRef
	Consumer TaskRef
	Files    []string
}

// FindMissingDependencies computes O_A ∩ I_B for every task pair and returns
// the pairs whose overlap is not covered by a declared depends edge. The
// result is deterministic: ordered by (Consumer, Producer) labels, files
// sorted ascending. An empty result means every observable data flow is
// declared; clean checkouts (no generated files on disk) trivially return
// empty, which is why the runner re-validates against actually-produced
// paths at run time.
func FindMissingDependencies(tasks []Task) []MissingDependency {
	producer := map[string]int{}
	for i, t := range tasks {
		for _, out := range t.Outputs {
			producer[out] = i
		}
	}
	var out []MissingDependency
	for i, t := range tasks {
		declared := make(map[TaskRef]struct{}, len(t.DependsOn))
		for _, d := range t.DependsOn {
			declared[d] = struct{}{}
		}
		byProducer := map[int][]string{}
		for _, in := range t.Inputs {
			j, ok := producer[in]
			if !ok || j == i {
				continue
			}
			if _, ok := declared[tasks[j].Ref()]; ok {
				continue
			}
			byProducer[j] = append(byProducer[j], in)
		}
		for j, files := range byProducer {
			sort.Strings(files)
			out = append(out, MissingDependency{Producer: tasks[j].Ref(), Consumer: t.Ref(), Files: files})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Consumer != out[j].Consumer {
			return out[i].Consumer.Label() < out[j].Consumer.Label()
		}
		return out[i].Producer.Label() < out[j].Producer.Label()
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// FormatMissing renders one missing dependency as an actionable message,
// including the exact depends entry to add. The suggested spec path is
// relative to the consumer's spec dir (ADR-0013 D1) and omitted entirely for
// same-dir references.
func FormatMissing(m MissingDependency) string {
	evidence := "generated files"
	if len(m.Files) > 0 {
		evidence = m.Files[0]
		if len(m.Files) > 1 {
			evidence = fmt.Sprintf("%s (+%d more)", m.Files[0], len(m.Files)-1)
		}
	}
	return fmt.Sprintf("%s reads %s produced by %s but does not declare the dependency; add %s to %q in %s",
		m.Consumer.Label(), evidence, m.Producer.Label(),
		suggestedDependEntry(m), m.Consumer.Name, specYAMLPath(m.Consumer))
}

func specYAMLPath(r TaskRef) string {
	dir := filepath.ToSlash(r.SpecRelpath)
	if dir == "" || dir == "." {
		return "sloff.yml"
	}
	return dir + "/sloff.yml"
}

func suggestedDependEntry(m MissingDependency) string {
	if m.Consumer.SpecRelpath == m.Producer.SpecRelpath {
		return fmt.Sprintf("`depends: [{task: %s}]`", m.Producer.Name)
	}
	rel, err := filepath.Rel(m.Consumer.SpecRelpath, m.Producer.SpecRelpath)
	if err != nil {
		rel = m.Producer.SpecRelpath
	}
	return fmt.Sprintf("`depends: [{spec: %s, task: %s}]`", filepath.ToSlash(rel), m.Producer.Name)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/sloff/depgraph/... -v`
Expected: PASS (existing Build tests still green — Build untouched)

- [ ] **Step 5: Commit**

```bash
git add internal/sloff/depgraph/
git commit -m "feat(depgraph): add TaskRef, DependsOn carrier, and overlap validation helpers"
```

---

### Task 4: runner — resolve depends into tasks, wire reference validation

Still additive: nothing consumes `DependsOn` yet, suite stays green.

**Files:**
- Modify: `internal/sloff/runner/runner.go`
- Test: `internal/sloff/runner/depends_internal_test.go` (new file, package `runner`)

- [ ] **Step 1: Write the failing test**

Create `internal/sloff/runner/depends_internal_test.go`:

```go
package runner

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
	"github.com/izumin5210/sloff/internal/sloff/spec"
)

func TestResolveDepends_JoinsRelativeSpecDirs(t *testing.T) {
	got := resolveDepends("proto/svc", []spec.Depend{
		{Task: "lint"},                          // same spec file
		{Spec: "../options", Task: "gen"},       // sibling dir
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sloff/runner/... -run TestResolveDepends -v`
Expected: FAIL (undefined: `resolveDepends`)

- [ ] **Step 3: Implement**

In `internal/sloff/runner/runner.go`:

Add next to `mergeInputs` (helpers area):

```go
// resolveDepends maps declared depends entries to depgraph TaskRefs. The
// reference rules (existence, self-reference, duplicates, repo-root escape)
// are enforced by spec.ValidateDependReferences before collectTasks runs, so
// this is pure path arithmetic: clean-join the consumer's spec dir with each
// entry's relative spec path (empty = same dir), mirroring how inputs/outputs
// globs resolve (ADR-0013 D1).
func resolveDepends(specDir string, depends []spec.Depend) []depgraph.TaskRef {
	if len(depends) == 0 {
		return nil
	}
	dirSlash := filepath.ToSlash(specDir)
	out := make([]depgraph.TaskRef, 0, len(depends))
	for _, d := range depends {
		out = append(out, depgraph.TaskRef{
			SpecRelpath: filepath.FromSlash(path.Join(dirSlash, d.Spec)),
			Name:        d.Task,
		})
	}
	return out
}
```

In `collectTasks`, extend the `depgraph.Task` literal:

```go
			t := depgraph.Task{
				SpecRelpath: sp.Dir,
				Name:        c.Name,
				Inputs:      mergedInputs,
				Outputs:     outputs,
				DependsOn:   resolveDepends(sp.Dir, c.Depends),
			}
```

In `prepareRegistry`, after the `ValidateToolReferences` call:

```go
	if err := spec.ValidateDependReferences(r.opts.Specs); err != nil {
		return nil, nil, err
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/sloff/runner/... ./internal/sloff/spec/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/sloff/runner/
git commit -m "feat(runner): resolve declared depends into depgraph tasks"
```

---

### Task 5: the ordering switch — Build / predecessors / explain / fixtures

This is the breaking core: edges come from `DependsOn` only. Everything that relied on overlap-derived ordering is updated in the same commit so the suite stays green.

**Files:**
- Modify: `internal/sloff/depgraph/depgraph.go` (Build + package comment)
- Modify: `internal/sloff/depgraph/depgraph_test.go`
- Modify: `internal/sloff/runner/runner.go` (`taskPredecessorIndices`, stale comments)
- Modify: `internal/sloff/explain/explain.go` (`Edges`, `LabelSample`, comments)
- Modify: `internal/sloff/explain/explain_test.go`
- Modify: `cmd/sloff/graph.go` (help text), `cmd/sloff/graph_test.go` (docstring)
- Modify: `internal/sloff/runner/runner_test.go` (2 inline chain tests)
- Modify: `testdata/e2e/graph/{simple-chain-mermaid,simple-chain-dot,multi-deps-mermaid}/initial/spec/sloff.yml`
- Modify: `testdata/e2e/graph/pnpmlocal-build-chain-mermaid/initial/sloff.yml`

- [ ] **Step 1: Rewrite depgraph Build tests (failing first)**

In `internal/sloff/depgraph/depgraph_test.go`, replace `TestBuild_BDependsOnAPlacesABeforeB`, `TestBuild_DiamondRespectsTopologicalOrder`, `TestBuild_CycleErrors`, `TestBuild_DependencyDetectedAcrossSpecDirs` with declared-edge versions, and add the two new tests. (Keep `TestBuild_EmptyReturnsEmpty`, `TestBuild_NoDependenciesPreservesStableOrder`, `TestBuild_DuplicateOutputProducersErrors` unchanged.) The `taskD` / `ref` helpers from Task 3's `validate_test.go` are in the same package:

```go
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
```

Run: `go test ./internal/sloff/depgraph/... -v` → Expected: new tests FAIL (Build still derives from overlap).

- [ ] **Step 2: Switch `depgraph.Build` to declared edges**

In `internal/sloff/depgraph/depgraph.go`:

Replace the package comment:

```go
// Package depgraph builds the task DAG from each task's spec-declared
// depends entries (ADR-0013) and emits a stable topological order. File
// overlap between inputs/outputs is no longer an ordering source; it remains
// the evidence FindMissingDependencies uses to validate that every observed
// data flow has a declared edge.
package depgraph
```

In `Build`, replace the `keyToIdx` construction and the edge-derivation loop. Old:

```go
	type idx = int
	keyToIdx := make(map[string]idx, len(tasks))
	for i, t := range tasks {
		keyToIdx[taskKey(t)] = i
	}
```

New:

```go
	type idx = int
	keyToIdx := make(map[TaskRef]idx, len(tasks))
	for i, t := range tasks {
		keyToIdx[t.Ref()] = i
	}
```

Old edge loop:

```go
	for i, t := range tasks {
		for _, in := range t.Inputs {
			producer, ok := outputProducer[in]
			if !ok || producer == i {
				continue
			}
			if _, dup := edges[i][producer]; dup {
				continue
			}
			edges[i][producer] = struct{}{}
			inDegree[i]++
		}
	}
```

New:

```go
	for i, t := range tasks {
		for _, dep := range t.DependsOn {
			j, ok := keyToIdx[dep]
			if !ok {
				return nil, fmt.Errorf("%s: depends on unknown task %s", taskLabel(t), dep.Label())
			}
			if j == i {
				return nil, fmt.Errorf("%s: depends on itself", taskLabel(t))
			}
			if _, dup := edges[i][j]; dup {
				continue
			}
			edges[i][j] = struct{}{}
			inDegree[i]++
		}
	}
```

Keep the duplicate-output-producer block (`outputProducer` / `conflicts`) and the Kahn block exactly as they are. Note: `outputProducer` is now only used by conflict detection — keep the variable; remove nothing else.

Run: `go test ./internal/sloff/depgraph/... -v` → Expected: PASS.

- [ ] **Step 3: Switch runner predecessors to declared edges**

In `internal/sloff/runner/runner.go`, replace `taskPredecessorIndices` (whole function, including its comment):

```go
// taskPredecessorIndices returns, for each task index in ordered, the indices
// of its declared dependencies (ADR-0013). Same edge source depgraph.Build
// uses; we recompute it here so the runner stays decoupled from depgraph's
// internal edge representation.
func taskPredecessorIndices(ordered []depgraph.Task) [][]int {
	byRef := make(map[depgraph.TaskRef]int, len(ordered))
	for i, t := range ordered {
		byRef[t.Ref()] = i
	}
	preds := make([][]int, len(ordered))
	for i, t := range ordered {
		seen := map[int]struct{}{}
		for _, dep := range t.DependsOn {
			p, ok := byRef[dep]
			if !ok || p == i {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			preds[i] = append(preds[i], p)
		}
	}
	return preds
}
```

Also update two stale comments in the same file:
1. Package comment line `// Package runner orchestrates spec discovery, preflight, dependency-graph derivation` → `// Package runner orchestrates spec discovery, preflight, declared-dependency DAG construction`
2. `runTasks` doc comment sentence `A task starts as soon as every task that produces one of its inputs has finished — depgraph already sorted them, so we only need to re-derive each task's predecessor set from the same output→producer mapping it used.` → `A task starts as soon as every declared dependency has finished — depgraph already sorted them, so we only need to look up each task's DependsOn indices.`
3. `collectTasks` doc comment: replace `Folding extras in here is what lets depgraph wire up workspace-tool build tasks to their consumers via the usual output-overlap rule, instead of needing a parallel dependency channel.` with `Folding extras in here keeps resolver-contributed sources inside files_hash and makes them visible to overlap validation (ADR-0013 D3).`

- [ ] **Step 4: Switch `explain.Edges` to declared edges (tests first)**

In `internal/sloff/explain/explain_test.go`:
- Update the `task` helper and affected tests to declare deps. Replace the helper and the listed tests:

```go
func task(spec, name string, in, out []string) depgraph.Task {
	return depgraph.Task{SpecRelpath: spec, Name: name, Inputs: in, Outputs: out}
}

func taskD(spec, name string, in, out []string, deps ...depgraph.TaskRef) depgraph.Task {
	return depgraph.Task{SpecRelpath: spec, Name: name, Inputs: in, Outputs: out, DependsOn: deps}
}

func dref(spec, name string) depgraph.TaskRef {
	return depgraph.TaskRef{SpecRelpath: spec, Name: name}
}
```

- `TestEdges_SingleEdgeCarriesIntersectionFile`: consumer task becomes `taskD("svc", "consumer", []string{"shared.pb.go"}, []string{"out.go"}, dref("svc", "producer"))`. Expected edge unchanged.
- `TestEdges_MultipleJustifyingFilesDeduplicateAndSort`: task B becomes `taskD("", "B", []string{"a.pb.go", "b.pb.go", "other.in"}, []string{"final.go"}, dref("", "A"))`. Expected unchanged.
- `TestEdges_DiamondOrdersByToThenFrom`: task C becomes `taskD("", "C", []string{"b.out", "a.out"}, []string{"c.out"}, dref("", "B"), dref("", "A"))` (declare in B,A order to prove output ordering is still A→C then B→C).
- `TestEdges_SelfReferenceIsIgnored`: delete (self-reference is now rejected at spec load; Edges never sees it). Replace with:

```go
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
```

- `TestEdge_LabelSampleEmptyFilesYieldsEmpty` → rename/replace:

```go
func TestEdge_LabelSampleEmptyFilesYieldsDeclared(t *testing.T) {
	e := explain.Edge{}
	if got := e.LabelSample(); got != "(declared)" {
		t.Errorf("expected (declared), got %q", got)
	}
}
```

- `TestRenderMermaid_NodeAndEdgeOrderDeterministic` / `TestRenderDOT_QuotedLabelsMatchMermaidOrdering`: the consumer task in both becomes `taskD("svc", "consumer", []string{"shared.pb.go"}, []string{"out.go"}, dref("svc", "producer"))`; expected strings unchanged.

Run: `go test ./internal/sloff/explain/... -v` → Expected: FAIL.

Then in `internal/sloff/explain/explain.go`:

Replace the package comment:

```go
// Package explain projects depgraph tasks into their declared dependency
// edges (ADR-0013) plus the file-overlap evidence observable for each edge.
// The renderers in this package consume that projection to emit Mermaid /
// DOT for `sloff graph`; the same projection is the seed for the future
// `sloff run --explain`.
package explain
```

Replace the `Edge` doc comment and `Edges` function:

```go
// Edge is a single declared dependency. Files is the observed O_From ∩ I_To
// — every repo-relative path that evidences the edge in the current tree —
// sorted ascending. Files may be empty on a clean checkout (the generated
// files don't exist yet); the edge still renders, captioned "(declared)".
type Edge struct {
	From  TaskRef
	To    TaskRef
	Files []string
}
```

```go
// Edges projects each task's declared depends entries into renderable edges,
// attaching the observed file-overlap evidence when the current tree allows
// computing it. Edge ordering is deterministic — by To, then by From — and
// files within each edge are sorted ascending, so the output is suitable for
// byte-stable goldens.
func Edges(tasks []depgraph.Task) []Edge {
	if len(tasks) == 0 {
		return nil
	}
	byRef := make(map[TaskRef]int, len(tasks))
	for i, t := range tasks {
		byRef[taskRefOf(t)] = i
	}
	outputSets := make([]map[string]struct{}, len(tasks))
	for i, t := range tasks {
		set := make(map[string]struct{}, len(t.Outputs))
		for _, o := range t.Outputs {
			set[o] = struct{}{}
		}
		outputSets[i] = set
	}
	var out []Edge
	for i, t := range tasks {
		for _, dep := range t.DependsOn {
			from := TaskRef{SpecRelpath: dep.SpecRelpath, Name: dep.Name}
			j, ok := byRef[from]
			if !ok {
				// Unresolvable refs are rejected by spec.ValidateDependReferences;
				// a caller bypassing that check gets the edge skipped, not a panic.
				continue
			}
			var files []string
			for _, in := range t.Inputs {
				if _, hit := outputSets[j][in]; hit {
					files = append(files, in)
				}
			}
			sort.Strings(files)
			out = append(out, Edge{From: from, To: taskRefOf(tasks[i]), Files: files})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].To != out[j].To {
			return lessRef(out[i].To, out[j].To)
		}
		return lessRef(out[i].From, out[j].From)
	})
	return out
}
```

Update `LabelSample` (and its doc comment's first sentence accordingly):

```go
	switch len(e.Files) {
	case 0:
		return "(declared)"
	case 1:
		return e.Files[0]
	default:
		return fmt.Sprintf("%s (+%d more)", e.Files[0], len(e.Files)-1)
	}
```

Run: `go test ./internal/sloff/explain/... -v` → Expected: PASS.

- [ ] **Step 5: Add depends to graph fixtures**

`testdata/e2e/graph/simple-chain-mermaid/initial/spec/sloff.yml` and `testdata/e2e/graph/simple-chain-dot/initial/spec/sloff.yml` — append to the `consumer` command:

```yaml
    depends:
      - task: producer
```

`testdata/e2e/graph/multi-deps-mermaid/initial/spec/sloff.yml` — same `depends` block on `consumer`.

`testdata/e2e/graph/pnpmlocal-build-chain-mermaid/initial/sloff.yml` — append to the `gen` command:

```yaml
    depends:
      - task: build-codegen
```

Update the `TestGraph_PnpmLocal_BuildChain_Mermaid` docstring in `cmd/sloff/graph_test.go` to:

```go
// TestGraph_PnpmLocal_BuildChain_Mermaid validates that resolver-contributed
// ExtraInputs are visible to the graph subcommand: the gen task pulls
// @org/codegen via pnpm-local and declares depends on build-codegen; the
// rendered edge carries the dist files as overlap evidence because the
// resolver folded them into gen's inputs. The fixture deliberately omits
// node_modules/.pnpm/lock.yaml to cover the "graph remains usable when
// install state is drifted" claim from runner.Plan's docstring.
```

- [ ] **Step 6: Fix the two inline runner chain tests**

In `internal/sloff/runner/runner_test.go`:

`TestRunner_FallbackLoadServesTransitiveCacheHitAfterUpstreamRegen` — the `downstream` command in the inline YAML gains depends:

```go
	yml := `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: upstream
    cmd: ["sh", "-c", "sed 's/^/out-from-/' marker.txt > a-output.txt"]
    inputs: ["marker.txt"]
    outputs: ["a-output.txt"]
    tools: [versioner]
  - name: downstream
    cmd: ["sh", "-c", "sed 's/^/b-from-/' a-output.txt > b-output.txt"]
    inputs: ["a-output.txt"]
    outputs: ["b-output.txt"]
    tools: [versioner]
    depends:
      - task: upstream
`
```

`TestRunner_FlushPersistsRecordsAfterPartialFailure` — declare the edge and delete the `good.txt` placeholder hack (the pre-write and the two comments explaining it; the hack existed precisely because clean-state overlap derivation lost the edge — declared depends makes it obsolete). Remove this block:

```go
	// Pre-place a `good.txt` placeholder so depgraph sees the file at
	// planning time and wires fail-task → ok-task. ...
	if err := os.WriteFile(filepath.Join(specDir, "good.txt"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
```

Replace the comment above `yml` with:

```go
	// fail-task declares depends on ok-task so the scheduler guarantees
	// ok-task completes (and enqueues its record) before fail-task starts
	// failing (ADR-0013: ordering comes from the declaration, not from
	// file overlap, so no placeholder file is needed on a clean dir).
```

And the inline YAML's `fail-task` gains:

```yaml
    depends:
      - task: ok-task
```

- [ ] **Step 7: Update graph cmd help text**

In `cmd/sloff/graph.go`, `Short` / `Long`:

```go
		Short: "Render the declared task DAG (Mermaid or DOT)",
		Long: `graph emits the dependency DAG declared via each task's depends
entries for every discovered sloff.yml. Each edge is annotated with a
sample of the files in the producer's outputs ∩ consumer's inputs
intersection when those files exist, so "why does B depend on A?" can
be answered without reading every spec; edges whose evidence is not
observable in the current tree are captioned "(declared)".

The subcommand is meant to remain useful in broken environments:
preflight (install drift) and resolver Versions (e.g. <bin> --version
for the script channel) are both skipped, since their failures don't
affect the graph and drift / missing binaries are exactly what the
user is trying to debug. Resolver Inputs are still resolved — failures
there mean the graph would be missing overlap evidence, so they fail
loud.`,
```

- [ ] **Step 8: Run the full suite and reconcile goldens**

Run: `go test ./...`
Expected: graph goldens should already match (same edges, same evidence). If a golden diff appears, inspect it — only label/ordering changes consistent with this task are acceptable — then refresh:

```bash
go test ./cmd/sloff/... -update-graph
go test ./internal/sloff/runner/... -update   # only if a runner golden legitimately changed (none expected)
go test ./...
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/sloff/depgraph/ internal/sloff/runner/ internal/sloff/explain/ cmd/sloff/ testdata/e2e/graph/
git commit -m "feat!: order tasks by declared depends instead of file overlap

Per ADR-0013 D2, depgraph.Build and the runner scheduler consume only
spec-declared depends edges; inputs/outputs overlap no longer creates
edges, so ordering is identical on clean and generated checkouts.
explain/graph render declared edges, captioned \"(declared)\" when the
overlap evidence is not observable in the current tree."
```

---

### Task 6: plan-time validation — fail Run on undeclared dependencies, warn in graph

**Files:**
- Modify: `internal/sloff/runner/runner.go` (`Run`, `Plan`, `missingDependsError`)
- Modify: `cmd/sloff/graph.go`
- Modify: `internal/sloff/runner/runner_test.go` (harness `expectError` option + new test)
- Create: `testdata/e2e/runner/depends-missing-plan-error/initial/spec/{sloff.yml,input.txt,produced.txt}`

- [ ] **Step 1: Extend the E2E harness with error assertion**

In `internal/sloff/runner/runner_test.go`, replace `runStepConfig`, the option funcs, and the tail of `runStep`:

```go
type runStepConfig struct {
	force   bool
	wantErr string
}

type runStepOption func(*runStepConfig)

// withForce flips Options.Force for this runStep so it exercises the ADR-0012
// fingerprint bypass path.
func withForce() runStepOption {
	return func(c *runStepConfig) { c.force = true }
}

// expectError makes the step assert that Run fails with an error containing
// substr, instead of failing the test on error.
func expectError(substr string) runStepOption {
	return func(c *runStepConfig) { c.wantErr = substr }
}
```

and in `runStep`, replace

```go
		if err := r.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
```

with

```go
		err = r.Run(context.Background())
		if cfg.wantErr != "" {
			if err == nil {
				t.Fatalf("Run: expected error containing %q, got nil", cfg.wantErr)
			}
			if !strings.Contains(err.Error(), cfg.wantErr) {
				t.Fatalf("Run: error %q does not contain %q", err, cfg.wantErr)
			}
			return
		}
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
```

- [ ] **Step 2: Create the fixture and the failing test**

`testdata/e2e/runner/depends-missing-plan-error/initial/spec/input.txt`:

```
hello
```

`testdata/e2e/runner/depends-missing-plan-error/initial/spec/produced.txt` (committed so the overlap is observable at plan time):

```
hello
```

`testdata/e2e/runner/depends-missing-plan-error/initial/spec/sloff.yml`:

```yaml
tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: producer
    cmd: ["sh", "-c", "cp input.txt produced.txt"]
    inputs: ["input.txt"]
    outputs: ["produced.txt"]
    tools: [versioner]
  - name: consumer
    cmd: ["sh", "-c", "cp produced.txt final.txt"]
    inputs: ["produced.txt"]
    outputs: ["final.txt"]
    tools: [versioner]
```

Append to `runner_test.go` (near the other E2E tests):

```go
// TestRunner_DependsMissingAtPlanTimeErrors locks ADR-0013 D3's plan-time
// check: produced.txt exists on disk, so the consumer/producer overlap is
// observable before execution and Run must fail — with the exact depends
// entry to add — without executing any task.
func TestRunner_DependsMissingAtPlanTimeErrors(t *testing.T) {
	runE2E(t, "depends-missing-plan-error",
		runStep(expectError("undeclared task dependencies")),
	)
}
```

Run: `go test ./internal/sloff/runner/... -run TestRunner_DependsMissingAtPlanTimeErrors -v`
Expected: FAIL (Run succeeds; no plan-time check yet)

- [ ] **Step 3: Implement the plan-time check and the new Plan signature**

In `internal/sloff/runner/runner.go`:

In `Run`, immediately after the `depgraphBuildTraced` call:

```go
	ordered, err := r.depgraphBuildTraced(ctx, tasks)
	if err != nil {
		return err
	}
	// Plan-time half of ADR-0013 D3: with the current tree's files, every
	// observable producer→consumer overlap must be covered by a declared
	// depends edge. The run-time half (validateProducedDependencies) covers
	// what a clean checkout hides from this check.
	if missing := depgraph.FindMissingDependencies(ordered); len(missing) > 0 {
		return missingDependsError(missing)
	}
```

Add the helper near `taskLabel`:

```go
// missingDependsError aggregates undeclared-dependency violations into one
// actionable error (ADR-0013 D3: depends-missing is a hard failure).
func missingDependsError(missing []depgraph.MissingDependency) error {
	lines := make([]string, len(missing))
	for i, m := range missing {
		lines[i] = depgraph.FormatMissing(m)
	}
	return fmt.Errorf("undeclared task dependencies detected:\n  %s", strings.Join(lines, "\n  "))
}
```

Change `Plan` to also return the validation result (graph downgrades it to a warning — ADR-0013 D3):

```go
// Plan resolves all discovered specs into a topologically-ordered task list
// without running preflight or executing any cmd, plus the overlap-validation
// findings for the current tree. Callers decide severity: Run fails on a
// non-empty missing list, `sloff graph` prints warnings and still renders
// (the graph is a debugging surface for exactly this kind of spec problem).
//
// Plan deliberately calls `Registry.Inputs` only (not `Versions`) because
// the depgraph never reads ResolvedVersions — they only feed
// `resolved_versions_hash` (architecture.md, ADR-0008 D6 addendum). Skipping
// Versions means `script` resolvers don't spawn `<bin> --version` here, which
// keeps graph-style consumers usable when prebuilt binaries aren't installed.
//
// Preflight is intentionally skipped for the same reason: debugging tools
// that read the depgraph must remain useful when the install state is
// drifted, since drift is one of the conditions users reach for the graph
// to investigate.
func (r *Runner) Plan(ctx context.Context) ([]depgraph.Task, []depgraph.MissingDependency, error) {
	registry, referencedToolNames, err := r.prepareRegistry()
	if err != nil {
		return nil, nil, err
	}
	inputsByTool, err := r.resolveInputContribs(ctx, registry, referencedToolNames)
	if err != nil {
		return nil, nil, err
	}
	tasks, err := r.collectTasksTraced(ctx, inputsByTool, nil)
	if err != nil {
		return nil, nil, err
	}
	ordered, err := r.depgraphBuildTraced(ctx, tasks)
	if err != nil {
		return nil, nil, err
	}
	return ordered, depgraph.FindMissingDependencies(ordered), nil
}
```

- [ ] **Step 4: Update the graph cmd**

In `cmd/sloff/graph.go`:

Add `"github.com/izumin5210/sloff/internal/sloff/depgraph"` to imports.

`RunE` passes stderr through:

```go
		RunE: func(cobraCmd *cobra.Command, _ []string) error {
			return graphE(cobraCmd.Context(), cobraCmd.OutOrStdout(), cobraCmd.ErrOrStderr(), root, pattern, format)
		},
```

`graphE` signature and the Plan call site:

```go
func graphE(ctx context.Context, out, errOut io.Writer, rawRoot, pattern, format string) (err error) {
```

```go
	tasks, missing, err := r.Plan(ctx)
	if err != nil {
		return err
	}
	// ADR-0013 D3: graph downgrades the depends-missing check to a warning so
	// the DAG stays inspectable while the user fixes the spec.
	for _, m := range missing {
		fmt.Fprintf(errOut, "warning: %s\n", depgraph.FormatMissing(m))
	}
	edges := explain.Edges(tasks)
```

- [ ] **Step 5: Generate the golden and run the suite**

```bash
go test ./internal/sloff/runner/... -run TestRunner_DependsMissingAtPlanTimeErrors -update
go test ./...
```

Expected: PASS. The new fixture's `expected/` equals `initial/` (the run fails before executing anything — verify the snapshot contains no `.sloff/` directory and no `final.txt`).

- [ ] **Step 6: Commit**

```bash
git add internal/sloff/runner/ cmd/sloff/graph.go testdata/e2e/runner/depends-missing-plan-error/
git commit -m "feat(runner): fail on undeclared dependencies at plan time

sloff graph downgrades the same finding to a stderr warning so the DAG
stays inspectable while the spec is being fixed (ADR-0013 D3)."
```

---

### Task 7: run-time validation + unobserved-depends warning

**Files:**
- Modify: `internal/sloff/runner/runner.go`
- Modify: `internal/sloff/runner/runner_test.go` (harness `withReadOnly` / `expectWarn`, 2 new tests)
- Modify: `internal/sloff/runner/depends_internal_test.go` (`taskReadsPath` tests)
- Create: `testdata/e2e/runner/depends-missing-runtime-error/initial/spec/{sloff.yml,seed.txt}`
- Create: `testdata/e2e/runner/depends-unobserved-warning/initial/spec/{sloff.yml,seed.txt}`

- [ ] **Step 1: Unit tests for `taskReadsPath` (failing first)**

Append to `internal/sloff/runner/depends_internal_test.go`:

```go
func TestTaskReadsPath_LiteralPatternMatchesCleanStatePath(t *testing.T) {
	info := taskInfo{
		specRelpath: "spec",
		command:     spec.Command{Inputs: []string{"a-out.txt"}},
	}
	// Clean state: the produced file was NOT in the expanded input set, only
	// the pattern can see it.
	if !taskReadsPath(info, map[string]struct{}{}, "spec/a-out.txt") {
		t.Error("expected literal pattern to match the produced path")
	}
}

func TestTaskReadsPath_GlobPatternMatches(t *testing.T) {
	info := taskInfo{
		specRelpath: "proto/svc",
		command:     spec.Command{Inputs: []string{"../../gen/**/*.pb.go"}},
	}
	if !taskReadsPath(info, map[string]struct{}{}, "gen/foo/bar.pb.go") {
		t.Error("expected glob pattern to match")
	}
	if taskReadsPath(info, map[string]struct{}{}, "gen/foo/bar.txt") {
		t.Error("non-matching path must not match")
	}
}

func TestTaskReadsPath_ExpandedInputSetMatches(t *testing.T) {
	info := taskInfo{specRelpath: "spec", command: spec.Command{Inputs: []string{"unrelated.txt"}}}
	set := map[string]struct{}{"spec/extra-input.go": {}} // e.g. resolver ExtraInputs
	if !taskReadsPath(info, set, "spec/extra-input.go") {
		t.Error("expected expanded input set to match")
	}
}
```

Run: `go test ./internal/sloff/runner/... -run TestTaskReadsPath -v` → Expected: FAIL (undefined `taskReadsPath`).

- [ ] **Step 2: Implement run-time validation**

In `internal/sloff/runner/runner.go`:

Add `"github.com/bmatcuk/doublestar/v4"` to imports.

Change the `producedBy` field (and its comment's last sentence stays):

```go
	producedByMu sync.Mutex
	producedBy   map[string]depgraph.TaskRef
```

Change `recordProducedPaths` to take the producer's ref:

```go
// recordProducedPaths registers the resolved output paths of a task and fails when one
// of those paths was already produced by a different task in this run. This catches spec
// conflicts that depgraph cannot see at planning time on a clean checkout, where the
// pre-run glob expansion of generated files comes back empty. Protected by producedByMu
// so concurrent runTask goroutines don't race on the shared map.
func (r *Runner) recordProducedPaths(producer depgraph.TaskRef, paths []string) error {
	r.producedByMu.Lock()
	defer r.producedByMu.Unlock()
	for _, p := range paths {
		if existing, exists := r.producedBy[p]; exists && existing != producer {
			return fmt.Errorf("duplicate output %q produced by %s and %s; fix the spec to give each generated path exactly one writer", p, existing.Label(), producer.Label())
		}
		r.producedBy[p] = producer
	}
	return nil
}
```

In `runTask`, replace `taskLabel := taskLabel(t)` with `ref := t.Ref()` and update both call sites to `r.recordProducedPaths(ref, paths)` / `r.recordProducedPaths(ref, outputPaths)`. (The local `taskLabel` shadow disappears; the package-level `taskLabel(t)` func stays for `detectOutputPatternConflicts`.)

In `Run`, replace

```go
	r.producedBy = map[string]string{}
	runErr := r.runTasks(ctx, ordered)
```

with

```go
	r.producedBy = map[string]depgraph.TaskRef{}
	runErr := r.runTasks(ctx, ordered)
	// Run-time half of ADR-0013 D3: validate against what was actually
	// produced (clean checkouts hide everything from the plan-time check).
	if depErr := r.validateProducedDependencies(ordered); depErr != nil {
		runErr = errors.Join(runErr, depErr)
	}
	if runErr == nil {
		r.warnUnobservedDepends(ordered)
	}
```

Add the three functions (near `recordProducedPaths`):

```go
// validateProducedDependencies is the run-time half of ADR-0013 D3's
// depends-missing check. Plan-time validation only sees files that already
// exist; here every path actually produced during this run (fingerprint-hit
// tasks included — their recorded outputs also pass through
// recordProducedPaths) is matched against every other task's input surface.
// A match without a declared depends edge means this run may have executed
// in the wrong order — fail loudly with the exact entry to add.
func (r *Runner) validateProducedDependencies(ordered []depgraph.Task) error {
	r.producedByMu.Lock()
	produced := make(map[string]depgraph.TaskRef, len(r.producedBy))
	for p, ref := range r.producedBy {
		produced[p] = ref
	}
	r.producedByMu.Unlock()
	if len(produced) == 0 {
		return nil
	}
	paths := make([]string, 0, len(produced))
	for p := range produced {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var missing []depgraph.MissingDependency
	for _, t := range ordered {
		consumer := t.Ref()
		info := r.byKey[depgraphKey(t)]
		declared := make(map[depgraph.TaskRef]struct{}, len(t.DependsOn))
		for _, d := range t.DependsOn {
			declared[d] = struct{}{}
		}
		inputSet := make(map[string]struct{}, len(info.inputPaths))
		for _, p := range info.inputPaths {
			inputSet[p] = struct{}{}
		}
		byProducer := map[depgraph.TaskRef][]string{}
		for _, p := range paths {
			producer := produced[p]
			if producer == consumer {
				continue
			}
			if _, ok := declared[producer]; ok {
				continue
			}
			if !taskReadsPath(info, inputSet, p) {
				continue
			}
			byProducer[producer] = append(byProducer[producer], p)
		}
		producers := make([]depgraph.TaskRef, 0, len(byProducer))
		for ref := range byProducer {
			producers = append(producers, ref)
		}
		sort.Slice(producers, func(i, j int) bool { return producers[i].Label() < producers[j].Label() })
		for _, ref := range producers {
			missing = append(missing, depgraph.MissingDependency{Producer: ref, Consumer: consumer, Files: byProducer[ref]})
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return missingDependsError(missing)
}

// taskReadsPath reports whether produced path p belongs to the task's input
// surface: either it was already in the expanded input set at collect time,
// or it matches one of the declared input patterns — the clean-state case,
// where the file did not exist when globs were expanded and only the pattern
// can see it. Pattern-vs-path matching is exact and cheap (unlike the
// glob-vs-glob intersection ADR-0004 D3 rejected).
func taskReadsPath(info taskInfo, inputSet map[string]struct{}, p string) bool {
	if _, ok := inputSet[p]; ok {
		return true
	}
	slashPath := filepath.ToSlash(p)
	specDir := filepath.ToSlash(info.specRelpath)
	for _, pattern := range info.command.Inputs {
		joined := path.Join(specDir, pattern)
		if joined == ".." || strings.HasPrefix(joined, "../") {
			continue // already rejected by glob.Expand at collect time
		}
		if ok, err := doublestar.Match(joined, slashPath); err == nil && ok {
			return true
		}
	}
	return false
}

// warnUnobservedDepends emits ADR-0013 D3's "inputs omission" warning: a
// declared depends edge whose producer ran in this run, yet none of its
// produced paths landed in the consumer's input surface. That usually means
// the consumer's inputs are missing the upstream's generated files, so the
// upstream can change without invalidating the consumer's fingerprint.
// Conditional outputs (ADR-0004 D2) can legitimately produce zero overlap,
// hence a warning rather than an error.
func (r *Runner) warnUnobservedDepends(ordered []depgraph.Task) {
	r.producedByMu.Lock()
	producedByRef := map[depgraph.TaskRef][]string{}
	for p, ref := range r.producedBy {
		producedByRef[ref] = append(producedByRef[ref], p)
	}
	r.producedByMu.Unlock()
	for _, refPaths := range producedByRef {
		sort.Strings(refPaths)
	}

	for _, t := range ordered {
		info := r.byKey[depgraphKey(t)]
		inputSet := make(map[string]struct{}, len(info.inputPaths))
		for _, p := range info.inputPaths {
			inputSet[p] = struct{}{}
		}
		for _, dep := range t.DependsOn {
			outs, ran := producedByRef[dep]
			if !ran {
				continue
			}
			overlap := false
			for _, p := range outs {
				if taskReadsPath(info, inputSet, p) {
					overlap = true
					break
				}
			}
			if !overlap {
				r.logger.Warnf("%s depends on %s but none of the files it produced match this task's inputs; if the dependency is real, add the upstream outputs to inputs (the fingerprint cannot invalidate otherwise); conditional outputs (ADR-0004 D2) can also cause this",
					t.Ref().Label(), dep.Label())
			}
		}
	}
}
```

Run: `go test ./internal/sloff/runner/... -run TestTaskReadsPath -v` → Expected: PASS.
Run: `go test ./...` → Expected: PASS (no fixture exercises the new paths yet).

- [ ] **Step 3: Harness options for ReadOnly and warning capture**

In `internal/sloff/runner/runner_test.go`:

Add `"sync"` to imports. Extend the harness:

```go
type runStepConfig struct {
	force    bool
	readOnly bool
	wantErr  string
	wantWarn string
}

// withReadOnly sets Options.ReadOnly so no fingerprint record is written.
// Used by fixtures whose record bytes would otherwise depend on scheduling
// order (e.g. whether an upstream output existed when a racing task hashed
// its inputs).
func withReadOnly() runStepOption {
	return func(c *runStepConfig) { c.readOnly = true }
}

// expectWarn asserts that Run logs at least one warning containing substr.
func expectWarn(substr string) runStepOption {
	return func(c *runStepConfig) { c.wantWarn = substr }
}

// captureLogger records warnings for expectWarn assertions while discarding
// info/error chatter.
type captureLogger struct {
	mu    sync.Mutex
	warns []string
}

func (c *captureLogger) Infof(format string, args ...any) {}
func (c *captureLogger) Errorf(format string, args ...any) {}
func (c *captureLogger) Warnf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.warns = append(c.warns, fmt.Sprintf(format, args...))
}
```

In `runStep`, pass the new knobs and assert. The `runner.New` call gains:

```go
			logs := &captureLogger{}
			r := runner.New(runner.Options{
				RepoRoot:  h.workdir,
				Specs:     specs,
				Storage:   local.New(h.workdir, local.WithClock(func() time.Time { return fixedClock })),
				Resolvers: resolverReg,
				Preflight: preflightReg,
				Force:     cfg.force,
				ReadOnly:  cfg.readOnly,
				Logger:    logs,
			})
```

and after the error assertion block from Task 6, add:

```go
		if cfg.wantWarn != "" {
			logs.mu.Lock()
			warns := append([]string(nil), logs.warns...)
			logs.mu.Unlock()
			found := false
			for _, w := range warns {
				if strings.Contains(w, cfg.wantWarn) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("Run: no warning containing %q; warnings: %v", cfg.wantWarn, warns)
			}
		}
```

(Note: setting `Logger` means runner log lines no longer reach stderr in these tests; existing tests don't assert on log output, so this is safe to apply unconditionally.)

- [ ] **Step 4: Fixtures + failing E2E tests**

`testdata/e2e/runner/depends-missing-runtime-error/initial/spec/seed.txt`:

```
seed
```

`testdata/e2e/runner/depends-missing-runtime-error/initial/spec/sloff.yml` — clean state: `a-out.txt` does not exist, so plan-time validation sees nothing; the consumer's cmd succeeds regardless of order (no read), so only the run-time check can catch the omission:

```yaml
tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: producer
    cmd: ["sh", "-c", "echo generated > a-out.txt"]
    inputs: ["seed.txt"]
    outputs: ["a-out.txt"]
    tools: [versioner]
  - name: consumer
    cmd: ["sh", "-c", "echo fixed > b-out.txt"]
    inputs: ["a-out.txt", "seed.txt"]
    outputs: ["b-out.txt"]
    tools: [versioner]
```

`testdata/e2e/runner/depends-unobserved-warning/initial/spec/seed.txt`:

```
seed
```

`testdata/e2e/runner/depends-unobserved-warning/initial/spec/sloff.yml` — consumer declares the edge but its inputs don't reference anything the producer generates:

```yaml
tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: producer
    cmd: ["sh", "-c", "echo generated > a-out.txt"]
    inputs: ["seed.txt"]
    outputs: ["a-out.txt"]
    tools: [versioner]
  - name: consumer
    cmd: ["sh", "-c", "echo fixed > b-out.txt"]
    inputs: ["seed.txt"]
    outputs: ["b-out.txt"]
    tools: [versioner]
    depends:
      - task: producer
```

Tests:

```go
// TestRunner_DependsMissingAtRunTimeErrors covers the clean-checkout hole the
// plan-time check cannot see: a-out.txt does not exist at plan time, so the
// overlap is only discoverable after the producer actually writes it. The
// run must end in the undeclared-dependency error. ReadOnly keeps the golden
// deterministic: the consumer's record bytes would otherwise depend on
// whether a-out.txt existed when its inputs were hashed (scheduling race).
func TestRunner_DependsMissingAtRunTimeErrors(t *testing.T) {
	runE2E(t, "depends-missing-runtime-error",
		runStep(withReadOnly(), expectError("undeclared task dependencies")),
	)
}

// TestRunner_DependsWithoutObservedOverlapWarns locks ADR-0013 D3's inputs-
// omission warning: the declared edge is honored for ordering, but no
// produced file lands in the consumer's input surface, so the consumer's
// fingerprint cannot invalidate when the producer changes — warn, don't fail
// (conditional outputs can legitimately look like this).
func TestRunner_DependsWithoutObservedOverlapWarns(t *testing.T) {
	runE2E(t, "depends-unobserved-warning",
		runStep(expectWarn("none of the files it produced match")),
	)
}
```

Run: `go test ./internal/sloff/runner/... -run 'TestRunner_DependsMissingAtRunTime|TestRunner_DependsWithoutObserved' -v`
Expected: FAIL only on missing `expected/` goldens (the behavior itself was implemented in Step 2; if the assertions themselves fail, fix the implementation before generating goldens).

- [ ] **Step 5: Generate goldens, full suite**

```bash
go test ./internal/sloff/runner/... -run 'TestRunner_DependsMissingAtRunTime|TestRunner_DependsWithoutObserved' -update
go test ./...
```

Expected: PASS. Sanity-check the snapshots:
- `depends-missing-runtime-error/expected/` has `a-out.txt` (`generated`), `b-out.txt` (`fixed`), and **no** `.sloff/` directory (ReadOnly).
- `depends-unobserved-warning/expected/` has both outputs **and** `.sloff/fingerprints/spec/{producer,consumer}/` records.

- [ ] **Step 6: Commit**

```bash
git add internal/sloff/runner/ testdata/e2e/runner/depends-missing-runtime-error/ testdata/e2e/runner/depends-unobserved-warning/
git commit -m "feat(runner): validate produced paths against declared depends at run time

Clean checkouts hide every generated file from the plan-time overlap
check; matching actually-produced paths against each task's input
patterns closes that hole (path-vs-glob is exact, unlike the
glob-vs-glob intersection ADR-0004 D3 rejected). Declared edges with no
observed overlap get the ADR-0013 inputs-omission warning."
```

---

### Task 8: remaining E2E — clean-state ordering, cross-spec depends, graph goldens

**Files:**
- Create: `testdata/e2e/runner/depends-clean-state-ordering/initial/spec/{sloff.yml,input.txt}`
- Create: `testdata/e2e/runner/depends-cross-spec/initial/{specA/{sloff.yml,seed.txt},specB/sloff.yml}`
- Create: `testdata/e2e/graph/declared-edge-clean-mermaid/{initial/spec/{sloff.yml,input.txt},expected.txt}`
- Create: `testdata/e2e/graph/missing-depends-warning-mermaid/{initial/spec/{sloff.yml,input.txt,produced.txt},expected.txt}`
- Modify: `internal/sloff/runner/runner_test.go`, `cmd/sloff/graph_test.go`

- [ ] **Step 1: Clean-state ordering fixture (the motivating case)**

`testdata/e2e/runner/depends-clean-state-ordering/initial/spec/input.txt`:

```
hello
```

`testdata/e2e/runner/depends-clean-state-ordering/initial/spec/sloff.yml` — no generated file exists; the consumer's cmd hard-fails if it runs before the producer (`cp` of a missing file), so a green run proves the declared ordering:

```yaml
tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: producer
    cmd: ["sh", "-c", "cp input.txt produced.txt"]
    inputs: ["input.txt"]
    outputs: ["produced.txt"]
    tools: [versioner]
  - name: consumer
    cmd: ["sh", "-c", "cp produced.txt final.txt"]
    inputs: ["produced.txt"]
    outputs: ["final.txt"]
    tools: [versioner]
    depends:
      - task: producer
```

Test (two runs: first exercises clean-state ordering, second exercises fingerprint hits flowing through recordProducedPaths and the run-time validation):

```go
// TestRunner_DependsCleanStateOrdering is the ADR-0013 motivating scenario:
// no generated file exists, yet the declared edge orders producer before
// consumer (the old overlap derivation found no edge here and the consumer's
// `cp` would race and fail). The second run must be a full fingerprint hit
// and still pass run-time validation.
func TestRunner_DependsCleanStateOrdering(t *testing.T) {
	runE2E(t, "depends-clean-state-ordering",
		runStep(),
		runStep(),
	)
}
```

- [ ] **Step 2: Cross-spec depends fixture**

`testdata/e2e/runner/depends-cross-spec/initial/specA/seed.txt`:

```
seed
```

`testdata/e2e/runner/depends-cross-spec/initial/specA/sloff.yml`:

```yaml
tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: producer
    cmd: ["sh", "-c", "cp seed.txt a.txt"]
    inputs: ["seed.txt"]
    outputs: ["a.txt"]
    tools: [versioner]
```

`testdata/e2e/runner/depends-cross-spec/initial/specB/sloff.yml` (references the tool and the task across spec dirs; clean state):

```yaml
commands:
  - name: consumer
    cmd: ["sh", "-c", "cp ../specA/a.txt b.txt"]
    inputs: ["../specA/a.txt"]
    outputs: ["b.txt"]
    tools: [versioner]
    depends:
      - spec: ../specA
        task: producer
```

Test:

```go
// TestRunner_DependsCrossSpec covers the {spec: ../dir, task: name} reference
// form end to end on a clean checkout: resolution against the declaring
// file's dir, cross-dir ordering, and per-spec record layout.
func TestRunner_DependsCrossSpec(t *testing.T) {
	runE2E(t, "depends-cross-spec", runStep())
}
```

- [ ] **Step 3: Graph fixture — declared edge without observable overlap**

`testdata/e2e/graph/declared-edge-clean-mermaid/initial/spec/input.txt`:

```
hello
```

`testdata/e2e/graph/declared-edge-clean-mermaid/initial/spec/sloff.yml` (same chain as simple-chain-mermaid **plus** depends, **minus** the committed `produced.txt` — clean state):

```yaml
tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: producer
    cmd: ["sh", "-c", "cp input.txt produced.txt"]
    inputs: ["input.txt"]
    outputs: ["produced.txt"]
    tools: [versioner]
  - name: consumer
    cmd: ["sh", "-c", "cp produced.txt final.txt"]
    inputs: ["produced.txt"]
    outputs: ["final.txt"]
    tools: [versioner]
    depends:
      - task: producer
```

`testdata/e2e/graph/declared-edge-clean-mermaid/expected.txt`:

```
flowchart TD
    n_spec_consumer["spec:consumer"]
    n_spec_producer["spec:producer"]
    n_spec_producer -->|"(declared)"| n_spec_consumer
```

- [ ] **Step 4: Graph fixture — missing depends prints warning, still renders**

`testdata/e2e/graph/missing-depends-warning-mermaid/initial/spec/input.txt`:

```
hello
```

`testdata/e2e/graph/missing-depends-warning-mermaid/initial/spec/produced.txt`:

```
hello
```

`testdata/e2e/graph/missing-depends-warning-mermaid/initial/spec/sloff.yml` (chain **without** depends, `produced.txt` committed so the overlap is observable):

```yaml
tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: producer
    cmd: ["sh", "-c", "cp input.txt produced.txt"]
    inputs: ["input.txt"]
    outputs: ["produced.txt"]
    tools: [versioner]
  - name: consumer
    cmd: ["sh", "-c", "cp produced.txt final.txt"]
    inputs: ["produced.txt"]
    outputs: ["final.txt"]
    tools: [versioner]
```

`testdata/e2e/graph/missing-depends-warning-mermaid/expected.txt` (nodes only — no declared edge to render):

```
flowchart TD
    n_spec_consumer["spec:consumer"]
    n_spec_producer["spec:producer"]
```

In `cmd/sloff/graph_test.go`, add a stderr-capturing variant and the tests:

```go
// runGraphCmdCaptureStderr is runGraphCmd plus the stderr text, for tests
// asserting on the ADR-0013 depends-missing warning channel.
func runGraphCmdCaptureStderr(t *testing.T, h *graphHarness, extra ...string) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	args := append([]string{"graph", "--root", h.workdir}, extra...)
	root.SetArgs(args)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("graph cmd: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// TestGraph_DeclaredEdgeWithoutObservableOverlap_Mermaid locks the clean-
// checkout rendering: the declared edge appears with the "(declared)"
// caption because no generated file exists to evidence it.
func TestGraph_DeclaredEdgeWithoutObservableOverlap_Mermaid(t *testing.T) {
	h := setupGraphHarness(t, "declared-edge-clean-mermaid")
	got := runGraphCmd(t, h, "--format", "mermaid")
	assertGraphGolden(t, h, got)
}

// TestGraph_MissingDependsWarnsButRenders locks ADR-0013 D3's graph
// downgrade: an observable overlap without a declared edge produces a
// stderr warning (with the suggested depends entry), while stdout still
// renders the node set so the user can inspect the DAG they actually have.
func TestGraph_MissingDependsWarnsButRenders(t *testing.T) {
	h := setupGraphHarness(t, "missing-depends-warning-mermaid")
	stdout, stderr := runGraphCmdCaptureStderr(t, h, "--format", "mermaid")
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "depends: [{task: producer}]") {
		t.Errorf("expected depends warning on stderr, got: %q", stderr)
	}
	assertGraphGolden(t, h, stdout)
}
```

(Add `"strings"` to graph_test.go imports.)

- [ ] **Step 5: Generate goldens, run everything**

```bash
go test ./internal/sloff/runner/... -run 'TestRunner_DependsCleanStateOrdering|TestRunner_DependsCrossSpec' -update
go test ./cmd/sloff/... -run 'TestGraph_DeclaredEdge|TestGraph_MissingDepends' -v
go test ./...
```

Expected: PASS. If the hand-written graph `expected.txt` files mismatch, compare against actual output and fix whichever side is wrong (`-update-graph` regenerates if the actual output is correct).

Sanity-check `depends-clean-state-ordering/expected/`: `produced.txt` and `final.txt` both contain `hello`, and `.sloff/fingerprints/spec/{producer,consumer}/` each hold one record.

- [ ] **Step 6: Commit**

```bash
git add testdata/e2e/ internal/sloff/runner/runner_test.go cmd/sloff/graph_test.go
git commit -m "test: cover clean-state ordering, cross-spec depends, and graph rendering for ADR-0013"
```

---

### Task 9: final sweep

**Files:**
- Modify: leftovers only (see steps)

- [ ] **Step 1: Sweep for stale references to overlap-derived ordering**

Run: `grep -rn "output-overlap\|auto-detect\|overlap rule\|derives a task DAG" internal/ cmd/ --include="*.go"`
Expected hits to fix (update wording to declared-depends + validation; leave historical comments that describe what *was* rejected):
- any remaining doc comments in `internal/sloff/explain/dot.go` header (check `// RenderDOT` comment for "auto-detected")
- `internal/sloff/runner/runner.go` — confirm the Task 5 comment edits landed; fix stragglers

- [ ] **Step 2: Verify everything**

```bash
gofmt -l internal/ cmd/
go vet ./...
go test ./...
```

Expected: gofmt prints nothing; vet and tests pass.

- [ ] **Step 3: Cross-check against ADR-0013**

- D1 syntax + load validation → Tasks 1–2 ✓
- D2 declared-only ordering, cycle error → Task 5 ✓
- D3 plan-time error / run-time error / warning / graph downgrade → Tasks 6–8 ✓
- D4 depends not in input_hash → no hash-path change anywhere (verify `hash.Input` call sites untouched: `git diff main -- internal/sloff/hash` is empty) ✓
- D5 pre-1.0 breaking → fixtures migrated in Task 5 ✓

- [ ] **Step 4: Commit (only if Step 1 changed files)**

```bash
git add -A
git commit -m "chore: sweep stale overlap-derivation wording from comments"
```
