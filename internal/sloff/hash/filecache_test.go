package hash

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	filecachev1 "github.com/izumin5210/sloff/internal/proto/sloff/filecache/v1"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFileCache_FilesMatchesUncached(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha")
	writeFile(t, root, "sub/b.txt", "beta")

	paths := []string{"a.txt", filepath.Join("sub", "b.txt")}

	want, err := Files(root, paths)
	if err != nil {
		t.Fatal(err)
	}

	c := NewFileCache()
	got, err := c.Files(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("cached digest %q != uncached %q", got, want)
	}

	// Second call is served from cache and must stay identical.
	got2, err := c.Files(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if got2 != want {
		t.Fatalf("second cached digest %q != uncached %q", got2, want)
	}
}

func TestFileCache_DetectsRewrite(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha")
	paths := []string{"a.txt"}

	c := NewFileCache()
	before, err := c.Files(root, paths)
	if err != nil {
		t.Fatal(err)
	}

	// Rewrite with different content; ensure the mtime moves even on coarse
	// filesystems so the cache invalidation path is exercised deterministically.
	writeFile(t, root, "a.txt", "ALPHA")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), future, future); err != nil {
		t.Fatal(err)
	}

	after, err := c.Files(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatalf("digest unchanged after rewrite: %q", after)
	}

	want, err := Files(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if after != want {
		t.Fatalf("cached digest %q != uncached %q after rewrite", after, want)
	}
}

func TestFileCache_FileHexMatchesFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha")

	want, err := File(root, "a.txt")
	if err != nil {
		t.Fatal(err)
	}

	c := NewFileCache()
	got, err := c.FileHex(root, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("FileHex %q != File %q", got, want)
	}
}

// TestFileCache_FilesAndDigestsMatchesFiles: the folded digest equals Files,
// and the per-file entries carry each path's standalone hex digest in path
// order — the single-pass replacement for Files + a separate per-file pass.
func TestFileCache_FilesAndDigestsMatchesFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha")
	writeFile(t, root, "sub/b.txt", "beta")
	paths := []string{"a.txt", filepath.Join("sub", "b.txt")}

	c := NewFileCache()
	wantFolded, err := c.Files(root, paths)
	if err != nil {
		t.Fatal(err)
	}

	folded, entries, err := c.FilesAndDigests(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	if folded != wantFolded {
		t.Fatalf("folded digest %q != Files %q", folded, wantFolded)
	}
	if len(entries) != len(paths) {
		t.Fatalf("got %d entries, want %d", len(entries), len(paths))
	}
	for i, p := range paths {
		if entries[i].Path != p {
			t.Errorf("entry %d path = %q, want %q", i, entries[i].Path, p)
		}
		want, err := File(root, p)
		if err != nil {
			t.Fatal(err)
		}
		if entries[i].Hex != want {
			t.Errorf("entry %d hex = %q, want %q", i, entries[i].Hex, want)
		}
	}
}

func TestFileCache_MissingFile(t *testing.T) {
	root := t.TempDir()
	c := NewFileCache()
	if _, err := c.Files(root, []string{"missing.txt"}); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestFileCache_MissingFileErrorIsErrNotExist pins that Files surfaces the
// missing-file condition through errors.Is(err, fs.ErrNotExist) — the
// runner's prefetch (ADR-0019 D6) relies on this to distinguish "input not
// generated yet" (skip the task's prefetch) from a hard I/O error (fatal).
// A lossy wrap anywhere in the digest chain would silently turn the skip
// back into a run-aborting failure on cold trees.
func TestFileCache_MissingFileErrorIsErrNotExist(t *testing.T) {
	root := t.TempDir()
	c := NewFileCache()
	_, err := c.Files(root, []string{"missing.txt"})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected errors.Is(err, fs.ErrNotExist), got %v", err)
	}
}

