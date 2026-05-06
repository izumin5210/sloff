package buf_test

import (
	"context"
	"fmt"
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

// TestResolver_EmitsBufDepFromBufLock guards the canonical BSR-deps path: a
// buf.yaml dep paired with a buf.lock entry must produce a buf-dep ToolVersion
// keyed on the locked commit, so that `buf dep update` invalidates the cache
// even though the dep's name didn't change.
func TestResolver_EmitsBufDepFromBufLock(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
`,
		"proto/buf.yaml": `version: v2
modules:
  - path: .
deps:
  - buf.build/googleapis/googleapis
`,
		"proto/buf.lock": `version: v2
deps:
  - name: buf.build/googleapis/googleapis
    commit: 28151c0d0a1641bf938a7672c500e01d
    digest: shake256:abc123
`,
	})

	versions, err := buf.New(root).Resolve(
		context.Background(), "proto", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("len(versions) = %d, want 2 (remote plugin + buf-dep): %+v", len(versions), versions)
	}

	var dep toolresolver.ToolVersion
	for _, v := range versions {
		if v.Source == "buf-dep:buf.build/googleapis/googleapis" {
			dep = v
			break
		}
	}
	want := toolresolver.ToolVersion{
		Name:    "buf.build/googleapis/googleapis",
		Source:  "buf-dep:buf.build/googleapis/googleapis",
		Version: "buf-dep:buf.build/googleapis/googleapis@28151c0d0a1641bf938a7672c500e01d",
	}
	if diff := cmp.Diff(want, dep); diff != "" {
		t.Errorf("dep ToolVersion mismatch (-want +got):\n%s", diff)
	}
}

// TestResolver_BufDepsSortedDeterministically guards that emitted buf-dep
// entries are sorted by name; otherwise reordering buf.yaml deps would shift
// generator_version_snapshot and produce unstable record YAML even though the
// hashed material is unchanged.
func TestResolver_BufDepsSortedDeterministically(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins: []
`,
		"proto/buf.yaml": `version: v2
deps:
  - buf.build/zeta/zeta
  - buf.build/alpha/alpha
`,
		"proto/buf.lock": `version: v2
deps:
  - name: buf.build/alpha/alpha
    commit: aaaa
  - name: buf.build/zeta/zeta
    commit: zzzz
`,
	})

	versions, err := buf.New(root).Resolve(
		context.Background(), "proto", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("len(versions) = %d, want 2", len(versions))
	}
	if versions[0].Name >= versions[1].Name {
		t.Errorf("expected dep ordering by Name, got %q before %q", versions[0].Name, versions[1].Name)
	}
}

// TestResolver_BufDepCommitChangeShiftsVersion is the cache-invalidation guard
// for `buf dep update`: the same dep with a different locked commit must
// produce a different version string. Otherwise upgrading a dep would
// silently hit the previous cache entry.
func TestResolver_BufDepCommitChangeShiftsVersion(t *testing.T) {
	makeRepo := func(commit string) string {
		return setupRepo(t, map[string]string{
			"proto/buf.gen.yaml": `version: v2
plugins: []
`,
			"proto/buf.yaml": `version: v2
deps:
  - buf.build/googleapis/googleapis
`,
			"proto/buf.lock": fmt.Sprintf(`version: v2
deps:
  - name: buf.build/googleapis/googleapis
    commit: %s
`, commit),
		})
	}
	resolve := func(root string) string {
		t.Helper()
		v, err := buf.New(root).Resolve(
			context.Background(), "proto", nil,
			&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
		)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(v) != 1 {
			t.Fatalf("len(versions) = %d, want 1: %+v", len(v), v)
		}
		return v[0].Version
	}
	v1 := resolve(makeRepo("aaaa"))
	v2 := resolve(makeRepo("bbbb"))
	if v1 == v2 {
		t.Errorf("commit change must shift version, both runs returned %q", v1)
	}
}

// TestResolver_BufDepStripsExplicitTag guards that a buf.yaml dep written with
// an explicit `:vX.Y.Z` tag still matches the bare-name lock entry. Without
// normalisation, every tagged dep would surface as "missing from buf.lock"
// even though buf itself accepts the form.
func TestResolver_BufDepStripsExplicitTag(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins: []
`,
		"proto/buf.yaml": `version: v2
deps:
  - buf.build/googleapis/googleapis:v1.0.0
`,
		"proto/buf.lock": `version: v2
deps:
  - name: buf.build/googleapis/googleapis
    commit: aaaa
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
		t.Fatalf("len(versions) = %d, want 1: %+v", len(versions), versions)
	}
}

