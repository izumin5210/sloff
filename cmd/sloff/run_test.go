package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// TestRun_ForceBypassesFingerprintHit drives ADR-0012's `--force` flag through
// the full CLI entry point: a normal first run populates the record, then a
// second run with `--force` must re-execute the cmd instead of taking the
// fingerprint shortcut. The fixture's cmd appends to ../marker.txt on every
// execution, so the marker file contains the run count.
func TestRun_ForceBypassesFingerprintHit(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "force-bypasses-hit")
	if _, err := runRunCmd(t, workdir); err != nil {
		t.Fatalf("first run failed: %v", err)
	}
	if _, err := runRunCmd(t, workdir, "--force"); err != nil {
		t.Fatalf("forced run failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(workdir, "marker.txt"))
	if err != nil {
		t.Fatalf("read marker.txt: %v", err)
	}
	if string(got) != "xx" {
		t.Fatalf("marker.txt = %q; want %q (force should have re-executed the cmd)", got, "xx")
	}
}

// TestRun_AllowStaleDeps verifies the SLOFF_ALLOW_STALE_DEPS env path: the
// run stays successful, outputs are produced, and the read-only mode keeps
// every fingerprint record off disk (the structural guard against polluted
// records). The underlying ReadOnly/preflight semantics are covered by
// runner_test.
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
	if _, err := os.Stat(filepath.Join(workdir, ".sloff", "fingerprints")); !os.IsNotExist(err) {
		t.Errorf("read-only mode must not write fingerprints, stat err=%v", err)
	}
}

