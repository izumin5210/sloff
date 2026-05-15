package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// errNoPager signals that neither $PAGER nor `less` was usable.
// Surfaced inline in the Model footer; never propagates to the run-level
// error so a missing pager doesn't fail an otherwise green run.
var errNoPager = errors.New("pager not found")

// resolvePagerCommand decides what process to spawn for viewing logPath.
// Resolution order matches ADR-0013:
//  1. $PAGER (if set, used verbatim with logPath appended)
//  2. `less -R +F` with hint prompts at the footer
//  3. error (caller renders inline)
//
// Split into a pure function so the resolution logic is testable without
// shelling out.
func resolvePagerCommand(env func(string) string, lookPath func(string) (string, error), logPath string) ([]string, error) {
	if env == nil {
		env = os.Getenv
	}
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if pager := strings.TrimSpace(env("PAGER")); pager != "" {
		// $PAGER may be a bare binary name (`less`) or a command with
		// flags (`less -R`). The shell-quoting story for the latter is
		// fragile, so we deliberately keep it simple: split on
		// whitespace and treat the first token as the program. Users
		// who need quoted args should set $PAGER to a wrapper script.
		fields := strings.Fields(pager)
		return append(fields, logPath), nil
	}
	if _, err := lookPath("less"); err == nil {
		return []string{
			"less",
			"-R",
			"+F",
			`-Pw^C\: pause to scroll/search/quit  (following)`,
			`-Psq\: quit  /\: search  G\: bottom  F\: follow`,
			logPath,
		}, nil
	}
	return nil, errNoPager
}

// openPagerCmd returns a tea.Cmd that hands the terminal off to the resolved
// pager via tea.ExecProcess. bubbletea suspends its altscreen for the
// duration; when the pager exits, the Program redraws.
//
// We open the file ourselves first only to confirm it exists; the pager then
// opens its own descriptor. This avoids a confusing "less spins forever on
// a missing file" state when LogDir was set but the task hasn't actually
// started yet.
func openPagerCmd(logPath string) tea.Cmd {
	if logPath == "" {
		return func() tea.Msg { return pagerFinishedMsg{Err: fmt.Errorf("no log file for this task")} }
	}
	if _, err := os.Stat(logPath); err != nil {
		return func() tea.Msg { return pagerFinishedMsg{Err: err} }
	}
	args, err := resolvePagerCommand(nil, nil, logPath)
	if err != nil {
		return func() tea.Msg { return pagerFinishedMsg{Err: err} }
	}
	cmd := exec.Command(args[0], args[1:]...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return pagerFinishedMsg{Err: err}
	})
}
