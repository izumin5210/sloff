package tui

import "github.com/izumin5210/sloff/internal/sloff/runner"

// phaseChangedMsg surfaces a pre-execution phase transition (preflight,
// resolving versions, …) so the Model can render a one-line "preparing"
// indicator above the (still-empty) task list.
type phaseChangedMsg struct {
	Phase runner.Phase
}

// runStartedMsg fans the EventSink.RunStarted payload into the Program's
// message loop. The Model uses it to seed its row list in depgraph order.
type runStartedMsg struct {
	Tasks []runner.TaskRef
}

// taskStartedMsg flips a row from pending to running and stores the log path
// the user can later open with `l`.
type taskStartedMsg struct {
	Ref     runner.TaskRef
	LogPath string
}

// taskFinishedMsg flips a row to its terminal status. The Model decides
// whether to gray it out (success / skip) or paint it red (fail) based on
// runner.TaskResult.Outcome.
type taskFinishedMsg struct {
	Ref    runner.TaskRef
	Result runner.TaskResult
}

// runFinishedMsg signals that the runner goroutine has returned. The Model
// records the run-level error (if any) and triggers tea.Quit so the Program
// exits without further user input — per ADR-0013, the TUI always auto-quits.
type runFinishedMsg struct {
	Err error
}

// pagerFinishedMsg fires after tea.ExecProcess returns control to the
// Program. A non-nil Err is rendered inline (e.g. "pager not found"); the
// run itself is unaffected.
type pagerFinishedMsg struct {
	Err error
}
