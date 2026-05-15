package runner

// TaskRef identifies a task in EventSink callbacks. SpecRelpath is the
// spec directory relative to RepoRoot (forward slashes); Name is the
// command name from the spec.
type TaskRef struct {
	SpecRelpath string
	Name        string
}

// TaskOutcome enumerates how a task finished.
type TaskOutcome int

const (
	// TaskSucceeded means the cmd ran and the post-exec output check passed.
	TaskSucceeded TaskOutcome = iota
	// TaskSkipped means the fingerprint hit short-circuited the cmd.
	TaskSkipped
	// TaskFailed means the cmd or its surrounding bookkeeping returned an error.
	TaskFailed
)

// TaskResult is the payload of TaskFinished.
type TaskResult struct {
	Outcome TaskOutcome
	// Err is non-nil only for TaskFailed. Display layers format it; runner
	// continues to propagate the same error through errgroup so callers
	// observe the run-level outcome via Run().
	Err error
}

// Phase enumerates the pre-execution stages a run goes through before the
// first task can start. Each value corresponds to a discrete chunk of work
// in Run() — phase boundaries are where the user-visible progress display
// changes, so they're worth surfacing to EventSink even when no per-task
// callback fires in between.
type Phase int

const (
	// PhasePreflight runs registered Checkers (lockfile drift etc.).
	PhasePreflight Phase = iota
	// PhaseResolveInputs invokes each referenced tool's Inputs resolver.
	PhaseResolveInputs
	// PhaseResolveVersions invokes each referenced tool's Versions resolver.
	PhaseResolveVersions
	// PhasePlanning expands globs, derives the depgraph, and topo-orders tasks.
	PhasePlanning
	// PhasePrefetchFingerprints batch-loads cache records for the planned task set.
	PhasePrefetchFingerprints
	// PhaseRunningTasks is the transition into per-task execution. Once this
	// phase fires, the display layer can switch from a single-line "preparing"
	// indicator to a full task list (RunStarted follows immediately after).
	PhaseRunningTasks
)

// String returns a human-readable phase label. Stable across versions because
// display layers may render it verbatim.
func (p Phase) String() string {
	switch p {
	case PhasePreflight:
		return "preflight"
	case PhaseResolveInputs:
		return "resolving tool inputs"
	case PhaseResolveVersions:
		return "resolving tool versions"
	case PhasePlanning:
		return "planning task graph"
	case PhasePrefetchFingerprints:
		return "loading fingerprints"
	case PhaseRunningTasks:
		return "running tasks"
	}
	return "unknown"
}

// EventSink receives per-task lifecycle notifications. nil disables the hook
// (runner falls back to its built-in Logger output). When non-nil, runner
// suppresses its own "RUN" / "SKIP" log lines so the sink is the single
// source of truth and the display layer doesn't fight with stderr.
type EventSink interface {
	// PhaseChanged fires at the start of each pre-execution phase, plus once
	// more when the runner is about to start scheduling tasks (PhaseRunningTasks).
	// Display layers use it to render a "preparing: <phase>" line before
	// RunStarted arrives.
	PhaseChanged(phase Phase)

	// RunStarted fires once after the task list is finalised and before any
	// task is scheduled. tasks is in depgraph topo order so display layers
	// can lay out the list deterministically.
	RunStarted(tasks []TaskRef)

	// TaskStarted fires when runner is about to execute the cmd for ref.
	// logPath is the absolute path of the task log file (empty when
	// Options.LogDir is unset). Not called for fingerprint hits — those
	// produce TaskFinished with TaskSkipped directly.
	TaskStarted(ref TaskRef, logPath string)

	// TaskFinished fires after a task terminates (success, skip, or fail).
	TaskFinished(ref TaskRef, result TaskResult)
}
