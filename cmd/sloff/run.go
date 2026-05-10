package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
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

const allowStaleDepsEnv = "SLOFF_ALLOW_STALE_DEPS"

func newRunCmd() *cobra.Command {
	var (
		root    string
		pattern string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Discover specs and execute every task with fingerprint-aware orchestration",
		RunE: func(cobraCmd *cobra.Command, _ []string) error {
			return runE(cobraCmd.Context(), root, pattern)
		},
	}
	cmd.Flags().StringVar(&root, "root", ".", "Repository root containing .sloff/fingerprints and lockfiles")
	cmd.Flags().StringVar(&pattern, "pattern", "**/sloff.yml", "Glob pattern (relative to --root) used to discover specs")
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
		Preflight: buildPreflight(root),
		ReadOnly:  readOnly,
	})

	return r.Run(ctx)
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
