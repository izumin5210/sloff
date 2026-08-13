package runner_test

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	preflightpnpm "github.com/izumin5210/sloff/internal/sloff/preflight/pnpmlocal"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/golocal"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

// removeStep deletes a file or directory tree from the workdir, simulating
// state a developer forgot to commit (deleted outputs, removed tool sources).
func removeStep(relpath string) step {
	return func(t *testing.T, h *harness) {
		t.Helper()
		if err := os.RemoveAll(filepath.Join(h.workdir, relpath)); err != nil {
			t.Fatal(err)
		}
	}
}

// chmodStep changes a file's permission bits. Fixtures use 0o000 to make a
// recorded output unreadable and a later 0o644 to restore it before the
// golden compare (readTree cannot read a 0o000 file either).
func chmodStep(relpath string, mode os.FileMode) step {
	return func(t *testing.T, h *harness) {
		t.Helper()
		if err := os.Chmod(filepath.Join(h.workdir, relpath), mode); err != nil {
			t.Fatal(err)
		}
	}
}

// gitCommitStep stages and commits the whole workdir. Fixtures use it to turn
// worktree files into *tracked* files, so a later removeStep produces the
// "git-tracked but absent from the worktree" state the pnpm-local resolver
// enumerates via `git ls-files --cached`. Host git config is isolated so a
// developer's gpg-sign or hooks setup cannot break the harness.
func gitCommitStep() step {
	return func(t *testing.T, h *harness) {
		t.Helper()
		for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "fixture", "--no-gpg-sign"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = h.workdir
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
				"GIT_AUTHOR_NAME=sloff-test", "GIT_AUTHOR_EMAIL=sloff-test@example.com",
				"GIT_COMMITTER_NAME=sloff-test", "GIT_COMMITTER_EMAIL=sloff-test@example.com")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}
}

type checkStepConfig struct {
	readOnly          bool
	wantErr           string
	wantStatuses      map[string]string
	wantOutputIssues  map[string][]string
	wantMissingInputs map[string][]string
	wantTools         map[string]string
	wantDriftTasks    *[]string
}

type checkStepOption func(*checkStepConfig)

// withCheckReadOnly sets Options.ReadOnly for the check, simulating
// SLOFF_ALLOW_STALE_DEPS=1 in the caller's environment. Check must ignore it
// (ADR-0021: the escape hatch cannot weaken the verification).
func withCheckReadOnly() checkStepOption {
	return func(c *checkStepConfig) { c.readOnly = true }
}

// expectCheckErr asserts Check returns an error containing substr.
func expectCheckErr(substr string) checkStepOption {
	return func(c *checkStepConfig) { c.wantErr = substr }
}

// expectCheckStatuses asserts the report contains exactly the given
// task-key → status entries. Keys are path.Join(specRelpath, taskName) in
// slash form; barrier tasks must be absent.
func expectCheckStatuses(m map[string]string) checkStepOption {
	return func(c *checkStepConfig) { c.wantStatuses = m }
}

// expectCheckOutputIssues asserts the per-file drift detail of an
// output-mismatch result. Each issue is "<reason> <slash path>".
func expectCheckOutputIssues(taskKey string, issues ...string) checkStepOption {
	return func(c *checkStepConfig) {
		if c.wantOutputIssues == nil {
			c.wantOutputIssues = map[string][]string{}
		}
		c.wantOutputIssues[taskKey] = issues
	}
}

// expectCheckMissingInputs asserts the missing-input detail of an
// input-missing result (slash-form repo-relative paths).
func expectCheckMissingInputs(taskKey string, paths ...string) checkStepOption {
	return func(c *checkStepConfig) {
		if c.wantMissingInputs == nil {
			c.wantMissingInputs = map[string][]string{}
		}
		c.wantMissingInputs[taskKey] = paths
	}
}

