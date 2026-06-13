package glob_test

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"testing"

	"github.com/bmatcuk/doublestar/v4"
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

// TestExpand_EquivalentToDoublestarGlob is the guard for the walk-once-per-base
// optimisation: across shared bases, literal patterns, no-match patterns, a
// missing base, and a root-anchored (base ".") pattern, the in-memory match
// path must return exactly what a per-pattern doublestar.Glob would.
func TestExpand_EquivalentToDoublestarGlob(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "svc", "a", "cmd", "main.gen.go"), "")
	mustWrite(t, filepath.Join(root, "svc", "a", "server", "server.gen.go"), "")
	mustWrite(t, filepath.Join(root, "svc", "a", "x.go"), "")
	mustWrite(t, filepath.Join(root, "svc", "b", "cmd", "main.gen.go"), "")
	mustWrite(t, filepath.Join(root, "svc", "b", "y.go"), "")
	mustWrite(t, filepath.Join(root, "svc", "top.go"), "")
	mustWrite(t, filepath.Join(root, "other", "z.go"), "")

	patternSets := [][]string{
		{"svc/**/*.go"},
		{"svc/**/cmd/main.gen.go"},
		{"svc/**/server/server.gen.go"},
		// Several patterns sharing the "svc" base in one pass — the case the
		// optimisation targets (one walk, many matches).
		{"svc/**/*.go", "svc/**/cmd/main.gen.go", "svc/**/server/server.gen.go"},
		{"svc/top.go"},        // literal file under a base
		{"svc/**/missing.go"}, // existing base, no match
		{"missing/**/*.go"},   // absent base
		{"**/*.go"},           // base "." → doublestar.Glob fallback
	}
	for _, ps := range patternSets {
		got, err := glob.Expand(root, ".", ps)
		if err != nil {
			t.Fatalf("Expand(%v): %v", ps, err)
		}
		want := referenceExpand(t, root, ".", ps)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Expand(%v) diverges from doublestar.Glob (-want +got):\n%s", ps, diff)
		}
	}
}

// referenceExpand reproduces the pre-optimisation behaviour (one
// doublestar.Glob per pattern) so the optimised Expander can be asserted
// byte-for-byte equivalent.
func referenceExpand(t *testing.T, root, specDir string, patterns []string) []string {
	t.Helper()
	fsys := os.DirFS(root)
	seen := map[string]struct{}{}
	for _, p := range patterns {
		joined := path.Join(filepath.ToSlash(specDir), p)
		matches, err := doublestar.Glob(fsys, joined, doublestar.WithFilesOnly())
		if err != nil {
			t.Fatalf("reference glob %q: %v", p, err)
		}
		for _, m := range matches {
			seen[filepath.FromSlash(m)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
