package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/izumin5210/sloff/internal/sloff/runner"
)

// status enumerates the row-level states the TUI cares about. It mirrors
// runner.TaskOutcome plus the transient `pending` / `running` states the
// outcome enum doesn't express (since runner only reports terminals).
type status int

const (
	statusPending status = iota
	statusRunning
	statusSucceeded
	statusSkipped
	statusFailed
)

type taskRow struct {
	ref     runner.TaskRef
	status  status
	err     error
	logPath string
}

// Model is the bubbletea model for `sloff run`. Held by tea.Program; the
// EventSink implementation pushes msgs into the same Program so the runner
// goroutine never touches Model fields directly.
type Model struct {
	rows       []taskRow
	indexByRef map[runner.TaskRef]int
	cursor     int
	keys       keyMap
	styles     styles
	now        time.Time

	phase    runner.Phase
	phaseSet bool // distinguishes "no phase seen yet" from PhasePreflight (zero value)

	runDone   bool
	runErr    error
	pagerErr  error
	quitting  bool
	cancelRun func()
}

// NewModel builds the initial Model. cancelRun is invoked when the user
// hits q / Ctrl+C so the runner goroutine stops promptly; the Program does
// not call tea.Quit until runFinishedMsg arrives, so cancellation and shutdown
// stay ordered (no goroutine leak after Run returns).
func NewModel(cancelRun func()) Model {
	return Model{
		indexByRef: map[runner.TaskRef]int{},
		keys:       defaultKeyMap(),
		styles:     defaultStyles(),
		cancelRun:  cancelRun,
	}
}

// Init seeds the initial commands: spinner ticks so a running row repaints
// even between events.
func (m Model) Init() tea.Cmd {
	return spinnerTickCmd()
}

// Update is the bubbletea reducer. Returns the new Model and (optionally) a
// follow-up Cmd. Splitting per-msg case bodies into small handlers keeps
// each path small enough to reason about under concurrent event delivery.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case phaseChangedMsg:
		m.phase = msg.Phase
		m.phaseSet = true
		return m, nil
	case runStartedMsg:
		return m.handleRunStarted(msg), nil
	case taskStartedMsg:
		return m.handleTaskStarted(msg), nil
	case taskFinishedMsg:
		return m.handleTaskFinished(msg), nil
	case runFinishedMsg:
		m.runDone = true
		m.runErr = msg.Err
		return m, tea.Quit
	case pagerFinishedMsg:
		m.pagerErr = msg.Err
		return m, nil
	case spinnerTickMsg:
		m.now = time.Time(msg)
		// Re-issue the tick unconditionally until the Program quits.
		// Gating on hasRunning() looks like an optimisation but it
		// drops the tick before the first TaskStarted lands, so the
		// spinner never advances — we'd then need a separate path to
		// resume ticking on TaskStarted. Repainting every spinnerInterval
		// is cheap (bubbletea diff-renders), so just keep the loop running.
		return m, spinnerTickCmd()
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.WindowSizeMsg:
		return m, nil
	}
	return m, nil
}

func (m Model) handleRunStarted(msg runStartedMsg) Model {
	m.rows = make([]taskRow, len(msg.Tasks))
	m.indexByRef = make(map[runner.TaskRef]int, len(msg.Tasks))
	for i, ref := range msg.Tasks {
		m.rows[i] = taskRow{ref: ref, status: statusPending}
		m.indexByRef[ref] = i
	}
	if m.cursor >= len(m.rows) {
		m.cursor = 0
	}
	return m
}

func (m Model) handleTaskStarted(msg taskStartedMsg) Model {
	if idx, ok := m.indexByRef[msg.Ref]; ok {
		m.rows[idx].status = statusRunning
		m.rows[idx].logPath = msg.LogPath
	}
	return m
}

func (m Model) handleTaskFinished(msg taskFinishedMsg) Model {
	idx, ok := m.indexByRef[msg.Ref]
	if !ok {
		return m
	}
	switch msg.Result.Outcome {
	case runner.TaskSucceeded:
		m.rows[idx].status = statusSucceeded
	case runner.TaskSkipped:
		m.rows[idx].status = statusSkipped
	case runner.TaskFailed:
		m.rows[idx].status = statusFailed
		m.rows[idx].err = msg.Result.Err
	}
	return m
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		// Don't tea.Quit synchronously: the runner goroutine still needs
		// to drain. Cancel its context and wait for runFinishedMsg to
		// arrive — that handler is what calls tea.Quit. The intermediate
		// state is rendered as "cancelling..." in the footer.
		m.quitting = true
		if m.cancelRun != nil {
			m.cancelRun()
		}
		return m, nil
	case key.Matches(msg, m.keys.Up):
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case key.Matches(msg, m.keys.Down):
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
		return m, nil
	case key.Matches(msg, m.keys.Top):
		m.cursor = 0
		return m, nil
	case key.Matches(msg, m.keys.Bottom):
		if len(m.rows) > 0 {
			m.cursor = len(m.rows) - 1
		}
		return m, nil
	case key.Matches(msg, m.keys.LogFile):
		if m.cursor >= len(m.rows) {
			return m, nil
		}
		m.pagerErr = nil
		return m, openPagerCmd(m.rows[m.cursor].logPath)
	}
	return m, nil
}

