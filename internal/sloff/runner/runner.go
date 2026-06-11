// Package runner orchestrates spec discovery, preflight, dependency-graph derivation
// and per-task fingerprint lookup/execute/write. It is the integration point for the
// foundation packages (spec / glob / hash / fingerprint / depgraph / toolresolver / preflight).
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"golang.org/x/sync/errgroup"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/depgraph"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	"github.com/izumin5210/sloff/internal/sloff/glob"
	"github.com/izumin5210/sloff/internal/sloff/hash"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
)

// runnerTracerName is the InstrumentationScope name attached to every span
// runner emits. It is the import path so trace consumers can group sloff's
// runner-side activity under a single library identity.
const runnerTracerName = "github.com/izumin5210/sloff/internal/sloff/runner"

// endSpan finishes span with error status when *errp is non-nil. The pointer
// indirection lets callers tie span outcome to a named return value:
//
//	defer endSpan(span, &err)
func endSpan(span trace.Span, errp *error) {
	if errp != nil && *errp != nil {
		span.RecordError(*errp)
		span.SetStatus(codes.Error, (*errp).Error())
	}
	span.End()
}

// inputHashAttr is the truncated form of the full input hash used as a span
// attribute. The full hex is preserved on the cache record; spans only need
// enough to correlate sibling tasks across one run.
func inputHashAttr(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// Logger is the minimal logging surface the runner uses. log.Default() is used by default.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

type stdLogger struct{ l *log.Logger }

func (s stdLogger) Infof(format string, args ...any)  { s.l.Printf("INFO  "+format, args...) }
func (s stdLogger) Warnf(format string, args ...any)  { s.l.Printf("WARN  "+format, args...) }
func (s stdLogger) Errorf(format string, args ...any) { s.l.Printf("ERROR "+format, args...) }

// Options configure a Runner.
type Options struct {
	RepoRoot  string
	Specs     []spec.Spec
	Storage   fingerprint.Storage
	Resolvers *toolresolver.Registry
	Preflight *preflight.Registry

	// ReadOnly suppresses Storage.Save (used when SLOFF_ALLOW_STALE_DEPS=1).
	ReadOnly bool

	// Force bypasses the fingerprint hit decision so every task re-executes
	// regardless of the cached output_hash match. Records are still written
	// (subject to ReadOnly and ADR-0009 §write-skip), and preflight still
	// runs — see ADR-0012 for the rationale and the explicit decision not to
	// mirror this knob into an env var.
	Force bool

	// Stdout/Stderr are forwarded to spawned processes; nil falls back to os.Stdout / os.Stderr.
	Stdout io.Writer
	Stderr io.Writer

	Logger Logger

	// TracerProvider is where runner emits OpenTelemetry spans. nil yields a
	// noop provider so embedding callers that haven't configured tracing pay
	// no cost and never accidentally bleed sloff's spans through a host's
	// global TracerProvider. The CLI entry points pass a sloff-local provider
	// configured from OTEL_*/SLOFF_OTEL_* env (cmd/sloff/otel.go); embedders
	// can pass their own to fan sloff spans into their pipeline.
	TracerProvider trace.TracerProvider
}

// Runner executes all discovered specs in topological order with fingerprint lookup and
// output-comparison invalidation.
type Runner struct {
	opts   Options
	logger Logger
	stdout io.Writer
	stderr io.Writer
	tracer trace.Tracer        // derived once in New from opts.TracerProvider
	byKey  map[string]taskInfo // depgraph.Task key → taskInfo, filled by collectTasks

	// prefetched caches the records loaded by Storage.LoadMany at the top of
	// Run; runTask consults this map first to avoid a per-task round-trip
	// against remote backends. Populated once before runTasks starts and
	// read concurrently by every task goroutine afterwards, so it does not
	// need a mutex on the read path.
	//
	// prefetchedKeys records *which* keys were asked about. A map miss
	// alone cannot distinguish "asked and got nothing back" from "never
	// asked because the optimistic key didn't include the actual on-disk
	// state". The latter happens when an upstream task regenerates outputs
	// mid-run and the downstream task's actual input_hash diverges from
	// the pre-run optimistic one; fingerprintLookup falls back to a live
	// Storage.Load in that case so transitive cache hits still work.
	prefetched     map[fingerprint.Key]*fingerprintv1.Record
	prefetchedKeys map[fingerprint.Key]struct{}

	// pending accumulates records that runTask wants to persist; the run flushes
	// them via Storage.SaveMany at the end. Per-task Save would defeat the bulk
	// API on remote backends; deferring to a single batch write is acceptable
	// because fingerprint records are self-healing (a missing record only costs
	// one extra generator run).
	pendingMu sync.Mutex
	pending   []fingerprint.KeyRecord

	// producedBy maps each resolved output path to the task that produced it. Cross-task
	// duplicate writes are spec conflicts that depgraph cannot catch on a clean checkout
	// (pre-execution globs are empty), so the runner records writers as runs progress and
	// fails the run if a later task lands on a path another task already produced. Guarded
	// by producedByMu so independent tasks running in parallel via runTasks don't race on
	// the shared map.
	producedByMu sync.Mutex
	producedBy   map[string]string
}

// New constructs a Runner.
func New(opts Options) *Runner {
	logger := opts.Logger
	if logger == nil {
		logger = stdLogger{l: log.New(os.Stderr, "sloff ", log.LstdFlags)}
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	tp := opts.TracerProvider
	if tp == nil {
		// Default to a sloff-local noop so callers that don't configure
		// tracing pay nothing and don't accidentally route runner spans
		// through whatever the host process happens to have on the global
		// TracerProvider.
		tp = noop.NewTracerProvider()
	}
	return &Runner{
		opts:   opts,
		logger: logger,
		stdout: stdout,
		stderr: stderr,
		tracer: tp.Tracer(runnerTracerName),
	}
}

// Run executes preflight then every task. Errors during preflight or task execution
// abort the run.
func (r *Runner) Run(ctx context.Context) error {
	if r.opts.Force {
		// One run-scoped notice; task-level forced reruns are surfaced through
		// the sloff.force span attribute (ADR-0012 §"観測性") so the per-task
		// RUN log stays uniform with cache-miss runs.
		r.logger.Infof("force mode: fingerprint hits will be bypassed and every task re-executed")
	}
	// ADR-0008: build the repo-wide tool registry once, validate every task's
	// references resolve to a defined tool, then resolve each *referenced*
	// tool exactly once. Storing results by name lets collectTasks fan a
	// tool out across N referencing tasks at zero additional resolver cost.
	//
	// We deliberately resolve only the subset of tools that some command
	// actually references. The registry is repo-wide and may carry catalog-
	// style definitions whose dependencies aren't installed on this machine
	// (e.g. a pnpm-local entry for a workspace package missing from the
	// current checkout); resolving them eagerly would block unrelated tasks
	// that never use those tools.
	registry, referencedToolNames, err := r.prepareRegistry()
	if err != nil {
		return err
	}

	// Preflight runs only the checkers whose resolver name matches a tool the
	// current spec set actually references — same scoping discipline as
	// resolveReferencedTools below. Catalog tools that no command pulls in
	// stay inert; their checkers aren't invoked either.
	if err := r.runPreflight(ctx, registry, referencedToolNames); err != nil {
		return err
	}

	inputsByTool, err := r.resolveInputContribs(ctx, registry, referencedToolNames)
	if err != nil {
		return err
	}
	versionsByTool, err := r.resolveVersionContribs(ctx, registry, referencedToolNames)
	if err != nil {
		return err
	}

	tasks, err := r.collectTasksTraced(ctx, inputsByTool, versionsByTool)
	if err != nil {
		return err
	}
	ordered, err := r.depgraphBuildTraced(ctx, tasks)
	if err != nil {
		return err
	}

	if err := r.prefetchFingerprints(ctx, ordered); err != nil {
		return err
	}

	r.producedBy = map[string]string{}
	runErr := r.runTasks(ctx, ordered)
	// Flush even when runTasks returned an error so records queued by tasks
	// that completed *before* a later failure are still persisted. Failed
	// tasks never enqueue a record (runTask only calls fingerprintStore
	// after a successful generator + output hash), so the queue holds only
	// good entries.
	//
	// Use a detached context for the flush: if the run was canceled via
	// the parent ctx (Ctrl-C, CI timeout, parent process abort), passing
	// the canceled ctx straight through would make Storage.SaveMany abort
	// immediately and drop every queued record. We instead allow a short
	// budget for the bulk write to drain — modeled after the OTel
	// shutdown drain in cmd/sloff — so an interrupted run still warms
	// the cache for the next invocation.
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()
	flushErr := r.flushFingerprints(flushCtx)
	return errors.Join(runErr, flushErr)
}

// flushTimeout caps the post-run SaveMany. Generous enough to give a
// remote backend (DynamoDB BatchWriteItem fan-out) time to drain a
// large queue, but bounded so a hung backend cannot block process
// exit indefinitely.
const flushTimeout = 30 * time.Second

// prefetchFingerprints computes an optimistic input_hash for every task using
// the on-disk state at run start, then issues a single Storage.LoadMany to pull
// down every record that might be a hit. The result feeds the per-task lookup
// path so backends with non-trivial per-key RTT (e.g. DynamoDB) only pay the
// network cost once per run instead of once per task.
//
// "Optimistic" matters when an upstream task is a miss and regenerates outputs
// that some downstream task lists as input. Under sloff's deterministic-
// generator assumption (ADR-0009 byte stability) the regenerated bytes match
// what was on disk at prefetch time, so the optimistic input_hash equals the
// real one. When the downstream actually sees different bytes (typical when
// the user pulls a colleague's branch and an upstream tool produces new
// output on first run here), the runtime input_hash diverges from the
// optimistic one and fingerprintLookup falls back to a live Storage.Load
// so a record written by the colleague — or by CI — still lands as a hit.
func (r *Runner) prefetchFingerprints(ctx context.Context, ordered []depgraph.Task) (err error) {
	ctx, span := r.tracer.Start(ctx, "runner.fingerprint.prefetch", trace.WithAttributes(
		attribute.Int("sloff.task.count", len(ordered)),
	))
	defer endSpan(span, &err)

	if len(ordered) == 0 {
		r.prefetched = map[fingerprint.Key]*fingerprintv1.Record{}
		r.prefetchedKeys = map[fingerprint.Key]struct{}{}
		return nil
	}

	keys := make([]fingerprint.Key, len(ordered))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(taskConcurrency(len(ordered)))
	for i, t := range ordered {
		g.Go(func() error {
			key, err := r.optimisticKey(gctx, t)
			if err != nil {
				return fmt.Errorf("prefetch %s/%s: %w", t.SpecRelpath, t.Name, err)
			}
			keys[i] = key
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	loaded, err := r.opts.Storage.LoadMany(ctx, keys)
	if err != nil {
		return fmt.Errorf("prefetch: %w", err)
	}
	r.prefetched = loaded
	r.prefetchedKeys = make(map[fingerprint.Key]struct{}, len(keys))
	for _, k := range keys {
		r.prefetchedKeys[k] = struct{}{}
	}
	span.SetAttributes(attribute.Int("sloff.fingerprint.prefetched", len(loaded)))
	return nil
}

// optimisticKey computes the input_hash for t using the same hash composition
// runTask uses, but without actually executing the task. Reads input files
// from disk; safe to call before any task has run because it only observes
// state, never mutates.
func (r *Runner) optimisticKey(ctx context.Context, t depgraph.Task) (fingerprint.Key, error) {
	if err := ctx.Err(); err != nil {
		return fingerprint.Key{}, err
	}
	info := r.byKey[depgraphKey(t)]
	filesHash, err := hash.Files(r.opts.RepoRoot, info.inputPaths)
	if err != nil {
		return fingerprint.Key{}, err
	}
	cmdHash := hash.Cmd(info.command.Cmd)
	resolvedVersionsHash := hash.ResolvedVersions(versionStrings(info.versions))
	inputHash := hash.Input(filesHash, cmdHash, resolvedVersionsHash)
	return fingerprint.Key{SpecRelpath: t.SpecRelpath, TaskID: t.Name, InputHash: inputHash}, nil
}

// flushFingerprints persists every record runTask accumulated via
// fingerprintStore as one Storage.SaveMany. End-of-run batching matters most
// for remote backends; for the local backend SaveMany degenerates to per-key
// parallel writes and the timing is indistinguishable from per-task Save.
func (r *Runner) flushFingerprints(ctx context.Context) (err error) {
	r.pendingMu.Lock()
	pending := r.pending
	r.pending = nil
	r.pendingMu.Unlock()

	if len(pending) == 0 {
		return nil
	}

	ctx, span := r.tracer.Start(ctx, "runner.fingerprint.flush", trace.WithAttributes(
		attribute.Int("sloff.fingerprint.pending", len(pending)),
	))
	defer endSpan(span, &err)
	return r.opts.Storage.SaveMany(ctx, pending)
}

// collectTasksTraced wraps the ctx-free collectTasks with a span. The
// underlying call has no cancelable I/O, so the span purely captures phase
// timing and the resolved task count for the trace tree.
func (r *Runner) collectTasksTraced(ctx context.Context, inputsByTool map[string][]string, versionsByTool map[string][]toolresolver.ResolvedVersion) (tasks []depgraph.Task, err error) {
	_, span := r.tracer.Start(ctx, "runner.collect_tasks")
	defer endSpan(span, &err)
	tasks, err = r.collectTasks(inputsByTool, versionsByTool)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.Int("sloff.task.count", len(tasks)))
	return tasks, nil
}

// depgraphBuildTraced wraps depgraph.Build with a span. depgraph.Build is a
// pure function that doesn't take ctx; the wrapper exists only so the phase
// shows up in the trace tree alongside the others.
func (r *Runner) depgraphBuildTraced(ctx context.Context, tasks []depgraph.Task) (ordered []depgraph.Task, err error) {
	_, span := r.tracer.Start(ctx, "runner.depgraph.build", trace.WithAttributes(
		attribute.Int("sloff.task.count", len(tasks)),
	))
	defer endSpan(span, &err)
	return depgraph.Build(tasks)
}

// runTasks executes the topologically-ordered task list with bounded
// concurrency. A task starts as soon as every task that produces one of its
// inputs has finished — depgraph already sorted them, so we only need to
// re-derive each task's predecessor set from the same output→producer mapping
// it used. Independent tasks (the common case for fingerprint-hit runs across
// service-local gen-db, where each spec's outputs sit in its own service dir)
// fan out across NumCPU workers; tasks with real producer→consumer chains
// (buf-default → buf-custom, build-protoc-plugins → buf-custom, …) still
// serialise inside the chain.
//
// First failure short-circuits the run: dependents of a failed task are not
// scheduled and the first non-context error is returned. Cancellation is
// observed before each task starts so a Ctrl-C drops in-flight tasks at the
// next scheduling boundary rather than waiting for the whole queue to drain.
func (r *Runner) runTasks(ctx context.Context, ordered []depgraph.Task) (err error) {
	if len(ordered) == 0 {
		return nil
	}

	concurrency := taskConcurrency(len(ordered))
	ctx, span := r.tracer.Start(ctx, "runner.tasks.run", trace.WithAttributes(
		attribute.Int("sloff.task.count", len(ordered)),
		attribute.Int("sloff.tasks.concurrency", concurrency),
	))
	defer endSpan(span, &err)

	predecessors := taskPredecessorIndices(ordered)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	done := make([]chan struct{}, len(ordered))
	failed := make([]bool, len(ordered))
	for i := range done {
		done[i] = make(chan struct{})
	}

	for i, t := range ordered {
		g.Go(func() error {
			defer close(done[i])
			for _, d := range predecessors[i] {
				select {
				case <-done[d]:
				case <-gctx.Done():
					failed[i] = true
					return nil
				}
				if failed[d] {
					failed[i] = true
					return nil
				}
			}
			if err := gctx.Err(); err != nil {
				failed[i] = true
				return nil
			}
			if err := r.runTask(gctx, t); err != nil {
				failed[i] = true
				return err
			}
			return nil
		})
	}
	err = g.Wait()
	return err
}

// taskPredecessorIndices returns, for each task index in ordered, the set of
// indices whose Outputs produce one of this task's Inputs. Same intersection
// rule depgraph.Build uses internally; we recompute it here so the runner
// stays decoupled from depgraph's internal edge representation.
func taskPredecessorIndices(ordered []depgraph.Task) [][]int {
	producer := map[string]int{}
	for i, t := range ordered {
		for _, out := range t.Outputs {
			producer[out] = i
		}
	}
	preds := make([][]int, len(ordered))
	for i, t := range ordered {
		seen := map[int]struct{}{}
		for _, in := range t.Inputs {
			p, ok := producer[in]
			if !ok || p == i {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			preds[i] = append(preds[i], p)
		}
	}
	return preds
}

// taskConcurrency caps how many tasks runTasks executes in parallel.
// Cache-hit tasks are I/O-bound (re-reading every input + every output to
// re-hash); fingerprint-miss tasks spawn a child generator. NumCPU is the same
// budget the resolver fan-out uses, mostly because most tasks block on file
// reads and a few on subprocess wait. Values much higher than NumCPU on
// SSD-backed APFS show diminishing returns and risk blowing past the
// per-process file descriptor limit.
func taskConcurrency(n int) int {
	if n <= 0 {
		return 1
	}
	cpu := max(runtime.NumCPU(), 1)
	if n < cpu {
		return n
	}
	return cpu
}

// Plan resolves all discovered specs into a topologically-ordered task list
// without running preflight or executing any cmd. It is the planning core
// shared with `sloff graph` (and the future `sloff run --explain` once that
// path is wired up): same registry / Inputs path as Run, so callers observe
// the exact set of inputs / outputs the runner would orchestrate.
//
// Plan deliberately calls `Registry.Inputs` only (not `Versions`) because
// the depgraph never reads ResolvedVersions — they only feed `resolved_versions_hash`
// (architecture.md, ADR-0008 D6 addendum). Skipping Versions means
// `script` resolvers don't spawn `<bin> --version` here, which keeps
// graph-style consumers usable when prebuilt binaries aren't installed.
//
// Preflight is intentionally skipped for the same reason: debugging tools
// that read the depgraph must remain useful when the install state is
// drifted, since drift is one of the conditions users reach for the graph
// to investigate.
func (r *Runner) Plan(ctx context.Context) ([]depgraph.Task, error) {
	registry, referencedToolNames, err := r.prepareRegistry()
	if err != nil {
		return nil, err
	}
	inputsByTool, err := r.resolveInputContribs(ctx, registry, referencedToolNames)
	if err != nil {
		return nil, err
	}
	tasks, err := r.collectTasksTraced(ctx, inputsByTool, nil)
	if err != nil {
		return nil, err
	}
	return r.depgraphBuildTraced(ctx, tasks)
}

// prepareRegistry builds the repo-wide tool registry, validates command tool
// references against it, and collects the deduplicated set of names some
// command actually pulls in. Both Run and Plan need this same triple, so the
// helper keeps the two flows from diverging on the validation rules.
func (r *Runner) prepareRegistry() (*spec.ToolRegistry, []string, error) {
	registry, err := spec.BuildToolRegistry(r.opts.Specs)
	if err != nil {
		return nil, nil, err
	}
	if err := spec.ValidateToolReferences(r.opts.Specs, registry); err != nil {
		return nil, nil, err
	}
	if err := spec.ValidateDependReferences(r.opts.Specs); err != nil {
		return nil, nil, err
	}
	return registry, referencedTools(r.opts.Specs), nil
}

// runPreflight invokes only the registered Checkers whose names match a
// resolver actually pulled in by some command in this run. Scoping by
// referenced resolver names keeps catalog-style tool registries lean: a
// pnpm-local Checker shouldn't run (and shouldn't fail) when no command
// uses pnpm-local at all.
//
// Drift-style failures arrive as preflight.Issue entries; the runner
// reports them and either aborts (the default) or, when ReadOnly is set
// via SLOFF_ALLOW_STALE_DEPS, degrades to read-only so fingerprints
// are not written for a known-suspect run. Hard errors from a checker
// (the check itself couldn't execute) bypass the read-only fall-through
// and fail the run regardless.
func (r *Runner) runPreflight(ctx context.Context, registry *spec.ToolRegistry, referencedToolNames []string) (err error) {
	ctx, span := r.tracer.Start(ctx, "runner.preflight", trace.WithAttributes(
		attribute.Int("sloff.tool.referenced_count", len(referencedToolNames)),
	))
	defer endSpan(span, &err)

	if r.opts.Preflight == nil {
		span.SetAttributes(attribute.String("sloff.preflight.skipped_reason", "no_registry"))
		return nil
	}
	checkers := scopeCheckers(r.opts.Preflight.Names(), registry, referencedToolNames)
	span.SetAttributes(attribute.Int("sloff.preflight.checker_count", len(checkers)))
	if len(checkers) == 0 {
		span.SetAttributes(attribute.String("sloff.preflight.skipped_reason", "no_referenced_checkers"))
		return nil
	}
	res, err := r.opts.Preflight.Run(ctx, ".", checkers)
	if err != nil {
		return err
	}
	span.SetAttributes(attribute.Bool("sloff.preflight.ok", res.OK))
	if !res.OK {
		span.SetAttributes(attribute.Int("sloff.preflight.issue_count", len(res.Issues)))
		r.reportPreflightIssues(res.Issues)
		if !r.opts.ReadOnly {
			err = fmt.Errorf("preflight failed (%d issues); set SLOFF_ALLOW_STALE_DEPS=1 to bypass", len(res.Issues))
			return err
		}
		r.logger.Warnf("preflight issues ignored due to ReadOnly mode; fingerprints will not be written")
	}
	return nil
}

// scopeCheckers intersects the registered Checker names with the resolver
// names referenced by any command in this run. The resolver/checker pairing
// is by Name (architecture.md), so a referenced tool whose Declared.Resolver
// is "pnpm-local" pulls in the "pnpm-local" Checker if one exists.
func scopeCheckers(checkerNames []string, registry *spec.ToolRegistry, referencedToolNames []string) []string {
	if len(checkerNames) == 0 {
		return nil
	}
	usedResolvers := map[string]struct{}{}
	for _, toolName := range referencedToolNames {
		if entry, ok := registry.Lookup(toolName); ok {
			usedResolvers[entry.Declared.Resolver] = struct{}{}
		}
	}
	out := make([]string, 0, len(checkerNames))
	for _, name := range checkerNames {
		if _, ok := usedResolvers[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// resolveInputContribs invokes Registry.Inputs once per referenced tool name.
// specDir for each invocation is the dir where the tool was *defined*
// (ADR-0008 D3), not where it's referenced from, so tool definitions stay
// self-contained relative to their host sloff.yml.
//
// Names not in the registry have already been rejected by
// ValidateToolReferences; a missing entry here would be a programmer error.
//
// Splitting Inputs from Versions lets callers that only care about depgraph
// structure (`sloff graph` / future `--explain`-style read-only debug
// surfaces) skip the Versions path entirely; see IZU-16.
func (r *Runner) resolveInputContribs(ctx context.Context, registry *spec.ToolRegistry, referenced []string) (out map[string][]string, err error) {
	ctx, span := r.tracer.Start(ctx, "runner.resolve.inputs", trace.WithAttributes(
		attribute.Int("sloff.tool.referenced_count", len(referenced)),
	))
	defer endSpan(span, &err)

	results := make([][]string, len(referenced))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(resolverConcurrency(len(referenced)))
	for i, name := range referenced {
		entry, ok := registry.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("runner: referenced tool %q missing from registry; ValidateToolReferences should have caught this", name)
		}
		declared := []toolresolver.DeclaredTool{toolresolverDeclared(entry.Declared)}
		g.Go(func() (gerr error) {
			toolCtx, toolSpan := r.tracer.Start(gctx,
				fmt.Sprintf("resolver.%s[%s]", entry.Declared.Resolver, entry.Name),
				trace.WithAttributes(
					attribute.String("sloff.tool.name", entry.Name),
					attribute.String("sloff.resolver.channel", entry.Declared.Resolver),
					attribute.String("sloff.resolver.phase", "inputs"),
				))
			defer endSpan(toolSpan, &gerr)

			ins, gerr := r.opts.Resolvers.Inputs(toolCtx, entry.SpecDir, declared)
			if gerr != nil {
				gerr = fmt.Errorf("resolve inputs for tool %q (defined in %s): %w", entry.Name, entry.SpecDir, gerr)
				return gerr
			}
			toolSpan.SetAttributes(attribute.Int("sloff.tool.input.count", len(ins)))
			results[i] = ins
			return nil
		})
	}
	if err = g.Wait(); err != nil {
		return nil, err
	}
	out = make(map[string][]string, len(referenced))
	for i, name := range referenced {
		out[name] = results[i]
	}
	return out, nil
}

// resolveVersionContribs invokes Registry.Versions once per referenced tool
// name. Same scoping discipline as resolveInputContribs.
func (r *Runner) resolveVersionContribs(ctx context.Context, registry *spec.ToolRegistry, referenced []string) (out map[string][]toolresolver.ResolvedVersion, err error) {
	ctx, span := r.tracer.Start(ctx, "runner.resolve.versions", trace.WithAttributes(
		attribute.Int("sloff.tool.referenced_count", len(referenced)),
	))
	defer endSpan(span, &err)

	results := make([][]toolresolver.ResolvedVersion, len(referenced))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(resolverConcurrency(len(referenced)))
	for i, name := range referenced {
		entry, ok := registry.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("runner: referenced tool %q missing from registry; ValidateToolReferences should have caught this", name)
		}
		declared := []toolresolver.DeclaredTool{toolresolverDeclared(entry.Declared)}
		g.Go(func() (gerr error) {
			toolCtx, toolSpan := r.tracer.Start(gctx,
				fmt.Sprintf("resolver.%s[%s]", entry.Declared.Resolver, entry.Name),
				trace.WithAttributes(
					attribute.String("sloff.tool.name", entry.Name),
					attribute.String("sloff.resolver.channel", entry.Declared.Resolver),
					attribute.String("sloff.resolver.phase", "versions"),
				))
			defer endSpan(toolSpan, &gerr)

			vs, gerr := r.opts.Resolvers.Versions(toolCtx, entry.SpecDir, declared)
			if gerr != nil {
				gerr = fmt.Errorf("resolve versions for tool %q (defined in %s): %w", entry.Name, entry.SpecDir, gerr)
				return gerr
			}
			toolSpan.SetAttributes(attribute.Int("sloff.tool.version.count", len(vs)))
			results[i] = vs
			return nil
		})
	}
	if err = g.Wait(); err != nil {
		return nil, err
	}
	out = make(map[string][]toolresolver.ResolvedVersion, len(referenced))
	for i, name := range referenced {
		out[name] = results[i]
	}
	return out, nil
}

// resolverConcurrency caps how many resolver goroutines run in parallel.
// `packages.Load` ultimately spawns `go list` (which itself parallelises across
// GOMAXPROCS), so letting dozens of go-local resolvers fan out unbounded would
// stampede the file system and the Go toolchain's own internal parallelism.
// `runtime.NumCPU()` keeps each box loaded but bounded; it comfortably hosts
// the script / go-local / pnpm-local fan-out a typical polyglot monorepo has.
func resolverConcurrency(n int) int {
	if n <= 0 {
		return 1
	}
	cpu := max(runtime.NumCPU(), 1)
	if n < cpu {
		return n
	}
	return cpu
}

// referencedTools collects the deduplicated, sorted set of tool names any
// command in specs references. Tools defined in the registry but never
// referenced are intentionally absent so the runner doesn't pay (or fail on)
// their resolver cost.
func referencedTools(specs []spec.Spec) []string {
	seen := map[string]struct{}{}
	for _, sp := range specs {
		for _, c := range sp.File.Commands {
			for _, name := range c.Tools {
				seen[name] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// taskInfo carries the bits of spec.Command needed to execute it. Stored on the depgraph.Task
// via the SpecRelpath/Name key — we look it back up by key when executing.
type taskInfo struct {
	specRelpath    string
	command        spec.Command
	inputPaths     []string
	outputPatterns []string
	// versions holds the per-task ResolvedVersion concatenation in tools[] order,
	// pre-computed during collectTasks so runTask can hash without revisiting
	// the resolver registry. nil when collectTasks ran without versions
	// (depgraph-only callers).
	versions []toolresolver.ResolvedVersion
}

// collectTasks expands inputs/outputs for every spec command and folds each
// task's referenced tools' contributions into the task's input set. Folding
// extras in here is what lets depgraph wire up workspace-tool build tasks to
// their consumers via the usual output-overlap rule, instead of needing a
// parallel dependency channel.
//
// versionsByTool may be nil for callers that don't need resolved_versions_hash (graph-
// style consumers); inputsByTool must always be present so depgraph sees the
// same inputs the runner would.
func (r *Runner) collectTasks(inputsByTool map[string][]string, versionsByTool map[string][]toolresolver.ResolvedVersion) ([]depgraph.Task, error) {
	r.byKey = map[string]taskInfo{}
	tasks := make([]depgraph.Task, 0)
	for _, sp := range r.opts.Specs {
		for _, c := range sp.File.Commands {
			inputs, err := glob.Expand(r.opts.RepoRoot, sp.Dir, c.Inputs)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: expand inputs: %w", sp.Dir, c.Name, err)
			}
			outputs, err := glob.Expand(r.opts.RepoRoot, sp.Dir, c.Outputs)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: expand outputs: %w", sp.Dir, c.Name, err)
			}

			extraInputs := combineToolInputs(c.Tools, inputsByTool)
			mergedInputs := mergeInputs(inputs, extraInputs)

			var versions []toolresolver.ResolvedVersion
			if versionsByTool != nil {
				versions = combineResolvedVersions(c.Tools, versionsByTool)
			}

			t := depgraph.Task{
				SpecRelpath: sp.Dir,
				Name:        c.Name,
				Inputs:      mergedInputs,
				Outputs:     outputs,
				DependsOn:   resolveDepends(sp.Dir, c.Depends),
			}
			tasks = append(tasks, t)
			r.byKey[depgraphKey(t)] = taskInfo{
				specRelpath:    sp.Dir,
				command:        c,
				inputPaths:     mergedInputs,
				outputPatterns: c.Outputs,
				versions:       versions,
			}
		}
	}
	if err := detectOutputPatternConflicts(tasks, r.byKey); err != nil {
		return nil, err
	}
	return tasks, nil
}

// detectOutputPatternConflicts fails fast when two tasks declare output globs that, once
// resolved against their respective spec dirs, point at the same path (e.g. both list
// `shared.txt` from the same sloff.yml, or both list `../gen/foo.go` whose `..` paths
// land on the same absolute file). It complements depgraph.Build's expanded-path
// conflict detector: that one operates on the post-glob file set, which is empty on a
// clean checkout and therefore can't see two tasks aimed at the same path before either
// has run. The pattern-string check works regardless of checkout state, and matters more
// under the parallel scheduler where the runtime recordProducedPaths trip races against
// OS-level cmd errors when two cmds try to create the same file simultaneously.
//
// Outputs are interpreted relative to the declaring spec's dir (ADR-0008 / IZU-17), so
// the conflict map is keyed by `path.Clean(path.Join(specDir, pattern))` rather than the
// raw pattern string. Without that, two service-local specs each declaring
// `outputs: ["internal/foo.gen.go"]` would be flagged as duplicates even though they
// resolve to distinct files (services/a/internal/foo.gen.go vs services/b/...). Using
// the resolved repo-relative path keys also makes the error message point at the
// concrete file the user must disambiguate, not the ambiguous source pattern.
//
// Glob overlaps that aren't string-equal (e.g. `**/*.go` vs `pkg/foo.go`) are not
// detected here; they still surface via the runtime recordProducedPaths check after
// both cmds finish writing.
func detectOutputPatternConflicts(tasks []depgraph.Task, byKey map[string]taskInfo) error {
	patternProducers := map[string][]string{}
	for _, t := range tasks {
		info := byKey[depgraphKey(t)]
		label := taskLabel(t)
		specDir := filepath.ToSlash(t.SpecRelpath)
		seen := map[string]struct{}{}
		for _, p := range info.outputPatterns {
			key := path.Clean(path.Join(specDir, p))
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			patternProducers[key] = append(patternProducers[key], label)
		}
	}
	var parts []string
	for pattern, labels := range patternProducers {
		if len(labels) <= 1 {
			continue
		}
		sort.Strings(labels)
		parts = append(parts, fmt.Sprintf("%q -> [%s]", pattern, strings.Join(labels, ", ")))
	}
	if len(parts) == 0 {
		return nil
	}
	sort.Strings(parts)
	return fmt.Errorf("duplicate output pattern producers: %s; fix the spec to give each output pattern exactly one writer", strings.Join(parts, "; "))
}

// combineToolInputs concatenates ExtraInputs in the order tools appear in
// the spec's tools[] list. ValidateToolReferences has already guaranteed
// every name resolves, so a missing entry here is a programmer error and
// we panic to surface it loudly during tests.
func combineToolInputs(names []string, inputsByTool map[string][]string) []string {
	var combined []string
	for _, name := range names {
		v, ok := inputsByTool[name]
		if !ok {
			panic(fmt.Sprintf("runner: tool %q missing from resolved inputs map; ValidateToolReferences should have caught this", name))
		}
		combined = append(combined, v...)
	}
	return combined
}

// combineResolvedVersions is the Versions sibling of combineToolInputs.
func combineResolvedVersions(names []string, versionsByTool map[string][]toolresolver.ResolvedVersion) []toolresolver.ResolvedVersion {
	var combined []toolresolver.ResolvedVersion
	for _, name := range names {
		v, ok := versionsByTool[name]
		if !ok {
			panic(fmt.Sprintf("runner: tool %q missing from resolved versions map; ValidateToolReferences should have caught this", name))
		}
		combined = append(combined, v...)
	}
	return combined
}

// resolveDepends maps declared depends entries to depgraph TaskRefs. The
// reference rules (existence, self-reference, duplicates, repo-root escape)
// are enforced by spec.ValidateDependReferences before collectTasks runs, so
// this is pure path arithmetic: clean-join the consumer's spec dir with each
// entry's relative spec path (empty = same dir), mirroring how inputs/outputs
// globs resolve (ADR-0013 D1).
func resolveDepends(specDir string, depends []spec.Depend) []depgraph.TaskRef {
	if len(depends) == 0 {
		return nil
	}
	dirSlash := filepath.ToSlash(specDir)
	out := make([]depgraph.TaskRef, 0, len(depends))
	for _, d := range depends {
		out = append(out, depgraph.TaskRef{
			SpecRelpath: filepath.FromSlash(path.Join(dirSlash, d.Spec)),
			Name:        d.Task,
		})
	}
	return out
}

// mergeInputs returns the deduplicated, sorted union of declared and extra
// input paths. Extras are normalised to OS-native form so they compare equal
// to glob.Expand output, which uses the OS separator.
func mergeInputs(declared, extra []string) []string {
	if len(extra) == 0 {
		return declared
	}
	seen := make(map[string]struct{}, len(declared)+len(extra))
	out := make([]string, 0, len(declared)+len(extra))
	add := func(p string) {
		if p == "" {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, p := range declared {
		add(p)
	}
	for _, p := range extra {
		add(filepath.FromSlash(p))
	}
	sort.Strings(out)
	return out
}

func depgraphKey(t depgraph.Task) string { return t.SpecRelpath + "\x00" + t.Name }

func (r *Runner) runTask(ctx context.Context, t depgraph.Task) (err error) {
	info := r.byKey[depgraphKey(t)]
	versions := info.versions

	ctx, span := r.tracer.Start(ctx, "runner.task.run", trace.WithAttributes(
		attribute.String("sloff.spec", t.SpecRelpath),
		attribute.String("sloff.task.name", t.Name),
		attribute.Int("sloff.tool.count", len(versions)),
		// Stamp every task span regardless of whether the lookup is a hit, a
		// stale record, or a clean miss — without this, a forced rerun of a
		// task that already had no cached record would be indistinguishable
		// from an ordinary cache miss in trace analysis (ADR-0012).
		attribute.Bool("sloff.force", r.opts.Force),
	))
	defer endSpan(span, &err)

	filesHash, err := hash.Files(r.opts.RepoRoot, info.inputPaths)
	if err != nil {
		return fmt.Errorf("%s: hash inputs: %w", t.Name, err)
	}
	cmdHash := hash.Cmd(info.command.Cmd)
	resolvedVersionsHash := hash.ResolvedVersions(versionStrings(versions))
	inputHash := hash.Input(filesHash, cmdHash, resolvedVersionsHash)
	span.SetAttributes(attribute.String("sloff.input.hash", inputHashAttr(inputHash)))

	key := fingerprint.Key{SpecRelpath: t.SpecRelpath, TaskID: t.Name, InputHash: inputHash}
	taskLabel := taskLabel(t)
	hit, existing, paths, err := r.fingerprintLookup(ctx, key)
	if err != nil {
		return fmt.Errorf("%s: load record: %w", t.Name, err)
	}
	// ADR-0012: --force bypasses the hit decision but keeps the lookup so the
	// loaded record still drives the post-exec write-skip rule (ADR-0009 §4).
	// We drop hit→false rather than skipping the Storage round-trip so a forced
	// rerun that produces byte-identical output preserves the record's
	// first-observed informational fields. The sloff.force attribute is set
	// unconditionally at span creation time so cache-miss reruns are also
	// distinguishable from genuine misses in traces.
	if hit && r.opts.Force {
		hit = false
	}
	span.SetAttributes(attribute.Bool("sloff.fingerprint.hit", hit))
	if hit {
		if err := r.recordProducedPaths(taskLabel, paths); err != nil {
			return err
		}
		r.logger.Infof("SKIP %s/%s (fingerprint hit)", t.SpecRelpath, t.Name)
		return nil
	}

	r.logger.Infof("RUN  %s/%s", t.SpecRelpath, t.Name)
	if err := r.execCmd(ctx, info); err != nil {
		return fmt.Errorf("%s: %w", t.Name, err)
	}

	outputPaths, err := r.resolveOutputs(info)
	if err != nil {
		return fmt.Errorf("%s: %w", t.Name, err)
	}
	if err := r.recordProducedPaths(taskLabel, outputPaths); err != nil {
		return err
	}
	outputHash, err := hash.Files(r.opts.RepoRoot, outputPaths)
	if err != nil {
		return fmt.Errorf("%s: hash outputs: %w", t.Name, err)
	}
	files, err := perFileHashes(r.opts.RepoRoot, outputPaths)
	if err != nil {
		return fmt.Errorf("%s: per-file hash: %w", t.Name, err)
	}

	if r.opts.ReadOnly {
		r.logger.Warnf("%s/%s: ReadOnly mode, record not written", t.SpecRelpath, t.Name)
		return nil
	}

	newRec := &fingerprintv1.Record{
		SchemaVersion: fingerprint.SchemaVersion,
		Spec: &fingerprintv1.Spec{
			Cmd:    strings.Join(info.command.Cmd, " "),
			Dir:    info.specRelpath,
			TaskId: info.command.Name,
		},
		Input: &fingerprintv1.Input{
			Hash:                 inputHash,
			FilesHash:            filesHash,
			CmdHash:              cmdHash,
			ResolvedVersionsHash: resolvedVersionsHash,
			ResolvedVersions:     resolvedVersionsFromTool(versions),
		},
		Output: &fingerprintv1.Output{
			Hash:  outputHash,
			Files: files,
		},
	}

	// Write-skip rule (ADR-0009 §"byte stability"): if a record already exists at
	// this key with the same output identity (hash + per-file (path, hash) set),
	// the existing entry is still semantically correct. Skip the rewrite so
	// proto runtime byte-level drift never reaches git and the informational
	// resolved_versions[*].source field keeps its first-observed value. The
	// initial-creation-time concern that motivated keeping generated_at stable
	// has migrated to the filename's timestamp prefix (ADR-0010), which the
	// Storage backend preserves on in-place overwrites.
	if existing != nil && outputsEquivalent(existing.GetOutput(), newRec.GetOutput()) {
		return nil
	}

	if err := r.fingerprintStore(ctx, key, newRec); err != nil {
		return fmt.Errorf("%s: save record: %w", t.Name, err)
	}
	return nil
}

// fingerprintLookup resolves the record for key, preferring the prefetched
// map (zero RTT) and falling back to a live Storage.Load only when the
// prefetch never queried this key. Returns (hit, existing, paths, err)
// where:
//   - hit=true only when a record exists AND its output files still hash
//     to the recorded value; paths is the recorded output paths so the
//     caller can skip exec.
//   - hit=false otherwise; existing may still be non-nil if a stale
//     record was loaded (the caller uses it for the post-exec
//     write-skip check against the freshly-built record's output
//     identity).
//
// The span's sloff.fingerprint.state attribute records the resolution
// path (hit / fallback_hit / stale / not_found / error) so trace
// consumers can analyse cache health and the fallback rate without
// re-running.
func (r *Runner) fingerprintLookup(ctx context.Context, key fingerprint.Key) (hit bool, existing *fingerprintv1.Record, paths []string, err error) {
	_, span := r.tracer.Start(ctx, "runner.fingerprint.load")
	defer func() {
		if err != nil {
			span.SetAttributes(attribute.String("sloff.fingerprint.state", "error"))
		}
		endSpan(span, &err)
	}()

	rec, ok, err := r.lookupRecord(ctx, key, span)
	if err != nil {
		return false, nil, nil, err
	}
	if !ok {
		span.SetAttributes(attribute.String("sloff.fingerprint.state", "not_found"))
		return false, nil, nil, nil
	}
	candidate := fingerprint.FilePaths(rec.GetOutput().GetFiles())
	current, hashErr := hash.Files(r.opts.RepoRoot, candidate)
	if hashErr == nil && current == rec.GetOutput().GetHash() {
		// The state attribute may already have been set to "fallback_hit"
		// inside lookupRecord; the output-comparison promotion to "hit"
		// distinguishes "served from prefetch map" from "served from live
		// Storage.Load + matched output".
		span.SetAttributes(attribute.String("sloff.fingerprint.state", "hit"))
		return true, rec, candidate, nil
	}
	span.SetAttributes(attribute.String("sloff.fingerprint.state", "stale"))
	return false, rec, nil, nil
}

// lookupRecord fetches the record for key from the prefetched map, falling
// back to Storage.Load when the prefetch never asked about this key
// (typical when an upstream task regenerated outputs mid-run and the
// downstream task's actual input_hash diverges from the optimistic one
// computed pre-run). When the prefetch *did* ask and got nothing back,
// the absence is treated as authoritative — no live Load is issued —
// because the backend already told us during prefetch that this key has
// no record.
func (r *Runner) lookupRecord(ctx context.Context, key fingerprint.Key, span trace.Span) (*fingerprintv1.Record, bool, error) {
	if rec, ok := r.prefetched[key]; ok {
		return rec, true, nil
	}
	if _, queried := r.prefetchedKeys[key]; queried {
		return nil, false, nil
	}
	rec, ok, err := r.opts.Storage.Load(ctx, key)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	span.SetAttributes(attribute.String("sloff.fingerprint.state", "fallback_hit"))
	return rec, true, nil
}

// fingerprintStore appends the record to the run-level pending queue. The
// real Storage.SaveMany call happens in flushFingerprints at the end of Run.
// Deferring the write is what lets remote backends amortise per-task RTT
// over a single batch; for local backend the difference is invisible.
func (r *Runner) fingerprintStore(ctx context.Context, key fingerprint.Key, rec *fingerprintv1.Record) (err error) {
	_, span := r.tracer.Start(ctx, "runner.fingerprint.queue", trace.WithAttributes(
		attribute.Int("sloff.output.file_count", len(rec.GetOutput().GetFiles())),
	))
	defer endSpan(span, &err)
	r.pendingMu.Lock()
	r.pending = append(r.pending, fingerprint.KeyRecord{Key: key, Record: rec})
	r.pendingMu.Unlock()
	return nil
}

// outputsEquivalent reports whether two Output values represent the same
// produced file set. Hash and per-entry (path, hash) tuples must match; field
// order is normalised by sorting because callers may build Output before the
// proto Marshal helper sorts FileEntries.
func outputsEquivalent(a, b *fingerprintv1.Output) bool {
	if a.GetHash() != b.GetHash() {
		return false
	}
	if len(a.GetFiles()) != len(b.GetFiles()) {
		return false
	}
	left := append([]*fingerprintv1.FileEntry(nil), a.GetFiles()...)
	right := append([]*fingerprintv1.FileEntry(nil), b.GetFiles()...)
	sort.Slice(left, func(i, j int) bool { return left[i].GetPath() < left[j].GetPath() })
	sort.Slice(right, func(i, j int) bool { return right[i].GetPath() < right[j].GetPath() })
	for i := range left {
		if left[i].GetPath() != right[i].GetPath() || left[i].GetHash() != right[i].GetHash() {
			return false
		}
	}
	return true
}

// recordProducedPaths registers the resolved output paths of a task and fails when one
// of those paths was already produced by a different task in this run. This catches spec
// conflicts that depgraph cannot see at planning time on a clean checkout, where the
// pre-run glob expansion of generated files comes back empty. Protected by producedByMu
// so concurrent runTask goroutines don't race on the shared map.
func (r *Runner) recordProducedPaths(taskLabel string, paths []string) error {
	r.producedByMu.Lock()
	defer r.producedByMu.Unlock()
	for _, p := range paths {
		if existing, exists := r.producedBy[p]; exists && existing != taskLabel {
			return fmt.Errorf("duplicate output %q produced by %s and %s; fix the spec to give each generated path exactly one writer", p, existing, taskLabel)
		}
		r.producedBy[p] = taskLabel
	}
	return nil
}

func taskLabel(t depgraph.Task) string {
	if t.SpecRelpath == "" {
		return t.Name
	}
	return t.SpecRelpath + ":" + t.Name
}

// resolveOutputs re-expands every declared output pattern after execution and fails when
// the union of matches is empty. A successful run that produced no declared outputs would
// otherwise be persisted as a fingerprint with an empty file set, letting subsequent
// fingerprint hits permanently mask the broken generator. Individual patterns are allowed to
// resolve to zero files (conditional artifacts), so long as some pattern produced output.
func (r *Runner) resolveOutputs(info taskInfo) ([]string, error) {
	outputs, err := glob.Expand(r.opts.RepoRoot, info.specRelpath, info.outputPatterns)
	if err != nil {
		return nil, fmt.Errorf("re-expand outputs: %w", err)
	}
	if len(outputs) == 0 {
		return nil, fmt.Errorf("declared outputs %v produced no files", info.outputPatterns)
	}
	return outputs, nil
}

func (r *Runner) execCmd(ctx context.Context, info taskInfo) (err error) {
	ctx, span := r.tracer.Start(ctx, "runner.task.exec")
	defer endSpan(span, &err)

	if len(info.command.Cmd) == 0 {
		return fmt.Errorf("empty cmd")
	}
	span.SetAttributes(
		attribute.String("sloff.cmd", info.command.Cmd[0]),
		attribute.Int("sloff.cmd.argv_count", len(info.command.Cmd)),
	)
	cmd := exec.CommandContext(ctx, info.command.Cmd[0], info.command.Cmd[1:]...)
	cmd.Dir = filepath.Join(r.opts.RepoRoot, info.specRelpath)
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	cmd.Env = childEnv(os.Environ())
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		span.SetAttributes(attribute.Int("process.exit_code", ee.ExitCode()))
	} else if err == nil {
		span.SetAttributes(attribute.Int("process.exit_code", 0))
	}
	return err
}

// childEnv returns env entries with SLOFF_OTEL_* keys removed. ADR-0009 D2'
// scopes the SLOFF_-prefixed tracing config to the **current** sloff run; if a
// task cmd happens to invoke another `sloff` (or any tool that honors the same
// prefix), the child should not silently inherit the parent's silence /
// endpoint overrides. Standard OTEL_* keys still flow through so otel-aware
// codegen tools see whatever the user's shell configured.
func childEnv(parent []string) []string {
	out := make([]string, 0, len(parent))
	for _, kv := range parent {
		if strings.HasPrefix(kv, "SLOFF_OTEL_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (r *Runner) reportPreflightIssues(issues []preflight.Issue) {
	for _, i := range issues {
		r.logger.Errorf("preflight [%s] %s -- run: %s", i.Channel, i.Detail, i.Suggestion)
	}
}

// Helpers ------------------------------------------------------------------

// toolresolverDeclared bridges spec.DeclaredTool (the YAML-parsed shape) to
// toolresolver.DeclaredTool (the resolver dispatch shape). The two types stay
// separate so the spec package doesn't depend on toolresolver, but the field
// set is structurally identical.
func toolresolverDeclared(t spec.DeclaredTool) toolresolver.DeclaredTool {
	return toolresolver.DeclaredTool{
		Resolver:    t.Resolver,
		Exec:        t.Exec,
		Extract:     t.Extract,
		Entry:       t.Entry,
		PackageName: t.PackageName,
	}
}

func versionStrings(versions []toolresolver.ResolvedVersion) []string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.Version
	}
	return out
}

func resolvedVersionsFromTool(versions []toolresolver.ResolvedVersion) []*fingerprintv1.ResolvedVersion {
	if len(versions) == 0 {
		return nil
	}
	out := make([]*fingerprintv1.ResolvedVersion, len(versions))
	for i, v := range versions {
		out[i] = &fingerprintv1.ResolvedVersion{Name: v.Name, Source: v.Source, Version: v.Version}
	}
	return out
}

func perFileHashes(root string, paths []string) ([]*fingerprintv1.FileEntry, error) {
	out := make([]*fingerprintv1.FileEntry, 0, len(paths))
	for _, p := range paths {
		h, err := hash.File(root, p)
		if err != nil {
			return nil, err
		}
		out = append(out, &fingerprintv1.FileEntry{Path: p, Hash: h})
	}
	return out, nil
}
