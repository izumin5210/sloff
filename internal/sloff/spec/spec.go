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
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	yaml "github.com/goccy/go-yaml"
)

// File represents one parsed sloff.yml file. Each file may carry tool
// definitions, command definitions, or both — at least one must be present.
// Tool names declared here are merged into the repo-wide tool registry by
// BuildToolRegistry; commands reference tools by name only.
type File struct {
	Tools    map[string]DeclaredTool `yaml:"tools,omitempty"`
	Commands []Command               `yaml:"commands,omitempty"`
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
type Command struct {
	Cmd     CmdLine  `yaml:"cmd"`
	Depends []Depend `yaml:"depends,omitempty"`
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
}

type rawDeclaredTool struct {
	Exec      []string `yaml:"exec"`
	Extract   string   `yaml:"extract"`
	GoLocal   string   `yaml:"go-local"`
	PnpmLocal string   `yaml:"pnpm-local"`
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
	if len(f.Tools) == 0 && len(f.Commands) == 0 {
		return errors.New("sloff.yml must declare at least one of tools[] or commands[]")
	}
	if err := validateTools(f.Tools); err != nil {
		return err
	}
	return validateCommands(f.Commands)
}

func validateTools(tools map[string]DeclaredTool) error {
	for name := range tools {
		if !toolNamePattern.MatchString(name) {
			return fmt.Errorf("tools[%q]: name must match %s (lower-case letters, digits, hyphen, underscore)",
				name, toolNamePattern)
		}
	}
	return nil
}

func validateCommands(cmds []Command) error {
	seen := make(map[string]struct{}, len(cmds))
	for i, c := range cmds {
		if c.Name == "" {
			return fmt.Errorf("commands[%d]: name is required", i)
		}
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
			if strings.HasPrefix(d.Spec, "/") {
				return fmt.Errorf("commands[%d] (%s): depends[%d]: spec must be a relative path, got %q", i, c.Name, j, d.Spec)
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

// Discover walks root and returns each file matching pattern (a doublestar
// expression like "**/sloff.yml") parsed and validated. Heavy build / VCS
// directories listed in discoverSkipDirs are pruned without descent.
//
// Ordering is path-ascending and deterministic — fs.WalkDir visits siblings in
// lexical order, so callers no longer depend on doublestar.Glob's traversal
// order to be stable.
func Discover(root, pattern string) ([]Spec, error) {
	if _, err := doublestar.Match(pattern, ""); err != nil {
		// doublestar.Match rejects malformed patterns up-front so we surface the
		// same error the previous Glob path did, instead of silently walking the
		// whole tree and matching nothing.
		return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}

	var specs []Spec
	walkErr := filepath.WalkDir(root, func(osPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if osPath == root {
				return nil
			}
			if _, skip := discoverSkipDirs[d.Name()]; skip {
				return fs.SkipDir
			}
			return nil
		}
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
		specs = append(specs, Spec{
			Dir:  filepath.FromSlash(path.Dir(slashRel)),
			Path: filepath.FromSlash(slashRel),
			File: f,
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return specs, nil
}
