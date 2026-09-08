package spec_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/spec"
)

// benchSinkSpecs keeps every measured result observable so the compiler
// cannot elide the discovery work under test.
var benchSinkSpecs []spec.Spec

const (
	benchTopDirs     = 16 // enough top-level fan-out for the concurrent walk (PR #54) to matter
	benchDirsPerTop  = 40
	benchFilesPerDir = 15
	benchSpecsPerTop = 2
)

// benchSpecYAML is a minimal valid sloff.yml. Discover parses every match, so
// fixture specs must pass full structural validation, not just exist.
const benchSpecYAML = `tools:
  vers:
    exec: ["echo", "v1.0.0"]
commands:
  - name: gen
    cmd: ["sh", "-c", "true"]
    inputs: ["in.txt"]
    outputs: ["out.txt"]
    tools: [vers]
`

type specBenchTree struct {
	root      string
	wantSpecs int
}

// specBenchFixture builds the monorepo-shaped tree once per process (shared
// across -count repetitions) so the ~13k-file setup never leaks into a timed
// region: 16 top-level trees of ~600 files each with 2 sparse sloff.yml specs
// per tree, plus node_modules decoys (a large top-level one and a nested one)
// that also carry sloff.yml files. If the pruning from PR #17 regresses, the
// walk pays for the decoy subtrees AND the spec count changes, so the
// validation below fails loudly instead of the benchmark quietly timing the
// wrong tree.
var specBenchFixture = sync.OnceValues(func() (*specBenchTree, error) {
	root, err := os.MkdirTemp("", "sloff-spec-bench-*")
	if err != nil {
		return nil, err
	}
	payload := []byte("bench payload\n")
	for t := range benchTopDirs {
		top := filepath.Join(root, fmt.Sprintf("top-%02d", t))
		for d := range benchDirsPerTop {
			dir := filepath.Join(top, fmt.Sprintf("dir-%02d", d))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
			for f := range benchFilesPerDir {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%02d.txt", f)), payload, 0o644); err != nil {
					return nil, err
				}
			}
		}
		// Sparse specs, like a real monorepo: a couple per top-level tree.
		for _, d := range []int{0, benchDirsPerTop / 2} {
			specPath := filepath.Join(top, fmt.Sprintf("dir-%02d", d), "sloff.yml")
			if err := os.WriteFile(specPath, []byte(benchSpecYAML), 0o644); err != nil {
				return nil, err
			}
		}
	}
	// Decoys: a heavy top-level node_modules (pruned by Discover's root loop)
	// and a nested one (pruned inside walkSpecs). Both must stay invisible.
	if err := benchWriteNodeModules(filepath.Join(root, "node_modules"), 100, 20); err != nil {
		return nil, err
	}
	if err := benchWriteNodeModules(filepath.Join(root, "top-03", "dir-05", "node_modules"), 25, 20); err != nil {
		return nil, err
	}
	return &specBenchTree{root: root, wantSpecs: benchTopDirs * benchSpecsPerTop}, nil
})

// benchWriteNodeModules fabricates a node_modules subtree of pkgs packages
// with filesPerPkg files each, plus a valid sloff.yml per package — a tripwire
// that inflates the discovered spec count if pruning ever stops skipping it.
func benchWriteNodeModules(base string, pkgs, filesPerPkg int) error {
	payload := []byte("module.exports = {};\n")
	for p := range pkgs {
		dir := filepath.Join(base, fmt.Sprintf("pkg-%03d", p), "lib")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for f := range filesPerPkg {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("mod-%02d.js", f)), payload, 0o644); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(base, fmt.Sprintf("pkg-%03d", p), "sloff.yml"), []byte(benchSpecYAML), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// BenchmarkDiscover times the full spec discovery walk (match + parse) over a
// monorepo-shaped tree. It guards two past optimisations at once: the
// concurrent per-top-level-child walk (PR #54 — serialising it regresses this
// number) and node_modules/.git pruning (PR #17 — without it the walk pays for
// thousands of decoy files and the spec-count validation fails).
func BenchmarkDiscover(b *testing.B) {
	fx, err := specBenchFixture()
	if err != nil {
		b.Fatal(err)
	}

	// Validate once, outside the timed region: the exact expected spec count.
	// Finding fewer means the fixture is broken (a benchmark over 0 specs
	// would lie); finding more means pruning leaked the node_modules tripwires.
	specs, err := spec.Discover(fx.root, "**/sloff.yml")
	if err != nil {
		b.Fatal(err)
	}
	if len(specs) != fx.wantSpecs {
		b.Fatalf("Discover found %d specs, want %d (fixture or pruning is broken)", len(specs), fx.wantSpecs)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := spec.Discover(fx.root, "**/sloff.yml")
		if err != nil {
			b.Fatal(err)
		}
		benchSinkSpecs = out
	}
}
