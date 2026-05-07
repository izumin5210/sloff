package runner_test

import (
	"context"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/cache/local"
	"github.com/izumin5210/lazygen/internal/lazygen/preflight"
	"github.com/izumin5210/lazygen/internal/lazygen/runner"
	"github.com/izumin5210/lazygen/internal/lazygen/spec"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/golocal"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/pnpmlocal"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/script"
)

// updateGolden rewrites testdata/e2e/runner/<case>/expected/ from the actual final state
// of the test instead of comparing against it. Used to refresh fixtures after intentional
// behaviour changes:
//
//	go test ./internal/lazygen/runner/... -update
var updateGolden = flag.Bool("update", false, "rewrite expected/ fixtures from actual outputs")

// fixedClock is the timestamp injected into runner.Options.Clock for every E2E test so
// that record.GeneratedAt is deterministic and the YAML files can be committed as
// goldens.
var fixedClock = time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

// step is one operation applied to the test working directory.
type step func(t *testing.T, h *harness)

type harness struct {
	t           *testing.T
	caseDir     string // testdata/e2e/runner/<name>
	workdir     string // freshly-copied tempdir initialised from <caseDir>/initial
	expectedDir string // <caseDir>/expected
}

func runE2E(t *testing.T, name string, steps ...step) {
	t.Helper()
	h := setupHarness(t, name)
	for _, s := range steps {
		s(t, h)
	}
	if *updateGolden {
		h.snapshotExpected(t)
		return
	}
	h.assertExpected(t)
}

func setupHarness(t *testing.T, name string) *harness {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	caseDir := filepath.Join(repoRoot, "testdata", "e2e", "runner", name)
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
	// branch on fixture content. The init is also configured with a
	// deterministic identity so any incidental `git add` (none today) would
	// not need ambient user config.
	gitInitWorkdir(t, workdir)
	return &harness{
		t:           t,
		caseDir:     caseDir,
		workdir:     workdir,
		expectedDir: filepath.Join(caseDir, "expected"),
	}
}

