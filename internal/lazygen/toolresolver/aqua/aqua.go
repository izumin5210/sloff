// Package aqua implements toolresolver.Resolver for tools distributed via the aqua
// package manager (https://aquaproj.github.io/). The resolver returns OS-neutral logical
// version strings of the form "aqua:<owner>/<name>@<version>".
package aqua

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
)

// Name is the resolver identifier referenced by spec `tools: [{aqua: <name>}]`.
const Name = "aqua"

// Resolver resolves tool versions for binaries declared in aqua.yaml.
type Resolver struct {
	cfg *Config

	byBinary map[string]Package // last path segment of name → Package
	byName   map[string]Package // full name (e.g. "bufbuild/buf") → Package
}

// New loads aqua.yaml from repoRoot and returns a Resolver.
func New(repoRoot string) (*Resolver, error) {
	cfg, err := LoadConfig(repoRoot)
	if err != nil {
		return nil, err
	}
	return NewFromConfig(cfg), nil
}

// NewFromConfig returns a Resolver backed by the given Config. Useful for tests.
func NewFromConfig(cfg *Config) *Resolver {
	r := &Resolver{
		cfg:      cfg,
		byBinary: map[string]Package{},
		byName:   map[string]Package{},
	}
	for _, p := range cfg.Packages {
		r.byBinary[binaryName(p.Name)] = p
		r.byName[p.Name] = p
	}
	return r
}

// Name implements toolresolver.Resolver.
func (r *Resolver) Name() string { return Name }

// CanResolve reports whether cmd[0]'s base name matches the binary name of one of the
// declared aqua packages. The mapping uses the last "/"-separated segment of the
// package name; aqua-registry overrides for binary names that differ from the package
// name are out of scope for the MVP.
func (r *Resolver) CanResolve(_ string, cmd []string) bool {
	if len(cmd) == 0 {
		return false
	}
	_, ok := r.byBinary[filepath.Base(cmd[0])]
	return ok
}

// Resolve returns the ToolVersion for the matching aqua package. When declaredKey is
// non-empty it is interpreted as the package name (e.g. "bufbuild/buf"); otherwise the
// auto-dispatch path is used and cmd[0]'s base name is looked up.
func (r *Resolver) Resolve(_ context.Context, _ string, cmd []string, declaredKey string) ([]toolresolver.ToolVersion, error) {
	pkg, ok := r.lookup(cmd, declaredKey)
	if !ok {
		if declaredKey != "" {
			return nil, fmt.Errorf("aqua: package %q is not declared in aqua.yaml", declaredKey)
		}
		return nil, fmt.Errorf("aqua: no package matches binary %q", baseOrEmpty(cmd))
	}
	return []toolresolver.ToolVersion{{
		Name:    pkg.Name,
		Source:  ConfigFileName,
		Version: fmt.Sprintf("aqua:%s@%s", pkg.Name, pkg.Version),
	}}, nil
}

func (r *Resolver) lookup(cmd []string, declaredKey string) (Package, bool) {
	if declaredKey != "" {
		p, ok := r.byName[declaredKey]
		return p, ok
	}
	if len(cmd) == 0 {
		return Package{}, false
	}
	p, ok := r.byBinary[filepath.Base(cmd[0])]
	return p, ok
}

func binaryName(pkgName string) string {
	return filepath.Base(pkgName)
}

func baseOrEmpty(cmd []string) string {
	if len(cmd) == 0 {
		return ""
	}
	return filepath.Base(cmd[0])
}
