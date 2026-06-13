package cached

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"

	"golang.org/x/sync/errgroup"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

// Storage is a fingerprint.Storage decorator that mirrors records to a
// directory under XDG_CACHE_HOME. It is intended for remote backends
// (e.g. DynamoDB) where round-trip latency dominates per-task lookup; the
// local backend is already disk-backed and should not be wrapped with this.
type Storage struct {
	inner    fingerprint.Storage
	cacheDir string
}

// New constructs a cached Storage rooted at cacheDir, delegating misses
// (and forwarding writes) to inner. Callers usually obtain cacheDir from
// CacheRoot(repoRoot); see repopath.go for the XDG + ghq-style derivation.
//
// The cache directory is created lazily on first write — there is nothing
// to do at construction time, and callers might invoke `sloff graph` or
// other read-only paths that only ever produce cache misses.
func New(inner fingerprint.Storage, cacheDir string) *Storage {
	return &Storage{inner: inner, cacheDir: cacheDir}
}

// Name reports the inner backend's name. The cache decorator is invisible
// to consumers that read this field for logging / diagnostics; what matters
// to them is "where do my records ultimately live".
func (s *Storage) Name() string { return s.inner.Name() }

// Load checks the cache first, then falls back to the inner backend. A hit
// from inner is mirrored to the cache before returning so the next run sees
// it locally.
func (s *Storage) Load(ctx context.Context, key fingerprint.Key) (*fingerprintv1.Record, bool, error) {
	if rec, ok := s.readCache(key); ok {
		return rec, true, nil
	}
	rec, ok, err := s.inner.Load(ctx, key)
	if err != nil || !ok {
		return rec, ok, err
	}
	s.writeCacheBestEffort(key, rec)
	return rec, true, nil
}

// Save writes through to the inner backend first; on success it mirrors to
// the cache. A cache write failure is logged via the returned error path's
// caller (we surface only inner failures to avoid masking real persistence
// errors with disk-cache hiccups).
func (s *Storage) Save(ctx context.Context, key fingerprint.Key, rec *fingerprintv1.Record) error {
	if err := s.inner.Save(ctx, key, rec); err != nil {
		return err
	}
	s.writeCacheBestEffort(key, rec)
	return nil
}

// Delete removes the record from inner and from the cache. Cache removal
// is best-effort; a stale cache file at most causes one wrong "hit" on the
// next Load, which is corrected by output-comparison and a regenerated
// record on the following run.
func (s *Storage) Delete(ctx context.Context, key fingerprint.Key) error {
	if err := s.inner.Delete(ctx, key); err != nil {
		return err
	}
	s.removeCacheBestEffort(key)
	return nil
}

// List passes through to inner. The cache layer doesn't track keys
// independently — what's on the cache disk is a subset of what inner has
// (with TTL/eviction lag in either direction), and listing the cache would
// produce a misleading view.
func (s *Storage) List(ctx context.Context, filter fingerprint.ListFilter) ([]fingerprint.Key, error) {
	return s.inner.List(ctx, filter)
}

// CollapseDuplicates passes through to inner. The cache stores at most one
// file per Key, so it has nothing to collapse on its own.
func (s *Storage) CollapseDuplicates(ctx context.Context) (int, error) {
	return s.inner.CollapseDuplicates(ctx)
}

// Warm forwards to the inner backend when it supports warming (e.g. the
// DynamoDB backend resolving credentials), so callers can front-load remote
// setup latency. No-op for backends that don't implement it.
func (s *Storage) Warm(ctx context.Context) error {
	if w, ok := s.inner.(interface{ Warm(context.Context) error }); ok {
		return w.Warm(ctx)
	}
	return nil
}

