// Package script implements toolresolver.Resolver for the prebuilt-binary channel: it
// runs a user-declared command (typically <bin> --version), captures stdout, optionally
// applies an extract regex, and returns the resulting string as the OS-neutral logical
// version. Because the running binary is the source of truth, lockfile-vs-install drift
// is structurally impossible for this channel and there is no companion preflight checker.
package script

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
)

// Name is the resolver identifier referenced by spec tools[] entries.
const Name = "script"

// Resolver is the script-based version resolver.
type Resolver struct {
	repoRoot string

	mu    sync.Mutex
	cache map[string]string // (exec joined + "\x00" + extract) → already-resolved version literal
}

// New returns a Resolver rooted at repoRoot. Subprocesses inherit the parent environment
// and run with a working directory of <repoRoot>/<specDir> when Resolve is invoked.
func New(repoRoot string) *Resolver {
	return &Resolver{repoRoot: repoRoot, cache: map[string]string{}}
}

// Name implements toolresolver.Resolver.
func (r *Resolver) Name() string { return Name }

// CanResolve always returns false. The script resolver is declared-only because
// auto-dispatch ("just call cmd[0] --version") could silently capture build timestamps,
// commit hashes, or OS/arch tokens in --version output and break OS-neutral caching.
// Users opt in by writing tools: [{exec: [...]}] in spec.
func (r *Resolver) CanResolve(string, []string) bool { return false }

// Resolve implements toolresolver.Resolver. declared must be non-nil and must specify Exec.
func (r *Resolver) Resolve(ctx context.Context, specDir string, _ []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
	if declared == nil {
		return nil, errors.New("script: requires explicit tools[] declaration; auto-dispatch is not supported")
	}
	if len(declared.Exec) == 0 {
		return nil, errors.New("script: exec is required")
	}

	cacheKey := strings.Join(declared.Exec, "\x00") + "\x01" + declared.Extract

	r.mu.Lock()
	if cached, ok := r.cache[cacheKey]; ok {
		r.mu.Unlock()
		return []toolresolver.ToolVersion{makeVersion(declared.Exec[0], cached)}, nil
	}
	r.mu.Unlock()

	stdout, err := r.runVersion(ctx, specDir, declared.Exec)
	if err != nil {
		return nil, err
	}
	captured, err := applyExtract(stdout, declared.Extract)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.cache[cacheKey] = captured
	r.mu.Unlock()

	return []toolresolver.ToolVersion{makeVersion(declared.Exec[0], captured)}, nil
}

func (r *Resolver) runVersion(ctx context.Context, specDir string, argv []string) (string, error) {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = filepath.Join(r.repoRoot, specDir)
	var out bytes.Buffer
	c.Stdout = &out
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("script: %s failed: %w", strings.Join(argv, " "), err)
	}
	return strings.TrimSpace(out.String()), nil
}

func applyExtract(stdout, pattern string) (string, error) {
	if pattern == "" {
		if stdout == "" {
			return "", errors.New("script: stdout is empty (no extract pattern configured)")
		}
		return stdout, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("script: invalid extract pattern %q: %w", pattern, err)
	}
	m := re.FindStringSubmatch(stdout)
	switch {
	case m == nil:
		return "", fmt.Errorf("script: extract pattern %q did not match stdout %q", pattern, stdout)
	case len(m) >= 2:
		return m[1], nil
	default:
		return m[0], nil
	}
}

func makeVersion(execHead, captured string) toolresolver.ToolVersion {
	bin := filepath.Base(execHead)
	return toolresolver.ToolVersion{
		Name:    bin,
		Source:  "script:" + bin,
		Version: "script:" + bin + "@" + captured,
	}
}
