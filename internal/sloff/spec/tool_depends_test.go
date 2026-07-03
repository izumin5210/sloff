package spec_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/spec"
)

// Tool bootstrap depends (ADR-0019 D1): parse + load-time validation.

func TestParse_ToolDependsEntries(t *testing.T) {
	yml := `tools:
  gen-foo:
    go-local: ./cmd/gen-foo
    depends:
      - task: gen-proto
      - spec: ../gen
        task: gen-bar
`
	f, err := spec.Parse([]byte(yml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := f.Tools["gen-foo"].Depends
	want := []spec.Depend{
		{Task: "gen-proto"},
		{Spec: "../gen", Task: "gen-bar"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("tool depends mismatch (-want +got):\n%s", diff)
	}
}

// TestParse_ToolDependsOnEveryResolverForm pins that depends is orthogonal to
// the resolver shape: script / go-local / pnpm-local tools can all declare it
// (the deferred-resolution gate is the declaration, not the channel).
func TestParse_ToolDependsOnEveryResolverForm(t *testing.T) {
	yml := `tools:
  scripted:
    exec: ["sh", "-c", "echo v1.0.0"]
    depends:
      - task: build-bin
  golocal:
    go-local: ./cmd/golocal
    depends:
      - task: gen-src
  pnpmlocal:
    pnpm-local: "@org/codegen"
    depends:
      - task: gen-lib
`
	f, err := spec.Parse([]byte(yml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for name, wantTask := range map[string]string{
		"scripted":  "build-bin",
		"golocal":   "gen-src",
		"pnpmlocal": "gen-lib",
	} {
		deps := f.Tools[name].Depends
		if len(deps) != 1 || deps[0].Task != wantTask {
			t.Errorf("tools[%q].Depends = %v, want single entry on task %q", name, deps, wantTask)
		}
	}
}

func TestParse_ToolDependsOmittedIsNil(t *testing.T) {
	yml := `tools:
  gen-foo:
    go-local: ./cmd/gen-foo
`
	f, err := spec.Parse([]byte(yml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Tools["gen-foo"].Depends != nil {
		t.Errorf("expected nil depends, got %v", f.Tools["gen-foo"].Depends)
	}
}

func TestParse_ToolDependsTaskRequired(t *testing.T) {
	yml := `tools:
  gen-foo:
    go-local: ./cmd/gen-foo
    depends:
      - spec: ../gen
`
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "task is required") {
		t.Errorf("expected 'task is required' error, got %v", err)
	}
}

func TestParse_ToolDependsSpecMustBeRelative(t *testing.T) {
	yml := `tools:
  gen-foo:
    go-local: ./cmd/gen-foo
    depends:
      - spec: /abs/path
        task: gen
`
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "must be a relative path") {
		t.Errorf("expected relative-path error, got %v", err)
	}
}

// TestParse_ToolDependsRejectsGlobPattern locks the v1 scope decision
// (ADR-0019 D1): unlike task depends (ADR-0016), a tool's bootstrap depends
// must name concrete tasks — a glob is a load-time error, not a pattern.
func TestParse_ToolDependsRejectsGlobPattern(t *testing.T) {
	yml := `tools:
  gen-foo:
    go-local: ./cmd/gen-foo
    depends:
      - task: "gen-*"
`
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "glob patterns are not supported") {
		t.Errorf("expected glob-unsupported error, got %v", err)
	}
}

// TestParse_ToolDependsRejectsGlobPatternInSpec pins the v1 scope decision
// for the spec field too: ADR-0019 D1 rejects glob metacharacters in both
// the task and the spec fields of a tool's bootstrap depends.
func TestParse_ToolDependsRejectsGlobPatternInSpec(t *testing.T) {
	yml := `tools:
  gen-foo:
    go-local: ./cmd/gen-foo
    depends:
      - spec: "services/*"
        task: gen
`
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "glob patterns are not supported") {
		t.Errorf("expected glob-unsupported error, got %v", err)
	}
}

func TestParse_ToolDependsRejectsDuplicateEntry(t *testing.T) {
	yml := `tools:
  gen-foo:
    go-local: ./cmd/gen-foo
    depends:
      - task: gen-proto
      - task: gen-proto
`
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "duplicate depends entry") {
		t.Errorf("expected duplicate-entry error, got %v", err)
	}
}

// TestParse_ToolDependsRejectsDuplicateAcrossSpecSpellings pins that "" and
// "." spell the same spec dir, so an entry duplicated across the two forms is
// still rejected.
func TestParse_ToolDependsRejectsDuplicateAcrossSpecSpellings(t *testing.T) {
	yml := `tools:
  gen-foo:
    go-local: ./cmd/gen-foo
    depends:
      - task: gen-proto
      - spec: "."
        task: gen-proto
`
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "duplicate depends entry") {
		t.Errorf("expected duplicate-entry error, got %v", err)
	}
}

// TestParse_ToolDependsExistenceNotCheckedAtLoad pins the D1 split: the
// referenced task does not need to exist at parse time (it may be
// provider-generated, or the tool may never be referenced). Existence is an
// injection-time concern.
func TestParse_ToolDependsExistenceNotCheckedAtLoad(t *testing.T) {
	yml := `tools:
  gen-foo:
    go-local: ./cmd/gen-foo
    depends:
      - task: does-not-exist-anywhere
`
	if _, err := spec.Parse([]byte(yml)); err != nil {
		t.Errorf("Parse should not validate depends targets, got %v", err)
	}
}
