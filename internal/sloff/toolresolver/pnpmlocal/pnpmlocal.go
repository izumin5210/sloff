// Package pnpmlocal implements toolresolver.Resolver for pnpm workspace-local
// tools. A "tool" here is a workspace package (referenced via "workspace:*"
// in pnpm-lock.yaml); the resolver computes its fingerprint contribution along the
// same axes the spec already has:
//
//   - ExtraInputs (files_hash channel): every file inside the package's dir
//     that git tracks, plus every file inside transitively-linked workspace
//     dirs. .gitignore'd paths (typically dist/, build/, etc.) are excluded.
//     Source edits flip files_hash so consumer tasks rerun the cmd, which
//     itself is responsible for any rebuild step (ADR-0008 D7, mirroring the
//     `go run` model used by go-local).
//   - Versions (resolved_versions_hash channel): every transitively-reachable external
//     npm package, surgically walked from pnpm-lock.yaml's importers and
//     snapshots. Each entry is encoded as `pnpm-deps:<pkg>@<version>` so peer
//     suffixes round-trip and resolved_versions_hash flips on registry-resolved bumps
//     (Turborepo-equivalent precision).
//
// External npm packages used as tools — i.e. anything not in the workspace —
// are out of scope per ADR-0007 and go through the script resolver instead.
//
// Per ADR-0005 the resolver is declared-only: it runs when a task's
// `tools: [name]` list resolves to a tool whose definition is
// `pnpm-local: <package-name>` and the package name is registered as a
// workspace member.
package pnpmlocal

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
)

// Name is the resolver identifier referenced by spec tools[] entries.
const Name = "pnpm-local"

// DepsPrefix is the version-string prefix for transitive external npm
// dependency entries. Mirrors go-deps used by the go-local resolver so both
// channels expose a uniform "<channel>-deps:<pkg>@<version>" shape on the
// Result.Versions side regardless of language.
const DepsPrefix = "pnpm-deps:"

// Resolver resolves a pnpm workspace-local tool's contributions: source files
// (via FileEnumerator) as ExtraInputs and transitive npm deps as
// pnpm-deps ResolvedVersions. The resolver itself does NOT orchestrate the
// workspace package's build — that's the consuming task's cmd's job. It also
// does NOT validate that node_modules matches pnpm-lock.yaml; that lives in
// the preflight subsystem (preflight/pnpmlocal/) so the runner can apply the
// shared SLOFF_ALLOW_STALE_DEPS read-only escape hatch uniformly.
type Resolver struct {
	repoRoot   string
	enumerator FileEnumerator

	once         sync.Once
	workspace    *Workspace
	workspaceErr error

	// pkgs caches the (extra inputs, versions) pair per declared package
	// name so Inputs and Versions don't both walk the lockfile and call the
	// file enumerator. The cached entry is computed lazily on first call;
	// subsequent calls (Inputs after Versions, or vice versa) read from
	// memory. Errors are cached too so a failing package doesn't keep
	// retrying within a single run.
	pkgsMu sync.Mutex
	pkgs   map[string]*pkgComputation
}

// pkgComputation is the memoised result of WalkDeps + collectFiles for one
// declared package name. Both Inputs and Versions read from the same value,
// so the heavy work happens at most once per (resolver, package) per run.
type pkgComputation struct {
	once     sync.Once
	inputs   []string
	versions []toolresolver.ResolvedVersion
	err      error
}

// New constructs a Resolver. The pnpm-lock.yaml is loaded lazily on the first
// Resolve call so sloff runs without a pnpm workspace (Go-only repos)
// don't pay the cost or fail at startup. Pass GitLsFiles for the production
// enumerator; tests inject a fake to avoid a real git working tree.
//
// Install-state drift detection (pnpm-lock.yaml vs node_modules/.pnpm/lock.yaml)
// lives in preflight/pnpmlocal/, not here, so it inherits the shared
// preflight scoping and SLOFF_ALLOW_STALE_DEPS escape-hatch behaviour.
func New(repoRoot string, enumerator FileEnumerator) (*Resolver, error) {
	if enumerator == nil {
		return nil, errors.New("pnpm-local: file enumerator is required")
	}
	return &Resolver{repoRoot: repoRoot, enumerator: enumerator}, nil
}

// Name implements toolresolver.Resolver.
func (r *Resolver) Name() string { return Name }

// Inputs returns the union of git-tracked / non-ignored files inside the
// declared workspace package and every transitively-linked workspace dir,
// repo-relative slash form. The runner folds these into the consuming
// task's input set so source edits flip files_hash and depgraph wires up
// upstream codegen via output overlap.
//
// Inputs and Versions share a per-package cache (pkgComputation) so the
// lockfile walk and FileEnumerator invocation happen at most once per
// declared package name per run.
func (r *Resolver) Inputs(ctx context.Context, _ string, declared *toolresolver.DeclaredTool) ([]string, error) {
	pc, err := r.computeFor(ctx, declared)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), pc.inputs...), nil
}

