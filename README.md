# sloff 🦥

> A fingerprint-aware codegen orchestrator for polyglot monorepos. Skips work when inputs haven't changed — and never lies about it.

`sloff` runs your code generators (proto / SQL / mock / GraphQL / etc.) and fingerprints their outputs in git so devs and CI skip re-generation when nothing has changed. Fingerprint hits are validated by **comparing both inputs *and* the actual output files** against the recorded state — so even when the fingerprint looks valid, a drifted output triggers re-generation rather than a stale skip.

## Features

- **Output-comparison fingerprint hits.** A hit requires the recorded `input_hash` *and* the on-disk output files to match. Drifted outputs (manual edits, formatter runs, partial checkouts) are re-generated, never silently skipped.
- **OS-portable fingerprints.** Fingerprints are deterministic protobuf binary records committed to git (inspect them with `sloff fingerprint show`). A record built on macOS works on Linux CI without rebuilds — tool versions are captured as logical strings, never as OS-specific binary hashes.
- **Reads your existing toolchain.** No replacement for aqua / mise / nix / pnpm / `go.mod`. Tool versions come from the runtime binary's `--version`, lockfiles, or repo source — whichever is the actual source of truth.
- **Explicit, validated dependencies.** Execution order comes from declared `depends` edges, so it is deterministic even on a freshly cleaned tree. Declarations are cross-checked against observed `inputs` / `outputs` overlap — reading another task's outputs without declaring the edge is an error, so the DAG can't silently drift from reality.
- **Cold-state bootstrap.** A tool whose sources import generated files declares its producers once, on the tool (`tools.<name>.depends`); resolution defers until they've run, so `sloff run` succeeds in one shot even after deleting every generated file.
- **Dynamic task sets.** `command_providers` run at plan time and emit tasks as JSON — per-directory fan-out and import-closure inputs stay out of hand-written YAML, and generated tasks flow through the same validation and fingerprinting as static ones.
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

`sloff run` discovers every `sloff.yml`, orders tasks by their declared `depends` edges, and either skips or re-runs each task based on fingerprint lookup.

## Task dependencies

When a task consumes files that another task generates, declare the edge with `depends`. Each entry names a task in the same file, or in another spec dir (relative to the declaring `sloff.yml`):

```yaml
commands:
  - name: bundle
    cmd: ./bundle.sh
    inputs: ["../gen/**/*.pb.ts"]
    outputs: ["dist/bundle.ts"]
    tools: [bundler]
    depends:
      - { spec: ../gen, task: codegen } # task in another spec dir
      - { task: lint-schema }           # task in this file
```

`depends` only controls scheduling — invalidation still flows through file contents (when an upstream output listed in your `inputs` changes, your fingerprint changes). To keep the two honest, sloff validates declarations against observed `inputs` / `outputs` overlap:

- Reading files another task produces **without** declaring `depends` on it → **error**, with the missing edge spelled out.
- Declaring `depends` on a task whose outputs never appear in your `inputs` → **warning** (that dependency will never invalidate you).

### Pattern dependencies

`task` accepts glob patterns, expanded at plan time against the target spec's task set — including dynamically generated tasks:

```yaml
    depends:
      - { spec: ../gen, task: "gen-*" } # every gen-* task, present and future
```

### Barrier tasks

A `barrier: true` task is a pure aggregation point: it executes nothing, has no fingerprint, and completes when all of its `depends` complete (failing if any of them fails). Use it to give "these N tasks are done" a single name:

```yaml
commands:
  - name: gen-all
    barrier: true
    depends:
      - { task: "gen-*" }
```

Barriers declare only `depends` — `cmd` / `inputs` / `outputs` / `tools` are rejected. Depending on a barrier is not a substitute for data edges: a task that actually reads a member's outputs still needs a direct `depends` on that producer.

### Tool bootstrap dependencies

When a tool's *own sources* depend on generated files (e.g. an in-repo protoc plugin that imports generated `*.pb.go`), declare that on the tool instead of repeating it on every consumer task:

```yaml
tools:
  protoc-gen-foo:
    go-local: ./cmd/protoc-gen-foo
    depends:
      - { task: gen-options } # the task that generates what the tool imports
```

sloff injects the edge into every task that uses the tool. On a clean tree — where the tool can't even be resolved until those files exist — resolution is deferred until its declared dependencies have run, so a single `sloff run` bootstraps from zero. Tools *without* a `depends` declaration keep failing fast at run start.

## Dynamic tasks

When the task set itself is derived from your tree (per-directory codegen, import-closure inputs), declare a `command_providers` entry instead of generating `sloff.yml` files out-of-band:

```yaml
command_providers:
  - name: proto-perdir
    exec: ["go", "run", "./tools/emit-proto-tasks"]
```

The provider runs at plan time (cwd = the spec dir) and prints the task list as JSON on stdout:

