package lister

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// NewEsbuild returns a SourceLister backed by github.com/evanw/esbuild's Go API.
//
// It is the standard SourceLister for the pnpm-local resolver. esbuild bundles
// the entry in-process with Metafile=true, then we walk metafile.inputs to get
// the transitive set of source files. node_modules paths are filtered out
// because per ADR-0007 external npm dependencies belong to the script resolver,
// not pnpm-local. Workspace transitive dependencies are picked up because pnpm
// symlinks them into the workspace tree at their real path (PreserveSymlinks
// stays false so we record the resolved path, which lives outside node_modules).
//
// Patterns esbuild cannot statically analyse (eval, runtime require, dynamic
// import()) are out of scope; the user retreats to lister.NewGlob for those
// (see resolver-pnpm-local.md).
func NewEsbuild(repoRoot string) SourceLister {
	return &esbuildLister{repoRoot: repoRoot}
}

type esbuildLister struct {
	repoRoot string
}

func (l *esbuildLister) List(_ context.Context, specDir, entry string) (Listing, error) {
	if !isRelativeEntry(entry) {
		return Listing{}, fmt.Errorf("esbuild lister: entry must start with %q or %q (or be %q / %q), got %q",
			"./", "../", ".", "..", entry)
	}

	entryAbs := filepath.Join(l.repoRoot, specDir, filepath.FromSlash(entry))
	if rel, err := filepath.Rel(l.repoRoot, entryAbs); err != nil || strings.HasPrefix(rel, "..") {
		return Listing{}, fmt.Errorf("esbuild lister: entry %q resolves outside repo root", entry)
	}

	result := api.Build(api.BuildOptions{
		EntryPoints:   []string{entryAbs},
		AbsWorkingDir: l.repoRoot,
		Bundle:        true,
		Metafile:      true,
		Write:         false,
		// Platform=Node so esbuild applies Node module resolution (handles
		// "type": "module" / CommonJS interop / .ts imports) which matches
		// how pnpm tools actually run.
		Platform: api.PlatformNode,
		// Silent — esbuild Build emits warnings/errors via Result; piping its
		// own stderr would confuse users running `lazygen run`.
		LogLevel: api.LogLevelSilent,
	})
	if len(result.Errors) > 0 {
		return Listing{}, fmt.Errorf("esbuild lister: build %q: %s", entry, joinMessages(result.Errors))
	}

	files, err := extractInputs(l.repoRoot, result.Metafile)
	if err != nil {
		return Listing{}, fmt.Errorf("esbuild lister: parse metafile for %q: %w", entry, err)
	}
	return Listing{InternalFiles: files}, nil
}

// extractInputs parses esbuild's metafile JSON, projects each input path to a
// repo-relative slash form, drops node_modules paths (ADR-0007), and returns
// the result sorted ascending.
func extractInputs(repoRoot, metafile string) ([]string, error) {
	if metafile == "" {
		return nil, errors.New("empty metafile")
	}
	var meta struct {
		Inputs map[string]struct{} `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(metafile), &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metafile: %w", err)
	}

	seen := make(map[string]struct{}, len(meta.Inputs))
	for in := range meta.Inputs {
		// esbuild emits forward-slash paths relative to AbsWorkingDir (which we
		// set to repoRoot); on macOS/Linux this is already what we want. We
		// guard for absolute paths just in case esbuild emits one for files it
		// could not relativise (it typically does this for entries outside the
		// CWD that we already reject above).
		clean := filepath.ToSlash(in)
		if filepath.IsAbs(filepath.FromSlash(clean)) {
			rel, err := filepath.Rel(repoRoot, filepath.FromSlash(clean))
			if err != nil {
				continue
			}
			clean = filepath.ToSlash(rel)
		}
		clean = path.Clean(clean)
		if clean == "" || strings.HasPrefix(clean, "../") {
			continue
		}
		if hasSegment(clean, "node_modules") {
			continue
		}
		seen[filepath.FromSlash(clean)] = struct{}{}
	}

	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, filepath.ToSlash(f))
	}
	sort.Strings(out)
	return out, nil
}

func hasSegment(slashPath, segment string) bool {
	for _, p := range strings.Split(slashPath, "/") {
		if p == segment {
			return true
		}
	}
	return false
}

func joinMessages(msgs []api.Message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.Text)
	}
	return strings.Join(parts, "; ")
}

func isRelativeEntry(s string) bool {
	return s == "." || s == ".." ||
		strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}
