// Package toolresolver dispatches `cmd[0]`-driven tool version resolution to per-channel
// resolvers (aqua, go-external, pnpm-external, ...) and produces the OS-neutral logical
// version strings that feed the cache record's tools_hash component.
package toolresolver

import "context"

// Resolver is implemented by each distribution channel.
//
// Resolve returns one or more ToolVersion entries; for example, the buf resolver expands
// a single `buf generate` into the buf binary plus each plugin's version.
type Resolver interface {
	// Name is the resolver identifier used in spec `tools: [{<name>: <key>}]`.
	Name() string

	// CanResolve reports whether the resolver wants to handle the given cmd via auto-dispatch.
	CanResolve(specDir string, cmd []string) bool

	// Resolve returns the OS-neutral ToolVersion entries for the cmd. declaredKey is the
	// value supplied in the spec when the resolver was declared explicitly; it is empty
	// when called via auto-dispatch.
	Resolve(ctx context.Context, specDir string, cmd []string, declaredKey string) ([]ToolVersion, error)
}

// ToolVersion is the OS-neutral logical version of a single tool.
type ToolVersion struct {
	Name    string // human-friendly identifier, e.g. "buf"
	Source  string // origin label, e.g. "aqua.yaml"
	Version string // logical version string, e.g. "aqua:bufbuild/buf@v1.30.0"
}

// DeclaredTool mirrors a single entry of a spec's tools: list. The runner translates
// from spec.DeclaredTool into this type when it calls Registry.Resolve.
type DeclaredTool struct {
	Resolver string // resolver name
	Key      string // resolver-specific identifier
}
