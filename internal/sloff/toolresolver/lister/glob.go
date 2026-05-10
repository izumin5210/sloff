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

func (l *globLister) List(_ context.Context, specDir, entry string) (Listing, error) {
	base, err := normalizeEntry(entry)
	if err != nil {
		return Listing{}, err
	}
	absBase := filepath.Join(l.repoRoot, specDir, base)
	// Refuse entries that resolve outside repoRoot. Parent-relative entries
	// (e.g. `../cmd/gen`) are valid as long as the final target stays inside
	// the repo; absolute or deep `../../../...` traversals would tie the
	// listing to per-developer paths and break OS-neutral fingerprint sharing.
	if rel, err := filepath.Rel(l.repoRoot, absBase); err != nil || strings.HasPrefix(rel, "..") {
		return Listing{}, fmt.Errorf("entry %q resolves outside repo root", entry)
	}
	fsys := os.DirFS(absBase)

	seen := map[string]struct{}{}
	for _, p := range l.includes {
		matches, err := doublestar.Glob(fsys, p, doublestar.WithFilesOnly())
		if err != nil {
			return Listing{}, fmt.Errorf("glob include %q: %w", p, err)
		}
		for _, m := range matches {
			seen[joinRel(specDir, base, m)] = struct{}{}
		}
	}
	for _, p := range l.excludes {
		matches, err := doublestar.Glob(fsys, p, doublestar.WithFilesOnly())
		if err != nil {
			return Listing{}, fmt.Errorf("glob exclude %q: %w", p, err)
		}
		for _, m := range matches {
			delete(seen, joinRel(specDir, base, m))
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
// "./cmd/foo", "../cmd/foo", "../...", ".", "..") into an OS-native relative
// directory the lister can join onto specDir. Parent-relative forms are
// preserved because nested specs that share a generator with a parent
// directory rely on them; the caller separately verifies that the joined
// (repoRoot, specDir, base) target stays inside the repo.
func normalizeEntry(entry string) (string, error) {
	if entry != "." && entry != ".." &&
		!strings.HasPrefix(entry, "./") && !strings.HasPrefix(entry, "../") {
		return "", fmt.Errorf("entry must start with %q or %q (or be %q / %q), got %q",
			"./", "../", ".", "..", entry)
	}
	e := strings.TrimSuffix(entry, "/...")
	e = strings.TrimSuffix(e, "/")
	if e == "" {
		return ".", nil
	}
	return filepath.Clean(filepath.FromSlash(e)), nil
}

// joinRel composes a repo-relative slash-form path from specDir, the normalized
// entry base, and a glob match. path.Join collapses any `..` segments that
// came in through a parent-relative entry so the resulting key is canonical.
// Slashes (not OS-native separators) are required so the same source tree
// hashes identically across Windows and Unix; downstream callers that need
// to read the file convert with filepath.FromSlash.
func joinRel(specDir, base, match string) string {
	parts := match
	if base != "." {
		parts = path.Join(filepath.ToSlash(base), parts)
	}
	if slashSpec := filepath.ToSlash(specDir); slashSpec != "" && slashSpec != "." {
		parts = path.Join(slashSpec, parts)
	}
	return parts
}