// TestFileCache_MixedErrors_PermissionWinsOverNotExist verifies that when input
// files include both a missing file (fs.ErrNotExist) and an unreadable file
// (permission error), the permission error is returned — not the ErrNotExist.
// The runner's prefetch (ADR-0019 D6) excludes a task from prefetch only when
// ALL its input-hashing failures are ErrNotExist; any non-ErrNotExist error
// must win deterministically so a permission problem is never silently swallowed
// and deferred to mid-run.
func TestFileCache_MixedErrors_PermissionWinsOverNotExist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o000 is not enforced on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root: chmod 0o000 does not prevent reads")
	}

	root := t.TempDir()
	// unreadable.txt exists but cannot be read.
	unreadable := filepath.Join(root, "unreadable.txt")
	if err := os.WriteFile(unreadable, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(unreadable, 0o644) }) //nolint:errcheck

	c := NewFileCache()
	// Paths: [missing file, unreadable file]. Both will fail, but the permission
	// error must be returned rather than the ErrNotExist.
	_, err := c.Files(root, []string{"missing.txt", "unreadable.txt"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected a non-ErrNotExist error (permission denied), got ErrNotExist: %v", err)
	}
}

// ageFile pushes a file's mtime/atime well into the past so the mtime racy
// guard keeps it. ctime still moves to "now" (userspace can't backdate it), so
// tests that need the entry actually persisted must Save via saveSettled —
// otherwise the ctime racy guard drops it.
func ageFile(t *testing.T, full string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatal(err)
	}
}

