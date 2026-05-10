package fingerprint

import (
	"context"
	"fmt"
)

// Builder is the function signature LoadStorage uses to construct each known
// backend. Splitting the wiring out keeps this package free of any direct
// import on backend implementations (callers register them), so the
// fingerprint package can be imported by tests / library users without
// pulling in the AWS SDK transitively.
type Builder func(ctx context.Context, repoRoot string, cfg Config) (Storage, error)

// LoadStorage reads <repoRoot>/.sloff/config.yml, picks the backend it asks
// for, and invokes the matching builder. Callers wire concrete builders for
// their build (e.g. cmd/sloff registers both local and s3) and pass the map
// in here.
//
// A missing config file is not an error; ResolvedBackend defaults to local,
// so repos that never opt in to S3 keep working with no config file at all.
func LoadStorage(ctx context.Context, repoRoot string, builders map[BackendName]Builder) (Storage, error) {
	cfg, err := LoadConfig(repoRoot)
	if err != nil {
		return nil, err
	}
	backend := cfg.ResolvedBackend()
	build, ok := builders[backend]
	if !ok {
		return nil, fmt.Errorf("fingerprint: backend %q is not available in this build", backend)
	}
	return build(ctx, repoRoot, cfg)
}
