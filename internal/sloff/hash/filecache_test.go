package hash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
