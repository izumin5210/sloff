// Package glob expands inputs/outputs patterns declared in a sloff.yml.
package glob

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
)

// EscapesRoot reports whether joined — a path.Join-cleaned, slash-form,
// repo-root-relative pattern or path — escapes the repository root. Expand
// and the depends-validation layers share this single definition of the
// escape policy.
func EscapesRoot(joined string) bool {
	return joined == ".." || strings.HasPrefix(joined, "../")
}

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
	return NewExpander(repoRoot).Expand(specDir, patterns)
}

// Expander expands patterns like Expand while memoising per-pattern glob
// results within a single planning pass. Different specs routinely declare
// the same normalised pattern (e.g. `**/*.proto` from proto/, or
// `../go/pkg/proto/**/*.pb.go` from several spec dirs, which all join to the
// same repo-relative pattern); without memoisation each occurrence re-walks
// the same — often huge — subtree.
//
// The cache is keyed by the joined (spec-dir-resolved) pattern, so it is only
// valid while the underlying tree is unchanged. Use it for plan-phase
// expansion; never reuse one across task execution, which mutates the tree
// (post-run output resolution must call Expand directly).
//
// Safe for concurrent use. Two goroutines racing on the same uncached
// pattern may both walk it; the duplicate walk is accepted instead of
// single-flighting because planning passes fan out across many distinct
// patterns and collisions are rare.
type Expander struct {
	repoRoot string
	mu       sync.Mutex
	// cache maps the joined slash-form pattern to its raw doublestar matches
	// (slash-form, repo-relative).
	cache map[string][]string
}

// NewExpander returns an Expander rooted at repoRoot with an empty cache.
func NewExpander(repoRoot string) *Expander {
	return &Expander{repoRoot: repoRoot, cache: map[string][]string{}}
}

// Expand behaves exactly like the package-level Expand, consulting the
// memoised per-pattern results first.
func (e *Expander) Expand(specDir string, patterns []string) ([]string, error) {
	fsys := os.DirFS(e.repoRoot)
	specDirSlash := filepath.ToSlash(specDir)

	seen := make(map[string]struct{})
	for _, p := range patterns {
		// path.Join already calls path.Clean, which collapses `.` / `..` while
		// leaving doublestar's `**` token intact (Clean only normalises path
		// separators and dot segments).
		joined := path.Join(specDirSlash, p)
		if EscapesRoot(joined) {
			return nil, fmt.Errorf("glob %q escapes repo root", p)
		}

		e.mu.Lock()
		matches, ok := e.cache[joined]
		e.mu.Unlock()
		if !ok {
			var err error
			matches, err = doublestar.Glob(fsys, joined, doublestar.WithFilesOnly())
			if err != nil {
				return nil, fmt.Errorf("glob %q: %w", p, err)
			}
			e.mu.Lock()
			e.cache[joined] = matches
			e.mu.Unlock()
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
