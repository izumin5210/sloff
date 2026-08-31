// Package pnpmlocal implements the preflight Checker that protects pnpm-local
// from running against a stale node_modules. The check is the install-state
// dual of the resolver's lockfile-derived hashing: pnpm snapshots
// pnpm-lock.yaml into node_modules/.pnpm/lock.yaml at install time (verbatim
// through pnpm 11; final YAML document only from pnpm 12), so a per-document
// byte comparison cleanly detects "lockfile updated, pnpm install was
// forgotten" without parsing semantics or shelling out to pnpm.
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

// Checker verifies that node_modules tracks the current pnpm-lock.yaml.
type Checker struct {
	repoRoot string
}

// New returns a Checker rooted at repoRoot.
func New(repoRoot string) *Checker {
	return &Checker{repoRoot: repoRoot}
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
