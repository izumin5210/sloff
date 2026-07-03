package runner

import (
	"fmt"
	"path"
	"path/filepath"

	"github.com/izumin5210/sloff/internal/sloff/glob"
	"github.com/izumin5210/sloff/internal/sloff/spec"
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
