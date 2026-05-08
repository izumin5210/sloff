// Package pnpmlocal implements toolresolver.Resolver for pnpm workspace-local
// tools. A "tool" here is a workspace package (referenced via "workspace:*"
// in pnpm-lock.yaml); the resolver computes its cache contribution along the
// same axes the spec already has:
//
//   - ExtraInputs (files_hash channel): every file inside the package's dir
//     that git tracks, plus every file inside transitively-linked workspace
//     dirs. .gitignore'd paths (typically dist/, build/, etc.) are excluded.
//     Source edits flip files_hash so consumer tasks rerun the cmd, which
//     itself is responsible for any rebuild step (ADR-0008 D7, mirroring the
//     `go run` model used by go-local).
//   - Versions (tools_hash channel): every transitively-reachable external
//     npm package, surgically walked from pnpm-lock.yaml's importers and
//     snapshots. Each entry is encoded as `pnpm-deps:<pkg>@<version>` so peer
//     suffixes round-trip and tools_hash flips on registry-resolved bumps
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

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
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
// pnpm-deps ToolVersions. The resolver itself does NOT orchestrate the
// workspace package's build — that's the consuming task's cmd's job.
type Resolver struct {
	repoRoot     string
	enumerator   FileEnumerator
	driftChecker DriftChecker

	once         sync.Once
	workspace    *Workspace
	workspaceErr error
}

// DriftChecker verifies that node_modules is in sync with pnpm-lock.yaml.
// AssertInstallInSync is the production implementation; tests inject a fake
// to keep the resolver decoupled from a real pnpm install.
type DriftChecker func(repoRoot string) error

// New constructs a Resolver. The pnpm-lock.yaml is loaded lazily on the first
// Resolve call so lazygen runs without a pnpm workspace (Go-only repos)
// don't pay the cost or fail at startup. Pass GitLsFiles for the production
// enumerator; tests inject a fake to avoid a real git working tree.
//
// The drift checker defaults to AssertInstallInSync, which compares
// pnpm-lock.yaml with node_modules/.pnpm/lock.yaml byte-by-byte to catch
// "lockfile updated, pnpm install forgotten" silent drift before the
// resolver hands a stale-install cache key downstream. Tests inject a no-op
// or fake checker to avoid materialising a real pnpm install in fixtures.
func New(repoRoot string, enumerator FileEnumerator, driftChecker DriftChecker) (*Resolver, error) {
	if enumerator == nil {
		return nil, errors.New("pnpm-local: file enumerator is required")
	}
	if driftChecker == nil {
		return nil, errors.New("pnpm-local: drift checker is required")
	}
	return &Resolver{repoRoot: repoRoot, enumerator: enumerator, driftChecker: driftChecker}, nil
}

// Name implements toolresolver.Resolver.
func (r *Resolver) Name() string { return Name }

// Resolve walks the lockfile from declared.PackageName's importer entry,
// gathering the workspace dirs (via link: edges) and the transitive external
// npm dep set (via snapshots). For each workspace dir, the FileEnumerator
// produces the list of git-tracked / non-ignored files; the union becomes
// ExtraInputs. Externals become individual `pnpm-deps:<pkg>@<ver>`
// ToolVersions.
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
	// Confirm node_modules tracks the current pnpm-lock.yaml before we trust
	// the lockfile-derived versions. Without this, a stale install would let
	// the resolver hand back fresh-lockfile versions while the cmd actually
	// runs against an older dep graph — the silent-stale failure mode that
	// motivated this check.
	if err := r.driftChecker(r.repoRoot); err != nil {
		return toolresolver.Result{}, err
	}
	pkg, ok := ws.Lookup(declared.PackageName)
	if !ok {
		return toolresolver.Result{}, fmt.Errorf("%w: %q", ErrNotWorkspacePackage, declared.PackageName)
	}

	walk, err := WalkDeps(ws.lockfile, filepath.ToSlash(pkg.Dir))
	if err != nil {
		return toolresolver.Result{}, fmt.Errorf("pnpm-local: walk deps for %q: %w", pkg.Name, err)
	}

	extraInputs, err := r.collectFiles(ctx, walk.Workspaces)
	if err != nil {
		return toolresolver.Result{}, fmt.Errorf("pnpm-local: enumerate files for %q: %w", pkg.Name, err)
	}

	versions := make([]toolresolver.ToolVersion, 0, len(walk.Externals))
	source := Name + ":" + pkg.Name
	for _, e := range walk.Externals {
		versions = append(versions, toolresolver.ToolVersion{
			Name:    e,
			Source:  source,
			Version: DepsPrefix + e,
		})
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