// saveSettled persists as if the run happened well after the test's files
// settled. A freshly written file carries a "now" ctime that the racy guard
// would drop; advancing Save's reference time past racyMargin treats both
// timestamps as settled, so warm-reuse assertions see a populated cache. Plain
// Save() (real clock) is exercised by TestFileCache_SaveDropsRacyEntries.
func saveSettled(t *testing.T, c *FileCache) {
	t.Helper()
	if err := c.saveAt(time.Now().Add(2 * racyMargin)); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestFileCache_PersistsAcrossInstances is the warm-run contract (ADR-0014): a
// digest computed in one cache instance is reused by a fresh instance loaded
// from the same path, without rehashing.
func TestFileCache_PersistsAcrossInstances(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha")
	full := filepath.Join(root, "a.txt")
	ageFile(t, full)
	cachePath := filepath.Join(t.TempDir(), "fh.pb")

	c1 := NewPersistentFileCache(cachePath)
	want, err := c1.Files(root, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	saveSettled(t, c1)

	c2 := NewPersistentFileCache(cachePath)
	if _, ok := c2.entries[full]; !ok {
		t.Fatalf("entry for %s not loaded from persistent cache", full)
	}
	got, err := c2.Files(root, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("reloaded digest %q != original %q", got, want)
	}
}

// TestFileCache_PersistContentChangeNotStale: a content change (which advances
// mtime) must invalidate the persisted entry.
func TestFileCache_PersistContentChangeNotStale(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha")
	full := filepath.Join(root, "a.txt")
	ageFile(t, full)
	cachePath := filepath.Join(t.TempDir(), "fh.pb")

	c1 := NewPersistentFileCache(cachePath)
	stale, _ := c1.Files(root, []string{"a.txt"})
	saveSettled(t, c1)

	writeFile(t, root, "a.txt", "alphaX") // different size + fresh mtime
	c2 := NewPersistentFileCache(cachePath)
	got, err := c2.Files(root, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got == stale {
		t.Fatal("persisted cache served a stale digest after content change")
	}
	want, _ := Files(root, []string{"a.txt"})
	if got != want {
		t.Fatalf("digest %q != uncached %q", got, want)
	}
}

// TestFileCache_CtimeInvalidatesMtimePreservedRewrite is the core hardening
// guarantee of ADR-0014: a same-size content rewrite whose mtime is reset to
// the old value (as rsync --times / tar / cp -p / restore can do) is still
// detected because ctime advances. A (size, mtime)-only cache would serve a
// stale digest here — a silent wrong SKIP.
func TestFileCache_CtimeInvalidatesMtimePreservedRewrite(t *testing.T) {
	root := t.TempDir()
	full := filepath.Join(root, "a.txt")
	writeFile(t, root, "a.txt", "AAAA")
	ageFile(t, full)

	fi, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := fileIdentity(fi); !ok {
		t.Skip("platform without ctime/inode identity; cache degrades to (size, mtime)")
	}
	oldMtime := fi.ModTime()

	cachePath := filepath.Join(t.TempDir(), "fh.pb")
	c1 := NewPersistentFileCache(cachePath)
	stale, _ := c1.Files(root, []string{"a.txt"})
	saveSettled(t, c1)

	// Same size, mtime reset to the cached value, but content differs. chtimes
	// cannot rewind ctime, so ctime now differs from the cached entry.
	if err := os.WriteFile(full, []byte("BBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, oldMtime, oldMtime); err != nil {
		t.Fatal(err)
	}

	c2 := NewPersistentFileCache(cachePath)
	got, err := c2.Files(root, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got == stale {
		t.Fatal("mtime-preserved rewrite served stale digest: ctime hardening not effective")
	}
	want, _ := Files(root, []string{"a.txt"})
	if got != want {
		t.Fatalf("digest %q != uncached %q", got, want)
	}
}

// TestFileCache_SaveDropsRacyEntries pins the racy-window contract directly on
// the persisted set, building entries by hand so each timestamp axis is
// controlled independently (a real file's ctime can't be backdated from
// userspace). An entry is racy — and must be dropped — when EITHER its mtime or
// (on identity-bearing platforms) its ctime is within racyMargin of the Save:
// mtime catches ordinary edits, ctime catches mtime-preserving rewrites
// (rsync --times / tar / cp -p) where ctime is the only fresh stamp.
func TestFileCache_SaveDropsRacyEntries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	old := now.Add(-time.Hour)
	cachePath := filepath.Join(t.TempDir(), "fh.pb")

	c := NewPersistentFileCache(cachePath)
	c.entries = map[string]fileCacheEntry{
		"/settled":     {size: 1, mtime: old, ctime: old.UnixNano(), inode: 1, idOK: true, digest: []byte("s")},
		"/racy-mtime":  {size: 1, mtime: now, ctime: old.UnixNano(), inode: 2, idOK: true, digest: []byte("m")},
		"/racy-ctime":  {size: 1, mtime: old, ctime: now.UnixNano(), inode: 3, idOK: true, digest: []byte("c")},
		"/no-identity": {size: 1, mtime: old, ctime: now.UnixNano(), inode: 0, idOK: false, digest: []byte("n")},
	}
	if err := c.saveAt(now); err != nil {
		t.Fatalf("saveAt: %v", err)
	}

	c2 := NewPersistentFileCache(cachePath)
	for _, tc := range []struct {
		path      string
		persisted bool
		why       string
	}{
		{"/settled", true, "both timestamps older than racyMargin"},
		{"/racy-mtime", false, "mtime within racyMargin"},
		{"/racy-ctime", false, "ctime within racyMargin (mtime-preserved rewrite window)"},
		{"/no-identity", true, "idOK=false ignores ctime; mtime is settled"},
	} {
		if _, ok := c2.entries[tc.path]; ok != tc.persisted {
			t.Errorf("%s: persisted=%v, want %v (%s)", tc.path, ok, tc.persisted, tc.why)
		}
	}
}

// TestFileCache_RacyCheckAnchoredToRunStart guards against measuring the racy
// window from the (deferred) Save time. A long run started while a file's
// stamps were still in the racy window must drop that entry even though Save
// happens many seconds later — anchoring to Save time would persist a digest a
// same-tick rewrite has already made stale.
func TestFileCache_RacyCheckAnchoredToRunStart(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "fh.pb")
	c := NewPersistentFileCache(cachePath)
	// Run started an hour ago, in the same tick as the file's stamps; Save runs
	// now (real clock), far outside racyMargin from those stamps.
	start := time.Now().Add(-time.Hour)
	c.startedAt = start
	c.entries = map[string]fileCacheEntry{
		"/x": {size: 1, mtime: start, ctime: start.UnixNano(), inode: 1, idOK: true, digest: []byte("d")},
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	c2 := NewPersistentFileCache(cachePath)
	if _, ok := c2.entries["/x"]; ok {
		t.Error("entry observed within racyMargin of run start must not persist, even when Save is deferred past the window")
	}
}

// TestFileCache_IgnoresVersionMismatch: a cache written with an incompatible
// schema version is ignored wholesale (cold), never returning incompatible
// digests.
func TestFileCache_IgnoresVersionMismatch(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "fh.pb")
	b, err := proto.Marshal(&filecachev1.Cache{
		SchemaVersion: filecachev1.SchemaVersion(99), // unsupported / future
		Entries:       []*filecachev1.Entry{{Path: "/x", Digest: []byte("d")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewPersistentFileCache(cachePath)
	if len(c.entries) != 0 {
		t.Fatalf("version mismatch should yield an empty cache, got %d entries", len(c.entries))
	}
}
