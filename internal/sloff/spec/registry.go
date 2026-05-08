package spec

import (
	"fmt"
	"sort"
)

// ToolRegistry is the repo-wide name → tool definition index built by merging
// every discovered sloff.yml's tools[] block. Per ADR-0008 the namespace is
// flat (no scoping by spec dir), so tool names must be unique across all
// files; collisions are reported at registry-build time with both definition
// sites named so users can rename or consolidate.
//
// Each ToolEntry remembers the spec dir that defined it. That dir — not the
// referencing task's dir — is what resolvers use as the path-resolution base
// (ADR-0008 D3), keeping tool definitions self-contained even when
// referenced from unrelated parts of the repo.
type ToolRegistry struct {
	byName map[string]ToolEntry
}

// ToolEntry describes a single registered tool: its global name, the spec
// directory where it was defined (OS-native repo-relative), and the
// DeclaredTool body that carries resolver-specific fields.
type ToolEntry struct {
	Name     string
	SpecDir  string
	Declared DeclaredTool
}

// BuildToolRegistry merges every spec's tools[] map into a single namespace
// and validates uniqueness. The error message points at both definition sites
// so a user faced with a collision can rename one without re-running.
func BuildToolRegistry(specs []Spec) (*ToolRegistry, error) {
	reg := &ToolRegistry{byName: map[string]ToolEntry{}}
	for _, sp := range specs {
		for name, tool := range sp.File.Tools {
			if existing, dup := reg.byName[name]; dup {
				existingPath := registryDefinitionPath(existing.SpecDir)
				newPath := registryDefinitionPath(sp.Dir)
				return nil, fmt.Errorf("tool %q defined twice: in %s and %s", name, existingPath, newPath)
			}
			reg.byName[name] = ToolEntry{Name: name, SpecDir: sp.Dir, Declared: tool}
		}
	}
	return reg, nil
}

// Lookup returns the ToolEntry registered under name, or false if no
// discovered sloff.yml declared it. Callers (validators, runner) use this
// to fail fast on undefined references.
func (r *ToolRegistry) Lookup(name string) (ToolEntry, bool) {
	e, ok := r.byName[name]
	return e, ok
}

// All returns every registered ToolEntry sorted ascending by Name. Used by
// the runner's tool pre-resolve pass so resolver invocations are
// deterministic regardless of map iteration order.
func (r *ToolRegistry) All() []ToolEntry {
	out := make([]ToolEntry, 0, len(r.byName))
	for _, e := range r.byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ValidateToolReferences walks every command's Tools list and ensures each
// referenced name has a registered ToolEntry. Errors carry the originating
// task path so users can locate the offending reference quickly.
func ValidateToolReferences(specs []Spec, reg *ToolRegistry) error {
	for _, sp := range specs {
		for _, c := range sp.File.Commands {
			for _, name := range c.Tools {
				if _, ok := reg.Lookup(name); !ok {
					return fmt.Errorf("%s/%s: references undefined tool %q (no sloff.yml declares it under tools)",
						registryDefinitionPath(sp.Dir), c.Name, name)
				}
			}
		}
	}
	return nil
}

// registryDefinitionPath formats a spec dir for inclusion in error messages.
// The repo root prints as "<root>/sloff.yml" rather than an empty path so
// users see a concrete location instead of guessing where "" is.
func registryDefinitionPath(specDir string) string {
	if specDir == "" || specDir == "." {
		return "sloff.yml"
	}
	return specDir + "/sloff.yml"
}
