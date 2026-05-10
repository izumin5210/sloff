# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

`sloff` is a fingerprint-aware codegen orchestrator for monorepos, shipped as a single Go binary. The rationale, intent, and per-channel resolver details all live under `docs/`. Read these first whenever you need to make a design-related decision:

- `docs/adr/` — finalized decisions
- `docs/design/architecture.md` — overall architecture. Start here when in doubt.
- `docs/design/resolver-*.md` — detailed design for each distribution channel's resolver

## Common commands

```sh
go build ./cmd/sloff                 # build CLI
go test ./...                          # run all tests (unit + E2E)
go test ./internal/sloff/runner/...  # run a single package
go test ./internal/sloff/runner/... -run TestE2E_FirstRun  # run a single test
```

### E2E tests

E2E tests are the primary safety net for this project. Every feature addition and bug fix MUST be accompanied by comprehensive E2E coverage — do not rely on unit tests alone to validate behavior changes.

The E2E tests under `internal/sloff/runner` compare against `testdata/e2e/runner/<case>/{initial,expected}/` as goldens. When intentionally changing behavior:

```sh
go test ./internal/sloff/runner/... -update   # rewrite expected/ from actual outputs
```

When adding E2E tests, create a dedicated fixture directory per test case under `testdata/e2e/<package>/<case>/` and aim for comprehensive case coverage (happy path, edge cases, regression scenarios) rather than overloading a single case. For bug fixes, add a regression case that fails before the fix.

## Repository-specific conventions

- Whenever you want to change something that constitutes a design decision (spec required fields, presence of a manual `depends`, resolver auto-dispatch policy, etc.), first review and update the corresponding ADR / design doc.

## Commit and Pull Request Rules
- Write commit messages, PR titles, and PR descriptions in English.
- Use Conventional Commits for titles: `<type>(<scope>): <description>`.
- PR descriptions must include:
    - `Why`: reason for the change.
    - `What`: outline of the change plus the important / watch-out points. Do not exhaustively list every file or implementation detail — the diff already shows them.
- Optional `Notes for reviewers` only when there is something the diff does not convey (e.g. generated files, follow-up work). Keep it brief.
- Do not add a `Test plan` section.
- If `Why` is unknown, ask the user before finalizing the PR description.
- Prose (PR bodies, comments, docs) must add what the diff / code does not convey. If a sentence only narrates what the artifact already shows, delete it.
