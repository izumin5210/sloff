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

// DeclaredTool is one entry of a command's tools: list. The YAML form is a single-key map,
// e.g. `{aqua: bufbuild/buf}` or `{go-external: google.golang.org/protobuf/cmd/protoc-gen-go}`.
type DeclaredTool struct {
	Key      string // resolver-specific identifier, e.g. "bufbuild/buf"
	Resolver string // resolver name, e.g. "aqua"
}

// UnmarshalYAML implements goccy/go-yaml's BytesUnmarshaler.
func (d *DeclaredTool) UnmarshalYAML(b []byte) error {
	var m map[string]string
	if err := yaml.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("tools entry must be a single-key map: %w", err)
	}
	if len(m) != 1 {
		return fmt.Errorf("tools entry must have exactly one key, got %d", len(m))
	}
	for k, v := range m {
		d.Resolver = k
		d.Key = v
	}
	return nil
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
