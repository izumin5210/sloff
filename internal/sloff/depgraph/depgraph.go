// Package depgraph builds the task DAG and emits a stable topological order.
// Execution-order edges come from the spec-declared depends entries carried
// on Task.DependsOn (ADR-0013 D2); inputs/outputs file overlap is used only
// for validation (duplicate-producer detection and FindMissingDependencies).
package depgraph

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

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
	// Barrier marks a pure aggregation node (ADR-0017): no cmd, no
	// inputs/outputs, only DependsOn. Barrier nodes participate in ordering and
	// cycle detection like any other node; the runner completes them without
	// executing or fingerprinting anything, and the overlap validations skip
	// them structurally (no inputs to read, no outputs to produce).
	Barrier bool
}

// Ref returns the task's identity key.
func (t Task) Ref() TaskRef { return TaskRef{SpecRelpath: t.SpecRelpath, Name: t.Name} }

// Build returns the tasks in execution order. Edges come exclusively from
// spec-declared DependsOn entries (ADR-0013 D2); inputs/outputs file overlap
// no longer creates edges. Ties between independent tasks are broken
// deterministically by (SpecRelpath, Name). Duplicate-producer detection
// (conflicting output paths) is retained. A cycle or an unknown dependency
// reference yields an error.
func Build(tasks []Task) ([]Task, error) {
	if len(tasks) == 0 {
		return nil, nil
	}

	type idx = int
	keyToIdx := make(map[TaskRef]idx, len(tasks))
	for i, t := range tasks {
		keyToIdx[t.Ref()] = i
	}

	// outputProducer: file path → idx of the task that produces it. Two tasks producing
	// the same path is a spec conflict that would leave execution order undefined and
	// downstream fingerprint decisions wired to the wrong writer; surface every conflicting task.
	outputProducer := make(map[string]idx)
	conflicts := make(map[string][]idx)
	for i, t := range tasks {
		for _, out := range t.Outputs {
			if existing, exists := outputProducer[out]; exists {
				if _, recorded := conflicts[out]; !recorded {
					conflicts[out] = []idx{existing}
				}
				conflicts[out] = append(conflicts[out], i)
				continue
			}
			outputProducer[out] = i
		}
	}
	if len(conflicts) > 0 {
		return nil, conflictError(tasks, conflicts)
	}

	// edges[i] = set of tasks that must run before i (i depends on them)
	edges := make([]map[idx]struct{}, len(tasks))
	for i := range edges {
		edges[i] = map[idx]struct{}{}
	}
	// inDegree[i] = |edges[i]|
	inDegree := make([]int, len(tasks))

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

	// Kahn's algorithm with deterministic tie-breaking.
	ready := make([]idx, 0, len(tasks))
	for i := range tasks {
		if inDegree[i] == 0 {
			ready = append(ready, i)
		}
	}
	sortByKey(ready, tasks)

	out := make([]Task, 0, len(tasks))
	for len(ready) > 0 {
		next := ready[0]
		ready = ready[1:]
		out = append(out, tasks[next])

		// Find consumers of next (tasks where edges[j] contains next).
		var unblocked []idx
		for j, deps := range edges {
			if _, ok := deps[next]; !ok {
				continue
			}
			delete(edges[j], next)
			inDegree[j]--
			if inDegree[j] == 0 {
				unblocked = append(unblocked, j)
			}
		}
		if len(unblocked) > 0 {
			sortByKey(unblocked, tasks)
			ready = append(ready, unblocked...)
			sortByKey(ready, tasks)
		}
	}

	if len(out) != len(tasks) {
		return nil, &CycleError{Tasks: remainingTasks(tasks, out)}
	}
	return out, nil
}

// CycleError is returned by Build when a dependency cycle is detected. Tasks
// holds the task refs that form (or are part of) the cycle. Error() renders
// the same human-readable message as the previous plain-fmt.Errorf form so
// non-tool callers are unchanged; callers that need the task set can type-assert.
type CycleError struct {
	Tasks []TaskRef
}

func (e *CycleError) Error() string {
	labels := make([]string, len(e.Tasks))
	for i, r := range e.Tasks {
		labels[i] = r.Label()
	}
	sort.Strings(labels)
	return fmt.Sprintf("cycle detected involving: %s", strings.Join(labels, ", "))
}

