package script_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/script"
)

func TestResolver_NameAndCanResolveAreInert(t *testing.T) {
	r := script.New("/tmp")
	if r.Name() != "script" {
		t.Errorf("Name() = %q, want script", r.Name())
	}
	// CanResolve always false: script resolver is declared-only, never auto-dispatch.
	if r.CanResolve(".", []string{"buf", "--version"}) {
		t.Error("CanResolve must return false")
	}
}

func TestResolver_StdoutTrimmedAsVersion(t *testing.T) {
	root := t.TempDir()
	r := script.New(root)

	versions, err := r.Resolve(context.Background(), ".", nil, &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "echo v1.0.0"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []toolresolver.ToolVersion{{
		Name:    "sh",
		Source:  "script:sh",
		Version: "script:sh@v1.0.0",
	}}
	if diff := cmp.Diff(want, versions); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestResolver_ExtractRegexCapturesGroup1(t *testing.T) {
	root := t.TempDir()
	r := script.New(root)

	versions, err := r.Resolve(context.Background(), ".", nil, &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "echo 'go version go1.26.2 darwin/arm64'"},
		Extract:  `(go[0-9]+\.[0-9]+(?:\.[0-9]+)?)`,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := versions[0].Version; got != "script:sh@go1.26.2" {
		t.Errorf("Version = %q, want script:sh@go1.26.2", got)
	}
}

func TestResolver_ExtractRegexWithoutGroupUsesFullMatch(t *testing.T) {
	root := t.TempDir()
	r := script.New(root)

	versions, err := r.Resolve(context.Background(), ".", nil, &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "echo 'fake-tool v1.0.0 build abcdef'"},
		Extract:  `v[0-9.]+`,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := versions[0].Version; got != "script:sh@v1.0.0" {
		t.Errorf("Version = %q, want script:sh@v1.0.0", got)
	}
}

func TestResolver_FailsOnNonZeroExit(t *testing.T) {
	root := t.TempDir()
	r := script.New(root)

	_, err := r.Resolve(context.Background(), ".", nil, &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "exit 7"},
	})
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
}

func TestResolver_FailsOnEmptyStdout(t *testing.T) {
	root := t.TempDir()
	r := script.New(root)

	_, err := r.Resolve(context.Background(), ".", nil, &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "true"},
	})
	if err == nil {
		t.Fatal("expected error when stdout is empty")
	}
}

func TestResolver_FailsWhenExtractDoesNotMatch(t *testing.T) {
	root := t.TempDir()
	r := script.New(root)

	_, err := r.Resolve(context.Background(), ".", nil, &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "echo no-match-here"},
		Extract:  `v[0-9.]+`,
	})
	if err == nil {
		t.Fatal("expected error when extract pattern does not match")
	}
}

func TestResolver_FailsOnInvalidExtractRegex(t *testing.T) {
	root := t.TempDir()
	r := script.New(root)

	_, err := r.Resolve(context.Background(), ".", nil, &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "echo v1"},
		Extract:  `(unbalanced`,
	})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestResolver_FailsWithoutExec(t *testing.T) {
	r := script.New(t.TempDir())
	_, err := r.Resolve(context.Background(), ".", nil, &toolresolver.DeclaredTool{Resolver: "script"})
	if err == nil {
		t.Fatal("expected error when exec is empty")
	}
}

func TestResolver_FailsWithoutDeclared(t *testing.T) {
	r := script.New(t.TempDir())
	_, err := r.Resolve(context.Background(), ".", nil, nil)
	if err == nil {
		t.Fatal("expected error when called via auto-dispatch (declared == nil)")
	}
}

func TestResolver_RunsRelativeToSpecDir(t *testing.T) {
	root := t.TempDir()
	specDir := "spec"
	if err := os.MkdirAll(filepath.Join(root, specDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, specDir, "marker.txt"), []byte("v9.9.9"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := script.New(root)
	versions, err := r.Resolve(context.Background(), specDir, nil, &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "cat marker.txt"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := versions[0].Version; got != "script:sh@v9.9.9" {
		t.Errorf("Version = %q, want script:sh@v9.9.9", got)
	}
}

// TestResolver_MemoizesAcrossInvocations exercises the per-Resolver cache: invoking
// Resolve twice with the same exec/extract should run the subprocess once. We approximate
// "subprocess invocation count" by writing to a counter file from the script.
func TestResolver_MemoizesAcrossInvocations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script unavailable on windows")
	}
	root := t.TempDir()
	counter := filepath.Join(root, "counter.txt")

	r := script.New(root)
	declared := &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "printf x >> " + counter + "; echo v1.0.0"},
	}

	for range 3 {
		if _, err := r.Resolve(context.Background(), ".", nil, declared); err != nil {
			t.Fatal(err)
		}
	}

	contents, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 1 {
		t.Errorf("subprocess should run exactly once with memoization, got %d invocations", len(contents))
	}
}
