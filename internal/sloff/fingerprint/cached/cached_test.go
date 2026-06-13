package cached_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint/cached"
)

// memStorage is a minimal fingerprint.Storage used to drive the cache
// decorator from tests. It intentionally records every Load / Save call so
// each test can assert "this read was served from cache, not from inner".
type memStorage struct {
	mu       sync.Mutex
	records  map[fingerprint.Key]*fingerprintv1.Record
	loadHits []fingerprint.Key
	saveHits []fingerprint.Key
	loadMany int
	saveMany int

	loadErr error
	saveErr error
}

func newMem() *memStorage {
	return &memStorage{records: map[fingerprint.Key]*fingerprintv1.Record{}}
}

func (m *memStorage) Name() string { return "mem" }

func (m *memStorage) Load(_ context.Context, key fingerprint.Key) (*fingerprintv1.Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadHits = append(m.loadHits, key)
	if m.loadErr != nil {
		return nil, false, m.loadErr
	}
	rec, ok := m.records[key]
	return rec, ok, nil
}

func (m *memStorage) Save(_ context.Context, key fingerprint.Key, rec *fingerprintv1.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveHits = append(m.saveHits, key)
	if m.saveErr != nil {
		return m.saveErr
	}
	m.records[key] = rec
	return nil
}

func (m *memStorage) Delete(_ context.Context, key fingerprint.Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.records, key)
	return nil
}

func (m *memStorage) List(_ context.Context, _ fingerprint.ListFilter) ([]fingerprint.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]fingerprint.Key, 0, len(m.records))
	for k := range m.records {
		out = append(out, k)
	}
	return out, nil
}

func (m *memStorage) CollapseDuplicates(context.Context) (int, error) { return 0, nil }

func (m *memStorage) LoadMany(_ context.Context, keys []fingerprint.Key) (map[fingerprint.Key]*fingerprintv1.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadMany++
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	out := make(map[fingerprint.Key]*fingerprintv1.Record, len(keys))
	for _, k := range keys {
		if rec, ok := m.records[k]; ok {
			out[k] = rec
		}
	}
	return out, nil
}

func (m *memStorage) SaveMany(_ context.Context, items []fingerprint.KeyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveMany++
	if m.saveErr != nil {
		return m.saveErr
	}
	for _, it := range items {
		m.records[it.Key] = it.Record
	}
	return nil
}

func newRecord(taskID string) *fingerprintv1.Record {
	return &fingerprintv1.Record{
		Input:         &fingerprintv1.Input{Hash: "deadbeef"},
		Output:        &fingerprintv1.Output{Hash: "cafebabe"},
		SchemaVersion: fingerprint.SchemaVersion,
		Spec: &fingerprintv1.Spec{
			Cmd:    "echo",
			Dir:    "spec/a",
			TaskId: taskID,
		},
	}
}

func newCached(t *testing.T) (*cached.Storage, *memStorage, string) {
	t.Helper()
	mem := newMem()
	dir := t.TempDir()
	return cached.New(mem, dir), mem, dir
}

// warmableMem is a memStorage that also implements the optional Warm hook, so
// the decorator's forwarding can be asserted.
type warmableMem struct {
	*memStorage
	warmCalls int
}

func (w *warmableMem) Warm(context.Context) error {
	w.warmCalls++
	return nil
}

// TestWarm_ForwardsToInnerWhenSupported locks the contract that warming the
// decorator front-loads the inner backend's setup (e.g. DynamoDB credential
// resolution) — the whole point of the run-start warm-up.
func TestWarm_ForwardsToInnerWhenSupported(t *testing.T) {
	w := &warmableMem{memStorage: newMem()}
	s := cached.New(w, t.TempDir())
	if err := s.Warm(context.Background()); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if w.warmCalls != 1 {
		t.Errorf("inner.Warm calls = %d, want 1", w.warmCalls)
	}
}

