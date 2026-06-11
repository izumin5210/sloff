package runner_test

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
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

// fixedClock is injected via local.WithClock for every E2E run so the
// timestamp prefix on the on-disk record filenames (ADR-0010,
// `<YYYYMMDDHHMMSSsss>-<input_hash>.pb`) is deterministic and the goldens
// under testdata/e2e/runner/<case>/expected/ are byte-stable.
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
// the on-disk record filename's timestamp prefix is deterministic for golden compare.
func runStep(opts ...runStepOption) step {
	cfg := runStepConfig{}
	for _, o := range opts {
		o(&cfg)
	}
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
			Storage:   local.New(h.workdir, local.WithClock(func() time.Time { return fixedClock })),
			Resolvers: resolverReg,
			Preflight: preflightReg,
			Force:     cfg.force,
		})
		err = r.Run(context.Background())
		if cfg.wantErr != "" {
			if err == nil {
				t.Fatalf("Run: expected error containing %q, got nil", cfg.wantErr)
			}
			if !strings.Contains(err.Error(), cfg.wantErr) {
				t.Fatalf("Run: error %q does not contain %q", err, cfg.wantErr)
			}
			return
		}
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
}

type runStepConfig struct {
	force   bool
	wantErr string
}

type runStepOption func(*runStepConfig)

// withForce flips Options.Force for this runStep so it exercises the ADR-0012
// fingerprint bypass path.
func withForce() runStepOption {
	return func(c *runStepConfig) { c.force = true }
}

// expectError makes the step assert that Run fails with an error containing
// substr, instead of failing the test on error.
func expectError(substr string) runStepOption {
	return func(c *runStepConfig) { c.wantErr = substr }
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
//
// For each .pb fingerprint:
//   - the raw bytes are added at the .pb path (byte stability check)
//   - if there is no committed .json sibling on disk, a synthesised one decoded
//     via fingerprint.MarshalJSON is added at the .json path
//
// Committed .json siblings (in expectedDir) are read as-is. The asymmetry —
// workdir synthesises, expectedDir reads from disk — is what catches drift
// between the committed JSON and what the .pb actually decodes to.
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
		slashRel := filepath.ToSlash(rel)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[slashRel] = string(b)
		if filepath.Ext(p) == fingerprint.FileExt && !hasJSONSibling(p) {
			j, err := decodeRecordToJSON(b)
			if err != nil {
				return fmt.Errorf("decode %s: %w", p, err)
			}
			out[strings.TrimSuffix(slashRel, fingerprint.FileExt)+".json"] = string(j)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// mirrorTree copies all regular files (not symlinks) from src into dst, creating dst and
// any necessary subdirectories. The .git directory is skipped (see readTree). Each .pb
// fingerprint additionally writes a decoded .json sibling so the goldens carry both
// the canonical bytes and the human-readable view.
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
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return err
		}
		if filepath.Ext(p) == fingerprint.FileExt {
			j, err := decodeRecordToJSON(b)
			if err != nil {
				return fmt.Errorf("decode %s: %w", p, err)
			}
			jsonTarget := strings.TrimSuffix(target, fingerprint.FileExt) + ".json"
			if err := os.WriteFile(jsonTarget, j, 0o644); err != nil {
				return err
			}
		}
		return nil
	})
}

// decodeRecordToJSON unmarshals the proto wire bytes of a fingerprint and
// re-encodes them via fingerprint.MarshalJSON, the same path `sloff fingerprint show` uses.
func decodeRecordToJSON(pb []byte) ([]byte, error) {
	rec, err := fingerprint.Unmarshal(pb)
	if err != nil {
		return nil, err
	}
	return fingerprint.MarshalJSON(rec)
}

func hasJSONSibling(pbPath string) bool {
	json := strings.TrimSuffix(pbPath, fingerprint.FileExt) + ".json"
	_, err := os.Stat(json)
	return err == nil
}

func TestRunner_FirstRunWritesRecord(t *testing.T) {
	runE2E(t, "first-run-writes-record", runStep())
}

func TestRunner_SecondRunHits(t *testing.T) {
	runE2E(t, "second-run-hits", runStep(), runStep())
}