// Versions returns one ResolvedVersion per transitively-reachable external npm
// package, encoded as `pnpm-deps:<pkg>@<version>` so peer suffixes round-trip
// and resolved_versions_hash flips on registry-resolved bumps.
func (r *Resolver) Versions(ctx context.Context, _ string, declared *toolresolver.DeclaredTool) ([]toolresolver.ResolvedVersion, error) {
	pc, err := r.computeFor(ctx, declared)
	if err != nil {
		return nil, err
	}
	return append([]toolresolver.ResolvedVersion(nil), pc.versions...), nil
}

// computeFor returns the per-package cached computation (workspace files +
// external deps) for declared.PackageName. The first caller pays the
// lockfile walk and file enumeration; subsequent callers (Inputs after
// Versions, or vice versa) read from memory.
func (r *Resolver) computeFor(ctx context.Context, declared *toolresolver.DeclaredTool) (*pkgComputation, error) {
	if declared == nil {
		return nil, errors.New("pnpm-local: declared tool is required (auto-dispatch was removed in ADR-0005)")
	}
	if declared.PackageName == "" {
		return nil, errors.New("pnpm-local: declared package name is required")
	}
	pc := r.pkgComputationFor(declared.PackageName)
	pc.once.Do(func() {
		pc.inputs, pc.versions, pc.err = r.compute(ctx, declared.PackageName)
	})
	if pc.err != nil {
		return nil, pc.err
	}
	return pc, nil
}

func (r *Resolver) pkgComputationFor(packageName string) *pkgComputation {
	r.pkgsMu.Lock()
	defer r.pkgsMu.Unlock()
	if r.pkgs == nil {
		r.pkgs = map[string]*pkgComputation{}
	}
	if existing, ok := r.pkgs[packageName]; ok {
		return existing
	}
	pc := &pkgComputation{}
	r.pkgs[packageName] = pc
	return pc
}

// compute walks the lockfile from packageName's importer entry, gathering
// the workspace dirs (via link: edges) and transitive external npm deps
// (via snapshots), then runs the FileEnumerator for the union of workspace
// dirs. The result feeds both Inputs (workspace files) and Versions (npm
// dep entries).
func (r *Resolver) compute(ctx context.Context, packageName string) ([]string, []toolresolver.ResolvedVersion, error) {
	ws, err := r.loadWorkspace()
	if err != nil {
		return nil, nil, fmt.Errorf("pnpm-local: load workspace: %w", err)
	}
	pkg, ok := ws.Lookup(packageName)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %q", ErrNotWorkspacePackage, packageName)
	}

	walk, err := WalkDeps(ws.lockfile, filepath.ToSlash(pkg.Dir))
	if err != nil {
		return nil, nil, fmt.Errorf("pnpm-local: walk deps for %q: %w", pkg.Name, err)
	}

	extraInputs, err := r.collectFiles(ctx, walk.Workspaces)
	if err != nil {
		return nil, nil, fmt.Errorf("pnpm-local: enumerate files for %q: %w", pkg.Name, err)
	}

	versions := make([]toolresolver.ResolvedVersion, 0, len(walk.Externals))
	source := Name + ":" + pkg.Name
	for _, e := range walk.Externals {
		versions = append(versions, toolresolver.ResolvedVersion{
			Name:    e,
			Source:  source,
			Version: DepsPrefix + e,
		})
	}
	return extraInputs, versions, nil
}

// loadWorkspace caches the (Workspace, error) pair so a single sloff run
// loads the lockfile / package.json files exactly once even when many tasks
// reference different workspace packages.
func (r *Resolver) loadWorkspace() (*Workspace, error) {
	r.once.Do(func() {
		r.workspace, r.workspaceErr = LoadWorkspace(r.repoRoot)
	})
	return r.workspace, r.workspaceErr
}

// collectFiles enumerates every file in each workspace dir via the injected
// enumerator and returns the deduplicated, sorted union as repo-relative
// slash-form paths suitable for the runner's input merge.
func (r *Resolver) collectFiles(ctx context.Context, workspaceDirs []string) ([]string, error) {
	seen := map[string]struct{}{}
	for _, slashDir := range workspaceDirs {
		osDir := filepath.FromSlash(slashDir)
		files, err := r.enumerator(ctx, r.repoRoot, osDir)
		if err != nil {
			return nil, fmt.Errorf("dir %q: %w", slashDir, err)
		}
		for _, f := range files {
			seen[filepath.ToSlash(f)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}
