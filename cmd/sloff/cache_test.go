package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	cachev1 "github.com/izumin5210/sloff/internal/proto/sloff/cache/v1"
	"github.com/izumin5210/sloff/internal/sloff/cache"
)

// TestRunCacheDiff_TreatsRepeatedFieldOrderAsEqual locks the "semantic diff"
// promise of `sloff cache diff`: two records whose repeated fields are
// shuffled but otherwise identical must compare equal, since cache.Marshal
// already canonicalises that order on write. Bypassing cache.Marshal lets
// us prove the comparison itself is order-insensitive (rather than relying
// on the writer to have sorted).
func TestRunCacheDiff_TreatsRepeatedFieldOrderAsEqual(t *testing.T) {
	dir := t.TempDir()

	makeRecord := func(filesOrder []*cachev1.FileEntry, versionsOrder []*cachev1.ResolvedVersion) *cachev1.Record {
		return &cachev1.Record{
			SchemaVersion: cache.SchemaVersion,
			Spec:          &cachev1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
			Input: &cachev1.Input{
				Hash:                 "deadbeef",
				FilesHash:            "files",
				CmdHash:              "cmd",
				ResolvedVersionsHash: "versions",
				ResolvedVersions:     versionsOrder,
			},
			Output: &cachev1.Output{
				Hash:  "out",
				Files: filesOrder,
			},
		}
	}

	files := []*cachev1.FileEntry{
		{Path: "a.txt", Hash: "h-a"},
		{Path: "b.txt", Hash: "h-b"},
	}
	versions := []*cachev1.ResolvedVersion{
		{Name: "buf", Version: "v1"},
		{Name: "protoc-gen-go", Version: "v2"},
	}

	a := makeRecord(files, versions)
	b := makeRecord(
		[]*cachev1.FileEntry{files[1], files[0]},
		[]*cachev1.ResolvedVersion{versions[1], versions[0]},
	)

	pathA := writePB(t, dir, "a.pb", a)
	pathB := writePB(t, dir, "b.pb", b)

	var out bytes.Buffer
	if err := runCacheDiff(&out, pathA, pathB); err != nil {
		t.Errorf("expected no diff for repeated-field order difference, got err=%v output=%s", err, out.String())
	}
}

// writePB encodes rec via raw proto.Marshal (NOT cache.Marshal) so we can
// preserve the in-memory order of repeated fields the test wants to exercise.
func writePB(t *testing.T, dir, name string, rec *cachev1.Record) string {
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

// TestRunCacheShow_RejectsCorruptRecord guards that `sloff cache show`
// surfaces corruption rather than printing `{}` for a zero-byte file.
// readRecord goes through cache.Unmarshal which validates schema_version,
// so empty / SCHEMA_VERSION_UNSPECIFIED records fail loudly here as well
// as on the storage path.
func TestRunCacheShow_RejectsCorruptRecord(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.pb")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCacheShow(&out, p); err == nil {
		t.Errorf("expected error for empty record file, got output: %q", out.String())
	}
}

