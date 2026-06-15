// Package toolresolver dispatches tool version resolution to per-channel resolvers
// (script for prebuilt binaries — including external npm / Go OSS packages, see
// ADR-0007 — and go-local / pnpm-local for internal sources) and produces the
// OS-neutral logical version strings that feed the fingerprint's resolved_versions_hash
// component, plus the ExtraInputs that feed the runner's depgraph derivation.
package toolresolver

import "context"

// Resolver is implemented by each distribution channel. Each resolver exposes
// two intentionally separate contribution channels for a declared tool, so
// callers that only need one (`sloff graph` consumes Inputs but not Versions;
// `sloff run` consumes both) don't pay for the other:
//
//   - Inputs returns repo-relative file paths the runner folds into the
//     consuming task's input set. The pnpm-local and go-local resolvers use
//     this so workspace / repo-local tool sources land in consumer task
//     inputs, feeding files_hash and the ADR-0013 overlap validation (any
//     task generating those sources must be declared in depends). Resolvers
//     whose channel inherently
//     has no source contribution (script — the version is captured by
//     spawning `<bin> --version`) return nil.
//   - Versions returns OS-neutral logical version entries that feed
//     resolved_versions_hash. Resolvers that only contribute via Inputs (none today, but
//     the interface admits it) return nil.
//
// Splitting the two contributions makes graph-style consumers honest about
// their needs: `sloff graph` only invokes Inputs, so a missing prebuilt
// binary doesn't fail graph rendering even though it would fail `sloff run`.
//
// Implementations are expected to memoise any shared discovery work between
// Inputs and Versions for the same declared tool (lockfile walks,
// packages.Load, etc.) so successive calls don't double-pay (ADR-0008).
//
// Per ADR-0005 sloff runs every resolver through the spec's tools[]
// declarations only; there is no cmd-shape auto-dispatch. declared is
// therefore always non-nil and carries the fields the user wrote.
type Resolver interface {
	// Name is the resolver identifier (e.g. "script") that
	// DeclaredTool.Resolver refers to.
	Name() string

	// Inputs returns the ExtraInputs contribution for one declared tool.
	Inputs(ctx context.Context, specDir string, declared *DeclaredTool) ([]string, error)

	// Versions returns the ResolvedVersion contribution for one declared tool.
	Versions(ctx context.Context, specDir string, declared *DeclaredTool) ([]ResolvedVersion, error)
}

// Prewarmer is an optional interface a Resolver may implement to batch the
// expensive per-tool discovery work (e.g. packages.Load / lockfile walks)
// across every declared tool referencing it, before the runner fans out the
// per-tool Inputs/Versions calls. The runner calls Prewarm once, ahead of
// resolution; resolvers that don't implement it keep paying the original
// per-tool cost.
//
// Prewarm MUST be a pure cache-warming optimisation: a resolver that
// implements it has to return Inputs/Versions identical to what it would
// without Prewarm, so the runner can treat a Prewarm error as non-fatal and
// fall back to the per-tool path (which recomputes — and re-surfaces — the
// same work).
type Prewarmer interface {
	Prewarm(ctx context.Context, reqs []PrewarmRequest) error
}

// PrewarmRequest pairs one declared tool with the spec dir it was defined in,
// mirroring the (specDir, declared) arguments the runner passes to
// Inputs/Versions. The runner builds one per referenced tool.
type PrewarmRequest struct {
	SpecDir  string
	Declared *DeclaredTool
}

// ResolvedVersion is the OS-neutral logical version of a single tool.
type ResolvedVersion struct {
	Name    string // human-friendly identifier, e.g. "buf"
	Source  string // origin label, e.g. "script:buf"
	Version string // logical version string, e.g. "script:buf@1.30.0"
}

// DeclaredTool mirrors one tools[] entry of a spec. The runner translates
// spec.DeclaredTool into this type before calling resolver methods. Field
// semantics are resolver-specific: the script resolver consumes Exec and
// Extract; the go-local resolver consumes Entry; the pnpm-local resolver
// consumes PackageName; future resolvers add their own fields.
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
