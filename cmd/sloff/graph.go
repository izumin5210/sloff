package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/izumin5210/sloff/internal/sloff/cache/local"
	"github.com/izumin5210/sloff/internal/sloff/explain"
	"github.com/izumin5210/sloff/internal/sloff/runner"
	"github.com/izumin5210/sloff/internal/sloff/spec"
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
		Short: "Render the auto-detected task DAG (Mermaid or DOT)",
		Long: `graph emits the inputs/outputs-derived dependency DAG for every
discovered sloff.yml. Each edge is annotated with a sample of the
files in the producer's outputs ∩ consumer's inputs intersection,
so "why does B depend on A?" can be answered without reading every
spec.

Preflight checks are skipped on purpose: the subcommand is also
useful when the install state is drifted, since drift is one of
the things users reach for the graph to investigate.`,
		RunE: func(cobraCmd *cobra.Command, _ []string) error {
			return graphE(cobraCmd.Context(), cobraCmd.OutOrStdout(), root, pattern, format)
		},
	}
	cmd.Flags().StringVar(&root, "root", ".", "Repository root containing sloff.yml specs")
	cmd.Flags().StringVar(&pattern, "pattern", "**/sloff.yml", "Glob pattern (relative to --root) used to discover specs")
	cmd.Flags().StringVar(&format, "format", graphFormatMermaid,
		fmt.Sprintf("Output format: %s | %s", graphFormatMermaid, graphFormatDOT))
	return cmd
}

func graphE(ctx context.Context, out io.Writer, rawRoot, pattern, format string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	root, err := filepath.Abs(rawRoot)
	if err != nil {
		return fmt.Errorf("resolve --root: %w", err)
	}

	specs, err := spec.Discover(root, pattern)
	if err != nil {
		return fmt.Errorf("discover specs: %w", err)
	}

	resolvers, err := buildResolvers(root)
	if err != nil {
		return err
	}

	// Storage is wired only because runner.New keeps it on the Options struct;
	// Plan never touches it. Preflight is left nil so install drift never
	// blocks rendering — graph is the tool users reach for *when* the build
	// is broken.
	r := runner.New(runner.Options{
		RepoRoot:  root,
		Specs:     specs,
		Storage:   local.New(root),
		Resolvers: resolvers,
	})

	tasks, err := r.Plan(ctx)
	if err != nil {
		return err
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
