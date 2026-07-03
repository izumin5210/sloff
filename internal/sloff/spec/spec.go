// Package spec provides parsers for sloff.yml task specs and the
// repository-wide tool registry that named tools[] references resolve against
// (ADR-0008).
package spec

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	yaml "github.com/goccy/go-yaml"
	"golang.org/x/sync/errgroup"

	"github.com/izumin5210/sloff/internal/sloff/glob"
)

// File represents one parsed sloff.yml file. Each file may carry tool
// definitions, command definitions, command providers, or any mix — at least
// one must be present. Tool names declared here are merged into the repo-wide
// tool registry by BuildToolRegistry; commands reference tools by name only.
// CommandProviders are programs the runner execs at plan time to emit
// additional commands dynamically (ADR-0015); they are resolved before
// collectTasks, after which their output is indistinguishable from a
// hand-written command.
type File struct {
	Tools            map[string]DeclaredTool `yaml:"tools,omitempty"`
	Commands         []Command               `yaml:"commands,omitempty"`
	CommandProviders []CommandProviderDecl   `yaml:"command_providers,omitempty"`
}

// CommandProviderDecl is one entry of the file-level command_providers: list
// (ADR-0015). Exec is run at plan time with cwd set to the declaring spec dir;
// its stdout is a versioned JSON envelope of task definitions that the runner
// folds into the command set. Name is an identifier used in diagnostics and
// must be unique within the file.
type CommandProviderDecl struct {
	Name string   `yaml:"name"`
	Exec []string `yaml:"exec"`
}

// Depend is one entry of commands[*].depends — a reference to another task
// that must complete before this command runs (ADR-0013). Spec is the
// dependency's spec dir relative to the sloff.yml that declares the
// reference; empty means the same file. Depends affects scheduling only and
// never feeds the fingerprint input_hash (ADR-0013 D4).
type Depend struct {
	Spec string `yaml:"spec,omitempty"`
	Task string `yaml:"task"`
}

// Command corresponds to one entry in commands[]. Tools is a list of tool
// names that must resolve to entries in the repo-wide tool registry.
//
// Barrier marks a pure aggregation node (ADR-0017): a task that carries only
// depends and exists so a set of tasks can be referenced under one name
// (a barrier / alias, like Ninja's phony). Barrier tasks must not declare
// cmd / inputs / outputs / tools and are never executed or fingerprinted.
type Command struct {
	Cmd     CmdLine  `yaml:"cmd"`
	Depends []Depend `yaml:"depends,omitempty"`
	Barrier bool     `yaml:"barrier,omitempty"`
	Inputs  []string `yaml:"inputs"`
	Name    string   `yaml:"name"`
	Outputs []string `yaml:"outputs"`
	Tools   []string `yaml:"tools,omitempty"`
}

// CmdLine is a command-line that may be either a YAML scalar (whitespace-split into args)
// or a sequence of strings (taken as-is, preserving spaces inside arguments).
type CmdLine []string

// UnmarshalYAML implements goccy/go-yaml's BytesUnmarshaler.
func (c *CmdLine) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err == nil {
		*c = strings.Fields(s)
		return nil
	}
	var list []string
	if err := yaml.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("cmd must be a string or a list of strings: %w", err)
	}
	*c = list
	return nil
}

// DeclaredTool is one entry of the file-level tools: map. The resolver is
// determined by which fields are present in the YAML; the three forms are
// mutually exclusive.
//
// Example:
//
//	tools:
//	  buf:
//	    exec: ["buf", "--version"]
//	  go-version:
//	    exec: ["go", "version"]
//	    extract: 'go[0-9]+\.[0-9]+(?:\.[0-9]+)?'
//	  protoc-gen-foo:
//	    go-local: ./cmd/protoc-gen-foo/...
//	  codegen:
//	    pnpm-local: '@org/my-codegen'
type DeclaredTool struct {
	// Resolver is the resolver name inferred from the YAML shape, e.g. "script".
	Resolver string

	// Exec / Extract are the script resolver inputs.
	Exec    []string
	Extract string

	// Entry is the go-local resolver input: the main package import path
	// (must begin with "./").
	Entry string

	// PackageName is the pnpm-local resolver input: a workspace package name
	// (e.g. "@org/my-codegen") that pnpm-lock.yaml registers as an importer.
	PackageName string

	// Depends declares the tool's bootstrap dependencies (ADR-0019 D1): tasks
	// that must complete before the tool's sources can be resolved (e.g. a
	// codegen task producing files the tool imports). Spec paths are relative
	// to the sloff.yml that defines the tool (ADR-0008 D3). The runner injects
	// these as depends edges into every task referencing the tool; on a
	// resolution failure a tool with a non-empty Depends is deferred instead
	// of failing the run. Declarable on any resolver form.
	Depends []Depend
}

