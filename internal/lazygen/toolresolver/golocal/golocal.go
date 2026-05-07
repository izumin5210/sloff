// Package golocal implements toolresolver.Resolver for repo-local Go tools.
//
// It applies to tools that are built from sources living inside the repository
// (typical examples: bespoke protoc plugins, code generators wired up via
// `go run ./cmd/...`). These tools have no SemVer to read, so the cache key
// is split across two of lazygen's hash buckets:
//
//   - Internal source files (main module / repo-local sources) become
//     ExtraInputs and feed files_hash via the runner's input merge. This is
//     what lets depgraph wire upstream codegen tasks (whose outputs land
//     inside the same source tree the tool reads) to this task automatically
//     by the existing output-overlap rule, with no extra dependency channel.
//   - External Go modules become individual ToolVersion entries
//     ("go-deps:<path>@<version>+sum:<go.sum-line>") and feed tools_hash, so
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
//     go-deps ToolVersion encoding both the original path and the
//     replacement target, so swapping replacement targets flips tools_hash.
package golocal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

// Name is the resolver identifier referenced by spec tools[] entries.
const Name = "go-local"

// DepsPrefix is the version-string prefix for external Go module entries.
// Mirrors the pnpm-deps prefix used by pnpm-local; both are surfaced via the
// same Result.Versions channel so consumers see a uniform "<channel>-deps"
// shape regardless of language.
const DepsPrefix = "go-deps:"

// Resolver resolves a Go-local tool's source contributions (as ExtraInputs)
// and external module set (as go-deps ToolVersions).
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

// Resolve splits the lister's output into:
//   - ExtraInputs: every internal Go file path the lister enumerated
//     (repo-relative slash form), folded into the task's input set so
//     files_hash captures their content and depgraph can wire upstream
//     producers via output overlap.
//   - Versions: one ToolVersion per external module, encoded as
//     "go-deps:<path>@<version>+sum:<go.sum-line>" so dep bumps and go.sum
//     drift both invalidate tools_hash.
func (r *Resolver) Resolve(ctx context.Context, specDir string, _ []string, declared *toolresolver.DeclaredTool) (toolresolver.Result, error) {
	entry, err := r.resolveEntry(declared)
	if err != nil {
		return toolresolver.Result{}, err
	}

	// The lister evaluates entry inside the spec's working directory, matching
	// where `go run ./cmd/foo` actually executes. This is what makes monorepos
	// with multiple Go modules work: a spec under submodule/ asks the lister to
	// resolve against submodule's go.mod, not the repo-root module.
	listing, err := r.lister.List(ctx, specDir, entry)
	if err != nil {
		return toolresolver.Result{}, fmt.Errorf("go-local: list sources for %q (spec %q): %w", entry, specDir, err)
	}

	source := Name + ":" + entry
	versions := make([]toolresolver.ToolVersion, 0, len(listing.ExternalModules))
	for _, m := range listing.ExternalModules {
		versions = append(versions, toolresolver.ToolVersion{
			Name:    m.Path,
			Source:  source,
			Version: encodeExternalVersion(m),
		})
	}

	return toolresolver.Result{
		Versions:    versions,
		ExtraInputs: append([]string(nil), listing.InternalFiles...),
	}, nil
}

// encodeExternalVersion produces the canonical hash-input string for one
// external Go module. The go.sum line is folded in as a SHA-256 digest so the
// version stays single-line and bounded length, while still flipping the cache
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
