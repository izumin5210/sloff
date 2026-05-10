package fingerprint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	yaml "github.com/goccy/go-yaml"
)

// ConfigFileRelpath is the repo-relative path of the sloff configuration file.
// It sits next to the fingerprint store under .sloff/ so a repo's sloff
// surface stays contained in a single directory; spec files (sloff.yml under
// spec dirs) are unrelated.
const ConfigFileRelpath = ".sloff/config.yml"

// DefaultS3Prefix is the object-key prefix used when fingerprint.s3.prefix is
// unset. Choosing a non-empty default lets a single bucket be shared between
// distinct sloff usages or between sloff and other tools without callers
// having to think about namespacing.
const DefaultS3Prefix = "sloff/fingerprints"

// BackendName identifies a Storage implementation in the config file.
type BackendName string

const (
	BackendLocal BackendName = "local"
	BackendS3    BackendName = "s3"
)

// Config is the parsed shape of <repoRoot>/.sloff/config.yml. Missing files
// produce a zero Config (Fingerprint.Backend == ""), which LoadConfig and the
// runtime treat as an implicit "local" so the file is genuinely optional for
// repos that never opt in to S3.
type Config struct {
	Fingerprint FingerprintConfig `yaml:"fingerprint"`
}

// FingerprintConfig groups every fingerprint-storage-related setting under the
// top-level `fingerprint:` key so future per-backend sections (hybrid, memory)
// nest naturally.
type FingerprintConfig struct {
	Backend BackendName `yaml:"backend,omitempty"`
	S3      *S3Config   `yaml:"s3,omitempty"`
}

// S3Config carries the S3-backend connection knobs that are intentionally
// committed to the repo (bucket / prefix / region / endpoint / addressing
// style). Credentials are deliberately absent: they come from the AWS default
// credential chain so .sloff/config.yml stays safe to commit.
type S3Config struct {
	Bucket       string `yaml:"bucket"`
	Prefix       string `yaml:"prefix,omitempty"`
	Region       string `yaml:"region,omitempty"`
	Endpoint     string `yaml:"endpoint,omitempty"`
	UsePathStyle *bool  `yaml:"use_path_style,omitempty"`
}

// LoadConfig reads <repoRoot>/.sloff/config.yml. A missing file is not an
// error — it yields a zero Config equivalent to `backend: local`. Any other
// I/O or parse failure is surfaced as-is so the caller can fail the run
// rather than silently fall back.
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
