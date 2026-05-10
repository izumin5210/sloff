package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

func newFingerprintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fingerprint",
		Short: "Inspect on-disk fingerprints",
		Long: `fingerprint groups subcommands that decode the protobuf-encoded record
files under .sloff/fingerprints/. Records are opaque to grep/editor search
by design (ADR-0009); these commands provide the debug path for
verifying record contents and comparing two records.`,
	}
	cmd.AddCommand(newFingerprintShowCmd())
	cmd.AddCommand(newFingerprintDiffCmd())
	cmd.AddCommand(newFingerprintGCCmd())
	return cmd
}

func newFingerprintGCCmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Collapse duplicate timestamp variants of fingerprints",
		Long: `gc walks .sloff/fingerprints/ and, for each (spec, task, input_hash) Key
that has more than one <timestamp>-<input_hash>.pb sibling, removes
every variant except the earliest-prefix one. Save's in-line collapse
only fires on fingerprint miss with a changed output, which is rare under
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
			return runFingerprintGC(cobraCmd.Context(), cobraCmd.OutOrStdout(), root)
		},
	}
	cmd.Flags().StringVar(&root, "repo-root", "", "repo root containing .sloff/fingerprints/ (default: cwd)")
	return cmd
}

func runFingerprintGC(ctx context.Context, w io.Writer, root string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	st, err := loadStorage(ctx, root)
	if err != nil {
		return err
	}
	removed, err := st.CollapseDuplicates(ctx)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "collapsed %d duplicate record file(s)\n", removed)
	return err
}

func newFingerprintShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <path>",
		Short: "Decode a fingerprint (.pb) and print as JSON",
		Long: `show reads a single fingerprint file, decodes it via the v1
schema, and writes the canonical protojson representation to stdout.

The output is suitable as a git diff textconv; configure with:

  git config diff.sloff-fingerprint.textconv "sloff fingerprint show"
  echo '*.pb diff=sloff-fingerprint' >> .gitattributes

so 'git diff' on .sloff/fingerprints/ shows the decoded form.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return runFingerprintShow(cobraCmd.OutOrStdout(), args[0])
		},
	}
	return cmd
}

func newFingerprintDiffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff <a> <b>",
		Short: "Print a semantic diff between two fingerprints",
		Long: `diff decodes two record files and prints a field-level diff of
their canonical protojson form. Returns exit code 1 when the
records differ semantically (for use in scripts / pre-commit
hooks).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return runFingerprintDiff(cobraCmd.OutOrStdout(), args[0], args[1])
		},
	}
	return cmd
}

func runFingerprintShow(w io.Writer, path string) error {
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

func runFingerprintDiff(w io.Writer, pathA, pathB string) error {
	a, err := readRecord(pathA)
	if err != nil {
		return fmt.Errorf("read %s: %w", pathA, err)
	}
	b, err := readRecord(pathB)
	if err != nil {
		return fmt.Errorf("read %s: %w", pathB, err)
	}
	// Records the runner writes are already canonical, but a caller could
	// hand `fingerprint diff` a record that was hand-crafted or written by a
	// sloff build with a different sort discipline. Normalise repeated
	// fields so order-only differences never surface.
	fingerprint.Sort(a)
	fingerprint.Sort(b)

	// Semantic equality ignores fields documented as informational
	// (resolved_versions[*].source doesn't feed into the fingerprint key,
	// ADR-0009). Two records with the same fingerprint identity but different
	// resolver source label exit 0 silently. Use `sloff fingerprint show` on each
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
	return errFingerprintsDiffer
}

// recordsSemanticallyEqual compares two records ignoring informational fields.
// Defined here rather than in package fingerprint because the asymmetry — present in
// the wire format, but excluded from fingerprint identity — is a CLI concern.
func recordsSemanticallyEqual(a, b *fingerprintv1.Record) bool {
	aClean := proto.Clone(a).(*fingerprintv1.Record)
	bClean := proto.Clone(b).(*fingerprintv1.Record)
	clearInformationalFields(aClean)
	clearInformationalFields(bClean)
	return proto.Equal(aClean, bClean)
}

func clearInformationalFields(rec *fingerprintv1.Record) {
	if in := rec.GetInput(); in != nil {
		for _, v := range in.GetResolvedVersions() {
			v.Source = ""
		}
	}
}

// errFingerprintsDiffer is returned by `fingerprint diff` when the two records are
// not proto.Equal. main handles exitCodeError specially so the diff stays on
// stdout without a duplicate message on stderr.
var errFingerprintsDiffer = &exitCodeError{code: 1}

func readRecord(path string) (*fingerprintv1.Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rec, err := fingerprint.Unmarshal(b)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return rec, nil
}

func marshalRecordJSON(rec *fingerprintv1.Record) ([]byte, error) {
	return fingerprint.MarshalJSON(rec)
}
