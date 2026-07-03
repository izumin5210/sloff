package runner

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
	"github.com/izumin5210/sloff/internal/sloff/glob"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
)

// injectToolDepends folds every referenced tool's bootstrap depends (ADR-0019
// D2) into the depends list of each task that lists the tool in tools[]. The
// injected entries are ordinary literal edges from here on: depgraph
// construction, the ADR-0013 D3 overlap checks, and graph rendering treat
// them exactly like hand-written depends.
//
// It runs after ValidateToolReferences (every tools[] name resolves) and
// before ValidateDependReferences, which would reject the duplicates the
// dedup below removes. Tool depends paths are declared relative to the
// tool-defining spec dir (ADR-0008 D3), so each entry is rebased onto the
// consumer's spec dir before it joins the consumer's depends.
//
// Only tools some command references are processed: a broken depends on an
// unreferenced catalog tool stays inert, mirroring how its resolver config
// is never validated either (ADR-0008).
func (r *Runner) injectToolDepends(registry *spec.ToolRegistry) error {
	type taskKey struct{ dir, name string }
	// Same index ValidateDependReferences builds. Existence is checked here,
	// not at spec load, so the error names the tool that declared the entry —
	// the later pass would blame the consumer task for an edge it never wrote.
	defined := map[taskKey]struct{}{}
	for _, sp := range r.opts.Specs {
		dir := filepath.ToSlash(sp.Dir)
		for _, c := range sp.File.Commands {
			defined[taskKey{dir, c.Name}] = struct{}{}
		}
	}

	// Immutable-copy discipline (see expandCommandProviders): specs without an
	// injection are passed through untouched, sharing their *File.
	out := make([]spec.Spec, 0, len(r.opts.Specs))
	for _, sp := range r.opts.Specs {
		consumerDir := filepath.ToSlash(sp.Dir)
		var newCmds []spec.Command
		for ci, c := range sp.File.Commands {
			if len(c.Tools) == 0 {
				continue
			}
			// Dedup key set seeded with the consumer's own edges, resolved to
			// the same (target dir, task) identity ValidateDependReferences
			// uses; injection must not re-add an edge the user already wrote
			// (backward compatibility with pre-ADR-0019 specs), nor add the
			// same edge twice when several of the task's tools declare it.
			seen := make(map[taskKey]struct{}, len(c.Depends))
			for _, d := range c.Depends {
				seen[taskKey{path.Join(consumerDir, d.Spec), d.Task}] = struct{}{}
			}
			var injected []spec.Depend
			for _, toolName := range c.Tools {
				entry, ok := registry.Lookup(toolName)
				if !ok {
					// ValidateToolReferences ran first; unreachable.
					continue
				}
				toolDir := filepath.ToSlash(entry.SpecDir)
				for i, d := range entry.Declared.Depends {
					target := path.Join(toolDir, d.Spec)
					if glob.EscapesRoot(target) {
						return fmt.Errorf("tool %q (defined in %s): depends[%d]: spec %q escapes repo root",
							toolName, providerDefinitionPath(entry.SpecDir), i, d.Spec)
					}
					if target == consumerDir && d.Task == c.Name {
						// ADR-0019 D2: the closure producer itself uses the
						// tool — bootstrapping is structurally impossible.
						// Failing loudly (never silently skipping the edge)
						// is what keeps the ordering guarantee honest.
						return fmt.Errorf("tool %q declares depends on %s:%s, but that task itself uses the tool; a tool's bootstrap producer cannot reference the tool (split the producing task so it does not use the tool)",
							toolName, target, d.Task)
					}
					if _, ok := defined[taskKey{target, d.Task}]; !ok {
						return fmt.Errorf("tool %q (defined in %s): depends[%d]: task %q not found in spec dir %q",
							toolName, providerDefinitionPath(entry.SpecDir), i, d.Task, target)
					}
					k := taskKey{target, d.Task}
					if _, dup := seen[k]; dup {
						continue
					}
					seen[k] = struct{}{}
					rel, err := relSpecDir(consumerDir, target)
					if err != nil {
						return fmt.Errorf("tool %q: rebase depends %s:%s onto %s: %w", toolName, target, d.Task, consumerDir, err)
					}
					injected = append(injected, spec.Depend{Spec: rel, Task: d.Task})
				}
			}
			if len(injected) == 0 {
				continue
			}
			if newCmds == nil {
				newCmds = append([]spec.Command(nil), sp.File.Commands...)
			}
			merged := append(append([]spec.Depend(nil), c.Depends...), injected...)
			cNew := newCmds[ci]
			cNew.Depends = merged
			newCmds[ci] = cNew
		}
		if newCmds == nil {
			out = append(out, sp)
			continue
		}
		newFile := *sp.File
		newFile.Commands = newCmds
		out = append(out, spec.Spec{Dir: sp.Dir, Path: sp.Path, File: &newFile})
	}
	r.opts.Specs = out
	return nil
}

