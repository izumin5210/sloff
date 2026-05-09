// Package glob expands inputs/outputs patterns declared in a sloff.yml.
package glob

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Expand evaluates each pattern relative to specDir (which itself is relative to
// repoRoot) and returns the union of matched file paths, expressed relative to
// repoRoot, in path-ascending order with duplicates removed.
//
// Patterns use doublestar syntax (`**` is supported). Only regular files are
// returned; directories matched by a pattern are filtered out.
//
// Patterns may use `..` to reference siblings of specDir
// (e.g. `../<sibling>/**/*.go`). This is the canonical way to express a
// cross-directory codegen task whose inputs / outputs span more than one
// dir while keeping the spec located alongside the concern it owns. Patterns
// whose normalised form would resolve outside repoRoot are rejected so a
// malformed spec can never read or hash files outside the repo.
func Expand(repoRoot, specDir string, patterns []string) ([]string, error) {
	fsys := os.DirFS(repoRoot)
	specDirSlash := filepath.ToSlash(specDir)

	seen := make(map[string]struct{})
	for _, p := range patterns {
		// path.Join already calls path.Clean, which collapses `.` / `..` while
		// leaving doublestar's `**` token intact (Clean only normalises path
		// separators and dot segments).
		joined := path.Join(specDirSlash, p)
		if joined == ".." || strings.HasPrefix(joined, "../") {
			return nil, fmt.Errorf("glob %q escapes repo root", p)
		}

		matches, err := doublestar.Glob(fsys, joined, doublestar.WithFilesOnly())
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", p, err)
		}
		for _, m := range matches {
			seen[filepath.FromSlash(m)] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
