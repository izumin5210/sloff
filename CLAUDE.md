# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

`lazygen` is a cache-aware codegen orchestrator for monorepos, shipped as a single Go binary. The rationale, intent, and per-channel resolver details all live under `docs/`. Read these first whenever you need to make a design-related decision:

- `docs/adr/` — finalized decisions
- `docs/design/architecture.md` — overall architecture. Start here when in doubt.
- `docs/design/resolver-*.md` — detailed design for each distribution channel's resolver

## Common commands

```sh
go build ./cmd/lazygen                 # build CLI
go test ./...                          # run all tests (unit + E2E)
go test ./internal/lazygen/runner/...  # run a single package
go test ./internal/lazygen/runner/... -run TestE2E_FirstRun  # run a single test
```

### E2E tests

E2E tests are the primary safety net for this project. Every feature addition and bug fix MUST be accompanied by comprehensive E2E coverage — do not rely on unit tests alone to validate behavior changes.

The E2E tests under `internal/lazygen/runner` compare against `testdata/e2e/runner/<case>/{initial,expected}/` as goldens. When intentionally changing behavior:

```sh
go test ./internal/lazygen/runner/... -update   # rewrite expected/ from actual outputs
```

When adding E2E tests, create a dedicated fixture directory per test case under `testdata/e2e/<package>/<case>/` and aim for comprehensive case coverage (happy path, edge cases, regression scenarios) rather than overloading a single case. For bug fixes, add a regression case that fails before the fix.

## Repository-specific conventions

- Whenever you want to change something that constitutes a design decision (spec required fields, presence of a manual `depends`, resolver auto-dispatch policy, etc.), first review and update the corresponding ADR / design doc.

## Commit and Pull Request Rules
- Write commit messages, PR titles, and PR descriptions in English.
- Use Conventional Commits for titles: `<type>(<scope>): <description>`.
- PR descriptions must include:
    - `Why`: reason for the change.
    - `Summary`: short overview of what changed.
- If `Why` is unknown, ask the user before finalizing the PR description.
- Before writing a sentence (in PR descriptions, code comments, docs, etc.), ask "what does the reader lose if I delete this?" If nothing, delete it. The diff / code / source artifact is already there; prose should add what the artifact alone does not convey, not narrate it.
