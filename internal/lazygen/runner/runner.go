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
}

// Runner executes all discovered specs in topological order with cache lookup and
// output-comparison invalidation.
type Runner struct {
	opts     Options
	logger   Logger
	stdout   io.Writer
	stderr   io.Writer
	checkers []string                // resolver names of registered preflight checkers (run all in MVP)
	byKey    map[string]taskInfo     // depgraph.Task key → taskInfo, filled by collectTasks
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

	tasks, err := r.collectTasks()
	if err != nil {
		return err
	}
	ordered, err := depgraph.Build(tasks)
	if err != nil {
		return err
	}

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

// taskInfo carries the bits of spec.Command needed to execute it. Stored on the depgraph.Task
// via the SpecRelpath/Name key — we look it back up by key when executing.
type taskInfo struct {
	specRelpath    string
	command        spec.Command
	inputPaths     []string
	outputPatterns []string
}

// collectTasks expands inputs/outputs for every spec command and returns the depgraph.Task slice.
func (r *Runner) collectTasks() ([]depgraph.Task, error) {
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
			t := depgraph.Task{
				SpecRelpath: sp.Dir,
				Name:        c.Name,
				Inputs:      inputs,
				Outputs:     outputs,
			}
			tasks = append(tasks, t)
			r.byKey[depgraphKey(t)] = taskInfo{
				specRelpath:    sp.Dir,
				command:        c,
				inputPaths:     inputs,
				outputPatterns: c.Outputs,
			}
		}
	}
	return tasks, nil
}

func depgraphKey(t depgraph.Task) string { return t.SpecRelpath + "\x00" + t.Name }

func (r *Runner) runTask(ctx context.Context, t depgraph.Task) error {
	info := r.byKey[depgraphKey(t)]

	versions, err := r.opts.Resolvers.Resolve(ctx, info.specRelpath, info.command.Cmd, declaredFromSpec(info.command.Tools))
	if err != nil {
		return fmt.Errorf("%s: resolve tools: %w", t.Name, err)
	}

	filesHash, err := hash.Files(r.opts.RepoRoot, info.inputPaths)
	if err != nil {
		return fmt.Errorf("%s: hash inputs: %w", t.Name, err)
	}
	cmdHash := hash.Cmd(info.command.Cmd)
	toolsHash := hash.Tools(versionStrings(versions))
	inputHash := hash.Input(filesHash, cmdHash, toolsHash)

	key := cache.Key{SpecRelpath: t.SpecRelpath, TaskID: t.Name, InputHash: inputHash}

	if rec, ok, err := r.opts.Storage.Load(ctx, key); err != nil {
		return fmt.Errorf("%s: load record: %w", t.Name, err)
	} else if ok {
		current, err := hash.Files(r.opts.RepoRoot, rec.Output.Files.Paths())
		if err == nil && current == rec.Output.Hash {
			r.logger.Infof("SKIP %s/%s (cache hit)", t.SpecRelpath, t.Name)
			return nil
		}
	}

	r.logger.Infof("RUN  %s/%s", t.SpecRelpath, t.Name)
	if err := r.execCmd(ctx, info); err != nil {
		return fmt.Errorf("%s: %w", t.Name, err)
	}

	outputPaths, err := glob.Expand(r.opts.RepoRoot, info.specRelpath, info.outputPatterns)
	if err != nil {
		return fmt.Errorf("%s: re-expand outputs: %w", t.Name, err)
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
		GeneratedAt:              time.Now().UTC(),
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

func declaredFromSpec(tools []spec.DeclaredTool) []toolresolver.DeclaredTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]toolresolver.DeclaredTool, len(tools))
	for i, t := range tools {
		out[i] = toolresolver.DeclaredTool{
			Resolver: t.Resolver,
			Exec:     t.Exec,
			Extract:  t.Extract,
		}
	}
	return out
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
