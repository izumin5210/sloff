package script_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver"
	"github.com/izumin5210/sloff/internal/sloff/toolresolver/script"
)

func TestResolver_Name(t *testing.T) {
	r := script.New("/tmp")
	if r.Name() != "script" {
		t.Errorf("Name() = %q, want script", r.Name())
	}
}

// TestResolver_InputsAlwaysNilWithoutSpawn locks the IZU-16 split contract:
// the script channel never folds source files into the consuming task, so
// Inputs must return nil without ever spawning the version subprocess.
// Without this guarantee, `sloff graph` would still pay the binary-must-exist
// cost it explicitly tries to avoid.
func TestResolver_InputsAlwaysNilWithoutSpawn(t *testing.T) {
	r := script.New(t.TempDir())
	got, err := r.Inputs(context.Background(), ".", &toolresolver.DeclaredTool{
		Resolver: "script",
		// `false` would exit 1 if Inputs ever tried to spawn it; the test
		// passes only when Inputs short-circuits.
		Exec: []string{"false"},
	})
	if err != nil {
		t.Fatalf("Inputs: %v", err)
	}
	if got != nil {
		t.Errorf("script.Inputs must return nil, got %v", got)
	}
}

func TestResolver_VersionsTrimStdoutAsVersion(t *testing.T) {
	root := t.TempDir()
	r := script.New(root)

	got, err := r.Versions(context.Background(), ".", &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "echo v1.0.0"},
	})
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	want := []toolresolver.ResolvedVersion{{
		Name:    "sh",
		Source:  "script:sh",
		Version: "script:sh@v1.0.0",
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

func TestResolver_ExtractRegexCapturesGroup1(t *testing.T) {
	r := script.New(t.TempDir())

	got, err := r.Versions(context.Background(), ".", &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "echo 'go version go1.26.2 darwin/arm64'"},
		Extract:  `(go[0-9]+\.[0-9]+(?:\.[0-9]+)?)`,
	})
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if got[0].Version != "script:sh@go1.26.2" {
		t.Errorf("Version = %q, want script:sh@go1.26.2", got[0].Version)
	}
}

func TestResolver_ExtractRegexWithoutGroupUsesFullMatch(t *testing.T) {
	r := script.New(t.TempDir())

	got, err := r.Versions(context.Background(), ".", &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "echo 'fake-tool v1.0.0 build abcdef'"},
		Extract:  `v[0-9.]+`,
	})
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if got[0].Version != "script:sh@v1.0.0" {
		t.Errorf("Version = %q, want script:sh@v1.0.0", got[0].Version)
	}
}

func TestResolver_VersionsFailsOnNonZeroExit(t *testing.T) {
	r := script.New(t.TempDir())

	_, err := r.Versions(context.Background(), ".", &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "exit 7"},
	})
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
}

func TestResolver_VersionsFailsOnEmptyStdout(t *testing.T) {
	r := script.New(t.TempDir())

	_, err := r.Versions(context.Background(), ".", &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "true"},
	})
	if err == nil {
		t.Fatal("expected error when stdout is empty")
	}
}

func TestResolver_VersionsFailsWhenExtractDoesNotMatch(t *testing.T) {
	r := script.New(t.TempDir())

	_, err := r.Versions(context.Background(), ".", &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "echo no-match-here"},
		Extract:  `v[0-9.]+`,
	})
	if err == nil {
		t.Fatal("expected error when extract pattern does not match")
	}
}

func TestResolver_VersionsFailsOnInvalidExtractRegex(t *testing.T) {
	r := script.New(t.TempDir())

	_, err := r.Versions(context.Background(), ".", &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "echo v1"},
		Extract:  `(unbalanced`,
	})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

// TestResolver_BothMethodsFailWithoutExec keeps the declared-shape validation
// uniform across both methods so a graph-only consumer that calls Inputs
// surfaces the same shape errors the runner would have surfaced via Versions.
func TestResolver_BothMethodsFailWithoutExec(t *testing.T) {
	r := script.New(t.TempDir())
	if _, err := r.Inputs(context.Background(), ".", &toolresolver.DeclaredTool{Resolver: "script"}); err == nil {
		t.Error("Inputs: expected error when exec is empty")
	}
	if _, err := r.Versions(context.Background(), ".", &toolresolver.DeclaredTool{Resolver: "script"}); err == nil {
		t.Error("Versions: expected error when exec is empty")
	}
}

func TestResolver_BothMethodsFailWithoutDeclared(t *testing.T) {
	r := script.New(t.TempDir())
	if _, err := r.Inputs(context.Background(), ".", nil); err == nil {
		t.Error("Inputs: expected error when called via auto-dispatch (declared == nil)")
	}
	if _, err := r.Versions(context.Background(), ".", nil); err == nil {
		t.Error("Versions: expected error when called via auto-dispatch (declared == nil)")
	}
}

func TestResolver_VersionsRunsRelativeToSpecDir(t *testing.T) {
	root := t.TempDir()
	specDir := "spec"
	if err := os.MkdirAll(filepath.Join(root, specDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, specDir, "marker.txt"), []byte("v9.9.9"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := script.New(root)
	got, err := r.Versions(context.Background(), specDir, &toolresolver.DeclaredTool{
		Resolver: "script",
		Exec:     []string{"sh", "-c", "cat marker.txt"},
	})
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if got[0].Version != "script:sh@v9.9.9" {
		t.Errorf("Version = %q, want script:sh@v9.9.9", got[0].Version)
	}
}

// TestResolver_MemoizesAcrossInvocations exercises the per-Resolver cache:
// invoking Versions twice with the same exec/extract should run the
// subprocess once. We approximate "subprocess invocation count" by writing
// to a counter file from the script.
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
		if _, err := r.Versions(context.Background(), ".", declared); err != nil {
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
