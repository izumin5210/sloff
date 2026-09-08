package runner_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
)

// TestResolve_EagerResolutionOverlapsPrewarm guards the PR #57 latency win:
// the batch prewarm (go-local `packages.Load`) runs in the background while
// the eager channels (script / pnpm-local) resolve concurrently, instead of
// prewarm completing before the fan-out starts. A wall-clock benchmark can't
// see this reliably, so the guard asserts the concurrency structure itself
// with handshaking fakes:
//
//   - the prewarmed resolver's Prewarm waits (bounded) for an eager Versions
//     call to start — observing one proves the fan-out overlapped the
//     in-flight prewarm. If the runner regresses to serialising prewarm
//     before the fan-out, the wait times out and the assertion fails.
//   - the prewarmed resolver's own Inputs/Versions must only fire after
//     Prewarm finished (the #53/#57 gating that makes per-tool go-local
//     resolution a cache hit).
func TestResolve_EagerResolutionOverlapsPrewarm(t *testing.T) {
	root := t.TempDir()
	specYAML := `tools:
  gotool:
    go-local: ./cmd/gotool
  scripttool:
    exec: ["true"]

commands:
  - name: gen
    cmd: ["sh", "-c", "echo x > out.txt"]
    inputs: ["in.txt"]
    outputs: ["out.txt"]
    tools: [gotool, scripttool]
`
	if err := os.WriteFile(filepath.Join(root, "sloff.yml"), []byte(specYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "in.txt"), []byte("in\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	probe := &overlapProbe{eagerStarted: make(chan struct{}), prewarmDone: make(chan struct{})}
	reg := toolresolver.NewRegistry()
	reg.Register(&overlapEagerResolver{name: "script", probe: probe})
	reg.Register(&overlapPrewarmResolver{name: "go-local", probe: probe})

	specs, err := spec.Discover(root, "**/sloff.yml")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	r := runner.New(runner.Options{
		RepoRoot:  root,
		Specs:     specs,
		Storage:   local.New(root),
		Resolvers: reg,
		Preflight: preflight.NewRegistry(),
		Logger:    &captureLogger{t: t},
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !probe.sawOverlap.Load() {
		t.Error("prewarm never observed an eager resolver call while in flight; " +
			"the PR #57 overlap (eager channels resolving concurrently with the background prewarm) appears to be regressed")
	}
	if probe.gatingViolated.Load() {
		t.Error("the prewarmed channel resolved before Prewarm completed; " +
			"gated tools must wait for the prewarm so their per-tool resolution hits the warmed cache (#53/#57)")
	}
}

// overlapTimeout bounds the handshake waits so a regression fails the test
// instead of deadlocking it. Generous because CI runners stall; irrelevant to
// the healthy path, which completes the handshake in microseconds.
const overlapTimeout = 10 * time.Second

type overlapProbe struct {
	eagerOnce    sync.Once
	eagerStarted chan struct{}
	prewarmDone  chan struct{}

	sawOverlap     atomic.Bool
	gatingViolated atomic.Bool
}

// overlapEagerResolver plays the script channel: no Prewarmer implementation,
// so the runner resolves it eagerly, concurrently with the prewarm.
type overlapEagerResolver struct {
	name  string
	probe *overlapProbe
}

func (r *overlapEagerResolver) Name() string { return r.name }

func (r *overlapEagerResolver) Inputs(context.Context, string, *toolresolver.DeclaredTool) ([]string, error) {
	return nil, nil
}

func (r *overlapEagerResolver) Versions(_ context.Context, _ string, declared *toolresolver.DeclaredTool) ([]toolresolver.ResolvedVersion, error) {
	r.probe.eagerOnce.Do(func() { close(r.probe.eagerStarted) })
	return []toolresolver.ResolvedVersion{{Name: "scripttool", Source: "fake", Version: "v1"}}, nil
}

// overlapPrewarmResolver plays the go-local channel: implementing Prewarmer
// puts it in the gated set.
type overlapPrewarmResolver struct {
	name  string
	probe *overlapProbe
}

func (r *overlapPrewarmResolver) Name() string { return r.name }

func (r *overlapPrewarmResolver) Prewarm(context.Context, []toolresolver.PrewarmRequest) error {
	select {
	case <-r.probe.eagerStarted:
		r.probe.sawOverlap.Store(true)
	case <-time.After(overlapTimeout):
	}
	close(r.probe.prewarmDone)
	return nil
}

func (r *overlapPrewarmResolver) checkGated() {
	select {
	case <-r.probe.prewarmDone:
	default:
		r.probe.gatingViolated.Store(true)
	}
}

func (r *overlapPrewarmResolver) Inputs(context.Context, string, *toolresolver.DeclaredTool) ([]string, error) {
	r.checkGated()
	return nil, nil
}

func (r *overlapPrewarmResolver) Versions(context.Context, string, *toolresolver.DeclaredTool) ([]toolresolver.ResolvedVersion, error) {
	r.checkGated()
	return []toolresolver.ResolvedVersion{{Name: "gotool", Source: "fake", Version: "src:v1"}}, nil
}
