// Package toolresolver dispatches tool version resolution to per-channel resolvers
// (script for prebuilt binaries, pnpm-external for npm packages, go-local / pnpm-local
// for internal sources, buf for composite plugin commands) and produces the OS-neutral
// logical version strings that feed the cache record's tools_hash component.
package toolresolver

import "context"

// Resolver is implemented by each distribution channel.
//
// Resolve returns one or more ToolVersion entries; for example, the buf resolver expands
// a single `buf generate` into the buf binary plus each plugin's version.
type Resolver interface {
	// Name is the resolver identifier (e.g. "script") that DeclaredTool.Resolver refers to.
	Name() string

	// CanResolve reports whether the resolver wants to handle the given cmd via auto-dispatch.
	// It is consulted only when no DeclaredTool of this resolver is supplied for the task.
	CanResolve(specDir string, cmd []string) bool

	// Resolve returns the OS-neutral ToolVersion entries for the cmd. declared carries the
	// fields supplied in the spec when the resolver was named explicitly; it is nil when
	// called via auto-dispatch.
	Resolve(ctx context.Context, specDir string, cmd []string, declared *DeclaredTool) ([]ToolVersion, error)
}

// ToolVersion is the OS-neutral logical version of a single tool.
type ToolVersion struct {
	Name    string // human-friendly identifier, e.g. "buf"
	Source  string // origin label, e.g. "script:buf"
	Version string // logical version string, e.g. "script:buf@1.30.0"
}

// DeclaredTool mirrors one tools[] entry of a spec. The runner translates spec.DeclaredTool
// into this type before calling Registry.Resolve. Field semantics are resolver-specific:
// the script resolver consumes Exec and Extract; the go-local resolver consumes Entry;
// future resolvers add their own fields.
type DeclaredTool struct {
	Resolver string

	// Exec / Extract are the script resolver inputs.
	Exec    []string
	Extract string

	// Entry is the go-local resolver input: the main package import path
	// (e.g. "./cmd/protoc-gen-foo/...").
	Entry string
}
