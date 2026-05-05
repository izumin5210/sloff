package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/izumin5210/lazygen/internal/lazygen/cache/gitfile"
	"github.com/izumin5210/lazygen/internal/lazygen/preflight"
	"github.com/izumin5210/lazygen/internal/lazygen/runner"
	"github.com/izumin5210/lazygen/internal/lazygen/spec"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/script"
)

const fixtureScriptSimple = "script-simple"

// fixture wraps a freshly-copied testdata/e2e/<name> tree with helpers used by the tests.
//
// Tests mutate files inside f.root (a t.TempDir copy of the fixture) and rebuild the
// runner via f.newRunner so that the resolver is given a fresh per-run cache.
type fixture struct {
	t    *testing.T
	root string
}

func loadFixture(t *testing.T, name string) *fixture {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile is .../<repo>/internal/lazygen/runner/runner_test.go — repo root is 3 up.
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	src := filepath.Join(repoRoot, "testdata", "e2e", name)
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}
	return &fixture{t: t, root: dst}
}

func (f *fixture) newRunner() *runner.Runner {
	f.t.Helper()
	specs, err := spec.Discover(f.root, "**/lazygen.yml")
	if err != nil {
		f.t.Fatal(err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(script.New(f.root))
	return runner.New(runner.Options{
		RepoRoot:  f.root,
		Specs:     specs,
		Storage:   gitfile.New(f.root),
		Resolvers: resolverReg,
		Preflight: preflight.NewRegistry(), // PR1 ships no concrete checkers
	})
}

func (f *fixture) markerCount() int {
	b, err := os.ReadFile(filepath.Join(f.root, "marker.txt"))
	if err != nil {
		return 0
	}
	return len(b)
}

func (f *fixture) writeFile(relpath, contents string) {
	f.t.Helper()
	full := filepath.Join(f.root, relpath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func TestRunner_FirstRunWritesRecord(t *testing.T) {
	f := loadFixture(t, fixtureScriptSimple)
	if err := f.newRunner().Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.markerCount() != 1 {
		t.Errorf("expected 1 generator execution, got %d", f.markerCount())
	}
	matches, _ := filepath.Glob(filepath.Join(f.root, ".lazygen", "cache", "spec", "copy", "*.yml"))
	if len(matches) != 1 {
		t.Errorf("expected 1 record file, got %d (%v)", len(matches), matches)
	}
}

func TestRunner_SecondRunHits(t *testing.T) {
	f := loadFixture(t, fixtureScriptSimple)
	r := f.newRunner()
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.markerCount() != 1 {
		t.Errorf("second run should have hit cache, marker=%d", f.markerCount())
	}
}

func TestRunner_InputChangeInvalidates(t *testing.T) {
	f := loadFixture(t, fixtureScriptSimple)
	if err := f.newRunner().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.writeFile("spec/input.txt", "world")
	if err := f.newRunner().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.markerCount() != 2 {
		t.Errorf("input change should re-run, marker=%d", f.markerCount())
	}
}

func TestRunner_ToolVersionBumpInvalidates(t *testing.T) {
	f := loadFixture(t, fixtureScriptSimple)
	if err := f.newRunner().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Rewriting the tools[].exec to print a different version flips tools_hash → cache miss.
	f.writeFile("spec/lazygen.yml", `commands:
  - name: copy
    cmd: ["sh", "-c", "cp input.txt output.txt; printf x >> ../marker.txt"]
    inputs: ["input.txt"]
    outputs: ["output.txt"]
    tools:
      - exec: ["sh", "-c", "echo v2.0.0"]
        extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
`)
	if err := f.newRunner().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.markerCount() != 2 {
		t.Errorf("tool version bump should re-run, marker=%d", f.markerCount())
	}
}

func TestRunner_OutputDriftInvalidates(t *testing.T) {
	f := loadFixture(t, fixtureScriptSimple)
	if err := f.newRunner().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Modify the recorded output → output-comparison fails → re-run.
	f.writeFile("spec/output.txt", "tampered")
	if err := f.newRunner().Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.markerCount() != 2 {
		t.Errorf("output drift should re-run, marker=%d", f.markerCount())
	}
}
