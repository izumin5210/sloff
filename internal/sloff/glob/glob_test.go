package glob_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/glob"
)

func TestExpand(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "spec", "a.proto"), "")
	mustWrite(t, filepath.Join(root, "spec", "sub", "b.proto"), "")
	mustWrite(t, filepath.Join(root, "spec", "buf.gen.yaml"), "")
	mustWrite(t, filepath.Join(root, "spec", "ignored.txt"), "")

	got, err := glob.Expand(root, "spec", []string{"**/*.proto", "buf.gen.yaml"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []string{
		filepath.Join("spec", "a.proto"),
		filepath.Join("spec", "buf.gen.yaml"),
		filepath.Join("spec", "sub", "b.proto"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestExpand_DedupesOverlappingPatterns(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "s", "x.go"), "")

	got, err := glob.Expand(root, "s", []string{"**/*.go", "x.go"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []string{filepath.Join("s", "x.go")}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestExpand_NoMatchReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "s", "keep.txt"), "")

	got, err := glob.Expand(root, "s", []string{"**/*.proto"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestExpand_OnlyFilesNotDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "s", "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "s", "subdir", "file.txt"), "")

	got, err := glob.Expand(root, "s", []string{"**"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	for _, p := range got {
		fi, err := os.Stat(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		if fi.IsDir() {
			t.Errorf("Expand returned directory %q", p)
		}
	}
}

func TestExpand_InvalidPatternErrors(t *testing.T) {
	root := t.TempDir()
	_, err := glob.Expand(root, ".", []string{"["})
	if err == nil {
		t.Fatal("expected error for invalid pattern")
	}
}

// TestExpand_CrossDirParentPattern covers cross-dir codegen tasks (e.g. a buf
// task that lives under proto/ but writes outputs into a sibling Go / TS dir):
// patterns containing ".." must escape the spec dir but stay within repoRoot.
func TestExpand_CrossDirParentPattern(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "gen", "go", "a.pb.go"), "")
	mustWrite(t, filepath.Join(root, "gen", "go", "sub", "b.pb.go"), "")
	mustWrite(t, filepath.Join(root, "proto", "spec", "a.proto"), "")

	got, err := glob.Expand(root, filepath.Join("proto", "spec"), []string{"../../gen/go/**/*.pb.go"})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []string{
		filepath.Join("gen", "go", "a.pb.go"),
		filepath.Join("gen", "go", "sub", "b.pb.go"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestExpand_CrossDirMixedPatterns confirms a single Expand call can mix
// spec-local patterns and parent-traversing patterns; the union is returned
// in repoRoot-relative form.
func TestExpand_CrossDirMixedPatterns(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "proto", "spec", "buf.gen.yaml"), "")
	mustWrite(t, filepath.Join(root, "proto", "spec", "a.proto"), "")
	mustWrite(t, filepath.Join(root, "gen", "go", "a.pb.go"), "")

	got, err := glob.Expand(root, filepath.Join("proto", "spec"), []string{
		"**/*.proto",
		"buf.gen.yaml",
		"../../gen/go/**/*.pb.go",
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []string{
		filepath.Join("gen", "go", "a.pb.go"),
		filepath.Join("proto", "spec", "a.proto"),
		filepath.Join("proto", "spec", "buf.gen.yaml"),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestExpand_EscapesRepoRootErrors covers the safety side of the cross-dir
// support: a pattern whose normalised form points outside repoRoot must be
// rejected so a malformed spec can never read or hash files outside the repo.
func TestExpand_EscapesRepoRootErrors(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "spec", "x.go"), "")

	cases := []struct {
		name    string
		specDir string
		pattern string
	}{
		{name: "single dotdot from repo root", specDir: ".", pattern: "../escape"},
		{name: "two dotdots from spec dir", specDir: "spec", pattern: "../../escape"},
		{name: "deep escape from spec dir", specDir: "spec", pattern: "../../../etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := glob.Expand(root, tc.specDir, []string{tc.pattern})
			if err == nil {
				t.Fatalf("expected error for pattern %q", tc.pattern)
			}
		})
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