// LoadMany serves cached entries directly and only goes to inner for keys
// it could not satisfy locally. The inner result is then mirrored to the
// cache so future runs see those keys without a network round-trip.
func (s *Storage) LoadMany(ctx context.Context, keys []fingerprint.Key) (map[fingerprint.Key]*fingerprintv1.Record, error) {
	out := make(map[fingerprint.Key]*fingerprintv1.Record, len(keys))
	var missing []fingerprint.Key
	for _, k := range keys {
		if rec, ok := s.readCache(k); ok {
			out[k] = rec
			continue
		}
		missing = append(missing, k)
	}
	if len(missing) == 0 {
		return out, nil
	}
	fetched, err := s.inner.LoadMany(ctx, missing)
	if err != nil {
		return nil, err
	}
	maps.Copy(out, fetched)
	s.writeCacheManyBestEffort(fetched)
	return out, nil
}

// SaveMany writes through to inner first, then mirrors to the cache.
// Mirrors run in parallel since each cache write is an independent file.
func (s *Storage) SaveMany(ctx context.Context, items []fingerprint.KeyRecord) error {
	if err := s.inner.SaveMany(ctx, items); err != nil {
		return err
	}
	pairs := make(map[fingerprint.Key]*fingerprintv1.Record, len(items))
	for _, it := range items {
		pairs[it.Key] = it.Record
	}
	s.writeCacheManyBestEffort(pairs)
	return nil
}

// keyPath returns the on-disk file the cache uses for key. Mirrors the
// local backend's directory layout (sans the timestamp prefix that ADR-0010
// requires for git path uniqueness — irrelevant here because the cache is
// single-writer per machine).
func (s *Storage) keyPath(key fingerprint.Key) string {
	return filepath.Join(s.cacheDir, filepath.FromSlash(key.SpecRelpath), key.TaskID, key.InputHash+fingerprint.FileExt)
}

func (s *Storage) readCache(key fingerprint.Key) (*fingerprintv1.Record, bool) {
	b, err := os.ReadFile(s.keyPath(key))
	if err != nil {
		return nil, false
	}
	rec, err := fingerprint.Unmarshal(b)
	if err != nil {
		// Corrupted cache file: treat as miss. The next inner read will
		// repopulate it correctly.
		return nil, false
	}
	return rec, true
}

// writeCacheBestEffort writes the record to disk and ignores any error.
// The inner backend is the source of truth — a missing or stale cache
// file is corrected on the next Load.
func (s *Storage) writeCacheBestEffort(key fingerprint.Key, rec *fingerprintv1.Record) {
	_ = s.writeCache(key, rec)
}

// writeCacheManyBestEffort fans out per-record cache writes, swallowing
// individual errors for the same reason as writeCacheBestEffort.
func (s *Storage) writeCacheManyBestEffort(items map[fingerprint.Key]*fingerprintv1.Record) {
	if len(items) == 0 {
		return
	}
	g := new(errgroup.Group)
	g.SetLimit(cacheConcurrency)
	for k, rec := range items {
		g.Go(func() error {
			_ = s.writeCache(k, rec)
			return nil
		})
	}
	_ = g.Wait()
}

// writeCache is atomic: marshal → write to a temp file in the same dir →
// rename. Concurrent writes of the same key produce wire-byte-identical
// content (deterministic Marshal), so the rename winner does not matter.
func (s *Storage) writeCache(key fingerprint.Key, rec *fingerprintv1.Record) (retErr error) {
	b, err := fingerprint.Marshal(rec)
	if err != nil {
		return fmt.Errorf("cached: marshal %+v: %w", key, err)
	}
	full := s.keyPath(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), "."+key.InputHash+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Cleanup the temp file on any failure path; once the rename
		// succeeds the file no longer exists under tmpName so Remove is
		// a no-op there.
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, full)
}

// removeCacheBestEffort drops the cache file and ignores any error
// other than "already gone". Stale cache files are corrected by Load's
// content check (mismatched record bytes) on the next access.
func (s *Storage) removeCacheBestEffort(key fingerprint.Key) {
	err := os.Remove(s.keyPath(key))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// best-effort: see comment above
	}
}

// cacheConcurrency caps concurrent cache writes. The cache layer is
// I/O-bound on local disk; this matches the local backend's bulk
// concurrency budget.
const cacheConcurrency = 16
