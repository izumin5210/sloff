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

	"github.com/izumin5210/sloff/internal/sloff/cache/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	preflightpnpm "github.com/izumin5210/sloff/internal/sloff/preflight/pnpmlocal"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/golocal"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

// updateGolden rewrites testdata/e2e/runner/<case>/expected/ from the actual final state
// of the test instead of comparing against it. Used to refresh fixtures after intentional
// behaviour changes:
//
//	go test ./internal/sloff/runner/... -update
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
		{"config", "user.email", "sloff-test@example.com"},
		{"config", "user.name", "sloff-test"},
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
		specs, err := spec.Discover(h.workdir, "**/sloff.yml")
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
		preflightReg := preflight.NewRegistry()
		preflightReg.Register(preflightpnpm.New(h.workdir))
		r := runner.New(runner.Options{
			RepoRoot:  h.workdir,
			Specs:     specs,
			Storage:   local.New(h.workdir),
			Resolvers: resolverReg,
			Preflight: preflightReg,
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
		writeStep("spec/sloff.yml", `tools:
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
// rebasing: when sloff.yml lives under spec/ and the cmd is `go run ./cmd/copy`,
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

// TestRunner_CrossDirOutputsRoundTrip exercises the IZU-17 cross-dir glob
// support end to end: the spec lives at proto/spec/sloff.yml but the generator
// writes outputs into ../../gen/go/. The first run must materialise the
// outputs and persist a record; the second run must locate the same record,
// re-hash the cross-dir output, and SKIP without re-executing the cmd. The
// marker.txt counter (incremented on every cmd execution) detects any
// regression where the runner stops recognising cross-dir outputs as cacheable.
func TestRunner_CrossDirOutputsRoundTrip(t *testing.T) {
	runE2E(t, "cross-dir-outputs", runStep(), runStep())
}

// TestRunner_PerSpecDistinctOutputsDoNotCollide is the IZU-18 regression guard at the
// E2E layer: two service-local sloff.yml files each declare `outputs:
// ["internal/foo.gen.go"]`, which resolve to distinct repo-relative paths
// (services/a/internal/foo.gen.go vs services/b/...). Before the fix the runner keyed
// detectOutputPatternConflicts by raw pattern string and refused to start with
// "duplicate output pattern producers". After the fix both tasks run, each generator
// lands its own file, and the cache stores one record per spec.
func TestRunner_PerSpecDistinctOutputsDoNotCollide(t *testing.T) {
	runE2E(t, "per-spec-distinct-outputs", runStep())
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
	if err := os.WriteFile(filepath.Join(specDir, "sloff.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := spec.Discover(workdir, "**/sloff.yml")
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

	cacheDir := filepath.Join(workdir, ".sloff", "cache")
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
	if err := os.WriteFile(filepath.Join(specDir, "sloff.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := spec.Discover(workdir, "**/sloff.yml")
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

// TestRunner_PnpmLocal_FailsWhenInstallSnapshotMissing guards the drift
// preflight end to end: when a task references a pnpm-local tool but
// node_modules/.pnpm/lock.yaml is missing (pnpm install was never run
// against this checkout), the runner aborts before any cmd executes.
// Without the abort, the resolver would hand the cmd a stale-install cache
// key and silent stale outputs would propagate.
func TestRunner_PnpmLocal_FailsWhenInstallSnapshotMissing(t *testing.T) {
	workdir, specs := setupPnpmDriftFixture(t, false /* installInSync */)
	r := newPnpmDriftRunner(t, workdir, specs, false /* readOnly */)

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when node_modules/.pnpm/lock.yaml is missing")
	}
	if !strings.Contains(err.Error(), "preflight failed") {
		t.Errorf("error should mention preflight failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "SLOFF_ALLOW_STALE_DEPS") {
		t.Errorf("error should mention the bypass env var, got: %v", err)
	}
}

// TestRunner_PnpmLocal_DriftDegradesToReadOnlyUnderEscapeHatch covers the
// SLOFF_ALLOW_STALE_DEPS=1 path: drift surfaces as a preflight Issue but
// the runner continues in read-only mode (cache records are not written).
// This is the existing preflight escape hatch; pnpm-local's drift checker
// inherits it by virtue of going through the preflight subsystem instead of
// failing inside the resolver.
func TestRunner_PnpmLocal_DriftDegradesToReadOnlyUnderEscapeHatch(t *testing.T) {
	workdir, specs := setupPnpmDriftFixture(t, false /* installInSync */)
	r := newPnpmDriftRunner(t, workdir, specs, true /* readOnly */)

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("expected drift to degrade to read-only under SLOFF_ALLOW_STALE_DEPS, got: %v", err)
	}
	cacheDir := filepath.Join(workdir, ".sloff", "cache")
	if entries, err := os.ReadDir(cacheDir); err == nil && len(entries) > 0 {
		t.Errorf("read-only mode must not write cache records, got: %v", entries)
	}
}

// setupPnpmDriftFixture materialises a minimal repo that uses a pnpm-local
// tool, with or without a matching install snapshot. installInSync=false
// leaves node_modules/.pnpm/lock.yaml absent so AssertInstallInSync fails;
// true mirrors pnpm-lock.yaml into it so the drift check passes.
func setupPnpmDriftFixture(t *testing.T, installInSync bool) (string, []spec.Spec) {
	t.Helper()
	workdir := t.TempDir()
	gitInitWorkdir(t, workdir)
	write := func(rel, contents string) {
		full := filepath.Join(workdir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const lockfile = `lockfileVersion: '9.0'
importers:
  packages/codegen: {}
`
	write("pnpm-lock.yaml", lockfile)
	write("package.json", `{"name":"monorepo-root","private":true}`)
	write("packages/codegen/package.json", `{"name":"@org/codegen"}`)
	write("input.txt", "hello")
	write("sloff.yml", `tools:
  codegen:
    pnpm-local: "@org/codegen"

commands:
  - name: gen
    cmd: ["sh", "-c", "cp input.txt out.txt"]
    inputs: ["input.txt"]
    outputs: ["out.txt"]
    tools: [codegen]
`)
	if installInSync {
		write("node_modules/.pnpm/lock.yaml", lockfile)
	}

	specs, err := spec.Discover(workdir, "**/sloff.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	return workdir, specs
}

// newPnpmDriftRunner wires the pnpm-local resolver and preflight checker
// the same way the production CLI does, so the drift-detection paths get
// exercised end to end.
func newPnpmDriftRunner(t *testing.T, workdir string, specs []spec.Spec, readOnly bool) *runner.Runner {
	t.Helper()
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(script.New(workdir))
	resolverReg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
	pnpmRes, err := pnpmlocal.New(workdir, pnpmlocal.GitLsFiles)
	if err != nil {
		t.Fatalf("pnpmlocal.New: %v", err)
	}
	resolverReg.Register(pnpmRes)
	preflightReg := preflight.NewRegistry()
	preflightReg.Register(preflightpnpm.New(workdir))
	return runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir),
		Resolvers: resolverReg,
		Preflight: preflightReg,
		ReadOnly:  readOnly,
		Clock:     func() time.Time { return fixedClock },
	})
}

// TestRunner_UnreferencedBrokenToolDoesNotBlockOtherTasks guards that the
// pre-resolve pass scopes itself to tools commands actually reference. A
// catalog-style repo can declare tools whose dependencies are absent on the
// current machine (a pnpm-local entry whose workspace package isn't in this
// checkout, a script tool not installed locally); resolving them eagerly
// would fail the run for unrelated tasks. The test installs a script tool
// that exits non-zero — but no command references it — and expects the run
// to succeed for the task that uses a different, healthy tool.
func TestRunner_UnreferencedBrokenToolDoesNotBlockOtherTasks(t *testing.T) {
	workdir := t.TempDir()
	specDir := filepath.Join(workdir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "sloff.yml"), []byte(`tools:
  healthy:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  broken:
    exec: ["sh", "-c", "exit 7"]

commands:
  - name: gen
    cmd: ["sh", "-c", "cp input.txt out.txt"]
    inputs: ["input.txt"]
    outputs: ["out.txt"]
    tools: [healthy]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := spec.Discover(workdir, "**/sloff.yml")
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
		t.Fatalf("Run must succeed when broken tool is unreferenced, got: %v", err)
	}
}

// TestRunner_DuplicateToolNameAcrossSpecsErrors guards the ADR-0008 D2
// invariant: tool names live in a flat repo-wide namespace, so two
// sloff.yml files defining the same name must fail the run with both
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
	if err := os.WriteFile(filepath.Join(workdir, "a", "sloff.yml"), []byte(`tools:
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
	if err := os.WriteFile(filepath.Join(workdir, "b", "sloff.yml"), []byte(`tools:
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

	specs, err := spec.Discover(workdir, "**/sloff.yml")
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
	for _, want := range []string{"shared", "a/sloff.yml", "b/sloff.yml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// TestRunner_UndefinedToolReferenceErrors catches the case a task references
// a tool name that no sloff.yml declared. ADR-0008 requires this to fail
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
	if err := os.WriteFile(filepath.Join(specDir, "sloff.yml"), []byte(`commands:
  - name: gen
    cmd: ["sh", "-c", "cp input.txt out.txt"]
    inputs: ["input.txt"]
    outputs: ["out.txt"]
    tools: [missing-tool]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := spec.Discover(workdir, "**/sloff.yml")
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
	for _, want := range []string{"missing-tool", "gen", "spec/sloff.yml"} {
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
	if err := os.WriteFile(filepath.Join(specDir, "sloff.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	specs, err := spec.Discover(workdir, "**/sloff.yml")
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
