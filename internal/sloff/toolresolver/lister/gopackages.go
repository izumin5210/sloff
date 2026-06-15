package lister

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// NewGoPackages returns a SourceLister backed by golang.org/x/tools/go/packages.
//
// `packages.Load` is invoked in-process; sloff does not spawn `go list`
// directly. The returned listing contains repo-relative paths for the entry's
// own module files (the "internal code" partition) and module-level entries for
// every transitively imported external module (the "external" partition).
//
// stdlib (`pkg.Module == nil`) is intentionally omitted: hashing $GOROOT files
// would tie the fingerprint to absolute install paths and break OS-neutral sharing
// (architecture R3). Users who need to invalidate on Go toolchain bumps add a
// `tools: [{exec: ["go", "version"], extract: ...}]` entry alongside go-local;
// see resolver-go-local.md.
func NewGoPackages(repoRoot string) SourceLister {
	return &goPackagesLister{repoRoot: repoRoot}
}

type goPackagesLister struct {
	repoRoot string
}

func (l *goPackagesLister) List(ctx context.Context, specDir, entry string) (Listing, error) {
	if err := validateEntry(entry); err != nil {
		return Listing{}, err
	}

	cfg := &packages.Config{
		// NeedEmbedFiles surfaces //go:embed targets in pkg.EmbedFiles. Without it,
		// edits to embedded templates / schemas / data files would not change the
		// resolved_versions_hash and sloff would serve stale fingerprint hits even though `go run`
		// rebuilds the binary on every embed change.
		Mode: packages.NeedFiles | packages.NeedEmbedFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedModule,
		// Run packages.Load in the spec's working directory so monorepo specs
		// whose go.mod sits beside their sloff.yml resolve correctly. Without
		// this the loader would always anchor at the repo root and fail (or
		// pick the wrong module) for nested-module repositories.
		Dir:     filepath.Join(l.repoRoot, specDir),
		Context: ctx,
	}
	pkgs, err := packages.Load(cfg, entry)
	if err != nil {
		return Listing{}, fmt.Errorf("packages.Load %q: %w", entry, err)
	}
	if errs := collectPackageErrors(pkgs); len(errs) > 0 {
		return Listing{}, fmt.Errorf("packages.Load %q: %s", entry, strings.Join(errs, "; "))
	}
	if len(pkgs) == 0 {
		return Listing{}, fmt.Errorf("packages.Load %q: no packages matched", entry)
	}

	// Read go.sum from every *loaded* main module, not from the repo root.
	// In a nested-module monorepo (submodule/go.mod + submodule/go.sum) the
	// repo-root go.sum may not exist or may track an unrelated module set,
	// so fingerprinting external deps against it would leave resolved_versions_hash
	// stable across submodule dependency bumps. When `go.work` brings
	// several main modules into the same build, every module's go.sum is
	// concatenated so external dep bumps in any sibling module flip the
	// hash too.
	goSum, err := readGoSumForMainModules(pkgs)
	if err != nil {
		return Listing{}, fmt.Errorf("read go.sum: %w", err)
	}

	listing, err := l.walk(pkgs, goSum)
	if err != nil {
		return Listing{}, err
	}
	return listing, nil
}

// validateEntry rejects entries not in the `go run`-compatible spec-relative
// form the resolver accepts. Shared by List and ListBatch.
func validateEntry(entry string) error {
	if entry != "." && entry != ".." &&
		!strings.HasPrefix(entry, "./") && !strings.HasPrefix(entry, "../") {
		return fmt.Errorf("entry must start with %q or %q (or be %q / %q), got %q",
			"./", "../", ".", "..", entry)
	}
	return nil
}

