package runner_test

import (
	"context"
	"flag"
	"io/fs"
	"os"
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
	t            *testing.T
	caseDir      string // testdata/e2e/runner/<name>
	workdir      string // freshly-copied tempdir initialised from <caseDir>/initial
	expectedDir  string // <caseDir>/expected
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
	return &harness{
		t:           t,
		caseDir:     caseDir,
		workdir:     workdir,
		expectedDir: filepath.Join(caseDir, "expected"),
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
// root. Symlinks are not followed; directories are implied by their entries.
func readTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
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
// any necessary subdirectories. Existing dst contents are not deleted; callers should
// remove dst beforehand if they want a clean snapshot.
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
	runE2E(t, "input-change-invalidates",
		runStep(),
		writeStep("spec/input.txt", "world"),
		runStep(),
	)
}

func TestRunner_ToolVersionBumpInvalidates(t *testing.T) {
	runE2E(t, "tool-version-bump-invalidates",
		runStep(),
		writeStep("spec/lazygen.yml", `commands:
  - name: copy
    cmd: ["sh", "-c", "cp input.txt output.txt; printf x >> ../marker.txt"]
    inputs: ["input.txt"]
    outputs: ["output.txt"]
    tools:
      - exec: ["sh", "-c", "echo v2.0.0"]
        extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
`),
		runStep(),
	)
}

func TestRunner_OutputDriftInvalidates(t *testing.T) {
	runE2E(t, "output-drift-invalidates",
		runStep(),
		writeStep("spec/output.txt", "tampered"),
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
	yml := `commands:
  - name: writes-nothing
    cmd: ["sh", "-c", "true"]
    inputs: ["input.txt"]
    outputs: ["output.txt"]
    tools:
      - exec: ["sh", "-c", "echo v1.0.0"]
        extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
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
