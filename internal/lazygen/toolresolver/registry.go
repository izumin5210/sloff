package toolresolver

import (
	"context"
	"fmt"
)

// Registry holds resolvers and dispatches Resolve calls to the right one based on the
// presence of explicit `tools:` declarations in a spec, falling back to CanResolve in
// registration order.
type Registry struct {
	byName  map[string]Resolver
	inOrder []Resolver
}

// NewRegistry returns an empty Registry. Resolvers must be added via Register.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Resolver{}}
}

// Register adds resolver. Registration order determines auto-dispatch priority.
// Re-registering a name overwrites the previous entry but does not duplicate it in the
// dispatch order; this is a developer-facing API meant to be called once at startup.
func (r *Registry) Register(resolver Resolver) {
	if _, exists := r.byName[resolver.Name()]; !exists {
		r.inOrder = append(r.inOrder, resolver)
	}
	r.byName[resolver.Name()] = resolver
}

// Resolve returns the union of ToolVersions for cmd. If declared is non-empty each entry's
// resolver is looked up by name and called with the full DeclaredTool; otherwise the
// registered resolvers are tried in order and the first whose CanResolve returns true is
// used. If neither path produces a match an error is returned: an unresolved tool would
// hash to an empty version set and let stale cache hits survive a generator upgrade.
func (r *Registry) Resolve(ctx context.Context, specDir string, cmd []string, declared []DeclaredTool) ([]ToolVersion, error) {
	if len(declared) > 0 {
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
	for _, res := range r.inOrder {
		if res.CanResolve(specDir, cmd) {
			v, err := res.Resolve(ctx, specDir, cmd, nil)
			if err != nil {
				return nil, fmt.Errorf("resolver %s: %w", res.Name(), err)
			}
			return v, nil
		}
	}
	return nil, fmt.Errorf("no resolver matched cmd %v", cmd)
}
