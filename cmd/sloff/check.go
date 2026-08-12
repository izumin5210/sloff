package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	"github.com/izumin5210/sloff/internal/sloff/runner"
)

// Exit codes of `sloff check` (ADR-0021). Drift and infrastructure problems
// need distinct codes so CI can tell "ask the author to regenerate and
// commit" apart from "the check itself could not run".
const (
	checkExitDrift = 1
	checkExitError = 2
)

// checkIssueListCap bounds how many per-file issues one task prints; large
// generated trees can drift by thousands of files and the first few name the
// problem well enough.
const checkIssueListCap = 20

func newCheckCmd() *cobra.Command {
	var (
		root    string
		pattern string
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Verify that generated outputs and fingerprint records are committed and up to date, without running any generator",
		Long: `Verify that the working tree matches its committed fingerprint records
without executing any generator. Every task's fingerprint hit decision is
evaluated read-only; a task that would have to run is reported as drift.

Exit codes: 0 = up to date, 1 = drift detected, 2 = the check could not run
(spec, tool, or environment problems).`,
		RunE: func(cobraCmd *cobra.Command, _ []string) error {
			return checkE(cobraCmd.Context(), cobraCmd.OutOrStdout(), cobraCmd.ErrOrStderr(), root, pattern)
		},
	}
	cmd.Flags().StringVar(&root, "root", ".", "Repository root containing .sloff/fingerprints and lockfiles")
	cmd.Flags().StringVar(&pattern, "pattern", "**/sloff.yml", "Glob pattern (relative to --root) used to discover specs")
	return cmd
}

// checkE owns its output completely: every failure is printed to errOut here
// and converted to an exitCodeError, so main never double-prints and the
// 0/1/2 contract holds for everything past flag parsing.
func checkE(ctx context.Context, out, errOut io.Writer, rawRoot, pattern string) error {
	rep, recordsInRepo, err := runCheck(ctx, errOut, rawRoot, pattern)
	if rep != nil {
		printCheckReport(out, rep, recordsInRepo)
	}
	if err != nil {
		fmt.Fprintf(errOut, "sloff check: %v\n", err)
		return &exitCodeError{code: checkExitError}
	}
	if drifted := rep.Drift(); len(drifted) > 0 {
		fmt.Fprintln(out, checkRemediation(recordsInRepo))
		return &exitCodeError{code: checkExitDrift}
	}
	return nil
}

// checkRemediation is the fix-it guidance for a drifted check. With the
// local backend the records live in the repo and must be committed alongside
// the outputs; remote backends receive the records from `sloff run` directly,
// so only the outputs need committing.
func checkRemediation(recordsInRepo bool) string {
	if recordsInRepo {
		return "To fix: run `sloff run`, then commit the regenerated outputs and the .sloff/fingerprints/ changes."
	}
	return "To fix: run `sloff run` to regenerate and store the records in the fingerprint backend, then commit the regenerated outputs."
}

// runCheck wires the same resolver/preflight/storage stack as runE and calls
// Runner.Check. ReadOnly is intentionally never set: check ignores
// SLOFF_ALLOW_STALE_DEPS (a verification whose guarantee an env var can
// weaken is not a verification), so the variable only earns a warning here.
// recordsInRepo reports whether the selected backend stores records inside
// the repository (local backend), which decides how drift messages phrase
// the record half of the remediation.
func runCheck(ctx context.Context, errOut io.Writer, rawRoot, pattern string) (rep *runner.CheckReport, recordsInRepo bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	tp, shutdown, err := setupTracing(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("setup tracing: %w", err)
	}
	defer flushTracing(shutdown)

	tracer := tp.Tracer(cmdTracerName)
	ctx, span := tracer.Start(ctx, "sloff.check", trace.WithAttributes(
		attribute.String("sloff.subcommand", "check"),
		attribute.String("sloff.spec.pattern", pattern),
	))
	defer endSpan(span, &err)

	allowStale, err := allowStaleDepsEnabled()
	if err != nil {
		return nil, false, err
	}
	if allowStale {
		fmt.Fprintf(errOut, "sloff check: %s is ignored by check; preflight failures always fail the verification\n", allowStaleDepsEnv)
	}

	root, err := filepath.Abs(rawRoot)
	if err != nil {
		return nil, false, fmt.Errorf("resolve --root: %w", err)
	}
	span.SetAttributes(attribute.String("sloff.repo_root", root))

	specs, err := discoverSpecs(ctx, tracer, root, pattern)
	if err != nil {
		return nil, false, err
	}
	span.SetAttributes(attribute.Int("sloff.spec.count", len(specs)))

	noHashCache, err := fileHashCacheDisabled()
	if err != nil {
		return nil, false, err
	}
	hashCachePath := fileHashCachePath(root)
	if noHashCache {
		hashCachePath = ""
	}

	resolvers, err := buildResolvers(root)
	if err != nil {
		return nil, false, err
	}

	storage, err := loadStorage(ctx, root)
	if err != nil {
		return nil, false, fmt.Errorf("load fingerprint storage: %w", err)
	}
	// The cached decorator wrapping remote backends delegates Name() to the
	// inner backend, so this comparison sees "dynamodb", not the wrapper.
	recordsInRepo = storage.Name() == string(fingerprint.BackendLocal)
	span.SetAttributes(attribute.String("sloff.fingerprint.backend", storage.Name()))

	r := runner.New(runner.Options{
		RepoRoot:          root,
		Specs:             specs,
		Storage:           storage,
		Resolvers:         resolvers,
		Preflight:         buildPreflight(root),
		FileHashCachePath: hashCachePath,
		TracerProvider:    tp,
	})

	rep, err = r.Check(ctx)
	if rep != nil {
		span.SetAttributes(
			attribute.Int("sloff.check.task_count", len(rep.Results)),
			attribute.Int("sloff.check.drift_count", len(rep.Drift())),
		)
	}
	return rep, recordsInRepo, err
}