// TestWarm_NoopWhenInnerLacksWarm guards that backends without a Warm hook
// (e.g. the local disk backend) don't break the run-start warm-up.
func TestWarm_NoopWhenInnerLacksWarm(t *testing.T) {
	s, _, _ := newCached(t) // inner is a plain memStorage (no Warm)
	if err := s.Warm(context.Background()); err != nil {
		t.Errorf("Warm should no-op when inner lacks Warm, got %v", err)
	}
}

func TestSave_WritesThroughToCache(t *testing.T) {
	st, mem, dir := newCached(t)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}
	rec := newRecord("gen")

	if err := st.Save(ctx, key, rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := mem.records[key]; got == nil {
		t.Fatal("inner did not receive Save")
	}
	cachePath := filepath.Join(dir, "spec/a", "gen", "h1.pb")
	if _, err := os.Stat(cachePath); err != nil {
		t.Errorf("expected cache file at %s, got err=%v", cachePath, err)
	}
}

func TestLoad_CacheHitSkipsInner(t *testing.T) {
	st, mem, _ := newCached(t)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}

	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatal(err)
	}
	mem.loadHits = nil

	got, ok, err := st.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got == nil {
		t.Fatal("expected hit on cache")
	}
	if len(mem.loadHits) != 0 {
		t.Errorf("inner.Load was called despite cache hit; hits=%v", mem.loadHits)
	}
}

func TestLoad_CacheMissPopulatesCacheFromInner(t *testing.T) {
	st, mem, dir := newCached(t)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}
	rec := newRecord("gen")
	mem.records[key] = rec

	got, ok, err := st.Load(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Load (first call): ok=%v err=%v", ok, err)
	}
	if diff := cmp.Diff(rec, got, protocmp.Transform()); diff != "" {
		t.Errorf("returned record differs from inner (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(filepath.Join(dir, "spec/a", "gen", "h1.pb")); err != nil {
		t.Errorf("expected cache populated, got err=%v", err)
	}

	// Second call must come from cache.
	mem.loadHits = nil
	if _, ok, err := st.Load(ctx, key); err != nil || !ok {
		t.Fatalf("Load (second call): ok=%v err=%v", ok, err)
	}
	if len(mem.loadHits) != 0 {
		t.Errorf("expected second Load to skip inner, hits=%v", mem.loadHits)
	}
}

func TestLoad_CorruptCacheTreatedAsMiss(t *testing.T) {
	st, mem, dir := newCached(t)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}
	rec := newRecord("gen")
	mem.records[key] = rec

	cachePath := filepath.Join(dir, "spec/a", "gen", "h1.pb")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok, err := st.Load(ctx, key)
	if err != nil || !ok {
		t.Fatalf("Load on corrupt cache: ok=%v err=%v", ok, err)
	}
	if diff := cmp.Diff(rec, got, protocmp.Transform()); diff != "" {
		t.Errorf("returned record differs from inner (-want +got):\n%s", diff)
	}
	if len(mem.loadHits) != 1 {
		t.Errorf("expected exactly one inner.Load on corrupt cache, got %d", len(mem.loadHits))
	}
}

func TestLoadMany_PartialCacheHit(t *testing.T) {
	st, mem, _ := newCached(t)
	ctx := context.Background()
	cached1 := fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}
	cached2 := fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h2"}
	remote := fingerprint.Key{SpecRelpath: "spec/b", TaskID: "other", InputHash: "h3"}
	missing := fingerprint.Key{SpecRelpath: "spec/c", TaskID: "none", InputHash: "h4"}

	for _, k := range []fingerprint.Key{cached1, cached2} {
		if err := st.Save(ctx, k, newRecord(k.TaskID)); err != nil {
			t.Fatal(err)
		}
	}
	mem.records[remote] = newRecord(remote.TaskID)
	mem.loadMany = 0
	mem.loadHits = nil

	got, err := st.LoadMany(ctx, []fingerprint.Key{cached1, cached2, remote, missing})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 records (missing key dropped), got %d", len(got))
	}
	for _, k := range []fingerprint.Key{cached1, cached2, remote} {
		if _, ok := got[k]; !ok {
			t.Errorf("expected %+v in result", k)
		}
	}
	if mem.loadMany != 1 {
		t.Errorf("expected exactly one inner.LoadMany, got %d", mem.loadMany)
	}
}

