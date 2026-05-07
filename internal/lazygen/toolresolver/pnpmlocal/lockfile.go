package pnpmlocal

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	yaml "github.com/goccy/go-yaml"
)

// LockfileName is the canonical name pnpm uses for its lockfile.
const LockfileName = "pnpm-lock.yaml"

// Lockfile is a minimal view over pnpm-lock.yaml. We only need the importer
// keys ( = workspace package paths) to map declared package names back to
// directories on disk. Dependency graph details are intentionally ignored:
// pnpm-local hashes source files directly, so the lockfile only acts as the
// authoritative list of workspace members.
type Lockfile struct {
	Importers map[string]importerStub `yaml:"importers"`
}

// importerStub is intentionally empty — yaml decoding still walks past unknown
// keys, and we don't consume any of importers[*]'s fields.
type importerStub struct{}

// LoadLockfile reads <repoRoot>/pnpm-lock.yaml.
func LoadLockfile(repoRoot string) (*Lockfile, error) {
	p := filepath.Join(repoRoot, LockfileName)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	return parseLockfile(b, p)
}

func parseLockfile(b []byte, sourcePath string) (*Lockfile, error) {
	var lf Lockfile
	if err := yaml.Unmarshal(b, &lf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", sourcePath, err)
	}
	return &lf, nil
}

// WorkspacePaths returns importer paths sorted ascending. The root importer
// (key ".") is included as-is; the caller decides whether to treat the root as
// a workspace package by reading its package.json.
func (l *Lockfile) WorkspacePaths() []string {
	paths := make([]string, 0, len(l.Importers))
	for k := range l.Importers {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	return paths
}
