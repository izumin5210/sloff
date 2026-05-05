// Package glob expands inputs/outputs patterns declared in a lazygen.yml.
package glob

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/bmatcuk/doublestar/v4"
)

// Expand evaluates each pattern relative to specDir (which itself is relative to repoRoot)
// and returns the union of matched file paths, expressed relative to repoRoot, in
// path-ascending order with duplicates removed.
//
// Patterns use doublestar syntax (** is supported). Only regular files are returned;
// directories matched by a pattern are filtered out.
func Expand(repoRoot, specDir string, patterns []string) ([]string, error) {
	specAbs := filepath.Join(repoRoot, specDir)
	fsys := os.DirFS(specAbs)
	specDirSlash := filepath.ToSlash(specDir)

	seen := make(map[string]struct{})
	for _, p := range patterns {
		matches, err := doublestar.Glob(fsys, p, doublestar.WithFilesOnly())
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", p, err)
		}
		for _, m := range matches {
			joined := path.Join(specDirSlash, m)
			seen[filepath.FromSlash(joined)] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
