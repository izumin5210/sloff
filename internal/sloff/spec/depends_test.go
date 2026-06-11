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

// buildSpecs assembles a discovered-spec set without touching the
// filesystem: dirs are OS-native (as spec.Discover produces them).
func buildSpecs(t *testing.T, byDir map[string]string) []spec.Spec {
	t.Helper()
	out := make([]spec.Spec, 0, len(byDir))
	for dir, yml := range byDir {
		f, err := spec.Parse([]byte(yml))
		if err != nil {
			t.Fatalf("Parse %s: %v", dir, err)
		}
		out = append(out, spec.Spec{Dir: dir, Path: dir + "/sloff.yml", File: f})
	}
	return out
}

const producerYAML = `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
commands:
  - name: gen
    cmd: ["sh", "-c", "true"]
    inputs: ["in.txt"]
    outputs: ["out.txt"]
    tools: [versioner]
`

func consumerYAML(dependsBlock string) string {
	return `commands:
  - name: consume
    cmd: ["sh", "-c", "true"]
    inputs: ["x.txt"]
    outputs: ["y.txt"]
    tools: [versioner]
` + dependsBlock
}

func TestValidateDependReferences_OK(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/options": producerYAML,
		"proto/svc": consumerYAML(`    depends:
      - spec: ../options
        task: gen
`),
	})
	if err := spec.ValidateDependReferences(specs); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

func TestValidateDependReferences_UnknownSpecDirErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/svc": consumerYAML(`    depends:
      - spec: ../nowhere
        task: gen
`),
	})
	err := spec.ValidateDependReferences(specs)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestValidateDependReferences_UnknownTaskErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/options": producerYAML,
		"proto/svc": consumerYAML(`    depends:
      - spec: ../options
        task: missing-task
`),
	})
	err := spec.ValidateDependReferences(specs)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error, got %v", err)
	}
}

func TestValidateDependReferences_SelfReferenceErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/svc": consumerYAML(`    depends:
      - task: consume
`),
	})
	err := spec.ValidateDependReferences(specs)
	if err == nil || !strings.Contains(err.Error(), "itself") {
		t.Errorf("expected self-reference error, got %v", err)
	}
}

func TestValidateDependReferences_DuplicateEdgeErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/options": producerYAML,
		"proto/svc": consumerYAML(`    depends:
      - spec: ../options
        task: gen
      - spec: ../../proto/options
        task: gen
`),
	})
	err := spec.ValidateDependReferences(specs)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got %v", err)
	}
}

func TestValidateDependReferences_RepoRootEscapeErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/svc": consumerYAML(`    depends:
      - spec: ../../../outside
        task: gen
`),
	})
	err := spec.ValidateDependReferences(specs)
	if err == nil || !strings.Contains(err.Error(), "escapes repo root") {
		t.Errorf("expected escape error, got %v", err)
	}
}
