package provider

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/spec"
)

func TestParse_ValidEnvelope(t *testing.T) {
	in := `{
	  "schema_version": "v1",
	  "tasks": [
	    {"name": "copy-b", "cmd": ["sh", "-c", "cp b.src b.out"], "inputs": ["b.src"], "outputs": ["b.out"], "tools": ["versioner"]},
	    {"name": "copy-a", "cmd": "cp a.src a.out", "inputs": ["a.src"], "outputs": ["a.out"], "tools": ["versioner"], "depends": [{"task": "copy-b"}]}
	  ]
	}`
	got, err := parse("gen", []byte(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []spec.Command{
		// Sorted by name: copy-a precedes copy-b regardless of emit order.
		{
			Name:    "copy-a",
			Cmd:     spec.CmdLine{"cp", "a.src", "a.out"}, // string form is whitespace-split
			Inputs:  []string{"a.src"},
			Outputs: []string{"a.out"},
			Tools:   []string{"versioner"},
			Depends: []spec.Depend{{Task: "copy-b"}},
		},
		{
			Name:    "copy-b",
			Cmd:     spec.CmdLine{"sh", "-c", "cp b.src b.out"}, // array form preserved verbatim
			Inputs:  []string{"b.src"},
			Outputs: []string{"b.out"},
			Tools:   []string{"versioner"},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("parse mismatch (-want +got):\n%s", diff)
	}
}

func TestParse_UnknownSchemaVersionFails(t *testing.T) {
	in := `{"schema_version": "v2", "tasks": []}`
	_, err := parse("gen", []byte(in))
	if err == nil {
		t.Fatal("expected error for unknown schema_version")
	}
	if !strings.Contains(err.Error(), "schema_version") || !strings.Contains(err.Error(), "v2") {
		t.Errorf("error should name the unsupported version, got: %v", err)
	}
}

func TestParse_InvalidJSONFails(t *testing.T) {
	_, err := parse("gen", []byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "gen") {
		t.Errorf("error should name the provider, got: %v", err)
	}
}

func TestParse_EmptyTasks(t *testing.T) {
	// An explicit empty list is a valid "no tasks" declaration.
	got, err := parse("gen", []byte(`{"schema_version": "v1", "tasks": []}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no commands, got %d", len(got))
	}
}

func TestParse_MissingTasksFails(t *testing.T) {
	// An absent or null "tasks" field must fail rather than be treated as an
	// empty set, otherwise a malformed provider silently suppresses codegen.
	for _, in := range []string{
		`{"schema_version": "v1"}`,
		`{"schema_version": "v1", "tasks": null}`,
	} {
		_, err := parse("gen", []byte(in))
		if err == nil {
			t.Fatalf("expected error for missing tasks in %s", in)
		}
		if !strings.Contains(err.Error(), "tasks") {
			t.Errorf("error should mention the tasks field, got: %v", err)
		}
	}
}

func TestExpand_ExecAndParse(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	decl := spec.CommandProviderDecl{
		Name: "gen",
		Exec: []string{"sh", "-c", `printf '{"schema_version":"v1","tasks":[{"name":"t","cmd":"true","inputs":["i"],"outputs":["o"],"tools":["v"]}]}'`},
	}
	cmds, err := Expand(context.Background(), t.TempDir(), ".", decl)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(cmds) != 1 || cmds[0].Name != "t" {
		t.Fatalf("unexpected commands: %+v", cmds)
	}
}

func TestExpand_ExecFailureSurfacesStderr(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	decl := spec.CommandProviderDecl{
		Name: "gen",
		Exec: []string{"sh", "-c", "echo boom >&2; exit 3"},
	}
	_, err := Expand(context.Background(), t.TempDir(), ".", decl)
	if err == nil {
		t.Fatal("expected error from failing provider")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should include provider stderr, got: %v", err)
	}
}
