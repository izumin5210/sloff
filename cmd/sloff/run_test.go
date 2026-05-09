package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// setupRunHarness mirrors graph_test's harness but for the `run` subcommand.
// The runner E2E fixtures are reused — copying initial/ into a tempdir keeps
// run-cmd assertions decoupled from the runner package's golden comparison.
func setupRunHarness(t *testing.T, runnerCase string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	initial := filepath.Join(repoRoot, "testdata", "e2e", "runner", runnerCase, "initial")
	if _, err := os.Stat(initial); err != nil {
		t.Fatalf("initial fixture missing: %s", initial)
	}
	workdir := t.TempDir()
	if err := os.CopyFS(workdir, os.DirFS(initial)); err != nil {
		t.Fatalf("copy initial: %v", err)
	}
	gitInitGraphWorkdir(t, workdir)
	return workdir
}

// runRunCmd invokes the assembled root command with `run` and the given flags.
// Returns combined stderr so tests can assert on both error path and the
// happy-path log line emitted by runner.Logger.
func runRunCmd(t *testing.T, workdir string, extra ...string) (stderr string, err error) {
	t.Helper()
	var stdout, errBuf bytes.Buffer
	root := newRootCmd()
	args := append([]string{"run", "--root", workdir}, extra...)
	root.SetArgs(args)
	root.SetOut(&stdout)
	root.SetErr(&errBuf)
	err = root.ExecuteContext(context.Background())
	return errBuf.String(), err
}

// TestRun_FirstRunWritesRecord exercises the runE happy path against the
// runner package's first-run-writes-record fixture: the spec defines one
// command (`sh -c "cp ..."`) wired to a script-channel tool, and a clean run
// must succeed end to end so the OTel root span and phase wrappers introduced
// in this PR are exercised in coverage.
func TestRun_FirstRunWritesRecord(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "first-run-writes-record")
	if _, err := runRunCmd(t, workdir); err != nil {
		t.Fatalf("run cmd failed: %v", err)
	}
	// The fixture's command writes spec/output.txt — its presence confirms the
	// runner reached exec, not just early-exited.
	if _, err := os.Stat(filepath.Join(workdir, "spec", "output.txt")); err != nil {
		t.Fatalf("expected spec/output.txt to exist after run: %v", err)
	}
}

// TestRun_AllowStaleDeps verifies the SLOFF_ALLOW_STALE_DEPS env path. Setting
// it should keep the run successful and skip cache writes; the smoke is "no
// crash, output produced" — the underlying ReadOnly/preflight semantics are
// covered by runner_test.
func TestRun_AllowStaleDeps(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "first-run-writes-record")
	t.Setenv(allowStaleDepsEnv, "1")
	if _, err := runRunCmd(t, workdir); err != nil {
		t.Fatalf("run cmd failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "spec", "output.txt")); err != nil {
		t.Fatalf("expected spec/output.txt to exist after run: %v", err)
	}
}

// TestRun_InvalidRoot ensures runE surfaces a meaningful error when --root
// resolves to a path that does not exist.
func TestRun_InvalidRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := runRunCmd(t, missing); err == nil {
		t.Fatal("expected error for missing --root, got nil")
	}
}
