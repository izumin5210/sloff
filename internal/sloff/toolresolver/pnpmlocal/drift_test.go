package pnpmlocal_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
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

// TestAssertInstallInSync_ByteIdenticalSnapshotPasses is the happy path:
// pnpm copies pnpm-lock.yaml verbatim into node_modules/.pnpm/lock.yaml at
// install time, so a fresh install produces identical bytes and the check
// passes silently.
func TestAssertInstallInSync_ByteIdenticalSnapshotPasses(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), sampleLockfile)
	mustWrite(t, filepath.Join(root, "node_modules", ".pnpm", "lock.yaml"), sampleLockfile)

	if err := pnpmlocal.AssertInstallInSync(root); err != nil {
		t.Fatalf("AssertInstallInSync: %v", err)
	}
}

// TestAssertInstallInSync_MissingSnapshotErrors covers the most common
// failure: pnpm install was never run on this checkout, so .pnpm/lock.yaml
// doesn't exist. The error must wrap ErrInstallStale so the runner can
// surface a clean "run pnpm install" message instead of a low-level fs
// error.
func TestAssertInstallInSync_MissingSnapshotErrors(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), sampleLockfile)

	err := pnpmlocal.AssertInstallInSync(root)
	if err == nil {
		t.Fatal("expected error when node_modules/.pnpm/lock.yaml is missing")
	}
	if !errors.Is(err, pnpmlocal.ErrInstallStale) {
		t.Errorf("error should wrap ErrInstallStale, got %v", err)
	}
	if !strings.Contains(err.Error(), "pnpm install") {
		t.Errorf("error should advise running pnpm install, got %q", err)
	}
}

// TestAssertInstallInSync_ContentDriftErrors is the silent-stale scenario:
// the user pulled an updated pnpm-lock.yaml but didn't rerun pnpm install,
// so node_modules/.pnpm/lock.yaml still reflects the previous lockfile
// content. Byte mismatch must error so the resolver doesn't hand fresh-
// lockfile versions to a stale-install runtime.
func TestAssertInstallInSync_ContentDriftErrors(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), sampleLockfile)
	// Snapshot reflects an older state — different version pin.
	mustWrite(t, filepath.Join(root, "node_modules", ".pnpm", "lock.yaml"),
		strings.ReplaceAll(sampleLockfile, "4.17.21", "4.17.20"))

	err := pnpmlocal.AssertInstallInSync(root)
	if err == nil {
		t.Fatal("expected error when pnpm-lock.yaml differs from .pnpm/lock.yaml")
	}
	if !errors.Is(err, pnpmlocal.ErrInstallStale) {
		t.Errorf("error should wrap ErrInstallStale, got %v", err)
	}
}

// TestAssertInstallInSync_Pnpm12MultiDocLockfilePasses covers the pnpm 12
// snapshot behaviour: pnpm 12 prepends a self-pin document to pnpm-lock.yaml
// but writes only the final (lockfile) document to node_modules/.pnpm/lock.yaml.
// A whole-file byte comparison would therefore report permanent drift that no
// `pnpm install` can clear; the comparison must be per final document.
func TestAssertInstallInSync_Pnpm12MultiDocLockfilePasses(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), pnpm12HeadDocument+sampleLockfile)
	mustWrite(t, filepath.Join(root, "node_modules", ".pnpm", "lock.yaml"), sampleLockfile)

	if err := pnpmlocal.AssertInstallInSync(root); err != nil {
		t.Fatalf("AssertInstallInSync: %v", err)
	}
}

// TestAssertInstallInSync_Pnpm12MultiDocDriftStillErrors pins that scoping the
// comparison to the final document didn't blunt the check: real content drift
// in the lockfile document must still surface as ErrInstallStale.
func TestAssertInstallInSync_Pnpm12MultiDocDriftStillErrors(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), pnpm12HeadDocument+sampleLockfile)
	mustWrite(t, filepath.Join(root, "node_modules", ".pnpm", "lock.yaml"),
		strings.ReplaceAll(sampleLockfile, "4.17.21", "4.17.20"))

	if err := pnpmlocal.AssertInstallInSync(root); !errors.Is(err, pnpmlocal.ErrInstallStale) {
		t.Errorf("drift within the lockfile document must error with ErrInstallStale, got %v", err)
	}
}

// TestAssertInstallInSync_MissingLockfileErrors guards the boundary where
// the workspace has no lockfile at all (pnpm wasn't introduced yet, or it
// was deleted). We surface a plain IO error rather than ErrInstallStale —
// "stale" implies install vs lockfile mismatch, but here lockfile itself
// is absent and the user message should reflect that.
func TestAssertInstallInSync_MissingLockfileErrors(t *testing.T) {
	root := t.TempDir()

	err := pnpmlocal.AssertInstallInSync(root)
	if err == nil {
		t.Fatal("expected error when pnpm-lock.yaml is missing")
	}
	if errors.Is(err, pnpmlocal.ErrInstallStale) {
		t.Errorf("missing lockfile should not be reported as install drift, got %v", err)
	}
}

// TestAssertInstallInSync_WhitespaceOnlyDriftErrors guards against the
// "but it's just whitespace!" intuition: even cosmetic edits to
// pnpm-lock.yaml mean pnpm hasn't refreshed its install snapshot, so the
// install state is conceptually drifted. The byte comparison catches this
// without needing any YAML semantics.
func TestAssertInstallInSync_WhitespaceOnlyDriftErrors(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), sampleLockfile+"\n")
	mustWrite(t, filepath.Join(root, "node_modules", ".pnpm", "lock.yaml"), sampleLockfile)

	if err := pnpmlocal.AssertInstallInSync(root); !errors.Is(err, pnpmlocal.ErrInstallStale) {
		t.Errorf("trailing-newline drift must error with ErrInstallStale, got %v", err)
	}
}

// mustWrite is shared with other pnpmlocal_test files via the lockfile_test
// declaration; redeclaring here would conflict at compile time.
var _ = os.WriteFile // defensive — keep the import alive even if other test
// files get filtered out at build time.
