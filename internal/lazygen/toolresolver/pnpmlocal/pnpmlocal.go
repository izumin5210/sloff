// Package pnpmlocal implements toolresolver.Resolver for pnpm workspace-local
// tools. It applies to internal js/ts generators distributed as workspace
// packages (referenced via "workspace:*" in pnpm-lock.yaml). External npm
// packages are intentionally out of scope: per ADR-0007 lazygen absorbs them
// into the script resolver.
//
// Per ADR-0005 the resolver is declared-only: it runs when the spec wrote
// `tools: [{pnpm-local: <package-name>}]` and the package name resolves to a
// workspace member.
//
// Hashing strategy follows resolver-pnpm-local.md:
//   - the workspace package's package.json bin (or main, when bin is absent)
//     determines the entry point set
//   - each entry is forwarded to an injectable lister.SourceLister; the
//     standard implementation is lister.NewEsbuild, which traverses transitive
//     imports in-process via esbuild's Go API
//   - the union of returned files is hashed in path-ascending order
package pnpmlocal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// Resolver resolves the logical version of a pnpm workspace-local tool to
// "pnpm-local:<package-name>@sha256:<hex>".
type Resolver struct {
	repoRoot string
	lister   lister.SourceLister

	once         sync.Once
	workspace    *Workspace
	workspaceErr error
}

// New constructs a Resolver. The pnpm-lock.yaml is loaded lazily on the first
// Resolve call so that lazygen runs without a pnpm workspace (Go-only repos)
// don't pay the cost or fail at startup.
func New(repoRoot string, l lister.SourceLister) (*Resolver, error) {
	if l == nil {
		return nil, errors.New("pnpm-local: lister is required")
	}
	return &Resolver{repoRoot: repoRoot, lister: l}, nil
}

// Name implements toolresolver.Resolver.
func (r *Resolver) Name() string { return Name }

// Resolve returns one ToolVersion. declared.PackageName names a workspace
// member registered in pnpm-lock.yaml's importers; non-workspace names are
// rejected with ErrNotWorkspacePackage so the user can switch to the script
// resolver per ADR-0007.
func (r *Resolver) Resolve(ctx context.Context, _ string, _ []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
	if declared == nil {
		return nil, errors.New("pnpm-local: declared tool is required (auto-dispatch was removed in ADR-0005)")
	}
	if declared.PackageName == "" {
		return nil, errors.New("pnpm-local: declared package name is required")
	}

	ws, err := r.loadWorkspace()
	if err != nil {
		return nil, fmt.Errorf("pnpm-local: load workspace: %w", err)
	}
	pkg, ok := ws.Lookup(declared.PackageName)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotWorkspacePackage, declared.PackageName)
	}
	if len(pkg.EntryPoints) == 0 {
		return nil, fmt.Errorf("pnpm-local: %s has no bin/main entry in package.json", pkg.Name)
	}

	files, err := r.collectFiles(ctx, pkg)
	if err != nil {
		return nil, fmt.Errorf("pnpm-local: list sources for %q: %w", pkg.Name, err)
	}
	digest, err := hashFiles(r.repoRoot, files)
	if err != nil {
		return nil, fmt.Errorf("pnpm-local: hash sources for %q: %w", pkg.Name, err)
	}

	source := Name + ":" + pkg.Name
	return []toolresolver.ToolVersion{{
		Name:    pkg.Name,
		Source:  source,
		Version: source + "@sha256:" + hex.EncodeToString(digest),
	}}, nil
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

// collectFiles asks the lister for each entry's transitive set and returns the
// sorted union. specDir is empty because the entry path is composed
// repo-relative (workspace package directory + package-relative entry).
func (r *Resolver) collectFiles(ctx context.Context, pkg WorkspacePackage) ([]string, error) {
	seen := map[string]struct{}{}
	for _, ep := range pkg.EntryPoints {
		entry := "./" + path.Join(filepath.ToSlash(pkg.Dir), filepath.ToSlash(ep))
		listing, err := r.lister.List(ctx, "", entry)
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

// hashFiles folds the file set into a deterministic SHA256: each file's
// repo-relative slash-form path and its content digest are written in path
// order, NUL-separated. Path is included alongside content so renames change
// the digest even when content is identical.
func hashFiles(repoRoot string, files []string) ([]byte, error) {
	h := sha256.New()
	for _, f := range files {
		digest, err := fileSHA256(filepath.Join(repoRoot, filepath.FromSlash(f)))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		h.Write([]byte(f))
		h.Write([]byte{0})
		h.Write(digest)
		h.Write([]byte{0})
	}
	return h.Sum(nil), nil
}

func fileSHA256(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
