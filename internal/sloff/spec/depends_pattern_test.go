package spec_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/spec"
)

// genSpecYAML builds a producer spec exposing the given task names so a
// consumer's depends pattern has something to match.
func genSpecYAML(names ...string) string {
	var b strings.Builder
	b.WriteString("tools:\n  versioner:\n    exec: [\"sh\", \"-c\", \"echo v1.0.0\"]\ncommands:\n")
	for _, n := range names {
		b.WriteString("  - name: " + n + "\n")
		b.WriteString("    cmd: [\"sh\", \"-c\", \"true\"]\n")
		b.WriteString("    inputs: [\"" + n + ".in\"]\n")
		b.WriteString("    outputs: [\"" + n + ".out\"]\n")
		b.WriteString("    tools: [versioner]\n")
	}
	return b.String()
}

// dependsOf returns the (expanded) depends of one command after running
// ExpandDependPatterns through the spec set.
func dependsOf(t *testing.T, specs []spec.Spec, dir, name string) []spec.Depend {
	t.Helper()
	for _, sp := range specs {
		if sp.Dir != dir {
			continue
		}
		for _, c := range sp.File.Commands {
			if c.Name == name {
				return c.Depends
			}
		}
	}
	t.Fatalf("command %s/%s not found", dir, name)
	return nil
}

func TestIsDependPattern(t *testing.T) {
	cases := map[string]bool{
		"gen":      false,
		"gen-a":    false,
		"buf_go":   false,
		"gen-*":    true,
		"gen-?":    true,
		"gen-[ab]": true,
		"*":        true,
	}
	for in, want := range cases {
		if got := spec.IsDependPattern(in); got != want {
			t.Errorf("IsDependPattern(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestExpandDependPatterns_MatchesAcrossSpec(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/gen": genSpecYAML("gen-a", "gen-b", "other"),
		"proto/svc": consumerYAML(`    depends:
      - spec: ../gen
        task: "gen-*"
`),
	})
	out, prov, err := spec.ExpandDependPatterns(specs)
	if err != nil {
		t.Fatalf("ExpandDependPatterns: %v", err)
	}
	want := []spec.Depend{
		{Spec: "../gen", Task: "gen-a"},
		{Spec: "../gen", Task: "gen-b"},
	}
	if diff := cmp.Diff(want, dependsOf(t, out, "proto/svc", "consume")); diff != "" {
		t.Errorf("expanded depends mismatch (-want +got):\n%s", diff)
	}
	// "other" must not be pulled in.
	for _, d := range dependsOf(t, out, "proto/svc", "consume") {
		if d.Task == "other" {
			t.Errorf("pattern wrongly matched 'other'")
		}
	}
	// Provenance records the one pattern and its full match set.
	if len(prov) != 1 || prov[0].Pattern != "gen-*" || len(prov[0].Edges) != 2 {
		t.Errorf("unexpected provenance: %+v", prov)
	}
	// The expanded set must pass the literal-only reference check.
	if err := spec.ValidateDependReferences(out); err != nil {
		t.Errorf("expanded set failed ValidateDependReferences: %v", err)
	}
}

func TestExpandDependPatterns_SameSpecExcludesSelf(t *testing.T) {
	// A pattern declared in the same spec that also matches the declaring
	// command must not create a self-edge (ADR-0016 D3).
	yml := `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
commands:
  - name: gen-a
    cmd: ["sh", "-c", "true"]
    inputs: ["a.in"]
    outputs: ["a.out"]
    tools: [versioner]
  - name: gen-b
    cmd: ["sh", "-c", "true"]
    inputs: ["b.in"]
    outputs: ["b.out"]
    tools: [versioner]
    depends:
      - task: "gen-*"
`
	specs := buildSpecs(t, map[string]string{"proto/gen": yml})
	out, _, err := spec.ExpandDependPatterns(specs)
	if err != nil {
		t.Fatalf("ExpandDependPatterns: %v", err)
	}
	want := []spec.Depend{{Task: "gen-a"}} // gen-b excluded (self)
	if diff := cmp.Diff(want, dependsOf(t, out, "proto/gen", "gen-b")); diff != "" {
		t.Errorf("self-exclude mismatch (-want +got):\n%s", diff)
	}
	if err := spec.ValidateDependReferences(out); err != nil {
		t.Errorf("expanded set failed ValidateDependReferences: %v", err)
	}
}

func TestExpandDependPatterns_ZeroMatchErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/gen": genSpecYAML("gen-a"),
		"proto/svc": consumerYAML(`    depends:
      - spec: ../gen
        task: "nomatch-*"
`),
	})
	_, _, err := spec.ExpandDependPatterns(specs)
	if err == nil || !strings.Contains(err.Error(), "matched no task") {
		t.Errorf("expected zero-match error, got %v", err)
	}
}

