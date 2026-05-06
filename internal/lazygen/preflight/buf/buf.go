// Package buf implements preflight.Checker for buf-related lockfile / pinning
// invariants that must hold before any buf resolver hashing is trusted:
//
//   - every remote plugin in a referenced buf.gen.yaml carries a pinned :vX.Y.Z
//     tag (the lazygen-side equivalent of "lockfile-as-SSoT" because BSR codegen
//     plugins have no resolved-version surface — see resolver-buf.md)
//   - every dep declared in a buf.yaml is recorded in the sibling buf.lock so
//     `buf dep update` was run after the deps list was edited
//
// The checker is constructed with the post-discovery spec list so that one Run
// covers every buf subject in the repo. The Check call's specDir argument is
// ignored because subjects are aggregated from the entire spec set, not scoped
// to a single dir.
package buf

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	yaml "github.com/goccy/go-yaml"

	"github.com/izumin5210/lazygen/internal/lazygen/preflight"
	"github.com/izumin5210/lazygen/internal/lazygen/spec"
	bufresolver "github.com/izumin5210/lazygen/internal/lazygen/toolresolver/buf"
)

const checkerName = "buf"

// Checker validates buf-related lockfile / pinning state. A new Checker is
// constructed per lazygen run after spec discovery; it never mutates state.
type Checker struct {
	repoRoot string
	subjects []subject
}

// subject is one buf.gen.yaml the checker should examine. SpecDir is relative
// to repoRoot (forward slashes); GenPath is relative to SpecDir.
type subject struct {
	SpecDir string
	GenPath string
}

// New aggregates buf subjects across every command in specs and returns a
// Checker ready to run. The aggregation deduplicates by (specDir, genPath) so
// shared buf.gen.yaml files (referenced from multiple tasks) are only checked
// once per run.
func New(repoRoot string, specs []spec.Spec) *Checker {
	return &Checker{
		repoRoot: repoRoot,
		subjects: collectSubjects(specs),
	}
}

func collectSubjects(specs []spec.Spec) []subject {
	seen := map[string]struct{}{}
	var subs []subject
	for _, sp := range specs {
		for _, cmd := range sp.File.Commands {
			for _, tool := range cmd.Tools {
				if tool.Resolver != bufresolver.Name || tool.BufGenPath == "" {
					continue
				}
				key := filepath.ToSlash(sp.Dir) + "\x00" + tool.BufGenPath
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				subs = append(subs, subject{
					SpecDir: filepath.ToSlash(sp.Dir),
					GenPath: tool.BufGenPath,
				})
			}
		}
	}
	sort.Slice(subs, func(i, j int) bool {
		if subs[i].SpecDir != subs[j].SpecDir {
			return subs[i].SpecDir < subs[j].SpecDir
		}
		return subs[i].GenPath < subs[j].GenPath
	})
	return subs
}

// Name implements preflight.Checker.
func (c *Checker) Name() string { return checkerName }

// Check implements preflight.Checker. The specDir parameter is ignored because
// the checker holds its own subject list from spec discovery.
func (c *Checker) Check(_ context.Context, _ string) (preflight.Result, error) {
	var issues []preflight.Issue
	// Each spec dir's buf.yaml/buf.lock pair only needs validating once even
	// when several tasks under the same dir declare distinct buf.gen.yaml
	// files; we dedupe by spec dir so a 4-task spec doesn't emit 4 identical
	// "buf dep update" issues.
	checkedModuleRoots := map[string]struct{}{}

	for _, s := range c.subjects {
		genIssues, err := c.lintBufGen(s)
		if err != nil {
			return preflight.Result{}, err
		}
		issues = append(issues, genIssues...)

		moduleRoot, ok, err := c.findBufModuleRoot(s)
		if err != nil {
			return preflight.Result{}, err
		}
		if !ok {
			continue
		}
		if _, dup := checkedModuleRoots[moduleRoot]; dup {
			continue
		}
		checkedModuleRoots[moduleRoot] = struct{}{}

		lockIssues, err := c.checkBufLock(moduleRoot)
		if err != nil {
			return preflight.Result{}, err
		}
		issues = append(issues, lockIssues...)
	}

	return preflight.Result{OK: len(issues) == 0, Issues: issues}, nil
}

// lintBufGen reads the declared buf.gen.yaml and emits an issue for every
// remote plugin without a pinned tag. Reading the file is treated as a
// preflight error rather than an issue: a missing or malformed buf.gen.yaml
// would also break the resolver, so failing the check loudly surfaces the
// underlying spec problem instead of pretending the tools_hash is fine.
func (c *Checker) lintBufGen(s subject) ([]preflight.Issue, error) {
	full := filepath.Join(c.repoRoot, filepath.FromSlash(s.SpecDir), filepath.FromSlash(s.GenPath))
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path.Join(s.SpecDir, s.GenPath), err)
	}

	var doc bufGenDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path.Join(s.SpecDir, s.GenPath), err)
	}

	var issues []preflight.Issue
	for _, plugin := range doc.Plugins {
		if plugin.Remote == "" || bufresolver.HasPinnedTag(plugin.Remote) {
			continue
		}
		issues = append(issues, preflight.Issue{
			Channel:    checkerName,
			Detail:     fmt.Sprintf("%s: remote plugin %q must specify a pinned :vX.Y.Z tag", path.Join(s.SpecDir, s.GenPath), plugin.Remote),
			Suggestion: "pin the plugin version in buf.gen.yaml (e.g. :v1.35.2)",
		})
	}
	return issues, nil
}

