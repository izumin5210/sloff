package local_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/local"
)

var fixedClock = time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

func newStorage(root string, now time.Time) *local.Storage {
	return local.New(root, local.WithClock(func() time.Time { return now }))
}

func newRecord(taskID string) *fingerprintv1.Record {
	return &fingerprintv1.Record{
		Input:         &fingerprintv1.Input{Hash: "deadbeef"},
		Output:        &fingerprintv1.Output{Hash: "cafebabe"},
		SchemaVersion: fingerprint.SchemaVersion,
		Spec: &fingerprintv1.Spec{
			Cmd:    "echo hi",
			Dir:    "path/to/spec",
			TaskId: taskID,
		},
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "path/to/spec", TaskID: "gen", InputHash: "deadbeef"}
	rec := newRecord("gen")

	if err := st.Save(ctx, key, rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := st.Load(ctx, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatal("Load: expected hit")
	}
	if diff := cmp.Diff(rec, got, protocmp.Transform()); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestLoad_MissReturnsFalse(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	got, ok, err := st.Load(ctx, fingerprint.Key{SpecRelpath: "x", TaskID: "y", InputHash: "z"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ok || got != nil {
		t.Errorf("expected miss, got ok=%v rec=%v", ok, got)
	}
}

func TestSave_PreservesSpecRelpathHierarchyAndTimestampPrefix(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "path/to/spec", TaskID: "gen", InputHash: "abc123"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Spec dir hierarchy is preserved verbatim (deviates from architecture.md's
	// optional "_" substitution so List can losslessly recover the spec_relpath).
	// The filename carries a YYYYMMDDHHMMSSsss timestamp prefix per ADR-0010;
	// fixedClock = 2026-05-05 12:00:00.000 UTC.
	want := filepath.Join(root, ".sloff", "fingerprints", "path", "to", "spec", "gen", "20260505120000000-abc123.pb")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected record at %s, got err=%v", want, err)
	}
}

func TestSave_PreservesPrefixOnInPlaceOverwrite(t *testing.T) {
	root := t.TempDir()
	now := fixedClock
	clockFn := func() time.Time { return now }
	st := local.New(root, local.WithClock(clockFn))
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen", "20260505120000000-h.pb")
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("first save missing: %v", err)
	}

	// Advance the clock by an hour. A second Save for the same Key must not
	// produce a new filename; the original prefix is the canonical
	// initial-creation timestamp.
	now = now.Add(time.Hour)
	updated := newRecord("gen")
	updated.Output.Hash = "newhash"
	if err := st.Save(ctx, key, updated); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Errorf("expected prefix preserved at %s, got err=%v", first, err)
	}
	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected exactly 1 file after re-save, got %d: %v", len(entries), names)
	}
}

// rawV2Bytes materialises record bytes carrying the superseded
// SCHEMA_VERSION_V2 via raw proto marshal, bypassing fingerprint.Marshal's
// writer-side validation, so tests can plant legacy on-disk records.
func rawV2Bytes(t *testing.T, taskID string) []byte {
	t.Helper()
	rec := newRecord(taskID)
	rec.SchemaVersion = fingerprintv1.SchemaVersion_SCHEMA_VERSION_V2
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(rec)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	return b
}

// TestLoad_TreatsSupersededSchemaVersionAsMiss guards the ADR-0010 migration
// contract: a leftover V2 record reads as a miss (so the runner regenerates
// it through the normal miss path) rather than as a hard error that would
// abort the whole run via the prefetch LoadMany.
func TestLoad_TreatsSupersededSchemaVersionAsMiss(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20260505120000000-h.pb"), rawV2Bytes(t, "gen"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec, ok, err := st.Load(ctx, fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"})
	if err != nil {
		t.Fatalf("Load: expected superseded V2 record to read as a miss, got error: %v", err)
	}
	if ok || rec != nil {
		t.Errorf("Load: expected miss for superseded V2 record, got ok=%v rec=%v", ok, rec)
	}
}

