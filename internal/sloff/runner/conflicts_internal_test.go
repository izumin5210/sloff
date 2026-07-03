package runner

import (
	"strings"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
	"github.com/izumin5210/sloff/internal/sloff/spec"
)

// makeTaskInfo builds a taskInfo with only the fields detectOutputPatternConflicts reads
// (specRelpath / outputPatterns). The other fields stay zero so the test stays focused on
// the conflict-detection contract.
func makeTaskInfo(specRelpath, name string, outputs []string) (depgraph.Task, *taskInfo) {
	t := depgraph.Task{SpecRelpath: specRelpath, Name: name, Outputs: outputs}
	info := &taskInfo{
		specRelpath:    specRelpath,
		command:        spec.Command{Name: name, Outputs: outputs},
		outputPatterns: outputs,
	}
	return t, info
}

// TestDetectOutputPatternConflicts_DistinctSpecDirsSameRelpath is the IZU-18 regression
// guard. Two service-local specs declaring the same relative output pattern resolve to
// distinct absolute paths once joined with their spec dir, so the planner must NOT flag
// them as duplicates. Before the fix this returned a "duplicate output pattern producers"
// error and prevented the run from starting at all.
func TestDetectOutputPatternConflicts_DistinctSpecDirsSameRelpath(t *testing.T) {
	tasks := make([]depgraph.Task, 0, 2)
	byKey := map[depgraph.TaskRef]*taskInfo{}
	for _, dir := range []string{"services/a", "services/b"} {
		task, info := makeTaskInfo(dir, "gen-db", []string{"internal/db/db.gen.go"})
		tasks = append(tasks, task)
		byKey[task.Ref()] = info
	}

	if err := detectOutputPatternConflicts(tasks, byKey); err != nil {
		t.Fatalf("expected no conflict for same relative pattern under distinct spec dirs, got: %v", err)
	}
}

// TestDetectOutputPatternConflicts_SameSpecDirSamePattern keeps the original guarantee:
// two tasks inside the same sloff.yml that declare the same output pattern must still be
// flagged at planning time, before either cmd runs. The error must name both task labels
// so the user can fix the spec.
func TestDetectOutputPatternConflicts_SameSpecDirSamePattern(t *testing.T) {
	t1, i1 := makeTaskInfo("spec", "first", []string{"shared.txt"})
	t2, i2 := makeTaskInfo("spec", "second", []string{"shared.txt"})
	tasks := []depgraph.Task{t1, t2}
	byKey := map[depgraph.TaskRef]*taskInfo{
		t1.Ref(): i1,
		t2.Ref(): i2,
	}

	err := detectOutputPatternConflicts(tasks, byKey)
	if err == nil {
		t.Fatal("expected duplicate-pattern error when two tasks in the same spec write the same output")
	}
	for _, want := range []string{"spec:first", "spec:second", "shared.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err.Error(), want)
		}
	}
}

// TestDetectOutputPatternConflicts_DotDotResolvesAcrossSpecs covers patterns that use
// `..` to escape the spec dir (ADR-0008 / cross-dir codegen, IZU-17). After joining with
// each spec's dir the absolute paths must match for the conflict to fire — two specs
// pointing the same `../../gen/foo.go` therefore must collide, while two specs whose
// `..` paths resolve to different absolute locations must not.
func TestDetectOutputPatternConflicts_DotDotResolvesAcrossSpecs(t *testing.T) {
	t.Run("collide when ..-resolved paths match", func(t *testing.T) {
		t1, i1 := makeTaskInfo("services/a/spec", "gen", []string{"../../shared/out.go"})
		t2, i2 := makeTaskInfo("services/b/spec", "gen", []string{"../../shared/out.go"})
		tasks := []depgraph.Task{t1, t2}
		byKey := map[depgraph.TaskRef]*taskInfo{
			t1.Ref(): i1,
			t2.Ref(): i2,
		}

		err := detectOutputPatternConflicts(tasks, byKey)
		if err == nil {
			t.Fatal("expected duplicate-pattern error: both ../../shared/out.go resolve to services/shared/out.go")
		}
		if !strings.Contains(err.Error(), "services/shared/out.go") {
			t.Errorf("error %q should report the resolved absolute path, not the raw pattern", err.Error())
		}
	})

	t.Run("do not collide when ..-resolved paths differ", func(t *testing.T) {
		t1, i1 := makeTaskInfo("services/a/spec", "gen", []string{"../out/a.go"})
		t2, i2 := makeTaskInfo("services/b/spec", "gen", []string{"../out/b.go"})
		tasks := []depgraph.Task{t1, t2}
		byKey := map[depgraph.TaskRef]*taskInfo{
			t1.Ref(): i1,
			t2.Ref(): i2,
		}

		if err := detectOutputPatternConflicts(tasks, byKey); err != nil {
			t.Fatalf("expected no conflict for distinct ..-resolved paths, got: %v", err)
		}
	})
}
