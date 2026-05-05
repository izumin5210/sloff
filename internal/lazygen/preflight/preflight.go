// Package preflight verifies that lockfiles and installed dependencies are mutually
// consistent before the runner trusts the cache. Each distribution channel registers a
// Checker; the Registry runs the subset that applies to a given run.
package preflight

import "context"

// Checker validates the install state of one distribution channel (aqua / go-external /
// pnpm-external / ...). Implementations must be read-only.
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