// TestSave_OverwritesSupersededRecordInPlace follows the miss through to the
// rewrite: Save for the same Key must collapse onto the existing V2 file
// (preserving the ADR-0010 creation-timestamp prefix) and leave a single V3
// record behind — no residue, no second filename.
func TestSave_OverwritesSupersededRecordInPlace(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock.Add(time.Hour))
	ctx := context.Background()

	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "20260505120000000-h.pb")
	if err := os.WriteFile(legacy, rawV2Bytes(t, "gen"), 0o644); err != nil {
		t.Fatal(err)
	}

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "20260505120000000-h.pb" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected the V2 file overwritten in place, got: %v", names)
	}
	b, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := fingerprint.Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal rewritten record: %v", err)
	}
	if rec.GetSchemaVersion() != fingerprint.SchemaVersion {
		t.Errorf("expected rewritten record at schema %v, got %v", fingerprint.SchemaVersion, rec.GetSchemaVersion())
	}
}

// TestSave_LeavesNoTempArtifacts guards the atomic-write implementation:
// both the fresh-write and in-place-overwrite paths must finish with exactly
// the record file on disk — a leftover temp file would accumulate in the
// git-tracked fingerprint tree.
func TestSave_LeavesNoTempArtifacts(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatalf("Save (fresh): %v", err)
	}
	updated := newRecord("gen")
	updated.Output.Hash = "newhash"
	if err := st.Save(ctx, key, updated); err != nil {
		t.Fatalf("Save (overwrite): %v", err)
	}

	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), fingerprint.FileExt) {
			t.Errorf("unexpected non-record artifact left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected exactly 1 record file, got %d: %v", len(entries), names)
	}
}

func TestSave_CollapsesPostMergeDuplicates(t *testing.T) {
	root := t.TempDir()
	now := fixedClock
	st := local.New(root, local.WithClock(func() time.Time { return now }))
	ctx := context.Background()

	// Pre-seed two duplicate timestamp variants of the same Key, as if two
	// branches independently produced first-writes that were later merged.
	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(dir, "20260101000000000-h.pb")
	newer := filepath.Join(dir, "20260601000000000-h.pb")
	rec := newRecord("gen")
	bytes, err := fingerprint.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{older, newer} {
		if err := os.WriteFile(p, bytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	updated := newRecord("gen")
	updated.Output.Hash = "newhash"
	if err := st.Save(ctx, key, updated); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(older); err != nil {
		t.Errorf("expected earliest prefix kept at %s, got err=%v", older, err)
	}
	if _, err := os.Stat(newer); !os.IsNotExist(err) {
		t.Errorf("expected later duplicate removed at %s, got err=%v", newer, err)
	}
}

func TestLoad_ReturnsLatestAmongDuplicates(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	older := newRecord("gen")
	older.Output.Hash = "older"
	newer := newRecord("gen")
	newer.Output.Hash = "newer"
	for path, rec := range map[string]*fingerprintv1.Record{
		filepath.Join(dir, "20260101000000000-h.pb"): older,
		filepath.Join(dir, "20260601000000000-h.pb"): newer,
	} {
		b, err := fingerprint.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, ok, err := st.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected hit")
	}
	if got.GetOutput().GetHash() != "newer" {
		t.Errorf("expected latest record (newer), got hash=%q", got.GetOutput().GetHash())
	}
}

func TestDelete_RemovesFile(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "task", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("task")); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err := st.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected miss after Delete")
	}
}

func TestDelete_RemovesAllTimestampVariants(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "task", InputHash: "h"}
	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "task")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bytes, err := fingerprint.Marshal(newRecord("task"))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(dir, "20260101000000000-h.pb"),
		filepath.Join(dir, "20260601000000000-h.pb"),
	}
	for _, p := range paths {
		if err := os.WriteFile(p, bytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, got err=%v", p, err)
		}
	}
}

func TestDelete_MissingKeyIsNoop(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()
	if err := st.Delete(ctx, fingerprint.Key{SpecRelpath: "s", TaskID: "t", InputHash: "h"}); err != nil {
		t.Errorf("Delete on missing should be noop, got %v", err)
	}
}

func TestList_AllAndFiltered(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	keys := []fingerprint.Key{
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"},
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h2"},
		{SpecRelpath: "spec/b", TaskID: "other", InputHash: "h3"},
	}
	for _, k := range keys {
		if err := st.Save(ctx, k, newRecord(k.TaskID)); err != nil {
			t.Fatal(err)
		}
	}

	all, err := st.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 keys, got %d: %+v", len(all), all)
	}

	bySpec, err := st.List(ctx, fingerprint.ListFilter{SpecRelpath: "spec/a"})
	if err != nil {
		t.Fatalf("List bySpec: %v", err)
	}
	if len(bySpec) != 2 {
		t.Errorf("expected 2 keys for spec/a, got %d: %+v", len(bySpec), bySpec)
	}

	byTask, err := st.List(ctx, fingerprint.ListFilter{TaskID: "other"})
	if err != nil {
		t.Fatalf("List byTask: %v", err)
	}
	if len(byTask) != 1 || byTask[0].InputHash != "h3" {
		t.Errorf("expected single h3, got %+v", byTask)
	}
}

