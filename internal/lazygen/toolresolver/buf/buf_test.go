package buf_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/buf"
)

func TestResolver_Name(t *testing.T) {
	if r := buf.New(t.TempDir()); r.Name() != "buf" {
		t.Errorf("Name() = %q, want buf", r.Name())
	}
}

func TestResolver_FailsWithoutDeclaration(t *testing.T) {
	r := buf.New(t.TempDir())
	if _, err := r.Resolve(context.Background(), ".", nil, nil); err == nil {
		t.Fatal("expected error when declared is nil")
	}
}

func TestResolver_FailsWithoutBufGenPath(t *testing.T) {
	r := buf.New(t.TempDir())
	_, err := r.Resolve(context.Background(), ".", nil, &toolresolver.DeclaredTool{Resolver: "buf"})
	if err == nil {
		t.Fatal("expected error when BufGenPath is empty")
	}
}

// TestResolver_RemotePluginVersion guards the canonical path: a buf.gen.yaml
// with one pinned remote plugin produces a single ToolVersion whose version
// string carries the host/owner/name, the pinned tag, and revision 0 (the
// default when revision is omitted).
func TestResolver_RemotePluginVersion(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:v1.35.2
    out: gen
`,
	})

	versions, err := buf.New(root).Resolve(
		context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := []toolresolver.ToolVersion{{
		Name:    "buf.build/protocolbuffers/go",
		Source:  "buf-remote:buf.build/protocolbuffers/go",
		Version: "buf-remote:buf.build/protocolbuffers/go@v1.35.2+rev0",
	}}
	if diff := cmp.Diff(want, versions); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
}

// TestResolver_RevisionMixedIntoVersion guards that the BSR `revision:` field
// (used when buf repackages without bumping the upstream version) makes it
// into the cache-key string. Without this, a revision bump would silently hit
// the previous cache entry.
func TestResolver_RevisionMixedIntoVersion(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:v1.35.2
    revision: 3
    out: gen
`,
	})

	versions, err := buf.New(root).Resolve(
		context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := versions[0].Version; !strings.HasSuffix(got, "+rev3") {
		t.Errorf("Version = %q, want suffix +rev3", got)
	}
}

// TestResolver_LocalAndProtocBuiltinAreIgnored confirms ADR-0006 D1: only
// remote: plugins are emitted by the buf resolver. Locals and protoc_builtins
// must be declared separately in spec tools[] and resolved by their own
// resolver, otherwise a buf-only declaration would invent a non-existent
// version for them.
func TestResolver_LocalAndProtocBuiltinAreIgnored(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"buf.gen.yaml": `version: v2
plugins:
  - local: protoc-gen-go
    out: gen
  - protoc_builtin: java
    out: gen/java
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
`,
	})

	versions, err := buf.New(root).Resolve(
		context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1 (only the remote plugin)", len(versions))
	}
	if versions[0].Name != "buf.build/grpc/go" {
		t.Errorf("Name = %q, want buf.build/grpc/go", versions[0].Name)
	}
}

// TestResolver_LocalCanBeListForm guards that `local:` written as a YAML list
// (the exec form) does not break parsing. We do not consume locals, so the
// resolver must merely tolerate either YAML shape rather than fail to read
// the document.
func TestResolver_LocalCanBeListForm(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"buf.gen.yaml": `version: v2
plugins:
  - local: ["go", "run", "./cmd/protoc-gen-foo"]
    out: gen
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
`,
	})

	versions, err := buf.New(root).Resolve(
		context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
}

// TestResolver_NoRemotePluginsReturnsEmpty: a buf.gen.yaml with only locals
// is a legitimate declaration ("buf resolver checked, no remotes here"),
// caller still benefits from the lint side of the resolver. Empty result is
// not an error.
func TestResolver_NoRemotePluginsReturnsEmpty(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"buf.gen.yaml": `version: v2
plugins:
  - local: protoc-gen-go
    out: gen
`,
	})

	versions, err := buf.New(root).Resolve(
		context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("len(versions) = %d, want 0", len(versions))
	}
}

func TestResolver_RejectsUnpinnedRemote(t *testing.T) {
	cases := map[string]string{
		"missing tag": `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
`,
		"latest tag": `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:latest
    out: gen
`,
		"non-semver tag": `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:1.35
    out: gen
`,
		"prerelease suffix": `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:v1.35.2-rc.1
    out: gen
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := setupRepo(t, map[string]string{"buf.gen.yaml": body})
			_, err := buf.New(root).Resolve(
				context.Background(), ".", nil,
				&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
			)
			if err == nil {
				t.Fatal("expected error for unpinned remote plugin")
			}
		})
	}
}

func TestResolver_RejectsMalformedRemote(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"buf.gen.yaml": `version: v2
plugins:
  - remote: not/enough:v1.0.0
    out: gen
`,
	})
	_, err := buf.New(root).Resolve(
		context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err == nil {
		t.Fatal("expected error for malformed remote plugin")
	}
}

// TestResolver_ResolvesUnderSpecDir guards that BufGenPath is interpreted
// relative to the spec dir, not the repo root. Without this the resolver
// would behave fine for root specs and silently fail (or hash the wrong
// file) under nested specs that ship their own buf.gen.yaml.
func TestResolver_ResolvesUnderSpecDir(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
`,
	})

	versions, err := buf.New(root).Resolve(
		context.Background(), "proto", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
}

// TestResolver_VersionStableAcrossRuns is the cache-determinism guard. A
// flaky version string here would invalidate the cache on every run, which
// is the failure mode lazygen exists to prevent.
func TestResolver_VersionStableAcrossRuns(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/grpc/go:v1.5.1
    revision: 2
    out: gen
  - remote: buf.build/protocolbuffers/go:v1.35.2
    out: gen
`,
	})

	r := buf.New(root)
	v1, err := r.Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"})
	if err != nil {
		t.Fatalf("Resolve #1: %v", err)
	}
	v2, err := r.Resolve(context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"})
	if err != nil {
		t.Fatalf("Resolve #2: %v", err)
	}
	if diff := cmp.Diff(v1, v2); diff != "" {
		t.Errorf("version output is not stable across runs:\n%s", diff)
	}
}

func setupRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, contents := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
