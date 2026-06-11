// Package explain projects depgraph tasks into their declared dependency
// edges (ADR-0013) plus the file-overlap evidence observable for each edge.
// The renderers in this package consume that projection to emit Mermaid /
// DOT for `sloff graph`; the same projection is the seed for the future
// `sloff run --explain`.
package explain

import (
	"fmt"
	"sort"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
)

// TaskRef identifies a depgraph task by its (spec_relpath, name) key. It is
// an alias of depgraph.TaskRef so graph projections and depgraph/runner
// error messages share one identity type and one Label rendering.
type TaskRef = depgraph.TaskRef

// Edge is a single declared dependency. Files is the observed O_From ∩ I_To
// — every repo-relative path that evidences the edge in the current tree —
// sorted ascending. Files may be empty on a clean checkout (the generated
// files don't exist yet); the edge still renders, captioned "(declared)".
type Edge struct {
	From  TaskRef
	To    TaskRef
	Files []string
}

// LabelSample renders the edge caption used by the graph subcommand:
// "(declared)" when no overlap evidence is observable in the current tree,
// the first justifying file alone when there is exactly one, otherwise that
// file annotated with "(+N more)". A wide monorepo can produce dozens of
// justifying files per edge; truncating to a sample matches the issue's
// "サンプル" wording (IZU-7) and keeps the rendered graph readable.
func (e Edge) LabelSample() string {
	switch len(e.Files) {
	case 0:
		return "(declared)"
	case 1:
		return e.Files[0]
	default:
		return fmt.Sprintf("%s (+%d more)", e.Files[0], len(e.Files)-1)
	}
}

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
		byRef[t.Ref()] = i
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
			j, ok := byRef[dep]
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
			out = append(out, Edge{From: dep, To: tasks[i].Ref(), Files: files})
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

func lessRef(a, b TaskRef) bool {
	if a.SpecRelpath != b.SpecRelpath {
		return a.SpecRelpath < b.SpecRelpath
	}
	return a.Name < b.Name
}

// orderedRefs returns task refs sorted by (SpecRelpath, Name) — the canonical
// node ordering used by both renderers so format swaps don't reorder graphs.
func orderedRefs(tasks []depgraph.Task) []TaskRef {
	refs := make([]TaskRef, len(tasks))
	for i, t := range tasks {
		refs[i] = t.Ref()
	}
	sort.Slice(refs, func(i, j int) bool { return lessRef(refs[i], refs[j]) })
	return refs
}
