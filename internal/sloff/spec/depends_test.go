package spec_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/spec"
)

const dependsSpecYAML = `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]

commands:
  - name: producer
    cmd: ["sh", "-c", "true"]
    inputs: ["in.txt"]
    outputs: ["mid.txt"]
    tools: [versioner]
  - name: consumer
    cmd: ["sh", "-c", "true"]
    inputs: ["mid.txt"]
    outputs: ["out.txt"]
    tools: [versioner]
    depends:
      - task: producer
      - spec: ../other
        task: gen
`

func TestParse_DependsEntries(t *testing.T) {
	f, err := spec.Parse([]byte(dependsSpecYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := f.Commands[1].Depends
	want := []spec.Depend{
		{Task: "producer"},
		{Spec: "../other", Task: "gen"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("depends mismatch (-want +got):\n%s", diff)
	}
}

func TestParse_DependsOmittedIsNil(t *testing.T) {
	f, err := spec.Parse([]byte(dependsSpecYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Commands[0].Depends != nil {
		t.Errorf("expected nil depends, got %v", f.Commands[0].Depends)
	}
}

func TestParse_DependsTaskRequired(t *testing.T) {
	yml := `commands:
  - name: consumer
    cmd: ["sh", "-c", "true"]
    inputs: ["mid.txt"]
    outputs: ["out.txt"]
    tools: [versioner]
    depends:
      - spec: ../other
`
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "task is required") {
		t.Errorf("expected 'task is required' error, got %v", err)
	}
}

func TestParse_DependsSpecMustBeRelative(t *testing.T) {
	yml := `commands:
  - name: consumer
    cmd: ["sh", "-c", "true"]
    inputs: ["mid.txt"]
    outputs: ["out.txt"]
    tools: [versioner]
    depends:
      - spec: /abs/path
        task: gen
`
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "must be a relative path") {
		t.Errorf("expected relative-path error, got %v", err)
	}
}
