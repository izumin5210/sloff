package benchgen_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/izumin5210/sloff/internal/sloff/benchgen"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

// smallParams keeps the shape non-degenerate (wide fan + chains + sink) while
// staying fast enough for the -race test job.
func smallParams() benchgen.Params {
	return benchgen.Params{
		Seed:          42,
		WideTasks:     4,
		Chains:        2,
		ChainDepth:    3,
		FilesPerTask:  3,
		FileSizeBytes: 128,
	}
}

func TestGenerate_Shape(t *testing.T) {
	p := smallParams()
	root := t.TempDir()
	repo, err := benchgen.Generate(root, p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	wantTasks := p.WideTasks + p.Chains*p.ChainDepth + 1 // + sink
	if repo.TaskCount != wantTasks {
		t.Errorf("TaskCount = %d, want %d", repo.TaskCount, wantTasks)
	}
	wantSrc := (p.WideTasks + p.Chains*p.ChainDepth) * p.FilesPerTask
	if repo.SourceFileCount != wantSrc {
		t.Errorf("SourceFileCount = %d, want %d", repo.SourceFileCount, wantSrc)
	}

	// The reported counts must match what is actually on disk; a generator
	// that misreports its own shape would make every macro-benchmark lie.
	var gotSpecs, gotSrc int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.Name() == "sloff.yml":
			gotSpecs++
		case strings.HasSuffix(d.Name(), ".src"):
			gotSrc++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if gotSpecs != repo.SpecFileCount {
		t.Errorf("sloff.yml files on disk = %d, reported %d", gotSpecs, repo.SpecFileCount)
	}
	if gotSrc != repo.SourceFileCount {
		t.Errorf(".src files on disk = %d, reported %d", gotSrc, repo.SourceFileCount)
	}
	if len(repo.OutputPaths) != wantTasks {
		t.Errorf("OutputPaths = %d entries, want %d (one per task)", len(repo.OutputPaths), wantTasks)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(repo.MutableInput))); err != nil {
		t.Errorf("MutableInput %q missing on disk: %v", repo.MutableInput, err)
	}

	specs, err := spec.Discover(root, "**/sloff.yml")
	if err != nil {
		t.Fatalf("Discover over generated repo: %v", err)
	}
	if len(specs) != repo.SpecFileCount {
		t.Errorf("Discover found %d specs, want %d", len(specs), repo.SpecFileCount)
	}
}

func TestGenerate_Deterministic(t *testing.T) {
	p := smallParams()
	a := t.TempDir()
	b := t.TempDir()
	if _, err := benchgen.Generate(a, p); err != nil {
		t.Fatalf("Generate a: %v", err)
	}
	if _, err := benchgen.Generate(b, p); err != nil {
		t.Fatalf("Generate b: %v", err)
	}
	if ha, hb := treeDigest(t, a), treeDigest(t, b); ha != hb {
		t.Errorf("same params produced different trees: %s vs %s", ha, hb)
	}

	p2 := p
	p2.Seed = 43
	c := t.TempDir()
	if _, err := benchgen.Generate(c, p2); err != nil {
		t.Fatalf("Generate c: %v", err)
	}
	if ha, hc := treeDigest(t, a), treeDigest(t, c); ha == hc {
		t.Errorf("different seeds produced identical trees (%s)", ha)
	}
}

func TestGenerate_RejectsDegenerateParams(t *testing.T) {
	p := smallParams()
	p.ChainDepth = 1 // no chain edge left — the "deep chain" scenario would silently vanish
	if _, err := benchgen.Generate(t.TempDir(), p); err == nil {
		t.Fatal("Generate accepted ChainDepth=1")
	}
}

// TestGenerate_RepoRunsGreen drives a real cold run and a real warm run over
// a small generated repo. This is the guard that the generated shape is a
// valid sloff monorepo end to end: specs parse, depends validate, every task
// executes cold, and every task hits warm.
func TestGenerate_RepoRunsGreen(t *testing.T) {
	p := smallParams()
	root := t.TempDir()
	repo, err := benchgen.Generate(root, p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	run := func() *countingLogger {
		t.Helper()
		specs, err := spec.Discover(root, "**/sloff.yml")
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		reg := toolresolver.NewRegistry()
		reg.Register(script.New(root))
		logs := &countingLogger{}
		r := runner.New(runner.Options{
			RepoRoot:  root,
			Specs:     specs,
			Storage:   local.New(root, local.WithClock(func() time.Time { return time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC) })),
			Resolvers: reg,
			Preflight: preflight.NewRegistry(),
			Logger:    logs,
			Stdout:    io.Discard,
			Stderr:    io.Discard,
		})
		if err := r.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return logs
	}

	// Bootstrap from the clean tree. Cross-task inputs (`../d0/out.gen`)
	// don't exist yet at glob-expansion time, so downstream records are keyed
	// off the incomplete input set — sloff's model assumes outputs are
	// git-managed and present at run start. The benchmark scenarios therefore
	// treat "cold" as "outputs present, fingerprints absent" (fresh clone),
	// which this bootstrap establishes.
	bootstrap := run()
	if bootstrap.runs != repo.TaskCount || bootstrap.skips != 0 {
		t.Errorf("bootstrap run: RUN=%d SKIP=%d, want RUN=%d SKIP=0", bootstrap.runs, bootstrap.skips, repo.TaskCount)
	}
	for _, out := range repo.OutputPaths {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(out))); err != nil {
			t.Errorf("output %q missing after bootstrap run: %v", out, err)
		}
	}

	// Fresh-clone cold: outputs on disk, no fingerprints.
	if err := os.RemoveAll(filepath.Join(root, ".sloff")); err != nil {
		t.Fatalf("wipe fingerprints: %v", err)
	}
	cold := run()
	if cold.runs != repo.TaskCount || cold.skips != 0 {
		t.Errorf("cold run: RUN=%d SKIP=%d, want RUN=%d SKIP=0", cold.runs, cold.skips, repo.TaskCount)
	}

	warm := run()
	if warm.skips != repo.TaskCount || warm.runs != 0 {
		t.Errorf("warm run: RUN=%d SKIP=%d, want RUN=0 SKIP=%d", warm.runs, warm.skips, repo.TaskCount)
	}

	// Rewriting the designated mutable input must invalidate exactly one
	// chain (head, its downstream via output propagation, and the sink) and
	// nothing else.
	mut := filepath.Join(root, filepath.FromSlash(repo.MutableInput))
	if err := os.WriteFile(mut, []byte("mutated content\n"), 0o644); err != nil {
		t.Fatalf("mutate input: %v", err)
	}
	wantRuns := p.ChainDepth + 1 // whole mutated chain + sink
	incr := run()
	if incr.runs != wantRuns || incr.skips != repo.TaskCount-wantRuns {
		t.Errorf("incremental run: RUN=%d SKIP=%d, want RUN=%d SKIP=%d",
			incr.runs, incr.skips, wantRuns, repo.TaskCount-wantRuns)
	}
}

type countingLogger struct {
	mu    sync.Mutex
	runs  int
	skips int
}

func (l *countingLogger) Infof(format string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case strings.HasPrefix(format, "RUN"):
		l.runs++
	case strings.HasPrefix(format, "SKIP"):
		l.skips++
	}
}

func (l *countingLogger) Warnf(string, ...any)  {}
func (l *countingLogger) Errorf(string, ...any) {}

func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%x\x00", filepath.ToSlash(rel), sha256.Sum256(b))
		return nil
	})
	if err != nil {
		t.Fatalf("treeDigest: %v", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
