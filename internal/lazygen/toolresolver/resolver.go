// Package toolresolver dispatches tool version resolution to per-channel resolvers
// (script for prebuilt binaries — including external npm / Go OSS packages, see
// ADR-0007 — and go-local / pnpm-local for internal sources) and produces the
// OS-neutral logical version strings that feed the cache record's tools_hash
// component.
package toolresolver

import "context"

// Resolver is implemented by each distribution channel.
//
// Resolve returns one or more ToolVersion entries; for example, the buf resolver expands
// a single `buf generate` into the buf binary plus each plugin's version.
//
// Per ADR-0005 lazygen runs every resolver through the spec's tools[] declarations only;
// there is no cmd-shape auto-dispatch. declared is therefore always non-nil and carries
// the fields the user wrote.
type Resolver interface {
	// Name is the resolver identifier (e.g. "script") that DeclaredTool.Resolver refers to.
	Name() string

	// Resolve returns the OS-neutral ToolVersion entries for the cmd, using the
	// resolver-specific fields supplied via declared.
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
// the pnpm-local resolver consumes PackageName; future resolvers add their own fields.
type DeclaredTool struct {
	Resolver string

	// Exec / Extract are the script resolver inputs.
	Exec    []string
	Extract string

	// Entry is the go-local resolver input: the main package import path
	// (e.g. "./cmd/protoc-gen-foo/...").
	Entry string

	// PackageName is the pnpm-local resolver input: a workspace package name
	// declared in the matching pnpm-lock.yaml importer's package.json
	// (e.g. "@org/my-codegen"). External npm packages are out of scope —
	// per ADR-0007 they belong to the script resolver.
	PackageName string
}
