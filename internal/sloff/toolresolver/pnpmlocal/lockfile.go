package pnpmlocal

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	yaml "github.com/goccy/go-yaml"
)

// LockfileName is the canonical name pnpm uses for its lockfile.
const LockfileName = "pnpm-lock.yaml"

// supportedLockfileVersion is the only pnpm-lock.yaml schema sloff
// understands. Older formats (v5/v6) lay out dependency data under different
// keys, so decoding them with the v9 view succeeds but silently yields empty
// importers/snapshots — external dep bumps would then stop invalidating
// fingerprints (R4). Note that goccy/go-yaml converts v5's unquoted float
// version (`lockfileVersion: 5.4`) to a string instead of erroring, so this
// explicit check is the only place such lockfiles get caught.
const supportedLockfileVersion = "9.0"

// Lockfile is a partial view over pnpm-lock.yaml v9. We parse:
//   - Importers: workspace path → its declared deps (used to resolve the
//     entry set of external dep walks per workspace package)
//   - Snapshots: <pkg@version> → its transitive deps (used to walk the
//     external dep graph rooted at a workspace package, Turborepo-style)
//
// Other top-level keys (settings, packages, time, etc.) are ignored.
type Lockfile struct {
	LockfileVersion string              `yaml:"lockfileVersion"`
	Importers       map[string]Importer `yaml:"importers"`
	Snapshots       map[string]Snapshot `yaml:"snapshots"`
}

// Importer mirrors pnpm-lock.yaml's importers[<path>] entry. We collect the
// three dependency buckets so the externals walk doesn't accidentally miss
// devDependencies (codegen tools commonly live there) or optionalDependencies
// (platform-specific peers).
type Importer struct {
	Dependencies         map[string]ImporterDep `yaml:"dependencies"`
	DevDependencies      map[string]ImporterDep `yaml:"devDependencies"`
	OptionalDependencies map[string]ImporterDep `yaml:"optionalDependencies"`
}

// ImporterDep is one dep entry under importers[<path>].dependencies. Specifier
// is what the user wrote in package.json (e.g. "^4.17.0"); Version is what
// pnpm resolved it to in the lockfile (e.g. "4.17.21" or "4.17.21(peer@1)" or
// "link:../util" for workspace links).
type ImporterDep struct {
	Specifier string `yaml:"specifier"`
	Version   string `yaml:"version"`
}

// Snapshot is one entry under snapshots[<pkg@version>] — the resolved
// dependency tree of that package at that exact context. Values in
// Dependencies are version strings (with optional peer-context suffixes)
// that compose with the dep name into another snapshot key.
type Snapshot struct {
	Dependencies         map[string]string `yaml:"dependencies"`
	OptionalDependencies map[string]string `yaml:"optionalDependencies"`
}

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
	if err := yaml.Unmarshal(lastYAMLDocument(b), &lf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", sourcePath, err)
	}
	if lf.LockfileVersion != supportedLockfileVersion {
		got := fmt.Sprintf("%q", lf.LockfileVersion)
		if lf.LockfileVersion == "" {
			got = "missing"
		}
		return nil, fmt.Errorf("parse %s: unsupported lockfileVersion %s: sloff supports pnpm lockfileVersion %q only (pnpm v9+); regenerate the lockfile with a supported pnpm version", sourcePath, got, supportedLockfileVersion)
	}
	return &lf, nil
}

// lastYAMLDocument returns the bytes of the final document in a YAML stream.
// pnpm 12 writes pnpm-lock.yaml as a multi-document stream: a leading
// document pinning pnpm itself (configDependencies /
// packageManagerDependencies) followed by the actual lockfile document. The
// leading document carries the same lockfileVersion as the real one, so the
// version guard cannot tell them apart — position (last) is the contract.
// Single-document files from earlier pnpm versions are returned unchanged.
//
// Splitting is line-based on "---" end-of-directives markers at column zero.
// That is exact for pnpm-generated lockfiles (top-level block mappings, no
// block scalars that could embed a column-zero "---"), and both sides of the
// install-drift comparison go through this same function, so a hypothetical
// mis-split cannot introduce asymmetry there.
func lastYAMLDocument(b []byte) []byte {
	marker := []byte("---")
	start := 0
	for i := 0; i < len(b); {
		next := len(b)
		line := b[i:]
		if nl := bytes.IndexByte(line, '\n'); nl >= 0 {
			line = line[:nl]
			next = i + nl + 1
		}
		if bytes.Equal(bytes.TrimRight(line, " \t\r"), marker) {
			start = next
		}
		i = next
	}
	return b[start:]
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
