package explain

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
)

// nonSlugChar matches everything Mermaid identifiers don't accept; replacing
// each match with "_" keeps the resulting id legal across the dialect.
var nonSlugChar = regexp.MustCompile(`[^A-Za-z0-9_]`)

// nodeIDs assigns each ref a Mermaid-safe identifier derived from its label.
// Distinct refs that slug to the same base id receive a "_2", "_3", ...
// suffix in iteration order; passing in already-sorted refs keeps the
// suffix assignment deterministic across runs.
func nodeIDs(refs []TaskRef) map[TaskRef]string {
	used := make(map[string]struct{}, len(refs))
	out := make(map[TaskRef]string, len(refs))
	for _, r := range refs {
		base := "n_" + nonSlugChar.ReplaceAllString(r.Label(), "_")
		id := base
		for n := 2; ; n++ {
			if _, taken := used[id]; !taken {
				break
			}
			id = fmt.Sprintf("%s_%d", base, n)
		}
		used[id] = struct{}{}
		out[r] = id
	}
	return out
}

// RenderMermaid emits a Mermaid `flowchart TD` representation of tasks and
// their declared dependency edges. Output is byte-stable: nodes appear in
// (spec_relpath, name) order; edges are sorted by (To, From); each edge label
// carries a sample of the justifying files (architecture.md:598).
func RenderMermaid(tasks []depgraph.Task, edges []Edge) string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")

	refs := orderedRefs(tasks)
	ids := nodeIDs(refs)

	for _, r := range refs {
		fmt.Fprintf(&b, "    %s[\"%s\"]\n", ids[r], escapeMermaidLabel(r.Label()))
	}
	for _, e := range edges {
		fmt.Fprintf(&b, "    %s -->|\"%s\"| %s\n", ids[e.From], escapeMermaidLabel(e.LabelSample()), ids[e.To])
	}
	return b.String()
}

// escapeMermaidLabel preserves user-visible content inside a `"..."` Mermaid
// label by backslash-escaping internal double quotes. File paths shouldn't
// contain quotes in practice; this keeps the rendering well-formed if they
// ever do without invoking heavier sanitization.
func escapeMermaidLabel(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}
