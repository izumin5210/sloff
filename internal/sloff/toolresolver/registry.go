package toolresolver

import (
	"context"
	"fmt"
)

// Registry holds resolvers keyed by Name. Per ADR-0005 sloff has no cmd-shape
// auto-dispatch: every Inputs / Versions call is driven by the spec's tools[]
// declarations, each of which names a Resolver explicitly. The registry is
// therefore a pure name → Resolver lookup.
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

// Inputs concatenates every declared tool's ExtraInputs contribution in the
// order they appear in declared. An empty declared slice is rejected because
// spec validation (ADR-0004 D1) already requires tools[]; reaching this code
// with no declarations indicates a programmer error elsewhere.
func (r *Registry) Inputs(ctx context.Context, specDir string, declared []DeclaredTool) ([]string, error) {
	if len(declared) == 0 {
		return nil, fmt.Errorf("toolresolver: empty tools[] declaration (spec validation should have caught this)")
	}
	var out []string
	for i := range declared {
		d := &declared[i]
		res, ok := r.byName[d.Resolver]
		if !ok {
			return nil, fmt.Errorf("unknown resolver %q in tools declaration", d.Resolver)
		}
		ins, err := res.Inputs(ctx, specDir, d)
		if err != nil {
			return nil, fmt.Errorf("resolver %s: %w", d.Resolver, err)
		}
		out = append(out, ins...)
	}
	return out, nil
}

// Versions concatenates every declared tool's ResolvedVersion contribution in the
// order they appear in declared. Same empty-slice contract as Inputs.
func (r *Registry) Versions(ctx context.Context, specDir string, declared []DeclaredTool) ([]ResolvedVersion, error) {
	if len(declared) == 0 {
		return nil, fmt.Errorf("toolresolver: empty tools[] declaration (spec validation should have caught this)")
	}
	var out []ResolvedVersion
	for i := range declared {
		d := &declared[i]
		res, ok := r.byName[d.Resolver]
		if !ok {
			return nil, fmt.Errorf("unknown resolver %q in tools declaration", d.Resolver)
		}
		vs, err := res.Versions(ctx, specDir, d)
		if err != nil {
			return nil, fmt.Errorf("resolver %s: %w", d.Resolver, err)
		}
		out = append(out, vs...)
	}
	return out, nil
}
