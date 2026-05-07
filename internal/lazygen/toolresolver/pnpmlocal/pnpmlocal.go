// Package pnpmlocal implements toolresolver.Resolver for pnpm workspace-local
// tools. It applies to internal js/ts generators distributed as workspace
// packages (referenced via "workspace:*" in pnpm-lock.yaml). External npm
// packages are intentionally NOT a separate channel: per ADR-0007 lazygen
// absorbs them into the script resolver, except for the surgical version-graph
// hashing this resolver performs against pnpm-lock.yaml — emitted as
// "pnpm-deps:<pkg>@<version>" ToolVersions — so that runtime-resolved
// external deps still flip the cache when the workspace tool's transitive
// npm graph shifts (mirrors Turborepo's per-package hashing).
//
// Per ADR-0005 the resolver is declared-only: it runs when the spec wrote
// `tools: [{pnpm-local: <package-name>}]` and the package name resolves to a
// workspace member.
//
// Hashing strategy:
//   - The workspace package's bin / main entry feeds an injectable
//     lister.SourceLister; the resulting workspace file paths are returned
//     as ExtraInputs so the runner folds them into the task's input glob.
//     This integrates with depgraph: a separately-declared build task whose
//     outputs cover those files becomes an upstream dependency automatically.
//   - The package's transitive external deps (walked from pnpm-lock.yaml's
//     snapshots graph) are returned as ToolVersion entries so external
//     bumps flip tools_hash without re-running the lister.
package pnpmlocal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

// Name is the resolver identifier referenced by spec tools[] entries.
const Name = "pnpm-local"

// DepsPrefix is the version-string prefix for transitive external npm
// dependency entries. Mirrors go-deps used by the go-local resolver so both
// channels expose a uniform "<channel>-deps:<pkg>@<version>" shape on the
// Result.Versions side regardless of language.
const DepsPrefix = "pnpm-deps:"

// Resolver resolves pnpm workspace-local tool dependencies into a Result that
// combines (a) workspace source paths (via the lister) as ExtraInputs and
// (b) transitive external dep versions (via lockfile graph walk) as
// ToolVersions.
type Resolver struct {
	repoRoot string
	lister   lister.SourceLister

	once         sync.Once
	workspace    *Workspace
	workspaceErr error
}

// New constructs a Resolver. The pnpm-lock.yaml is loaded lazily on the first
// Resolve call so lazygen runs without a pnpm workspace (Go-only repos)
// don't pay the cost or fail at startup.
func New(repoRoot string, l lister.SourceLister) (*Resolver, error) {
	if l == nil {
		return nil, errors.New("pnpm-local: lister is required")
	}
	return &Resolver{repoRoot: repoRoot, lister: l}, nil
}

// Name implements toolresolver.Resolver.
func (r *Resolver) Name() string { return Name }

// Resolve returns the workspace+externals contribution. declared.PackageName
// must name a workspace member registered in pnpm-lock.yaml's importers;
// non-workspace names are rejected with ErrNotWorkspacePackage so the user
// switches to the script resolver per ADR-0007.
func (r *Resolver) Resolve(ctx context.Context, _ string, _ []string, declared *toolresolver.DeclaredTool) (toolresolver.Result, error) {
	if declared == nil {
		return toolresolver.Result{}, errors.New("pnpm-local: declared tool is required (auto-dispatch was removed in ADR-0005)")
	}
	if declared.PackageName == "" {
		return toolresolver.Result{}, errors.New("pnpm-local: declared package name is required")
	}

	ws, err := r.loadWorkspace()
	if err != nil {
		return toolresolver.Result{}, fmt.Errorf("pnpm-local: load workspace: %w", err)
	}
	pkg, ok := ws.Lookup(declared.PackageName)
	if !ok {
		return toolresolver.Result{}, fmt.Errorf("%w: %q", ErrNotWorkspacePackage, declared.PackageName)
	}
	if len(pkg.EntryPoints) == 0 {
		return toolresolver.Result{}, fmt.Errorf("pnpm-local: %s has no bin/main entry in package.json", pkg.Name)
	}

	extraInputs, err := r.collectInputs(ctx, pkg)
	if err != nil {
		return toolresolver.Result{}, fmt.Errorf("pnpm-local: list sources for %q: %w", pkg.Name, err)
	}

	versions, err := r.collectExternalVersions(ws, pkg)
	if err != nil {
		return toolresolver.Result{}, fmt.Errorf("pnpm-local: collect externals for %q: %w", pkg.Name, err)
	}

	return toolresolver.Result{Versions: versions, ExtraInputs: extraInputs}, nil
}

// loadWorkspace caches the (Workspace, error) pair so a single lazygen run
// loads the lockfile / package.json files exactly once even when many tasks
// reference different workspace packages.
func (r *Resolver) loadWorkspace() (*Workspace, error) {
	r.once.Do(func() {
		r.workspace, r.workspaceErr = LoadWorkspace(r.repoRoot)
	})
	return r.workspace, r.workspaceErr
}

// collectInputs asks the lister for each entry's transitive set and returns
// the sorted union as repo-relative slash-form paths.
//
// The bin path itself is always included even when the lister can't read it
// (typical fresh-checkout case where dist/ is gitignored and an upstream
// build task hasn't run yet). Without this fall-back depgraph would have
// nothing to overlap against the build task's `dist/**` outputs and the
// dependency edge would be missed; with it, the build runs first and the
// transitive set becomes accurate from the next run onward.
func (r *Resolver) collectInputs(ctx context.Context, pkg WorkspacePackage) ([]string, error) {
	seen := map[string]struct{}{}
	for _, ep := range pkg.EntryPoints {
		rel := path.Join(filepath.ToSlash(pkg.Dir), filepath.ToSlash(ep))
		seen[rel] = struct{}{}

		abs := filepath.Join(r.repoRoot, filepath.FromSlash(rel))
		if _, err := os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
			continue
		}

		listing, err := r.lister.List(ctx, "", "./"+rel)
		if err != nil {
			return nil, err
		}
		for _, f := range listing.InternalFiles {
			seen[f] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

// collectExternalVersions returns one ToolVersion per transitively-reachable
// external npm package. Each version is the surgical lockfile slice for this
// workspace package (Turborepo-style), so unrelated lockfile churn does not
// invalidate the cache.
func (r *Resolver) collectExternalVersions(ws *Workspace, pkg WorkspacePackage) ([]toolresolver.ToolVersion, error) {
	externals, err := CollectExternals(ws.lockfile, filepath.ToSlash(pkg.Dir))
	if err != nil {
		return nil, err
	}
	out := make([]toolresolver.ToolVersion, 0, len(externals))
	for _, e := range externals {
		out = append(out, toolresolver.ToolVersion{
			Name:    e,
			Source:  Name + ":" + pkg.Name,
			Version: DepsPrefix + e,
		})
	}
	return out, nil
}
