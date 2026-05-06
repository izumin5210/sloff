// Package golocal implements toolresolver.Resolver for repo-local Go tools.
//
// It applies to tools that are built from sources living inside the repository
// (typical examples: bespoke protoc plugins, code generators wired up via
// `go run ./cmd/...`). These tools have no SemVer to read, so the logical
// version is the SHA256 of their transitive source contributions, computed via
// an injectable lister.SourceLister.
//
// Per ADR-0005 the resolver is declared-only: it is invoked when the spec wrote
// `tools: [{go-local: ./cmd/foo}]` for the task. The same declaration form is
// used regardless of whether the cmd is `go run ./cmd/foo` or a prebuilt
// binary produced from those sources.
//
// Hashing strategy follows resolver-go-local.md:
//   - internal files (main module / repo-local sources) are SHA256'd by content
//   - external modules are summarised by `<path>@<version>` plus their go.sum
//     line; their files are not read (cryptographically equivalent via go.sum)
//   - replace directives are treated as external (keeps the resolver fast even
//     when local replace points at sibling repositories)
package golocal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

// Name is the resolver identifier referenced by spec tools[] entries.
const Name = "go-local"

// Resolver resolves the logical version of a Go-local tool to
// "go-local:<entry>@sha256:<hex>".
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

// Resolve returns one ToolVersion. declared.Entry names the main package
// (spec-dir-relative, must start with "./"). Per ADR-0005 there is no
// auto-dispatch path: the resolver only runs when the spec wrote
// `tools: [{go-local: ./...}]` for this task. The returned Version is
// OS-neutral: `go-local:<entry>@sha256:<hex>`.
func (r *Resolver) Resolve(ctx context.Context, specDir string, _ []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
	entry, err := r.resolveEntry(declared)
	if err != nil {
		return nil, err
	}

	// The lister evaluates entry inside the spec's working directory, matching
	// where `go run ./cmd/foo` actually executes. This is what makes monorepos
	// with multiple Go modules work: a spec under submodule/ asks the lister to
	// resolve against submodule's go.mod, not the repo-root module.
	listing, err := r.lister.List(ctx, specDir, entry)
	if err != nil {
		return nil, fmt.Errorf("go-local: list sources for %q (spec %q): %w", entry, specDir, err)
	}
	digest, err := hashListing(r.repoRoot, listing)
	if err != nil {
		return nil, fmt.Errorf("go-local: hash sources for %q (spec %q): %w", entry, specDir, err)
	}

	source := Name + ":" + entry
	return []toolresolver.ToolVersion{{
		Name:    entry,
		Source:  source,
		Version: source + "@sha256:" + hex.EncodeToString(digest),
	}}, nil
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

// hashListing folds the listing into a deterministic SHA256 by:
//   - reading each internal file's content and writing path + content digest
//   - writing each external module's "<path>@<version>" label + go.sum line
//
// All entries are NUL-separated and sorted upstream so the digest is invariant
// to lister enumeration order.
func hashListing(repoRoot string, l lister.Listing) ([]byte, error) {
	h := sha256.New()
	for _, f := range l.InternalFiles {
		digest, err := fileSHA256(filepath.Join(repoRoot, f))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		h.Write([]byte(f))
		h.Write([]byte{0})
		h.Write(digest)
		h.Write([]byte{0})
	}
	for _, m := range l.ExternalModules {
		label := m.Path + "@" + m.Version
		h.Write([]byte(label))
		h.Write([]byte{0})
		h.Write([]byte(m.GoSumLine))
		h.Write([]byte{0})
	}
	return h.Sum(nil), nil
}

func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path)
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
