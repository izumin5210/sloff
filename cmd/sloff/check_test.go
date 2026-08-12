package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/runner"
)

// runCheckCmd invokes the assembled root command with `check` and the given
// flags, returning stdout (the report), stderr, and the command error.
func runCheckCmd(t *testing.T, workdir string, extra ...string) (stdout, stderr string, err error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := newRootCmd()
	args := append([]string{"check", "--root", workdir}, extra...)
	root.SetArgs(args)
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	err = root.ExecuteContext(context.Background())
	return outBuf.String(), errBuf.String(), err
}

// wantExitCode asserts err is an exitCodeError carrying code.
func wantExitCode(t *testing.T, err error, code int) {
	t.Helper()
	var ec *exitCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("expected exitCodeError(%d), got: %v", code, err)
	}
	if ec.code != code {
		t.Fatalf("expected exit code %d, got %d", code, ec.code)
	}
}

// TestCheck_CleanExitsZero: after a run has written records, a check of the
// untouched tree reports no drift and returns nil (exit 0).
func TestCheck_CleanExitsZero(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "check-clean")
	if _, err := runRunCmd(t, workdir); err != nil {
		t.Fatalf("run cmd failed: %v", err)
	}
	stdout, _, err := runCheckCmd(t, workdir)
	if err != nil {
		t.Fatalf("check cmd failed: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "no drift") {
		t.Errorf("expected clean summary, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "DRIFT") {
		t.Errorf("clean check must not print drift lines, got:\n%s", stdout)
	}
}

// TestCheck_DriftExitsOne: tampering with a generated output after the run
// makes check report the task and exit 1 with remediation guidance.
func TestCheck_DriftExitsOne(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "check-clean")
	if _, err := runRunCmd(t, workdir); err != nil {
		t.Fatalf("run cmd failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "spec", "output.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCheckCmd(t, workdir)
	wantExitCode(t, err, checkExitDrift)
	for _, want := range []string{"DRIFT spec/copy", "modified: spec/output.txt", "1 drifted", "sloff run"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestCheck_NoRecordExitsOne: a check against a tree that has never been
// generated reports no-record drift, and must not write any record itself.
func TestCheck_NoRecordExitsOne(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "check-clean")
	stdout, _, err := runCheckCmd(t, workdir)
	wantExitCode(t, err, checkExitDrift)
	if !strings.Contains(stdout, "no fingerprint record") {
		t.Errorf("expected no-record drift, got:\n%s", stdout)
	}
	if _, statErr := os.Stat(filepath.Join(workdir, ".sloff")); !os.IsNotExist(statErr) {
		t.Errorf("check must not create fingerprint state, stat(.sloff) err=%v", statErr)
	}
}

// TestCheck_PreflightFailureExitsTwo: an install-drift preflight failure is
// an environment problem, not drift — exit 2 with the cause on stderr.
func TestCheck_PreflightFailureExitsTwo(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "check-preflight-strict")
	_, stderr, err := runCheckCmd(t, workdir)
	wantExitCode(t, err, checkExitError)
	if !strings.Contains(stderr, "preflight failed") {
		t.Errorf("expected preflight failure on stderr, got:\n%s", stderr)
	}
}

// TestCheck_AllowStaleDepsIgnored: the escape hatch that degrades preflight
// failures for `run` must not weaken `check` — same exit 2, plus a warning
// that the variable is ignored.
func TestCheck_AllowStaleDepsIgnored(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "check-preflight-strict")
	t.Setenv(allowStaleDepsEnv, "1")
	_, stderr, err := runCheckCmd(t, workdir)
	wantExitCode(t, err, checkExitError)
	if !strings.Contains(stderr, "ignored by check") {
		t.Errorf("expected ignored-escape-hatch warning on stderr, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "preflight failed") {
		t.Errorf("expected preflight failure on stderr, got:\n%s", stderr)
	}
}

// TestCheck_EnvClassifiedToolFailureIsNotDrift: when a tool cannot resolve
// but every producer in its depends closure is clean, the task must render
// as CANNOT VERIFY (not DRIFT) and the command must exit 2 — the output has
// to agree with the exit-code contract instead of misdirecting the user to
// `sloff run`.
func TestCheck_EnvClassifiedToolFailureIsNotDrift(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	workdir := setupRunHarness(t, "check-tooldepends-env-error")
	if _, err := runRunCmd(t, workdir); err != nil {
		t.Fatalf("run cmd failed: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(workdir, "cmd", "tool")); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runCheckCmd(t, workdir)
	wantExitCode(t, err, checkExitError)
	if !strings.Contains(stdout, "CANNOT VERIFY consume") {
		t.Errorf("expected CANNOT VERIFY line for consume, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "DRIFT") {
		t.Errorf("environment-classified failure must not print DRIFT lines, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "1 unverifiable") {
		t.Errorf("summary should count the unverifiable task, got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "could not be resolved") {
		t.Errorf("expected resolution failure on stderr, got:\n%s", stderr)
	}
}

// TestCheck_BackendAwareRecordWording pins the remediation and no-record
// phrasing to the storage backend: the local backend stores records in the
// repo (commit .sloff/fingerprints/), remote backends receive them from
// `sloff run` directly, so pointing users at a repo path that never changes
// would be wrong.
func TestCheck_BackendAwareRecordWording(t *testing.T) {
	local := checkRemediation(true)
	remote := checkRemediation(false)
	if !strings.Contains(local, ".sloff/fingerprints/") {
		t.Errorf("local remediation should mention the record path, got: %s", local)
	}
	if strings.Contains(remote, ".sloff/fingerprints/") {
		t.Errorf("remote remediation must not point at the in-repo record path, got: %s", remote)
	}

	rep := &runner.CheckReport{Results: []runner.CheckResult{
		{SpecRelpath: "spec", Task: "copy", Status: runner.CheckNoRecord},
	}}
	var localOut, remoteOut bytes.Buffer
	printCheckReport(&localOut, rep, true)
	printCheckReport(&remoteOut, rep, false)
	if !strings.Contains(localOut.String(), "not committed") {
		t.Errorf("local no-record message should mention committing, got:\n%s", localOut.String())
	}
	if strings.Contains(remoteOut.String(), "not committed") {
		t.Errorf("remote no-record message must not mention committing, got:\n%s", remoteOut.String())
	}
	if !strings.Contains(remoteOut.String(), "fingerprint backend") {
		t.Errorf("remote no-record message should point at the backend, got:\n%s", remoteOut.String())
	}
}

// TestCheck_AllowStaleDepsInvalidValueExitsTwo keeps the fail-loudly contract
// for unparseable env values on the check path too.
func TestCheck_AllowStaleDepsInvalidValueExitsTwo(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "check-clean")
	t.Setenv(allowStaleDepsEnv, "yes")
	_, stderr, err := runCheckCmd(t, workdir)
	wantExitCode(t, err, checkExitError)
	if !strings.Contains(stderr, allowStaleDepsEnv) || !strings.Contains(stderr, "yes") {
		t.Errorf("error should name the env var and the offending value, got:\n%s", stderr)
	}
}
