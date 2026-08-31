package pnpmlocal

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// installSnapshotPath is where pnpm copies pnpm-lock.yaml at install time.
// On every successful install, pnpm writes a snapshot here so future commands
// (and external tools like sloff) can detect "lockfile was edited but pnpm
// install was not re-run" by simple file comparison. Through pnpm 11 the
// snapshot is byte-for-byte; pnpm 12 snapshots only the final (lockfile)
// document of its multi-document pnpm-lock.yaml, omitting the leading
// self-pin document.
const installSnapshotPath = "node_modules/.pnpm/lock.yaml"

// ErrInstallStale is the sentinel returned when node_modules is out of sync
// with pnpm-lock.yaml — either because pnpm install was never run or because
// pnpm-lock.yaml has been updated since the last install. The runner surfaces
// this through the resolver error chain so the user sees a clear "run pnpm
// install" message instead of a confused stale-output fingerprint hit later.
var ErrInstallStale = errors.New("pnpm-local: node_modules is out of sync with pnpm-lock.yaml")

// AssertInstallInSync verifies that pnpm install has been run against the
// current pnpm-lock.yaml: it confirms <repoRoot>/node_modules/.pnpm/lock.yaml
// exists and its final YAML document is byte-identical to the final document
// of <repoRoot>/pnpm-lock.yaml. Scoping to the final document keeps the check
// meaningful across pnpm versions: pnpm 12 prepends a self-pin document to
// pnpm-lock.yaml that it deliberately omits from the install snapshot, so a
// whole-file comparison would report drift that no install can clear.
//
// This is a passive check: we read what pnpm itself wrote and compare it to
// what pnpm would read on the next install. We don't try to replicate
// pnpm's internal hashing, and we don't shell out to `pnpm install` — both
// were considered and rejected (see resolver-pnpm-local.md). The byte
// comparison is exact: even whitespace-only edits to pnpm-lock.yaml flip
// the result, which is the desired behaviour because such edits indicate
// the user has changed the lockfile in a way that wasn't picked up by
// pnpm install.
//
// Returns ErrInstallStale (wrapped) when node_modules is missing or stale.
func AssertInstallInSync(repoRoot string) error {
	lockPath := filepath.Join(repoRoot, LockfileName)
	snapPath := filepath.Join(repoRoot, filepath.FromSlash(installSnapshotPath))

	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("pnpm-local: read %s: %w", LockfileName, err)
	}

	snapBytes, err := os.ReadFile(snapPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s is missing — please run `pnpm install`", ErrInstallStale, installSnapshotPath)
		}
		return fmt.Errorf("pnpm-local: read %s: %w", installSnapshotPath, err)
	}

	if sha256.Sum256(lastYAMLDocument(lockBytes)) != sha256.Sum256(lastYAMLDocument(snapBytes)) {
		return fmt.Errorf("%w: pnpm-lock.yaml differs from %s — please run `pnpm install`",
			ErrInstallStale, installSnapshotPath)
	}
	return nil
}
