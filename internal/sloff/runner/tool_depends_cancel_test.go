package runner_test

// Tests for ADR-0019 cancellation behaviour (F1 and F2 findings):
//
//	F1: deferToolResolution must NOT demote a tool on context.Canceled /
//	    context.DeadlineExceeded — the caller must propagate the context error.
//	    Run and Plan must both return a non-nil error.
//
//	F2: deferredTool.resolve must not latch context.Canceled as the definitive
//	    outcome (must not freeze the once) and must not wrap it in attributedErr
//	    (which appends "a task generating the tool's sources may be missing …").

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/preflight"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

// cancellingResolver is a pnpm-local stub whose Inputs method returns a
// configurable error on the first call and a different error on the second.
// It impersonates the pnpm-local channel so tool declarations in the fixture
// spec use the `pnpm-local` key.
type cancellingResolver struct {
	eagerErr error
	retryErr error
	calls    atomic.Int32
}

func (s *cancellingResolver) Name() string { return "pnpm-local" }

func (s *cancellingResolver) Inputs(_ context.Context, _ string, _ *toolresolver.DeclaredTool) ([]string, error) {
	n := s.calls.Add(1)
	if n == 1 {
		return nil, s.eagerErr
	}
	if s.retryErr != nil {
		return nil, s.retryErr
	}
	return nil, nil
}

func (s *cancellingResolver) Versions(_ context.Context, _ string, _ *toolresolver.DeclaredTool) ([]toolresolver.ResolvedVersion, error) {
	return []toolresolver.ResolvedVersion{{Name: "stub", Source: "stub:v1", Version: "stub:v1@1.0"}}, nil
}

// cancelTestSpec is the minimal spec used by F1/F2 tests: one pnpm-local tool
// with a depends declaration (enabling deferral) and a producer+consumer pair.
// The producer uses a script tool (versioner) so it passes the spec validation
// rule that every command must declare at least one tool.
const cancelTestSpec = `tools:
  versioner:
    exec: ["sh", "-c", "echo v1.0.0"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  my-codegen:
    pnpm-local: "@org/my-codegen"
    depends:
      - task: producer
commands:
  - name: producer
    cmd: ["sh", "-c", "cp input.txt out1.txt"]
    inputs: ["input.txt"]
    outputs: ["out1.txt"]
    tools: [versioner]
  - name: consumer
    cmd: ["sh", "-c", "cp input.txt out2.txt"]
    inputs: ["input.txt"]
    outputs: ["out2.txt"]
    tools: [my-codegen]
`

// TestRunner_ToolDepends_CancelNeverDefers (F1 regression): when the tool's
// eager Inputs call returns context.Canceled, deferToolResolution must NOT
// demote the tool. Run must return a non-nil error that satisfies
// errors.Is(err, context.Canceled), and must not log the "deferred until"
// warning.
func TestRunner_ToolDepends_CancelNeverDefers(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"input.txt": "hello\n",
		"sloff.yml": cancelTestSpec,
	})

	stub := &cancellingResolver{eagerErr: context.Canceled}
	reg := toolresolver.NewRegistry()
	reg.Register(script.New(workdir))
	reg.Register(stub)

	logs := &captureLogger{t: t}
	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: reg,
		Preflight: preflight.NewRegistry(),
		Logger:    logs,
	})

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run: expected non-nil error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run error must satisfy errors.Is(err, context.Canceled); got %v", err)
	}

	logs.mu.Lock()
	warns := append([]string(nil), logs.warns...)
	logs.mu.Unlock()
	for _, w := range warns {
		if strings.Contains(w, "deferred until") {
			t.Errorf("unexpected deferred warning on context cancellation: %q", w)
		}
	}
}

// TestRunner_ToolDepends_CancelNeverDefers_Plan (F1 Plan path): Plan must also
// return a non-nil error wrapping context.Canceled instead of succeeding with
// an empty contribution.
func TestRunner_ToolDepends_CancelNeverDefers_Plan(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"input.txt": "hello\n",
		"sloff.yml": cancelTestSpec,
	})

	stub := &cancellingResolver{eagerErr: context.Canceled}
	reg := toolresolver.NewRegistry()
	reg.Register(script.New(workdir))
	reg.Register(stub)

	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: reg,
		Preflight: preflight.NewRegistry(),
	})

	_, _, err := r.Plan(context.Background())
	if err == nil {
		t.Fatal("Plan: expected non-nil error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Plan error must satisfy errors.Is(err, context.Canceled); got %v", err)
	}
}

// TestRunner_ToolDepends_DeferredRetryContextCancel (F2 regression): when
// eager Inputs fails with a real error (→ tool is deferred) and the retry at
// ensureToolsResolved returns context.Canceled:
//   - the consumer error must satisfy errors.Is(err, context.Canceled)
//   - the error must NOT contain the attribution hint "a task generating the
//     tool's sources may be missing from the tool's depends"
func TestRunner_ToolDepends_DeferredRetryContextCancel(t *testing.T) {
	workdir, specs := setupProviderWorkdir(t, map[string]string{
		"input.txt": "hello\n",
		"sloff.yml": cancelTestSpec,
	})

	// Eager call returns a real error → deferral. Retry returns context.Canceled.
	stub := &cancellingResolver{
		eagerErr: errors.New("tool source not compiled yet"),
		retryErr: context.Canceled,
	}
	reg := toolresolver.NewRegistry()
	reg.Register(script.New(workdir))
	reg.Register(stub)

	logs := &captureLogger{t: t}
	r := runner.New(runner.Options{
		RepoRoot:  workdir,
		Specs:     specs,
		Storage:   local.New(workdir, local.WithClock(func() time.Time { return fixedClock })),
		Resolvers: reg,
		Preflight: preflight.NewRegistry(),
		Logger:    logs,
	})

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run: expected non-nil error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run error must satisfy errors.Is(err, context.Canceled); got %v", err)
	}
	if strings.Contains(err.Error(), "a task generating the tool's sources may be missing") {
		t.Errorf("context cancellation must not produce the spec-hint attribution; got: %v", err)
	}
}
