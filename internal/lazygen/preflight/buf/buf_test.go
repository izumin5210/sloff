package buf_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/izumin5210/lazygen/internal/lazygen/preflight"
	"github.com/izumin5210/lazygen/internal/lazygen/preflight/buf"
	"github.com/izumin5210/lazygen/internal/lazygen/spec"
)

func TestChecker_Name(t *testing.T) {
	if c := buf.New(t.TempDir(), nil); c.Name() != "buf" {
		t.Errorf("Name() = %q, want buf", c.Name())
	}
}

func TestChecker_NoSubjectsIsOK(t *testing.T) {
	c := buf.New(t.TempDir(), nil)
	res, err := c.Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.OK || len(res.Issues) > 0 {
		t.Errorf("expected OK with no issues, got %+v", res)
	}
}

// TestChecker_PinnedRemotePassesLint guards the happy path: a buf.gen.yaml
// with a properly pinned remote plugin and no buf.yaml deps must surface no
// issues.
func TestChecker_PinnedRemotePassesLint(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:v1.35.2
    out: gen
`,
	})
	specs := []spec.Spec{makeSpec("proto", "gen", "buf.gen.yaml")}

	res, err := buf.New(root, specs).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.OK || len(res.Issues) > 0 {
		t.Errorf("expected OK, got %+v", res)
	}
}

func TestChecker_FlagsUnpinnedRemote(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:latest
    out: gen
`,
	})
	specs := []spec.Spec{makeSpec("proto", "gen", "buf.gen.yaml")}

	res, err := buf.New(root, specs).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.OK {
		t.Fatal("expected issues, got OK")
	}
	if len(res.Issues) != 1 {
		t.Fatalf("len(Issues) = %d, want 1: %+v", len(res.Issues), res.Issues)
	}
	if !strings.Contains(res.Issues[0].Detail, "pinned") {
		t.Errorf("issue detail should mention pinning, got %q", res.Issues[0].Detail)
	}
}

// TestChecker_DedupSubjects guards the aggregation: when several tasks share
// the same buf.gen.yaml the same issue must not be reported once per task.
func TestChecker_DedupSubjects(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:latest
    out: gen
`,
	})
	specs := []spec.Spec{
		makeSpec("proto", "gen-go", "buf.gen.yaml"),
		makeSpec("proto", "gen-grpc", "buf.gen.yaml"),
	}

	res, err := buf.New(root, specs).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(res.Issues) != 1 {
		t.Errorf("len(Issues) = %d, want 1 (dedup)", len(res.Issues))
	}
}

// TestChecker_ReportsBufYAMLLockDrift guards the buf.yaml ↔ buf.lock check:
// adding a dep to buf.yaml without re-running `buf dep update` must surface
// as a preflight issue.
func TestChecker_ReportsBufYAMLLockDrift(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.yaml": `version: v2
modules:
  - path: .
deps:
  - buf.build/googleapis/googleapis
  - buf.build/grpc-ecosystem/grpc-gateway
`,
		"proto/buf.lock": `version: v2
deps:
  - name: buf.build/googleapis/googleapis
    commit: abc
    digest: shake256:000
`,
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
`,
	})
	specs := []spec.Spec{makeSpec("proto", "gen", "buf.gen.yaml")}

	res, err := buf.New(root, specs).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.OK {
		t.Fatal("expected drift issue, got OK")
	}
	gotDetails := joinDetails(res.Issues)
	if !strings.Contains(gotDetails, "grpc-gateway") {
		t.Errorf("expected drift issue to mention grpc-gateway, got %q", gotDetails)
	}
}

// TestChecker_ReportsMissingBufLock guards the case where buf.yaml declares
// deps but buf.lock is absent entirely (e.g. forgot to run `buf dep update`
// after adding the file).
func TestChecker_ReportsMissingBufLock(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.yaml": `version: v2
deps:
  - buf.build/googleapis/googleapis
`,
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
`,
	})
	specs := []spec.Spec{makeSpec("proto", "gen", "buf.gen.yaml")}

	res, err := buf.New(root, specs).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.OK {
		t.Fatal("expected missing-lock issue, got OK")
	}
	gotDetails := joinDetails(res.Issues)
	if !strings.Contains(gotDetails, "buf.lock") {
		t.Errorf("expected missing-lock issue to mention buf.lock, got %q", gotDetails)
	}
}

// TestChecker_NoBufYAMLIsAllowed guards a buf module-less setup: a repo can
// use buf for codegen without committing buf.yaml/buf.lock, and that must not
// raise issues.
func TestChecker_NoBufYAMLIsAllowed(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
`,
	})
	specs := []spec.Spec{makeSpec("proto", "gen", "buf.gen.yaml")}

	res, err := buf.New(root, specs).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.OK || len(res.Issues) > 0 {
		t.Errorf("expected OK, got %+v", res)
	}
}

// TestChecker_AncestorBufYAML guards that buf.yaml can live above the spec
// dir (e.g. repo-wide module root with per-language buf.gen.yaml). The
// checker must walk up to find it.
func TestChecker_AncestorBufYAML(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"buf.yaml": `version: v2
deps:
  - buf.build/googleapis/googleapis
`,
		"buf.lock": `version: v2
deps:
  - name: buf.build/googleapis/googleapis
    commit: abc
    digest: shake256:000
`,
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
`,
	})
	specs := []spec.Spec{makeSpec("proto", "gen", "buf.gen.yaml")}

	res, err := buf.New(root, specs).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.OK || len(res.Issues) > 0 {
		t.Errorf("expected OK with ancestor buf.yaml/buf.lock, got %+v", res)
	}
}

// TestChecker_DepsWithVersionTagsCompare guards that a buf.yaml dep written
// with an explicit `:vX.Y.Z` tag still matches the bare-name lock entry.
// Without normalising, every tagged dep would falsely report as missing.
func TestChecker_DepsWithVersionTagsCompare(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.yaml": `version: v2
deps:
  - buf.build/googleapis/googleapis:v1.0.0
`,
		"proto/buf.lock": `version: v2
deps:
  - name: buf.build/googleapis/googleapis
    commit: abc
    digest: shake256:000
`,
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
`,
	})
	specs := []spec.Spec{makeSpec("proto", "gen", "buf.gen.yaml")}

	res, err := buf.New(root, specs).Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.OK {
		t.Errorf("expected OK with tag-stripped dep match, got %+v", res)
	}
}

// makeSpec assembles a spec.Spec with a single command that declares a buf
// tool. Inputs/outputs/cmd carry placeholder values because the preflight
// checker only inspects tools[].
func makeSpec(specDir, taskName, bufGenPath string) spec.Spec {
	return spec.Spec{
		Dir:  specDir,
		Path: filepath.Join(specDir, "lazygen.yml"),
		File: &spec.File{
			Commands: []spec.Command{{
				Name:    taskName,
				Cmd:     []string{"buf", "generate"},
				Inputs:  []string{"**/*.proto"},
				Outputs: []string{"**/*.pb.go"},
				Tools: []spec.DeclaredTool{
					{Resolver: "script", Exec: []string{"buf", "--version"}},
					{Resolver: "buf", BufGenPath: bufGenPath},
				},
			}},
		},
	}
}

func setupRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, contents := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func joinDetails(issues []preflight.Issue) string {
	parts := make([]string, len(issues))
	for i, iss := range issues {
		parts[i] = iss.Detail
	}
	return strings.Join(parts, "\n")
}
