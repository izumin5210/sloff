// Package depgraph builds the task DAG and emits a stable topological order.
// Execution-order edges come from the spec-declared depends entries carried
// on Task.DependsOn (ADR-0013 D2); inputs/outputs file overlap is used only
// for validation (duplicate-producer detection and FindMissingDependencies).
package depgraph

import (
	"fmt"
	"path/filepath"
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
		return nil, fmt.Errorf("cycle detected involving: %s", remainingTaskKeys(tasks, out))
	}
	return out, nil
}

func taskKey(t Task) string {
	return t.SpecRelpath + "\x00" + t.Name
}

func sortByKey(indices []int, tasks []Task) {
	sort.SliceStable(indices, func(i, j int) bool {
		return taskKey(tasks[indices[i]]) < taskKey(tasks[indices[j]])
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

func remainingTaskKeys(all, emitted []Task) string {
	emittedSet := make(map[string]struct{}, len(emitted))
	for _, t := range emitted {
		emittedSet[taskKey(t)] = struct{}{}
	}
	var rest []string
	for _, t := range all {
		if _, ok := emittedSet[taskKey(t)]; ok {
			continue
		}
		rest = append(rest, t.Ref().Label())
	}
	sort.Strings(rest)
	return strings.Join(rest, ", ")
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
	consumerDir := normalizeSpecDir(m.Consumer.SpecRelpath)
	producerDir := normalizeSpecDir(m.Producer.SpecRelpath)
	if consumerDir == producerDir {
		return fmt.Sprintf("`depends: [{task: %s}]`", m.Producer.Name)
	}
	rel, err := filepath.Rel(orDot(consumerDir), orDot(producerDir))
	if err != nil {
		rel = m.Producer.SpecRelpath
	}
	return fmt.Sprintf("`depends: [{spec: %s, task: %s}]`", filepath.ToSlash(rel), m.Producer.Name)
}

// normalizeSpecDir maps the two "repo root" spellings to one form.
func normalizeSpecDir(s string) string {
	if s == "." {
		return ""
	}
	return s
}

func orDot(s string) string {
	if s == "" {
		return "."
	}
	return s
}
