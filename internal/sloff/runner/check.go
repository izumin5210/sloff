package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/depgraph"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	"github.com/izumin5210/sloff/internal/sloff/hash"
	"github.com/izumin5210/sloff/internal/sloff/spec"
)

// CheckStatus classifies one task's drift-verification outcome (ADR-0021).
type CheckStatus int

const (
	// CheckOK: a record exists for the current input_hash and the recorded
	// output files hash to the recorded value — the committed tree matches
	// what an honest run produced for these inputs.
	CheckOK CheckStatus = iota
	// CheckNoRecord: no record exists for the current input_hash. Either the
	// generator was not re-run after an input change, or it was run but the
	// record was not committed; the two are indistinguishable here.
	CheckNoRecord
	// CheckOutputMismatch: a record exists but at least one recorded output
	// file is missing from the tree or hashes differently.
	CheckOutputMismatch
	// CheckInputMissing: an input file (declared or tool-contributed) is
	// absent from the tree, so the input_hash cannot be computed. Typically
	// an upstream task's output that was never committed — that producer is
	// reported as drift in the same run.
	CheckInputMissing
	// CheckUnverifiable: a referenced tool could not be resolved, so the
	// input_hash cannot be computed. Check classifies the cause via the
	// tool's depends producers: when any of them drifts the whole report
	// counts as drift; when all are clean Check returns an error instead
	// (environment problem, not missing generation).
	CheckUnverifiable
)

func (s CheckStatus) String() string {
	switch s {
	case CheckOK:
		return "ok"
	case CheckNoRecord:
		return "no-record"
	case CheckOutputMismatch:
		return "output-mismatch"
	case CheckInputMissing:
		return "input-missing"
	case CheckUnverifiable:
		return "unverifiable"
	}
	return fmt.Sprintf("CheckStatus(%d)", int(s))
}

// isDrift reports whether the status is concrete evidence that the tree does
// not match its records. CheckUnverifiable is intentionally excluded: it is
// an absence of evidence, and classifyToolFailures decides whether it rides
// along with producer drift or escalates to a Check error.
func (s CheckStatus) isDrift() bool {
	switch s {
	case CheckNoRecord, CheckOutputMismatch, CheckInputMissing:
		return true
	}
	return false
}

// CheckFileIssue is the per-file detail of a CheckOutputMismatch result.
type CheckFileIssue struct {
	// Path is the repo-relative slash-form path as recorded in the record.
	Path string
	// Reason is "missing", "modified", or "unreadable".
	Reason string
}

// CheckResult is one task's verification outcome.
type CheckResult struct {
	SpecRelpath string
	Task        string
	Status      CheckStatus

	// OutputIssues lists the recorded output files that no longer match
	// (CheckOutputMismatch only).
	OutputIssues []CheckFileIssue
	// MissingInputs lists the repo-relative slash-form input paths absent
	// from the tree (CheckInputMissing only).
	MissingInputs []string
	// Tool names the unresolvable tool (CheckUnverifiable only).
	Tool string
	// ToolProducersDrifted reports, for CheckUnverifiable, whether the tool's
	// depends closure showed concrete drift: true means missing generation
	// explains the failure (the result counts as drift), false means every
	// producer is clean and Check surfaced the cause as an error instead
	// (environment problem, CLI exit 2). Set by classifyToolFailures.
	ToolProducersDrifted bool
}

// CheckReport is the outcome of Runner.Check for every non-barrier task, in
// scheduling order.
type CheckReport struct {
	Results []CheckResult
}

// Drift returns the results that represent drift: every concrete non-ok
// status, plus unverifiable results whose tool's depends closure drifted.
// Environment-classified unverifiable results are excluded — Check reported
// their cause as an error (CLI exit 2), and listing them as drift would
// misdirect the user to `sloff run`.
func (rep *CheckReport) Drift() []CheckResult {
	var out []CheckResult
	for _, res := range rep.Results {
		switch {
		case res.Status == CheckOK:
		case res.Status == CheckUnverifiable && !res.ToolProducersDrifted:
		default:
			out = append(out, res)
		}
	}
	return out
}

// Clean reports whether every task verified as a fingerprint hit.
func (rep *CheckReport) Clean() bool { return len(rep.Drift()) == 0 }

