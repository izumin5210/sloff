// Package buf implements toolresolver.Resolver for the remote-plugin slice of a
// buf.gen.yaml. It only emits ToolVersion entries for `remote:` plugins; the buf
// binary itself, `protoc_builtin:` plugins, and `local:` plugins are declared
// separately in spec tools[] and resolved by their own resolvers (per ADR-0006).
//
// Hashing strategy:
//   - parse the spec-relative buf.gen.yaml (v2; no v1 support today)
//   - require every `remote:` entry to carry a pinned `:vX.Y.Z` tag because the
//     resolved version is otherwise unobtainable without hitting BSR (see
//     docs/design/resolver-buf.md for the rationale)
//   - emit `buf-remote:<host>/<owner>/<name>@<version>+rev<revision>` so that
//     repackaged-but-same-version BSR plugins still invalidate the cache via
//     their `revision:` field
package buf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	yaml "github.com/goccy/go-yaml"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
)

// Name is the resolver identifier referenced by spec tools[] entries.
const Name = "buf"

// Resolver resolves remote plugin versions declared in a buf.gen.yaml.
type Resolver struct {
	repoRoot string
}

// New returns a Resolver rooted at repoRoot. The resolver reads files via os; no
// caching is performed because every Resolve call re-parses the spec-relative
// buf.gen.yaml. Callers that need memoisation can wrap with their own decorator,
// but in practice each task declares one buf entry so the cost is negligible.
func New(repoRoot string) *Resolver {
	return &Resolver{repoRoot: repoRoot}
}

// Name implements toolresolver.Resolver.
func (r *Resolver) Name() string { return Name }

// Resolve implements toolresolver.Resolver. declared.BufGenPath must point at a
// buf.gen.yaml relative to specDir; absolute / parent-relative paths were
// rejected up front by spec parsing.
func (r *Resolver) Resolve(_ context.Context, specDir string, _ []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
	if declared == nil {
		return nil, errors.New("buf: requires explicit tools[] declaration; auto-dispatch is not supported")
	}
	if declared.BufGenPath == "" {
		return nil, errors.New("buf: declared buf-gen-path is required")
	}

	rel := filepath.FromSlash(declared.BufGenPath)
	full := filepath.Join(r.repoRoot, specDir, rel)
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("buf: read %s: %w", filepath.Join(specDir, rel), err)
	}
	doc, err := parseBufGen(data)
	if err != nil {
		return nil, fmt.Errorf("buf: parse %s: %w", filepath.Join(specDir, rel), err)
	}

	versions := make([]toolresolver.ToolVersion, 0, len(doc.Plugins))
	for i, plugin := range doc.Plugins {
		if plugin.Remote == "" {
			continue
		}
		ref, err := parseRemote(plugin.Remote)
		if err != nil {
			return nil, fmt.Errorf("buf: %s plugins[%d]: %w", filepath.Join(specDir, rel), i, err)
		}
		identity := ref.Host + "/" + ref.Owner + "/" + ref.Name
		versions = append(versions, toolresolver.ToolVersion{
			Name:    identity,
			Source:  "buf-remote:" + identity,
			Version: fmt.Sprintf("buf-remote:%s@%s+rev%d", identity, ref.Version, plugin.Revision),
		})
	}
	return versions, nil
}

// bufGenDoc is the minimal projection of buf.gen.yaml v2 the resolver needs. We
// deliberately ignore everything but `plugins:` so that future field additions
// in the upstream schema (inputs, managed mode, etc.) parse silently and only
// the parts we actually hash become parse errors.
type bufGenDoc struct {
	Plugins []bufGenPlugin `yaml:"plugins"`
}

// bufGenPlugin captures the three plugin kinds. `local` is preserved as a raw
// node because users sometimes write it as a string and sometimes as a list;
// since the resolver does not consume locals, we never have to decode it.
type bufGenPlugin struct {
	Local         yaml.RawMessage `yaml:"local"`
	ProtocBuiltin string          `yaml:"protoc_builtin"`
	Remote        string          `yaml:"remote"`
	Revision      int             `yaml:"revision"`
}

func parseBufGen(data []byte) (*bufGenDoc, error) {
	var doc bufGenDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// remoteRef is the parsed shape of `host/owner/name:version`.
type remoteRef struct {
	Host    string
	Owner   string
	Name    string
	Version string
}

// pinnedTag matches the `vX.Y.Z` shape buf currently issues. We deliberately
// reject pre-release / build metadata suffixes (e.g. `-rc.1`) at this layer:
// the resolver's job is to enforce the pinned-tag invariant from the design
// doc, and pre-release tags on BSR plugins are rare enough that surfacing
// them as an explicit failure (rather than a silent cache hit) is the right
// default. We can loosen this if real usage demands it.
var pinnedTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// parseRemote splits a `host/owner/name:version` reference into its parts and
// rejects anything that lacks a pinned `:vX.Y.Z` tag. This is the single point
// where the design doc's "remote plugins must be pinned" rule is enforced for
// hashing; the preflight checker performs the same check earlier so failures
// surface before any hashing work runs.
func parseRemote(s string) (remoteRef, error) {
	colon := strings.LastIndex(s, ":")
	if colon < 0 {
		return remoteRef{}, fmt.Errorf("remote plugin %q must include a pinned :vX.Y.Z tag", s)
	}
	head, version := s[:colon], s[colon+1:]
	if version == "" {
		return remoteRef{}, fmt.Errorf("remote plugin %q has empty version (expected :vX.Y.Z)", s)
	}
	if !pinnedTag.MatchString(version) {
		return remoteRef{}, fmt.Errorf("remote plugin %q version %q is not pinned (expected :vX.Y.Z)", s, version)
	}

	parts := strings.Split(head, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return remoteRef{}, fmt.Errorf("remote plugin %q must be host/owner/name:version", s)
	}
	return remoteRef{Host: parts[0], Owner: parts[1], Name: parts[2], Version: version}, nil
}

// HasPinnedTag reports whether s already carries a pinned :vX.Y.Z tag. The
// preflight checker imports this so the resolver and the lint use the same
// rule by construction.
func HasPinnedTag(s string) bool {
	colon := strings.LastIndex(s, ":")
	if colon < 0 {
		return false
	}
	return pinnedTag.MatchString(s[colon+1:])
}

// LoadBufGenYAML reads and parses a buf.gen.yaml at <repoRoot>/<specDir>/<rel>.
// The preflight checker calls this so it walks the same parser as the resolver
// and never disagrees about which plugins exist.
func LoadBufGenYAML(repoRoot, specDir, rel string) (*Doc, error) {
	full := filepath.Join(repoRoot, specDir, filepath.FromSlash(rel))
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	doc, err := parseBufGen(data)
	if err != nil {
		return nil, err
	}
	out := &Doc{Plugins: make([]Plugin, len(doc.Plugins))}
	for i, p := range doc.Plugins {
		out.Plugins[i] = Plugin{
			ProtocBuiltin: p.ProtocBuiltin,
			Remote:        p.Remote,
			Revision:      p.Revision,
		}
	}
	return out, nil
}

// Doc / Plugin are the public projection used by the preflight checker.
type Doc struct {
	Plugins []Plugin
}

// Plugin only exposes fields the preflight checker inspects (protoc_builtin /
// remote / revision); local plugins remain opaque on purpose because nothing
// outside this package needs to look at them.
type Plugin struct {
	ProtocBuiltin string
	Remote        string
	Revision      int
}