func TestList_DedupesPostMergeDuplicates(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bytes, err := fingerprint.Marshal(newRecord("gen"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"20260101000000000-h.pb", "20260601000000000-h.pb"} {
		if err := os.WriteFile(filepath.Join(dir, name), bytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != key {
		t.Errorf("expected single dedupe entry, got %+v", got)
	}
}

func TestList_OlderThan(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	old := fingerprint.Key{SpecRelpath: "s", TaskID: "t", InputHash: "old"}
	if err := st.Save(ctx, old, newRecord("t")); err != nil {
		t.Fatal(err)
	}
	// Backdate the on-disk mtime to something definitively older.
	oldFile := filepath.Join(root, ".sloff", "fingerprints", "s", "t", "20260505120000000-old.pb")
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatal(err)
	}

	// new record after the cutoff
	newKey := fingerprint.Key{SpecRelpath: "s", TaskID: "t", InputHash: "new"}
	if err := st.Save(ctx, newKey, newRecord("t")); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-1 * time.Hour)
	got, err := st.List(ctx, fingerprint.ListFilter{OlderThan: cutoff})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].InputHash != "old" {
		t.Errorf("expected only old, got %+v", got)
	}
}

func TestCollapseDuplicates(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bytes, err := fingerprint.Marshal(newRecord("gen"))
	if err != nil {
		t.Fatal(err)
	}
	files := []string{
		"20260101000000000-h.pb",
		"20260301000000000-h.pb",
		"20260601000000000-h.pb",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), bytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := st.CollapseDuplicates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("expected 2 removals, got %d", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, files[0])); err != nil {
		t.Errorf("expected earliest preserved, got err=%v", err)
	}
	for _, name := range files[1:] {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, got err=%v", name, err)
		}
	}
}

func TestName(t *testing.T) {
	if name := local.New("/tmp").Name(); name != "local" {
		t.Errorf("Name() = %q, want local", name)
	}
}

// TestNew_DefaultClockUsesNow exercises the default-clock branch of New
// (which the WithClock path otherwise displaces) so the new Storage stamps
// records with the wall clock when no Option is passed.
func TestNew_DefaultClockUsesNow(t *testing.T) {
	root := t.TempDir()
	st := local.New(root)
	ctx := context.Background()

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "deadbeef"}
	before := time.Now().UTC().Add(-time.Second)
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected single record, got entries=%v err=%v", entries, err)
	}
	name := entries[0].Name()
	if len(name) < 17 {
		t.Fatalf("filename too short: %q", name)
	}
	stamp, err := time.Parse("20060102150405", name[:14])
	if err != nil {
		t.Fatalf("parse prefix from %q: %v", name, err)
	}
	if stamp.Before(before) || stamp.After(after) {
		t.Errorf("default clock prefix %v outside [%v, %v]", stamp, before, after)
	}
}