// Check verifies that the current tree matches its committed fingerprint
// records without executing any generator (ADR-0021). It runs the same
// planning phases as Run — provider expansion, tool resolution, preflight,
// plan-time overlap validation — then evaluates every non-barrier task's
// fingerprint hit decision read-only. No record is written or collapsed and
// the tree is never mutated; the only side effect is the host-local per-file
// digest cache (ADR-0014), which Run shares.
//
// Preflight is strict: the SLOFF_ALLOW_STALE_DEPS degrade path (Options.
// ReadOnly) is ignored, because a verification command whose guarantees can
// be weakened by an env var is not a verification command.
//
// The returned error means the check itself could not run (spec / tool /
// preflight / storage problems — CLI exit 2); drift is reported through the
// CheckReport (CLI exit 1). Both can be non-nil together: a partial report
// accompanies an unverifiable-tool error so the caller can still show the
// drift it did find.
func (r *Runner) Check(ctx context.Context) (rep *CheckReport, err error) {
	// Same background storage warm-up as Run (remote backends only).
	if w, ok := r.opts.Storage.(interface{ Warm(context.Context) error }); ok {
		go func() { _ = w.Warm(ctx) }()
	}
	// Persist the per-file digest cache like Run does: check hashes the same
	// files a run would, so a CI job that caches XDG_CACHE_HOME gets
	// incremental hashing on the next check.
	defer func() { _ = r.fileCache.Save() }()

	if err := r.expandCommandProviders(ctx); err != nil {
		return nil, err
	}
	if err := r.expandDependPatterns(ctx); err != nil {
		return nil, err
	}
	registry, referencedToolNames, err := r.prepareRegistry()
	if err != nil {
		return nil, err
	}
	if err := r.preflightPass(ctx, registry, referencedToolNames, true); err != nil {
		return nil, err
	}
	if err := r.resolveContribs(ctx, registry, referencedToolNames); err != nil {
		return nil, err
	}
	tasks, err := r.collectTasksTraced(ctx, r.inputsByTool, r.versionsByTool)
	if err != nil {
		return nil, err
	}
	ordered, err := r.depgraphBuildTraced(ctx, tasks)
	if err != nil {
		return nil, err
	}
	if missing := r.findMissingDependenciesTraced(ctx, ordered); len(missing) > 0 {
		return nil, depgraph.MissingDependenciesError(missing)
	}

	// ADR-0019 deferred tools resolve "after their depends complete" in Run;
	// check never executes producers, so retry immediately against the
	// committed tree and latch the outcome before task evaluation.
	toolFailures := r.resolveDeferredForCheck(ctx)

	if err := r.prefetchFingerprints(ctx, ordered); err != nil {
		return nil, err
	}
	results, err := r.checkTasks(ctx, ordered)
	if err != nil {
		return nil, err
	}
	rep = &CheckReport{Results: results}
	if len(toolFailures) > 0 {
		if envErr := r.classifyToolFailures(ordered, results, toolFailures); envErr != nil {
			return rep, envErr
		}
	}
	return rep, nil
}

// resolveDeferredForCheck retries every deferred tool once against the
// current tree and latches the outcome on the deferredTool, so the per-task
// evaluation (and ensureToolsResolved's fast path) observes a definitive
// result. Sequential on purpose: this only runs on drifted or broken trees,
// and a stable name order keeps spans and error output deterministic.
func (r *Runner) resolveDeferredForCheck(ctx context.Context) map[string]error {
	if len(r.deferredTools) == 0 {
		return nil
	}
	failures := map[string]error{}
	for _, name := range slices.Sorted(maps.Keys(r.deferredTools)) {
		dt := r.deferredTools[name]
		ins, vs, span, _, retErr := r.resolveToolContrib(ctx, dt.entry, "check", true)
		var outcome error
		if retErr != nil {
			outcome = fmt.Errorf("tool %q (defined in %s) could not be resolved: at plan time: %v; retried at check time: %w",
				dt.entry.Name, providerDefinitionPath(dt.entry.SpecDir), dt.planErr, retErr)
			failures[name] = outcome
		}
		spanErr := outcome
		endSpan(span, &spanErr)
		dt.mu.Lock()
		dt.inputs, dt.versions = ins, vs
		dt.err = outcome
		dt.done = true
		dt.mu.Unlock()
	}
	return failures
}

