package pnpmlocal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PackageJSON is the subset of <pkg>/package.json the resolver actually needs.
// Currently that's just the package name: under ADR-0008 D7 the resolver
// hashes a workspace's source files via git ls-files (not via bin/main entry
// resolution), so the bin and main fields are intentionally not parsed.
type PackageJSON struct {
	Name string
}

// LoadPackageJSON reads <repoRoot>/<pkgDir>/package.json and parses it.
func LoadPackageJSON(repoRoot, pkgDir string) (*PackageJSON, error) {
	p := filepath.Join(repoRoot, pkgDir, "package.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	pj, err := ParsePackageJSON(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return pj, nil
}

// ParsePackageJSON parses a package.json byte payload.
func ParsePackageJSON(b []byte) (*PackageJSON, error) {
	var raw struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("invalid package.json: %w", err)
	}
	return &PackageJSON{Name: raw.Name}, nil
}
