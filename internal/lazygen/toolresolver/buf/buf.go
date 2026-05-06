// Package buf implements toolresolver.Resolver for the buf-driven slice of a
// codegen task: it emits ToolVersion entries for `remote:` plugins declared in
// buf.gen.yaml and for BSR module dependencies declared in the surrounding
// buf.yaml / buf.lock pair. The buf binary, `protoc_builtin:` plugins, and
// `local:` plugins are declared separately in spec tools[] and resolved by
// their own resolvers (per ADR-0006).
//
// Hashing strategy:
//   - parse the spec-relative buf.gen.yaml (v2; no v1 support today)
//   - require every `remote:` entry to carry a pinned `:vX.Y.Z` tag because the
//     resolved version is otherwise unobtainable without hitting BSR (see
//     docs/design/resolver-buf.md for the rationale)
//   - emit `buf-remote:<host>/<owner>/<name>@<version>+rev<revision>` so that
//     repackaged-but-same-version BSR plugins still invalidate the cache via
//     their `revision:` field
//   - walk up from the buf.gen.yaml directory to find buf.yaml; for every
//     dep declared there, look up the resolved commit in the sibling buf.lock
//     and emit `buf-dep:<host>/<owner>/<name>@<commit>` so that `buf dep
//     update` invalidates downstream caches even though the BSR module's name
//     hasn't changed
//
// This package also exports the buf.yaml / buf.lock / module-root helpers so
// the preflight checker can share the same parsing surface.
package buf

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	yaml "github.com/goccy/go-yaml"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
)

// Name is the resolver identifier referenced by spec tools[] entries.
const Name = "buf"

// Resolver resolves remote plugin and BSR dep versions for a buf.gen.yaml.
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
	bufGenLabel := filepath.Join(specDir, rel)
	full := filepath.Join(r.repoRoot, specDir, rel)
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("buf: read %s: %w", bufGenLabel, err)
	}
	doc, err := parseBufGen(data)
	if err != nil {
		return nil, fmt.Errorf("buf: parse %s: %w", bufGenLabel, err)
	}

	versions := make([]toolresolver.ToolVersion, 0, len(doc.Plugins))
	for i, plugin := range doc.Plugins {
		if plugin.Remote == "" {
			continue
		}
		ref, err := parseRemote(plugin.Remote)
		if err != nil {
			return nil, fmt.Errorf("buf: %s plugins[%d]: %w", bufGenLabel, i, err)
		}
		identity := ref.Host + "/" + ref.Owner + "/" + ref.Name
		versions = append(versions, toolresolver.ToolVersion{
			Name:    identity,
			Source:  "buf-remote:" + identity,
			Version: fmt.Sprintf("buf-remote:%s@%s+rev%d", identity, ref.Version, plugin.Revision),
		})
	}

	depVersions, err := r.resolveBSRDeps(specDir, declared.BufGenPath)
	if err != nil {
		return nil, err
	}
	versions = append(versions, depVersions...)

	return versions, nil
}

