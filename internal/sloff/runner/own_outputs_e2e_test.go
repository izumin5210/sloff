package runner_test

import "testing"

// TestRunner_PnpmLocal_OwnOutputsExcluded locks ADR-0022: when a task's
// pnpm-local tool is its own package, the resolver contributes the task's own
// committed outputs. Those must not reach the input hash, or a run that
// rewrites them records a key the post-run tree no longer matches — one run
// would leave check reporting drift and the next run would execute again.
func TestRunner_PnpmLocal_OwnOutputsExcluded(t *testing.T) {
	runE2E(
		t, "pnpmlocal-own-outputs-excluded",
		gitCommitStep(),
		runStep(expectInfo("RUN  packages/app/gen")),
		checkStep(expectCheckDriftTasks()),
		runStep(expectInfo("SKIP packages/app/gen"), expectNoInfoContaining("RUN ")),
	)
}

// TestRunner_DeclaredInputsOverlapOwnOutputs covers the hand-written side of
// ADR-0022: a declared inputs glob that also matches the task's own outputs is
// excluded the same way, but warned about because it is usually a spec mistake.
func TestRunner_DeclaredInputsOverlapOwnOutputs(t *testing.T) {
	runE2E(
		t, "declared-inputs-overlap-own-outputs",
		runStep(expectWarn("gen/out.txt")),
		checkStep(expectCheckDriftTasks()),
		runStep(expectInfo("SKIP ./gen"), expectNoInfoContaining("RUN ")),
	)
}

// TestRunner_PnpmLocal_OwnOutputsExcludedDeferred covers ADR-0022 D1 on the
// deferred-tool path: when the task's own-package tool is deferred on a cold
// run (ADR-0019 D4), the exec-time input rebuild must drop the task's own
// outputs too, so the cold-written record hits on warm runs (ADR-0019 D7).
func TestRunner_PnpmLocal_OwnOutputsExcludedDeferred(t *testing.T) {
	requireSh(t)
	runE2E(
		t, "pnpmlocal-own-outputs-excluded-deferred",
		runStep(
			expectWarn("deferred until its declared depends complete"),
			expectInfo(`resolved tool "app-pkg" after its declared depends completed`),
		),
		checkStep(expectCheckDriftTasks()),
		runStep(expectNoWarns(), expectNoInfoContaining("RUN ")),
	)
}
