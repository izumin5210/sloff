// Package lister enumerates the source contributions that feed the go-local
// resolver's hash. It is a Resolver-internal helper, not a top-level extension
// point, and ships two implementations: globLister (precision-light fallback)
// and goPackagesLister (transitive analysis via golang.org/x/tools/go/packages).
//
// All implementations share the contract documented on SourceLister: in-process
// (no subprocess), OS-neutral output, sensitive to in-scope source changes, and
// safely composable with NewMemoized for per-run caching.
package lister

import (
	"context"
	"sync"
)

// SourceLister returns the file set and external module set that contribute to
// the logical version of an internal Go tool. The go-local resolver consumes
// the listing to compute its tools_hash component.
type SourceLister interface {
	// List returns the listing for the given entry. The entry is the main
	// package import path in `go run`-compatible form, e.g. "./cmd/foo" or
	// "./cmd/foo/...".
	List(ctx context.Context, entry string) (Listing, error)
}

// Listing is the union of source contributions enumerated for one entry.
// Implementations always return paths relative to the lister's repo root so
// the result is reproducible across machines.
type Listing struct {
	// InternalFiles are repo-relative file paths whose contents the resolver
	// must SHA256 (one file = one hash input).
	InternalFiles []string

	// ExternalModules are external Go modules whose label (Path@Version) and
	// go.sum line are hashed without reading individual files. This keeps
	// $GOMODCACHE re-hashing off the hot path while preserving cryptographic
	// strength via go.sum.
	ExternalModules []ExternalModule
}

// ExternalModule represents one external Go module reachable from the entry.
// The resolver hashes Path@Version + GoSumLine; module sources are not read.
type ExternalModule struct {
	// Path is the module import path, e.g. "google.golang.org/protobuf".
	Path string

	// Version is the module version (e.g. "v1.34.2"). For replace directives
	// pointing to a local directory, Version carries a "replace=<path>" label
	// so that switching to/from a replace directive still changes the hash.
	Version string

	// GoSumLine is the verbatim go.sum line(s) associated with Path@Version,
	// joined by "\n". Empty when go.sum does not record the module
	// (e.g. local replace), in which case Path@Version alone is the hash key.
	GoSumLine string
}

// Memoized wraps a SourceLister with a per-entry result cache valid for the
// lifetime of the wrapper. It exists so that a single lazygen run that resolves
// the same entry from many tasks pays the underlying List cost exactly once;
// the result is a pure function of the entry, so caching is safe.
type Memoized struct {
	inner SourceLister
	mu    sync.Mutex
	cache map[string]Listing
}

// NewMemoized returns a Memoized wrapping inner.
func NewMemoized(inner SourceLister) *Memoized {
	return &Memoized{inner: inner, cache: map[string]Listing{}}
}

// List delegates to the wrapped SourceLister, caching successful results by
// entry. Errors are not cached so transient failures can be retried.
func (m *Memoized) List(ctx context.Context, entry string) (Listing, error) {
	m.mu.Lock()
	if l, ok := m.cache[entry]; ok {
		m.mu.Unlock()
		return l, nil
	}
	m.mu.Unlock()

	l, err := m.inner.List(ctx, entry)
	if err != nil {
		return Listing{}, err
	}

	m.mu.Lock()
	m.cache[entry] = l
	m.mu.Unlock()
	return l, nil
}
