package pnpmlocal_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/pnpmlocal"
)

// TestGitLsFiles_ReturnsTrackedAndUntrackedNonIgnored guards the core
// enumerator contract: tracked files AND untracked-but-not-ignored files
// surface, while gitignored paths (typically build outputs) do not. Without
// this, dist/ artefacts would land in ExtraInputs and tie the cache to
// per-developer build state.
func TestGitLsFiles_ReturnsTrackedAndUntrackedNonIgnored(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "packages", "codegen", "src", "cli.ts"), "export const x = 1;\n")
	mustWrite(t, filepath.Join(root, "packages", "codegen", "src", "lib.ts"), "export const y = 2;\n")
	mustWrite(t, filepath.Join(root, "packages", "codegen", "package.json"), `{"name":"@org/codegen"}`)
	mustWrite(t, filepath.Join(root, "packages", "codegen", ".gitignore"), "dist/\n")
	mustWrite(t, filepath.Join(root, "packages", "codegen", "dist", "cli.js"), "compiled\n")

	gitInit(t, root)

	got, err := pnpmlocal.GitLsFiles(context.Background(), root, filepath.Join("packages", "codegen"))
	if err != nil {
		t.Fatalf("GitLsFiles: %v", err)
	}
	sort.Strings(got)
	want := []string{
		"packages/codegen/.gitignore",
		"packages/codegen/package.json",
		"packages/codegen/src/cli.ts",
		"packages/codegen/src/lib.ts",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("file list mismatch (-want +got):\n%s", diff)
	}
}

// TestGitLsFiles_HonoursTrackedFilesEvenWhenNowIgnored is the subtle case:
// once a file is `git add`-ed, --cached returns it even if it later matches
// .gitignore. This guards that we don't accidentally drop committed files.
func TestGitLsFiles_HonoursTrackedFilesEvenWhenNowIgnored(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "packages", "codegen", "package.json"), `{"name":"x"}`)
	mustWrite(t, filepath.Join(root, "packages", "codegen", "dist", "cli.js"), "compiled\n")
	gitInit(t, root)
	// Track dist/cli.js BEFORE writing the .gitignore that would hide it.
	gitRun(t, root, "add", filepath.Join("packages", "codegen", "dist", "cli.js"))
	mustWrite(t, filepath.Join(root, "packages", "codegen", ".gitignore"), "dist/\n")

	got, err := pnpmlocal.GitLsFiles(context.Background(), root, filepath.Join("packages", "codegen"))
	if err != nil {
		t.Fatalf("GitLsFiles: %v", err)
	}
	if !slices.Contains(got, "packages/codegen/dist/cli.js") {
		t.Errorf("tracked dist/cli.js should still be returned despite .gitignore, got %v", got)
	}
}

// TestGitLsFiles_FailsOutsideGitRepo guards that the enumerator surfaces a
// useful error instead of silently returning an empty list. An empty list
// would make ExtraInputs degenerate and quietly lose source-change
// invalidation — far worse than failing fast.
func TestGitLsFiles_FailsOutsideGitRepo(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "packages", "codegen", "src", "cli.ts"), "x\n")

	if _, err := pnpmlocal.GitLsFiles(context.Background(), root, filepath.Join("packages", "codegen")); err == nil {
		t.Fatal("expected error when run outside a git repository")
	}
}

// gitInit runs `git init -q` in the given directory, then sets a deterministic
// committer config so any later `git add` doesn't blow up on missing identity.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "lazygen-test@example.com")
	gitRun(t, dir, "config", "user.name", "lazygen-test")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// mustWrite is shared with other pnpmlocal_test files via the lockfile_test
// declaration; redeclaring here would conflict at compile time, so this file
// relies on the package-level helper there.

var _ = os.WriteFile // keep "os" import alive for environments where the
// other test files are filtered out (defensive; unreachable in practice).
