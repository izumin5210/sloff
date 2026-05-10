// Package depgraph derives a task DAG from each task's inputs/outputs and emits a
// stable topological order. sloff never accepts a manual `depends:` declaration; the
// graph is recovered entirely from the file-set intersections.
package depgraph

import (
	"fmt"
	"sort"
	"strings"
)

// Task is one DAG node. SpecRelpath/Name together form the unique key.
type Task struct {
	SpecRelpath string
	Name        string
	Inputs      []string // expanded paths, repo-root relative
	Outputs     []string
}

// Build returns the tasks in execution order: A precedes B whenever some output of A
// also appears in B's inputs. Ties are broken deterministically by (SpecRelpath, Name).
// A cycle yields an error.
func Build(tasks []Task) ([]Task, error) {
	if len(tasks) == 0 {
		return nil, nil
	}

	type idx = int
	keyToIdx := make(map[string]idx, len(tasks))
	for i, t := range tasks {
		keyToIdx[taskKey(t)] = i
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

func taskLabel(t Task) string {
	if t.SpecRelpath == "" {
		return t.Name
	}
	return t.SpecRelpath + ":" + t.Name
}

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
		if t.SpecRelpath == "" {
			rest = append(rest, t.Name)
		} else {
			rest = append(rest, t.SpecRelpath+":"+t.Name)
		}
	}
	sort.Strings(rest)
	return strings.Join(rest, ", ")
}
