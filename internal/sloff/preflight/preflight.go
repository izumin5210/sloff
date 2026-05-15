// Package preflight verifies that build artefacts and source state are mutually
// consistent before the runner trusts the fingerprint. Each distribution channel that
// can drift between SSoT and runtime ( e.g. pnpm-local's dist/ vs src/ for build-
// required tools) registers a Checker; the Registry runs the subset that applies
// to a given run. External-package channels are intentionally outside this scope:
// per ADR-0007 sloff absorbs them into the script resolver, where the runtime
// binary itself is the SSoT and lockfile drift cannot occur.
package preflight

import "context"

// Checker validates the install/build state of one distribution channel
// (e.g. pnpm-local). Implementations must be read-only. A Checker MAY also
// implement Fixer (see fixer.go) to opt in to runner-driven auto-remediation
// when drift is detected; that is the only sanctioned escape from the
// read-only rule, and it is granted only because Fix and Check are separate
// methods on a separate interface.
type Checker interface {
	// Name is the checker identifier (matches the corresponding resolver name).
	Name() string

	// Check returns the result for the given specDir. Issues describe lockfile/install
	// drift; an error indicates a failure to perform the check itself (IO, parse, etc.).
	Check(ctx context.Context, specDir string) (Result, error)
}

// Result is the outcome of a single Check or an aggregated Run.
type Result struct {
	OK     bool
	Issues []Issue
}

// Issue describes one specific lockfile/install drift item.
type Issue struct {
	Channel    string // e.g. "aqua"
	Detail     string // human-readable description of the drift
	Suggestion string // remediation command, e.g. "aqua install"
}