// expectCheckUnverifiableTool asserts which tool made a task unverifiable.
func expectCheckUnverifiableTool(taskKey, tool string) checkStepOption {
	return func(c *checkStepConfig) {
		if c.wantTools == nil {
			c.wantTools = map[string]string{}
		}
		c.wantTools[taskKey] = tool
	}
}

// expectCheckDriftTasks asserts CheckReport.Drift() contains exactly these
// task keys (sorted compare). Pass none to assert the drift set is empty —
// the contract for environment-classified unverifiable results, which must
// surface via the Check error instead.
func expectCheckDriftTasks(keys ...string) checkStepOption {
	return func(c *checkStepConfig) { c.wantDriftTasks = &keys }
}

// checkStep runs runner.Check against the current workdir state and asserts
// the report. It builds the same resolver/preflight wiring as runStep; the
// golden compare that runE2E performs afterwards doubles as the read-only
// proof (a check that executed a generator or wrote a record would change the
// tree and fail the fixture diff).
func checkStep(opts ...checkStepOption) step {
	cfg := checkStepConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	return func(t *testing.T, h *harness) {
		t.Helper()
		specs, err := spec.Discover(h.workdir, "**/sloff.yml")
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		resolverReg := toolresolver.NewRegistry()
		resolverReg.Register(script.New(h.workdir))
		resolverReg.Register(golocal.New(h.workdir, lister.NewMemoized(lister.NewGoPackages(h.workdir))))
		pnpmRes, err := pnpmlocal.New(h.workdir, pnpmlocal.GitLsFiles)
		if err != nil {
			t.Fatalf("pnpmlocal.New: %v", err)
		}
		resolverReg.Register(pnpmRes)
		preflightReg := preflight.NewRegistry()
		preflightReg.Register(preflightpnpm.New(h.workdir))
		logs := &captureLogger{t: t}
		r := runner.New(runner.Options{
			RepoRoot:  h.workdir,
			Specs:     specs,
			Storage:   local.New(h.workdir, local.WithClock(func() time.Time { return fixedClock })),
			Resolvers: resolverReg,
			Preflight: preflightReg,
			ReadOnly:  cfg.readOnly,
			Logger:    logs,
		})
		rep, err := r.Check(context.Background())
		if cfg.wantErr != "" {
			if err == nil {
				t.Fatalf("Check: expected error containing %q, got nil", cfg.wantErr)
			}
			if !strings.Contains(err.Error(), cfg.wantErr) {
				t.Fatalf("Check: error %q does not contain %q", err, cfg.wantErr)
			}
		} else if err != nil {
			t.Fatalf("Check: %v", err)
		}

		if cfg.wantStatuses != nil {
			if rep == nil {
				t.Fatalf("Check: expected a report, got nil")
			}
			got := map[string]string{}
			for _, res := range rep.Results {
				got[checkTaskKey(res)] = res.Status.String()
			}
			if diff := cmp.Diff(cfg.wantStatuses, got); diff != "" {
				t.Errorf("Check statuses mismatch (-want +got):\n%s", diff)
			}
		}
		for taskKey, want := range cfg.wantOutputIssues {
			res, ok := findCheckResult(t, rep, taskKey)
			if !ok {
				continue
			}
			got := make([]string, 0, len(res.OutputIssues))
			for _, is := range res.OutputIssues {
				got = append(got, is.Reason+" "+is.Path)
			}
			sort.Strings(got)
			sorted := append([]string(nil), want...)
			sort.Strings(sorted)
			if diff := cmp.Diff(sorted, got); diff != "" {
				t.Errorf("Check output issues for %s mismatch (-want +got):\n%s", taskKey, diff)
			}
		}
		for taskKey, want := range cfg.wantMissingInputs {
			res, ok := findCheckResult(t, rep, taskKey)
			if !ok {
				continue
			}
			got := append([]string(nil), res.MissingInputs...)
			sort.Strings(got)
			sorted := append([]string(nil), want...)
			sort.Strings(sorted)
			if diff := cmp.Diff(sorted, got); diff != "" {
				t.Errorf("Check missing inputs for %s mismatch (-want +got):\n%s", taskKey, diff)
			}
		}
		for taskKey, want := range cfg.wantTools {
			res, ok := findCheckResult(t, rep, taskKey)
			if !ok {
				continue
			}
			if res.Tool != want {
				t.Errorf("Check tool for %s: want %q, got %q", taskKey, want, res.Tool)
			}
		}
		if cfg.wantDriftTasks != nil {
			if rep == nil {
				t.Fatalf("Check: expected a report, got nil")
			}
			got := []string{}
			for _, res := range rep.Drift() {
				got = append(got, checkTaskKey(res))
			}
			sort.Strings(got)
			want := append([]string{}, *cfg.wantDriftTasks...)
			sort.Strings(want)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("Check Drift() mismatch (-want +got):\n%s", diff)
			}
		}
	}
}