// TestRunner_ForceBypassesHit covers ADR-0012: the second run is invoked with
// Force=true so the fingerprint hit decision is bypassed even though inputs and
// the recorded output_hash match. The cmd appends to ../marker.txt, so the
// fixture's marker file proves whether cmd actually re-ran. Output bytes are
// identical across runs, so ADR-0009 §4 write-skip keeps the record file
// byte-stable (same timestamp prefix, same hash).
func TestRunner_ForceBypassesHit(t *testing.T) {
	runE2E(t, "force-bypasses-hit", runStep(), runStep(withForce()))
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
// (resolved_versions_hash via source change AND output_hash via content change).
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
// It is dropped into packages/codegen/dist/lib.js so the resolved_versions_hash changes
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
// the workspace package must flip resolved_versions_hash and trigger re-execution.
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
// regression where the runner stops recognising cross-dir outputs as fingerprintable.
func TestRunner_CrossDirOutputsRoundTrip(t *testing.T) {
	runE2E(t, "cross-dir-outputs", runStep(), runStep())
}

// TestRunner_PerSpecDistinctOutputsDoNotCollide is the IZU-18 regression guard at the
// E2E layer: two service-local sloff.yml files each declare `outputs:
// ["internal/foo.gen.go"]`, which resolve to distinct repo-relative paths
// (services/a/internal/foo.gen.go vs services/b/...). Before the fix the runner keyed
// detectOutputPatternConflicts by raw pattern string and refused to start with
// "duplicate output pattern producers". After the fix both tasks run, each generator
// lands its own file, and the fingerprint stores one record per spec.
func TestRunner_PerSpecDistinctOutputsDoNotCollide(t *testing.T) {
	runE2E(t, "per-spec-distinct-outputs", runStep())
}

// TestRunner_EmptyResolvedOutputsErrors guards against silently caching a successful run
// whose declared output patterns matched zero files. A generator that exits 0 without
// writing anything must fail loudly; otherwise the empty output set is persisted and
// future runs hit the fingerprint forever.
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
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
	})
	err = r.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when output pattern matches no files")
	}
	if !strings.Contains(err.Error(), "output.txt") {
		t.Errorf("error should mention the failing output pattern, got: %v", err)
	}

	fingerprintDir := filepath.Join(workdir, ".sloff", "fingerprints")
	if entries, err := os.ReadDir(fingerprintDir); err == nil && len(entries) > 0 {
		t.Errorf("fingerprint must not be written for failed run: %v", entries)
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
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
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

// TestRunner_PrefetchToleratesMissingInputFile locks the contract that a
// declared input that does not exist on disk does NOT make
// prefetchFingerprints abort the run with hash.Files crashing on a
// missing path. glob.Expand silently drops patterns that match no files
// (doublestar.Glob's filesystem-backed semantics), so info.inputPaths
// is empty for such tasks and hash.Files runs on an empty slice — a
// successful no-op that produces the SHA-256 of the empty digest. The
// case matters because real specs sometimes declare inputs whose files
// only exist after an unrelated task has run; that depgraph blind spot
// is tracked separately (DEV-23). This test pins the *prefetch*
// behaviour without depending on inter-task ordering.
func TestRunner_PrefetchToleratesMissingInputFile(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := t.TempDir()
	specDir := filepath.Join(workdir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A single task whose declared input is missing. Single task = no
	// inter-task races; the only thing that can fail this test is
	// prefetch (or hash.Files) crashing on a non-existent path.
	yml := `tools:
  v:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: solo
    cmd: ["sh", "-c", "echo ok > out.txt"]
    inputs: ["does-not-exist.txt"]
    outputs: ["out.txt"]
    tools: [v]
`
	if err := os.WriteFile(filepath.Join(specDir, "sloff.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	specs, err := spec.Discover(workdir, "**/sloff.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(script.New(workdir))
	resolverReg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
	})
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run aborted on missing input declaration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(specDir, "out.txt")); err != nil {
		t.Errorf("expected generator to have produced out.txt, got %v", err)
	}
}

// TestRunner_FallbackLoadServesTransitiveCacheHitAfterUpstreamRegen
// guards against the "upstream regen breaks downstream cache" regression
// Codex flagged: when an upstream task is a miss and regenerates its
// outputs mid-run, the downstream task's runtime input_hash diverges
// from the optimistic input_hash the prefetch computed against the
// pre-run on-disk bytes. Without a live-Load fallback, the downstream
// task would treat the prefetched-map miss as authoritative and rerun
// its generator even when the backend already holds a valid record for
// the post-regen key (typical when a colleague has already run sloff
// on this branch and uploaded results).
func TestRunner_FallbackLoadServesTransitiveCacheHitAfterUpstreamRegen(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := t.TempDir()
	specDir := filepath.Join(workdir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// `upstream` reads marker.txt and writes a-output.txt;
	// `downstream` reads a-output.txt and writes b-output.txt.
	// Using `sed` keeps the transformation deterministic so
	// regenerated bytes are stable across runs (ADR-0009).
	write := func(name, contents string) {
		if err := os.WriteFile(filepath.Join(specDir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("marker.txt", "v2\n")
	write("a-output.txt", "out-from-v2\n")
	write("b-output.txt", "b-from-out-from-v2\n")
	yml := `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: upstream
    cmd: ["sh", "-c", "sed 's/^/out-from-/' marker.txt > a-output.txt"]
    inputs: ["marker.txt"]
    outputs: ["a-output.txt"]
    tools: [versioner]
  - name: downstream
    cmd: ["sh", "-c", "sed 's/^/b-from-/' a-output.txt > b-output.txt"]
    inputs: ["a-output.txt"]
    outputs: ["b-output.txt"]
    tools: [versioner]
    depends:
      - task: upstream
`
	write("sloff.yml", yml)

	specs, err := spec.Discover(workdir, "**/sloff.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	newResolverReg := func() *toolresolver.Registry {
		reg := toolresolver.NewRegistry()
		reg.Register(script.New(workdir))
		reg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
		return reg
	}

	// Pass 1: populate the backend as if a colleague had already run
	// sloff against the v2 state. Both records land in storage.
	store := local.New(workdir, local.WithClock(func() time.Time { return fixedClock }))
	primer := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   store,
		Resolvers: newResolverReg(),
		Preflight: preflight.NewRegistry(),
	})
	if err := primer.Run(context.Background()); err != nil {
		t.Fatalf("primer Run: %v", err)
	}

	// Stale the upstream output on disk so its hash diverges from the
	// recorded post-regen hash. The downstream output stays as-is
	// (matches the recorded post-regen hash) so output-comparison on
	// the downstream side will succeed once the cached record is
	// recovered via the fallback Load.
	write("a-output.txt", "out-from-v1\n")

	// Pass 2: this is the run under test. Capture downstream's
	// b-output.txt mtime before and after to detect whether the
	// generator ran.
	bOutPath := filepath.Join(specDir, "b-output.txt")
	beforeStat, err := os.Stat(bOutPath)
	if err != nil {
		t.Fatal(err)
	}

	rerun := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   store,
		Resolvers: newResolverReg(),
		Preflight: preflight.NewRegistry(),
	})
	if err := rerun.Run(context.Background()); err != nil {
		t.Fatalf("rerun: %v", err)
	}

	// Upstream must have regenerated (its stale on-disk a-output was
	// rebuilt to match the cached record), otherwise the scenario
	// would not exercise the divergent-key code path at all.
	aBytes, err := os.ReadFile(filepath.Join(specDir, "a-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(aBytes)), "out-from-v2"; got != want {
		t.Fatalf("upstream did not regenerate a-output.txt: got %q, want %q", got, want)
	}

	// The contract under test: downstream resolved its cache hit
	// without re-running. mtime equality across the rerun is the
	// signal — a re-execution of `sed > b-output.txt` rewrites the
	// file even if the bytes are unchanged, so mtime would jump.
	afterStat, err := os.Stat(bOutPath)
	if err != nil {
		t.Fatal(err)
	}
	if !afterStat.ModTime().Equal(beforeStat.ModTime()) {
		t.Errorf("downstream ran its generator instead of falling back to a cache hit: b-output.txt mtime %v -> %v",
			beforeStat.ModTime(), afterStat.ModTime())
	}
}

// TestRunner_FlushPersistsRecordsAfterPartialFailure guards against the
// queueing regression Codex flagged: when one task succeeds and a later
// task fails, the successful task's fingerprint record must still be on
// disk after Run returns the failure error. The pre-bulk implementation
// achieved this because each task wrote its own record synchronously
// inside fingerprintStore. The bulk implementation defers writes to a
// single SaveMany at the end of Run; the flush therefore has to run
// even when runTasks returns an error, otherwise a single late failure
// invalidates the cache for every earlier success.
func TestRunner_FlushPersistsRecordsAfterPartialFailure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	workdir := t.TempDir()
	specDir := filepath.Join(workdir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "input.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// fail-task declares depends on ok-task so the scheduler guarantees
	// ok-task completes (and enqueues its record) before fail-task starts
	// failing (ADR-0013: ordering comes from the declaration, not from
	// file overlap, so no placeholder file is needed on a clean dir).
	yml := `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: ok-task
    cmd: ["sh", "-c", "cp input.txt good.txt"]
    inputs: ["input.txt"]
    outputs: ["good.txt"]
    tools: [versioner]
  - name: fail-task
    cmd: ["sh", "-c", "exit 1"]
    inputs: ["good.txt"]
    outputs: ["never-written.txt"]
    tools: [versioner]
    depends:
      - task: ok-task
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
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
	})
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("expected error from fail-task, got nil")
	}

	// ok-task ran to completion before fail-task, so its record must
	// have been persisted by the end-of-run flush even though the run
	// itself ended in error.
	okTaskRecordDir := filepath.Join(workdir, ".sloff", "fingerprints", "spec", "ok-task")
	entries, err := os.ReadDir(okTaskRecordDir)
	if err != nil {
		t.Fatalf("ok-task record dir missing (flush did not run on error path): %v", err)
	}
	if len(entries) == 0 {
		t.Errorf("expected at least one fingerprint record for ok-task after partial failure, got 0")
	}

	// fail-task's record must NOT be on disk: runTask never enqueues a
	// record when the generator fails, so flushFingerprints would never
	// see it. Asserting this guards against an accidental "queue
	// everything before exec" regression.
	failTaskRecordDir := filepath.Join(workdir, ".sloff", "fingerprints", "spec", "fail-task")
	if entries, err := os.ReadDir(failTaskRecordDir); err == nil && len(entries) > 0 {
		t.Errorf("fail-task fingerprint must not be persisted, got: %v", entries)
	}
}

// TestRunner_PnpmLocal_FailsWhenInstallSnapshotMissing guards the drift
// preflight end to end: when a task references a pnpm-local tool but
// node_modules/.pnpm/lock.yaml is missing (pnpm install was never run
// against this checkout), the runner aborts before any cmd executes.
// Without the abort, the resolver would hand the cmd a stale-install fingerprint
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
// the runner continues in read-only mode (fingerprints are not written).
// This is the existing preflight escape hatch; pnpm-local's drift checker
// inherits it by virtue of going through the preflight subsystem instead of
// failing inside the resolver.
func TestRunner_PnpmLocal_DriftDegradesToReadOnlyUnderEscapeHatch(t *testing.T) {
	workdir, specs := setupPnpmDriftFixture(t, false /* installInSync */)
	r := newPnpmDriftRunner(t, workdir, specs, true /* readOnly */)

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("expected drift to degrade to read-only under SLOFF_ALLOW_STALE_DEPS, got: %v", err)
	}
	fingerprintDir := filepath.Join(workdir, ".sloff", "fingerprints")
	if entries, err := os.ReadDir(fingerprintDir); err == nil && len(entries) > 0 {
		t.Errorf("read-only mode must not write fingerprints, got: %v", entries)
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
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: resolverReg,
		Preflight: preflightReg,
		ReadOnly:  readOnly,
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
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
	})
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run must succeed when broken tool is unreferenced, got: %v", err)
	}
}

// TestRunner_DuplicateToolNameAcrossSpecsErrors guards the ADR-0008 D2
// invariant: tool names live in a flat repo-wide namespace, so two
// sloff.yml files defining the same name must fail the run with both
// definition sites named — silently picking one would diverge fingerprint results
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
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
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
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
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
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(),
	})
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("expected success when at least one declared pattern produced files, got: %v", err)
	}
}

// TestRunner_ConcurrentFirstWriteMergeHits exercises the post-merge state that
// motivated ADR-0010: two branches independently produce a first-time record
// for the same (spec, task, input_hash) Key, so after merge the directory
// holds two `<timestamp>-<hash>.pb` files for the same Key. A subsequent
// `sloff run` must:
//   - Load the latest by filename timestamp and use it for output-comparison
//   - fingerprint hit (no cmd execution; marker.txt does not advance)
//   - leave both files on disk because Save is not invoked on a hit
//
// The earlier hash-only filename layout would have produced a byte-level
// merge conflict at this exact moment (different `generated_at`); under the
// new layout this scenario is a no-op for git, the runner, and the user.
func TestRunner_ConcurrentFirstWriteMergeHits(t *testing.T) {
	workdir := t.TempDir()
	specDir := filepath.Join(workdir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "input.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: copy
    cmd: ["sh", "-c", "cp input.txt output.txt; printf x >> ../marker.txt"]
    inputs: ["input.txt"]
    outputs: ["output.txt"]
    tools: [versioner]
`
	if err := os.WriteFile(filepath.Join(specDir, "sloff.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	build := func() *runner.Runner {
		t.Helper()
		specs, err := spec.Discover(workdir, "**/sloff.yml")
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		resolverReg := toolresolver.NewRegistry()
		resolverReg.Register(script.New(workdir))
		resolverReg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
		return runner.New(runner.Options{
			RepoRoot:  workdir,
			Specs:     specs,
			Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
			Resolvers: resolverReg,
			Preflight: preflight.NewRegistry(),
		})
	}

	// First run produces the canonical record at fixedClock's timestamp.
	if err := build().Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	fingerprintDir := filepath.Join(workdir, ".sloff", "fingerprints", "spec", "copy")
	entries, err := os.ReadDir(fingerprintDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("first Run should yield exactly one record, got %d: %v", len(entries), entries)
	}
	original := entries[0].Name()
	suffixIdx := strings.IndexByte(original, '-')
	if suffixIdx < 0 {
		t.Fatalf("unexpected filename %q (no timestamp prefix)", original)
	}
	hashSuffix := original[suffixIdx:]

	// Simulate the merge: drop a sibling `<earlier-timestamp>-<hash>.pb` next
	// to the existing record. Bytes are intentionally identical because the
	// generator is deterministic; only the filename's timestamp prefix
	// differs, which is exactly the ADR-0010 disambiguator.
	bytes, err := os.ReadFile(filepath.Join(fingerprintDir, original))
	if err != nil {
		t.Fatal(err)
	}
	earlier := "20260101000000000" + hashSuffix
	if err := os.WriteFile(filepath.Join(fingerprintDir, earlier), bytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// Snapshot marker.txt before the second run so we can assert no cmd ran.
	markerPath := filepath.Join(workdir, "marker.txt")
	beforeMarker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}

	// Second run: must fingerprint hit on output-comparison and not invoke Save.
	if err := build().Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	afterMarker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeMarker) != string(afterMarker) {
		t.Errorf("expected fingerprint hit (no cmd execution) on second run; marker advanced %q -> %q",
			beforeMarker, afterMarker)
	}

	// Both timestamp variants must still be present: a hit does not invoke
	// Save, so the duplicate-collapse path is not exercised here. (`sloff
	// fingerprint gc` is the planned collapse trigger for this exact state.)
	postEntries, err := os.ReadDir(fingerprintDir)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(postEntries))
	for _, e := range postEntries {
		got[e.Name()] = true
	}
	for _, want := range []string{original, earlier} {
		if !got[want] {
			t.Errorf("expected %q preserved on fingerprint hit, dir contents=%v", want, got)
		}
	}
}

// TestRunner_ConcurrentFirstWriteCollapsesOnRewrite covers the rare branch of
// ADR-0010's Save semantics: when two branches' first-writes were merged
// (multiple `<timestamp>-<hash>.pb` for the same Key) and a subsequent run
// happens to land in the fingerprint-miss-with-different-output path, Save must
// collapse the duplicates onto the earliest-prefix file. The
// "different-output for the same input" case is a non-deterministic
// generator (out of sloff scope), so the test forces the situation by
// hand-crafting a pre-existing record whose output.hash deliberately
// disagrees with what the generator currently produces.
// TestRunner_DependsMissingAtPlanTimeErrors locks ADR-0013 D3's plan-time
// check: produced.txt exists on disk, so the consumer/producer overlap is
// observable before execution and Run must fail — with the exact depends
// entry to add — without executing any task.
func TestRunner_DependsMissingAtPlanTimeErrors(t *testing.T) {
	runE2E(
		t, "depends-missing-plan-error",
		runStep(expectError("undeclared task dependencies")),
	)
}

func TestRunner_ConcurrentFirstWriteCollapsesOnRewrite(t *testing.T) {
	workdir := t.TempDir()
	specDir := filepath.Join(workdir, "spec")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "input.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yml := `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: copy
    cmd: ["sh", "-c", "cp input.txt output.txt"]
    inputs: ["input.txt"]
    outputs: ["output.txt"]
    tools: [versioner]
`
	if err := os.WriteFile(filepath.Join(specDir, "sloff.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}

	build := func() *runner.Runner {
		t.Helper()
		specs, err := spec.Discover(workdir, "**/sloff.yml")
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		resolverReg := toolresolver.NewRegistry()
		resolverReg.Register(script.New(workdir))
		resolverReg.Register(golocal.New(workdir, lister.NewMemoized(lister.NewGoPackages(workdir))))
		return runner.New(runner.Options{
			RepoRoot:  workdir,
			Specs:     specs,
			Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
			Resolvers: resolverReg,
			Preflight: preflight.NewRegistry(),
		})
	}

	// First run lays down the legitimate record so we know the input_hash on disk.
	if err := build().Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	fingerprintDir := filepath.Join(workdir, ".sloff", "fingerprints", "spec", "copy")
	entries, err := os.ReadDir(fingerprintDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("first Run should yield 1 record, got entries=%v err=%v", entries, err)
	}
	original := entries[0].Name()
	hashSuffix := original[strings.IndexByte(original, '-'):]

	// Inject a hand-crafted earlier-prefix record whose output.hash will
	// fail output-comparison so the runner enters the rewrite path. Bytes
	// reuse the original record except output.hash is mutated.
	originalBytes, err := os.ReadFile(filepath.Join(fingerprintDir, original))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := fingerprint.Unmarshal(originalBytes)
	if err != nil {
		t.Fatal(err)
	}
	rec.Output.Hash = strings.Repeat("0", len(rec.Output.Hash))
	mutated, err := fingerprint.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	earlier := "20260101000000000" + hashSuffix
	if err := os.WriteFile(filepath.Join(fingerprintDir, earlier), mutated, 0o644); err != nil {
		t.Fatal(err)
	}
	// Drop the legitimate later record so the load path actually picks up
	// the mutated one (load returns latest; we only want one survivor for
	// load deterministically). Simulating "two branches each only saw their
	// own record" in a controlled way.
	if err := os.Remove(filepath.Join(fingerprintDir, original)); err != nil {
		t.Fatal(err)
	}
	// Add a second duplicate (later than `earlier`) so Save has duplicates
	// to collapse.
	later := "20260601000000000" + hashSuffix
	if err := os.WriteFile(filepath.Join(fingerprintDir, later), mutated, 0o644); err != nil {
		t.Fatal(err)
	}

	// Run again. Output-comparison fails (mutated record has all-zeros
	// output.hash), generator runs, Save fires, duplicates are collapsed.
	if err := build().Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	postEntries, err := os.ReadDir(fingerprintDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(postEntries) != 1 {
		var names []string
		for _, e := range postEntries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly 1 record after collapse, got %d: %v", len(postEntries), names)
	}
	if postEntries[0].Name() != earlier {
		t.Errorf("expected earliest-prefix retained (%q), got %q", earlier, postEntries[0].Name())
	}
}