// checkTasks evaluates every non-barrier task concurrently. Evaluation only
// observes the tree, so unlike runTasks no dependency ordering is needed.
func (r *Runner) checkTasks(ctx context.Context, ordered []depgraph.Task) (results []CheckResult, err error) {
	real := make([]depgraph.Task, 0, len(ordered))
	for _, t := range ordered {
		if !t.Barrier {
			real = append(real, t)
		}
	}
	ctx, span := r.tracer.Start(ctx, "runner.tasks.check", trace.WithAttributes(
		attribute.Int("sloff.task.count", len(real)),
	))
	defer endSpan(span, &err)

	results = make([]CheckResult, len(real))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(taskConcurrency(len(real)))
	for i, t := range real {
		g.Go(func() error {
			res, cerr := r.checkTask(gctx, t)
			if cerr != nil {
				return cerr
			}
			results[i] = res
			return nil
		})
	}
	if err = g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// checkTask is runTask's hit-decision path without the execute/write half:
// compute the input_hash, look up the record, compare recorded outputs
// against the tree, and classify.
func (r *Runner) checkTask(ctx context.Context, t depgraph.Task) (res CheckResult, err error) {
	info := r.byKey[t.Ref()]
	res = CheckResult{SpecRelpath: t.SpecRelpath, Task: t.Name}

	ctx, span := r.tracer.Start(ctx, "runner.task.check", trace.WithAttributes(
		attribute.String("sloff.spec", t.SpecRelpath),
		attribute.String("sloff.task.name", t.Name),
	))
	defer func() {
		span.SetAttributes(attribute.String("sloff.check.status", res.Status.String()))
		endSpan(span, &err)
	}()

	if len(r.deferredTools) > 0 {
		for _, name := range info.command.Tools {
			dt, ok := r.deferredTools[name]
			if !ok {
				continue
			}
			dt.mu.Lock()
			failed := dt.err != nil
			dt.mu.Unlock()
			if failed {
				res.Status = CheckUnverifiable
				res.Tool = name
				return res, nil
			}
		}
		// Every deferred tool this task references resolved at check time;
		// rebuild the input surface exactly like the exec path would
		// (ADR-0019 D7 key equivalence).
		if rerr := r.ensureToolsResolved(ctx, t, info); rerr != nil {
			return res, rerr
		}
	}

	filesHash, hashErr := r.fileCache.Files(r.opts.RepoRoot, info.inputPaths)
	if hashErr != nil {
		if errors.Is(hashErr, fs.ErrNotExist) {
			res.Status = CheckInputMissing
			res.MissingInputs = missingInputPaths(r.opts.RepoRoot, info.inputPaths)
			return res, nil
		}
		return res, fmt.Errorf("%s: hash inputs: %w", t.Name, hashErr)
	}
	cmdHash := hash.Cmd(info.command.Cmd)
	resolvedVersionsHash := hash.ResolvedVersions(versionStrings(info.versions))
	inputHash := hash.Input(filesHash, cmdHash, resolvedVersionsHash)
	span.SetAttributes(attribute.String("sloff.input.hash", inputHashAttr(inputHash)))

	key := fingerprint.Key{SpecRelpath: t.SpecRelpath, TaskID: t.Name, InputHash: inputHash}
	hit, existing, _, lookErr := r.fingerprintLookup(ctx, key)
	if lookErr != nil {
		return res, fmt.Errorf("%s: load record: %w", t.Name, lookErr)
	}
	switch {
	case hit:
		res.Status = CheckOK
	case existing == nil:
		res.Status = CheckNoRecord
	default:
		res.Status = CheckOutputMismatch
		res.OutputIssues = r.diffRecordedOutputs(existing)
	}
	return res, nil
}

// diffRecordedOutputs lists which recorded output files stopped matching the
// tree. The hit decision already failed on the folded hash; this re-walks
// the recorded entries so the report can name the concrete files.
func (r *Runner) diffRecordedOutputs(rec *fingerprintv1.Record) []CheckFileIssue {
	var issues []CheckFileIssue
	for _, f := range rec.GetOutput().GetFiles() {
		rel := filepath.FromSlash(f.GetPath())
		if _, statErr := os.Stat(filepath.Join(r.opts.RepoRoot, rel)); errors.Is(statErr, fs.ErrNotExist) {
			issues = append(issues, CheckFileIssue{Path: f.GetPath(), Reason: "missing"})
			continue
		}
		hex, hashErr := r.fileCache.FileHex(r.opts.RepoRoot, rel)
		switch {
		case hashErr != nil:
			issues = append(issues, CheckFileIssue{Path: f.GetPath(), Reason: "unreadable"})
		case hex != f.GetHash():
			issues = append(issues, CheckFileIssue{Path: f.GetPath(), Reason: "modified"})
		}
	}
	return issues
}

// missingInputPaths stats each input path and returns the absent ones in
// slash form. Only called on the already-failed ErrNotExist path, so the
// extra stat pass costs nothing on healthy trees.
func missingInputPaths(root string, paths []string) []string {
	var missing []string
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(root, p)); errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, filepath.ToSlash(p))
		}
	}
	return missing
}

