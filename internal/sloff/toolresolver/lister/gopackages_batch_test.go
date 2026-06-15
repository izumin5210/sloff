package lister_test

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
)

// TestGoPackages_ListBatchMatchesPerEntry is the soundness guarantee the
// resolver's prewarm relies on: for every entry, the batch loader returns the
// same Listing a per-entry List would, and entries' file sets stay distinct
// rather than being unioned together.
func TestGoPackages_ListBatchMatchesPerEntry(t *testing.T) {
	requireGo(t)
	root := t.TempDir()
	mustWriteFile(t, root, "go.mod", "module example.test/batch\n\ngo 1.22\n")
	mustWriteFile(t, root, "pkg/util/util.go", "package util\n\nfunc Greet() string { return \"hi\" }\n")
	mustWriteFile(t, root, "cmd/a/main.go", "package main\n\nimport \"example.test/batch/pkg/util\"\n\nfunc main() { _ = util.Greet() }\n")
	mustWriteFile(t, root, "cmd/b/main.go", "package main\n\nfunc main() {}\n")

	gp := lister.NewGoPackages(root)
	bl, ok := gp.(lister.BatchSourceLister)
	if !ok {
		t.Fatal("NewGoPackages must implement BatchSourceLister")
	}

	entries := []string{"./cmd/a", "./cmd/b"}
	batched, err := bl.ListBatch(context.Background(), "", entries)
	if err != nil {
		t.Fatalf("ListBatch: %v", err)
	}
	for _, e := range entries {
		want, err := gp.List(context.Background(), "", e)
		if err != nil {
			t.Fatalf("List(%q): %v", e, err)
		}
		got, ok := batched[e]
		if !ok {
			t.Fatalf("ListBatch missing entry %q", e)
		}
		wantFiles := append([]string(nil), want.InternalFiles...)
		gotFiles := append([]string(nil), got.InternalFiles...)
		sort.Strings(wantFiles)
		sort.Strings(gotFiles)
		if diff := cmpStringSlices(wantFiles, gotFiles); diff != "" {
			t.Errorf("entry %q InternalFiles mismatch (batch vs per-entry):\n%s", e, diff)
		}
	}

	// cmd/a pulls in pkg/util; cmd/b does not. Confirm batching preserves that
	// distinction (each root is walked independently) instead of leaking one
	// entry's deps into the other.
	if a := batched["./cmd/a"]; !slices.Contains(a.InternalFiles, "pkg/util/util.go") {
		t.Errorf("./cmd/a should include pkg/util/util.go, got %v", a.InternalFiles)
	}
	if b := batched["./cmd/b"]; slices.Contains(b.InternalFiles, "pkg/util/util.go") {
		t.Errorf("./cmd/b should not include pkg/util/util.go, got %v", b.InternalFiles)
	}
}

// TestGoPackages_ListBatchSkipsWildcard verifies wildcard entries are left out
// of the batch result so the caller falls back to per-entry List for them — a
// "./cmd/..." can match many packages and has no single root to key on.
func TestGoPackages_ListBatchSkipsWildcard(t *testing.T) {
	requireGo(t)
	root := t.TempDir()
	mustWriteFile(t, root, "go.mod", "module example.test/batchwild\n\ngo 1.22\n")
	mustWriteFile(t, root, "cmd/a/main.go", "package main\n\nfunc main() {}\n")

	bl := lister.NewGoPackages(root).(lister.BatchSourceLister)
	batched, err := bl.ListBatch(context.Background(), "", []string{"./cmd/a/..."})
	if err != nil {
		t.Fatalf("ListBatch: %v", err)
	}
	if len(batched) != 0 {
		t.Errorf("wildcard entry should be skipped for fallback, got %v", batched)
	}
}

// TestGoPackages_ListBatchGoWorkDisjointMains exercises the multi-main-module
// path: a go.work build whose entries live in different repo-local modules with
// no import edge between them. The batch must still return, for each entry, a
// Listing byte-identical to a standalone List — and must not bleed one module's
// files (or, by extension, go.sum corpus) into the other.
func TestGoPackages_ListBatchGoWorkDisjointMains(t *testing.T) {
	requireGo(t)
	root := t.TempDir()
	mustWriteFile(t, root, "go.work", "go 1.22\n\nuse ./a\nuse ./b\n")
	mustWriteFile(t, root, "a/go.mod", "module example.test/a\n\ngo 1.22\n")
	mustWriteFile(t, root, "a/cmd/gen/main.go", "package main\n\nimport \"example.test/a/pkg/util\"\n\nfunc main() { _ = util.V }\n")
	mustWriteFile(t, root, "a/pkg/util/util.go", "package util\n\nconst V = \"a\"\n")
	mustWriteFile(t, root, "b/go.mod", "module example.test/b\n\ngo 1.22\n")
	mustWriteFile(t, root, "b/cmd/gen/main.go", "package main\n\nfunc main() {}\n")

	gp := lister.NewGoPackages(root)
	bl := gp.(lister.BatchSourceLister)
	entries := []string{"./a/cmd/gen", "./b/cmd/gen"}
	batched, err := bl.ListBatch(context.Background(), "", entries)
	if err != nil {
		t.Fatalf("ListBatch: %v", err)
	}
	for _, e := range entries {
		want, err := gp.List(context.Background(), "", e)
		if err != nil {
			t.Fatalf("List(%q): %v", e, err)
		}
		got, ok := batched[e]
		if !ok {
			t.Fatalf("ListBatch missing entry %q", e)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("entry %q batch vs per-entry mismatch:\n batch=%+v\n  want=%+v", e, got, want)
		}
	}
	// Cross-module isolation: a's internal file must not appear under b.
	if b := batched["./b/cmd/gen"]; slices.Contains(b.InternalFiles, "a/pkg/util/util.go") {
		t.Errorf("./b/cmd/gen leaked a's files: %v", b.InternalFiles)
	}
}
