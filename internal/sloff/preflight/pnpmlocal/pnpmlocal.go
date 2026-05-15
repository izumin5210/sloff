// Package pnpmlocal implements the preflight Checker that protects pnpm-local
// from running against a stale node_modules. The check is the install-state
// dual of the resolver's lockfile-derived hashing: pnpm copies pnpm-lock.yaml
// byte-for-byte into node_modules/.pnpm/lock.yaml at install time, so a byte
// comparison cleanly detects "lockfile updated, pnpm install was forgotten"
// without parsing semantics or shelling out to pnpm.
//
// Living in preflight (rather than inside the resolver) is deliberate: the
// runner already wires SLOFF_ALLOW_STALE_DEPS read-only fall-through and
// resolver-name scoping through the preflight registry, and install drift
// is a textbook "validate state before running cmds" concern. Whether a
// channel needs a Checker is per-channel — pnpm-local does, script and
// go-local don't — but the subsystem itself isn't tied to any particular
// failure category.
package pnpmlocal

import (
	"context"
	"errors"
	"fmt"

	"github.com/izumin5210/sloff/internal/sloff/preflight"
	pnpmws "github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
)

// Name matches the resolver Name so the runner can scope this Checker to
// runs that actually reference a pnpm-local tool (architecture.md pairs
// resolvers and checkers by Name).
const Name = pnpmws.Name

// installer is the contract for the side-effectful half of the Checker: a
// function that takes a repoRoot and either reconciles node_modules with
// pnpm-lock.yaml or returns an error explaining why it could not. In
// production this is runPnpmInstall; tests inject a fake via WithInstaller
// so the unit suite never spawns the real pnpm CLI.
type installer func(ctx context.Context, repoRoot string) error

// Checker verifies that node_modules tracks the current pnpm-lock.yaml. It
// also implements preflight.Fixer (ADR-0013) so the runner can auto-recover
// from drift by invoking the configured installer.
type Checker struct {
	repoRoot string
	install  installer
}

// Option configures a Checker at construction time.
type Option func(*Checker)

// WithInstaller overrides the default installer (pnpm install via os/exec)
// with the given function. Intended for tests; production callers rely on
// the default. Passing nil is treated as "use the default" so tests can
// reset back to it without re-constructing the Checker.
func WithInstaller(fn func(ctx context.Context, repoRoot string) error) Option {
	return func(c *Checker) { c.install = fn }
}

// New returns a Checker rooted at repoRoot, applying any Options.
func New(repoRoot string, opts ...Option) *Checker {
	c := &Checker{repoRoot: repoRoot}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name implements preflight.Checker.
func (c *Checker) Name() string { return Name }

// Check delegates to pnpmlocal.AssertInstallInSync. Drift is reported as a
// preflight.Issue so the runner can show it alongside other preflight
// findings and apply the shared SLOFF_ALLOW_STALE_DEPS read-only
// fall-through. Hard errors (e.g. missing pnpm-lock.yaml) propagate
// upwards as preflight invocation errors — they aren't "drift" so much as
// "the check itself can't run".
func (c *Checker) Check(_ context.Context, _ string) (preflight.Result, error) {
	if err := pnpmws.AssertInstallInSync(c.repoRoot); err != nil {
		if errors.Is(err, pnpmws.ErrInstallStale) {
			return preflight.Result{
				OK: false,
				Issues: []preflight.Issue{{
					Channel:    Name,
					Detail:     err.Error(),
					Suggestion: "pnpm install",
				}},
			}, nil
		}
		return preflight.Result{}, fmt.Errorf("pnpm-local preflight: %w", err)
	}
	return preflight.Result{OK: true}, nil
}

// Fix implements preflight.Fixer (ADR-0013). The runner calls this when
// Check reports drift and the run is not in ReadOnly mode; on success the
// runner re-runs Check to confirm the drift is gone.
//
// The repoRoot parameter comes from the runner (Options.RepoRoot) and may
// equal c.repoRoot, but we forward whatever the runner gave us so the
// Fixer contract (drift is healed at the path the runner is operating on)
// is honoured even if a future caller constructs the Checker with a
// different root.
func (c *Checker) Fix(ctx context.Context, repoRoot string) error {
	install := c.install
	if install == nil {
		install = runPnpmInstall
	}
	return install(ctx, repoRoot)
}
