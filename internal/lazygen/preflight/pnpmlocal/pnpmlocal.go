// Package pnpmlocal implements the preflight Checker for pnpm workspace-local
// tools. It catches the everyday "edited src/, forgot to rebuild" trap before
// lazygen invokes the generator, so users see one actionable fix-it message
// instead of opaque "module not found" runtime failures from the cmd.
//
// Build-required packages are detected heuristically by looking at
// package.json: any bin or main entry that points into dist/ marks the
// package as "needs build". ts-node / tsx style packages (bin points into
// src/) are skipped because they execute the source directly.
package pnpmlocal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/izumin5210/lazygen/internal/lazygen/preflight"
	pnpmws "github.com/izumin5210/lazygen/internal/lazygen/toolresolver/pnpmlocal"
)

// Name is the checker identifier; matches the resolver name (architecture.md
// pairs Resolver and Checker by Name).
const Name = "pnpm-local"

const distSegment = "dist"

// Checker verifies that build-required pnpm workspace packages have a fresh
// dist/.
type Checker struct {
	repoRoot string
}

// New returns a Checker rooted at repoRoot.
func New(repoRoot string) *Checker {
	return &Checker{repoRoot: repoRoot}
}

// Name implements preflight.Checker.
func (c *Checker) Name() string { return Name }

// Check loads the workspace and reports an Issue per build-required package
// whose dist/ is missing or older than its src/. A repo without pnpm-lock.yaml
// is treated as "no pnpm workspace" and returns OK silently — the CLI
// registers the checker unconditionally, but Go-only repos must not see
// pnpm-related errors on every run.
func (c *Checker) Check(_ context.Context, _ string) (preflight.Result, error) {
	ws, err := pnpmws.LoadWorkspace(c.repoRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return preflight.Result{OK: true}, nil
		}
		return preflight.Result{}, fmt.Errorf("pnpm-local preflight: %w", err)
	}

	var issues []preflight.Issue
	for _, pkg := range ws.All() {
		issue, ok, err := c.checkPackage(pkg)
		if err != nil {
			return preflight.Result{}, fmt.Errorf("pnpm-local preflight: %s: %w", pkg.Name, err)
		}
		if ok {
			issues = append(issues, issue)
		}
	}
	return preflight.Result{OK: len(issues) == 0, Issues: issues}, nil
}

// checkPackage returns (issue, true, nil) when pkg is build-required and its
// dist/ is missing or older than src/, (Issue{}, false, nil) when the package
// is fresh or doesn't require a build, and an error only on IO failures other
// than "src/dist not present".
func (c *Checker) checkPackage(pkg pnpmws.WorkspacePackage) (preflight.Issue, bool, error) {
	if !needsBuild(pkg) {
		return preflight.Issue{}, false, nil
	}
	pkgRoot := filepath.Join(c.repoRoot, pkg.Dir)
	distDir := filepath.Join(pkgRoot, distSegment)
	srcDir := filepath.Join(pkgRoot, "src")

	distLatest, distExists, err := newestMTime(distDir)
	if err != nil {
		return preflight.Issue{}, false, err
	}
	if !distExists {
		return preflight.Issue{
			Channel:    Name,
			Detail:     fmt.Sprintf("%s: dist/ does not exist; build artefact is missing", pkg.Name),
			Suggestion: fmt.Sprintf("pnpm --filter %s build", pkg.Name),
		}, true, nil
	}

	srcLatest, srcExists, err := newestMTime(srcDir)
	if err != nil {
		return preflight.Issue{}, false, err
	}
	// No src/ to compare against → assume the dist alone is the truth (some
	// workspace packages ship pre-built artefacts only). This avoids false
	// positives in unusual repo layouts.
	if !srcExists {
		return preflight.Issue{}, false, nil
	}
	if !distLatest.Before(srcLatest) {
		return preflight.Issue{}, false, nil
	}
	return preflight.Issue{
		Channel:    Name,
		Detail:     fmt.Sprintf("%s: dist/ is older than src/ — rebuild required", pkg.Name),
		Suggestion: fmt.Sprintf("pnpm --filter %s build", pkg.Name),
	}, true, nil
}

// needsBuild reports whether pkg's bin or main entries reference dist/...
// (the conventional output directory). Patterns like src/cli.ts (ts-node /
// tsx) intentionally fall through as "no build needed".
func needsBuild(pkg pnpmws.WorkspacePackage) bool {
	for _, p := range pkg.Bin {
		if pointsIntoDist(p) {
			return true
		}
	}
	if pointsIntoDist(pkg.Main) {
		return true
	}
	return false
}

func pointsIntoDist(p string) bool {
	if p == "" {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == distSegment {
			return true
		}
	}
	return false
}

// newestMTime walks dir recursively and returns the latest file mtime. The
// second return value reports whether the directory exists; a non-existent
// directory yields (zero, false, nil) so callers can distinguish "missing"
// from "empty".
func newestMTime(dir string) (time.Time, bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	if !info.IsDir() {
		return info.ModTime(), true, nil
	}

	var latest time.Time
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
		return nil
	})
	if err != nil {
		return time.Time{}, false, err
	}
	return latest, true, nil
}
