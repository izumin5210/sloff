// Package golocal implements toolresolver.Resolver for repo-local Go tools.
//
// It applies to tools that are built from sources living inside the repository
// (typical examples: bespoke protoc plugins, code generators wired up via
// `go run ./cmd/...`). These tools have no SemVer to read, so the logical
// version is the SHA256 of their transitive source contributions, computed via
// an injectable lister.SourceLister.
//
// The resolver covers two entry shapes:
//   - cmd auto-dispatch: `go run ./cmd/foo[/...]` triggers CanResolve
//   - explicit declaration: spec entry `tools: [{go-local: ./cmd/foo}]` selects
//     this resolver via Registry.byName, even when the actual cmd is a prebuilt
//     binary (e.g. installed by `go build -o bin/foo`)
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
	"path"
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

	// The cmd runs with cwd=<repoRoot>/<specDir>, so `go run ./cmd/foo` resolves
	// against the spec directory. The lister, however, operates at repoRoot, so
	// the entry must be rebased: `./cmd/foo` from spec "spec/sub" becomes
	// `./spec/sub/cmd/foo`. The version label keeps the spec-relative form so
	// the display string stays stable per generator regardless of where the
	// spec sits in the repository.
	listerEntry := rebaseEntryToRepoRoot(specDir, entry)

	listing, err := r.lister.List(ctx, listerEntry)
	if err != nil {
		return nil, fmt.Errorf("go-local: list sources for %q: %w", listerEntry, err)
	}
	digest, err := hashListing(r.repoRoot, listing)
	if err != nil {
		return nil, fmt.Errorf("go-local: hash sources for %q: %w", listerEntry, err)
	}

	source := Name + ":" + entry
	return []toolresolver.ToolVersion{{
		Name:    entry,
		Source:  source,
		Version: source + "@sha256:" + hex.EncodeToString(digest),
	}}, nil
}

// rebaseEntryToRepoRoot prefixes a spec-relative entry like "./cmd/foo" with
// the spec directory so the lister, which runs at repoRoot, sees the path it
// expects. The trailing "/..." (Go's recursive package suffix) is preserved.
func rebaseEntryToRepoRoot(specDir, entry string) string {
	trimmed := strings.TrimPrefix(entry, "./")
	slashDir := filepath.ToSlash(specDir)
	if slashDir == "" || slashDir == "." {
		return "./" + trimmed
	}
	return "./" + path.Join(slashDir, trimmed)
}

func (r *Resolver) resolveEntry(declared *toolresolver.DeclaredTool) (string, error) {
	if declared == nil {
		return "", errors.New("go-local: declared tool is required (auto-dispatch was removed in ADR-0005)")
	}
	if declared.Entry == "" {
		return "", errors.New("go-local: declared entry is required")
	}
	if !strings.HasPrefix(declared.Entry, "./") {
		return "", fmt.Errorf("go-local: declared entry must start with %q, got %q", "./", declared.Entry)
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