func TestExpandDependPatterns_DedupesLiteralAndPatterns(t *testing.T) {
	// gen-a is named both by a literal and by the pattern, and by two patterns;
	// the result must contain it once (ADR-0016 D3 union).
	specs := buildSpecs(t, map[string]string{
		"proto/gen": genSpecYAML("gen-a", "gen-b"),
		"proto/svc": consumerYAML(`    depends:
      - spec: ../gen
        task: gen-a
      - spec: ../gen
        task: "gen-*"
      - spec: ../gen
        task: "gen-a*"
`),
	})
	out, _, err := spec.ExpandDependPatterns(specs)
	if err != nil {
		t.Fatalf("ExpandDependPatterns: %v", err)
	}
	want := []spec.Depend{
		{Spec: "../gen", Task: "gen-a"}, // literal, kept first
		{Spec: "../gen", Task: "gen-b"}, // from gen-* (gen-a deduped against literal)
	}
	if diff := cmp.Diff(want, dependsOf(t, out, "proto/svc", "consume")); diff != "" {
		t.Errorf("dedupe mismatch (-want +got):\n%s", diff)
	}
	if err := spec.ValidateDependReferences(out); err != nil {
		t.Errorf("expanded set failed ValidateDependReferences: %v", err)
	}
}

func TestExpandDependPatterns_LiteralOnlyUnchanged(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/gen": producerYAML,
		"proto/svc": consumerYAML(`    depends:
      - spec: ../gen
        task: gen
`),
	})
	out, prov, err := spec.ExpandDependPatterns(specs)
	if err != nil {
		t.Fatalf("ExpandDependPatterns: %v", err)
	}
	if prov != nil {
		t.Errorf("expected no provenance for literal-only specs, got %v", prov)
	}
	// Specs without patterns are returned with their original *File pointers.
	for i := range specs {
		if specs[i].File != out[i].File {
			t.Errorf("literal-only spec %s File pointer was copied unnecessarily", specs[i].Dir)
		}
	}
}

func TestExpandDependPatterns_EscapesRootErrors(t *testing.T) {
	specs := buildSpecs(t, map[string]string{
		"proto/svc": consumerYAML(`    depends:
      - spec: ../../../outside
        task: "gen-*"
`),
	})
	_, _, err := spec.ExpandDependPatterns(specs)
	if err == nil || !strings.Contains(err.Error(), "escapes repo root") {
		t.Errorf("expected escape error, got %v", err)
	}
}

func TestExpandDependPatterns_DeterministicOrder(t *testing.T) {
	// Same logical set, different command declaration order in the producer:
	// the expanded edges must come out identically (sorted), independent of
	// discovery order (ADR-0016 D3 / R2).
	a := buildSpecs(t, map[string]string{
		"proto/gen": genSpecYAML("gen-c", "gen-a", "gen-b"),
		"proto/svc": consumerYAML("    depends:\n      - {spec: ../gen, task: \"gen-*\"}\n"),
	})
	out, _, err := spec.ExpandDependPatterns(a)
	if err != nil {
		t.Fatalf("ExpandDependPatterns: %v", err)
	}
	want := []spec.Depend{
		{Spec: "../gen", Task: "gen-a"},
		{Spec: "../gen", Task: "gen-b"},
		{Spec: "../gen", Task: "gen-c"},
	}
	if diff := cmp.Diff(want, dependsOf(t, out, "proto/svc", "consume")); diff != "" {
		t.Errorf("non-deterministic expansion (-want +got):\n%s", diff)
	}
}

func TestParse_DependsRejectsInvalidGlob(t *testing.T) {
	yml := `commands:
  - name: consume
    cmd: ["sh", "-c", "true"]
    inputs: ["x.txt"]
    outputs: ["y.txt"]
    tools: [versioner]
    depends:
      - task: "gen-[a"
`
	_, err := spec.Parse([]byte(yml))
	if err == nil || !strings.Contains(err.Error(), "invalid glob pattern") {
		t.Errorf("expected invalid-glob error, got %v", err)
	}
}
