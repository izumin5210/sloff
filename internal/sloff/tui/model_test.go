package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/izumin5210/sloff/internal/sloff/runner"
)

// dispatch is the test-only equivalent of tea.Program.Send: it threads a
// sequence of messages through Update and returns the resulting Model.
// Commands are discarded — Update is pure under our message set and tests
// don't need to schedule follow-up ticks.
func dispatch(t *testing.T, m Model, msgs ...tea.Msg) Model {
	t.Helper()
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		mm, ok := next.(Model)
		if !ok {
			t.Fatalf("Update returned %T, want Model", next)
		}
		m = mm
	}
	return m
}

func TestModel_RunStartedSeedsRowsInOrder(t *testing.T) {
	m := NewModel(nil)
	tasks := []runner.TaskRef{
		{SpecRelpath: "a", Name: "gen"},
		{SpecRelpath: "b", Name: "gen"},
	}
	m = dispatch(t, m, runStartedMsg{Tasks: tasks})
	if got := len(m.rows); got != 2 {
		t.Fatalf("len(rows) = %d, want 2", got)
	}
	if m.rows[0].ref != tasks[0] || m.rows[1].ref != tasks[1] {
		t.Errorf("rows not in topo order: %#v", m.rows)
	}
	for i, r := range m.rows {
		if r.status != statusPending {
			t.Errorf("rows[%d].status = %v, want pending", i, r.status)
		}
	}
}

func TestModel_TaskLifecycleTransitions(t *testing.T) {
	ref := runner.TaskRef{SpecRelpath: "spec", Name: "copy"}
	m := dispatch(
		t, NewModel(nil),
		runStartedMsg{Tasks: []runner.TaskRef{ref}},
		taskStartedMsg{Ref: ref, LogPath: "/tmp/copy.log"},
	)
	if m.rows[0].status != statusRunning {
		t.Errorf("after TaskStarted: status = %v, want running", m.rows[0].status)
	}
	if m.rows[0].logPath != "/tmp/copy.log" {
		t.Errorf("logPath = %q, want /tmp/copy.log", m.rows[0].logPath)
	}

	m = dispatch(t, m, taskFinishedMsg{Ref: ref, Result: runner.TaskResult{Outcome: runner.TaskSucceeded}})
	if m.rows[0].status != statusSucceeded {
		t.Errorf("after success: status = %v, want succeeded", m.rows[0].status)
	}
}

func TestModel_SkippedAndFailedOutcomes(t *testing.T) {
	skipRef := runner.TaskRef{SpecRelpath: "a", Name: "x"}
	failRef := runner.TaskRef{SpecRelpath: "b", Name: "y"}
	m := dispatch(
		t, NewModel(nil),
		runStartedMsg{Tasks: []runner.TaskRef{skipRef, failRef}},
		taskFinishedMsg{Ref: skipRef, Result: runner.TaskResult{Outcome: runner.TaskSkipped}},
		taskFinishedMsg{Ref: failRef, Result: runner.TaskResult{Outcome: runner.TaskFailed, Err: errors.New("boom")}},
	)
	if m.rows[0].status != statusSkipped {
		t.Errorf("skip row status = %v, want skipped", m.rows[0].status)
	}
	if m.rows[1].status != statusFailed {
		t.Errorf("fail row status = %v, want failed", m.rows[1].status)
	}
	if m.rows[1].err == nil {
		t.Errorf("fail row err = nil, want non-nil")
	}
}

func TestModel_RunFinishedTriggersQuit(t *testing.T) {
	m := NewModel(nil)
	next, cmd := m.Update(runFinishedMsg{Err: errors.New("oops")})
	mm := next.(Model)
	if !mm.runDone {
		t.Errorf("runDone = false, want true")
	}
	if mm.runErr == nil {
		t.Errorf("runErr = nil, want non-nil")
	}
	if cmd == nil {
		t.Fatalf("expected tea.Quit cmd, got nil")
	}
	// The cmd should resolve to tea.QuitMsg when invoked. We can't compare
	// tea.Quit directly (different reference each construction) but we can
	// inspect the message type it produces.
	if msg := cmd(); msg == nil {
		t.Errorf("cmd() returned nil msg")
	} else if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("cmd() = %T, want tea.QuitMsg", msg)
	}
}

func TestModel_QuitKeyCancelsRunButWaitsForFinish(t *testing.T) {
	cancelled := false
	m := NewModel(func() { cancelled = true })

	// Simulate q press. We expect quitting = true and cancelRun called, but
	// NO tea.Quit cmd yet — the Program waits for runFinishedMsg.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	mm := next.(Model)
	if !mm.quitting {
		t.Errorf("quitting = false, want true after q")
	}
	if !cancelled {
		t.Errorf("cancelRun not invoked on q")
	}
	if cmd != nil {
		t.Errorf("q press returned cmd %v, want nil (wait for runFinishedMsg)", cmd)
	}

	// runFinishedMsg follows once the runner drains.
	next, cmd = mm.Update(runFinishedMsg{Err: nil})
	if cmd == nil {
		t.Fatalf("expected tea.Quit after runFinishedMsg, got nil")
	}
	mm = next.(Model)
	if mm.runErr != nil {
		t.Errorf("runErr = %v, want nil", mm.runErr)
	}
}