// TestList_IgnoresForeignFiles guards the `splitFilename` filter: foreign
// files (no timestamp prefix, wrong extension, or hash-only legacy
// filenames from before ADR-0010) must be skipped by List rather than
// silently producing keys with bogus InputHash values.
func TestList_IgnoresForeignFiles(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bytes, err := fingerprint.Marshal(newRecord("gen"))
	if err != nil {
		t.Fatal(err)
	}
	// One legitimate record + an assortment of garbage that List must skip.
	if err := os.WriteFile(filepath.Join(dir, "20260505120000000-deadbeef.pb"), bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"deadbeef.pb",                   // legacy (pre ADR-0010) hash-only filename
		"notatimestamp-deadbeef.pb",     // dash present but prefix isn't all digits
		"abcdefghijklmnopq-deadbeef.pb", // prefix matches timestamp width but is not numeric
		"20260505120000000-stray.txt",   // wrong extension
		"20260505120000000-.pb",         // empty hash
		"-20260505120000000deadbeef.pb", // leading dash
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("noise"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InputHash != "deadbeef" {
		t.Errorf("expected single entry for the well-formed file, got %+v", got)
	}
}

// TestCollapseDuplicates_RespectsCtx covers the ctx.Err() short-circuit at
// the top of CollapseDuplicates' loop body. A pre-cancelled context must
// return the cancellation error before any file is removed, so a long-
// running gc invoked from a CI job that gets cancelled mid-run does not
// leave half-collapsed state on disk.
func TestCollapseDuplicates_RespectsCtx(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)

	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bytes, err := fingerprint.Marshal(newRecord("gen"))
	if err != nil {
		t.Fatal(err)
	}
	files := []string{
		"20260101000000000-h.pb",
		"20260601000000000-h.pb",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), bytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.CollapseDuplicates(ctx); err == nil {
		t.Error("expected cancelled ctx to surface error")
	}
	// Both files should still be on disk because the loop bailed before
	// attempting any os.Remove.
	for _, name := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s preserved after cancelled gc, got err=%v", name, err)
		}
	}
}

// TestSave_ErrorOnUnwritableDir covers the os.MkdirAll error branch of
// Save: a parent path that already exists as a regular file makes MkdirAll
// fail, and Save must surface the error rather than overwrite or silently
// continue.
func TestSave_ErrorOnUnwritableDir(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	// Create a file at the path where Save would otherwise create a
	// directory; MkdirAll then fails because a non-dir occupies the path.
	fingerprintRoot := filepath.Join(root, ".sloff", "fingerprints", "spec")
	if err := os.MkdirAll(fingerprintRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fingerprintRoot, "task"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "task", InputHash: "h"}
	if err := st.Save(ctx, key, newRecord("task")); err == nil {
		t.Error("expected Save to fail when parent path is occupied by a regular file")
	}
}

// TestLoad_PropagatesUnmarshalError covers the corrupt-on-disk branch of
// Load: a `<timestamp>-<hash>.pb` whose contents cannot be decoded must
// surface the decode error so the runner reports it instead of treating the
// file as a miss and silently regenerating.
func TestLoad_PropagatesUnmarshalError(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	dir := filepath.Join(root, ".sloff", "fingerprints", "spec", "gen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20260505120000000-h.pb"), []byte("not a proto"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := st.Load(ctx, fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"})
	if err == nil {
		t.Error("expected Load to surface decode error for corrupt record")
	}
}

func TestLoadMany(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	keys := []fingerprint.Key{
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"},
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h2"},
		{SpecRelpath: "spec/b", TaskID: "other", InputHash: "h3"},
	}
	for _, k := range keys {
		if err := st.Save(ctx, k, newRecord(k.TaskID)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.LoadMany(ctx, append(keys, fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "missing"}))
	if err != nil {
		t.Fatalf("LoadMany: %v", err)
	}
	if len(got) != len(keys) {
		t.Errorf("expected %d records (missing key excluded), got %d", len(keys), len(got))
	}
	for _, k := range keys {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %+v in LoadMany result", k)
		}
	}
}

func TestSaveMany(t *testing.T) {
	root := t.TempDir()
	st := newStorage(root, fixedClock)
	ctx := context.Background()

	items := []fingerprint.KeyRecord{
		{Key: fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}, Record: newRecord("gen")},
		{Key: fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h2"}, Record: newRecord("gen")},
		{Key: fingerprint.Key{SpecRelpath: "spec/b", TaskID: "other", InputHash: "h3"}, Record: newRecord("other")},
	}
	if err := st.SaveMany(ctx, items); err != nil {
		t.Fatalf("SaveMany: %v", err)
	}
	for _, it := range items {
		if _, ok, _ := st.Load(ctx, it.Key); !ok {
			t.Errorf("expected hit for %+v after SaveMany", it.Key)
		}
	}
}

func TestLoadMany_EmptyKeys(t *testing.T) {
	st := newStorage(t.TempDir(), fixedClock)
	got, err := st.LoadMany(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMany on empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d entries", len(got))
	}
}

func TestSaveMany_EmptyItems(t *testing.T) {
	st := newStorage(t.TempDir(), fixedClock)
	if err := st.SaveMany(context.Background(), nil); err != nil {
		t.Errorf("SaveMany on empty should be noop, got %v", err)
	}
}