// ListBatch resolves every entry sharing one spec dir with a single
// packages.Load, so the module's shared dependency graph — the dominant cost
// of `go list` on a large monorepo — is built once instead of once per entry.
// Each loaded root package is matched back to its entry by package directory;
// the result is byte-identical to calling List per entry because the listing
// for one entry is produced by walking that entry's root exactly as List does,
// and that entry's go.sum corpus is scoped to the main-module set its own root
// reaches — never widened by sibling entries in the same batch.
//
// Entries that can't map 1:1 to a single root — `./...`-style wildcards, a
// malformed entry, or a package that didn't load — are omitted from the
// result so the caller (Memoized.ListBatch) falls back to per-entry List for
// them. A load error that affects the whole batch is returned as-is.
func (l *goPackagesLister) ListBatch(ctx context.Context, specDir string, entries []string) (map[string]Listing, error) {
	// Only entries that resolve to a single concrete directory are batchable;
	// the rest are left for the caller's per-entry fallback (which also
	// re-surfaces a malformed-entry error through List).
	var batchable []string
	seen := map[string]struct{}{}
	for _, entry := range entries {
		if validateEntry(entry) != nil || strings.Contains(entry, "...") {
			continue
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		batchable = append(batchable, entry)
	}
	if len(batchable) == 0 {
		return map[string]Listing{}, nil
	}

	cfg := &packages.Config{
		Mode: packages.NeedFiles | packages.NeedEmbedFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedModule,
		Dir:     filepath.Join(l.repoRoot, specDir),
		Context: ctx,
	}
	pkgs, err := packages.Load(cfg, batchable...)
	if err != nil {
		return nil, fmt.Errorf("packages.Load batch %v: %w", batchable, err)
	}
	if errs := collectPackageErrors(pkgs); len(errs) > 0 {
		return nil, fmt.Errorf("packages.Load batch %v: %s", batchable, strings.Join(errs, "; "))
	}

	rootByDir := make(map[string]*packages.Package, len(pkgs))
	for _, pkg := range pkgs {
		if dir := packageDir(pkg); dir != "" {
			rootByDir[dir] = pkg
		}
	}

	// go.sum is scoped per entry to the main-module set its own root reaches —
	// exactly what a standalone List(entry) reads. Sharing one corpus across
	// the whole batch would, in a go.work build whose entries belong to disjoint
	// main modules, leak a sibling module's go.sum lines into an entry's Listing
	// and flip resolved_versions_hash depending on whether prewarm ran. Corpora
	// are memoised by main-module set so the common case (every entry in one
	// module) still reads go.sum once.
	goSumByScope := map[string][]byte{}
	out := make(map[string]Listing, len(batchable))
	for _, entry := range batchable {
		absDir := filepath.Clean(filepath.Join(l.repoRoot, specDir, entry))
		pkg, ok := rootByDir[absDir]
		if !ok {
			// Unmapped (e.g. an entry pattern that matched no package or
			// resolved to a dir we can't key on): leave for List fallback.
			continue
		}
		goMods := mainModuleGoMods([]*packages.Package{pkg})
		scope := strings.Join(goMods, "\x00")
		goSum, ok := goSumByScope[scope]
		if !ok {
			goSum, err = readGoSumFiles(goMods)
			if err != nil {
				return nil, fmt.Errorf("read go.sum for %q: %w", entry, err)
			}
			goSumByScope[scope] = goSum
		}
		listing, err := l.walk([]*packages.Package{pkg}, goSum)
		if err != nil {
			return nil, fmt.Errorf("walk %q: %w", entry, err)
		}
		out[entry] = listing
	}
	return out, nil
}

// packageDir returns the on-disk directory of pkg, derived from whichever file
// group is populated. Used to match a loaded root back to the entry that asked
// for it. Empty when pkg owns no files (should not happen for a main package).
func packageDir(pkg *packages.Package) string {
	for _, group := range [][]string{pkg.GoFiles, pkg.OtherFiles, pkg.IgnoredFiles, pkg.EmbedFiles} {
		if len(group) > 0 {
			return filepath.Dir(group[0])
		}
	}
	return ""
}

// mainModuleGoMods returns the sorted, de-duplicated go.mod paths of every main
// module reachable from roots. Multiple paths appear when a `go.work` file pulls
// several repo-local modules into one build. The set doubles as the scope key
// for a listing's go.sum corpus: two roots that reach the same main-module set
// may share one corpus, while roots reaching disjoint sets must not — otherwise
// one entry's listing would absorb a sibling module's go.sum lines.
//
// Sorting makes the key (and the downstream concatenation) deterministic
// regardless of how packages.Load happened to enumerate the modules.
func mainModuleGoMods(roots []*packages.Package) []string {
	seen := map[string]struct{}{}
	var goModPaths []string
	packages.Visit(roots, nil, func(pkg *packages.Package) {
		if pkg.Module == nil || !pkg.Module.Main || pkg.Module.GoMod == "" {
			return
		}
		if _, dup := seen[pkg.Module.GoMod]; dup {
			return
		}
		seen[pkg.Module.GoMod] = struct{}{}
		goModPaths = append(goModPaths, pkg.Module.GoMod)
	})
	sort.Strings(goModPaths)
	return goModPaths
}

// readGoSumFiles concatenates <dir>/go.sum for each go.mod path in goModPaths
// (which mainModuleGoMods already sorted, so the output is deterministic).
// Missing go.sum is tolerated (fresh module before `go mod tidy`, stdlib-only
// deps); an empty corpus ends up as empty GoSumLine values for any external
// modules, which is honest about the missing cryptographic anchor.
func readGoSumFiles(goModPaths []string) ([]byte, error) {
	if len(goModPaths) == 0 {
		return nil, nil
	}
	var combined []byte
	for _, p := range goModPaths {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(p), "go.sum"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if len(combined) > 0 && combined[len(combined)-1] != '\n' {
			combined = append(combined, '\n')
		}
		combined = append(combined, b...)
	}
	return combined, nil
}

// readGoSumForMainModules locates every main module reachable from roots and
// concatenates each <module dir>/go.sum. Multiple main modules show up when a
// `go.work` file pulls several repo-local modules into one build; combining
// their go.sum files lets dependency bumps in any used module flip
// resolved_versions_hash.
func readGoSumForMainModules(roots []*packages.Package) ([]byte, error) {
	return readGoSumFiles(mainModuleGoMods(roots))
}

func (l *goPackagesLister) walk(roots []*packages.Package, goSum []byte) (Listing, error) {
	internalSet := map[string]struct{}{}
	externalSet := map[string]ExternalModule{}
	visited := map[string]bool{}

	var visit func(*packages.Package) error
	visit = func(pkg *packages.Package) error {
		if visited[pkg.ID] {
			return nil
		}
		visited[pkg.ID] = true

		switch {
		case pkg.Module == nil:
			// stdlib: see package doc; hashing $GOROOT breaks OS-neutral fingerprint.
		case pkg.Module.Main:
			if err := l.collectInternalFiles(pkg, internalSet); err != nil {
				return err
			}
		case pkg.Module.Replace != nil && pkg.Module.Replace.Version == "":
			// Local replace (`replace example.com/a => ../local`) brings an
			// arbitrary directory into the build that is not covered by go.sum.
			// Treat its sources exactly like main-module sources so edits to
			// the replaced directory invalidate resolved_versions_hash. The collector
			// rejects paths that escape repoRoot — absolute-path replaces or
			// `../sibling-repo` targets would tie resolved_versions_hash to per-developer
			// machine layouts, which we leave to a future ADR.
			if err := l.collectInternalFiles(pkg, internalSet); err != nil {
				return fmt.Errorf("local replace %s => %s: %w",
					pkg.Module.Path, pkg.Module.Replace.Path, err)
			}
		default:
			labelPath, labelVersion, sumPath, sumVersion := externalLabel(pkg.Module)
			key := labelPath + "@" + labelVersion
			if _, ok := externalSet[key]; !ok {
				externalSet[key] = ExternalModule{
					Path:      labelPath,
					Version:   labelVersion,
					GoSumLine: lookupGoSum(goSum, sumPath, sumVersion),
				}
			}
		}

		// Imports is nil-safe; ranging over nil yields zero iterations.
		for _, dep := range pkg.Imports {
			if err := visit(dep); err != nil {
				return err
			}
		}
		return nil
	}

	for _, p := range roots {
		if err := visit(p); err != nil {
			return Listing{}, err
		}
	}

	internal := make([]string, 0, len(internalSet))
	for f := range internalSet {
		internal = append(internal, f)
	}
	sort.Strings(internal)

	external := make([]ExternalModule, 0, len(externalSet))
	for _, m := range externalSet {
		external = append(external, m)
	}
	sort.Slice(external, func(i, j int) bool {
		if external[i].Path != external[j].Path {
			return external[i].Path < external[j].Path
		}
		return external[i].Version < external[j].Version
	})

	return Listing{InternalFiles: internal, ExternalModules: external}, nil
}

// collectInternalFiles folds every source file pkg owns into internalSet,
// keyed by its repo-relative slash-form path. The four file groups together
// cover every input `go build` re-reads for that package:
//
//   - GoFiles: in-context Go sources
//   - EmbedFiles: //go:embed assets
//   - IgnoredFiles: build-tag / GOOS-conditional Go sources excluded from the
//     current build context (required for OS-neutral hashing — without them
//     foo_linux.go and foo_darwin.go would hash differently per host)
//   - OtherFiles: non-Go inputs (.s assembly, cgo .c/.cc, .syso)
//
// Paths are converted to slash form so the digest is identical on Windows
// and Unix. Files outside repoRoot are rejected because their absolute
// location varies per developer machine, which would break OS-neutral
// fingerprint sharing.
func (l *goPackagesLister) collectInternalFiles(pkg *packages.Package, internalSet map[string]struct{}) error {
	for _, group := range [][]string{pkg.GoFiles, pkg.EmbedFiles, pkg.IgnoredFiles, pkg.OtherFiles} {
		for _, f := range group {
			rel, err := filepath.Rel(l.repoRoot, f)
			if err != nil {
				return fmt.Errorf("rel %q: %w", f, err)
			}
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "../") || rel == ".." {
				return fmt.Errorf("internal file %q escapes repo root", f)
			}
			internalSet[rel] = struct{}{}
		}
	}
	return nil
}