// relSpecDir returns the slash-form relative path from one repo-relative spec
// dir to another ("." for the repo root on either side), i.e. the value a
// hand-written depends entry in fromDir would carry to reference toDir.
func relSpecDir(fromDirSlash, toDirSlash string) (string, error) {
	rel, err := filepath.Rel(filepath.FromSlash(fromDirSlash), filepath.FromSlash(toDirSlash))
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// deferredTool is one referenced tool demoted from eager resolution
// (ADR-0019 D3): its Inputs/Versions failed at run start, but the tool
// declared bootstrap depends, so the failure may just mean its sources have
// not been generated yet. The retry runs at most once per definitive outcome
// (ADR-0019 D4), triggered by the first consumer task to reach
// ensureToolsResolved; concurrent consumers wait on the in-flight attempt and
// observe the same outcome.
//
// Concurrency model: mu guards done/inflight/inputs/versions/err. A definitive
// outcome (success or real non-context error) sets done=true and is latched
// forever. A context error is not latched — the run is already shutting down
// and a later consumer with a live ctx should not observe a stale result.
type deferredTool struct {
	entry   spec.ToolEntry
	planErr error // the eager failure, kept for error attribution on retry failure

	mu       sync.Mutex
	cond     *sync.Cond // signals when the in-flight attempt finishes
	inflight bool       // true while one goroutine is executing the resolver calls
	done     bool       // true once a definitive (non-context) outcome is latched
	inputs   []string
	versions []toolresolver.ResolvedVersion
	err      error
}

// deferToolResolution demotes a failed eager resolution to deferred when the
// tool declared bootstrap depends (ADR-0019 D3). Returns false — leaving the
// caller to fail the run as before — for tools without a declaration, so
// typos and environment breakage keep dying at run start.
//
// Context cancellation is never a reason to demote: it is unrelated to whether
// the tool's declared depends have generated the sources it needs. A canceled
// context means the run is already shutting down; let the caller propagate the
// error immediately instead of hiding it behind a deferred retry that will also
// fail (or, worse, succeed with a stale context).
func (r *Runner) deferToolResolution(entry spec.ToolEntry, cause error, span trace.Span) bool {
	if len(entry.Declared.Depends) == 0 {
		return false
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return false
	}
	dt := &deferredTool{entry: entry, planErr: cause}
	dt.cond = sync.NewCond(&dt.mu)
	r.deferredMu.Lock()
	r.deferredTools[entry.Name] = dt
	r.deferredMu.Unlock()
	span.SetAttributes(attribute.Bool("sloff.tool.deferred", true))
	r.logger.Warnf("tool %q: resolution failed; deferred until its declared depends complete: %v", entry.Name, cause)
	return true
}

// ensureToolsResolved is the ADR-0019 D4 execution point. When any tool this
// task references was deferred, it (a) resolves each such tool — the
// injected edges guarantee the tool's declared depends completed before this
// task was scheduled — and (b) rebuilds this task's input surface and
// version set from the now-complete contribution set, so the input hash
// runTask computes next is identical to what an eager (warm) run produces
// (ADR-0019 D7: cold-written records must hit on warm runs).
//
// Concurrency: r.deferredTools is read-only after resolveContribs returns;
// each deferredTool resolves under its once. The rebuild writes only through
// this task's own *taskInfo — the byKey map is never mutated during the run,
// no other goroutine reads another task's info while runTasks is in flight,
// and the post-run validation passes read after the errgroup join, which
// establishes the happens-before. Hence no lock here.
func (r *Runner) ensureToolsResolved(ctx context.Context, t depgraph.Task, info *taskInfo) error {
	deferredUsed := false
	for _, name := range info.command.Tools {
		dt, ok := r.deferredTools[name]
		if !ok {
			continue
		}
		deferredUsed = true
		if err := dt.resolve(ctx, r); err != nil {
			return fmt.Errorf("%s: %w", t.Name, err)
		}
	}
	if !deferredUsed {
		return nil
	}

	// Re-concatenate every tool contribution in tools[] order — the same
	// order collectTasks used — swapping in the deferred results where the
	// eager maps hold the empty placeholder.
	var extras []string
	var versions []toolresolver.ResolvedVersion
	for _, name := range info.command.Tools {
		if dt, ok := r.deferredTools[name]; ok {
			extras = append(extras, dt.inputs...)
			versions = append(versions, dt.versions...)
			continue
		}
		extras = append(extras, r.inputsByTool[name]...)
		versions = append(versions, r.versionsByTool[name]...)
	}
	merged := mergeInputs(info.declaredInputs, extras)
	inputSet, joinedPatterns := inputSurface(info.specRelpath, info.command.Inputs, merged)
	info.inputPaths = merged
	info.versions = versions
	info.inputSet = inputSet
	info.joinedInputPatterns = joinedPatterns
	return nil
}

// resolve retries the deferred tool's Inputs/Versions. On a definitive failure
// the error names the tool and carries both the eager and the deferred cause:
// the pair is what distinguishes "depends didn't generate what the tool needs"
// (both causes alike) from an environment problem that appeared mid-run.
//
// Context errors (Canceled/DeadlineExceeded) are never latched as the
// definitive outcome. A context error means the run is shutting down due to an
// unrelated failure; wrapping it in attributedErr would misattribute a
// scheduling side-effect as a spec defect. The caller receives the raw context
// error (wrapped with the tool name) so errors.Is remains navigable, and a
// later consumer with a live ctx is not blocked by a frozen cancellation.
func (d *deferredTool) resolve(ctx context.Context, r *Runner) error {
	d.mu.Lock()
	// Fast-path: a definitive outcome (success or real failure) was already latched.
	if d.done {
		err := d.err
		d.mu.Unlock()
		return err
	}
	// If another goroutine is in flight, wait for it to finish. We may then
	// take the fast-path above (definitive outcome) or re-attempt ourselves if
	// the in-flight attempt ended with a context error.
	for d.inflight {
		d.cond.Wait()
	}
	if d.done {
		err := d.err
		d.mu.Unlock()
		return err
	}
	// We are the goroutine that will execute the resolver calls.
	d.inflight = true
	d.mu.Unlock()

	entry := d.entry
	// resolveToolContrib handles span creation, resolver calls, and success
	// count attributes; the span is returned open so we can record the error
	// status before closing.
	ins, vs, span, _, retErr := r.resolveToolContrib(ctx, entry, "deferred", true)

	// Determine whether the outcome is definitive.
	isCtxErr := errors.Is(retErr, context.Canceled) || errors.Is(retErr, context.DeadlineExceeded)

	var outcome, spanErr error
	if retErr == nil {
		r.logger.Infof("resolved tool %q after its declared depends completed", entry.Name)
	} else if !isCtxErr {
		// Real failure: compose the attributed error and latch it.
		outcome = d.attributedErr(retErr)
		spanErr = outcome
	} else {
		// Context error: return it transparently without latching.
		outcome = fmt.Errorf("tool %q: %w", entry.Name, retErr)
		spanErr = outcome
	}
	endSpan(span, &spanErr)

	d.mu.Lock()
	d.inflight = false
	if !isCtxErr {
		// Latch the definitive outcome (success or real error).
		d.done = true
		d.err = outcome
		d.inputs = ins
		d.versions = vs
	}
	d.cond.Broadcast()
	d.mu.Unlock()

	return outcome
}

// attributedErr composes the D4 failure message: tool as subject, both the
// run-start and the post-depends causes, and the most likely spec fix.
func (d *deferredTool) attributedErr(retryErr error) error {
	return fmt.Errorf("tool %q (defined in %s) could not be resolved: at run start: %v; retried after its declared depends completed: %w; a task generating the tool's sources may be missing from the tool's depends",
		d.entry.Name, providerDefinitionPath(d.entry.SpecDir), d.planErr, retryErr)
}