func TestLoadMany_AllCacheHitSkipsInner(t *testing.T) {
	st, mem, _ := newCached(t)
	ctx := context.Background()
	keys := []fingerprint.Key{
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"},
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h2"},
	}
	for _, k := range keys {
		if err := st.Save(ctx, k, newRecord(k.TaskID)); err != nil {
			t.Fatal(err)
		}
	}
	mem.loadMany = 0

	got, err := st.LoadMany(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(keys) {
		t.Errorf("expected %d cache hits, got %d", len(keys), len(got))
	}
	if mem.loadMany != 0 {
		t.Errorf("inner.LoadMany was called despite full cache hit, calls=%d", mem.loadMany)
	}
}

func TestLoadMany_InnerErrorPropagates(t *testing.T) {
	st, mem, _ := newCached(t)
	mem.loadErr = errors.New("boom")
	_, err := st.LoadMany(context.Background(), []fingerprint.Key{{SpecRelpath: "x", TaskID: "y", InputHash: "z"}})
	if err == nil {
		t.Fatal("expected inner LoadMany error to surface")
	}
}

func TestSaveMany_WritesThroughToCache(t *testing.T) {
	st, mem, dir := newCached(t)
	ctx := context.Background()
	items := []fingerprint.KeyRecord{
		{Key: fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}, Record: newRecord("gen")},
		{Key: fingerprint.Key{SpecRelpath: "spec/b", TaskID: "other", InputHash: "h2"}, Record: newRecord("other")},
	}

	if err := st.SaveMany(ctx, items); err != nil {
		t.Fatalf("SaveMany: %v", err)
	}
	if mem.saveMany != 1 {
		t.Errorf("expected exactly one inner.SaveMany, got %d", mem.saveMany)
	}
	for _, it := range items {
		path := filepath.Join(dir, filepath.FromSlash(it.Key.SpecRelpath), it.Key.TaskID, it.Key.InputHash+fingerprint.FileExt)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected cache file at %s, got err=%v", path, err)
		}
	}
}

func TestSaveMany_InnerErrorSkipsCache(t *testing.T) {
	st, mem, dir := newCached(t)
	mem.saveErr = errors.New("inner failed")
	items := []fingerprint.KeyRecord{
		{Key: fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}, Record: newRecord("gen")},
	}
	if err := st.SaveMany(context.Background(), items); err == nil {
		t.Fatal("expected inner SaveMany error to surface")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("expected cache untouched on inner failure, entries=%v", entries)
	}
}

func TestDelete_RemovesFromBoth(t *testing.T) {
	st, mem, dir := newCached(t)
	ctx := context.Background()
	key := fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}
	if err := st.Save(ctx, key, newRecord("gen")); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := mem.records[key]; ok {
		t.Error("inner record not deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "spec/a", "gen", "h1.pb")); !os.IsNotExist(err) {
		t.Errorf("expected cache file removed, got err=%v", err)
	}
}

func TestName_PassesInner(t *testing.T) {
	st, _, _ := newCached(t)
	if got := st.Name(); got != "mem" {
		t.Errorf("Name() = %q, want %q (inner backend's name)", got, "mem")
	}
}

