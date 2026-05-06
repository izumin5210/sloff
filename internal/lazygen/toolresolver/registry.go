package toolresolver

import (
	"context"
	"fmt"
)

// Registry holds resolvers keyed by Name. Per ADR-0005 lazygen has no cmd-shape
// auto-dispatch: every Resolve call is driven by the spec's tools[] declarations,
// each of which names a Resolver explicitly. The registry is therefore a pure
// name → Resolver lookup.
type Registry struct {
	byName map[string]Resolver
}

// NewRegistry returns an empty Registry. Resolvers must be added via Register.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Resolver{}}
}

// Register adds resolver, overwriting any previous entry with the same Name.
// This is a developer-facing API meant to be called once at startup.
func (r *Registry) Register(resolver Resolver) {
	r.byName[resolver.Name()] = resolver
}

// Resolve returns the concatenation of every declared tool's ToolVersions in the
// order the spec wrote them. An empty declared slice is rejected because spec
// validation (ADR-0004 D1) already requires tools[]; reaching this code with no
// declarations indicates a programmer error elsewhere.
func (r *Registry) Resolve(ctx context.Context, specDir string, cmd []string, declared []DeclaredTool) ([]ToolVersion, error) {
	if len(declared) == 0 {
		return nil, fmt.Errorf("toolresolver: empty tools[] declaration (spec validation should have caught this)")
	}
	var versions []ToolVersion
	for i := range declared {
		d := &declared[i]
		res, ok := r.byName[d.Resolver]
		if !ok {
			return nil, fmt.Errorf("unknown resolver %q in tools declaration", d.Resolver)
		}
		v, err := res.Resolve(ctx, specDir, cmd, d)
		if err != nil {
			return nil, fmt.Errorf("resolver %s: %w", d.Resolver, err)
		}
		versions = append(versions, v...)
	}
	return versions, nil
}
