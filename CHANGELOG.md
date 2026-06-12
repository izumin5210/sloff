# Changelog

## [v0.0.2](https://github.com/izumin5210/sloff/compare/v0.0.1...v0.0.2) - 2026-06-12
- ci: pin GitHub Actions to full SHAs via pinact by @izumin5210 in https://github.com/izumin5210/sloff/pull/36
- fix(pnpmlocal): fail fast on unsupported pnpm-lock.yaml lockfileVersion by @izumin5210 in https://github.com/izumin5210/sloff/pull/40
- fix(fingerprint): treat superseded V2 records as misses and write local records atomically by @izumin5210 in https://github.com/izumin5210/sloff/pull/41
- feat!: declare task dependencies explicitly in specs (ADR-0013) by @izumin5210 in https://github.com/izumin5210/sloff/pull/44
- fix(cli): parse SLOFF_ALLOW_STALE_DEPS as a boolean instead of any non-empty value by @izumin5210 in https://github.com/izumin5210/sloff/pull/42
- docs: renumber duplicated ADR-0009 (otel tracing) to ADR-0013 and fix README fingerprint format claim by @izumin5210 in https://github.com/izumin5210/sloff/pull/43

## [v0.0.1](https://github.com/izumin5210/sloff/commits/v0.0.1) - 2026-05-12
- feat: foundation pipeline with script resolver by @izumin5210 in https://github.com/izumin5210/sloff/pull/1
- fix: address Codex adversarial review findings by @izumin5210 in https://github.com/izumin5210/sloff/pull/2
- ci: setup by @izumin5210 in https://github.com/izumin5210/sloff/pull/3
- docs: add CLAUDE.md and AGENTS.md for agent guidance by @izumin5210 in https://github.com/izumin5210/sloff/pull/4
- ci: enforce gofumpt formatting and add fix/vet checks by @izumin5210 in https://github.com/izumin5210/sloff/pull/5
- feat(toolresolver): add go-local resolver and source listers by @izumin5210 in https://github.com/izumin5210/sloff/pull/6
- docs: sync design docs with implementation by @izumin5210 in https://github.com/izumin5210/sloff/pull/7
- docs(adr): lazygen does not special-case buf by @izumin5210 in https://github.com/izumin5210/sloff/pull/9
- ci: check `go mod tidy` by @izumin5210 in https://github.com/izumin5210/sloff/pull/11
- feat(toolresolver): pnpm-local resolver + named tools + cmd-owned build by @izumin5210 in https://github.com/izumin5210/sloff/pull/10
- docs(adr): expand competitive comparison and clarify lazygen scope by @izumin5210 in https://github.com/izumin5210/sloff/pull/12
- refactor: rename project from lazygen to sloff by @izumin5210 in https://github.com/izumin5210/sloff/pull/13
- docs: write README by @izumin5210 in https://github.com/izumin5210/sloff/pull/14
- refactor(toolresolver): split Resolver into Inputs / Versions methods by @izumin5210 in https://github.com/izumin5210/sloff/pull/16
- feat(cli): sloff graph subcommand for DAG visualization by @izumin5210 in https://github.com/izumin5210/sloff/pull/15
- perf: cut full-cache-hit run from minutes to seconds by @izumin5210 in https://github.com/izumin5210/sloff/pull/17
- feat(glob): allow `..` in inputs/outputs patterns for cross-dir codegen by @izumin5210 in https://github.com/izumin5210/sloff/pull/18
- ci: introduce octocov for coverage gating by @izumin5210 in https://github.com/izumin5210/sloff/pull/20
- fix(runner): namespace output pattern conflicts by spec-dir-resolved path by @izumin5210 in https://github.com/izumin5210/sloff/pull/19
- ci: enable race detector in test job by @izumin5210 in https://github.com/izumin5210/sloff/pull/23
- docs: tighten PR description rules by @izumin5210 in https://github.com/izumin5210/sloff/pull/24
- feat(cache)!: switch record encoding from YAML to protobuf binary by @izumin5210 in https://github.com/izumin5210/sloff/pull/22
- feat(cache)!: prefix record filenames with creation timestamp by @izumin5210 in https://github.com/izumin5210/sloff/pull/25
- refactor!: rename cache to fingerprint across docs, code, CLI, and storage by @izumin5210 in https://github.com/izumin5210/sloff/pull/26
- feat(otel): emit OpenTelemetry trace spans for run and graph by @izumin5210 in https://github.com/izumin5210/sloff/pull/21
- refactor(otel): rename cache-themed span and attribute names to fingerprint by @izumin5210 in https://github.com/izumin5210/sloff/pull/27
- feat(fingerprint): add DynamoDB-backed Storage with bulk APIs and 2-tier disk cache by @izumin5210 in https://github.com/izumin5210/sloff/pull/29
- fix(fingerprint): honour XDG_CACHE_HOME on macOS for the disk cache by @izumin5210 in https://github.com/izumin5210/sloff/pull/30
- build(release): introduce tagpr + goreleaser release flow by @izumin5210 in https://github.com/izumin5210/sloff/pull/31
- chore(ci): drop diff condition from octocov coverage gate by @izumin5210 in https://github.com/izumin5210/sloff/pull/34
- feat(cli): add `version` subcommand by @izumin5210 in https://github.com/izumin5210/sloff/pull/33
- feat(run): add --force flag to bypass fingerprint hits by @izumin5210 in https://github.com/izumin5210/sloff/pull/35
