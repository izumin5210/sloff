package preflight

import (
	"context"
	"fmt"
	"sort"
)

// Registry holds preflight Checkers keyed by Name.
type Registry struct {
	byName map[string]Checker
}

// NewRegistry returns an empty Registry. Checkers must be added via Register.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Checker{}}
}

// Register adds a Checker. Re-registering a name overwrites the previous entry.
func (r *Registry) Register(c Checker) {
	r.byName[c.Name()] = c
}

// Names returns every registered Checker's Name in registration-key (alphabetical) order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for k := range r.byName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Run executes the named Checkers (deduplicated) and aggregates their issues. The
// aggregated Result.OK is true only if every checker reported OK with no issues. A hard
// error from any Checker aborts the run and is wrapped with the checker's name.
func (r *Registry) Run(ctx context.Context, specDir string, names []string) (Result, error) {
	seen := make(map[string]struct{}, len(names))
	var aggregated Result
	aggregated.OK = true
	for _, name := range names {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}

		checker, ok := r.byName[name]
		if !ok {
			return Result{}, fmt.Errorf("unknown preflight checker %q", name)
		}
		res, err := checker.Check(ctx, specDir)
		if err != nil {
			return Result{}, fmt.Errorf("%s preflight: %w", name, err)
		}
		if !res.OK {
			aggregated.OK = false
		}
		aggregated.Issues = append(aggregated.Issues, res.Issues...)
	}
	return aggregated, nil
}
