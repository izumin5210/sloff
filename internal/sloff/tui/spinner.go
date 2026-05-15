package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// spinnerFrames is the braille spinner cycle. Time-based frame selection
// (see spinnerFrame) keeps every concurrently-running task in phase, which
// looks calmer than each row owning its own counter.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinnerInterval = 100 * time.Millisecond

// spinnerFrame returns the frame to render at instant now. Pure function of
// the clock so different rows in the same render snapshot all advance in
// lock-step, which is the property that makes the parallel run feel quiet
// rather than choppy.
func spinnerFrame(now time.Time) string {
	if now.IsZero() {
		return spinnerFrames[0]
	}
	idx := int(now.UnixMilli()/int64(spinnerInterval/time.Millisecond)) % len(spinnerFrames)
	if idx < 0 {
		idx += len(spinnerFrames)
	}
	return spinnerFrames[idx]
}

type spinnerTickMsg time.Time

// spinnerTickCmd schedules the next spinner repaint. The Model re-issues it
// from Update only while at least one task is still running, so the Program
// goes idle once the run finishes.
func spinnerTickCmd() tea.Cmd {
	return tea.Tick(spinnerInterval, func(t time.Time) tea.Msg {
		return spinnerTickMsg(t)
	})
}
