// Package spec provides parsers for lazygen.yml task specs and the
// repository-wide tool registry that named tools[] references resolve against
// (ADR-0008).
package spec

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	yaml "github.com/goccy/go-yaml"
)

// File represents one parsed lazygen.yml file. Each file may carry tool
// definitions, command definitions, or both — at least one must be present.
// Tool names declared here are merged into the repo-wide tool registry by
// BuildToolRegistry; commands reference tools by name only.
type File struct {
	Tools    map[string]DeclaredTool `yaml:"tools,omitempty"`
	Commands []Command               `yaml:"commands,omitempty"`
}

// Command corresponds to one entry in commands[]. Tools is a list of tool
// names that must resolve to entries in the repo-wide tool registry.
type Command struct {
	Cmd     CmdLine  `yaml:"cmd"`
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
		// lazygen.yml files that share a generator with their parent.
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

// Parse reads a lazygen.yml document and validates the file-level structural
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
		return errors.New("lazygen.yml must declare at least one of tools[] or commands[]")
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
		// tools is required because lazygen mixes resolved tool contributions into
		// the cache key (ADR-0004 D1). Empty tools means the task has no version
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
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("duplicate task name %q within the same lazygen.yml", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return nil
}

// Spec is one discovered lazygen.yml with its location relative to the discovery root.
type Spec struct {
	// Dir is the spec directory (the directory that contains lazygen.yml), relative to the
	// discovery root and using the OS-native path separator.
	Dir string
	// Path is the lazygen.yml path relative to the discovery root, OS-native separator.
	Path string
	File *File
}

// Discover walks root using the given doublestar pattern (e.g. "**/lazygen.yml") and returns
// each matched spec parsed and validated. The order of results is the doublestar.Glob order.
func Discover(root, pattern string) ([]Spec, error) {
	fsys := os.DirFS(root)
	matches, err := doublestar.Glob(fsys, pattern, doublestar.WithFilesOnly())
	if err != nil {
		return nil, fmt.Errorf("glob %q: %w", pattern, err)
	}
	specs := make([]Spec, 0, len(matches))
	for _, p := range matches {
		osPath := filepath.FromSlash(p)
		b, err := os.ReadFile(filepath.Join(root, osPath))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		f, err := Parse(b)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		specs = append(specs, Spec{
			Dir:  filepath.FromSlash(path.Dir(p)),
			Path: osPath,
			File: f,
		})
	}
	return specs, nil
}
