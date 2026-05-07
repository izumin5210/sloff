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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/izumin5210/lazygen/internal/lazygen/cache"
	"github.com/izumin5210/lazygen/internal/lazygen/depgraph"
	"github.com/izumin5210/lazygen/internal/lazygen/glob"
	"github.com/izumin5210/lazygen/internal/lazygen/hash"
	"github.com/izumin5210/lazygen/internal/lazygen/preflight"
	"github.com/izumin5210/lazygen/internal/lazygen/spec"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
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

	// ReadOnly suppresses Storage.Save (used when LAZYGEN_ALLOW_STALE_DEPS=1).
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
	opts     Options
	logger   Logger
	stdout   io.Writer
	stderr   io.Writer
	checkers []string            // resolver names of registered preflight checkers (run all in MVP)
	byKey    map[string]taskInfo // depgraph.Task key → taskInfo, filled by collectTasks

	// producedBy maps each resolved output path to the task that produced it. Cross-task
	// duplicate writes are spec conflicts that depgraph cannot catch on a clean checkout
	// (pre-execution globs are empty), so the runner records writers as runs progress and
	// fails the run if a later task lands on a path another task already produced.
	producedBy map[string]string
}

// New constructs a Runner.
func New(opts Options) *Runner {
	logger := opts.Logger
	if logger == nil {
		logger = stdLogger{l: log.New(os.Stderr, "lazygen ", log.LstdFlags)}
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
	r := &Runner{opts: opts, logger: logger, stdout: stdout, stderr: stderr}

	// PR1: run every registered checker. Future PRs will scope by used resolvers.
	if opts.Preflight != nil {
		for _, name := range opts.Preflight.Names() {
			r.checkers = append(r.checkers, name)
		}
	}
	return r
}

// Run executes preflight then every task. Errors during preflight or task execution
// abort the run.
func (r *Runner) Run(ctx context.Context) error {
	if r.opts.Preflight != nil && len(r.checkers) > 0 {
		res, err := r.opts.Preflight.Run(ctx, ".", r.checkers)
		if err != nil {
			return err
		}
		if !res.OK {
			r.reportPreflightIssues(res.Issues)
			if !r.opts.ReadOnly {
				return fmt.Errorf("preflight failed (%d issues); set LAZYGEN_ALLOW_STALE_DEPS=1 to bypass", len(res.Issues))
			}
			r.logger.Warnf("preflight issues ignored due to ReadOnly mode; cache records will not be written")
		}
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
	registry, err := spec.BuildToolRegistry(r.opts.Specs)
	if err != nil {
		return err
	}
	if err := spec.ValidateToolReferences(r.opts.Specs, registry); err != nil {
		return err
	}
	resolved, err := r.resolveReferencedTools(ctx, registry, referencedTools(r.opts.Specs))
	if err != nil {
		return err
	}

	tasks, err := r.collectTasks(resolved)
	if err != nil {
		return err
	}
	ordered, err := depgraph.Build(tasks)
	if err != nil {
		return err
	}

	r.producedBy = map[string]string{}
	for _, t := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.runTask(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// resolveReferencedTools invokes the toolresolver registry once per name in
// referenced and returns a name → Result map. specDir for each invocation is
// the dir where the tool was *defined* (ADR-0008 D3), not where it's
// referenced from, so tool definitions stay self-contained relative to their
// host lazygen.yml.
//
// Names not in the registry have already been rejected by
// ValidateToolReferences, so this loop assumes every name resolves; a missing
// entry would indicate a programmer error elsewhere and we surface it as
// such rather than silently dropping the contribution.
func (r *Runner) resolveReferencedTools(ctx context.Context, registry *spec.ToolRegistry, referenced []string) (map[string]toolresolver.Result, error) {
	out := make(map[string]toolresolver.Result, len(referenced))
	for _, name := range referenced {
		entry, ok := registry.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("runner: referenced tool %q missing from registry; ValidateToolReferences should have caught this", name)
		}
		declared := []toolresolver.DeclaredTool{toolresolverDeclared(entry.Declared)}
		res, err := r.opts.Resolvers.Resolve(ctx, entry.SpecDir, nil, declared)
		if err != nil {
			return nil, fmt.Errorf("resolve tool %q (defined in %s): %w", entry.Name, entry.SpecDir, err)
		}
		out[entry.Name] = res
	}
	return out, nil
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
	// resolverResult is computed once per task during collectTasks so depgraph
	// sees resolver-contributed extra inputs (e.g. pnpm-local pulls workspace
	// tool sources) and runTask can hash without re-running the resolver.
	resolverResult toolresolver.Result
}

// collectTasks expands inputs/outputs for every spec command and folds each
// task's referenced tools' contributions (from the pre-resolved per-tool
// cache) into the task's input set. Folding extras in here is what lets
// depgraph wire up workspace-tool build tasks to their consumers via the
// usual output-overlap rule, instead of needing a parallel dependency channel.
func (r *Runner) collectTasks(resolved map[string]toolresolver.Result) ([]depgraph.Task, error) {
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

			combined := combineToolResults(c.Tools, resolved)
			mergedInputs := mergeInputs(inputs, combined.ExtraInputs)

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
				resolverResult: combined,
			}
		}
	}
	return tasks, nil
}

// combineToolResults concatenates Versions and ExtraInputs in the order tools
// appear in the spec's tools[] list, mirroring the previous inline behaviour
// where dispatch order followed declaration order. ValidateToolReferences has
// already guaranteed every name resolves, so a missing entry here is a
// programmer error and we panic to surface it loudly during tests.
func combineToolResults(names []string, resolved map[string]toolresolver.Result) toolresolver.Result {
	var combined toolresolver.Result
	for _, name := range names {
		r, ok := resolved[name]
		if !ok {
			panic(fmt.Sprintf("runner: tool %q missing from resolved registry; ValidateToolReferences should have caught this", name))
		}
		combined.Versions = append(combined.Versions, r.Versions...)
		combined.ExtraInputs = append(combined.ExtraInputs, r.ExtraInputs...)
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
	versions := info.resolverResult.Versions

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

	rec := &cache.Record{
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
	if err := r.opts.Storage.Save(ctx, key, rec); err != nil {
		return fmt.Errorf("%s: save record: %w", t.Name, err)
	}
	return nil
}

// recordProducedPaths registers the resolved output paths of a task and fails when one
// of those paths was already produced by a different task in this run. This catches spec
// conflicts that depgraph cannot see at planning time on a clean checkout, where the
// pre-run glob expansion of generated files comes back empty.
func (r *Runner) recordProducedPaths(taskLabel string, paths []string) error {
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
