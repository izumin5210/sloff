package toolresolver

import (
	"context"
	"fmt"
)

// Registry holds resolvers keyed by Name. Per ADR-0005 sloff has no cmd-shape
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

// Resolve concatenates every declared tool's contribution in the order the spec
// wrote them. Versions feed tools_hash; ExtraInputs are merged into the task's
// input set by the runner before depgraph computes ordering. An empty declared
// slice is rejected because spec validation (ADR-0004 D1) already requires
// tools[]; reaching this code with no declarations indicates a programmer error
// elsewhere.
func (r *Registry) Resolve(ctx context.Context, specDir string, cmd []string, declared []DeclaredTool) (Result, error) {
	if len(declared) == 0 {
		return Result{}, fmt.Errorf("toolresolver: empty tools[] declaration (spec validation should have caught this)")
	}
	var combined Result
	for i := range declared {
		d := &declared[i]
		res, ok := r.byName[d.Resolver]
		if !ok {
			return Result{}, fmt.Errorf("unknown resolver %q in tools declaration", d.Resolver)
		}
		one, err := res.Resolve(ctx, specDir, cmd, d)
		if err != nil {
			return Result{}, fmt.Errorf("resolver %s: %w", d.Resolver, err)
		}
		combined.Versions = append(combined.Versions, one.Versions...)
		combined.ExtraInputs = append(combined.ExtraInputs, one.ExtraInputs...)
	}
	return combined, nil
}
