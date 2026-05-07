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
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/golocal"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/pnpmlocal"
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

	resolvers, err := buildResolvers(root)
	if err != nil {
		return err
	}

	r := runner.New(runner.Options{
		RepoRoot:  root,
		Specs:     specs,
		Storage:   local.New(root),
		Resolvers: resolvers,
		Preflight: preflight.NewRegistry(), // no concrete checkers (pnpm-local rebuild detection now flows through depgraph)
		ReadOnly:  readOnly,
	})

	return r.Run(ctx)
}

// buildResolvers wires up the resolver registry. Per ADR-0005 every resolver
// is declared-only: the script resolver runs for `tools: [{exec: [...]}]`
// entries, the go-local resolver runs for `tools: [{go-local: ./cmd/foo}]`,
// and the pnpm-local resolver runs for `tools: [{pnpm-local: '@org/pkg'}]`.
// Both source listers are memoised so repeated tasks against the same entry
// only pay packages.Load / esbuild.Build once per run.
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
