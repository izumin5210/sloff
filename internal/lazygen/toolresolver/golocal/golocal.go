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

// CanResolve auto-dispatches `go run ./...` shaped commands. Build-installed
// binaries (e.g. `bin/protoc-gen-foo`) must be opted in via tools[] declaration
// because there is no signal in the cmd that they are repo-local.
func (r *Resolver) CanResolve(_ string, cmd []string) bool {
	_, ok := extractGoRunEntry(cmd)
	return ok
}

// Resolve returns one ToolVersion. When declared is supplied, declared.Entry
// names the main package; otherwise the entry is extracted from a `go run`
// command. The returned Version is OS-neutral: `go-local:<entry>@sha256:<hex>`.
func (r *Resolver) Resolve(ctx context.Context, _ string, cmd []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
	entry, err := r.resolveEntry(cmd, declared)
	if err != nil {
		return nil, err
	}

	listing, err := r.lister.List(ctx, entry)
	if err != nil {
		return nil, fmt.Errorf("go-local: list sources for %q: %w", entry, err)
	}
	digest, err := hashListing(r.repoRoot, listing)
	if err != nil {
		return nil, fmt.Errorf("go-local: hash sources for %q: %w", entry, err)
	}

	source := Name + ":" + entry
	return []toolresolver.ToolVersion{{
		Name:    entry,
		Source:  source,
		Version: source + "@sha256:" + hex.EncodeToString(digest),
	}}, nil
}

func (r *Resolver) resolveEntry(cmd []string, declared *toolresolver.DeclaredTool) (string, error) {
	if declared != nil {
		if declared.Entry == "" {
			return "", errors.New("go-local: declared entry is required")
		}
		if !strings.HasPrefix(declared.Entry, "./") {
			return "", fmt.Errorf("go-local: declared entry must start with %q, got %q", "./", declared.Entry)
		}
		return declared.Entry, nil
	}
	entry, ok := extractGoRunEntry(cmd)
	if !ok {
		return "", fmt.Errorf("go-local: cmd is not a `go run ./...` form: %v", cmd)
	}
	return entry, nil
}

// extractGoRunEntry returns the first relative argument of a `go run ./...`
// invocation. Flags before the entry are tolerated (e.g.
// `go run -tags foo ./cmd/bar`); the entry is the first arg starting with "./".
// Returns false when cmd is not a go run command, or when no relative entry was
// found in its arguments.
func extractGoRunEntry(cmd []string) (string, bool) {
	if len(cmd) < 3 {
		return "", false
	}
	if filepath.Base(cmd[0]) != "go" || cmd[1] != "run" {
		return "", false
	}
	for _, a := range cmd[2:] {
		if strings.HasPrefix(a, "./") {
			return a, true
		}
	}
	return "", false
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
