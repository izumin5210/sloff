// Package explain projects depgraph tasks into the auto-detected edges and
// the file-overlap evidence that justified each edge. The renderers in this
// package consume that projection to emit Mermaid / DOT for `sloff graph`;
// the same projection is the seed for the future `sloff run --explain`
// (architecture.md:598, 605, 645).
package explain

import (
	"fmt"
	"sort"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
)

// TaskRef identifies a depgraph task by its (spec_relpath, name) key. The same
// pair is what depgraph and the runner use to thread tasks through the
// orchestrator, so callers can compare refs directly against runner state.
type TaskRef struct {
	SpecRelpath string
	Name        string
}

// Label returns the human-readable task identifier used in graph captions and
// renderer node labels. SpecRelpath is dropped when empty (depgraph's own
// unit tests) or "." (a sloff.yml at the discovery root, which the cache
// layer also collapses to no prefix in `pathFor`); both forms describe the
// same logical "no qualifier needed" state.
func (r TaskRef) Label() string {
	if r.SpecRelpath == "" || r.SpecRelpath == "." {
		return r.Name
	}
	return r.SpecRelpath + ":" + r.Name
}

// Edge is a single auto-detected dependency. Files is O_From ∩ I_To — every
// repo-relative path that justified the edge — sorted ascending. Carrying the
// full set (rather than just a sample) keeps the explain projection lossless;
// renderers decide how much of it to display.
type Edge struct {
	From  TaskRef
	To    TaskRef
	Files []string
}

// LabelSample renders the edge caption used by the graph subcommand: the
// first justifying file alone when there is exactly one, otherwise that file
// annotated with "(+N more)". A wide monorepo can produce dozens of
// justifying files per edge; truncating to a sample matches the issue's
// "サンプル" wording (IZU-7) and keeps the rendered graph readable.
func (e Edge) LabelSample() string {
	switch len(e.Files) {
	case 0:
		return ""
	case 1:
		return e.Files[0]
	default:
		return fmt.Sprintf("%s (+%d more)", e.Files[0], len(e.Files)-1)
	}
}

// Edges derives every (producer → consumer) dependency from the file overlap
// between tasks' outputs and inputs. Edge ordering is deterministic — by To,
// then by From — and files within each edge are sorted ascending, so the
// output is suitable for byte-stable goldens.
//
// Tasks themselves are not mutated; this is the read-only "explain"
// projection of the same depgraph the runner builds.
func Edges(tasks []depgraph.Task) []Edge {
	if len(tasks) == 0 {
		return nil
	}

	// outputProducer is built without conflict detection on purpose: the
	// runner's depgraph.Build already rejects duplicate producers, so any
	// duplicate that reaches Edges signals a caller bypassing that check
	// and is recoverable in the renderer (the later entry wins).
	outputProducer := make(map[string]int, len(tasks))
	for i, t := range tasks {
		for _, out := range t.Outputs {
			outputProducer[out] = i
		}
	}

	type edgeKey struct{ from, to int }
	fileSets := make(map[edgeKey]map[string]struct{})
	for i, t := range tasks {
		for _, in := range t.Inputs {
			j, ok := outputProducer[in]
			if !ok || j == i {
				continue
			}
			key := edgeKey{from: j, to: i}
			set := fileSets[key]
			if set == nil {
				set = map[string]struct{}{}
				fileSets[key] = set
			}
			set[in] = struct{}{}
		}
	}

	out := make([]Edge, 0, len(fileSets))
	for key, set := range fileSets {
		files := make([]string, 0, len(set))
		for f := range set {
			files = append(files, f)
		}
		sort.Strings(files)
		out = append(out, Edge{
			From:  taskRefOf(tasks[key.from]),
			To:    taskRefOf(tasks[key.to]),
			Files: files,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].To != out[j].To {
			return lessRef(out[i].To, out[j].To)
		}
		return lessRef(out[i].From, out[j].From)
	})
	return out
}

func taskRefOf(t depgraph.Task) TaskRef {
	return TaskRef{SpecRelpath: t.SpecRelpath, Name: t.Name}
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
		refs[i] = taskRefOf(t)
	}
	sort.Slice(refs, func(i, j int) bool { return lessRef(refs[i], refs[j]) })
	return refs
}
