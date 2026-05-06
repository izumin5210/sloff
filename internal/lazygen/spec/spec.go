// Package spec provides parsers for lazygen.yml task specs.
package spec

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	yaml "github.com/goccy/go-yaml"
)

// File represents one parsed lazygen.yml file.
type File struct {
	Commands []Command `yaml:"commands"`
}

// Command corresponds to one entry in commands[].
type Command struct {
	Cmd     CmdLine        `yaml:"cmd"`
	Inputs  []string       `yaml:"inputs"`
	Name    string         `yaml:"name"`
	Outputs []string       `yaml:"outputs"`
	Tools   []DeclaredTool `yaml:"tools,omitempty"`
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

// DeclaredTool is one entry of a command's tools: list. The resolver is determined by
// which fields are present in the YAML; for the script resolver an `exec` field is
// required and `extract` is optional, and for the go-local resolver a `go-local` field
// names the main package import path.
//
// Example:
//
//	tools:
//	  - exec: ["buf", "--version"]
//	  - exec: ["go", "version"]
//	    extract: 'go[0-9]+\.[0-9]+(?:\.[0-9]+)?'
//	  - go-local: ./cmd/protoc-gen-foo/...
type DeclaredTool struct {
	// Resolver is the resolver name inferred from the YAML shape, e.g. "script".
	Resolver string

	// Exec / Extract are the script resolver inputs.
	Exec    []string
	Extract string

	// Entry is the go-local resolver input: the main package import path
	// (must begin with "./").
	Entry string
}

type rawDeclaredTool struct {
	Exec    []string `yaml:"exec"`
	Extract string   `yaml:"extract"`
	GoLocal string   `yaml:"go-local"`
}

// UnmarshalYAML implements goccy/go-yaml's BytesUnmarshaler.
func (d *DeclaredTool) UnmarshalYAML(b []byte) error {
	var raw rawDeclaredTool
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("tools entry: %w", err)
	}
	hasExec := len(raw.Exec) > 0
	hasGoLocal := raw.GoLocal != ""
	if hasExec && hasGoLocal {
		return errors.New("tools entry: exec and go-local are mutually exclusive")
	}
	switch {
	case hasExec:
		d.Resolver = "script"
		d.Exec = raw.Exec
		d.Extract = raw.Extract
		return nil
	case hasGoLocal:
		// `./` prefix or a bare "." is required: this disambiguates a relative
		// repo path from a Go module import path and matches the forms expected
		// by `go run` / `go/packages` (e.g. `go run .` for a generator whose
		// main package is the spec directory itself).
		if raw.GoLocal != "." && !strings.HasPrefix(raw.GoLocal, "./") {
			return fmt.Errorf("tools entry: go-local must be %q or start with %q, got %q", ".", "./", raw.GoLocal)
		}
		d.Resolver = "go-local"
		d.Entry = raw.GoLocal
		return nil
	default:
		return errors.New("tools entry: required field is missing (supported: exec [+ extract], go-local)")
	}
}

// Parse reads a lazygen.yml document and validates the required fields.
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
	if len(f.Commands) == 0 {
		return errors.New("at least one command is required")
	}
	seen := make(map[string]struct{}, len(f.Commands))
	for i, c := range f.Commands {
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
		if len(c.Tools) == 0 {
			// tools is required because lazygen mixes the resolved tool versions into the
			// cache key. Without it, upgrading a generator binary cannot invalidate the
			// cache and stale outputs would be served indefinitely.
			return fmt.Errorf("commands[%d] (%s): tools is required", i, c.Name)
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
