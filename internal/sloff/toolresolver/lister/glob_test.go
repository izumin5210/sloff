package lister_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
)

func TestGlob_IncludesAndExcludes(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "cmd/foo/main.go", "package main\nfunc main() {}\n")
	mustWriteFile(t, root, "cmd/foo/util.go", "package main\n")
	mustWriteFile(t, root, "cmd/foo/util_test.go", "package main\n")
	mustWriteFile(t, root, "cmd/foo/sub/sub.go", "package sub\n")
	mustWriteFile(t, root, "cmd/foo/README.md", "ignore me")

	l := lister.NewGlob(root, []string{"**/*.go"}, []string{"**/*_test.go"})

	got, err := l.List(context.Background(), "", "./cmd/foo/...")
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
		got, err := l.List(context.Background(), "", entry)
		if err != nil {
			t.Fatalf("List(%q): %v", entry, err)
		}
		want := []string{"cmd/foo/main.go"}
		if diff := cmp.Diff(want, got.InternalFiles); diff != "" {
			t.Errorf("entry=%q mismatch (-want +got):\n%s", entry, diff)
		}
	}
}

// TestGlob_PrefixesSpecDirInResults guards that paths returned for a nested
// spec are recorded relative to the repo root, not the spec dir, so that
// downstream hashListing resolves them correctly.
func TestGlob_PrefixesSpecDirInResults(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "submodule/cmd/foo/main.go", "package main\n")

	l := lister.NewGlob(root, []string{"**/*.go"}, nil)
	got, err := l.List(context.Background(), "submodule", "./cmd/foo")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"submodule/cmd/foo/main.go"}
	if diff := cmp.Diff(want, got.InternalFiles); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestGlob_NeverReturnsExternalModules(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "cmd/foo/main.go", "package main\n")

	l := lister.NewGlob(root, []string{"**/*.go"}, nil)
	got, err := l.List(context.Background(), "", "./cmd/foo")
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

	// "cmd/foo" lacks a relative prefix; "/etc" is absolute; "../escape" from
	// the repo root resolves outside the repo. All three must error.
	for _, entry := range []string{"cmd/foo", "/etc", "../escape"} {
		if _, err := l.List(context.Background(), "", entry); err == nil {
			t.Errorf("List(%q) succeeded; want error", entry)
		}
	}
}

// TestGlob_AcceptsParentRelativeEntryWithinRepo covers nested specs that
// share a generator with a parent directory. The entry traverses up out of
// specDir but the final target still sits inside repoRoot, so the lister
// must accept it and emit repo-relative keys for downstream hashing.
func TestGlob_AcceptsParentRelativeEntryWithinRepo(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, "cmd/gen/main.go", "package main\n")

	l := lister.NewGlob(root, []string{"**/*.go"}, nil)
	got, err := l.List(context.Background(), filepath.Join("specs", "sub"), "../../cmd/gen")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"cmd/gen/main.go"}
	if diff := cmp.Diff(want, got.InternalFiles); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestGlob_RejectsParentRelativeEntryThatEscapesRepo guards the repoRoot
// boundary check: even with parent-relative support, a target that resolves
// outside repoRoot would tie the listing to per-developer paths.
func TestGlob_RejectsParentRelativeEntryThatEscapesRepo(t *testing.T) {
	root := t.TempDir()
	l := lister.NewGlob(root, []string{"**/*.go"}, nil)

	if _, err := l.List(context.Background(), "specs", "../../../escape"); err == nil {
		t.Error("expected error when parent-relative entry resolves outside repo root")
	}
}

func TestGlob_EmptyDirectoryReturnsNoFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cmd", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	l := lister.NewGlob(root, []string{"**/*.go"}, nil)
	got, err := l.List(context.Background(), "", "./cmd/empty")
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
