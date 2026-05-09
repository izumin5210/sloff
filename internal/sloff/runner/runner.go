// Package runner orchestrates spec discovery, preflight, dependency-graph derivation
// and per-task cache lookup/execute/write. It is the integration point for the
// foundation packages (spec / glob / hash / cache / depgraph / toolresolver / preflight).
package runner

import (
	"context"
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

	"golang.org/x/sync/errgroup"

	"github.com/izumin5210/sloff/internal/sloff/cache"
	"github.com/izumin5210/sloff/internal/sloff/depgraph"
	"github.com/izumin5210/sloff/internal/sloff/glob"
	"github.com/izumin5210/sloff/internal/sloff/hash"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
)

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
	Storage   cache.Storage
	Resolvers *toolresolver.Registry
	Preflight *preflight.Registry

	// ReadOnly suppresses Storage.Save (used when SLOFF_ALLOW_STALE_DEPS=1).
	ReadOnly bool

	// Stdout/Stderr are forwarded to spawned processes; nil falls back to os.Stdout / os.Stderr.
	Stdout io.Writer
	Stderr io.Writer

	Logger Logger

	// Clock supplies the timestamp written to record.GeneratedAt. Defaults to
	// time.Now().UTC(); tests inject a fixed clock so cache YAML is byte-deterministic.
	Clock func() time.Time
}

// Runner executes all discovered specs in topological order with cache lookup and
// output-comparison invalidation.
type Runner struct {
	opts   Options
	logger Logger
	stdout io.Writer
	stderr io.Writer
	byKey  map[string]taskInfo // depgraph.Task key → taskInfo, filled by collectTasks

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
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	return &Runner{opts: opts, logger: logger, stdout: stdout, stderr: stderr}
}

// Run executes preflight then every task. Errors during preflight or task execution
// abort the run.
func (r *Runner) Run(ctx context.Context) error {
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

	tasks, err := r.collectTasks(inputsByTool, versionsByTool)
	if err != nil {
		return err
	}
	ordered, err := depgraph.Build(tasks)
	if err != nil {
		return err
	}

	r.producedBy = map[string]string{}
	return r.runTasks(ctx, ordered)
}