// findBufModuleRoot walks up from the buf.gen.yaml's directory toward the repo
// root looking for a buf.yaml. The buf module root may sit at the spec dir, a
// subdirectory, or higher up; the first ancestor (or the dir itself) holding
// buf.yaml is treated as authoritative. Returns false when no buf.yaml exists
// anywhere on that chain — a legitimate setup if the repo doesn't use BSR
// modules.
//
// Returned path is relative to repoRoot, OS-native.
func (c *Checker) findBufModuleRoot(s subject) (string, bool, error) {
	startRel := filepath.FromSlash(path.Join(s.SpecDir, path.Dir(s.GenPath)))
	dir := startRel
	for {
		candidate := filepath.Join(c.repoRoot, dir, "buf.yaml")
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return dir, true, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}

		parent := filepath.Dir(dir)
		// dir == "." means we reached the repo root; one more iteration would
		// loop forever because filepath.Dir(".") is "." again.
		if parent == dir || dir == "." {
			return "", false, nil
		}
		dir = parent
	}
}

// checkBufLock loads <moduleRoot>/buf.yaml and <moduleRoot>/buf.lock and
// returns issues for every dep declared in buf.yaml that is missing from
// buf.lock. An entirely-missing buf.lock when buf.yaml has at least one dep
// is reported as a single issue (running `buf dep update` from scratch is
// the right fix), rather than emitting one issue per dep.
func (c *Checker) checkBufLock(moduleRoot string) ([]preflight.Issue, error) {
	yamlPath := filepath.Join(c.repoRoot, moduleRoot, "buf.yaml")
	yamlData, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path.Join(filepath.ToSlash(moduleRoot), "buf.yaml"), err)
	}
	var bufYAML bufYAMLDoc
	if err := yaml.Unmarshal(yamlData, &bufYAML); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path.Join(filepath.ToSlash(moduleRoot), "buf.yaml"), err)
	}
	if len(bufYAML.Deps) == 0 {
		return nil, nil
	}

	lockPath := filepath.Join(c.repoRoot, moduleRoot, "buf.lock")
	lockData, err := os.ReadFile(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return []preflight.Issue{{
			Channel:    checkerName,
			Detail:     fmt.Sprintf("%s declares deps but %s is missing", path.Join(filepath.ToSlash(moduleRoot), "buf.yaml"), path.Join(filepath.ToSlash(moduleRoot), "buf.lock")),
			Suggestion: "buf dep update",
		}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path.Join(filepath.ToSlash(moduleRoot), "buf.lock"), err)
	}
	var bufLock bufLockDoc
	if err := yaml.Unmarshal(lockData, &bufLock); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path.Join(filepath.ToSlash(moduleRoot), "buf.lock"), err)
	}
	locked := map[string]struct{}{}
	for _, d := range bufLock.Deps {
		// buf.lock v2 uses `name`; v1 uses `remote/owner/repository`. We treat
		// either as evidence the dep is locked because lazygen does not (yet)
		// support v1 explicitly but should not produce false positives for it.
		if d.Name != "" {
			locked[d.Name] = struct{}{}
		}
	}

	var issues []preflight.Issue
	for _, dep := range bufYAML.Deps {
		base := stripVersion(dep)
		if _, ok := locked[base]; ok {
			continue
		}
		issues = append(issues, preflight.Issue{
			Channel:    checkerName,
			Detail:     fmt.Sprintf("%s deps %q is missing from buf.lock", path.Join(filepath.ToSlash(moduleRoot), "buf.yaml"), dep),
			Suggestion: "buf dep update",
		})
	}
	return issues, nil
}

// stripVersion drops a `:tag` suffix from a buf.yaml dep entry. buf accepts
// both bare module identifiers and pinned ones; the lock keys on bare names,
// so we normalise before comparing.
func stripVersion(dep string) string {
	for i := len(dep) - 1; i >= 0; i-- {
		if dep[i] == ':' {
			return dep[:i]
		}
		if dep[i] == '/' {
			break
		}
	}
	return dep
}

// bufGenDoc / bufYAMLDoc / bufLockDoc are the minimal projections of buf's
// YAML schemas the checker reads. Fields we don't consume are dropped on
// unmarshal so future schema additions parse silently.

type bufGenDoc struct {
	Plugins []bufGenPlugin `yaml:"plugins"`
}

type bufGenPlugin struct {
	Remote string `yaml:"remote"`
}

type bufYAMLDoc struct {
	Deps []string `yaml:"deps"`
}

type bufLockDoc struct {
	Deps []bufLockDep `yaml:"deps"`
}

type bufLockDep struct {
	Name string `yaml:"name"`
}
