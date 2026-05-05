package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/izumin5210/lazygen/internal/lazygen/cache/gitfile"
	"github.com/izumin5210/lazygen/internal/lazygen/preflight"
	preflightaqua "github.com/izumin5210/lazygen/internal/lazygen/preflight/aqua"
	"github.com/izumin5210/lazygen/internal/lazygen/runner"
	"github.com/izumin5210/lazygen/internal/lazygen/spec"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	resolveraqua "github.com/izumin5210/lazygen/internal/lazygen/toolresolver/aqua"
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

	resolverReg, preflightReg, err := buildRegistries(root)
	if err != nil {
		return err
	}

	readOnly := os.Getenv(allowStaleDepsEnv) != ""

	r := runner.New(runner.Options{
		RepoRoot:  root,
		Specs:     specs,
		Storage:   gitfile.New(root),
		Resolvers: resolverReg,
		Preflight: preflightReg,
		ReadOnly:  readOnly,
	})

	return r.Run(ctx)
}

func buildRegistries(root string) (*toolresolver.Registry, *preflight.Registry, error) {
	resolverReg := toolresolver.NewRegistry()
	preflightReg := preflight.NewRegistry()

	aquaPath := filepath.Join(root, resolveraqua.ConfigFileName)
	if _, err := os.Stat(aquaPath); err == nil {
		res, err := resolveraqua.New(root)
		if err != nil {
			return nil, nil, fmt.Errorf("load aqua resolver: %w", err)
		}
		resolverReg.Register(res)

		check, err := preflightaqua.New(root)
		if err != nil {
			return nil, nil, fmt.Errorf("load aqua checker: %w", err)
		}
		preflightReg.Register(check)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("stat aqua.yaml: %w", err)
	}

	resolverReg.SetFallback(func(cmd []string) {
		fmt.Fprintf(os.Stderr, "lazygen: no resolver matched cmd %v; tools_hash falls back to cmd-string only\n", cmd)
	})

	return resolverReg, preflightReg, nil
}
