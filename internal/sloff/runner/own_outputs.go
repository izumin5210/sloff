package runner

import (
	"path"
	"path/filepath"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/izumin5210/sloff/internal/sloff/glob"
)

// joinOutputPatterns joins a task's output patterns with its spec dir in slash
// form so they can be matched against repo-relative input paths.
func joinOutputPatterns(specDir string, patterns []string) []string {
	dirSlash := filepath.ToSlash(specDir)
	joined := make([]string, 0, len(patterns))
	for _, p := range patterns {
		j := path.Join(dirSlash, p)
		if glob.EscapesRoot(j) {
			continue
		}
		joined = append(joined, j)
	}
	return joined
}

// dropOwnOutputs splits paths into those kept as inputs and those matching
// the task's own output patterns. A deterministic generator's result cannot
// depend on its own pre-run outputs, and hashing them makes any run that
// rewrites them record a key the post-run tree no longer matches (ADR-0022).
// Matching is by pattern, not by existence, so the split is the same on a
// clean checkout.
func dropOwnOutputs(paths, joinedOutputPatterns []string) (kept, dropped []string) {
	if len(joinedOutputPatterns) == 0 {
		return paths, nil
	}
	kept = make([]string, 0, len(paths))
	for _, p := range paths {
		if matchesAny(joinedOutputPatterns, filepath.ToSlash(p)) {
			dropped = append(dropped, p)
			continue
		}
		kept = append(kept, p)
	}
	return kept, dropped
}

func matchesAny(patterns []string, slashPath string) bool {
	for _, pattern := range patterns {
		if ok, err := doublestar.Match(pattern, slashPath); err == nil && ok {
			return true
		}
	}
	return false
}

func slashPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.ToSlash(p)
	}
	return out
}
