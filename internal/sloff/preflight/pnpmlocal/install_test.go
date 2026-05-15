package pnpmlocal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/preflight"
	preflightpnpm "github.com/izumin5210/sloff/internal/sloff/preflight/pnpmlocal"
)

// TestChecker_FixCallsInjectedInstaller is the happy path: the Fix method
// delegates to whichever installer was wired in. WithInstaller is the seam
// the runner exercises in tests (and that lets us avoid spawning real pnpm
// in the unit test suite).
func TestChecker_FixCallsInjectedInstaller(t *testing.T) {
	root := t.TempDir()
	var (
		called  bool
		gotRoot string
	)
	fake := func(_ context.Context, repoRoot string) error {
		called = true
		gotRoot = repoRoot
		return nil
	}
	c := preflightpnpm.New(root, preflightpnpm.WithInstaller(fake))
	if err := c.Fix(context.Background(), root); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if !called {
		t.Fatal("installer was never called")
	}
	if gotRoot != root {
		t.Errorf("installer got repoRoot %q, want %q", gotRoot, root)
	}
}

// TestChecker_FixPropagatesInstallerError pins the contract that Fix MUST
// surface the installer's error so the runner can wrap it into the
// "auto-install failed: pnpm-local: ..." message users see. A swallowed
// error would silently hide pnpm install failures.
func TestChecker_FixPropagatesInstallerError(t *testing.T) {
	root := t.TempDir()
	sentinel := errors.New("pnpm install boom")
	c := preflightpnpm.New(root, preflightpnpm.WithInstaller(
		func(context.Context, string) error { return sentinel },
	))
	err := c.Fix(context.Background(), root)
	if !errors.Is(err, sentinel) {
		t.Errorf("Fix err = %v, want errors.Is(err, %v)", err, sentinel)
	}
}

// TestChecker_SatisfiesFixerInterface is a compile-time check: the runner
// detects auto-fix capability via type assertion against preflight.Fixer.
// If the Checker ever stops satisfying the interface (refactor regression),
// the runner silently downgrades to "no Fixer available" and drift stops
// auto-healing — this assertion makes that a build-time failure instead.
func TestChecker_SatisfiesFixerInterface(t *testing.T) {
	var _ preflight.Fixer = preflightpnpm.New(t.TempDir())
}
