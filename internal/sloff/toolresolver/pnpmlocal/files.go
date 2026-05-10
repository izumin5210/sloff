package pnpmlocal

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// FileEnumerator returns the list of source files sloff should treat as
// "everything in pkgDir that the user committed (or could commit)" —
// repo-relative slash-form paths, deduplicated, but not necessarily sorted.
// pkgDir is repo-relative OS-native form. The implementation must respect
// .gitignore so that build outputs (typically dist/, build/, etc.) don't
// leak into ExtraInputs (which would tie the fingerprint to artefacts the user
// regenerates locally). Production enumerators must also exclude sloff's
// own state directory (sloffStateDir) — see GitLsFiles for why.
//
// Callers don't memoise: the resolver wraps the enumerator with its own
// per-tool cache so each workspace dir is enumerated exactly once per run.
type FileEnumerator func(ctx context.Context, repoRoot, pkgDir string) ([]string, error)

// sloffStateDir is the path prefix of the directory sloff owns inside
// the repo (fingerprints, etc.). pnpm-local enumeration must drop entries
// under this prefix because hashing them feeds sloff's own writes back
// into its inputs and self-invalidates the fingerprint on every subsequent run.
// Compared to .git/, which git itself hides from ls-files, .sloff/ is a
// regular tracked / untracked directory so we filter explicitly.
const sloffStateDir = ".sloff/"

// GitLsFiles is the default FileEnumerator: it shells out to
//
//	git ls-files --cached --others --exclude-standard -- <pkgDir>
//
// run from repoRoot. The flag set returns:
//
//   - --cached: tracked files (the canonical set the user committed)
//   - --others --exclude-standard: untracked files NOT matched by .gitignore /
//     .git/info/exclude / global ignore. This catches files the user
//     intends to commit but hasn't `git add`-ed yet, while still skipping
//     gitignored build artefacts.
//
// The combination matches Turborepo's default "everything in the package
// that's in (or could be in) git". We rely on git rather than implementing a
// .gitignore parser because sloff's fingerprints are already git-managed
// and `git ls-files` is the bit-exact source of truth for "what's in this
// repo from git's point of view".
func GitLsFiles(ctx context.Context, repoRoot, pkgDir string) ([]string, error) {
	relDir := filepath.ToSlash(pkgDir)
	if relDir == "" {
		relDir = "."
	}

	cmd := exec.CommandContext(
		ctx,
		"git", "ls-files",
		"--cached",
		"--others",
		"--exclude-standard",
		"--", relDir,
	)
	cmd.Dir = repoRoot

	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		stderr := strings.TrimSpace(errBuf.String())
		if stderr != "" {
			return nil, fmt.Errorf("git ls-files (dir %q): %w: %s", relDir, err, stderr)
		}
		return nil, fmt.Errorf("git ls-files (dir %q): %w", relDir, err)
	}

	// git ls-files emits one path per line, slash-form, repo-relative.
	// Trailing newline produces an empty final line we filter out. We also
	// drop anything under sloff's own state directory: when pkgDir is the
	// repo-root importer, ls-files would otherwise return .sloff/fingerprints/**,
	// and hashing files sloff itself rewrites every run makes the
	// resolver self-invalidate forever. Filtering lives here rather than in
	// the resolver because production callers always need it; the test
	// FileEnumerators don't go through this function and can't accidentally
	// surface .sloff/ paths anyway.
	lines := strings.Split(out.String(), "\n")
	files := make([]string, 0, len(lines))
	for _, l := range lines {
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, sloffStateDir) {
			continue
		}
		files = append(files, l)
	}
	return files, nil
}