// runTasks executes the topologically-ordered task list with bounded
// concurrency. A task starts as soon as every task that produces one of its
// inputs has finished — depgraph already sorted them, so we only need to
// re-derive each task's predecessor set from the same output→producer mapping
// it used. Independent tasks (the common case for cache-hit runs across
// service-local gen-db, where each spec's outputs sit in its own service dir)
// fan out across NumCPU workers; tasks with real producer→consumer chains
// (buf-default → buf-custom, build-protoc-plugins → buf-custom, …) still
// serialise inside the chain.
//
// First failure short-circuits the run: dependents of a failed task are not
// scheduled and the first non-context error is returned. Cancellation is
// observed before each task starts so a Ctrl-C drops in-flight tasks at the
// next scheduling boundary rather than waiting for the whole queue to drain.
func (r *Runner) runTasks(ctx context.Context, ordered []depgraph.Task) error {
	if len(ordered) == 0 {
		return nil
	}

	predecessors := taskPredecessorIndices(ordered)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(taskConcurrency(len(ordered)))

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
	return g.Wait()
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
// re-hash); cache-miss tasks spawn a child generator. NumCPU is the same
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
// the depgraph never reads ToolVersions — they only feed `tools_hash`
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
	tasks, err := r.collectTasks(inputsByTool, nil)
	if err != nil {
		return nil, err
	}
	return depgraph.Build(tasks)
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
// via SLOFF_ALLOW_STALE_DEPS, degrades to read-only so cache records
// are not written for a known-suspect run. Hard errors from a checker
// (the check itself couldn't execute) bypass the read-only fall-through
// and fail the run regardless.
func (r *Runner) runPreflight(ctx context.Context, registry *spec.ToolRegistry, referencedToolNames []string) error {
	if r.opts.Preflight == nil {
		return nil
	}
	checkers := scopeCheckers(r.opts.Preflight.Names(), registry, referencedToolNames)
	if len(checkers) == 0 {
		return nil
	}
	res, err := r.opts.Preflight.Run(ctx, ".", checkers)
	if err != nil {
		return err
	}
	if !res.OK {
		r.reportPreflightIssues(res.Issues)
		if !r.opts.ReadOnly {
			return fmt.Errorf("preflight failed (%d issues); set SLOFF_ALLOW_STALE_DEPS=1 to bypass", len(res.Issues))
		}
		r.logger.Warnf("preflight issues ignored due to ReadOnly mode; cache records will not be written")
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
func (r *Runner) resolveInputContribs(ctx context.Context, registry *spec.ToolRegistry, referenced []string) (map[string][]string, error) {
	results := make([][]string, len(referenced))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(resolverConcurrency(len(referenced)))
	for i, name := range referenced {
		entry, ok := registry.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("runner: referenced tool %q missing from registry; ValidateToolReferences should have caught this", name)
		}
		declared := []toolresolver.DeclaredTool{toolresolverDeclared(entry.Declared)}
		g.Go(func() error {
			ins, err := r.opts.Resolvers.Inputs(gctx, entry.SpecDir, declared)
			if err != nil {
				return fmt.Errorf("resolve inputs for tool %q (defined in %s): %w", entry.Name, entry.SpecDir, err)
			}
			results[i] = ins
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(referenced))
	for i, name := range referenced {
		out[name] = results[i]
	}
	return out, nil
}

// resolveVersionContribs invokes Registry.Versions once per referenced tool
// name. Same scoping discipline as resolveInputContribs.
func (r *Runner) resolveVersionContribs(ctx context.Context, registry *spec.ToolRegistry, referenced []string) (map[string][]toolresolver.ToolVersion, error) {
	results := make([][]toolresolver.ToolVersion, len(referenced))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(resolverConcurrency(len(referenced)))
	for i, name := range referenced {
		entry, ok := registry.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("runner: referenced tool %q missing from registry; ValidateToolReferences should have caught this", name)
		}
		declared := []toolresolver.DeclaredTool{toolresolverDeclared(entry.Declared)}
		g.Go(func() error {
			vs, err := r.opts.Resolvers.Versions(gctx, entry.SpecDir, declared)
			if err != nil {
				return fmt.Errorf("resolve versions for tool %q (defined in %s): %w", entry.Name, entry.SpecDir, err)
			}
			results[i] = vs
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make(map[string][]toolresolver.ToolVersion, len(referenced))
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
	// versions holds the per-task ToolVersion concatenation in tools[] order,
	// pre-computed during collectTasks so runTask can hash without revisiting
	// the resolver registry. nil when collectTasks ran without versions
	// (depgraph-only callers).
	versions []toolresolver.ToolVersion
}

// collectTasks expands inputs/outputs for every spec command and folds each
// task's referenced tools' contributions into the task's input set. Folding
// extras in here is what lets depgraph wire up workspace-tool build tasks to
// their consumers via the usual output-overlap rule, instead of needing a
// parallel dependency channel.
//
// versionsByTool may be nil for callers that don't need tools_hash (graph-
// style consumers); inputsByTool must always be present so depgraph sees the
// same inputs the runner would.
func (r *Runner) collectTasks(inputsByTool map[string][]string, versionsByTool map[string][]toolresolver.ToolVersion) ([]depgraph.Task, error) {
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

			var versions []toolresolver.ToolVersion
			if versionsByTool != nil {
				versions = combineToolVersions(c.Tools, versionsByTool)
			}

			t := depgraph.Task{
				SpecRelpath: sp.Dir,
				Name:        c.Name,
				Inputs:      mergedInputs,
				Outputs:     outputs,
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

// combineToolVersions is the Versions sibling of combineToolInputs.
func combineToolVersions(names []string, versionsByTool map[string][]toolresolver.ToolVersion) []toolresolver.ToolVersion {
	var combined []toolresolver.ToolVersion
	for _, name := range names {
		v, ok := versionsByTool[name]
		if !ok {
			panic(fmt.Sprintf("runner: tool %q missing from resolved versions map; ValidateToolReferences should have caught this", name))
		}
		combined = append(combined, v...)
	}
	return combined
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

func (r *Runner) runTask(ctx context.Context, t depgraph.Task) error {
	info := r.byKey[depgraphKey(t)]
	versions := info.versions

	filesHash, err := hash.Files(r.opts.RepoRoot, info.inputPaths)
	if err != nil {
		return fmt.Errorf("%s: hash inputs: %w", t.Name, err)
	}
	cmdHash := hash.Cmd(info.command.Cmd)
	toolsHash := hash.Tools(versionStrings(versions))
	inputHash := hash.Input(filesHash, cmdHash, toolsHash)

	key := cache.Key{SpecRelpath: t.SpecRelpath, TaskID: t.Name, InputHash: inputHash}

	taskLabel := taskLabel(t)
	if rec, ok, err := r.opts.Storage.Load(ctx, key); err != nil {
		return fmt.Errorf("%s: load record: %w", t.Name, err)
	} else if ok {
		paths := rec.Output.Files.Paths()
		current, err := hash.Files(r.opts.RepoRoot, paths)
		if err == nil && current == rec.Output.Hash {
			if err := r.recordProducedPaths(taskLabel, paths); err != nil {
				return err
			}
			r.logger.Infof("SKIP %s/%s (cache hit)", t.SpecRelpath, t.Name)
			return nil
		}
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

	newRec := &cache.Record{
		GeneratedAt:              r.opts.Clock(),
		GeneratorVersionSnapshot: snapshotFromVersions(versions),
		Input: cache.Input{
			Hash: inputHash,
			Components: cache.InputComponents{
				CmdHash:   cmdHash,
				FilesHash: filesHash,
				ToolsHash: toolsHash,
			},
		},
		Output: cache.Output{
			Hash:  outputHash,
			Files: files,
		},
		SchemaVersion: cache.SchemaVersion,
		Spec: cache.RecordSpec{
			Cmd:    strings.Join(info.command.Cmd, " "),
			Dir:    info.specRelpath,
			TaskID: info.command.Name,
		},
	}
	if err := r.opts.Storage.Save(ctx, key, newRec); err != nil {
		return fmt.Errorf("%s: save record: %w", t.Name, err)
	}
	return nil
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
// otherwise be persisted as a cache record with an empty file set, letting subsequent
// cache hits permanently mask the broken generator. Individual patterns are allowed to
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

func (r *Runner) execCmd(ctx context.Context, info taskInfo) error {
	if len(info.command.Cmd) == 0 {
		return fmt.Errorf("empty cmd")
	}
	cmd := exec.CommandContext(ctx, info.command.Cmd[0], info.command.Cmd[1:]...)
	cmd.Dir = filepath.Join(r.opts.RepoRoot, info.specRelpath)
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	cmd.Env = os.Environ()
	return cmd.Run()
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

func versionStrings(versions []toolresolver.ToolVersion) []string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.Version
	}
	return out
}

func snapshotFromVersions(versions []toolresolver.ToolVersion) cache.GeneratorVersions {
	if len(versions) == 0 {
		return nil
	}
	out := make(cache.GeneratorVersions, len(versions))
	for i, v := range versions {
		out[i] = cache.GeneratorVersion{Name: v.Name, Source: v.Source, Version: v.Version}
	}
	return out
}

func perFileHashes(root string, paths []string) (cache.FileHashes, error) {
	out := make(cache.FileHashes, 0, len(paths))
	for _, p := range paths {
		h, err := hash.File(root, p)
		if err != nil {
			return nil, err
		}
		out = append(out, cache.FileHash{Path: p, Hash: h})
	}
	return out, nil
}