func sortByKey(indices []int, tasks []Task) {
	sort.SliceStable(indices, func(i, j int) bool {
		a, b := tasks[indices[i]].Ref(), tasks[indices[j]].Ref()
		if a.SpecRelpath != b.SpecRelpath {
			return a.SpecRelpath < b.SpecRelpath
		}
		return a.Name < b.Name
	})
}

func conflictError(tasks []Task, conflicts map[string][]int) error {
	paths := make([]string, 0, len(conflicts))
	for p := range conflicts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	parts := make([]string, 0, len(paths))
	for _, p := range paths {
		producers := conflicts[p]
		labels := make([]string, 0, len(producers))
		for _, idx := range producers {
			labels = append(labels, taskLabel(tasks[idx]))
		}
		sort.Strings(labels)
		parts = append(parts, fmt.Sprintf("%s -> [%s]", p, strings.Join(labels, ", ")))
	}
	return fmt.Errorf("duplicate output producers: %s", strings.Join(parts, "; "))
}

func taskLabel(t Task) string { return t.Ref().Label() }

func remainingTasks(all, emitted []Task) []TaskRef {
	emittedSet := make(map[TaskRef]struct{}, len(emitted))
	for _, t := range emitted {
		emittedSet[t.Ref()] = struct{}{}
	}
	var rest []TaskRef
	for _, t := range all {
		if _, ok := emittedSet[t.Ref()]; ok {
			continue
		}
		rest = append(rest, t.Ref())
	}
	return rest
}

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
		byProducer := map[int][]string{}
		for _, in := range t.Inputs {
			j, ok := producer[in]
			if !ok || j == i {
				continue
			}
			// depends lists are short (a handful of entries); a linear scan
			// beats building a set per task.
			if slices.Contains(t.DependsOn, tasks[j].Ref()) {
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
	evidence := FileSample(m.Files)
	if evidence == "" {
		evidence = "generated files"
	}
	return fmt.Sprintf("%s reads %s produced by %s but does not declare the dependency; add %s to %q in %s",
		m.Consumer.Label(), evidence, m.Producer.Label(),
		suggestedDependEntry(m), m.Consumer.Name, specYAMLPath(m.Consumer))
}

// MissingDependenciesError aggregates undeclared-dependency findings into one
// actionable error (ADR-0013 D3: a missing depends declaration is a hard
// failure for `sloff run`; `sloff graph` renders the same findings as
// warnings instead).
func MissingDependenciesError(missing []MissingDependency) error {
	lines := make([]string, len(missing))
	for i, m := range missing {
		lines[i] = FormatMissing(m)
	}
	return fmt.Errorf("undeclared task dependencies detected:\n  %s", strings.Join(lines, "\n  "))
}

// FileSample renders a file-evidence caption: the single file alone, or the
// first file annotated with how many more share the same edge. Empty input
// yields "" so each caller picks its own fallback ("generated files" in
// FormatMissing, "(declared)" in explain's edge captions).
func FileSample(files []string) string {
	switch len(files) {
	case 0:
		return ""
	case 1:
		return files[0]
	default:
		return fmt.Sprintf("%s (+%d more)", files[0], len(files)-1)
	}
}

func specYAMLPath(r TaskRef) string {
	dir := filepath.ToSlash(r.SpecRelpath)
	if dir == "" || dir == "." {
		return "sloff.yml"
	}
	return dir + "/sloff.yml"
}

func suggestedDependEntry(m MissingDependency) string {
	consumerDir := specDirForRel(m.Consumer.SpecRelpath)
	producerDir := specDirForRel(m.Producer.SpecRelpath)
	if consumerDir == producerDir {
		return fmt.Sprintf("`depends: [{task: %s}]`", m.Producer.Name)
	}
	rel, err := filepath.Rel(consumerDir, producerDir)
	if err != nil {
		rel = m.Producer.SpecRelpath
	}
	return fmt.Sprintf("`depends: [{spec: %s, task: %s}]`", filepath.ToSlash(rel), m.Producer.Name)
}

// specDirForRel maps both "repo root" spellings ("" and ".") to the "."
// form filepath.Rel accepts, so root-vs-subdir suggestions resolve cleanly.
func specDirForRel(s string) string {
	if s == "" {
		return "."
	}
	return s
}