// TestAllowStaleDepsEnabled covers the value-interpretation table directly:
// strconv.ParseBool semantics for set values, unset/empty as disabled, and
// hard errors (naming the env var and the offending value) for anything else.
func TestAllowStaleDepsEnabled(t *testing.T) {
	set := func(v string) *string { return &v }
	cases := []struct {
		name    string
		value   *string // nil = unset
		want    bool
		wantErr bool
	}{
		{name: "unset", value: nil, want: false},
		{name: "empty", value: set(""), want: false},
		{name: "1", value: set("1"), want: true},
		{name: "true", value: set("true"), want: true},
		{name: "TRUE", value: set("TRUE"), want: true},
		{name: "t", value: set("t"), want: true},
		{name: "0", value: set("0"), want: false},
		{name: "false", value: set("false"), want: false},
		{name: "yes", value: set("yes"), wantErr: true},
		{name: "oops", value: set("oops"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value == nil {
				// t.Setenv registers the restore; Unsetenv right after
				// leaves the variable absent for the test body.
				t.Setenv(allowStaleDepsEnv, "")
				os.Unsetenv(allowStaleDepsEnv)
			} else {
				t.Setenv(allowStaleDepsEnv, *tc.value)
			}
			got, err := allowStaleDepsEnabled()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", *tc.value)
				}
				if !strings.Contains(err.Error(), allowStaleDepsEnv) || !strings.Contains(err.Error(), *tc.value) {
					t.Errorf("error should name the env var and value, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("allowStaleDepsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFileHashCacheDisabled mirrors TestAllowStaleDepsEnabled for the
// SLOFF_NO_FILE_HASH_CACHE escape hatch: set values follow strconv.ParseBool,
// unset/empty keep the cache, and anything unparseable is a hard error that
// names the env var and the offending value.
func TestFileHashCacheDisabled(t *testing.T) {
	set := func(v string) *string { return &v }
	cases := []struct {
		name    string
		value   *string // nil = unset
		want    bool
		wantErr bool
	}{
		{name: "unset", value: nil, want: false},
		{name: "empty", value: set(""), want: false},
		{name: "1", value: set("1"), want: true},
		{name: "true", value: set("true"), want: true},
		{name: "0", value: set("0"), want: false},
		{name: "false", value: set("false"), want: false},
		{name: "yes", value: set("yes"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value == nil {
				t.Setenv(noFileHashCacheEnv, "")
				os.Unsetenv(noFileHashCacheEnv)
			} else {
				t.Setenv(noFileHashCacheEnv, *tc.value)
			}
			got, err := fileHashCacheDisabled()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", *tc.value)
				}
				if !strings.Contains(err.Error(), noFileHashCacheEnv) || !strings.Contains(err.Error(), *tc.value) {
					t.Errorf("error should name the env var and value, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("fileHashCacheDisabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRun_NoFileHashCacheInvalidValueFails guards the fail-loudly contract for
// the cache escape hatch: an unparseable value aborts the run with an
// actionable error rather than silently keeping or dropping the cache.
func TestRun_NoFileHashCacheInvalidValueFails(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "first-run-writes-record")
	t.Setenv(noFileHashCacheEnv, "nope")
	_, err := runRunCmd(t, workdir)
	if err == nil {
		t.Fatal("expected error for unparseable SLOFF_NO_FILE_HASH_CACHE value")
	}
	if !strings.Contains(err.Error(), noFileHashCacheEnv) || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the env var and the offending value, got: %v", err)
	}
}

// TestRun_AllowStaleDepsFalseValueWritesRecords pins the boolean
// interpretation of SLOFF_ALLOW_STALE_DEPS: explicitly disabling the escape
// hatch must behave exactly like leaving it unset, i.e. records are written.
// The previous any-non-empty check switched the run to read-only even for
// "0"/"false", so "disabling" the variable silently stopped fingerprinting.
func TestRun_AllowStaleDepsFalseValueWritesRecords(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	for _, v := range []string{"0", "false"} {
		t.Run(v, func(t *testing.T) {
			workdir := setupRunHarness(t, "first-run-writes-record")
			t.Setenv(allowStaleDepsEnv, v)
			if _, err := runRunCmd(t, workdir); err != nil {
				t.Fatalf("run cmd failed: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(workdir, ".sloff", "fingerprints"))
			if err != nil || len(entries) == 0 {
				t.Errorf("%s=%s must keep fingerprint writes enabled, got err=%v entries=%d", allowStaleDepsEnv, v, err, len(entries))
			}
		})
	}
}

// TestRun_AllowStaleDepsInvalidValueFails guards the fail-loudly contract: a
// value that doesn't parse as a boolean aborts the run with an actionable
// error instead of silently toggling the escape hatch in either direction.
func TestRun_AllowStaleDepsInvalidValueFails(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := setupRunHarness(t, "first-run-writes-record")
	t.Setenv(allowStaleDepsEnv, "yes")
	_, err := runRunCmd(t, workdir)
	if err == nil {
		t.Fatal("expected error for unparseable SLOFF_ALLOW_STALE_DEPS value")
	}
	if !strings.Contains(err.Error(), allowStaleDepsEnv) || !strings.Contains(err.Error(), "yes") {
		t.Errorf("error should name the env var and the offending value, got: %v", err)
	}
}

// addOriginRemote gives the workdir an `origin` remote so cached.CacheRoot can
// derive a namespace; without it the persistent file-hash cache stays disabled
// (CacheRoot errors → empty path) and the cache assertions below would be
// vacuous.
func addOriginRemote(t *testing.T, dir string) {
	t.Helper()
	c := exec.Command("git", "remote", "add", "origin", "https://github.com/sloff-test/fixture.git")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git remote add origin: %v\n%s", err, out)
	}
}

// TestRun_FileHashCacheWrittenByDefault is the baseline for the escape-hatch
// test below: with a derivable cache root and no opt-out, a successful run must
// persist filehashes.pb (ADR-0014). It also keeps the disabled-case assertion
// honest — if the cache were never written here, the "not written" check would
// pass vacuously.
func TestRun_FileHashCacheWrittenByDefault(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	workdir := setupRunHarness(t, "first-run-writes-record")
	addOriginRemote(t, workdir)

	cachePath := fileHashCachePath(workdir)
	if cachePath == "" {
		t.Skip("cache root not derivable in this environment")
	}
	if _, err := runRunCmd(t, workdir); err != nil {
		t.Fatalf("run cmd failed: %v", err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("expected persistent file-hash cache at %s, got: %v", cachePath, err)
	}
}

// TestRun_NoFileHashCacheSkipsPersistentCache exercises ADR-0014's escape
// hatch: SLOFF_NO_FILE_HASH_CACHE=1 must make the runner skip its persistent
// digest cache, so a suspected or corrupted filehashes.pb can never keep
// returning a stale digest. The cache file must not be created even though the
// cache root is derivable.
func TestRun_NoFileHashCacheSkipsPersistentCache(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	workdir := setupRunHarness(t, "first-run-writes-record")
	addOriginRemote(t, workdir)

	cachePath := fileHashCachePath(workdir)
	if cachePath == "" {
		t.Skip("cache root not derivable in this environment")
	}
	t.Setenv("SLOFF_NO_FILE_HASH_CACHE", "1")
	if _, err := runRunCmd(t, workdir); err != nil {
		t.Fatalf("run cmd failed: %v", err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("SLOFF_NO_FILE_HASH_CACHE=1 must skip the persistent cache, but stat(%s) err=%v", cachePath, err)
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

// TestRun_StorageLoadFailureSurfacesError covers runE's loadStorage error
// branch: a malformed .sloff/config.yml fails to parse, the storage builder
// rejects the run, and the error is surfaced through the cobra command
// instead of being swallowed.
func TestRun_StorageLoadFailureSurfacesError(t *testing.T) {
	workdir := setupRunHarness(t, "first-run-writes-record")
	dir := filepath.Join(workdir, ".sloff")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("fingerprint: [malformed]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runRunCmd(t, workdir); err == nil {
		t.Fatal("expected error when .sloff/config.yml fails to parse")
	}
}
