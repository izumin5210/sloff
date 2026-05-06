package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/izumin5210/lazygen/internal/lazygen/cache/local"
	"github.com/izumin5210/lazygen/internal/lazygen/preflight"
	"github.com/izumin5210/lazygen/internal/lazygen/runner"
	"github.com/izumin5210/lazygen/internal/lazygen/spec"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/script"
)

const allowStaleDepsEnv = "LAZYGEN_ALLOW_STALE_DEPS"

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
	cmd.Flags().StringVar(&root, "root", ".", "Repository root containing .lazygen/cache and lockfiles")
	cmd.Flags().StringVar(&pattern, "pattern", "**/lazygen.yml", "Glob pattern (relative to --root) used to discover specs")
	return cmd
}

func runE(ctx context.Context, rawRoot, pattern string) error {
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

	readOnly := os.Getenv(allowStaleDepsEnv) != ""

	r := runner.New(runner.Options{
		RepoRoot:  root,
		Specs:     specs,
		Storage:   local.New(root),
		Resolvers: buildResolvers(root),
		Preflight: preflight.NewRegistry(), // no concrete checkers in this build
		ReadOnly:  readOnly,
	})

	return r.Run(ctx)
}

// buildResolvers wires up the resolver registry. PR1 ships only the script resolver,
// which is universal (any prebuilt binary that has --version). Future PRs will register
// pnpm-external / go-local / pnpm-local / buf alongside it.
func buildResolvers(root string) *toolresolver.Registry {
	reg := toolresolver.NewRegistry()
	reg.Register(script.New(root))
	reg.SetFallback(func(cmd []string) {
		fmt.Fprintf(os.Stderr, "lazygen: no resolver matched cmd %v; tools_hash falls back to cmd-string only\n", cmd)
	})
	return reg
}
