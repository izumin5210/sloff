package local_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/cache"
	"github.com/izumin5210/sloff/internal/sloff/cache/local"
)

func newRecord(taskID string) *cache.Record {
	return &cache.Record{
		GeneratedAt:   time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC),
		Input:         cache.Input{Hash: "deadbeef"},
		Output:        cache.Output{Hash: "cafebabe"},
		SchemaVersion: cache.SchemaVersion,
		Spec: cache.RecordSpec{
			Cmd:    "echo hi",
			Dir:    "path/to/spec",
			TaskID: taskID,
		},
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	root := t.TempDir()
	st := local.New(root)
	ctx := context.Background()

	key := cache.Key{SpecRelpath: "path/to/spec", TaskID: "gen", InputHash: "deadbeef"}
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
	if diff := cmp.Diff(rec, got); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestLoad_MissReturnsFalse(t *testing.T) {
	root := t.TempDir()
	st := local.New(root)
	ctx := context.Background()

	got, ok, err := st.Load(ctx, cache.Key{SpecRelpath: "x", TaskID: "y", InputHash: "z"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ok || got != nil {
		t.Errorf("expected miss, got ok=%v rec=%v", ok, got)
	}
}

func TestSave_PreservesSpecRelpathHierarchy(t *testing.T) {
	root := t.TempDir()
	st := local.New(root)
	ctx := context.Background()

	key := cache.Key{SpecRelpath: "path/to/spec", TaskID: "gen", InputHash: "abc123"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Deviation from architecture.md: we keep the spec dir hierarchy verbatim instead of
	// flattening with "_". A "_" substitution would lose information on List for spec
	// dirs whose names contain underscores.
	want := filepath.Join(root, ".sloff", "cache", "path", "to", "spec", "gen", "abc123.pb")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected record at %s, got err=%v", want, err)
	}
}

func TestDelete_RemovesFile(t *testing.T) {
	root := t.TempDir()
	st := local.New(root)
	ctx := context.Background()

	key := cache.Key{SpecRelpath: "spec", TaskID: "task", InputHash: "h"}
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

func TestDelete_MissingKeyIsNoop(t *testing.T) {
	root := t.TempDir()
	st := local.New(root)
	ctx := context.Background()
	if err := st.Delete(ctx, cache.Key{SpecRelpath: "s", TaskID: "t", InputHash: "h"}); err != nil {
		t.Errorf("Delete on missing should be noop, got %v", err)
	}
}

func TestList_AllAndFiltered(t *testing.T) {
	root := t.TempDir()
	st := local.New(root)
	ctx := context.Background()

	keys := []cache.Key{
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"},
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h2"},
		{SpecRelpath: "spec/b", TaskID: "other", InputHash: "h3"},
	}
	for _, k := range keys {
		if err := st.Save(ctx, k, newRecord(k.TaskID)); err != nil {
			t.Fatal(err)
		}
	}

	all, err := st.List(ctx, cache.ListFilter{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 keys, got %d: %+v", len(all), all)
	}

	bySpec, err := st.List(ctx, cache.ListFilter{SpecRelpath: "spec/a"})
	if err != nil {
		t.Fatalf("List bySpec: %v", err)
	}
	if len(bySpec) != 2 {
		t.Errorf("expected 2 keys for spec/a, got %d: %+v", len(bySpec), bySpec)
	}

	byTask, err := st.List(ctx, cache.ListFilter{TaskID: "other"})
	if err != nil {
		t.Fatalf("List byTask: %v", err)
	}
	if len(byTask) != 1 || byTask[0].InputHash != "h3" {
		t.Errorf("expected single h3, got %+v", byTask)
	}
}

func TestList_OlderThan(t *testing.T) {
	root := t.TempDir()
	st := local.New(root)
	ctx := context.Background()

	old := cache.Key{SpecRelpath: "s", TaskID: "t", InputHash: "old"}
	if err := st.Save(ctx, old, newRecord("t")); err != nil {
		t.Fatal(err)
	}
	// Backdate the on-disk mtime to something definitively older.
	oldFile := filepath.Join(root, ".sloff", "cache", "s", "t", "old.pb")
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatal(err)
	}

	// new record after the cutoff
	newKey := cache.Key{SpecRelpath: "s", TaskID: "t", InputHash: "new"}
	if err := st.Save(ctx, newKey, newRecord("t")); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-1 * time.Hour)
	got, err := st.List(ctx, cache.ListFilter{OlderThan: cutoff})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].InputHash != "old" {
		t.Errorf("expected only old, got %+v", got)
	}
}

func TestName(t *testing.T) {
	if name := local.New("/tmp").Name(); name != "local" {
		t.Errorf("Name() = %q, want local", name)
	}
}