type rawDeclaredTool struct {
	Exec      []string `yaml:"exec"`
	Extract   string   `yaml:"extract"`
	GoLocal   string   `yaml:"go-local"`
	PnpmLocal string   `yaml:"pnpm-local"`
	Depends   []Depend `yaml:"depends"`
}

// UnmarshalYAML implements goccy/go-yaml's BytesUnmarshaler.
func (d *DeclaredTool) UnmarshalYAML(b []byte) error {
	var raw rawDeclaredTool
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("tool entry: %w", err)
	}
	hasExec := len(raw.Exec) > 0
	hasGoLocal := raw.GoLocal != ""
	hasPnpmLocal := raw.PnpmLocal != ""
	if moreThanOneTrue(hasExec, hasGoLocal, hasPnpmLocal) {
		return errors.New("tool entry: exec, go-local, and pnpm-local are mutually exclusive")
	}
	// depends is orthogonal to the resolver shape: any form may declare
	// bootstrap dependencies (ADR-0019 D1).
	d.Depends = raw.Depends
	switch {
	case hasExec:
		d.Resolver = "script"
		d.Exec = raw.Exec
		d.Extract = raw.Extract
		return nil
	case hasGoLocal:
		// Spec-relative entries must start with `./` or `../` (or be a bare
		// `.` / `..`): these forms disambiguate a relative repo path from a
		// Go module import path and match what `go run` / `go/packages`
		// accept. Parent-relative paths matter for tools defined in nested
		// sloff.yml files that share a generator with their parent.
		if !isRelativeEntry(raw.GoLocal) {
			return fmt.Errorf("tool entry: go-local must start with %q or %q (or be %q / %q), got %q",
				"./", "../", ".", "..", raw.GoLocal)
		}
		d.Resolver = "go-local"
		d.Entry = raw.GoLocal
		return nil
	case hasPnpmLocal:
		d.Resolver = "pnpm-local"
		d.PackageName = raw.PnpmLocal
		return nil
	default:
		return errors.New("tool entry: required field is missing (supported: exec [+ extract], go-local, pnpm-local)")
	}
}

func moreThanOneTrue(bs ...bool) bool {
	count := 0
	for _, b := range bs {
		if b {
			count++
			if count > 1 {
				return true
			}
		}
	}
	return false
}

// isRelativeEntry reports whether s is in the spec-relative entry form
// accepted by `go run` / `go/packages`: bare "." / "..", or starting with
// "./" / "../".
func isRelativeEntry(s string) bool {
	return s == "." || s == ".." ||
		strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

// toolNamePattern is the slug-style regex tool names must match per ADR-0008
// D4: lower-case letters, digits, hyphens and underscores only. Names like
// "go-local" and "pnpm-local" themselves match this so spec authors can reuse
// resolver names as tool names if they want.
var toolNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Parse reads a sloff.yml document and validates the file-level structural
// rules. Cross-file validation (tool name uniqueness across the repo, task
// references resolving to defined tools) lives in BuildToolRegistry /
// ValidateToolReferences and is run on the full Spec set after Discover.
func Parse(b []byte) (*File, error) {
	f := &File{}
	if err := yaml.Unmarshal(b, f); err != nil {
		return nil, err
	}
	if err := validate(f); err != nil {
		return nil, err
	}
	return f, nil
}

func validate(f *File) error {
	if len(f.Tools) == 0 && len(f.Commands) == 0 && len(f.CommandProviders) == 0 {
		return errors.New("sloff.yml must declare at least one of tools[], commands[], or command_providers[]")
	}
	if err := validateTools(f.Tools); err != nil {
		return err
	}
	if err := validateCommandProviders(f.CommandProviders); err != nil {
		return err
	}
	return ValidateCommands(f.Commands)
}

// validateCommandProviders checks the file-level command_providers[] entries
// (ADR-0015 D1): each needs a name and an exec, and names are unique within the
// file. The commands a provider emits are validated separately, after the
// runner execs the provider and merges the output into the command set.
func validateCommandProviders(providers []CommandProviderDecl) error {
	seen := make(map[string]struct{}, len(providers))
	for i, p := range providers {
		if p.Name == "" {
			return fmt.Errorf("command_providers[%d]: name is required", i)
		}
		if len(p.Exec) == 0 {
			return fmt.Errorf("command_providers[%d] (%s): exec is required", i, p.Name)
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("duplicate command provider name %q within the same sloff.yml", p.Name)
		}
		seen[p.Name] = struct{}{}
	}
	return nil
}

func validateTools(tools map[string]DeclaredTool) error {
	// Sorted iteration so the reported error is deterministic when several
	// tools are invalid (map order would otherwise pick one at random).
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !toolNamePattern.MatchString(name) {
			return fmt.Errorf("tools[%q]: name must match %s (lower-case letters, digits, hyphen, underscore)",
				name, toolNamePattern)
		}
		if err := validateToolDepends(name, tools[name].Depends); err != nil {
			return err
		}
	}
	return nil
}

