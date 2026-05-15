// Package tui renders `sloff run` progress as a bubbletea TUI when stdout
// is attached to a terminal. The Program receives runner lifecycle events
// through an EventSink implementation that fans them into Program.Send,
// keeping the runner goroutine ignorant of any display state.
//
// Architecture intent: the runner produces structured events; the TUI is the
// one piece of code that knows how to render them. Swapping the renderer
// (e.g. for structured JSON for an AI agent) is a matter of replacing this
// package's EventSink implementation — the runner doesn't change.
package tui

import (
	"context"
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/izumin5210/sloff/internal/sloff/runner"
)

// programSink adapts runner.EventSink to bubbletea Program messages. The
// Program pointer is set after tea.NewProgram by Run; sends issued before
// that point are dropped (which is safe: RunStarted is the first sink call
// and only fires after the goroutine starts, by which time the pointer is
// assigned).
type programSink struct {
	p *tea.Program
}

func (s *programSink) PhaseChanged(phase runner.Phase) {
	if s.p != nil {
		s.p.Send(phaseChangedMsg{Phase: phase})
	}
}

func (s *programSink) RunStarted(tasks []runner.TaskRef) {
	if s.p != nil {
		s.p.Send(runStartedMsg{Tasks: tasks})
	}
}

func (s *programSink) TaskStarted(ref runner.TaskRef, logPath string) {
	if s.p != nil {
		s.p.Send(taskStartedMsg{Ref: ref, LogPath: logPath})
	}
}

func (s *programSink) TaskFinished(ref runner.TaskRef, res runner.TaskResult) {
	if s.p != nil {
		s.p.Send(taskFinishedMsg{Ref: ref, Result: res})
	}
}

// Result is what Run returns to the caller. RunErr is the runner-level error
// (errgroup join), FailedTasks lists tasks whose final status was Failed so
// the CLI can print a post-quit summary with log paths.
type Result struct {
	RunErr      error
	FailedTasks []TaskSummary
}

// Run starts the TUI Program and invokes runFn in a goroutine with an
// EventSink wired to the Program. Returns once both (a) runFn has returned
// and (b) tea.Program.Run has unwound (auto-quit on runFinishedMsg).
//
// runFn must call sink methods only while it owns the run; once runFn
// returns, the Program quits and any further sink calls are no-ops because
// the Program is shut down.
//
// ctx is propagated to runFn. The Program also watches ctx via
// tea.WithContext, so an external SIGINT (forwarded into ctx by the caller)
// cancels both halves consistently.
func Run(ctx context.Context, runFn func(context.Context, runner.EventSink) error) (Result, error) {
	// Cancel scope is local so the Quit-key path can stop the runner
	// without tearing down the parent ctx. The parent ctx still cascades
	// in via context.WithCancel, so SIGINT in the caller still drains.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	model := NewModel(cancelRun)
	sink := &programSink{}
	prog := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithContext(ctx),
		// The caller is expected to wire SIGINT / SIGTERM into ctx
		// (signal.NotifyContext). Letting bubbletea also install handlers
		// double-attaches them and the second registration wins on some
		// shells, which dropped Ctrl+C handling in development. Disable
		// the internal handler and rely on ctx.
		tea.WithoutSignalHandler(),
	)
	sink.p = prog

	runErrCh := make(chan error, 1)
	go func() {
		// Always emit runFinishedMsg so the Program quits even if runFn
		// panics in a way recover can't catch — at the very least, ctx
		// cancellation will propagate and the Program tears down.
		err := runFn(runCtx, sink)
		runErrCh <- err
		prog.Send(runFinishedMsg{Err: err})
	}()

	finalModel, progErr := prog.Run()
	runErr := <-runErrCh

	res := Result{RunErr: runErr}
	if fm, ok := finalModel.(Model); ok {
		for _, row := range fm.Rows() {
			if row.Status == "failed" {
				res.FailedTasks = append(res.FailedTasks, row)
			}
		}
	}
	return res, joinErrs(progErr, runErr)
}

// joinErrs flattens the tea.Program error and the runner error into a
// single value. tea returning ErrProgramKilled (signal-driven quit) under
// a successful run is treated as nil to avoid masking the actual outcome.
func joinErrs(progErr, runErr error) error {
	if progErr != nil && !errors.Is(progErr, tea.ErrProgramKilled) {
		if runErr != nil {
			return fmt.Errorf("tui: %w; run: %w", progErr, runErr)
		}
		return progErr
	}
	return runErr
}