// resolveBSRDeps walks up from the buf.gen.yaml's directory looking for a
// buf.yaml; if found, every dep declared there is paired with a buf.lock entry
// and emitted as buf-dep:<host>/<owner>/<name>@<commit>. The walk anchors at
// the buf.gen.yaml directory rather than the spec dir because nothing prevents
// the spec dir from being deeper than the buf module root (the common case is
// they coincide, but ad-hoc placements should still resolve correctly).
//
// Errors during this step are surfaced as resolver errors rather than silent
// no-ops because trusting the cache requires every declared dep to map to a
// known commit; a half-locked module would mean a bumped buf.yaml dep silently
// shares a cache entry with the previous resolution.
func (r *Resolver) resolveBSRDeps(specDir, bufGenPath string) ([]toolresolver.ToolVersion, error) {
	moduleRoot, ok, err := FindBufModuleRoot(r.repoRoot, specDir, bufGenPath)
	if err != nil {
		return nil, fmt.Errorf("buf: find module root: %w", err)
	}
	if !ok {
		return nil, nil
	}

	bufYAML, _, err := LoadBufYAML(r.repoRoot, moduleRoot)
	if err != nil {
		return nil, fmt.Errorf("buf: load %s: %w", path.Join(filepath.ToSlash(moduleRoot), "buf.yaml"), err)
	}
	if bufYAML == nil || len(bufYAML.Deps) == 0 {
		return nil, nil
	}

	bufLock, lockExists, err := LoadBufLock(r.repoRoot, moduleRoot)
	if err != nil {
		return nil, fmt.Errorf("buf: load %s: %w", path.Join(filepath.ToSlash(moduleRoot), "buf.lock"), err)
	}
	if !lockExists {
		return nil, fmt.Errorf("buf: %s declares deps but %s is missing; run `buf dep update`",
			path.Join(filepath.ToSlash(moduleRoot), "buf.yaml"),
			path.Join(filepath.ToSlash(moduleRoot), "buf.lock"))
	}

	locked := make(map[string]BufLockDep, len(bufLock.Deps))
	for _, d := range bufLock.Deps {
		if d.Name != "" {
			locked[d.Name] = d
		}
	}

	versions := make([]toolresolver.ToolVersion, 0, len(bufYAML.Deps))
	for _, dep := range bufYAML.Deps {
		base := StripDepVersion(dep)
		entry, ok := locked[base]
		if !ok {
			return nil, fmt.Errorf("buf: %s declares dep %q but %s has no matching entry; run `buf dep update`",
				path.Join(filepath.ToSlash(moduleRoot), "buf.yaml"), dep,
				path.Join(filepath.ToSlash(moduleRoot), "buf.lock"))
		}
		if entry.Commit == "" {
			return nil, fmt.Errorf("buf: %s entry for %q has empty commit; run `buf dep update`",
				path.Join(filepath.ToSlash(moduleRoot), "buf.lock"), base)
		}
		versions = append(versions, toolresolver.ToolVersion{
			Name:    base,
			Source:  "buf-dep:" + base,
			Version: fmt.Sprintf("buf-dep:%s@%s", base, entry.Commit),
		})
	}
	// Sort so that deps appear in a stable order regardless of how the user
	// wrote buf.yaml; the runner's tools_hash sort would catch reorderings
	// anyway, but stabilising here keeps generator_version_snapshot readable.
	sort.Slice(versions, func(i, j int) bool { return versions[i].Name < versions[j].Name })
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

// FindBufModuleRoot walks up from the directory containing bufGenPath toward
// the repo root looking for a buf.yaml. Returns the module root relative to
// repoRoot (OS-native), or false when no buf.yaml is found anywhere on the
// chain — a legitimate setup if the repo doesn't use BSR modules.
//
// Anchoring at the buf.gen.yaml's directory (not the spec dir) lets the resolver
// and the preflight agree even when a spec dir is deeper than the module root.
func FindBufModuleRoot(repoRoot, specDir, bufGenPath string) (string, bool, error) {
	startRel := filepath.FromSlash(path.Join(filepath.ToSlash(specDir), path.Dir(bufGenPath)))
	dir := startRel
	for {
		candidate := filepath.Join(repoRoot, dir, "buf.yaml")
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return dir, true, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}

		parent := filepath.Dir(dir)
		// dir == "." means we reached the repo root; one more iteration would
		// loop forever because filepath.Dir(".") is "." again.
		if parent == dir || dir == "." {
			return "", false, nil
		}
		dir = parent
	}
}

// BufYAML is the public projection of buf.yaml's deps surface. Other fields
// (modules, lint config, breaking config, etc.) are intentionally dropped so
// schema additions in upstream buf parse silently for the parts we don't read.
type BufYAML struct {
	Deps []string
}

// LoadBufYAML reads and parses <repoRoot>/<moduleRoot>/buf.yaml. The second
// return value reports whether the file existed; (nil, false, nil) means
// "no buf.yaml here" so callers can treat that as a benign signal rather than
// an error.
func LoadBufYAML(repoRoot, moduleRoot string) (*BufYAML, bool, error) {
	full := filepath.Join(repoRoot, moduleRoot, "buf.yaml")
	data, err := os.ReadFile(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var raw bufYAMLDoc
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, false, err
	}
	return &BufYAML{Deps: raw.Deps}, true, nil
}

type bufYAMLDoc struct {
	Deps []string `yaml:"deps"`
}

// BufLock and BufLockDep are the public projection of buf.lock entries used
// by both the resolver (for hashing dep commits) and the preflight checker
// (for verifying every buf.yaml dep is locked).
type BufLock struct {
	Deps []BufLockDep
}

// BufLockDep mirrors one buf.lock v2 entry. Digest is read alongside Commit
// because both surface drift between consecutive `buf dep update` runs even
// when one of them is absent in older lockfiles.
type BufLockDep struct {
	Name   string
	Commit string
	Digest string
}

// LoadBufLock reads and parses <repoRoot>/<moduleRoot>/buf.lock. Returns
// (nil, false, nil) when the file doesn't exist; callers decide whether
// missing-when-deps-declared is an issue.
func LoadBufLock(repoRoot, moduleRoot string) (*BufLock, bool, error) {
	full := filepath.Join(repoRoot, moduleRoot, "buf.lock")
	data, err := os.ReadFile(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var raw bufLockDoc
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, false, err
	}
	out := &BufLock{Deps: make([]BufLockDep, len(raw.Deps))}
	for i, d := range raw.Deps {
		out.Deps[i] = BufLockDep{Name: d.Name, Commit: d.Commit, Digest: d.Digest}
	}
	return out, true, nil
}

type bufLockDoc struct {
	Deps []bufLockDep `yaml:"deps"`
}

type bufLockDep struct {
	Name   string `yaml:"name"`
	Commit string `yaml:"commit"`
	Digest string `yaml:"digest"`
}

// StripDepVersion drops a `:tag` suffix from a buf.yaml dep entry. buf accepts
// both bare module identifiers and tagged ones; the lock keys on bare names,
// so callers normalise via this helper before comparing.
func StripDepVersion(dep string) string {
	for i := len(dep) - 1; i >= 0; i-- {
		if dep[i] == ':' {
			return dep[:i]
		}
		if dep[i] == '/' {
			break
		}
	}
	return dep
}
