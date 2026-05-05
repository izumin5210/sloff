package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/izumin5210/lazygen/internal/lazygen/cache/gitfile"
	"github.com/izumin5210/lazygen/internal/lazygen/preflight"
	preflightaqua "github.com/izumin5210/lazygen/internal/lazygen/preflight/aqua"
	"github.com/izumin5210/lazygen/internal/lazygen/runner"
	"github.com/izumin5210/lazygen/internal/lazygen/spec"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	resolveraqua "github.com/izumin5210/lazygen/internal/lazygen/toolresolver/aqua"
)

// fixture sets up a minimal repo with one task that copies input.txt to output.txt via a
// shell script. The generator also writes/refreshes a marker file so tests can detect
// whether the cmd actually executed (a cache hit must NOT touch the marker).
type fixture struct {
	t       *testing.T
	root    string
	specDir string
	marker  string
	runs    *runner.Runner
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	specDir := filepath.Join(root, "spec")
	mustWrite(t, filepath.Join(specDir, "input.txt"), "hello")
	// lazygen.yml: single task that copies input → output and bumps the marker counter.
	mustWrite(t, filepath.Join(specDir, "lazygen.yml"), `commands:
  - name: copy
    cmd: ["sh", "-c", "cp input.txt output.txt; printf x >> ../marker.txt"]
    inputs: ["input.txt"]
    outputs: ["output.txt"]
    tools:
      - aqua: example/copier
`)
	// aqua.yaml + matching checksums (preflight should pass).
	mustWrite(t, filepath.Join(root, "aqua.yaml"), `packages:
  - name: example/copier@v1.0.0
`)
	mustWrite(t, filepath.Join(root, "aqua-checksums.json"), `{
  "checksums": [
    {"id": "github.com/example/copier/releases/download/v1.0.0/copier.tar.gz", "algorithm": "sha256", "checksum": "deadbeef"}
  ]
}`)

	specs, err := spec.Discover(root, "**/lazygen.yml")
	if err != nil {
		t.Fatal(err)
	}

	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(must(resolveraqua.New(root)))

	preflightReg := preflight.NewRegistry()
	preflightReg.Register(must(preflightaqua.New(root)))

	r := runner.New(runner.Options{
		RepoRoot:  root,
		Specs:     specs,
		Storage:   gitfile.New(root),
		Resolvers: resolverReg,
		Preflight: preflightReg,
	})

	return &fixture{
		t:       t,
		root:    root,
		specDir: filepath.Join("spec"),
		marker:  filepath.Join(root, "marker.txt"),
		runs:    r,
	}
}

func (f *fixture) markerCount() int {
	b, err := os.ReadFile(f.marker)
	if err != nil {
		return 0
	}
	return len(b)
}

func TestRunner_FirstRunWritesRecord(t *testing.T) {
	f := newFixture(t)
	if err := f.runs.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.markerCount() != 1 {
		t.Errorf("expected 1 generator execution, got %d", f.markerCount())
	}
	// record file exists
	matches, _ := filepath.Glob(filepath.Join(f.root, ".lazygen", "cache", "spec", "copy", "*.yml"))
	if len(matches) != 1 {
		t.Errorf("expected 1 record file, got %d (%v)", len(matches), matches)
	}
}

func TestRunner_SecondRunHits(t *testing.T) {
	f := newFixture(t)
	if err := f.runs.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.runs.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.markerCount() != 1 {
		t.Errorf("second run should have hit cache, marker=%d", f.markerCount())
	}
}

func TestRunner_InputChangeInvalidates(t *testing.T) {
	f := newFixture(t)
	if err := f.runs.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.root, f.specDir, "input.txt"), "world")
	if err := f.runs.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.markerCount() != 2 {
		t.Errorf("input change should re-run, marker=%d", f.markerCount())
	}
}

