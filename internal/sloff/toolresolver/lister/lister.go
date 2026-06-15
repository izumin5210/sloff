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
// the listing to compute its resolved_versions_hash component.
type SourceLister interface {
	// List returns the listing for the given entry, evaluated with the given
	// spec directory as the working module context. specDir is repo-relative
	// using OS-native separators (e.g. "" or "submodule"). entry is the main
	// package import path in `go run`-compatible spec-relative form
	// (e.g. "./cmd/foo" or "./cmd/foo/..."). Implementations must run
	// packages.Load (or equivalent) at <repoRoot>/<specDir> so that nested
	// monorepo specs whose go.mod sits beside sloff.yml resolve correctly.
	List(ctx context.Context, specDir, entry string) (Listing, error)
}

// BatchSourceLister is an optional SourceLister extension that resolves many
// entries sharing one spec dir in a single underlying load. goPackagesLister
// implements it by passing every entry to one packages.Load, so a monorepo
// whose tools all live in the same module builds that module's shared
// dependency graph once instead of once per entry. The returned map is keyed
// by entry; entries the batch could not resolve 1:1 (e.g. a "./..." wildcard
// or a missing package) are simply absent so the caller can fall back to List.
//
// ListBatch must return, for each resolved entry, a Listing identical to what
// List would return for that (specDir, entry) — batching is a performance
// optimisation only.
type BatchSourceLister interface {
	SourceLister
	ListBatch(ctx context.Context, specDir string, entries []string) (map[string]Listing, error)
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

// Memoized wraps a SourceLister with a per-(specDir, entry) result cache valid
// for the lifetime of the wrapper. It exists so that a single sloff run that
// resolves the same generator from many tasks pays the underlying List cost
// exactly once; the result is a pure function of (specDir, entry), so caching
// is safe.
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
// (specDir, entry). Errors are not cached so transient failures can be retried.
func (m *Memoized) List(ctx context.Context, specDir, entry string) (Listing, error) {
	key := specDir + "\x00" + entry

	m.mu.Lock()
	if l, ok := m.cache[key]; ok {
		m.mu.Unlock()
		return l, nil
	}
	m.mu.Unlock()

	l, err := m.inner.List(ctx, specDir, entry)
	if err != nil {
		return Listing{}, err
	}

	m.mu.Lock()
	m.cache[key] = l
	m.mu.Unlock()
	return l, nil
}

// ListBatch resolves entries under specDir, serving already-cached entries
// from the per-(specDir, entry) cache and computing the rest in a single batch
// when the inner lister is a BatchSourceLister. Every result is cached, so a
// later List for the same (specDir, entry) is a hit — this is the method the
// go-local resolver's prewarm phase drives to collapse N packages.Load calls
// into one. Entries the batch can't resolve 1:1 fall back to sequential List,
// and when the inner lister can't batch at all every entry takes that path.
func (m *Memoized) ListBatch(ctx context.Context, specDir string, entries []string) (map[string]Listing, error) {
	out := make(map[string]Listing, len(entries))
	var missing []string
	for _, e := range entries {
		key := specDir + "\x00" + e
		m.mu.Lock()
		l, ok := m.cache[key]
		m.mu.Unlock()
		if ok {
			out[e] = l
			continue
		}
		missing = append(missing, e)
	}
	if len(missing) == 0 {
		return out, nil
	}

	if bl, ok := m.inner.(BatchSourceLister); ok {
		batched, err := bl.ListBatch(ctx, specDir, missing)
		if err != nil {
			return nil, err
		}
		remaining := missing[:0:0]
		for _, e := range missing {
			l, ok := batched[e]
			if !ok {
				// Entry the batch declined (wildcard / unmapped): resolve it
				// the slow way below.
				remaining = append(remaining, e)
				continue
			}
			key := specDir + "\x00" + e
			m.mu.Lock()
			m.cache[key] = l
			m.mu.Unlock()
			out[e] = l
		}
		missing = remaining
	}

	for _, e := range missing {
		l, err := m.List(ctx, specDir, e) // List populates m.cache
		if err != nil {
			return nil, err
		}
		out[e] = l
	}
	return out, nil
}
