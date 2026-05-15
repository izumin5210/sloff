package tui

import (
	"github.com/charmbracelet/lipgloss"
)

// styles bundles every lipgloss style the View uses. Held on the Model so
// tests can swap a profile-less variant when the test environment has no
// real terminal colorspace.
//
// The distinction between successGlyph (green check, default-coloured body)
// and skipped (whole row faint) is deliberate: succeeded tasks deserve a
// positive visual signal because the cmd actually executed, while skipped
// rows are background noise the user already implicitly trusts.
type styles struct {
	header       lipgloss.Style
	phase        lipgloss.Style
	pending      lipgloss.Style
	running      lipgloss.Style
	skipped      lipgloss.Style
	successGlyph lipgloss.Style
	failed       lipgloss.Style
	cursor       lipgloss.Style
	footer       lipgloss.Style
	pagerErr     lipgloss.Style
}

func defaultStyles() styles {
	return styles{
		header:       lipgloss.NewStyle().Bold(true),
		phase:        lipgloss.NewStyle().Foreground(lipgloss.Color("14")),
		pending:      lipgloss.NewStyle(),
		running:      lipgloss.NewStyle(),
		skipped:      lipgloss.NewStyle().Faint(true),
		successGlyph: lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true),
		failed:       lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true),
		cursor:       lipgloss.NewStyle().Bold(true),
		footer:       lipgloss.NewStyle().Faint(true),
		pagerErr:     lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
	}
}

// statusGlyph returns the leading symbol for a row in its current status.
// Pure function (no styling) so tests can assert glyph selection without
// having to thread an ANSI parser through the assertions.
func statusGlyph(s status, spinner string) string {
	switch s {
	case statusPending:
		return "·"
	case statusRunning:
		return spinner
	case statusSucceeded:
		return "✓"
	case statusSkipped:
		return "✓"
	case statusFailed:
		return "✗"
	}
	return "?"
}

// statusLabel appends an optional suffix like "(cached)" so skipped rows are
// distinguishable from succeeded ones without changing colour.
func statusLabel(s status) string {
	switch s {
	case statusSkipped:
		return " (cached)"
	}
	return ""
}
