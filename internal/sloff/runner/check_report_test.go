package runner_test

import (
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/runner"
)

// TestCheckReport_CleanAndDriftSemantics pins the two report views against
// each other (ADR-0021):
//
//   - Clean is the strict "every task hit" verdict.
//   - Drift is the exit-1 view: concrete drift plus unverifiable results
//     whose tool producers drifted; environment-classified unverifiable
//     results are excluded because their cause travels via Check's error
//     (exit 2).
//
// The env-only case is the triangulating one: Drift must be empty while
// Clean must still be false — a report that could not verify every task is
// never clean, no matter how its failures were classified.
func TestCheckReport_CleanAndDriftSemantics(t *testing.T) {
	ok := runner.CheckResult{SpecRelpath: "spec", Task: "a", Status: runner.CheckOK}
	noRecord := runner.CheckResult{SpecRelpath: "spec", Task: "b", Status: runner.CheckNoRecord}
	unverifiableDrift := runner.CheckResult{
		SpecRelpath: "spec", Task: "c", Status: runner.CheckUnverifiable,
		Tool: "gen-tool", ToolProducersDrifted: true,
	}
	unverifiableEnv := runner.CheckResult{
		SpecRelpath: "spec", Task: "d", Status: runner.CheckUnverifiable,
		Tool: "gen-tool",
	}

	cases := []struct {
		name      string
		results   []runner.CheckResult
		wantClean bool
		wantDrift int
	}{
		{name: "empty", results: nil, wantClean: true, wantDrift: 0},
		{name: "all ok", results: []runner.CheckResult{ok}, wantClean: true, wantDrift: 0},
		{name: "concrete drift", results: []runner.CheckResult{ok, noRecord}, wantClean: false, wantDrift: 1},
		{name: "unverifiable with drifted producers", results: []runner.CheckResult{ok, unverifiableDrift}, wantClean: false, wantDrift: 1},
		{name: "unverifiable env-classified only", results: []runner.CheckResult{ok, unverifiableEnv}, wantClean: false, wantDrift: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := &runner.CheckReport{Results: tc.results}
			if got := rep.Clean(); got != tc.wantClean {
				t.Errorf("Clean() = %v, want %v", got, tc.wantClean)
			}
			if got := len(rep.Drift()); got != tc.wantDrift {
				t.Errorf("len(Drift()) = %d, want %d", got, tc.wantDrift)
			}
		})
	}
}
