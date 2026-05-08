package pnpmlocal_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	preflightpnpm "github.com/izumin5210/lazygen/internal/lazygen/preflight/pnpmlocal"
)

const sampleLockfile = `lockfileVersion: '9.0'
importers:
  packages/codegen:
    dependencies:
      lodash:
        specifier: ^4.17.0
        version: 4.17.21
snapshots:
  lodash@4.17.21: {}
`

// TestChecker_OKWhenInstallInSync is the green path: lockfile and install
// snapshot byte-match. The Checker must report OK with no issues so the
// runner doesn't block the pipeline on a healthy state.
func TestChecker_OKWhenInstallInSync(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), sampleLockfile)
	mustWrite(t, filepath.Join(root, "node_modules", ".pnpm", "lock.yaml"), sampleLockfile)

	res, err := preflightpnpm.New(root).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.OK || len(res.Issues) != 0 {
		t.Errorf("expected OK with no issues, got %+v", res)
	}
}

// TestChecker_DriftSurfacesAsIssue is the headline behaviour: install drift
// surfaces as a preflight.Issue (not a hard error), so the runner can apply
// the shared LAZYGEN_ALLOW_STALE_DEPS read-only fall-through uniformly with
// other preflight failures.
func TestChecker_DriftSurfacesAsIssue(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), sampleLockfile)
	mustWrite(t, filepath.Join(root, "node_modules", ".pnpm", "lock.yaml"),
		strings.ReplaceAll(sampleLockfile, "4.17.21", "4.17.20"))

	res, err := preflightpnpm.New(root).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check returned a hard error instead of an Issue: %v", err)
	}
	if res.OK {
		t.Fatal("expected NOT OK on drift")
	}
	if len(res.Issues) != 1 {
		t.Fatalf("expected exactly 1 Issue, got %d: %+v", len(res.Issues), res.Issues)
	}
	if res.Issues[0].Channel != "pnpm-local" {
		t.Errorf("Issue.Channel = %q, want pnpm-local", res.Issues[0].Channel)
	}
	if !strings.Contains(res.Issues[0].Suggestion, "pnpm install") {
		t.Errorf("Issue.Suggestion should advise pnpm install, got %q", res.Issues[0].Suggestion)
	}
}

// TestChecker_MissingSnapshotSurfacesAsIssue ensures the more common variant
// of drift — pnpm install never ran — also goes through the Issue channel
// rather than a hard error, so LAZYGEN_ALLOW_STALE_DEPS still applies.
func TestChecker_MissingSnapshotSurfacesAsIssue(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), sampleLockfile)
	// node_modules/.pnpm/lock.yaml intentionally absent.

	res, err := preflightpnpm.New(root).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check returned a hard error: %v", err)
	}
	if res.OK || len(res.Issues) != 1 {
		t.Errorf("expected one Issue for missing snapshot, got %+v", res)
	}
}

// TestChecker_MissingLockfileIsHardError guards the boundary: drift means
// "install vs lockfile mismatch", but a missing lockfile is a different
// failure (no pnpm setup at all). It must surface as a hard error so the
// runner doesn't silently degrade it via LAZYGEN_ALLOW_STALE_DEPS.
func TestChecker_MissingLockfileIsHardError(t *testing.T) {
	root := t.TempDir()
	if _, err := preflightpnpm.New(root).Check(context.Background(), "."); err == nil {
		t.Fatal("expected hard error when pnpm-lock.yaml is missing")
	}
}

func mustWrite(t *testing.T, full, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