func TestRunner_AquaVersionBumpInvalidates(t *testing.T) {
	f := newFixture(t)
	if err := f.runs.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Bump aqua version → tools_hash changes → cache miss.
	mustWrite(t, filepath.Join(f.root, "aqua.yaml"), `packages:
  - name: example/copier@v2.0.0
`)
	mustWrite(t, filepath.Join(f.root, "aqua-checksums.json"), `{
  "checksums": [
    {"id": "github.com/example/copier/releases/download/v2.0.0/copier.tar.gz", "algorithm": "sha256", "checksum": "feedface"}
  ]
}`)
	// Need to rebuild the runner because it cached the resolver/checker config at New.
	specs, err := spec.Discover(f.root, "**/lazygen.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(must(resolveraqua.New(f.root)))
	preflightReg := preflight.NewRegistry()
	preflightReg.Register(must(preflightaqua.New(f.root)))
	f.runs = runner.New(runner.Options{
		RepoRoot:  f.root,
		Specs:     specs,
		Storage:   gitfile.New(f.root),
		Resolvers: resolverReg,
		Preflight: preflightReg,
	})
	if err := f.runs.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.markerCount() != 2 {
		t.Errorf("aqua bump should re-run, marker=%d", f.markerCount())
	}
}

func TestRunner_OutputDriftInvalidates(t *testing.T) {
	f := newFixture(t)
	if err := f.runs.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Modify the recorded output → output-comparison fails → re-run.
	mustWrite(t, filepath.Join(f.root, f.specDir, "output.txt"), "tampered")
	if err := f.runs.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.markerCount() != 2 {
		t.Errorf("output drift should re-run, marker=%d", f.markerCount())
	}
}

func TestRunner_PreflightFailureAborts(t *testing.T) {
	f := newFixture(t)
	// Wipe checksums to simulate aqua install missing.
	if err := os.Remove(filepath.Join(f.root, "aqua-checksums.json")); err != nil {
		t.Fatal(err)
	}
	specs, err := spec.Discover(f.root, "**/lazygen.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(must(resolveraqua.New(f.root)))
	preflightReg := preflight.NewRegistry()
	preflightReg.Register(must(preflightaqua.New(f.root)))
	f.runs = runner.New(runner.Options{
		RepoRoot:  f.root,
		Specs:     specs,
		Storage:   gitfile.New(f.root),
		Resolvers: resolverReg,
		Preflight: preflightReg,
	})
	err = f.runs.Run(context.Background())
	if err == nil {
		t.Fatal("expected preflight error")
	}
	if f.markerCount() != 0 {
		t.Errorf("preflight failure must not run generators, marker=%d", f.markerCount())
	}
}

func TestRunner_AllowStaleDepsSkipsRecordWrite(t *testing.T) {
	f := newFixture(t)
	if err := os.Remove(filepath.Join(f.root, "aqua-checksums.json")); err != nil {
		t.Fatal(err)
	}
	specs, err := spec.Discover(f.root, "**/lazygen.yml")
	if err != nil {
		t.Fatal(err)
	}
	resolverReg := toolresolver.NewRegistry()
	resolverReg.Register(must(resolveraqua.New(f.root)))
	preflightReg := preflight.NewRegistry()
	preflightReg.Register(must(preflightaqua.New(f.root)))
	f.runs = runner.New(runner.Options{
		RepoRoot:  f.root,
		Specs:     specs,
		Storage:   gitfile.New(f.root),
		Resolvers: resolverReg,
		Preflight: preflightReg,
		ReadOnly:  true,
	})
	if err := f.runs.Run(context.Background()); err != nil {
		t.Fatalf("Run with ReadOnly should succeed: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(f.root, ".lazygen", "cache", "**", "*.yml"))
	if len(matches) != 0 {
		t.Errorf("ReadOnly mode must not write records, got %v", matches)
	}
	if f.markerCount() != 1 {
		t.Errorf("generator should still execute in ReadOnly mode, marker=%d", f.markerCount())
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