func checkTaskKey(res runner.CheckResult) string {
	return path.Join(filepath.ToSlash(res.SpecRelpath), res.Task)
}

func findCheckResult(t *testing.T, rep *runner.CheckReport, taskKey string) (runner.CheckResult, bool) {
	t.Helper()
	if rep == nil {
		t.Errorf("Check: expected a report with task %s, got nil report", taskKey)
		return runner.CheckResult{}, false
	}
	for _, res := range rep.Results {
		if checkTaskKey(res) == taskKey {
			return res, true
		}
	}
	t.Errorf("Check: task %s missing from report", taskKey)
	return runner.CheckResult{}, false
}

// A committed tree whose records and outputs all match passes clean. The
// marker file pins that check executed nothing: a generator run would append
// to it and break the golden.
func TestCheck_CleanTreePasses(t *testing.T) {
	runE2E(
		t, "check-clean",
		runStep(),
		checkStep(expectCheckStatuses(map[string]string{
			"spec/copy": "ok",
		})),
	)
}

// Editing an input without re-running the generator leaves no record for the
// new input_hash: the task drifts as no-record and nothing is written.
func TestCheck_InputChangeWithoutRegenIsNoRecordDrift(t *testing.T) {
	runE2E(
		t, "check-record-miss",
		runStep(),
		writeStep("spec/input.txt", "changed"),
		checkStep(expectCheckStatuses(map[string]string{
			"spec/copy": "no-record",
		})),
	)
}

// Hand-editing a generated output drifts as output-mismatch with the
// modified file listed.
func TestCheck_OutputModifiedIsMismatchDrift(t *testing.T) {
	runE2E(
		t, "check-output-modified",
		runStep(),
		writeStep("spec/output.txt", "tampered"),
		checkStep(
			expectCheckStatuses(map[string]string{
				"spec/copy": "output-mismatch",
			}),
			expectCheckOutputIssues("spec/copy", "modified spec/output.txt"),
		),
	)
}

// Deleting a generated output drifts as output-mismatch with the missing
// file listed.
func TestCheck_OutputMissingIsMismatchDrift(t *testing.T) {
	runE2E(
		t, "check-output-missing",
		runStep(),
		removeStep("spec/output.txt"),
		checkStep(
			expectCheckStatuses(map[string]string{
				"spec/copy": "output-mismatch",
			}),
			expectCheckOutputIssues("spec/copy", "missing spec/output.txt"),
		),
	)
}

// When an upstream task's output (a downstream input) is deleted, the drift
// is caught via the producer's output-mismatch. The consumer legitimately
// hits: its input glob expands to the same (empty) set a clean checkout
// would produce, matching the record its first clean-state run wrote — the
// exact parity `sloff run` has (ADR-0013 clean-state semantics), so check
// must not invent a stricter judgement than run.
func TestCheck_UpstreamOutputMissingCaughtViaProducer(t *testing.T) {
	runE2E(
		t, "check-upstream-output-missing",
		runStep(),
		removeStep("spec/mid.txt"),
		checkStep(
			expectCheckStatuses(map[string]string{
				"spec/produce": "output-mismatch",
				"spec/consume": "ok",
			}),
			expectCheckOutputIssues("spec/produce", "missing spec/mid.txt"),
		),
	)
}

