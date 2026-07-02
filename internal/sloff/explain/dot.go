package explain

import (
	"fmt"
	"strings"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
)

// RenderDOT emits a Graphviz DOT representation matching the same node and
// edge ordering as RenderMermaid, so swapping --format does not reorder the
// graph. DOT permits arbitrary characters inside double-quoted IDs (apart
// from an unescaped double quote), so we use the human-readable task label
// as the ID directly instead of a slugged form. Group tasks carry
// shape=hexagon — the same visual distinction the Mermaid renderer draws
// (ADR-0016 D6).
func RenderDOT(tasks []depgraph.Task, edges []Edge) string {
	var b strings.Builder
	b.WriteString("digraph sloff {\n")
	b.WriteString("    rankdir=TB;\n")
	b.WriteString("    node [shape=box];\n")

	refs := orderedRefs(tasks)
	groups := groupRefs(tasks)
	for _, r := range refs {
		if groups[r] {
			fmt.Fprintf(&b, "    %s [shape=hexagon];\n", quoteDOT(r.Label()))
		} else {
			fmt.Fprintf(&b, "    %s;\n", quoteDOT(r.Label()))
		}
	}
	for _, e := range edges {
		fmt.Fprintf(&b, "    %s -> %s [label=%s];\n",
			quoteDOT(e.From.Label()),
			quoteDOT(e.To.Label()),
			quoteDOT(e.LabelSample()))
	}
	b.WriteString("}\n")
	return b.String()
}

// quoteDOT wraps s in DOT double quotes, escaping internal backslashes and
// double quotes so the output stays well-formed for unusual file paths.
func quoteDOT(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
