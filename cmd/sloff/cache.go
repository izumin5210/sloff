package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	cachev1 "github.com/izumin5210/sloff/internal/proto/sloff/cache/v1"
	"github.com/izumin5210/sloff/internal/sloff/cache"
	"github.com/izumin5210/sloff/internal/sloff/cache/local"
)

func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect on-disk cache records",
		Long: `cache groups subcommands that decode the protobuf-encoded record
files under .sloff/cache/. Records are opaque to grep/editor search
by design (ADR-0009); these commands provide the debug path for
verifying record contents and comparing two records.`,
	}
	cmd.AddCommand(newCacheShowCmd())
	cmd.AddCommand(newCacheDiffCmd())
	cmd.AddCommand(newCacheGCCmd())
	return cmd
}

func newCacheGCCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Collapse duplicate timestamp variants of cache records",
		Long: `gc walks .sloff/cache/ and, for each (spec, task, input_hash) Key
that has more than one <timestamp>-<input_hash>.pb sibling, removes
every variant except the earliest-prefix one. Save's in-line collapse
only fires on cache miss with a changed output, which is rare under
deterministic-generator scope, so post-merge duplicates can otherwise
linger until this safety net sweeps them (ADR-0010 §"duplicate
collapse の責務").`,
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			if root == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				root = wd
			}
			return runCacheGC(cobraCmd.Context(), cobraCmd.OutOrStdout(), root)
		},
	}
	cmd.Flags().StringVar(&root, "repo-root", "", "repo root containing .sloff/cache/ (default: cwd)")
	return cmd
}

func runCacheGC(ctx context.Context, w io.Writer, root string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	st := local.New(root)
	removed, err := st.CollapseDuplicates(ctx)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "collapsed %d duplicate record file(s)\n", removed)
	return err
}

func newCacheShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <path>",
		Short: "Decode a cache record (.pb) and print as JSON",
		Long: `show reads a single cache record file, decodes it via the v1
schema, and writes the canonical protojson representation to stdout.

The output is suitable as a git diff textconv; configure with:

  git config diff.sloff-cache.textconv "sloff cache show"
  echo '*.pb diff=sloff-cache' >> .gitattributes

so 'git diff' on .sloff/cache/ shows the decoded form.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return runCacheShow(cobraCmd.OutOrStdout(), args[0])
		},
	}
	return cmd
}

func newCacheDiffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff <a> <b>",
		Short: "Print a semantic diff between two cache records",
		Long: `diff decodes two record files and prints a field-level diff of
their canonical protojson form. Returns exit code 1 when the
records differ semantically (for use in scripts / pre-commit
hooks).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return runCacheDiff(cobraCmd.OutOrStdout(), args[0], args[1])
		},
	}
	return cmd
}

func runCacheShow(w io.Writer, path string) error {
	rec, err := readRecord(path)
	if err != nil {
		return err
	}
	out, err := marshalRecordJSON(rec)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(out))
	return err
}

func runCacheDiff(w io.Writer, pathA, pathB string) error {
	a, err := readRecord(pathA)
	if err != nil {
		return fmt.Errorf("read %s: %w", pathA, err)
	}
	b, err := readRecord(pathB)
	if err != nil {
		return fmt.Errorf("read %s: %w", pathB, err)
	}
	// Records the runner writes are already canonical, but a caller could
	// hand `cache diff` a record that was hand-crafted or written by a
	// sloff build with a different sort discipline. Normalise repeated
	// fields so order-only differences never surface.
	cache.Sort(a)
	cache.Sort(b)

	// Semantic equality ignores fields documented as informational
	// (resolved_versions[*].source doesn't feed into the cache hash,
	// ADR-0009). Two records with the same cache identity but different
	// resolver source label exit 0 silently. Use `sloff cache show` on each
	// path for a full byte view. The earlier `generated_at` field was dropped
	// in ADR-0010 along with the schema_version V3 bump, so it no longer
	// participates in this comparison.
	if recordsSemanticallyEqual(a, b) {
		return nil
	}

	// Truly different records: print the full JSON diff (including the
	// informational source field, so the user can still see what shifted as
	// part of a semantically different record) and exit 1.
	jsonA, err := marshalRecordJSON(a)
	if err != nil {
		return err
	}
	jsonB, err := marshalRecordJSON(b)
	if err != nil {
		return err
	}
	diff := cmp.Diff(string(jsonA), string(jsonB))
	if _, err := fmt.Fprint(w, diff); err != nil {
		return err
	}
	return errCacheRecordsDiffer
}

// recordsSemanticallyEqual compares two records ignoring informational fields.
// Defined here rather than in package cache because the asymmetry — present in
// the wire format, but excluded from cache identity — is a CLI concern.
func recordsSemanticallyEqual(a, b *cachev1.Record) bool {
	aClean := proto.Clone(a).(*cachev1.Record)
	bClean := proto.Clone(b).(*cachev1.Record)
	clearInformationalFields(aClean)
	clearInformationalFields(bClean)
	return proto.Equal(aClean, bClean)
}

func clearInformationalFields(rec *cachev1.Record) {
	if in := rec.GetInput(); in != nil {
		for _, v := range in.GetResolvedVersions() {
			v.Source = ""
		}
	}
}

// errCacheRecordsDiffer is returned by `cache diff` when the two records are
// not proto.Equal. main handles exitCodeError specially so the diff stays on
// stdout without a duplicate message on stderr.
var errCacheRecordsDiffer = &exitCodeError{code: 1}

func readRecord(path string) (*cachev1.Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rec, err := cache.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return rec, nil
}

func marshalRecordJSON(rec *cachev1.Record) ([]byte, error) {
	return cache.MarshalJSON(rec)
}
