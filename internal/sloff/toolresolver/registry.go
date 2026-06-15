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

// Prewarm gives every registered resolver that implements Prewarmer a chance
// to batch expensive discovery work across all the declared tools referencing
// it, before the runner fans out per-tool Inputs/Versions calls. Requests are
// grouped by resolver channel; resolvers that don't implement Prewarmer are
// skipped. A prewarm is a pure cache-warming optimisation (see Prewarmer), so
// the caller may treat a returned error as non-fatal and fall back to the
// per-tool path.
func (r *Registry) Prewarm(ctx context.Context, reqs []PrewarmRequest) error {
	byResolver := map[string][]PrewarmRequest{}
	for _, req := range reqs {
		if req.Declared == nil {
			continue
		}
		byResolver[req.Declared.Resolver] = append(byResolver[req.Declared.Resolver], req)
	}
	for name, group := range byResolver {
		res, ok := r.byName[name]
		if !ok {
			continue
		}
		pw, ok := res.(Prewarmer)
		if !ok {
			continue
		}
		if err := pw.Prewarm(ctx, group); err != nil {
			return fmt.Errorf("prewarm %s: %w", name, err)
		}
	}
	return nil
}

// PrewarmChannels returns the set of resolver channel names whose resolver
// implements Prewarmer. The runner uses it to split referenced tools into a
// "gated" group (resolved after Prewarm so they hit the warmed cache) and an
// "eager" group (resolved concurrently with Prewarm), letting the batch
// discovery overlap the eager channels' work instead of preceding it.
func (r *Registry) PrewarmChannels() map[string]struct{} {
	out := map[string]struct{}{}
	for name, res := range r.byName {
		if _, ok := res.(Prewarmer); ok {
			out[name] = struct{}{}
		}
	}
	return out
}