// View renders the current Model as a string. Kept allocation-light because
// bubbletea re-renders on every msg; doing string formatting here rather
// than in helpers also makes Snapshot-style tests easier to read.
func (m Model) View() string {
	var b strings.Builder
	b.WriteString(m.styles.header.Render("sloff run"))
	b.WriteString("\n")

	spin := spinnerFrame(m.now)

	// Pre-execution phase indicator. Stays visible while the runner is still
	// in preflight / resolver / fingerprint-prefetch work and disappears as
	// soon as the task list seeds (RunStarted, which arrives just after
	// PhaseRunningTasks). Once the run is finished we suppress it entirely
	// to keep the post-run summary tidy.
	if m.phaseSet && !m.runDone && (m.phase != runner.PhaseRunningTasks || len(m.rows) == 0) {
		fmt.Fprintf(&b, "  %s %s\n", spin, m.styles.phase.Render(m.phase.String()))
	}
	b.WriteString("\n")

	for i, r := range m.rows {
		prefix := "  "
		if i == m.cursor {
			prefix = m.styles.cursor.Render("▸ ")
		}
		glyph := statusGlyph(r.status, spin)
		label := taskLabel(r.ref) + statusLabel(r.status)
		b.WriteString(prefix)
		b.WriteString(m.renderRow(r.status, glyph, label))
		b.WriteByte('\n')
	}

	b.WriteByte('\n')
	footer := "↑/↓ move  l view log  q quit"
	if m.quitting && !m.runDone {
		footer = "cancelling… (waiting for tasks to stop)"
	}
	b.WriteString(m.styles.footer.Render(footer))
	if m.pagerErr != nil {
		b.WriteByte('\n')
		b.WriteString(m.styles.pagerErr.Render(fmt.Sprintf("pager: %v", m.pagerErr)))
	}
	return b.String()
}

// renderRow returns the styled "<glyph> <label>" for one row. Succeeded rows
// get a green glyph but a default-coloured body so the positive signal stays
// readable against any terminal theme. Skipped rows are entirely faint —
// the cmd didn't actually execute, the row is informational.
func (m Model) renderRow(s status, glyph, label string) string {
	switch s {
	case statusFailed:
		return m.styles.failed.Render(glyph + " " + label)
	case statusSucceeded:
		return m.styles.successGlyph.Render(glyph) + " " + label
	case statusSkipped:
		return m.styles.skipped.Render(glyph + " " + label)
	case statusRunning:
		return m.styles.running.Render(glyph + " " + label)
	case statusPending:
		return m.styles.pending.Render(glyph + " " + label)
	}
	return glyph + " " + label
}

func taskLabel(ref runner.TaskRef) string {
	if ref.SpecRelpath == "" {
		return ref.Name
	}
	return ref.SpecRelpath + ":" + ref.Name
}

// RunErr returns the run-level error captured from runFinishedMsg, if any.
// Used by callers after tea.Program.Run() returns so the caller can decide
// whether to set a non-zero exit code or print a stderr summary.
func (m Model) RunErr() error { return m.runErr }

// Rows returns a copy of the rows for inspection (used in tests and by the
// caller for the post-run failure summary printed to stderr).
func (m Model) Rows() []TaskSummary {
	out := make([]TaskSummary, len(m.rows))
	for i, r := range m.rows {
		out[i] = TaskSummary{
			Ref:     r.ref,
			Status:  r.status.String(),
			LogPath: r.logPath,
			Err:     r.err,
		}
	}
	return out
}

// TaskSummary is the post-run view of a row. Stable shape exported so the
// CLI layer can print failure lines without depending on internal Model
// fields.
type TaskSummary struct {
	Ref     runner.TaskRef
	Status  string
	LogPath string
	Err     error
}

func (s status) String() string {
	switch s {
	case statusPending:
		return "pending"
	case statusRunning:
		return "running"
	case statusSucceeded:
		return "succeeded"
	case statusSkipped:
		return "skipped"
	case statusFailed:
		return "failed"
	}
	return "unknown"
}
