package lister_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

func TestGlob_IncludesAndExcludes(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "cmd/foo/main.go", "package main\nfunc main() {}\n")
	mustWriteFile(t, root, "cmd/foo/util.go", "package main\n")
	mustWriteFile(t, root, "cmd/foo/util_test.go", "package main\n")
	mustWriteFile(t, root, "cmd/foo/sub/sub.go", "package sub\n")
	mustWriteFile(t, root, "cmd/foo/README.md", "ignore me")

	l := lister.NewGlob(root, []string{"**/*.go"}, []string{"**/*_test.go"})

	got, err := l.List(context.Background(), "./cmd/foo/...")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Listings use slash-form paths so the hash is identical across OS.
	want := lister.Listing{InternalFiles: []string{
		"cmd/foo/main.go",
		"cmd/foo/sub/sub.go",
		"cmd/foo/util.go",
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestGlob_NormalizesEntryShape(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "cmd/foo/main.go", "package main\n")

	l := lister.NewGlob(root, []string{"**/*.go"}, nil)
	for _, entry := range []string{"./cmd/foo", "./cmd/foo/", "./cmd/foo/..."} {
		got, err := l.List(context.Background(), entry)
		if err != nil {
			t.Fatalf("List(%q): %v", entry, err)
		}
		want := []string{"cmd/foo/main.go"}
		if diff := cmp.Diff(want, got.InternalFiles); diff != "" {
			t.Errorf("entry=%q mismatch (-want +got):\n%s", entry, diff)
		}
	}
}

func TestGlob_NeverReturnsExternalModules(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "cmd/foo/main.go", "package main\n")

	l := lister.NewGlob(root, []string{"**/*.go"}, nil)
	got, err := l.List(context.Background(), "./cmd/foo")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got.ExternalModules) != 0 {
		t.Errorf("ExternalModules should always be empty for globLister, got %v", got.ExternalModules)
	}
}

func TestGlob_RejectsAbsoluteOrEscapingEntry(t *testing.T) {
	root := t.TempDir()
	l := lister.NewGlob(root, []string{"**/*.go"}, nil)

	for _, entry := range []string{"cmd/foo", "/etc", "../escape"} {
		if _, err := l.List(context.Background(), entry); err == nil {
			t.Errorf("List(%q) succeeded; want error", entry)
		}
	}
}

func TestGlob_EmptyDirectoryReturnsNoFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cmd", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	l := lister.NewGlob(root, []string{"**/*.go"}, nil)
	got, err := l.List(context.Background(), "./cmd/empty")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got.InternalFiles) != 0 {
		t.Errorf("InternalFiles = %v, want empty", got.InternalFiles)
	}
}

func mustWriteFile(t *testing.T, root, relPath, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