// printCheckReport prints one detail block per drifted task and a one-line
// summary. Clean tasks stay silent so CI logs surface only what needs acting
// on. Environment-classified unverifiable tasks (tool unresolvable, depends
// producers all clean) are rendered as CANNOT VERIFY rather than DRIFT: their
// cause arrives as the accompanying error and exit code 2, and a DRIFT label
// would misdirect the user to `sloff run`.
func printCheckReport(out io.Writer, rep *runner.CheckReport, recordsInRepo bool) {
	noRecordCause := "the generator was not re-run after an input change, or the record was not committed"
	if !recordsInRepo {
		noRecordCause = "the generator was not re-run after an input change, or its record was never stored in the fingerprint backend"
	}
	drifted := rep.Drift()
	for _, res := range drifted {
		label := checkTaskLabel(res)
		switch res.Status {
		case runner.CheckNoRecord:
			fmt.Fprintf(out, "DRIFT %s: no fingerprint record for the current inputs (%s)\n", label, noRecordCause)
		case runner.CheckOutputMismatch:
			fmt.Fprintf(out, "DRIFT %s: generated outputs do not match the fingerprint record\n", label)
			printCheckIssueList(out, outputIssueLines(res))
		case runner.CheckInputMissing:
			fmt.Fprintf(out, "DRIFT %s: input files are missing from the tree (typically an upstream task's outputs that were not committed)\n", label)
			printCheckIssueList(out, res.MissingInputs)
		case runner.CheckUnverifiable:
			fmt.Fprintf(out, "DRIFT %s: cannot verify — tool %q could not be resolved because tasks it depends on show drift; regenerating will restore its sources\n", label, res.Tool)
		}
	}
	unverifiableEnv := 0
	for _, res := range rep.Results {
		if res.Status == runner.CheckUnverifiable && !res.ToolProducersDrifted {
			unverifiableEnv++
			fmt.Fprintf(out, "CANNOT VERIFY %s: tool %q could not be resolved; every task it depends on is clean, so this is an environment or spec problem, not missing generation\n", checkTaskLabel(res), res.Tool)
		}
	}
	summary := fmt.Sprintf("sloff check: %d tasks checked", len(rep.Results))
	if len(drifted) > 0 {
		summary += fmt.Sprintf(", %d drifted", len(drifted))
	}
	if unverifiableEnv > 0 {
		summary += fmt.Sprintf(", %d unverifiable", unverifiableEnv)
	}
	if len(drifted) == 0 && unverifiableEnv == 0 {
		summary += ", no drift"
	}
	fmt.Fprintln(out, summary)
}

func outputIssueLines(res runner.CheckResult) []string {
	lines := make([]string, 0, len(res.OutputIssues))
	for _, is := range res.OutputIssues {
		lines = append(lines, fmt.Sprintf("%s: %s", is.Reason, is.Path))
	}
	return lines
}

func printCheckIssueList(out io.Writer, lines []string) {
	for i, line := range lines {
		if i == checkIssueListCap {
			fmt.Fprintf(out, "    ... and %d more\n", len(lines)-checkIssueListCap)
			return
		}
		fmt.Fprintf(out, "    %s\n", line)
	}
}

func checkTaskLabel(res runner.CheckResult) string {
	if res.SpecRelpath == "" || res.SpecRelpath == "." {
		return res.Task
	}
	return filepath.ToSlash(res.SpecRelpath) + "/" + res.Task
}