// TestResolver_FailsWhenBufLockMissing guards against silent cache trust when
// buf.yaml declares deps but buf.lock is absent. Preflight catches this earlier
// in normal runs but the resolver must also fail so a preflight bypass (e.g.
// LAZYGEN_ALLOW_STALE_DEPS) cannot lead to an unkeyed dep entering the hash.
func TestResolver_FailsWhenBufLockMissing(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins: []
`,
		"proto/buf.yaml": `version: v2
deps:
  - buf.build/googleapis/googleapis
`,
	})

	_, err := buf.New(root).Resolve(
		context.Background(), "proto", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err == nil {
		t.Fatal("expected error when buf.lock is missing")
	}
	if !strings.Contains(err.Error(), "buf dep update") {
		t.Errorf("error should suggest `buf dep update`, got: %v", err)
	}
}

// TestResolver_FailsWhenDepNotInLock guards drift between a freshly-added
// buf.yaml dep and the existing buf.lock. The resolver returns an error
// rather than emitting a partial dep set.
func TestResolver_FailsWhenDepNotInLock(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins: []
`,
		"proto/buf.yaml": `version: v2
deps:
  - buf.build/googleapis/googleapis
  - buf.build/grpc-ecosystem/grpc-gateway
`,
		"proto/buf.lock": `version: v2
deps:
  - name: buf.build/googleapis/googleapis
    commit: aaaa
`,
	})

	_, err := buf.New(root).Resolve(
		context.Background(), "proto", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "buf.gen.yaml"},
	)
	if err == nil {
		t.Fatal("expected error when a dep has no matching lock entry")
	}
	if !strings.Contains(err.Error(), "grpc-gateway") {
		t.Errorf("error should mention the missing dep, got: %v", err)
	}
}

// TestResolver_AcceptsV1BufLock guards backward compatibility with the v1
// lockfile schema (`remote` / `owner` / `repository` fields instead of
// `name`). Repos that haven't run `buf migrate` still ship v1 lockfiles, and
// silently failing them would surface as spurious "dep missing from buf.lock"
// errors even though the file is present and complete.
func TestResolver_AcceptsV1BufLock(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"proto/buf.gen.yaml": `version: v2
plugins: []
`,
		"proto/buf.yaml": `version: v2
deps:
  - buf.build/googleapis/googleapis
`,
		"proto/buf.lock": `version: v1
deps:
  - remote: buf.build
    owner: googleapis
    repository: googleapis
    commit: 28151c0d0a1641bf938a7672c500e01d
    digest: shake256:aaa
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
		t.Fatalf("len(versions) = %d, want 1: %+v", len(versions), versions)
	}
	want := toolresolver.ToolVersion{
		Name:    "buf.build/googleapis/googleapis",
		Source:  "buf-dep:buf.build/googleapis/googleapis",
		Version: "buf-dep:buf.build/googleapis/googleapis@28151c0d0a1641bf938a7672c500e01d",
	}
	if diff := cmp.Diff(want, versions[0]); diff != "" {
		t.Errorf("v1 lock dep mismatch (-want +got):\n%s", diff)
	}
}

// TestResolver_AncestorBufYAMLForDeps guards the module-root walk: buf.yaml
// can live above the spec dir when one workspace hosts multiple per-language
// buf.gen.yaml files. The resolver must walk up to find it.
func TestResolver_AncestorBufYAMLForDeps(t *testing.T) {
	root := setupRepo(t, map[string]string{
		"buf.yaml": `version: v2
deps:
  - buf.build/googleapis/googleapis
`,
		"buf.lock": `version: v2
deps:
  - name: buf.build/googleapis/googleapis
    commit: aaaa
`,
		"proto/buf.gen.yaml": `version: v2
plugins:
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
`,
	})

	versions, err := buf.New(root).Resolve(
		context.Background(), ".", nil,
		&toolresolver.DeclaredTool{Resolver: "buf", BufGenPath: "proto/buf.gen.yaml"},
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("len(versions) = %d, want 2 (remote + dep): %+v", len(versions), versions)
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
