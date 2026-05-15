package pnpmlocal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// runPnpmInstall is the default Fix implementation: it spawns `pnpm install`
// inside repoRoot and streams stdout/stderr straight through so the user sees
// pnpm's own progress and error output. The wrapping error keeps the
// exec.ExitError in the chain so callers (runner) can use errors.As to
// distinguish "process failed" from "could not launch process at all".
//
// We intentionally do NOT capture stderr into the error message: pnpm's
// stderr is already in front of the user via os.Stderr, so duplicating it
// into the error string would only add noise. The runner re-wraps this error
// as "auto-install failed: pnpm-local: <this>" so the failure mode is still
// unmistakable.
func runPnpmInstall(ctx context.Context, repoRoot string) error {
	cmd := exec.CommandContext(ctx, "pnpm", "install")
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pnpm install: %w", err)
	}
	return nil
}
