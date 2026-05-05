package aqua

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yaml "github.com/goccy/go-yaml"
)

// ConfigFileName is the file name of an aqua configuration file located at the repo root.
const ConfigFileName = "aqua.yaml"

// Config is the in-memory representation of a parsed aqua.yaml.
type Config struct {
	Packages []Package
}

// Package is a single aqua package entry, normalised so that name and version are
// always separate even when the source YAML used the inline `name@version` form.
type Package struct {
	Name    string
	Version string
}

type rawConfig struct {
	Packages []rawPackage `yaml:"packages"`
}

type rawPackage struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// ParseConfig parses an aqua.yaml document. Both the inline (`name: foo/bar@v1`) and
// split (`name: foo/bar` + `version: v1`) forms are accepted.
func ParseConfig(b []byte) (*Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse aqua.yaml: %w", err)
	}
	cfg := &Config{Packages: make([]Package, 0, len(raw.Packages))}
	for i, rp := range raw.Packages {
		pkg, err := normalisePackage(rp)
		if err != nil {
			return nil, fmt.Errorf("packages[%d]: %w", i, err)
		}
		cfg.Packages = append(cfg.Packages, pkg)
	}
	return cfg, nil
}

func normalisePackage(rp rawPackage) (Package, error) {
	name := rp.Name
	version := rp.Version
	if i := strings.LastIndex(rp.Name, "@"); i > 0 {
		inlineName := rp.Name[:i]
		inlineVer := rp.Name[i+1:]
		if version != "" && version != inlineVer {
			return Package{}, fmt.Errorf("package %q has conflicting versions: inline %q vs field %q", inlineName, inlineVer, version)
		}
		name = inlineName
		version = inlineVer
	}
	if name == "" {
		return Package{}, errors.New("name is required")
	}
	if version == "" {
		return Package{}, fmt.Errorf("package %q has no version", name)
	}
	return Package{Name: name, Version: version}, nil
}

// LoadConfig reads aqua.yaml from repoRoot and parses it.
func LoadConfig(repoRoot string) (*Config, error) {
	b, err := os.ReadFile(filepath.Join(repoRoot, ConfigFileName))
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}