```json
{
  "schema_version": "v1",
  "tasks": [
    {
      "name": "gen-foo",
      "cmd": ["buf", "generate", "--path", "foo"],
      "inputs": ["foo/**/*.proto"],
      "outputs": ["gen/foo/**/*.pb.go"],
      "tools": ["buf"]
    }
  ]
}
```

Generated tasks go through exactly the same validation, dependency checks, and fingerprinting as hand-written ones, and providers re-run on every `sloff run` — the task set can't drift from the tree.

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

## Fingerprint storage

By default sloff persists records to `.sloff/fingerprints/` under the repo root and commits them to git (ADR-0003). For monorepos where git-noise / clone-size pressure starts mattering, switch to the **DynamoDB backend** via `.sloff/config.yml`:

```yaml
# .sloff/config.yml
fingerprint:
  backend: dynamodb
  dynamodb:
    table: sloff-fingerprints # required
    region: us-east-1         # optional; falls back to AWS_REGION / shared config
    # endpoint: ""            # optional; emulator URL
    # expires_after_days: 0   # optional; >0 enables DynamoDB TTL-based GC
```

Credentials are resolved through the aws-sdk-go-v2 default chain (env vars / `~/.aws/credentials` / IRSA / IMDS), so the config file carries no secrets and is safe to commit. The DynamoDB backend is fronted by a transparent `$XDG_CACHE_HOME`-rooted disk cache so warm lookups stay local; see [Storage: DynamoDB](./docs/design/storage-dynamodb.md) for the full design (schema, caching, consistency, cost).

sloff does **not** auto-create the DynamoDB table — provision it once with your IaC of choice.

<details>
<summary>Table provisioning (AWS CLI / Terraform)</summary>

```sh
aws dynamodb create-table \
  --table-name sloff-fingerprints \
  --attribute-definitions \
      AttributeName=pk,AttributeType=S \
      AttributeName=sk,AttributeType=S \
  --key-schema \
      AttributeName=pk,KeyType=HASH \
      AttributeName=sk,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST
```

```hcl
resource "aws_dynamodb_table" "sloff_fingerprints" {
  name         = "sloff-fingerprints"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute { name = "pk"; type = "S" }
  attribute { name = "sk"; type = "S" }

  # Required only when expires_after_days > 0 in .sloff/config.yml.
  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }
}
```

</details>

<details>
<summary>Required IAM actions</summary>

```json
{
  "Effect": "Allow",
  "Action": [
    "dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:DeleteItem",
    "dynamodb:Query", "dynamodb:Scan",
    "dynamodb:BatchGetItem", "dynamodb:BatchWriteItem",
    "dynamodb:DescribeTable"
  ],
  "Resource": "arn:aws:dynamodb:<region>:<account>:table/sloff-fingerprints"
}
```

</details>

## CLI reference

| Command | What it does |
|---|---|
| `sloff run` | Discover specs and run / skip every task. `--force` re-executes everything while still writing records; `--root` / `--pattern` scope spec discovery. |
| `sloff graph` | Render the declared task DAG as Mermaid (default) or DOT (`--format dot`). |
| `sloff fingerprint show <file>` | Decode a fingerprint record to JSON — also works as a git diff textconv. |
| `sloff fingerprint diff <a> <b>` | Semantic diff between two records (exit code 1 if they differ). |
| `sloff fingerprint gc` | Collapse duplicate record variants left behind by branch merges. |
| `sloff version` | Print the binary version. |

Environment variables:

- `SLOFF_ALLOW_STALE_DEPS=1` — degrade preflight failures (e.g. pnpm install drift) from a hard error to a warning; the run proceeds but fingerprints are **not** written for a known-suspect run.
- `SLOFF_NO_FILE_HASH_CACHE=1` — skip the persistent per-file digest cache and rehash everything from disk.

sloff also emits OpenTelemetry trace spans (per-phase and per-task timing, fingerprint hit/miss) when the standard `OTEL_*` env vars are set — nothing is exported unless you set `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_TRACES_EXPORTER`. Every `OTEL_*` key can be overridden just for sloff via a `SLOFF_OTEL_*` twin.

## When to use sloff (vs alternatives)

sloff is well-suited when:

- You have a **polyglot codegen pipeline** (Go + JS/TS + external prebuilt binaries) and want shared fingerprint store across devs and CI.
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

sloff is intentionally narrow — **codegen orchestration with honest fingerprints, nothing else**.

## Documentation

- [Architecture](./docs/design/architecture.md) — overall design
- [Dynamic tasks](./docs/design/dynamic-tasks.md) — `command_providers` design space
- [Resolver: script](./docs/design/resolver-script.md)
- [Resolver: go-local](./docs/design/resolver-go-local.md)
- [Resolver: pnpm-local](./docs/design/resolver-pnpm-local.md)
- [Storage: DynamoDB](./docs/design/storage-dynamodb.md)
- [ADRs](./docs/adr/) — design decision records (in Japanese)

## License

[MIT](./LICENSE)
