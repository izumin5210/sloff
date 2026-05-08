# sloff 🦥

> A cache-aware codegen orchestrator for polyglot monorepos. Skips work when inputs haven't changed — and never lies about it.

`sloff` runs your code generators (proto / SQL / mock / GraphQL / etc.) and caches their outputs in git so devs and CI skip re-generation when nothing has changed. Cache hits are validated by **comparing both inputs *and* the actual output files** against the recorded state — so even when the cache record looks valid, a drifted output triggers re-generation rather than a stale skip.

## Features

- **Output-comparison cache hits.** A hit requires the recorded `input_hash` *and* the on-disk output files to match. Drifted outputs (manual edits, formatter runs, partial checkouts) are re-generated, never silently skipped.
- **OS-portable cache.** Cache records are deterministic YAML committed to git. A record built on macOS works on Linux CI without rebuilds — tool versions are captured as logical strings, never as OS-specific binary hashes.
- **Reads your existing toolchain.** No replacement for aqua / mise / nix / pnpm / `go.mod`. Tool versions come from the runtime binary's `--version`, lockfiles, or repo source — whichever is the actual source of truth.
- **Auto-derived dependencies.** Task ordering is computed from `inputs` / `outputs` glob intersections. There is no manual `depends:` field to keep in sync.
- **Single Go binary.** No runtime dependencies, no daemon, no language ecosystem to install.
- **Codegen-only by design.** Build / test / lint stay in your existing tooling (Make, npm scripts, etc.) — sloff does one thing.

## Install

```sh
go install github.com/izumin5210/sloff/cmd/sloff@latest
```

## Quick start

Place a `sloff.yml` in any directory containing your codegen inputs:

```yaml
tools:
  buf:
    exec: ["buf", "--version"]
  protoc-gen-go:
    exec: ["protoc-gen-go", "--version"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: protoc-gen-go
    cmd: buf generate
    inputs: ["**/*.proto", "buf.gen.yaml", "buf.yaml", "buf.lock"]
    outputs: ["**/*.pb.go", "**/*.connect.go"]
    tools: [buf, protoc-gen-go]
```

Then from anywhere in the repo:

```sh
sloff run
```

`sloff run` discovers every `sloff.yml`, builds a DAG from `inputs` / `outputs` overlap, and either skips or re-runs each task based on cache lookup.

## Tool resolvers

`tools:` entries dispatch to one of three resolvers based on shape:

### `script` — for prebuilt binaries

For anything with a `--version` command: aqua / mise / nix-distributed CLIs, `go tool`-managed bins, npm bins via `pnpm exec`, etc. The runtime binary's stdout is the version source of truth.

```yaml
tools:
  buf:
    exec: ["buf", "--version"]
  protoc-gen-go:
    exec: ["protoc-gen-go", "--version"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
```

### `go-local` — for repo-internal Go CLIs

For codegen tools you maintain in this repo as Go `cmd/...` packages, run via `go run`:

```yaml
tools:
  protoc-gen-foo:
    go-local: ./cmd/protoc-gen-foo
```

Internal `.go` source files contribute to the task's effective `inputs` (so source edits invalidate); external Go module versions come from `go.sum`.

### `pnpm-local` — for pnpm workspace internal packages

For codegen tools you maintain as pnpm workspace packages:

```yaml
tools:
  codegen:
    pnpm-local: "@org/codegen"

commands:
  - name: gen
    cmd: ["sh", "-c", "pnpm --filter @org/codegen build && pnpm exec my-codegen"]
    inputs: ["**/*.proto"]
    outputs: ["**/*.pb.ts"]
    tools: [codegen]
```

git-tracked files in the workspace package (and transitive workspace deps) contribute to `inputs`; external npm dep versions come from `pnpm-lock.yaml`. Build steps stay in the task `cmd`.

## When to use sloff (vs alternatives)

sloff is well-suited when:

- You have a **polyglot codegen pipeline** (Go + JS/TS + external prebuilt binaries) and want shared cache across devs and CI.
- You want to **keep your existing toolchain** (aqua / mise / nix / pnpm / go.mod) instead of migrating to a new build system.
- You're OK with **outputs being committed to git** (no separate artifact cache infrastructure).

You probably want a different tool when:

| Need | Better fit |
|---|---|
| Cache compile artifacts and binaries (not just codegen outputs) | [Bazel](https://bazel.build/) / [Buck2](https://buck2.build/) |
| Hermetic build with full toolchain isolation | [Bazel](https://bazel.build/) / [Buck2](https://buck2.build/) |
| General task runner (build / test / lint / dev server / formatter) | [moonrepo](https://moonrepo.dev/) / [Nx](https://nx.dev/) |
| JS/TS-only monorepo | [Turborepo](https://turbo.build/) / [Nx](https://nx.dev/) |
| File-grained dependency inference | [Pants](https://www.pantsbuild.org/) |
| Battle-tested at massive scale | Any of the above |

sloff is intentionally narrow — **codegen orchestration with an honest cache, nothing else**.

## Documentation

- [Architecture](./docs/design/architecture.md) — overall design
- [Resolver: script](./docs/design/resolver-script.md)
- [Resolver: go-local](./docs/design/resolver-go-local.md)
- [Resolver: pnpm-local](./docs/design/resolver-pnpm-local.md)
- [ADRs](./docs/adr/) — design decision records (in Japanese)

## License

[MIT](./LICENSE)