// validateToolDepends enforces the load-time rules on one tool's bootstrap
// depends (ADR-0019 D1): task required, spec relative, no glob patterns
// (unsupported in v1 — a tool's closure producers are a concrete task set),
// no duplicate entries. Existence of the referenced task is deliberately NOT
// checked here: it is validated at injection time so provider-generated
// tasks can be referenced, and unreferenced catalog tools stay unvalidated
// like their resolver config (ADR-0008).
func validateToolDepends(toolName string, depends []Depend) error {
	type key struct{ spec, task string }
	seen := make(map[key]struct{}, len(depends))
	for i, d := range depends {
		if d.Task == "" {
			return fmt.Errorf("tools[%q]: depends[%d]: task is required", toolName, i)
		}
		if filepath.IsAbs(d.Spec) {
			return fmt.Errorf("tools[%q]: depends[%d]: spec must be a relative path, got %q", toolName, i, d.Spec)
		}
		if IsDependPattern(d.Task) {
			return fmt.Errorf("tools[%q]: depends[%d]: glob patterns are not supported in tool depends, got %q", toolName, i, d.Task)
		}
		// path.Clean folds the "" vs "." spelling of "same dir" so the two
		// forms count as the same entry.
		k := key{path.Clean(d.Spec), d.Task}
		if _, dup := seen[k]; dup {
			return fmt.Errorf("tools[%q]: depends[%d]: duplicate depends entry %s:%s", toolName, i, k.spec, k.task)
		}
		seen[k] = struct{}{}
	}
	return nil
}

