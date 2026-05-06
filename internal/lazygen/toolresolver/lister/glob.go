package lister

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// NewGlob returns a SourceLister that enumerates files under the entry directory
// using doublestar include / exclude patterns evaluated relative to that entry.
//
// This is the "precision-light fallback" path documented in resolver-go-local.md:
// it never reads transitive imports, treats every match as internal, and never
// produces ExternalModules. It is the retreat option when goPackagesLister cannot
// analyse a project structure.
func NewGlob(repoRoot string, includes, excludes []string) SourceLister {
	return &globLister{
		repoRoot: repoRoot,
		includes: includes,
		excludes: excludes,
	}
}

type globLister struct {
	repoRoot string
	includes []string
	excludes []string
}

func (l *globLister) List(_ context.Context, entry string) (Listing, error) {
	base, err := normalizeEntry(entry)
	if err != nil {
		return Listing{}, err
	}
	absBase := filepath.Join(l.repoRoot, base)
	fsys := os.DirFS(absBase)

	seen := map[string]struct{}{}
	for _, p := range l.includes {
		matches, err := doublestar.Glob(fsys, p, doublestar.WithFilesOnly())
		if err != nil {
			return Listing{}, fmt.Errorf("glob include %q: %w", p, err)
		}
		for _, m := range matches {
			seen[joinRel(base, m)] = struct{}{}
		}
	}
	for _, p := range l.excludes {
		matches, err := doublestar.Glob(fsys, p, doublestar.WithFilesOnly())
		if err != nil {
			return Listing{}, fmt.Errorf("glob exclude %q: %w", p, err)
		}
		for _, m := range matches {
			delete(seen, joinRel(base, m))
		}
	}

	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return Listing{InternalFiles: files}, nil
}

// normalizeEntry converts a `go run`-form import path ("./cmd/foo/...",
// "./cmd/foo", "./cmd/foo/") into a slashless relative directory anchored at
// the repo root. Entries that escape the repo (e.g. "../foo") are rejected so
// the lister cannot produce paths outside the repoRoot.
func normalizeEntry(entry string) (string, error) {
	if !strings.HasPrefix(entry, "./") && entry != "." {
		return "", fmt.Errorf("entry must start with %q, got %q", "./", entry)
	}
	e := strings.TrimPrefix(entry, "./")
	e = strings.TrimSuffix(e, "/...")
	e = strings.TrimSuffix(e, "/")
	if e == "" || e == "." {
		return ".", nil
	}
	if strings.Contains(e, "..") {
		return "", fmt.Errorf("entry must not escape the repo root: %q", entry)
	}
	return filepath.FromSlash(e), nil
}

// joinRel joins a normalized base with a forward-slash glob match and returns a
// repo-relative OS-native path.
func joinRel(base, match string) string {
	if base == "." {
		return filepath.FromSlash(match)
	}
	return filepath.FromSlash(path.Join(filepath.ToSlash(base), match))
}
