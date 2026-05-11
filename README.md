# sloff 🦥

> A fingerprint-aware codegen orchestrator for polyglot monorepos. Skips work when inputs haven't changed — and never lies about it.

`sloff` runs your code generators (proto / SQL / mock / GraphQL / etc.) and fingerprints their outputs in git so devs and CI skip re-generation when nothing has changed. Fingerprint hits are validated by **comparing both inputs *and* the actual output files** against the recorded state — so even when the fingerprint looks valid, a drifted output triggers re-generation rather than a stale skip.

## Features

- **Output-comparison fingerprint hits.** A hit requires the recorded `input_hash` *and* the on-disk output files to match. Drifted outputs (manual edits, formatter runs, partial checkouts) are re-generated, never silently skipped.
- **OS-portable fingerprints.** Fingerprints are deterministic YAML committed to git. A record built on macOS works on Linux CI without rebuilds — tool versions are captured as logical strings, never as OS-specific binary hashes.
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

`sloff run` discovers every `sloff.yml`, builds a DAG from `inputs` / `outputs` overlap, and either skips or re-runs each task based on fingerprint lookup.

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

By default sloff persists fingerprint records to `.sloff/fingerprints/` under the repo root and expects them to be committed to git (the "shared via git" model from ADR-0003). For monorepos with hundreds of tasks where git-noise / clone-size pressure starts mattering, sloff can also offload the store to **Amazon DynamoDB** with a transparent local disk cache in front so per-task lookups stay sub-millisecond on warm runs.

Both backends speak the same wire format and can be swapped without re-generating records.

### Default: local (git-managed)

No configuration needed. Records land at `.sloff/fingerprints/<spec>/<task>/<TS>-<input_hash>.pb` and the user commits them. See [ADR-0003](./docs/adr/0003-fingerprint-storage-strategy.md) for the rationale and operational notes (`.gitattributes` setup, gc, etc.).

### DynamoDB backend (opt-in)

Selected via `.sloff/config.yml` at the repo root. Credentials are resolved through the aws-sdk-go-v2 default chain (env vars / `~/.aws/credentials` / IRSA / IMDS), so config does not carry secrets and is safe to commit.

```yaml
# .sloff/config.yml
fingerprint:
  backend: dynamodb           # omit or set to "local" for the default
  dynamodb:
    table: sloff-fingerprints # required
    region: us-east-1         # optional; falls back to AWS_REGION / shared config
    endpoint: ""              # optional; emulator URL (e.g. http://localhost:4566)
    expires_after_days: 0     # optional; 0 = no TTL, >0 enables DynamoDB TTL
```

#### Table provisioning

sloff does **not** create the table for you — give the table to your infra team (IaC or AWS Console) so the deployment surface stays explicit.

AWS CLI:

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

Terraform:

```hcl
resource "aws_dynamodb_table" "sloff_fingerprints" {
  name         = "sloff-fingerprints"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute { name = "pk"; type = "S" }
  attribute { name = "sk"; type = "S" }

  # Only required when `.sloff/config.yml` sets expires_after_days > 0.
  # Safe to enable unconditionally if you might enable TTL later.
  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }
}
```

#### Required IAM permissions

sloff needs these DynamoDB actions on the table's ARN:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:DeleteItem",
      "dynamodb:Query", "dynamodb:Scan",
      "dynamodb:BatchGetItem", "dynamodb:BatchWriteItem",
      "dynamodb:DescribeTable"
    ],
    "Resource": "arn:aws:dynamodb:<region>:<account>:table/sloff-fingerprints"
  }]
}
```

#### Two-tier local cache

When the DynamoDB backend is selected, sloff transparently mirrors every record to a disk cache under `$XDG_CACHE_HOME/sloff/fingerprints/<host>/<owner>/<repo>/...` (path derived from `git config --get remote.origin.url` in ghq style, so multiple worktrees of the same repo share the cache).

Implications:
- The first lookup after a fresh clone goes to DynamoDB; subsequent identical lookups hit the disk cache and skip the network entirely.
- Cache invalidation is structural — `input_hash` is content-addressable so a stale cache entry can never match a different input.
- The cache is per-user, never shared across machines, and safe to delete (`rm -rf "$XDG_CACHE_HOME/sloff"`) at any time.

#### Operational notes

- **`sloff fingerprint gc` is a no-op for DynamoDB.** The per-item schema has no duplicate variants to collapse; set `expires_after_days` to a positive value if you want automatic cleanup of stale records via DynamoDB's TTL.
- **Reads are eventually consistent.** A record written by another developer or CI run becomes visible to your `sloff run` within ~1 second under normal operation. The occasional pre-replication miss costs one extra generator run; correctness is preserved (the generator simply writes the same record again).
- **Concurrent writes are wire-byte identical for deterministic generators**, so last-write-wins on `PutItem` is safe; no `ConditionExpression` is needed.

Cost estimate at moderate scale (~10k tasks × 100 runs/day, mostly hits): ~$10–15/month on-demand, drops to ~$3 with the two-tier cache absorbing the bulk of reads.

See [Storage: DynamoDB](./docs/design/storage-dynamodb.md) for the schema, access pattern, and design trade-offs.

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
- [Resolver: script](./docs/design/resolver-script.md)
- [Resolver: go-local](./docs/design/resolver-go-local.md)
- [Resolver: pnpm-local](./docs/design/resolver-pnpm-local.md)
- [Storage: DynamoDB](./docs/design/storage-dynamodb.md)
- [ADRs](./docs/adr/) — design decision records (in Japanese)

## License

[MIT](./LICENSE)
