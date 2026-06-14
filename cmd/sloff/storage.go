package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/cached"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/dynamodb"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
)

// fileHashCachePath returns the host-local path where the runner persists its
// per-file content-digest cache (ADR-0014), co-located with the per-machine
// fingerprint cache root. Returns "" when the cache root can't be derived, in
// which case the runner keeps the digest cache in-memory only.
func fileHashCachePath(repoRoot string) string {
	dir, err := cached.CacheRoot(repoRoot)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "filehashes.v1.gob")
}

// loadStorage builds the fingerprint.Storage backend selected in
// <repoRoot>/.sloff/config.yml. The cmd layer is the single place that
// knows about every concrete backend, so the fingerprint package never
// imports the AWS SDK transitively. Remote backends are wrapped in a
// cached decorator under XDG_CACHE_HOME so per-task lookups can serve
// repeat hits from disk without a network round-trip.
func loadStorage(ctx context.Context, repoRoot string) (fingerprint.Storage, error) {
	return fingerprint.LoadStorage(ctx, repoRoot, defaultBuilders())
}

func defaultBuilders() map[fingerprint.BackendName]fingerprint.Builder {
	return map[fingerprint.BackendName]fingerprint.Builder{
		fingerprint.BackendLocal: func(_ context.Context, repoRoot string, _ fingerprint.Config) (fingerprint.Storage, error) {
			// Local backend is itself a disk store — wrapping it with the
			// cached decorator would only duplicate every record on the
			// same filesystem.
			return local.New(repoRoot), nil
		},
		fingerprint.BackendDynamoDB: func(ctx context.Context, repoRoot string, cfg fingerprint.Config) (fingerprint.Storage, error) {
			d := cfg.Fingerprint.DynamoDB
			if d == nil {
				return nil, fmt.Errorf("fingerprint backend %q requires a fingerprint.dynamodb section in .sloff/config.yml", fingerprint.BackendDynamoDB)
			}
			inner, err := dynamodb.New(ctx, dynamodb.Config{
				Table:            d.Table,
				Region:           d.Region,
				Endpoint:         d.Endpoint,
				ExpiresAfterDays: d.ExpiresAfterDays,
			})
			if err != nil {
				return nil, err
			}
			cacheDir, err := cached.CacheRoot(repoRoot)
			if err != nil {
				return nil, fmt.Errorf("derive cache dir: %w", err)
			}
			return cached.New(inner, cacheDir), nil
		},
	}
}
