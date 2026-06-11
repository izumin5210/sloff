package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// updateGraphGolden rewrites testdata/e2e/graph/<case>/expected.txt from the
// captured stdout of the graph cmd. Used after intentional rendering changes:
//
//	go test ./cmd/sloff/... -update-graph
var updateGraphGolden = flag.Bool("update-graph", false, "rewrite testdata/e2e/graph/<case>/expected.txt from actual outputs")

type graphHarness struct {
	caseDir  string
	workdir  string
	expected string // path to expected.txt
}

func setupGraphHarness(t *testing.T, name string) *graphHarness {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	caseDir := filepath.Join(repoRoot, "testdata", "e2e", "graph", name)
	initial := filepath.Join(caseDir, "initial")
	if _, err := os.Stat(initial); err != nil {
		t.Fatalf("initial fixture missing: %s", initial)
	}
	workdir := t.TempDir()
	if err := os.CopyFS(workdir, os.DirFS(initial)); err != nil {
		t.Fatalf("copy initial: %v", err)
	}
	// pnpm-local enumerates source files via `git ls-files`; non-pnpm cases
	// don't care, so we git-init every harness unconditionally rather than
	// branch on fixture content (matches runner harness behaviour).
	gitInitGraphWorkdir(t, workdir)
	return &graphHarness{
		caseDir:  caseDir,
		workdir:  workdir,
		expected: filepath.Join(caseDir, "expected.txt"),
	}
}

func gitInitGraphWorkdir(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available, skipping graph E2E: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "sloff-test@example.com"},
		{"config", "user.name", "sloff-test"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func runGraphCmd(t *testing.T, h *graphHarness, extra ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	args := append([]string{"graph", "--root", h.workdir}, extra...)
	root.SetArgs(args)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("graph cmd: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.String()
}

func assertGraphGolden(t *testing.T, h *graphHarness, got string) {
	t.Helper()
	if *updateGraphGolden {
		if err := os.MkdirAll(filepath.Dir(h.expected), 0o755); err != nil {
			t.Fatalf("ensure golden dir: %v", err)
		}
		if err := os.WriteFile(h.expected, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated golden: %s", h.expected)
		return
	}
	want, err := os.ReadFile(h.expected)
	if err != nil {
		t.Fatalf("read golden: %v (run `go test -update-graph` to populate)", err)
	}
	if got != string(want) {
		t.Errorf("graph output mismatch\nwant:\n%s\ngot:\n%s\nrerun with -update-graph to refresh", want, got)
	}
}

func TestGraph_SimpleChain_Mermaid(t *testing.T) {
	h := setupGraphHarness(t, "simple-chain-mermaid")
	got := runGraphCmd(t, h, "--format", "mermaid")
	assertGraphGolden(t, h, got)
}

// TestGraph_SimpleChain_DOT covers the same producer/consumer fixture as the
// mermaid case but rendered through --format dot, locking the contract that
// swapping formats does not reorder nodes/edges.
func TestGraph_SimpleChain_DOT(t *testing.T) {
	h := setupGraphHarness(t, "simple-chain-dot")
	got := runGraphCmd(t, h, "--format", "dot")
	assertGraphGolden(t, h, got)
}

// TestGraph_MultiDeps_Mermaid exercises the "+N more" edge caption: when the
// producer's outputs intersect the consumer's inputs in three files, the
// rendered edge label samples the first one and annotates the rest, per
// IZU-7's "サンプル併記" wording.
func TestGraph_MultiDeps_Mermaid(t *testing.T) {
	h := setupGraphHarness(t, "multi-deps-mermaid")
	got := runGraphCmd(t, h, "--format", "mermaid")
	assertGraphGolden(t, h, got)
}

// TestGraph_PnpmLocal_BuildChain_Mermaid validates that resolver-contributed
// ExtraInputs are visible to the graph subcommand: the gen task pulls
// @org/codegen via pnpm-local and declares depends on build-codegen; the
// rendered edge carries the dist files as overlap evidence because the
// resolver folded them into gen's inputs. The fixture deliberately omits
// node_modules/.pnpm/lock.yaml to cover the "graph remains usable when
// install state is drifted" claim from runner.Plan's docstring.
func TestGraph_PnpmLocal_BuildChain_Mermaid(t *testing.T) {
	h := setupGraphHarness(t, "pnpmlocal-build-chain-mermaid")
	got := runGraphCmd(t, h, "--format", "mermaid")
	assertGraphGolden(t, h, got)
}