func gitInitWorkdir(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available, skipping git-backed E2E: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "lazygen-test@example.com"},
		{"config", "user.name", "lazygen-test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// runStep runs the runner once against the current workdir state, with the fixed clock so
// generated_at is deterministic.
func runStep() step {
	return func(t *testing.T, h *harness) {
		t.Helper()
		specs, err := spec.Discover(h.workdir, "**/lazygen.yml")
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		resolverReg := toolresolver.NewRegistry()
		resolverReg.Register(script.New(h.workdir))
		resolverReg.Register(golocal.New(h.workdir, lister.NewMemoized(lister.NewGoPackages(h.workdir))))
		pnpmRes, err := pnpmlocal.New(h.workdir, pnpmlocal.GitLsFiles)
		if err != nil {
			t.Fatalf("pnpmlocal.New: %v", err)
		}
		resolverReg.Register(pnpmRes)
		r := runner.New(runner.Options{
			RepoRoot:  h.workdir,
			Specs:     specs,
			Storage:   local.New(h.workdir),
			Resolvers: resolverReg,
			Preflight: preflight.NewRegistry(),
			Clock:     func() time.Time { return fixedClock },
		})
		if err := r.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
}

func writeStep(relpath, contents string) step {
	return func(t *testing.T, h *harness) {
		t.Helper()
		full := filepath.Join(h.workdir, relpath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *harness) snapshotExpected(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(h.expectedDir); err != nil {
		t.Fatalf("clear expected dir: %v", err)
	}
	if err := mirrorTree(h.workdir, h.expectedDir); err != nil {
		t.Fatalf("snapshot expected: %v", err)
	}
	t.Logf("updated golden: %s", h.expectedDir)
}

func (h *harness) assertExpected(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(h.expectedDir); err != nil {
		t.Fatalf("expected/ missing: %s (run `go test -update` to populate)", h.expectedDir)
	}
	want, err := readTree(h.expectedDir)
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}
	got, err := readTree(h.workdir)
	if err != nil {
		t.Fatalf("read workdir: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("filesystem mismatch (-expected +got):\n%s\nrerun with `go test -update` to refresh", diff)
	}
}

// readTree returns a map[forward-slash relpath]string-content for every regular file under
// root. Symlinks are not followed; the .git directory is skipped because the harness git-
// inits the workdir for git ls-files but the goldens shouldn't capture that bookkeeping.
func readTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// mirrorTree copies all regular files (not symlinks) from src into dst, creating dst and
// any necessary subdirectories. The .git directory is skipped (see readTree).
func mirrorTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func TestRunner_FirstRunWritesRecord(t *testing.T) {
	runE2E(t, "first-run-writes-record", runStep())
}

func TestRunner_SecondRunHits(t *testing.T) {
	runE2E(t, "second-run-hits", runStep(), runStep())
}

func TestRunner_InputChangeInvalidates(t *testing.T) {
	runE2E(
		t, "input-change-invalidates",
		runStep(),
		writeStep("spec/input.txt", "world"),
		runStep(),
	)
}

func TestRunner_ToolVersionBumpInvalidates(t *testing.T) {
	runE2E(
		t, "tool-version-bump-invalidates",
		runStep(),
		writeStep("spec/lazygen.yml", `tools:
  versioner:
    exec: ["sh", "-c", "echo v2.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: copy
    cmd: ["sh", "-c", "cp input.txt output.txt; printf x >> ../marker.txt"]
    inputs: ["input.txt"]
    outputs: ["output.txt"]
    tools: [versioner]
`),
		runStep(),
	)
}

func TestRunner_OutputDriftInvalidates(t *testing.T) {
	runE2E(
		t, "output-drift-invalidates",
		runStep(),
		writeStep("spec/output.txt", "tampered"),
		runStep(),
	)
}

// goLocalGeneratorV2 is the post-edit body of cmd/copy/main.go used to flip the
// go-local resolver's source hash. The generator stays valid Go and produces a
// different output.txt so the test exercises both invalidate paths
// (tools_hash via source change AND output_hash via content change).
const goLocalGeneratorV2 = `package main

import (
	"io"
	"os"
)

func main() {
	in, err := os.Open("input.txt")
	if err != nil {
		panic(err)
	}
	defer in.Close()
	out, err := os.Create("output.txt")
	if err != nil {
		panic(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		panic(err)
	}
	if _, err := out.WriteString("v2\n"); err != nil {
		panic(err)
	}
}
`

func TestRunner_GoLocal_FirstRunWritesRecord(t *testing.T) {
	runE2E(t, "golocal-first-run-writes-record", runStep())
}

func TestRunner_GoLocal_SecondRunHits(t *testing.T) {
	runE2E(t, "golocal-second-run-hits", runStep(), runStep())
}

func TestRunner_GoLocal_SourceChangeInvalidates(t *testing.T) {
	runE2E(
		t, "golocal-source-change-invalidates",
		runStep(),
		writeStep("cmd/copy/main.go", goLocalGeneratorV2),
		runStep(),
	)
}

func TestRunner_GoLocal_InputChangeInvalidates(t *testing.T) {
	runE2E(
		t, "golocal-input-change-invalidates",
		runStep(),
		writeStep("input.txt", "world\n"),
		runStep(),
	)
}

// TestRunner_GoLocal_NestedSpecResolvesCorrectly guards the resolver's specDir
// rebasing: when lazygen.yml lives under spec/ and the cmd is `go run ./cmd/copy`,
// the resolver must hand "./spec/cmd/copy" (not "./cmd/copy") to the lister, or
// packages.Load fails to find the package. Without this fixture, regressions in
// the rebase logic would only surface on user repos with nested specs.
func TestRunner_GoLocal_NestedSpecResolvesCorrectly(t *testing.T) {
	runE2E(t, "golocal-nested-spec", runStep())
}

// pnpmLocalGeneratorV2 flips the source content the esbuild lister hashes.
// It is dropped into packages/codegen/dist/lib.js so the tools_hash changes
// even though input.txt and the cmd are unchanged.
const pnpmLocalGeneratorV2 = "export const helper = 'v2';\n"

func TestRunner_PnpmLocal_FirstRunWritesRecord(t *testing.T) {
	runE2E(t, "pnpmlocal-first-run-writes-record", runStep())
}

func TestRunner_PnpmLocal_SecondRunHits(t *testing.T) {
	runE2E(t, "pnpmlocal-second-run-hits", runStep(), runStep())
}

// TestRunner_PnpmLocal_SourceChangeInvalidates is the pnpm-local equivalent
// of the go-local source-change test: editing a transitive source file in
// the workspace package must flip tools_hash and trigger re-execution.
func TestRunner_PnpmLocal_SourceChangeInvalidates(t *testing.T) {
	runE2E(
		t, "pnpmlocal-source-change-invalidates",
		runStep(),
		writeStep("packages/codegen/dist/lib.js", pnpmLocalGeneratorV2),
		runStep(),
	)
}

func TestRunner_PnpmLocal_InputChangeInvalidates(t *testing.T) {
	runE2E(
		t, "pnpmlocal-input-change-invalidates",
		runStep(),
		writeStep("input.txt", "world\n"),
		runStep(),
	)
}

// TestRunner_EmptyResolvedOutputsErrors guards against silently caching a successful run
// whose declared output patterns matched zero files. A generator that exits 0 without
// writing anything must fail loudly; otherwise the empty output set is persisted and
// future runs hit the cache forever.
func TestRunner_EmptyResolvedOutputsErrors(t *testing.T) {
	workdir := t.TempDir()
	specDir := filepath.Join(workdir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: writes-nothing
    cmd: ["sh", "-c", "true"]
    inputs: ["input.txt"]
    outputs: ["output.txt"]
    tools: [versioner]
`
	if err := os.WriteFile(filepath.Join(specDir, "lazygen.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := spec.Discover(workdir, "**/lazygen.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(script.New(workdir))
	resolverReg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
		Clock:     func() time.Time { return fixedClock },
	})
	err = r.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when output pattern matches no files")
	}
	if !strings.Contains(err.Error(), "output.txt") {
		t.Errorf("error should mention the failing output pattern, got: %v", err)
	}

	cacheDir := filepath.Join(workdir, ".lazygen", "cache")
	if entries, err := os.ReadDir(cacheDir); err == nil && len(entries) > 0 {
		t.Errorf("cache record must not be written for failed run: %v", entries)
	}
}

// TestRunner_DuplicateProducerAtRuntimeErrors guards against silently overwriting an
// output produced by another task. Static depgraph detection only fires when the file
// already exists at planning time (e.g. the previous run committed it). For repos that
// gitignore generated files, the conflict only becomes observable after a task actually
// writes the path, so the runner cross-checks resolved outputs across tasks at runtime
// and aborts the run with both task names so the user can fix the spec.
func TestRunner_DuplicateProducerAtRuntimeErrors(t *testing.T) {
	workdir := t.TempDir()
	specDir := filepath.Join(workdir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: first
    cmd: ["sh", "-c", "cp input.txt shared.txt"]
    inputs: ["input.txt"]
    outputs: ["shared.txt"]
    tools: [versioner]
  - name: second
    cmd: ["sh", "-c", "cp input.txt shared.txt"]
    inputs: ["input.txt"]
    outputs: ["shared.txt"]
    tools: [versioner]
`
	if err := os.WriteFile(filepath.Join(specDir, "lazygen.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := spec.Discover(workdir, "**/lazygen.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(script.New(workdir))
	resolverReg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
		Clock:     func() time.Time { return fixedClock },
	})
	err = r.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when two tasks produced the same output path")
	}
	for _, want := range []string{"shared.txt", "first", "second"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// TestRunner_DuplicateToolNameAcrossSpecsErrors guards the ADR-0008 D2
// invariant: tool names live in a flat repo-wide namespace, so two
// lazygen.yml files defining the same name must fail the run with both
// definition sites named — silently picking one would diverge cache results
// from what the user wrote.
func TestRunner_DuplicateToolNameAcrossSpecsErrors(t *testing.T) {
	workdir := t.TempDir()
	for _, dir := range []string{"a", "b"} {
		full := filepath.Join(workdir, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, "input.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workdir, "a", "lazygen.yml"), []byte(`tools:
  shared:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
commands:
  - name: a
    cmd: ["sh", "-c", "cp input.txt out.txt"]
    inputs: ["input.txt"]
    outputs: ["out.txt"]
    tools: [shared]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "b", "lazygen.yml"), []byte(`tools:
  shared:
    exec: ["sh", "-c", "echo v2.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
commands:
  - name: b
    cmd: ["sh", "-c", "cp input.txt out.txt"]
    inputs: ["input.txt"]
    outputs: ["out.txt"]
    tools: [shared]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := spec.Discover(workdir, "**/lazygen.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(script.New(workdir))
	resolverReg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
		Clock:     func() time.Time { return fixedClock },
	})
	err = r.Run(context.Background())
	if err == nil {
		t.Fatal("expected error on duplicate tool name across specs")
	}
	for _, want := range []string{"shared", "a/lazygen.yml", "b/lazygen.yml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// TestRunner_UndefinedToolReferenceErrors catches the case a task references
// a tool name that no lazygen.yml declared. ADR-0008 requires this to fail
// at validation time rather than silently produce empty contributions.
func TestRunner_UndefinedToolReferenceErrors(t *testing.T) {
	workdir := t.TempDir()
	specDir := filepath.Join(workdir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "input.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "lazygen.yml"), []byte(`commands:
  - name: gen
    cmd: ["sh", "-c", "cp input.txt out.txt"]
    inputs: ["input.txt"]
    outputs: ["out.txt"]
    tools: [missing-tool]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := spec.Discover(workdir, "**/lazygen.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(script.New(workdir))
	resolverReg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
		Clock:     func() time.Time { return fixedClock },
	})
	err = r.Run(context.Background())
	if err == nil {
		t.Fatal("expected error on undefined tool reference")
	}
	for _, want := range []string{"missing-tool", "gen", "spec/lazygen.yml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// TestRunner_PartialOutputPatternsAllowed verifies that a generator that produces some
// declared output patterns but leaves others empty (e.g. a conditional artifact) is
// treated as a successful run. The union safeguard only fails when no declared pattern
// resolved to any file at all.
func TestRunner_PartialOutputPatternsAllowed(t *testing.T) {
	workdir := t.TempDir()
	specDir := filepath.Join(workdir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: partial
    cmd: ["sh", "-c", "cp input.txt produced.txt"]
    inputs: ["input.txt"]
    outputs: ["produced.txt", "optional/*.txt"]
    tools: [versioner]
`
	if err := os.WriteFile(filepath.Join(specDir, "lazygen.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := spec.Discover(workdir, "**/lazygen.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(script.New(workdir))
	resolverReg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
		Clock:     func() time.Time { return fixedClock },
	})
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("expected success when at least one declared pattern produced files, got: %v", err)
	}
}
