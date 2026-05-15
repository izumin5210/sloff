package preflight

import "context"

// Fixer is an optional remediation capability that a Checker MAY also satisfy.
//
// Fix is invoked by the runner when Check reports drift and the run is not in
// ReadOnly mode (i.e. SLOFF_ALLOW_STALE_DEPS=1 is not set). Implementations
// MAY have side effects — spawning subprocesses, mutating files on disk — and
// this is the explicit distinction from Checker.Check, which the preflight
// package contract requires to be read-only.
//
// After Fix returns, the runner re-runs Check to confirm the drift was
// actually resolved. A non-nil error from Fix, or a re-Check that still
// reports drift, fails the run. The rationale for this protocol is captured
// in ADR-0013.
type Fixer interface {
	Fix(ctx context.Context, repoRoot string) error
}
