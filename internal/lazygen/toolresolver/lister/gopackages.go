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
// `packages.Load` is invoked in-process; lazygen does not spawn `go list`
// directly. The returned listing contains repo-relative paths for the entry's
// own module files (the "internal code" partition) and module-level entries for
// every transitively imported external module (the "external" partition).
//
// stdlib (`pkg.Module == nil`) is intentionally omitted: hashing $GOROOT files
// would tie the cache to absolute install paths and break OS-neutral sharing
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
	if !strings.HasPrefix(entry, "./") && entry != "." {
		return Listing{}, fmt.Errorf("entry must start with %q, got %q", "./", entry)
	}

	cfg := &packages.Config{
		// NeedEmbedFiles surfaces //go:embed targets in pkg.EmbedFiles. Without it,
		// edits to embedded templates / schemas / data files would not change the
		// tools_hash and lazygen would serve stale cache hits even though `go run`
		// rebuilds the binary on every embed change.
		Mode: packages.NeedFiles | packages.NeedEmbedFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedModule,
		// Run packages.Load in the spec's working directory so monorepo specs
		// whose go.mod sits beside their lazygen.yml resolve correctly. Without
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
	// so fingerprinting external deps against it would leave tools_hash
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

// readGoSumForMainModules locates every main module reachable from roots and
// concatenates each <module dir>/go.sum. Multiple main modules show up when a
// `go.work` file pulls several repo-local modules into one build; combining
// their go.sum files lets dependency bumps in any used module flip
// tools_hash. Missing go.sum is tolerated (fresh module before `go mod tidy`,
// stdlib-only deps); empty go.sum ends up as empty GoSumLine values for any
// external modules, which is honest about the missing cryptographic anchor.
//
// Module order is sorted by GoMod path so concatenation is deterministic
// regardless of how packages.Load happened to enumerate them.
func readGoSumForMainModules(roots []*packages.Package) ([]byte, error) {
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
	if len(goModPaths) == 0 {
		return nil, nil
	}
	sort.Strings(goModPaths)

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
			// stdlib: see package doc; hashing $GOROOT breaks OS-neutral cache.
		case pkg.Module.Main:
			if err := l.collectInternalFiles(pkg, internalSet); err != nil {
				return err
			}
		case pkg.Module.Replace != nil && pkg.Module.Replace.Version == "":
			// Local replace (`replace example.com/a => ../local`) brings an
			// arbitrary directory into the build that is not covered by go.sum.
			// Treat its sources exactly like main-module sources so edits to
			// the replaced directory invalidate tools_hash. The collector
			// rejects paths that escape repoRoot — absolute-path replaces or
			// `../sibling-repo` targets would tie tools_hash to per-developer
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
// cache sharing.
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
// and would let replaced-module edits slip past the cache silently.
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
