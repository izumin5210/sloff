package fingerprint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	yaml "github.com/goccy/go-yaml"
)

// ConfigFileRelpath is the repo-relative path of the sloff configuration
// file. It sits next to the fingerprint store under .sloff/ so a repo's
// sloff surface stays contained in a single directory; spec files
// (sloff.yml under spec dirs) are unrelated.
const ConfigFileRelpath = ".sloff/config.yml"

// BackendName identifies a Storage implementation.
type BackendName string

const (
	BackendLocal    BackendName = "local"
	BackendDynamoDB BackendName = "dynamodb"
)

// Config is the parsed shape of <repoRoot>/.sloff/config.yml. Missing files
// produce a zero Config (Fingerprint.Backend == ""), which the runtime
// treats as an implicit `local` so the file is genuinely optional for
// repos that never opt in to a remote backend.
type Config struct {
	Fingerprint FingerprintConfig `yaml:"fingerprint"`
}

// FingerprintConfig groups every fingerprint-storage-related setting under
// the top-level `fingerprint:` key so future per-backend sections nest
// naturally.
type FingerprintConfig struct {
	Backend  BackendName     `yaml:"backend,omitempty"`
	DynamoDB *DynamoDBConfig `yaml:"dynamodb,omitempty"`
}

// DynamoDBConfig carries the DynamoDB-backend connection knobs that are
// intentionally committed to the repo (table / region / endpoint /
// expires_after_days). Credentials are deliberately absent: they come
// from the AWS default credential chain so .sloff/config.yml stays safe
// to commit.
type DynamoDBConfig struct {
	Table            string `yaml:"table"`
	Region           string `yaml:"region,omitempty"`
	Endpoint         string `yaml:"endpoint,omitempty"`
	ExpiresAfterDays int    `yaml:"expires_after_days,omitempty"`
}

// LoadConfig reads <repoRoot>/.sloff/config.yml. A missing file is not an
// error — it yields a zero Config equivalent to `backend: local`. Any
// other I/O or parse failure is surfaced as-is so the caller can fail the
// run rather than silently fall back.
func LoadConfig(repoRoot string) (Config, error) {
	path := filepath.Join(repoRoot, filepath.FromSlash(ConfigFileRelpath))
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// ResolvedBackend returns the backend the config asks for, defaulting to
// BackendLocal when the field is empty.
func (c Config) ResolvedBackend() BackendName {
	if c.Fingerprint.Backend == "" {
		return BackendLocal
	}
	return c.Fingerprint.Backend
}
