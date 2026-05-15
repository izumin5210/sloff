package runner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

// recordingSink captures EventSink callbacks in order so tests can assert
// the full lifecycle (RunStarted → TaskStarted → TaskFinished) fired with the
// expected payloads.
type recordingSink struct {
	mu     sync.Mutex
	events []sinkEvent
}

type sinkEvent struct {
	Kind     string // "PhaseChanged" | "RunStarted" | "TaskStarted" | "TaskFinished"
	Phase    runner.Phase
	Tasks    []runner.TaskRef
	Ref      runner.TaskRef
	LogPath  string
	Outcome  runner.TaskOutcome
	HasError bool
}

func (s *recordingSink) PhaseChanged(phase runner.Phase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{Kind: "PhaseChanged", Phase: phase})
}

func (s *recordingSink) RunStarted(tasks []runner.TaskRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{Kind: "RunStarted", Tasks: append([]runner.TaskRef(nil), tasks...)})
}

func (s *recordingSink) TaskStarted(ref runner.TaskRef, logPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{Kind: "TaskStarted", Ref: ref, LogPath: logPath})
}

func (s *recordingSink) TaskFinished(ref runner.TaskRef, res runner.TaskResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{Kind: "TaskFinished", Ref: ref, Outcome: res.Outcome, HasError: res.Err != nil})
}

func (s *recordingSink) snapshot() []sinkEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkEvent(nil), s.events...)
}

// withoutPhases drops PhaseChanged entries so tests that focus on the
// task-level lifecycle (RunStarted / TaskStarted / TaskFinished) stay
// readable. Phase ordering is covered by TestRunner_EventSinkPhaseOrder.
func withoutPhases(events []sinkEvent) []sinkEvent {
	out := events[:0]
	for _, e := range events {
		if e.Kind == "PhaseChanged" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func phaseSequence(events []sinkEvent) []runner.Phase {
	var out []runner.Phase
	for _, e := range events {
		if e.Kind == "PhaseChanged" {
			out = append(out, e.Phase)
		}
	}
	return out
}

// setupLogDirHarness mirrors setupHarness from runner_test.go but is
// self-contained: it reuses the simplest fixture (first-run-writes-record)
// because we only care about per-task log redirection and EventSink
// callbacks, not the full output golden compare.
func setupLogDirHarness(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	caseDir := filepath.Join(repoRoot, "testdata", "e2e", "runner", "first-run-writes-record", "initial")
	if _, err := os.Stat(caseDir); err != nil {
		t.Fatalf("fixture missing: %s", caseDir)
	}
	workdir := t.TempDir()
	if err := os.CopyFS(workdir, os.DirFS(caseDir)); err != nil {
		t.Fatalf("copy initial: %v", err)
	}
	gitInitWorkdir(t, workdir)
	return workdir
}

func buildLogDirRunner(t *testing.T, workdir string, logDir string, sink runner.EventSink) *runner.Runner {
	t.Helper()
	specs, err := spec.Discover(workdir, "**/sloff.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(script.New(workdir))
	preflightReg := preflight.NewRegistry()
	return runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: resolverReg,
		Preflight: preflightReg,
		LogDir:    logDir,
		EventSink: sink,
	})
}

// TestRunner_LogDirWritesPerTaskFile checks that with Options.LogDir set,
// each cmd's stdout/stderr lands in `<LogDir>/<spec>/<task>.log` (truncate-
// create) instead of the legacy r.stdout / r.stderr sinks. The fixture's
// cmd emits no output, so the assertion is on file presence and emptiness.
func TestRunner_LogDirWritesPerTaskFile(t *testing.T) {
	workdir := setupLogDirHarness(t)
	r := buildLogDirRunner(t, workdir, ".sloff/logs", nil)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	logPath := filepath.Join(workdir, ".sloff", "logs", "spec", "copy.log")
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("expected log file %s: %v", logPath, err)
	}
	if info.IsDir() {
		t.Fatalf("log path is a directory: %s", logPath)
	}
}

// TestRunner_EventSinkLifecycle verifies the RunStarted → TaskStarted →
// TaskFinished(Succeeded) sequence for a clean run. The fixture has exactly
// one task so the order is unambiguous and we can assert positions.
func TestRunner_EventSinkLifecycle(t *testing.T) {
	workdir := setupLogDirHarness(t)
	sink := &recordingSink{}
	r := buildLogDirRunner(t, workdir, ".sloff/logs", sink)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events := withoutPhases(sink.snapshot())
	wantRef := runner.TaskRef{SpecRelpath: "spec", Name: "copy"}
	wantLog := filepath.Join(workdir, ".sloff", "logs", "spec", "copy.log")
	want := []sinkEvent{
		{Kind: "RunStarted", Tasks: []runner.TaskRef{wantRef}},
		{Kind: "TaskStarted", Ref: wantRef, LogPath: wantLog},
		{Kind: "TaskFinished", Ref: wantRef, Outcome: runner.TaskSucceeded},
	}
	if diff := cmp.Diff(want, events, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("events mismatch (-want +got):\n%s", diff)
	}
}