func TestList_PassesThroughToInner(t *testing.T) {
	st, mem, _ := newCached(t)
	ctx := context.Background()
	keys := []fingerprint.Key{
		{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"},
		{SpecRelpath: "spec/b", TaskID: "other", InputHash: "h2"},
	}
	for _, k := range keys {
		mem.records[k] = newRecord(k.TaskID)
	}
	got, err := st.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(keys) {
		t.Errorf("expected %d keys, got %d: %+v", len(keys), len(got), got)
	}
}

func TestCollapseDuplicates_PassesThroughToInner(t *testing.T) {
	st, _, _ := newCached(t)
	got, err := st.CollapseDuplicates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("expected 0 (mem inner returns 0), got %d", got)
	}
}

func TestSave_InnerErrorSkipsCache(t *testing.T) {
	st, mem, dir := newCached(t)
	mem.saveErr = errors.New("inner failed")
	key := fingerprint.Key{SpecRelpath: "spec/a", TaskID: "gen", InputHash: "h1"}
	if err := st.Save(context.Background(), key, newRecord("gen")); err == nil {
		t.Fatal("expected inner Save error to surface")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("expected cache untouched on inner failure, entries=%v", entries)
	}
}

func TestDelete_InnerErrorPropagates(t *testing.T) {
	mem := newMem()
	mem.saveErr = nil
	innerErr := errors.New("delete failed")
	dir := t.TempDir()
	st := cached.New(failingDeleteStorage{memStorage: mem, err: innerErr}, dir)
	if err := st.Delete(context.Background(), fingerprint.Key{SpecRelpath: "x", TaskID: "y", InputHash: "z"}); !errors.Is(err, innerErr) {
		t.Errorf("expected inner Delete error to surface, got %v", err)
	}
}

// failingDeleteStorage forces Delete to surface a specific error so the
// decorator's pass-through behaviour on Delete failure is exercised
// without inventing yet another knob on memStorage.
type failingDeleteStorage struct {
	*memStorage
	err error
}

func (f failingDeleteStorage) Delete(_ context.Context, _ fingerprint.Key) error { return f.err }

// TestSave_CacheWriteFailureDoesNotMaskInnerSuccess covers the
// best-effort error swallowing in writeCacheBestEffort: when MkdirAll
// fails (a regular file blocks the directory path), Save still reports
// the inner success.
func TestSave_CacheWriteFailureDoesNotMaskInnerSuccess(t *testing.T) {
	dir := t.TempDir()
	// Drop a regular file at the path we'd otherwise want to create as a
	// directory; MkdirAll then fails with ENOTDIR and the cache write
	// silently skips.
	if err := os.WriteFile(filepath.Join(dir, "spec"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	mem := newMem()
	st := cached.New(mem, dir)
	key := fingerprint.Key{SpecRelpath: "spec/sub", TaskID: "gen", InputHash: "h"}
	if err := st.Save(context.Background(), key, newRecord("gen")); err != nil {
		t.Fatalf("Save should swallow cache write failure, got %v", err)
	}
	if mem.records[key] == nil {
		t.Error("inner record should still be persisted")
	}
}

// TestDelete_CacheRemoveFailureDoesNotMaskInnerSuccess covers the
// best-effort branch in removeCacheBestEffort. The cache dir contains
// a directory at the key path, which os.Remove cannot remove (it
// would need RemoveAll). We assert Delete still reports the inner
// result.
func TestDelete_CacheRemoveFailureDoesNotMaskInnerSuccess(t *testing.T) {
	dir := t.TempDir()
	mem := newMem()
	key := fingerprint.Key{SpecRelpath: "spec", TaskID: "gen", InputHash: "h"}
	mem.records[key] = newRecord("gen")
	// Pre-create a non-empty directory at the cache file's expected
	// path so os.Remove fails with ENOTEMPTY.
	target := filepath.Join(dir, "spec", "gen", "h.pb")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := cached.New(mem, dir)
	if err := st.Delete(context.Background(), key); err != nil {
		t.Fatalf("Delete should swallow cache remove failure, got %v", err)
	}
	if _, ok := mem.records[key]; ok {
		t.Error("inner record should be deleted")
	}
}