// externalLabel returns two (path, version) pairs for one external module:
//   - (labelPath, labelVersion) is the synthetic identity used as the hash
//     key. The original import path drives this so user-facing references
//     stay stable across replace directives.
//   - (sumPath, sumVersion) is what to look up in go.sum. For versioned
//     replace directives this is the *replacement* module, because go.sum
//     is keyed by the replaced-with path.
//
// Local replace directives (`replace foo => ../foo`) are intentionally not
// handled here — the caller rejects them upstream because they bypass go.sum
// and would let replaced-module edits slip past the fingerprint silently.
func externalLabel(m *packages.Module) (labelPath, labelVersion, sumPath, sumVersion string) {
	if m.Replace != nil && m.Replace.Version != "" {
		// versioned replace: encode replacement path+version into the label,
		// and use them for the go.sum lookup so the hash changes when the
		// user upgrades the replacement target.
		return m.Path,
			"replace=" + m.Replace.Path + "@" + m.Replace.Version,
			m.Replace.Path,
			m.Replace.Version
	}
	return m.Path, m.Version, m.Path, m.Version
}

// lookupGoSum returns the verbatim go.sum line(s) recorded for path@version,
// joined with "\n". Both the "<path> <version> h1:..." and the
// "<path> <version>/go.mod h1:..." entries are returned when present. Returns
// "" when the module is not in go.sum (e.g. local replace, or a fresh add that
// has not been tidied).
func lookupGoSum(goSum []byte, modPath, version string) string {
	if len(goSum) == 0 {
		return ""
	}
	bodyPrefix := []byte(modPath + " " + version + " ")
	modPrefix := []byte(modPath + " " + version + "/go.mod ")
	var lines []string
	for line := range bytes.SplitSeq(goSum, []byte("\n")) {
		if bytes.HasPrefix(line, bodyPrefix) || bytes.HasPrefix(line, modPrefix) {
			lines = append(lines, string(line))
		}
	}
	return strings.Join(lines, "\n")
}

func collectPackageErrors(pkgs []*packages.Package) []string {
	var errs []string
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, e := range pkg.Errors {
			errs = append(errs, e.Error())
		}
	})
	return errs
}
