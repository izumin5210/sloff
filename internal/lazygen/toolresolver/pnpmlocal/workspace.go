package pnpmlocal

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
)

// WorkspacePackage is a single pnpm workspace member resolved from
// pnpm-lock.yaml + its package.json.
type WorkspacePackage struct {
	// Name is the package name from package.json (e.g. "@org/codegen"). Empty
	// names are filtered out before the package becomes lookup-visible.
	Name string

	// Dir is the OS-native repo-relative path of the package directory.
	Dir string

	// EntryPoints is the union of bin entries (preferred) or main when bin is
	// empty, package-relative and sorted ascending. The lister consumes these
	// as esbuild EntryPoints.
	EntryPoints []string

	// Bin mirrors package.json's bin field after normalisation. The preflight
	// checker uses it to decide whether a package needs build (bin under dist/)
	// vs. ts-node/tsx-style direct source execution.
	Bin []string

	// Main mirrors package.json's main field verbatim.
	Main string
}

// Workspace bundles the pnpm-lock.yaml and per-package.json data needed to
// resolve a pnpm-local declaration to a workspace member. The lockfile is
// kept alive on the struct because the externals walk needs the snapshots
// graph at version-resolution time.
type Workspace struct {
	repoRoot string
	lockfile *Lockfile
	byName   map[string]WorkspacePackage
}

// LoadWorkspace reads <repoRoot>/pnpm-lock.yaml and the package.json of every
// importer, indexing packages by their declared name. Importers whose
// package.json omits "name" (typical for the monorepo root) are skipped.
//
// Importers whose package.json is missing on disk are also skipped: pnpm-lock
// commonly carries stale entries for renamed/removed workspace members until
// the user reruns `pnpm install`, and a root importer entry is sometimes
// listed without a corresponding manifest. Aborting the whole index on those
// would block every pnpm-local resolution for benign drift; the dependency
// walker (WalkDeps) already tolerates missing importer entries downstream,
// so we match that posture here. Parse errors and other IO failures still
// abort: those signal corrupt manifests, not just lockfile drift.
func LoadWorkspace(repoRoot string) (*Workspace, error) {
	lf, err := LoadLockfile(repoRoot)
	if err != nil {
		return nil, err
	}
	ws := &Workspace{
		repoRoot: repoRoot,
		lockfile: lf,
		byName:   make(map[string]WorkspacePackage, len(lf.Importers)),
	}
	for _, importer := range lf.WorkspacePaths() {
		pj, err := LoadPackageJSON(repoRoot, importer)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if pj.Name == "" {
			continue
		}
		if _, dup := ws.byName[pj.Name]; dup {
			return nil, fmt.Errorf("pnpm-local: duplicate workspace package name %q", pj.Name)
		}
		ws.byName[pj.Name] = WorkspacePackage{
			Name:        pj.Name,
			Dir:         filepath.FromSlash(importer),
			EntryPoints: append([]string(nil), pj.EntryPoints...),
			Bin:         append([]string(nil), pj.Bin...),
			Main:        pj.Main,
		}
	}
	return ws, nil
}

// Lookup returns the workspace package registered with name, or false if no
// importer's package.json declares that name.
func (w *Workspace) Lookup(name string) (WorkspacePackage, bool) {
	if name == "" {
		return WorkspacePackage{}, false
	}
	p, ok := w.byName[name]
	return p, ok
}

// All returns every workspace package sorted ascending by Name. Used by the
// preflight checker to iterate without re-reading the lockfile.
func (w *Workspace) All() []WorkspacePackage {
	out := make([]WorkspacePackage, 0, len(w.byName))
	for _, p := range w.byName {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ErrNotWorkspacePackage is the sentinel the resolver returns when a declared
// pnpm-local name does not match any workspace member. Callers compare via
// errors.Is.
var ErrNotWorkspacePackage = errors.New("pnpm-local: package is not a pnpm workspace member")
