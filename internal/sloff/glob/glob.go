// Package glob expands inputs/outputs patterns declared in a sloff.yml.
package glob

import (
	"fmt"
	"io/fs"
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

// Expander expands patterns like Expand while memoising both the per-pattern
// result AND the recursive file listing of each pattern's literal base dir,
// within a single planning pass.
//
// Two levels of reuse matter on a large repo:
//
//   - Different specs routinely declare the same normalised pattern
//     (e.g. `**/*.proto` from proto/), so identical patterns are served from
//     the per-pattern cache.
//   - Distinct patterns frequently share a literal base before their first
//     `**` (e.g. `<pkg>/**/*.go`, `<pkg>/**/server/server.gen.go`,
//     `<pkg>/**/cmd/main.gen.go` — all rooted at `<pkg>`). Calling
//     doublestar.Glob once per pattern re-walks that — often huge —
//     subtree every time. Instead we walk each base exactly once into a flat
//     file listing and match every sharing pattern against it in memory with
//     doublestar.Match. The match output is identical to Glob (same engine,
//     files-only), but the expensive directory descent happens once per base
//     rather than once per pattern.
//
// Both caches are keyed by paths/patterns relative to repoRoot and are only
// valid while the underlying tree is unchanged. Use it for plan-phase
// expansion; never reuse one across task execution, which mutates the tree
// (post-run output resolution must call Expand directly).
//
// Safe for concurrent use. Goroutines racing on the same uncached pattern or
// base may both compute it; the duplicate work is accepted instead of
// single-flighting because planning passes fan out across many distinct
// patterns and collisions are rare.
type Expander struct {
	repoRoot string
	fsys     fs.FS // os.DirFS(repoRoot); shared so base walks are reused

	mu sync.Mutex
	// cache maps the joined slash-form pattern to its matches (slash-form,
	// repo-relative).
	cache map[string][]string

	baseMu sync.Mutex
	// baseFiles maps a literal base dir (slash-form, repo-relative) to every
	// regular file beneath it (slash-form, repo-relative). An empty entry means
	// the base has no files or does not exist (parity with doublestar.Glob's
	// empty result), and is still cached so siblings skip the re-walk.
	baseFiles map[string][]string
}

// NewExpander returns an Expander rooted at repoRoot with empty caches.
func NewExpander(repoRoot string) *Expander {
	return &Expander{
		repoRoot:  repoRoot,
		fsys:      os.DirFS(repoRoot),
		cache:     map[string][]string{},
		baseFiles: map[string][]string{},
	}
}

// Expand behaves exactly like the package-level Expand, consulting the
// memoised per-pattern and per-base caches first.
func (e *Expander) Expand(specDir string, patterns []string) ([]string, error) {
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

		matches, err := e.match(joined)
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

// match returns the files matching the joined (spec-dir-resolved) pattern,
// reusing the per-pattern cache and, for patterns with a non-trivial literal
// base, a per-base file listing shared with sibling patterns.
func (e *Expander) match(joined string) ([]string, error) {
	e.mu.Lock()
	cached, ok := e.cache[joined]
	e.mu.Unlock()
	if ok {
		return cached, nil
	}

	matches, err := e.computeMatches(joined)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	e.cache[joined] = matches
	e.mu.Unlock()
	return matches, nil
}

func (e *Expander) computeMatches(joined string) ([]string, error) {
	// Reject malformed patterns up front so the in-memory match path errors
	// exactly like doublestar.Glob would — even when the base has no files and
	// doublestar.Match would otherwise never be called.
	if !doublestar.ValidatePattern(joined) {
		return nil, doublestar.ErrBadPattern
	}

	base, _ := doublestar.SplitPattern(joined)

	// A base of "." (or empty) means the pattern's `**`/`*` starts at the repo
	// root; flattening the whole tree would pull in .git / node_modules and
	// cost more than it saves. Fall back to doublestar.Glob, which prunes via
	// the pattern as it descends. Such root-anchored patterns are rare (real
	// specs root their globs at proto/, go/..., web/...).
	if base == "." || base == "" {
		return doublestar.Glob(e.fsys, joined, doublestar.WithFilesOnly())
	}

	// Without a `**` segment the pattern matches within a bounded depth, so a
	// direct Glob descends only as far as the pattern allows and never walks the
	// whole subtree. The flatten-the-base reuse only pays off for recursive
	// `**` patterns that would otherwise re-walk a shared base once per pattern;
	// for a shallow pattern it would walk the entire base subtree for nothing
	// (e.g. a single literal file under a large service dir). A non-segment
	// `**` (e.g. `a**b`) at worst takes the slower base walk and still matches
	// correctly, so the cheap substring test is safe.
	if !strings.Contains(joined, "**") {
		return doublestar.Glob(e.fsys, joined, doublestar.WithFilesOnly())
	}

	files, err := e.filesUnder(base)
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, f := range files {
		ok, err := doublestar.Match(joined, f)
		if err != nil {
			return nil, err
		}
		if ok {
			matches = append(matches, f)
		}
	}
	return matches, nil
}

// filesUnder returns every regular file beneath base (slash-form,
// repo-relative), walking it once and memoising the result so sibling patterns
// reuse it.
//
// base is the literal prefix doublestar.SplitPattern peeled off the pattern,
// and SplitPattern unescapes it — so a directory named e.g. `[api]` arrives as
// the four bytes `[api]`. We therefore must NOT splice base back into a glob
// string (`base+"/**"` would parse `[api]` as a character class and match
// nothing). Instead fs.Sub roots a subtree FS at base literally, and `**` is
// globbed inside it. fs.Sub keeps delegating Stat to the os.DirFS, so the walk
// still follows symlinked directories exactly as a per-pattern doublestar.Glob
// would. A non-existent base yields an empty list, matching the empty result
// doublestar.Glob produces for a pattern whose base is absent.
func (e *Expander) filesUnder(base string) ([]string, error) {
	e.baseMu.Lock()
	cached, ok := e.baseFiles[base]
	e.baseMu.Unlock()
	if ok {
		return cached, nil
	}

	sub, err := fs.Sub(e.fsys, base)
	if err != nil {
		return nil, err
	}
	rel, err := doublestar.Glob(sub, "**", doublestar.WithFilesOnly())
	if err != nil {
		return nil, err
	}
	files := make([]string, len(rel))
	for i, r := range rel {
		files[i] = base + "/" + r
	}

	e.baseMu.Lock()
	e.baseFiles[base] = files
	e.baseMu.Unlock()
	return files, nil
}
