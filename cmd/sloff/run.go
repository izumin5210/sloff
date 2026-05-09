package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

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

// otelShutdownTimeout caps the BatchSpanProcessor flush at process exit. CLI
// runs are short, so a hard ceiling on the drain prevents a slow / unreachable
// collector from masking the actual command's outcome.
const otelShutdownTimeout = 5 * time.Second

const allowStaleDepsEnv = "SLOFF_ALLOW_STALE_DEPS"

func newRunCmd() *cobra.Command {
	var (
		root    string
		pattern string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Discover specs and execute every task with cache-aware orchestration",
		RunE: func(cobraCmd *cobra.Command, _ []string) error {
			return runE(cobraCmd.Context(), root, pattern)
		},
	}
	cmd.Flags().StringVar(&root, "root", ".", "Repository root containing .sloff/cache and lockfiles")
	cmd.Flags().StringVar(&pattern, "pattern", "**/sloff.yml", "Glob pattern (relative to --root) used to discover specs")
	return cmd
}

func runE(ctx context.Context, rawRoot, pattern string) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	tp, shutdown, err := setupTracing(ctx)
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
	}
	defer flushTracing(shutdown)

	tracer := tp.Tracer(cmdTracerName)
	ctx, span := tracer.Start(ctx, "sloff.run", trace.WithAttributes(
		attribute.String("sloff.subcommand", "run"),
		attribute.String("sloff.spec.pattern", pattern),
	))
	defer endSpan(span, &err)

	root, err := filepath.Abs(rawRoot)
	if err != nil {
		return fmt.Errorf("resolve --root: %w", err)
	}
	span.SetAttributes(attribute.String("sloff.repo_root", root))

	specs, err := discoverSpecs(ctx, tracer, root, pattern)
	if err != nil {
		return err
	}
	span.SetAttributes(attribute.Int("sloff.spec.count", len(specs)))

	readOnly := os.Getenv(allowStaleDepsEnv) != ""
	span.SetAttributes(attribute.Bool("sloff.read_only", readOnly))

	resolvers, err := buildResolvers(root)
	if err != nil {
		return err
	}

	r := runner.New(runner.Options{
		RepoRoot:       root,
		Specs:          specs,
		Storage:        local.New(root),
		Resolvers:      resolvers,
		Preflight:      buildPreflight(root),
		ReadOnly:       readOnly,
		TracerProvider: tp,
	})

	return r.Run(ctx)
}

// discoverSpecs wraps spec.Discover with a span. spec.Discover doesn't take a
// context (its work is local file I/O), so the span purely captures timing and
// the resolved spec count for the trace tree. The tracer is passed in (rather
// than read from a package var) so cmd/sloff stays free of any global OTel
// state and concurrent invocations don't share Tracer instances.
func discoverSpecs(ctx context.Context, tracer trace.Tracer, root, pattern string) (specs []spec.Spec, err error) {
	_, span := tracer.Start(ctx, "spec.discover", trace.WithAttributes(
		attribute.String("sloff.spec.pattern", pattern),
	))
	defer endSpan(span, &err)

	specs, err = spec.Discover(root, pattern)
	if err != nil {
		return nil, fmt.Errorf("discover specs: %w", err)
	}
	span.SetAttributes(attribute.Int("sloff.spec.count", len(specs)))
	return specs, nil
}

// flushTracing drains the BatchSpanProcessor at process exit. Uses a fresh
// background context so the drain still runs after the parent context is
// canceled (Ctrl-C). Shutdown errors are logged but never escalated — exporter
// faults must not mask the command's own outcome.
func flushTracing(shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), otelShutdownTimeout)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "sloff: otel shutdown: %v\n", err)
	}
}

// buildResolvers wires up the resolver registry. Per ADR-0005 every resolver
// is declared-only: the script resolver runs for `tools: [{exec: [...]}]`
// entries, the go-local resolver runs for `tools: [{go-local: ./cmd/foo}]`,
// and the pnpm-local resolver runs for `tools: [{pnpm-local: '@org/pkg'}]`.
// Both source listers are memoised so repeated tasks against the same entry
// only pay packages.Load / git ls-files once per run.
func buildResolvers(root string) (*toolresolver.Registry, error) {
	reg := toolresolver.NewRegistry()
	reg.Register(script.New(root))
	reg.Register(golocal.New(root, lister.NewMemoized(lister.NewGoPackages(root))))
	pnpmRes, err := pnpmlocal.New(root, pnpmlocal.GitLsFiles)
	if err != nil {
		return nil, fmt.Errorf("build pnpm-local resolver: %w", err)
	}
	reg.Register(pnpmRes)
	return reg, nil
}

// buildPreflight wires up the preflight checkers. The runner scopes them to
// resolvers some command actually references, so registering a checker here
// is harmless for repos that don't use the corresponding resolver. Only
// channels that need pre-run state validation register a checker — script
// and go-local don't.
func buildPreflight(root string) *preflight.Registry {
	reg := preflight.NewRegistry()
	reg.Register(preflightpnpm.New(root))
	return reg
}