func TestModel_ViewRendersGlyphsForEachStatus(t *testing.T) {
	skipRef := runner.TaskRef{SpecRelpath: "a", Name: "x"}
	doneRef := runner.TaskRef{SpecRelpath: "b", Name: "y"}
	failRef := runner.TaskRef{SpecRelpath: "c", Name: "z"}
	runRef := runner.TaskRef{SpecRelpath: "d", Name: "w"}
	m := dispatch(
		t, NewModel(nil),
		runStartedMsg{Tasks: []runner.TaskRef{skipRef, doneRef, failRef, runRef}},
		taskFinishedMsg{Ref: skipRef, Result: runner.TaskResult{Outcome: runner.TaskSkipped}},
		taskFinishedMsg{Ref: doneRef, Result: runner.TaskResult{Outcome: runner.TaskSucceeded}},
		taskFinishedMsg{Ref: failRef, Result: runner.TaskResult{Outcome: runner.TaskFailed, Err: errors.New("nope")}},
		taskStartedMsg{Ref: runRef, LogPath: "/tmp/w.log"},
	)
	view := m.View()
	// Glyph presence assertions — robust against ANSI styling because
	// lipgloss styles wrap the runes but never replace them.
	for _, want := range []string{"a:x", "b:y", "c:z", "d:w", "(cached)", "✓", "✗"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q\n%s", want, view)
		}
	}
}

// TestModel_PhaseIndicatorVisibleBeforeRunStarted asserts that a
// phaseChangedMsg arriving before RunStarted shows up in the View. The
// preparing phases (preflight, resolving versions, …) are short enough
// that without this signal the user sees a blank screen and assumes the
// CLI hung.
func TestModel_PhaseIndicatorVisibleBeforeRunStarted(t *testing.T) {
	m := dispatch(t, NewModel(nil), phaseChangedMsg{Phase: runner.PhaseResolveInputs})
	view := m.View()
	if !strings.Contains(view, "resolving tool inputs") {
		t.Errorf("expected phase label in view, got:\n%s", view)
	}
}

// TestModel_PhaseIndicatorHiddenOnceTasksSeed checks that once RunStarted
// has seeded the row list, the standalone phase indicator stops rendering
// — at that point the list itself is the progress display.
func TestModel_PhaseIndicatorHiddenOnceTasksSeed(t *testing.T) {
	ref := runner.TaskRef{SpecRelpath: "spec", Name: "copy"}
	m := dispatch(
		t, NewModel(nil),
		phaseChangedMsg{Phase: runner.PhasePreflight},
		phaseChangedMsg{Phase: runner.PhaseRunningTasks},
		runStartedMsg{Tasks: []runner.TaskRef{ref}},
	)
	view := m.View()
	if strings.Contains(view, "running tasks") {
		t.Errorf("phase indicator should be hidden after RunStarted, view:\n%s", view)
	}
	if !strings.Contains(view, "spec:copy") {
		t.Errorf("task row missing from view:\n%s", view)
	}
}

// TestModel_SpinnerTickReissuesUnconditionally guards the bug where the
// spinner stopped advancing because Update only re-scheduled the tick while
// at least one row was running — TaskStarted arrives *after* the first tick,
// so the loop went silent before any row could enter the running state.
func TestModel_SpinnerTickReissuesUnconditionally(t *testing.T) {
	m := NewModel(nil)
	// No rows at all (= pre-RunStarted). The tick must still re-arm so
	// the spinner is already turning by the time TaskStarted lands.
	_, cmd := m.Update(spinnerTickMsg{})
	if cmd == nil {
		t.Fatal("spinnerTickMsg returned nil cmd before any rows; spinner would stall")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("scheduled cmd returned nil msg")
	}
}

func TestModel_RowsExposesFailureSummary(t *testing.T) {
	ref := runner.TaskRef{SpecRelpath: "spec", Name: "fail"}
	m := dispatch(
		t, NewModel(nil),
		runStartedMsg{Tasks: []runner.TaskRef{ref}},
		taskStartedMsg{Ref: ref, LogPath: "/tmp/fail.log"},
		taskFinishedMsg{Ref: ref, Result: runner.TaskResult{Outcome: runner.TaskFailed, Err: errors.New("kaboom")}},
	)
	rows := m.Rows()
	if len(rows) != 1 {
		t.Fatalf("Rows() len = %d, want 1", len(rows))
	}
	if rows[0].Status != "failed" {
		t.Errorf("Rows()[0].Status = %q, want failed", rows[0].Status)
	}
	if rows[0].LogPath != "/tmp/fail.log" {
		t.Errorf("Rows()[0].LogPath = %q, want /tmp/fail.log", rows[0].LogPath)
	}
	if rows[0].Err == nil {
		t.Errorf("Rows()[0].Err = nil, want non-nil")
	}
}
