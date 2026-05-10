package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

// TestRunCacheDiff_TreatsRepeatedFieldOrderAsEqual locks the "semantic diff"
// promise of `sloff fingerprint diff`: two records whose repeated fields are
// shuffled but otherwise identical must compare equal, since fingerprint.Marshal
// already canonicalises that order on write. Bypassing fingerprint.Marshal lets
// us prove the comparison itself is order-insensitive (rather than relying
// on the writer to have sorted).
func TestRunCacheDiff_TreatsRepeatedFieldOrderAsEqual(t *testing.T) {
	dir := t.TempDir()

	makeRecord := func(filesOrder []*fingerprintv1.FileEntry, versionsOrder []*fingerprintv1.ResolvedVersion) *fingerprintv1.Record {
		return &fingerprintv1.Record{
			SchemaVersion: fingerprint.SchemaVersion,
			Spec:          &fingerprintv1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
			Input: &fingerprintv1.Input{
				Hash:                 "deadbeef",
				FilesHash:            "files",
				CmdHash:              "cmd",
				ResolvedVersionsHash: "versions",
				ResolvedVersions:     versionsOrder,
			},
			Output: &fingerprintv1.Output{
				Hash:  "out",
				Files: filesOrder,
			},
		}
	}

	files := []*fingerprintv1.FileEntry{
		{Path: "a.txt", Hash: "h-a"},
		{Path: "b.txt", Hash: "h-b"},
	}
	versions := []*fingerprintv1.ResolvedVersion{
		{Name: "buf", Version: "v1"},
		{Name: "protoc-gen-go", Version: "v2"},
	}

	a := makeRecord(files, versions)
	b := makeRecord(
		[]*fingerprintv1.FileEntry{files[1], files[0]},
		[]*fingerprintv1.ResolvedVersion{versions[1], versions[0]},
	)

	pathA := writePB(t, dir, "a.pb", a)
	pathB := writePB(t, dir, "b.pb", b)

	var out bytes.Buffer
	if err := runFingerprintDiff(&out, pathA, pathB); err != nil {
		t.Errorf("expected no diff for repeated-field order difference, got err=%v output=%s", err, out.String())
	}
}

// writePB encodes rec via raw proto.Marshal (NOT fingerprint.Marshal) so we can
// preserve the in-memory order of repeated fields the test wants to exercise.
func writePB(t *testing.T, dir, name string, rec *fingerprintv1.Record) string {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(rec)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunCacheShow_RejectsCorruptRecord guards that `sloff fingerprint show`
// surfaces corruption rather than printing `{}` for a zero-byte file.
// readRecord goes through fingerprint.Unmarshal which validates schema_version,
// so empty / SCHEMA_VERSION_UNSPECIFIED records fail loudly here as well
// as on the storage path.
func TestRunCacheShow_RejectsCorruptRecord(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.pb")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runFingerprintShow(&out, p); err == nil {
		t.Errorf("expected error for empty record file, got output: %q", out.String())
	}
}

