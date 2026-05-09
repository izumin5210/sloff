package hash_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/hash"
)

func TestFilesHash_Deterministic(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "alpha")
	mustWrite(t, filepath.Join(root, "b.txt"), "beta")

	h1, err := hash.Files(root, []string{"a.txt", "b.txt"})
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	h2, err := hash.Files(root, []string{"b.txt", "a.txt"}) // unsorted input
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if h1 != h2 {
		t.Errorf("Files should sort internally: %s vs %s", h1, h2)
	}
}

func TestFilesHash_ChangesOnContent(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "v1")

	h1, err := hash.Files(root, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "a.txt"), "v2")
	h2, err := hash.Files(root, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("Files should change when content changes")
	}
}

func TestFilesHash_ChangesOnPath(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "same")
	mustWrite(t, filepath.Join(root, "b.txt"), "same")

	h1, err := hash.Files(root, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := hash.Files(root, []string{"b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("Files should distinguish paths even when content is identical")
	}
}

func TestFilesHash_MissingFileErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := hash.Files(root, []string{"missing"}); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFilesHash_EmptyListIsStable(t *testing.T) {
	root := t.TempDir()
	h, err := hash.Files(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(sha256.New().Sum(nil))
	if h != want {
		t.Errorf("got %s, want %s (sha256 of empty)", h, want)
	}
}

func TestCmdHash_DistinguishesArgBoundary(t *testing.T) {
	a := hash.Cmd([]string{"foo", "bar", "baz"})
	b := hash.Cmd([]string{"foo bar", "baz"})
	if a == b {
		t.Errorf("Cmd must distinguish %v from %v", []string{"foo", "bar", "baz"}, []string{"foo bar", "baz"})
	}
}

func TestCmdHash_Deterministic(t *testing.T) {
	a := hash.Cmd([]string{"buf", "generate"})
	b := hash.Cmd([]string{"buf", "generate"})
	if a != b {
		t.Error("Cmd must be deterministic")
	}
}

func TestResolvedVersionsHash_SortedInternally(t *testing.T) {
	a := hash.ResolvedVersions([]string{"aqua:bufbuild/buf@v1.30.0", "go-external:google.golang.org/protobuf@v1.34.2"})
	b := hash.ResolvedVersions([]string{"go-external:google.golang.org/protobuf@v1.34.2", "aqua:bufbuild/buf@v1.30.0"})
	if a != b {
		t.Error("ResolvedVersions must sort versions internally")
	}
}

func TestResolvedVersionsHash_EmptyIsStable(t *testing.T) {
	a := hash.ResolvedVersions(nil)
	b := hash.ResolvedVersions([]string{})
	if a != b {
		t.Error("ResolvedVersions must produce same hash for nil and empty slice")
	}
	want := hex.EncodeToString(sha256.New().Sum(nil))
	if a != want {
		t.Errorf("got %s, want %s (sha256 of empty)", a, want)
	}
}

func TestInputHash_CombinesAllThree(t *testing.T) {
	a := hash.Input("aaa", "bbb", "ccc")
	b := hash.Input("aaa", "bbb", "ccd") // resolved versions differ
	if a == b {
		t.Error("Input must depend on resolved_versions_hash")
	}
	c := hash.Input("aaa", "bbc", "ccc") // cmd differs
	if a == c {
		t.Error("Input must depend on cmd_hash")
	}
	d := hash.Input("aab", "bbb", "ccc") // files differs
	if a == d {
		t.Error("Input must depend on files_hash")
	}
}

func TestInputHash_Deterministic(t *testing.T) {
	first := hash.Input("aaa", "bbb", "ccc")
	second := hash.Input("aaa", "bbb", "ccc")
	if first != second {
		t.Error("Input must be deterministic")
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
