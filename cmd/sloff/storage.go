package main

import (
	"context"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/s3"
)

// loadStorage builds the fingerprint.Storage backend selected in
// <repoRoot>/.sloff/config.yml. The cmd layer is the single place that knows
// about every concrete backend, so the fingerprint package never imports the
// AWS SDK transitively.
func loadStorage(ctx context.Context, repoRoot string) (fingerprint.Storage, error) {
	return fingerprint.LoadStorage(ctx, repoRoot, defaultBuilders())
}

func defaultBuilders() map[fingerprint.BackendName]fingerprint.Builder {
	return map[fingerprint.BackendName]fingerprint.Builder{
		fingerprint.BackendLocal: func(_ context.Context, repoRoot string, _ fingerprint.Config) (fingerprint.Storage, error) {
			return local.New(repoRoot), nil
		},
		fingerprint.BackendS3: func(ctx context.Context, _ string, cfg fingerprint.Config) (fingerprint.Storage, error) {
			s3cfg := fingerprint.S3Config{}
			if cfg.Fingerprint.S3 != nil {
				s3cfg = *cfg.Fingerprint.S3
			}
			return s3.New(ctx, s3cfg)
		},
	}
}
