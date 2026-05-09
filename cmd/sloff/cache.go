package main

import (
	"fmt"
	"io"
	"os"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	cachev1 "github.com/izumin5210/sloff/internal/proto/sloff/cache/v1"
	"github.com/izumin5210/sloff/internal/sloff/cache"
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
	return cmd
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
	// Records the runner writes are already sorted by cache.Marshal, but a
	// caller could hand `cache diff` a record that was constructed by hand or
	// by a sloff build with a different sort discipline. Normalise both sides
	// so order-only differences in repeated fields don't surface as semantic
	// diffs (matches the "semantic diff" wording in the help text).
	cache.Sort(a)
	cache.Sort(b)
	if proto.Equal(a, b) {
		return nil
	}
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
