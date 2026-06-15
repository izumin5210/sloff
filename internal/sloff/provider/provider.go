// Package provider expands command_providers (ADR-0015): programs the runner
// execs at plan time whose stdout is a versioned JSON envelope of task
// definitions. Expand runs one provider and returns the generated commands as
// ordinary spec.Command values; the runner merges them into the command set
// before collectTasks, after which they are indistinguishable from
// hand-written tasks and flow through the same validation, depgraph, and
// fingerprint paths.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/izumin5210/sloff/internal/sloff/spec"
)

// SchemaVersion is the only provider-output schema this binary understands.
// Mirrors the fingerprint record policy (ADR-0009): an unknown version is a
// hard error rather than a best-effort read, so a forward-incompatible change
// fails loudly instead of silently dropping fields.
const SchemaVersion = "v1"

// output is the versioned JSON envelope a command provider prints to stdout.
type output struct {
	SchemaVersion string `json:"schema_version"`
	Tasks         []task `json:"tasks"`
}

// task is one entry of output.tasks, a 1:1 JSON mirror of spec.Command.
type task struct {
	Name    string   `json:"name"`
	Cmd     cmdLine  `json:"cmd"`
	Inputs  []string `json:"inputs"`
	Outputs []string `json:"outputs"`
	Tools   []string `json:"tools"`
	Depends []depend `json:"depends"`
}

type depend struct {
	Spec string `json:"spec"`
	Task string `json:"task"`
}

// cmdLine accepts either a JSON string (whitespace-split into argv) or a JSON
// array of strings (taken as-is), matching spec.CmdLine's YAML behaviour so a
// provider can write cmd the same way a hand-authored sloff.yml does.
type cmdLine []string

func (c *cmdLine) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = strings.Fields(s)
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("cmd must be a string or a list of strings: %w", err)
	}
	*c = list
	return nil
}

// Expand runs decl.Exec with cwd = repoRoot/specDir and converts its stdout
// (a versioned JSON envelope) into spec.Command values sorted by name
// (ADR-0015 D2/D5). specDir is the path basis the runner later interprets the
// emitted inputs/outputs/depends against (ADR-0008 D3), so a provider writes
// those exactly like a hand-authored command in the same sloff.yml. Per-field
// validation, name uniqueness against the rest of the command set, tool/depends
// reference resolution, and glob-escape checks are deliberately left to the
// shared downstream passes the runner already runs on every command.
func Expand(ctx context.Context, repoRoot, specDir string, decl spec.CommandProviderDecl) ([]spec.Command, error) {
	if len(decl.Exec) == 0 {
		return nil, fmt.Errorf("command provider %q: exec is empty", decl.Name)
	}
	cmd := exec.CommandContext(ctx, decl.Exec[0], decl.Exec[1:]...)
	cmd.Dir = filepath.Join(repoRoot, specDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("command provider %q: exec failed: %w\n%s", decl.Name, err, msg)
		}
		return nil, fmt.Errorf("command provider %q: exec failed: %w", decl.Name, err)
	}
	return parse(decl.Name, stdout)
}

// parse decodes a provider's stdout into sorted spec.Command values. Split out
// from Expand so the JSON contract can be unit-tested without spawning a
// subprocess.
func parse(providerName string, stdout []byte) ([]spec.Command, error) {
	var out output
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("command provider %q: invalid JSON output: %w", providerName, err)
	}
	if out.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("command provider %q: unsupported schema_version %q (want %q)", providerName, out.SchemaVersion, SchemaVersion)
	}
	cmds := make([]spec.Command, 0, len(out.Tasks))
	for _, t := range out.Tasks {
		cmds = append(cmds, spec.Command{
			Name:    t.Name,
			Cmd:     spec.CmdLine(t.Cmd),
			Inputs:  t.Inputs,
			Outputs: t.Outputs,
			Tools:   t.Tools,
			Depends: toDepends(t.Depends),
		})
	}
	// Sort by name so the merged command set — and therefore every downstream
	// hash key and error message — is independent of the order the provider
	// happened to print its tasks (ADR-0015 D5, R2).
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	return cmds, nil
}

func toDepends(ds []depend) []spec.Depend {
	if len(ds) == 0 {
		return nil
	}
	out := make([]spec.Depend, len(ds))
	for i, d := range ds {
		out[i] = spec.Depend{Spec: d.Spec, Task: d.Task}
	}
	return out
}
