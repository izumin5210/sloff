package hash

import (
	"os"
	"path/filepath"
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

func TestFileCache_MissingFile(t *testing.T) {
	root := t.TempDir()
	c := NewFileCache()
	if _, err := c.Files(root, []string{"missing.txt"}); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// ageFile pushes a file's mtime/atime well into the past so the racy guard
// (which drops just-modified entries) keeps it on Save. ctime updates to "now"
// as a side effect of the chtimes call, which is fine for these tests.
func ageFile(t *testing.T, full string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatal(err)
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
	if err := c1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

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
	if err := c1.Save(); err != nil {
		t.Fatal(err)
	}

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
	if err := c1.Save(); err != nil {
		t.Fatal(err)
	}

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

// TestFileCache_RacyEntryNotPersisted: a file modified within racyMargin of the
// Save must not be persisted (its mtime tick may not be settled), while a
// settled file is.
func TestFileCache_RacyEntryNotPersisted(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "stable.txt", "s")
	writeFile(t, root, "recent.txt", "r")
	stable := filepath.Join(root, "stable.txt")
	recent := filepath.Join(root, "recent.txt")
	ageFile(t, stable) // settled; recent keeps its just-written (now) mtime

	cachePath := filepath.Join(t.TempDir(), "fh.pb")
	c1 := NewPersistentFileCache(cachePath)
	if _, err := c1.Files(root, []string{"stable.txt", "recent.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := c1.Save(); err != nil {
		t.Fatal(err)
	}

	c2 := NewPersistentFileCache(cachePath)
	if _, ok := c2.entries[stable]; !ok {
		t.Error("settled entry should be persisted")
	}
	if _, ok := c2.entries[recent]; ok {
		t.Error("entry modified within racyMargin must not be persisted")
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