// classifyToolFailures decides, per unresolvable tool, whether the failure
// is explained by drift (some task in the tool's depends closure is not
// generated/committed — the unverifiable consumers then count as drift) or
// is an environment problem (every producer clean — escalate to an error so
// the CLI exits 2 instead of misdirecting the user to `sloff run`). The
// verdict is written back onto each unverifiable result so report consumers
// can render the two cases distinctly.
func (r *Runner) classifyToolFailures(ordered []depgraph.Task, results []CheckResult, failures map[string]error) error {
	statusByRef := make(map[depgraph.TaskRef]CheckStatus, len(results))
	for _, res := range results {
		statusByRef[depgraph.TaskRef{SpecRelpath: res.SpecRelpath, Name: res.Task}] = res.Status
	}
	byRef := make(map[depgraph.TaskRef]depgraph.Task, len(ordered))
	for _, t := range ordered {
		byRef[t.Ref()] = t
	}

	var envErrs []error
	driftedByTool := make(map[string]bool, len(failures))
	for _, name := range slices.Sorted(maps.Keys(failures)) {
		entry := r.deferredTools[name].entry
		drifted := toolProducersDrifted(entry, byRef, statusByRef)
		driftedByTool[name] = drifted
		if !drifted {
			envErrs = append(envErrs, fmt.Errorf("%w; every task in the tool's declared depends closure is clean, so this is not missing generation — fix the tool's sources or the environment", failures[name]))
		}
	}
	for i := range results {
		if results[i].Status == CheckUnverifiable {
			results[i].ToolProducersDrifted = driftedByTool[results[i].Tool]
		}
	}
	return errors.Join(envErrs...)
}

// toolProducersDrifted walks the tool's declared depends and their transitive
// dependencies, looking for concrete drift. Barriers appear in byRef but not
// in statusByRef; they are traversed, never judged.
func toolProducersDrifted(entry spec.ToolEntry, byRef map[depgraph.TaskRef]depgraph.Task, statusByRef map[depgraph.TaskRef]CheckStatus) bool {
	dirSlash := filepath.ToSlash(entry.SpecDir)
	queue := make([]depgraph.TaskRef, 0, len(entry.Declared.Depends))
	for _, d := range entry.Declared.Depends {
		// Same path arithmetic as resolveDepends / injectToolDepends: tool
		// depends are declared relative to the tool-defining spec dir
		// (ADR-0008 D3).
		queue = append(queue, depgraph.TaskRef{
			SpecRelpath: filepath.FromSlash(path.Join(dirSlash, d.Spec)),
			Name:        d.Task,
		})
	}
	seen := map[depgraph.TaskRef]struct{}{}
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		if _, dup := seen[ref]; dup {
			continue
		}
		seen[ref] = struct{}{}
		if st, ok := statusByRef[ref]; ok && st.isDrift() {
			return true
		}
		if t, ok := byRef[ref]; ok {
			queue = append(queue, t.DependsOn...)
		}
	}
	return false
}