// A tool-contributed input (pnpm-local ExtraInputs come from
// `git ls-files --cached`) that is tracked but absent from the worktree
// cannot be hashed: the consumer drifts as input-missing naming the file.
func TestCheck_TrackedToolSourceMissingIsInputMissingDrift(t *testing.T) {
	runE2E(
		t, "check-input-missing",
		runStep(),
		gitCommitStep(),
		removeStep("packages/codegen/src/index.js"),
		checkStep(
			expectCheckStatuses(map[string]string{
				"copy": "input-missing",
			}),
			expectCheckMissingInputs("copy", "packages/codegen/src/index.js"),
		),
	)
}

// An unreadable recorded output means the comparison itself failed: no
// mismatch is established, so Check must error (CLI exit 2) instead of
// reporting drift — the drift remediation (run and commit) would fail on the
// same unreadable file.
func TestCheck_UnreadableOutputIsCheckErrorNotDrift(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chmod 0o000 does not prevent reads")
	}
	runE2E(
		t, "check-output-unreadable",
		runStep(),
		chmodStep("spec/output.txt", 0o000),
		checkStep(expectCheckErr("cannot verify recorded outputs")),
		chmodStep("spec/output.txt", 0o644),
	)
}

// Barrier tasks carry no fingerprint (ADR-0017) and must be absent from the
// report entirely — the exact-match status assertion catches any barrier
// entry.
func TestCheck_BarrierExcludedFromReport(t *testing.T) {
	runE2E(
		t, "check-barrier",
		runStep(),
		checkStep(expectCheckStatuses(map[string]string{
			"spec/copy": "ok",
		})),
	)
}

// Check ignores the SLOFF_ALLOW_STALE_DEPS escape hatch: a preflight failure
// (pnpm install drift) always fails the check even when the caller runs with
// ReadOnly set (ADR-0021).
func TestCheck_PreflightFailsEvenWithEscapeHatch(t *testing.T) {
	runE2E(
		t, "check-preflight-strict",
		checkStep(
			withCheckReadOnly(),
			expectCheckErr("preflight failed"),
		),
	)
}

// A depends-declared tool that cannot resolve because its generated sources
// were never produced: the producer task itself reports drift, so the
// unverifiable consumer classifies as drift too and Check succeeds with a
// drifted report (exit 1 at the CLI, not exit 2).
func TestCheck_DeferredToolProducerDriftClassifiesAsDrift(t *testing.T) {
	runE2E(
		t, "check-tooldepends-producer-drift",
		checkStep(
			expectCheckStatuses(map[string]string{
				"gen-source": "no-record",
				"consume":    "unverifiable",
			}),
			expectCheckUnverifiableTool("consume", "gen-tool"),
			expectCheckDriftTasks("gen-source", "consume"),
		),
	)
}

// A depends-declared tool that cannot resolve while every producer in its
// depends closure is clean: the failure is an environment problem, so Check
// returns an error (exit 2 at the CLI) alongside the partial report.
func TestCheck_DeferredToolEnvFailureIsError(t *testing.T) {
	runE2E(
		t, "check-tooldepends-env-error",
		runStep(),
		removeStep("cmd/tool"),
		checkStep(
			expectCheckErr("could not be resolved"),
			expectCheckStatuses(map[string]string{
				"noop":    "ok",
				"consume": "unverifiable",
			}),
			expectCheckUnverifiableTool("consume", "gen-tool"),
			// Environment-classified: the unverifiable consumer must NOT count
			// as drift — the cause travels via the Check error (CLI exit 2).
			expectCheckDriftTasks(),
		),
	)
}