// TestRunCacheShow_HappyPath covers the canonical decode → JSON path that
// `sloff fingerprint show` exists to provide. We assert on hash-stable substrings
// of the output rather than the full document so the test stays robust to
// formatting tweaks of fingerprint.MarshalJSON.
func TestRunCacheShow_HappyPath(t *testing.T) {
	dir := t.TempDir()
	rec := &fingerprintv1.Record{
		SchemaVersion: fingerprint.SchemaVersion,
		Spec:          &fingerprintv1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
		Input:         &fingerprintv1.Input{Hash: "deadbeef"},
		Output:        &fingerprintv1.Output{Hash: "cafebabe"},
	}
	pb, err := fingerprint.Marshal(rec)
	if err != nil {
		t.Fatalf("fingerprint.Marshal: %v", err)
	}
	p := filepath.Join(dir, "rec.pb")
	if err := os.WriteFile(p, pb, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runFingerprintShow(&out, p); err != nil {
		t.Fatalf("runFingerprintShow: %v", err)
	}
	for _, want := range []string{`"schema_version": "SCHEMA_VERSION_V3"`, `"task_id": "copy"`, `"hash": "deadbeef"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// TestRunCacheShow_FileNotFound covers the os.ReadFile error path of
// readRecord — a missing path must surface as an error rather than a
// confusing decode error or empty output.
func TestRunCacheShow_FileNotFound(t *testing.T) {
	var out bytes.Buffer
	if err := runFingerprintShow(&out, filepath.Join(t.TempDir(), "missing.pb")); err == nil {
		t.Error("runFingerprintShow on missing path: expected error")
	}
}

// TestRunCacheDiff_FileNotFound covers the equivalent error path on the
// diff side, where readRecord must propagate file-system errors with a
// path-tagged wrap so the user sees which input is broken.
func TestRunCacheDiff_FileNotFound(t *testing.T) {
	dir := t.TempDir()
	rec := &fingerprintv1.Record{SchemaVersion: fingerprint.SchemaVersion}
	pb, err := fingerprint.Marshal(rec)
	if err != nil {
		t.Fatalf("fingerprint.Marshal: %v", err)
	}
	p := filepath.Join(dir, "rec.pb")
	if err := os.WriteFile(p, pb, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.pb")

	var out bytes.Buffer
	if err := runFingerprintDiff(&out, missing, p); err == nil {
		t.Error("expected error when first path is missing")
	}
	out.Reset()
	if err := runFingerprintDiff(&out, p, missing); err == nil {
		t.Error("expected error when second path is missing")
	}
}

// TestFingerprintCommandViaRootCmd exercises the cobra wiring: newRootCmd ->
// newFingerprintCmd -> newFingerprintShowCmd -> RunE -> runFingerprintShow. Calling the
// helpers directly skips the RunE wrappers, so this integration-style
// test is the only way to register coverage for the cobra glue.
func TestFingerprintCommandViaRootCmd(t *testing.T) {
	dir := t.TempDir()
	rec := &fingerprintv1.Record{
		SchemaVersion: fingerprint.SchemaVersion,
		Spec:          &fingerprintv1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
		Input:         &fingerprintv1.Input{Hash: "deadbeef"},
		Output:        &fingerprintv1.Output{Hash: "cafebabe"},
	}
	pb, err := fingerprint.Marshal(rec)
	if err != nil {
		t.Fatalf("fingerprint.Marshal: %v", err)
	}
	p := filepath.Join(dir, "rec.pb")
	if err := os.WriteFile(p, pb, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"fingerprint", "show", p})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(fingerprint show): %v", err)
	}
	if !strings.Contains(out.String(), `"task_id": "copy"`) {
		t.Errorf("expected task_id in output:\n%s", out.String())
	}

	// Same record diffed against itself: must exit 0 with no output.
	out.Reset()
	cmd = newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"fingerprint", "diff", p, p})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(fingerprint diff a==a): %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected silent output for self-diff, got: %q", out.String())
	}
}

// TestExitCodeError documents that exitCodeError.Error() is wired so
// stdlib helpers (errors.Is/As, fmt verbs) don't panic on the sentinel
// types main hands to os.Exit.
func TestExitCodeError(t *testing.T) {
	e := &exitCodeError{code: 7}
	if got := e.Error(); !strings.Contains(got, "7") {
		t.Errorf("Error() = %q, expected to mention exit code 7", got)
	}
}

// TestRunCacheDiff_IgnoresInformationalFieldDrift covers the "semantic" promise
// of `sloff fingerprint diff`: drift in fields ADR-0009 marks as informational
// (resolved_versions[*].source) must not change the exit code or produce a
// diff. ADR-0010 dropped the previous generated_at drift case from this
// guarantee by removing the field entirely.
func TestRunCacheDiff_IgnoresInformationalFieldDrift(t *testing.T) {
	dir := t.TempDir()

	base := func() *fingerprintv1.Record {
		return &fingerprintv1.Record{
			SchemaVersion: fingerprint.SchemaVersion,
			Spec:          &fingerprintv1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
			Input: &fingerprintv1.Input{
				Hash:                 "deadbeef",
				FilesHash:            "files",
				CmdHash:              "cmd",
				ResolvedVersionsHash: "versions",
				ResolvedVersions: []*fingerprintv1.ResolvedVersion{
					{Name: "buf", Version: "v1", Source: "script:buf"},
				},
			},
			Output: &fingerprintv1.Output{
				Hash:  "out",
				Files: []*fingerprintv1.FileEntry{{Path: "a.txt", Hash: "h-a"}},
			},
		}
	}

	a := base()
	b := base()
	// source drift: imagine the user migrated aqua → mise; same Version,
	// new label.
	b.Input.ResolvedVersions[0].Source = "mise:buf"

	pathA := writePB(t, dir, "a.pb", a)
	pathB := writePB(t, dir, "b.pb", b)

	var out bytes.Buffer
	if err := runFingerprintDiff(&out, pathA, pathB); err != nil {
		t.Errorf("expected exit 0 for informational-only drift, got err=%v output=%s", err, out.String())
	}
	if out.Len() != 0 {
		t.Errorf("expected silent output for informational-only drift, got: %q", out.String())
	}
}

// TestRunCacheGC_CollapsesDuplicates exercises the duplicate-collapse safety
// net introduced for ADR-0010. After a hand-crafted post-merge state with
// three timestamp variants of the same (spec, task, input_hash) Key,
// `sloff fingerprint gc` must leave only the earliest-prefix file.
func TestRunCacheGC_CollapsesDuplicates(t *testing.T) {
	root := t.TempDir()
	fingerprintDir := filepath.Join(root, ".sloff", "fingerprints", "spec", "copy")
	if err := os.MkdirAll(fingerprintDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &fingerprintv1.Record{
		SchemaVersion: fingerprint.SchemaVersion,
		Spec:          &fingerprintv1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
		Input:         &fingerprintv1.Input{Hash: "deadbeef"},
		Output:        &fingerprintv1.Output{Hash: "cafebabe"},
	}
	pb, err := fingerprint.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	files := []string{
		"20260101000000000-deadbeef.pb",
		"20260301000000000-deadbeef.pb",
		"20260601000000000-deadbeef.pb",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(fingerprintDir, name), pb, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	if err := runFingerprintGC(context.Background(), &out, root); err != nil {
		t.Fatalf("runFingerprintGC: %v", err)
	}
	entries, err := os.ReadDir(fingerprintDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != files[0] {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only %q to remain, got %v", files[0], names)
	}
	if !strings.Contains(out.String(), "2") {
		t.Errorf("expected gc output to mention removal count, got %q", out.String())
	}
}

// TestFingerprintGCCommandViaRootCmd exercises the cobra wiring for `fingerprint gc`,
// including the `--repo-root` flag plumb-through that the helper-only
// runFingerprintGC test does not cover. Without this, the RunE branch (cwd
// resolution + context propagation) drops out of the coverage profile.
func TestFingerprintGCCommandViaRootCmd(t *testing.T) {
	root := t.TempDir()
	fingerprintDir := filepath.Join(root, ".sloff", "fingerprints", "spec", "copy")
	if err := os.MkdirAll(fingerprintDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &fingerprintv1.Record{
		SchemaVersion: fingerprint.SchemaVersion,
		Spec:          &fingerprintv1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
		Input:         &fingerprintv1.Input{Hash: "deadbeef"},
		Output:        &fingerprintv1.Output{Hash: "cafebabe"},
	}
	pb, err := fingerprint.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"20260101000000000-deadbeef.pb",
		"20260601000000000-deadbeef.pb",
	} {
		if err := os.WriteFile(filepath.Join(fingerprintDir, name), pb, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"fingerprint", "gc", "--repo-root", root})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(fingerprint gc): %v", err)
	}
	if !strings.Contains(out.String(), "collapsed") {
		t.Errorf("expected gc summary in output, got: %q", out.String())
	}
	entries, err := os.ReadDir(fingerprintDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "20260101000000000-deadbeef.pb" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only earliest-prefix file remaining, got %v", names)
	}
}

// TestRunCacheGC_NoRecordsIsNoop covers the happy-zero path: a repo without
// any fingerprints must succeed with `collapsed 0 ...` rather than failing
// loudly. Captures the empty-list branch through CollapseDuplicates.
func TestRunCacheGC_NoRecordsIsNoop(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	if err := runFingerprintGC(context.Background(), &out, root); err != nil {
		t.Fatalf("runFingerprintGC: %v", err)
	}
	if !strings.Contains(out.String(), "collapsed 0") {
		t.Errorf("expected zero-collapse output, got: %q", out.String())
	}
}

// TestFingerprintGC_DefaultsToCwd covers the `--repo-root` omitted branch of
// newFingerprintGCCmd, where the command resolves repo root from cwd. We chdir
// into a tempdir, invoke `sloff fingerprint gc` with no flags, and assert it
// operated against the tempdir.
func TestFingerprintGC_DefaultsToCwd(t *testing.T) {
	root := t.TempDir()
	fingerprintDir := filepath.Join(root, ".sloff", "fingerprints", "spec", "copy")
	if err := os.MkdirAll(fingerprintDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &fingerprintv1.Record{
		SchemaVersion: fingerprint.SchemaVersion,
		Spec:          &fingerprintv1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
		Input:         &fingerprintv1.Input{Hash: "deadbeef"},
		Output:        &fingerprintv1.Output{Hash: "cafebabe"},
	}
	pb, err := fingerprint.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"20260101000000000-deadbeef.pb",
		"20260601000000000-deadbeef.pb",
	} {
		if err := os.WriteFile(filepath.Join(fingerprintDir, name), pb, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	prevWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWd) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"fingerprint", "gc"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(fingerprint gc): %v", err)
	}

	entries, err := os.ReadDir(fingerprintDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only earliest preserved after cwd-default gc, got %v", names)
	}
}

// TestRunCacheDiff_SurfacesSemanticDifference is the negative half: when the
// records differ in a hash-significant field (here output.hash), `fingerprint diff`
// must exit 1 and emit the JSON diff.
func TestRunCacheDiff_SurfacesSemanticDifference(t *testing.T) {
	dir := t.TempDir()

	base := func() *fingerprintv1.Record {
		return &fingerprintv1.Record{
			SchemaVersion: fingerprint.SchemaVersion,
			Spec:          &fingerprintv1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
			Input:         &fingerprintv1.Input{Hash: "deadbeef"},
			Output:        &fingerprintv1.Output{Hash: "out-a"},
		}
	}

	a := base()
	b := base()
	b.Output.Hash = "out-b"

	pathA := writePB(t, dir, "a.pb", a)
	pathB := writePB(t, dir, "b.pb", b)

	var out bytes.Buffer
	err := runFingerprintDiff(&out, pathA, pathB)
	if err == nil {
		t.Fatal("expected exit-code error for semantically different records")
	}
	if out.Len() == 0 {
		t.Error("expected JSON diff output for semantically different records")
	}
}