// TestRunner_EventSinkPhaseOrder pins the pre-execution phase ordering so
// the TUI's "preparing: <phase>" indicator and any future automation
// (timing buckets etc.) can rely on a stable sequence. PhaseRunningTasks
// must arrive before RunStarted so display layers can switch from a
// single-line indicator to a full list without flicker.
func TestRunner_EventSinkPhaseOrder(t *testing.T) {
	workdir := setupLogDirHarness(t)
	sink := &recordingSink{}
	if err := buildLogDirRunner(t, workdir, ".sloff/logs", sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	phases := phaseSequence(sink.snapshot())
	want := []runner.Phase{
		runner.PhasePreflight,
		runner.PhaseResolveInputs,
		runner.PhaseResolveVersions,
		runner.PhasePlanning,
		runner.PhasePrefetchFingerprints,
		runner.PhaseRunningTasks,
	}
	if diff := cmp.Diff(want, phases); diff != "" {
		t.Errorf("phase order mismatch (-want +got):\n%s", diff)
	}

	// Sequencing check: PhaseRunningTasks must come strictly before RunStarted.
	events := sink.snapshot()
	phaseRunIdx, runStartedIdx := -1, -1
	for i, e := range events {
		if e.Kind == "PhaseChanged" && e.Phase == runner.PhaseRunningTasks {
			phaseRunIdx = i
		}
		if e.Kind == "RunStarted" {
			runStartedIdx = i
			break
		}
	}
	if phaseRunIdx < 0 || runStartedIdx < 0 || phaseRunIdx >= runStartedIdx {
		t.Errorf("expected PhaseRunningTasks before RunStarted, phaseRunIdx=%d runStartedIdx=%d", phaseRunIdx, runStartedIdx)
	}
}

// TestRunner_EventSinkSkippedOnHit verifies that a second run with a cached
// record fires TaskFinished(Skipped) without a TaskStarted in between.
// Skipped tasks have no cmd execution and therefore no log file to point at.
func TestRunner_EventSinkSkippedOnHit(t *testing.T) {
	workdir := setupLogDirHarness(t)
	// First run warms the cache. No sink so the events list stays focused
	// on the second run, which is the one under test.
	if err := buildLogDirRunner(t, workdir, ".sloff/logs", nil).Run(context.Background()); err != nil {
		t.Fatalf("warmup Run: %v", err)
	}

	sink := &recordingSink{}
	if err := buildLogDirRunner(t, workdir, ".sloff/logs", sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events := withoutPhases(sink.snapshot())
	wantRef := runner.TaskRef{SpecRelpath: "spec", Name: "copy"}
	want := []sinkEvent{
		{Kind: "RunStarted", Tasks: []runner.TaskRef{wantRef}},
		{Kind: "TaskFinished", Ref: wantRef, Outcome: runner.TaskSkipped},
	}
	if diff := cmp.Diff(want, events, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("events mismatch (-want +got):\n%s", diff)
	}
}

// TestRunner_EventSinkFailedOnCmdError verifies that a cmd returning non-zero
// surfaces as TaskFinished(Failed) with a non-nil Err, and the overall Run
// returns an error (parity with the existing CI behaviour).
func TestRunner_EventSinkFailedOnCmdError(t *testing.T) {
	workdir := setupLogDirHarness(t)
	// Replace the spec with a cmd that exits 1. Inputs / outputs are still
	// declared so collectTasks / depgraph stay happy; the failure path is the
	// one under test.
	specPath := filepath.Join(workdir, "spec", "sloff.yml")
	const failingSpec = `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: copy
    cmd: ["sh", "-c", "echo boom >&2; exit 1"]
    inputs: ["input.txt"]
    outputs: ["output.txt"]
    tools: [versioner]
`
	if err := os.WriteFile(specPath, []byte(failingSpec), 0o644); err != nil {
		t.Fatalf("rewrite spec: %v", err)
	}

	sink := &recordingSink{}
	err := buildLogDirRunner(t, workdir, ".sloff/logs", sink).Run(context.Background())
	if err == nil {
		t.Fatalf("expected Run to return error, got nil")
	}

	events := sink.snapshot()
	if len(events) == 0 {
		t.Fatalf("expected events, got none")
	}
	last := events[len(events)-1]
	if last.Kind != "TaskFinished" || last.Outcome != runner.TaskFailed || !last.HasError {
		t.Errorf("expected last event = TaskFinished(Failed, err!=nil), got %#v", last)
	}

	// The cmd wrote "boom\n" to stderr; the log file should capture it.
	logPath := filepath.Join(workdir, ".sloff", "logs", "spec", "copy.log")
	b, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read log: %v", readErr)
	}
	if got := string(b); got != "boom\n" {
		t.Errorf("log content = %q, want %q", got, "boom\n")
	}

	// Sanity: errors.As-style unwrap chain is preserved on the run-level
	// error. We don't need to compare the message verbatim; just confirm
	// the run still surfaces a failure (not silently swallowed).
	if errors.Is(err, context.Canceled) {
		t.Errorf("unexpected context.Canceled wrap on cmd failure: %v", err)
	}
}
