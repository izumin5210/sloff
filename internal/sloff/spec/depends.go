package spec

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/izumin5210/sloff/internal/sloff/glob"
)

// dependPatternMeta are the glob metacharacters that distinguish a depends
// pattern from a literal task name. Task names (ADR-0008 D4) are restricted to
// [a-z0-9_-], so the presence of any of these is an unambiguous signal that the
// value is a pattern (ADR-0016 D1) — no separate syntax is needed.
const dependPatternMeta = "*?["

// IsDependPattern reports whether a depends `task` value is a glob pattern
// rather than a literal task name (ADR-0016 D1).
func IsDependPattern(task string) bool {
	return strings.ContainsAny(task, dependPatternMeta)
}

func commandHasDependPattern(deps []Depend) bool {
	for _, d := range deps {
		if IsDependPattern(d.Task) {
			return true
		}
	}
	return false
}

// ExpandedPattern records the literal edges one depends pattern entry expanded
// to for a single consumer command (ADR-0016 D4). The runner uses it to
// aggregate the inputs-omission warning (ADR-0013 D3) per pattern instead of
// per expanded edge: a deliberate "depend on the whole group" should not emit
// one warning per matched task. Edges are the full match set the pattern
// produced (before deduplication against literals or other patterns), so the
// warning can judge whether the pattern as a whole is observed.
type ExpandedPattern struct {
	ConsumerDir  string   // OS-native spec dir of the consumer command
	ConsumerName string   // consumer command name
	Pattern      string   // the original glob, for diagnostics
	Edges        []Depend // literal {Spec, Task} edges this pattern matched
}

// ExpandDependPatterns rewrites every commands[*].depends entry whose task is a
// glob pattern (ADR-0016 D1) into the literal {spec, task} edges it matches in
// the referenced spec, so every later pass (ValidateDependReferences, depgraph
// construction, overlap validation) sees only literal depends and needs no
// change (ADR-0016 D2/E).
//
// It must run after command_providers are expanded (ADR-0015 D2) so a pattern
// can match dynamically generated tasks. Matching is done against the task
// names of the single spec the entry's `spec` field resolves to; the declaring
// command is excluded from its own pattern, a pattern that matches nothing is
// an error, and the result is deterministic regardless of match order (ADR-0016
// D3). Specs without any pattern depends are returned unchanged (shared *File
// pointers are not copied); only commands that actually carry a pattern get a
// rewritten Depends slice.
//
// The second return value is the per-pattern provenance the runner needs for
// the aggregated inputs-omission warning; callers that do not need it can
// ignore it.
func ExpandDependPatterns(specs []Spec) ([]Spec, []ExpandedPattern, error) {
	anyPattern := false
	for _, sp := range specs {
		if specHasDependPattern(sp) {
			anyPattern = true
			break
		}
	}
	if !anyPattern {
		return specs, nil, nil
	}

	// Index task names by their spec dir (slash form), sorted so expansion is
	// independent of spec/command discovery order (ADR-0016 D3, R2).
	namesByDir := map[string][]string{}
	for _, sp := range specs {
		dir := filepath.ToSlash(sp.Dir)
		for _, c := range sp.File.Commands {
			namesByDir[dir] = append(namesByDir[dir], c.Name)
		}
	}
	for dir := range namesByDir {
		sort.Strings(namesByDir[dir])
	}

	var provenance []ExpandedPattern
	out := make([]Spec, 0, len(specs))
	for _, sp := range specs {
		if !specHasDependPattern(sp) {
			out = append(out, sp)
			continue
		}
		declDir := filepath.ToSlash(sp.Dir)
		newCmds := append([]Command(nil), sp.File.Commands...)
		for ci := range newCmds {
			c := newCmds[ci]
			if !commandHasDependPattern(c.Depends) {
				continue
			}
			expanded, groups, err := expandCommandDepends(sp.Dir, declDir, c, namesByDir)
			if err != nil {
				return nil, nil, err
			}
			c.Depends = expanded
			newCmds[ci] = c
			provenance = append(provenance, groups...)
		}
		newFile := *sp.File
		newFile.Commands = newCmds
		out = append(out, Spec{Dir: sp.Dir, Path: sp.Path, File: &newFile})
	}
	return out, provenance, nil
}

func specHasDependPattern(sp Spec) bool {
	for _, c := range sp.File.Commands {
		if commandHasDependPattern(c.Depends) {
			return true
		}
	}
	return false
}

// edgeKey identifies a depends edge by its resolved target dir and task, the
// same identity ValidateDependReferences uses to detect duplicates.
type edgeKey struct{ dir, task string }

// expandCommandDepends turns one command's mixed literal/pattern depends into a
// purely literal list. Literal entries are kept verbatim and in order
// (duplicate literals are left in place so ValidateDependReferences still flags
// them per ADR-0013 D1); pattern entries are expanded and appended, deduped
// against the literals and across patterns (ADR-0016 D3 union semantics).
func expandCommandDepends(declDirOS, declDir string, c Command, namesByDir map[string][]string) ([]Depend, []ExpandedPattern, error) {
	out := make([]Depend, 0, len(c.Depends))
	literal := map[edgeKey]struct{}{}
	for _, d := range c.Depends {
		if IsDependPattern(d.Task) {
			continue
		}
		out = append(out, d)
		literal[edgeKey{path.Join(declDir, d.Spec), d.Task}] = struct{}{}
	}

	added := map[edgeKey]struct{}{}
	var groups []ExpandedPattern
	for _, d := range c.Depends {
		if !IsDependPattern(d.Task) {
			continue
		}
		target := path.Join(declDir, d.Spec)
		if glob.EscapesRoot(target) {
			return nil, nil, fmt.Errorf("%s/%s: depends pattern %q: spec %q escapes repo root",
				registryDefinitionPath(declDirOS), c.Name, d.Task, d.Spec)
		}
		var matched []Depend
		for _, name := range namesByDir[target] {
			// A pattern means "every other matching task"; never let a command
			// depend on itself (ADR-0016 D3 / ADR-0013 self-reference rule).
			if target == declDir && name == c.Name {
				continue
			}
			ok, err := doublestar.Match(d.Task, name)
			if err != nil {
				return nil, nil, fmt.Errorf("%s/%s: depends pattern %q: %w",
					registryDefinitionPath(declDirOS), c.Name, d.Task, err)
			}
			if ok {
				matched = append(matched, Depend{Spec: d.Spec, Task: name})
			}
		}
		if len(matched) == 0 {
			return nil, nil, fmt.Errorf("%s/%s: depends pattern %q matched no task in spec dir %q",
				registryDefinitionPath(declDirOS), c.Name, d.Task, target)
		}
		for _, m := range matched {
			k := edgeKey{path.Join(declDir, m.Spec), m.Task}
			if _, dup := literal[k]; dup {
				continue
			}
			if _, dup := added[k]; dup {
				continue
			}
			added[k] = struct{}{}
			out = append(out, m)
		}
		groups = append(groups, ExpandedPattern{
			ConsumerDir:  declDirOS,
			ConsumerName: c.Name,
			Pattern:      d.Task,
			Edges:        matched,
		})
	}
	return out, groups, nil
}
