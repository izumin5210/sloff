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
	// Records the runner writes are already canonical, but a caller could
	// hand `cache diff` a record that was hand-crafted or written by a
	// sloff build with a different sort discipline. Normalise repeated
	// fields so order-only differences never surface.
	cache.Sort(a)
	cache.Sort(b)

	// Semantic equality ignores fields documented as informational
	// (generated_at, resolved_versions[*].source — neither feeds into the
	// cache hash, ADR-0009). Two records with the same cache identity but
	// different first-observed timestamp or resolver source label exit 0
	// silently. Use `sloff cache show` on each path for a full byte view.
	if recordsSemanticallyEqual(a, b) {
		return nil
	}

	// Truly different records: print the full JSON diff (including
	// informational fields, so the user can still see what shifted in
	// generated_at / source as part of a semantically different record)
	// and exit 1.
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
	rec.GeneratedAt = nil
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