// TestRunCacheShow_HappyPath covers the canonical decode → JSON path that
// `sloff cache show` exists to provide. We assert on hash-stable substrings
// of the output rather than the full document so the test stays robust to
// formatting tweaks of cache.MarshalJSON.
func TestRunCacheShow_HappyPath(t *testing.T) {
	dir := t.TempDir()
	rec := &cachev1.Record{
		SchemaVersion: cache.SchemaVersion,
		Spec:          &cachev1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
		Input:         &cachev1.Input{Hash: "deadbeef"},
		Output:        &cachev1.Output{Hash: "cafebabe"},
	}
	pb, err := cache.Marshal(rec)
	if err != nil {
		t.Fatalf("cache.Marshal: %v", err)
	}
	p := filepath.Join(dir, "rec.pb")
	if err := os.WriteFile(p, pb, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runCacheShow(&out, p); err != nil {
		t.Fatalf("runCacheShow: %v", err)
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
	if err := runCacheShow(&out, filepath.Join(t.TempDir(), "missing.pb")); err == nil {
		t.Error("runCacheShow on missing path: expected error")
	}
}

// TestRunCacheDiff_FileNotFound covers the equivalent error path on the
// diff side, where readRecord must propagate file-system errors with a
// path-tagged wrap so the user sees which input is broken.
func TestRunCacheDiff_FileNotFound(t *testing.T) {
	dir := t.TempDir()
	rec := &cachev1.Record{SchemaVersion: cache.SchemaVersion}
	pb, err := cache.Marshal(rec)
	if err != nil {
		t.Fatalf("cache.Marshal: %v", err)
	}
	p := filepath.Join(dir, "rec.pb")
	if err := os.WriteFile(p, pb, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.pb")

	var out bytes.Buffer
	if err := runCacheDiff(&out, missing, p); err == nil {
		t.Error("expected error when first path is missing")
	}
	out.Reset()
	if err := runCacheDiff(&out, p, missing); err == nil {
		t.Error("expected error when second path is missing")
	}
}

// TestCacheCommandViaRootCmd exercises the cobra wiring: newRootCmd ->
// newCacheCmd -> newCacheShowCmd -> RunE -> runCacheShow. Calling the
// helpers directly skips the RunE wrappers, so this integration-style
// test is the only way to register coverage for the cobra glue.
func TestCacheCommandViaRootCmd(t *testing.T) {
	dir := t.TempDir()
	rec := &cachev1.Record{
		SchemaVersion: cache.SchemaVersion,
		Spec:          &cachev1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
		Input:         &cachev1.Input{Hash: "deadbeef"},
		Output:        &cachev1.Output{Hash: "cafebabe"},
	}
	pb, err := cache.Marshal(rec)
	if err != nil {
		t.Fatalf("cache.Marshal: %v", err)
	}
	p := filepath.Join(dir, "rec.pb")
	if err := os.WriteFile(p, pb, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"cache", "show", p})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(cache show): %v", err)
	}
	if !strings.Contains(out.String(), `"task_id": "copy"`) {
		t.Errorf("expected task_id in output:\n%s", out.String())
	}

	// Same record diffed against itself: must exit 0 with no output.
	out.Reset()
	cmd = newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"cache", "diff", p, p})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(cache diff a==a): %v", err)
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
// of `sloff cache diff`: drift in fields ADR-0009 marks as informational
// (resolved_versions[*].source) must not change the exit code or produce a
// diff. ADR-0010 dropped the previous generated_at drift case from this
// guarantee by removing the field entirely.
func TestRunCacheDiff_IgnoresInformationalFieldDrift(t *testing.T) {
	dir := t.TempDir()

	base := func() *cachev1.Record {
		return &cachev1.Record{
			SchemaVersion: cache.SchemaVersion,
			Spec:          &cachev1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
			Input: &cachev1.Input{
				Hash:                 "deadbeef",
				FilesHash:            "files",
				CmdHash:              "cmd",
				ResolvedVersionsHash: "versions",
				ResolvedVersions: []*cachev1.ResolvedVersion{
					{Name: "buf", Version: "v1", Source: "script:buf"},
				},
			},
			Output: &cachev1.Output{
				Hash:  "out",
				Files: []*cachev1.FileEntry{{Path: "a.txt", Hash: "h-a"}},
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
	if err := runCacheDiff(&out, pathA, pathB); err != nil {
		t.Errorf("expected exit 0 for informational-only drift, got err=%v output=%s", err, out.String())
	}
	if out.Len() != 0 {
		t.Errorf("expected silent output for informational-only drift, got: %q", out.String())
	}
}

// TestRunCacheGC_CollapsesDuplicates exercises the duplicate-collapse safety
// net introduced for ADR-0010. After a hand-crafted post-merge state with
// three timestamp variants of the same (spec, task, input_hash) Key,
// `sloff cache gc` must leave only the earliest-prefix file.
func TestRunCacheGC_CollapsesDuplicates(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, ".sloff", "cache", "spec", "copy")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &cachev1.Record{
		SchemaVersion: cache.SchemaVersion,
		Spec:          &cachev1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
		Input:         &cachev1.Input{Hash: "deadbeef"},
		Output:        &cachev1.Output{Hash: "cafebabe"},
	}
	pb, err := cache.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	files := []string{
		"20260101000000000-deadbeef.pb",
		"20260301000000000-deadbeef.pb",
		"20260601000000000-deadbeef.pb",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(cacheDir, name), pb, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	if err := runCacheGC(context.Background(), &out, root); err != nil {
		t.Fatalf("runCacheGC: %v", err)
	}
	entries, err := os.ReadDir(cacheDir)
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

// TestRunCacheDiff_SurfacesSemanticDifference is the negative half: when the
// records differ in a hash-significant field (here output.hash), `cache diff`
// must exit 1 and emit the JSON diff.
func TestRunCacheDiff_SurfacesSemanticDifference(t *testing.T) {
	dir := t.TempDir()

	base := func() *cachev1.Record {
		return &cachev1.Record{
			SchemaVersion: cache.SchemaVersion,
			Spec:          &cachev1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
			Input:         &cachev1.Input{Hash: "deadbeef"},
			Output:        &cachev1.Output{Hash: "out-a"},
		}
	}

	a := base()
	b := base()
	b.Output.Hash = "out-b"

	pathA := writePB(t, dir, "a.pb", a)
	pathB := writePB(t, dir, "b.pb", b)

	var out bytes.Buffer
	err := runCacheDiff(&out, pathA, pathB)
	if err == nil {
		t.Fatal("expected exit-code error for semantically different records")
	}
	if out.Len() == 0 {
		t.Error("expected JSON diff output for semantically different records")
	}
}
