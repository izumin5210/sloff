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

	// Read go.sum from the *loaded module*, not from the repo root. In a
	// nested-module monorepo (submodule/go.mod + submodule/go.sum) the
	// repo-root go.sum may not exist or may track an unrelated module set,
	// so fingerprinting external deps against it would leave tools_hash
	// stable across submodule dependency bumps and produce stale cache hits.
	goSum, err := readGoSumForMainModule(pkgs)
	if err != nil {
		return Listing{}, fmt.Errorf("read go.sum: %w", err)
	}

	listing, err := l.walk(pkgs, goSum)
	if err != nil {
		return Listing{}, err
	}
	return listing, nil
}

// readGoSumForMainModule locates the main module via the loaded packages and
// reads <main module dir>/go.sum. Missing go.sum is not an error: a fresh
// module before `go mod tidy` has no entries, and packages whose only deps
// are stdlib have no use for it. In that case the empty []byte ends up as
// empty GoSumLine values for any external modules, which is honest about the
// missing cryptographic anchor.
func readGoSumForMainModule(roots []*packages.Package) ([]byte, error) {
	var goModPath string
	packages.Visit(roots, func(pkg *packages.Package) bool {
		if goModPath != "" {
			return false
		}
		if pkg.Module != nil && pkg.Module.Main && pkg.Module.GoMod != "" {
			goModPath = pkg.Module.GoMod
			return false
		}
		return true
	}, nil)
	if goModPath == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(goModPath), "go.sum"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return b, nil
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
			// GoFiles + EmbedFiles + IgnoredFiles together capture every source
			// file that the package owns regardless of the host build context.
			// IgnoredFiles is critical: without it, build-tagged sources like
			// foo_linux.go / foo_darwin.go / files behind custom -tags would
			// produce different hashes per OS and break the OS-neutral cache
			// contract this resolver promises. Paths are converted to slash
			// form so the digest is identical on Windows and Unix.
			for _, group := range [][]string{pkg.GoFiles, pkg.EmbedFiles, pkg.IgnoredFiles} {
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
		default:
			labelPath, labelVersion, sumPath, sumVersion := externalLabel(pkg.Module)
			key := labelPath + "@" + labelVersion
			if _, ok := externalSet[key]; !ok {
				var sumLine string
				if sumPath != "" && sumVersion != "" {
					sumLine = lookupGoSum(goSum, sumPath, sumVersion)
				}
				externalSet[key] = ExternalModule{
					Path:      labelPath,
					Version:   labelVersion,
					GoSumLine: sumLine,
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

// externalLabel returns two (path, version) pairs for one external module:
//   - (labelPath, labelVersion) is the synthetic identity used as the hash
//     key. The original import path drives this so user-facing references
//     stay stable across replace directives.
//   - (sumPath, sumVersion) is what to look up in go.sum. For versioned
//     replace directives this is the *replacement* module, because go.sum
//     is keyed by the replaced-with path. For local-directory replaces the
//     pair is empty since go.sum has no entry to read.
//
// Per resolver-go-local.md "replace は外部扱い", replace target contents are
// not re-read; the synthetic labelVersion encodes the target path/version
// so a change of replacement (e.g. `=> b v1` → `=> c v1`) flips the hash.
func externalLabel(m *packages.Module) (labelPath, labelVersion, sumPath, sumVersion string) {
	if m.Replace != nil {
		if m.Replace.Version != "" {
			// versioned replace: encode replacement path+version into the
			// label, and use them for the go.sum lookup so an actual hash
			// changes if the user upgrades the replacement target.
			return m.Path,
				"replace=" + m.Replace.Path + "@" + m.Replace.Version,
				m.Replace.Path,
				m.Replace.Version
		}
		// local-directory replace (`replace foo => ../foo`): no go.sum entry.
		return m.Path, "replace=" + m.Replace.Path, "", ""
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
