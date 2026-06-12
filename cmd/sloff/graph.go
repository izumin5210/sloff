package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/izumin5210/sloff/internal/sloff/depgraph"
	"github.com/izumin5210/sloff/internal/sloff/explain"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
	"github.com/izumin5210/sloff/internal/sloff/runner"
)

const (
	graphFormatMermaid = "mermaid"
	graphFormatDOT     = "dot"
)

func newGraphCmd() *cobra.Command {
	var (
		root    string
		pattern string
		format  string
	)
	cmd := &cobra.Command{
		Use:   "graph",
		Short: "Render the declared task DAG (Mermaid or DOT)",
		Long: `graph emits the dependency DAG declared via each task's depends
entries for every discovered sloff.yml. Each edge is annotated with a
sample of the files in the producer's outputs ∩ consumer's inputs
intersection when those files exist, so "why does B depend on A?" can
be answered without reading every spec; edges whose evidence is not
observable in the current tree are captioned "(declared)".

The subcommand is meant to remain useful in broken environments:
preflight (install drift) and resolver Versions (e.g. <bin> --version
for the script channel) are both skipped, since their failures don't
affect the graph and drift / missing binaries are exactly what the
user is trying to debug. Resolver Inputs are still resolved — failures
there mean the graph would be missing overlap evidence, so they fail
loud.`,
		RunE: func(cobraCmd *cobra.Command, _ []string) error {
			return graphE(cobraCmd.Context(), cobraCmd.OutOrStdout(), cobraCmd.ErrOrStderr(), root, pattern, format)
		},
	}
	cmd.Flags().StringVar(&root, "root", ".", "Repository root containing sloff.yml specs")
	cmd.Flags().StringVar(&pattern, "pattern", "**/sloff.yml", "Glob pattern (relative to --root) used to discover specs")
	cmd.Flags().StringVar(&format, "format", graphFormatMermaid,
		fmt.Sprintf("Output format: %s | %s", graphFormatMermaid, graphFormatDOT))
	return cmd
}

func graphE(ctx context.Context, out, errOut io.Writer, rawRoot, pattern, format string) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	tp, shutdown, err := setupTracing(ctx)
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
	}
	defer flushTracing(shutdown)

	tracer := tp.Tracer(cmdTracerName)
	ctx, span := tracer.Start(ctx, "sloff.graph", trace.WithAttributes(
		attribute.String("sloff.subcommand", "graph"),
		attribute.String("sloff.spec.pattern", pattern),
		attribute.String("sloff.graph.format", format),
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

	resolvers, err := buildResolvers(root)
	if err != nil {
		return err
	}

	// Plan never reads Storage, but runner.Options.Storage is a non-pointer
	// interface field, so something has to live there. Pin local.New(root)
	// as a cheap no-op stub instead of going through loadStorage; that way
	// graph keeps rendering even when an opt-in remote backend is selected
	// but its config / network is misconfigured. Preflight is left nil for
	// the same reason — install drift should not block diagnosis.
	// TODO(DEV-21): drop this stub once Plan no longer requires a Storage.
	r := runner.New(runner.Options{
		RepoRoot:       root,
		Specs:          specs,
		Storage:        local.New(root),
		Resolvers:      resolvers,
		TracerProvider: tp,
	})

	tasks, missing, err := r.Plan(ctx)
	if err != nil {
		return err
	}
	// ADR-0013 D3: graph downgrades the depends-missing check to a warning so
	// the DAG stays inspectable while the user fixes the spec.
	for _, m := range missing {
		fmt.Fprintf(errOut, "warning: %s\n", depgraph.FormatMissing(m))
	}
	edges := explain.Edges(tasks)

	var rendered string
	switch format {
	case graphFormatMermaid:
		rendered = explain.RenderMermaid(tasks, edges)
	case graphFormatDOT:
		rendered = explain.RenderDOT(tasks, edges)
	default:
		return fmt.Errorf("--format must be %s or %s, got %q", graphFormatMermaid, graphFormatDOT, format)
	}

	if _, err := io.WriteString(out, rendered); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}