// ValidateCommands enforces the per-command structural rules — required name /
// cmd / inputs / outputs / tools, non-empty tool and depends references, and
// name uniqueness within the set. Parse calls it on a file's static commands;
// the runner re-runs it on the merged static+generated command set after
// expanding command_providers (ADR-0015 D5) so dynamically emitted tasks face
// the same validation as hand-written ones.
//
// Barrier commands (ADR-0017 D1) invert the required-field rules: they must
// carry only depends. Forbidding the work-carrying fields structurally
// enforces "a barrier is an aggregation point, not a task with work", and an
// empty depends is rejected because a barrier that aggregates nothing is a
// spec mistake.
func ValidateCommands(cmds []Command) error {
	seen := make(map[string]struct{}, len(cmds))
	for i, c := range cmds {
		if c.Name == "" {
			return fmt.Errorf("commands[%d]: name is required", i)
		}
		// Task names share the tool-name slug rule (ADR-0008 D4). Enforcing it here
		// is what lets ADR-0016 treat any depends value carrying a glob
		// metacharacter as an unambiguous pattern: a task can never be named "gen-*",
		// so such a reference is always a pattern, never a literal target.
		if !toolNamePattern.MatchString(c.Name) {
			return fmt.Errorf("commands[%d] (%s): name must match %s (lower-case letters, digits, hyphen, underscore)",
				i, c.Name, toolNamePattern)
		}
		if c.Barrier {
			if len(c.Cmd) > 0 || len(c.Inputs) > 0 || len(c.Outputs) > 0 || len(c.Tools) > 0 {
				return fmt.Errorf("commands[%d] (%s): barrier tasks must not declare cmd, inputs, outputs, or tools", i, c.Name)
			}
			if len(c.Depends) == 0 {
				return fmt.Errorf("commands[%d] (%s): barrier tasks must declare at least one depends entry", i, c.Name)
			}
		} else {
			if len(c.Cmd) == 0 {
				return fmt.Errorf("commands[%d] (%s): cmd is required", i, c.Name)
			}
			if len(c.Inputs) == 0 {
				return fmt.Errorf("commands[%d] (%s): inputs is required", i, c.Name)
			}
			if len(c.Outputs) == 0 {
				return fmt.Errorf("commands[%d] (%s): outputs is required", i, c.Name)
			}
			// tools is required because sloff mixes resolved tool contributions into
			// the fingerprint key (ADR-0004 D1). Empty tools means the task has no version
			// signal at all and stale outputs could be served indefinitely.
			if len(c.Tools) == 0 {
				return fmt.Errorf("commands[%d] (%s): tools is required", i, c.Name)
			}
		}
		// Tool-name references are validated against the repo-wide registry by
		// ValidateToolReferences after Discover; here we just guard against empty
		// strings sneaking in.
		for j, name := range c.Tools {
			if name == "" {
				return fmt.Errorf("commands[%d] (%s): tools[%d] is empty", i, c.Name, j)
			}
		}
		for j, d := range c.Depends {
			if d.Task == "" {
				return fmt.Errorf("commands[%d] (%s): depends[%d]: task is required", i, c.Name, j)
			}
			if filepath.IsAbs(d.Spec) {
				return fmt.Errorf("commands[%d] (%s): depends[%d]: spec must be a relative path, got %q", i, c.Name, j, d.Spec)
			}
			// A glob pattern (ADR-0016 D1) is expanded to literal edges later;
			// reject a malformed glob at load time so the error points at the
			// declaring file rather than surfacing mid-run.
			if IsDependPattern(d.Task) && !doublestar.ValidatePattern(d.Task) {
				return fmt.Errorf("commands[%d] (%s): depends[%d]: invalid glob pattern %q", i, c.Name, j, d.Task)
			}
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("duplicate task name %q within the same sloff.yml", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return nil
}

// Spec is one discovered sloff.yml with its location relative to the discovery root.
type Spec struct {
	// Dir is the spec directory (the directory that contains sloff.yml), relative to the
	// discovery root and using the OS-native path separator.
	Dir string
	// Path is the sloff.yml path relative to the discovery root, OS-native separator.
	Path string
	File *File
}

// discoverSkipDirs are directory names Discover refuses to descend into. These
// are not (and never have been) places sloff specs live, but they balloon the
// `**/sloff.yml` walk catastrophically:
//
//   - `node_modules` in a polyglot monorepo can carry hundreds of thousands of
//     files and dwarf the rest of the repo by orders of magnitude. Walking it
//     took ~5 minutes wall in observed cases.
//   - `.git` is always present and stat-heavy; nothing there matches a YAML spec.
//
// Same skip discipline Turborepo / pnpm / Nx apply for the same reason.
// Respecting full `.gitignore` would cover more pathological repos but is left
// as a follow-up: the two entries below recover essentially all of the wall in
// practice without forcing every consumer to commit a .gitignore.
var discoverSkipDirs = map[string]struct{}{
	"node_modules": {},
	".git":         {},
}

// ValidateDependReferences checks every commands[*].depends entry across the
// discovered spec set (ADR-0013 D1): the referenced spec dir must stay inside
// the repo, the referenced (spec dir, task) must exist, self-references are
// rejected, and the same edge declared twice in one command is rejected.
// Like ValidateToolReferences, this is a cross-file pass run on the full set
// after Discover; per-file structural checks live in validate.
func ValidateDependReferences(specs []Spec) error {
	type taskKey struct{ dir, name string }
	defined := map[taskKey]struct{}{}
	for _, sp := range specs {
		dir := filepath.ToSlash(sp.Dir)
		for _, c := range sp.File.Commands {
			defined[taskKey{dir, c.Name}] = struct{}{}
		}
	}
	for _, sp := range specs {
		dir := filepath.ToSlash(sp.Dir)
		for _, c := range sp.File.Commands {
			seen := map[taskKey]struct{}{}
			for i, d := range c.Depends {
				// path.Join cleans, so "../options" resolves against the
				// declaring file's dir the same way inputs/outputs globs do.
				target := path.Join(dir, d.Spec)
				if glob.EscapesRoot(target) {
					return fmt.Errorf("%s/%s: depends[%d]: spec %q escapes repo root", registryDefinitionPath(sp.Dir), c.Name, i, d.Spec)
				}
				key := taskKey{target, d.Task}
				if target == dir && d.Task == c.Name {
					return fmt.Errorf("%s/%s: depends[%d]: task depends on itself", registryDefinitionPath(sp.Dir), c.Name, i)
				}
				if _, ok := defined[key]; !ok {
					return fmt.Errorf("%s/%s: depends[%d]: task %q not found in spec dir %q", registryDefinitionPath(sp.Dir), c.Name, i, d.Task, target)
				}
				if _, dup := seen[key]; dup {
					return fmt.Errorf("%s/%s: depends[%d]: duplicate depends entry %s:%s", registryDefinitionPath(sp.Dir), c.Name, i, target, d.Task)
				}
				seen[key] = struct{}{}
			}
		}
	}
	return nil
}

// Discover walks root and returns each file matching pattern (a doublestar
// expression like "**/sloff.yml") parsed and validated. Heavy build / VCS
// directories listed in discoverSkipDirs are pruned without descent.
//
// Top-level children of root are walked concurrently: a polyglot monorepo fans
// out into a few large, ReadDir-bound subtrees (go/, web/, …) whose walks
// otherwise run back-to-back on a single goroutine. The assembled result is
// sorted by path so ordering stays path-ascending and deterministic regardless
// of goroutine scheduling.
func Discover(root, pattern string) ([]Spec, error) {
	if _, err := doublestar.Match(pattern, ""); err != nil {
		// doublestar.Match rejects malformed patterns up-front so we surface the
		// same error the previous Glob path did, instead of silently walking the
		// whole tree and matching nothing.
		return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}

	rootEntries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", root, err)
	}

	var (
		mu    sync.Mutex
		specs []Spec
	)
	collect := func(s Spec) {
		mu.Lock()
		specs = append(specs, s)
		mu.Unlock()
	}

	g := new(errgroup.Group)
	g.SetLimit(max(runtime.GOMAXPROCS(0), 1))
	for _, e := range rootEntries {
		name := e.Name()
		if e.IsDir() {
			if _, skip := discoverSkipDirs[name]; skip {
				continue
			}
			g.Go(func() error { return walkSpecs(root, name, pattern, collect) })
		} else {
			g.Go(func() error { return matchSpecPath(root, filepath.Join(root, name), pattern, collect) })
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	sort.Slice(specs, func(i, j int) bool { return specs[i].Path < specs[j].Path })
	return specs, nil
}

// walkSpecs walks the subtree rooted at root/dir and collects every file
// matching pattern, pruning discoverSkipDirs without descent. It is the
// per-top-level-child body Discover fans out across goroutines.
func walkSpecs(root, dir, pattern string, collect func(Spec)) error {
	return filepath.WalkDir(filepath.Join(root, dir), func(osPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := discoverSkipDirs[d.Name()]; skip {
				return fs.SkipDir
			}
			return nil
		}
		return matchSpecPath(root, osPath, pattern, collect)
	})
}

// matchSpecPath matches one file (absolute osPath) against pattern and, on a
// hit, parses and collects it as a Spec keyed by its repo-relative slash path.
// Shared by the subtree walk and the root-level file check; the repo-relative
// path is computed against root so the slash-form key is identical to the old
// single-walk output.
func matchSpecPath(root, osPath, pattern string, collect func(Spec)) error {
	rel, err := filepath.Rel(root, osPath)
	if err != nil {
		return err
	}
	slashRel := filepath.ToSlash(rel)
	ok, err := doublestar.Match(pattern, slashRel)
	if err != nil {
		return fmt.Errorf("match %q against %q: %w", pattern, slashRel, err)
	}
	if !ok {
		return nil
	}
	b, readErr := os.ReadFile(osPath)
	if readErr != nil {
		return fmt.Errorf("read %s: %w", slashRel, readErr)
	}
	f, parseErr := Parse(b)
	if parseErr != nil {
		return fmt.Errorf("parse %s: %w", slashRel, parseErr)
	}
	collect(Spec{
		Dir:  filepath.FromSlash(path.Dir(slashRel)),
		Path: filepath.FromSlash(slashRel),
		File: f,
	})
	return nil
}
