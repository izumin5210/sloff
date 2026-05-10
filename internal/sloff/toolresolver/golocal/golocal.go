// Package golocal implements toolresolver.Resolver for repo-local Go tools.
//
// It applies to tools that are built from sources living inside the repository
// (typical examples: bespoke protoc plugins, code generators wired up via
// `go run ./cmd/...`). These tools have no SemVer to read, so the fingerprint key
// is split across two of sloff's hash buckets:
//
//   - Internal source files (main module / repo-local sources) become
//     ExtraInputs and feed files_hash via the runner's input merge. This is
//     what lets depgraph wire upstream codegen tasks (whose outputs land
//     inside the same source tree the tool reads) to this task automatically
//     by the existing output-overlap rule, with no extra dependency channel.
//   - External Go modules become individual ResolvedVersion entries
//     ("go-deps:<path>@<version>+sum:<go.sum-line>") and feed resolved_versions_hash, so
//     dep bumps invalidate without re-reading the lister-traversed source set.
//
// Per ADR-0005 the resolver is declared-only: invoked when the spec wrote
// `tools: [{go-local: ./cmd/foo}]` for the task, regardless of whether the cmd
// is `go run ./cmd/foo` or a prebuilt binary produced from those sources.
//
// Replace directives:
//   - Local replace (`replace foo => ../foo` without version): lister treats
//     replaced sources as internal — they show up in ExtraInputs.
//   - Versioned replace (`replace foo => bar v1.0.0`): the resolver emits a
//     go-deps ResolvedVersion encoding both the original path and the
//     replacement target, so swapping replacement targets flips resolved_versions_hash.
package golocal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
)

// Name is the resolver identifier referenced by spec tools[] entries.
const Name = "go-local"

// DepsPrefix is the version-string prefix for external Go module entries.
// Mirrors the pnpm-deps prefix used by pnpm-local; both are surfaced via the
// same Result.Versions channel so consumers see a uniform "<channel>-deps"
// shape regardless of language.
const DepsPrefix = "go-deps:"

// Resolver resolves a Go-local tool's source contributions (as ExtraInputs)
// and external module set (as go-deps ResolvedVersions).
type Resolver struct {
	repoRoot string
	lister   lister.SourceLister
}

// New returns a Resolver rooted at repoRoot that delegates source enumeration
// to l. Pass lister.NewMemoized(...) when many tasks share the same entry.
func New(repoRoot string, l lister.SourceLister) *Resolver {
	return &Resolver{repoRoot: repoRoot, lister: l}
}

// Name implements toolresolver.Resolver.
func (r *Resolver) Name() string { return Name }

// Inputs returns every internal Go file path the lister enumerated for this
// declared tool (repo-relative slash form). The runner folds these into the
// task's input set so files_hash captures their content and depgraph can
// wire upstream producers via output overlap.
//
// Inputs and Versions both consult the (memoised) lister — paying for a
// single packages.Load per (specDir, entry) per run, see ADR-0008.
func (r *Resolver) Inputs(ctx context.Context, specDir string, declared *toolresolver.DeclaredTool) ([]string, error) {
	listing, _, err := r.list(ctx, specDir, declared)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), listing.InternalFiles...), nil
}

// Versions returns one ResolvedVersion per external module reachable from the
// declared entry, encoded as "go-deps:<path>@<version>+sum:<go.sum-line>" so
// dep bumps and go.sum drift both invalidate resolved_versions_hash.
func (r *Resolver) Versions(ctx context.Context, specDir string, declared *toolresolver.DeclaredTool) ([]toolresolver.ResolvedVersion, error) {
	listing, entry, err := r.list(ctx, specDir, declared)
	if err != nil {
		return nil, err
	}
	source := Name + ":" + entry
	versions := make([]toolresolver.ResolvedVersion, 0, len(listing.ExternalModules))
	for _, m := range listing.ExternalModules {
		versions = append(versions, toolresolver.ResolvedVersion{
			Name:    m.Path,
			Source:  source,
			Version: encodeExternalVersion(m),
		})
	}
	return versions, nil
}

// list resolves declared into a (listing, entry) pair via the memoised
// SourceLister. The lister evaluates entry inside the spec's working
// directory, matching where `go run ./cmd/foo` actually executes — this is
// what makes monorepos with multiple Go modules work: a spec under
// submodule/ asks the lister to resolve against submodule's go.mod, not the
// repo-root module.
func (r *Resolver) list(ctx context.Context, specDir string, declared *toolresolver.DeclaredTool) (lister.Listing, string, error) {
	entry, err := r.resolveEntry(declared)
	if err != nil {
		return lister.Listing{}, "", err
	}
	listing, err := r.lister.List(ctx, specDir, entry)
	if err != nil {
		return lister.Listing{}, "", fmt.Errorf("go-local: list sources for %q (spec %q): %w", entry, specDir, err)
	}
	return listing, entry, nil
}

// encodeExternalVersion produces the canonical hash-input string for one
// external Go module. The go.sum line is folded in as a SHA-256 digest so the
// version stays single-line and bounded length, while still flipping the fingerprint
// when go.sum drifts (Go's classic supply-chain anchor).
func encodeExternalVersion(m lister.ExternalModule) string {
	label := DepsPrefix + m.Path + "@" + m.Version
	if m.GoSumLine == "" {
		return label
	}
	sum := sha256.Sum256([]byte(m.GoSumLine))
	return label + "+sum:" + hex.EncodeToString(sum[:])
}

// isRelativeEntry reports whether s is in the spec-relative entry form the
// resolver accepts: bare "." / "..", or starting with "./" / "../". Parent-
// relative forms are valid for nested specs that share a generator with their
// parent (e.g. `tools: [{go-local: ../cmd/gen}]`).
func isRelativeEntry(s string) bool {
	return s == "." || s == ".." ||
		strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

func (r *Resolver) resolveEntry(declared *toolresolver.DeclaredTool) (string, error) {
	if declared == nil {
		return "", errors.New("go-local: declared tool is required (auto-dispatch was removed in ADR-0005)")
	}
	if declared.Entry == "" {
		return "", errors.New("go-local: declared entry is required")
	}
	if !isRelativeEntry(declared.Entry) {
		return "", fmt.Errorf("go-local: declared entry must start with %q or %q (or be %q / %q), got %q",
			"./", "../", ".", "..", declared.Entry)
	}
	return declared.Entry, nil
}
