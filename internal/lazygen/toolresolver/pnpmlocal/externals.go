package pnpmlocal

import (
	"fmt"
	"sort"
	"strings"
)

// CollectExternals walks the pnpm-lock.yaml dependency graph rooted at the
// given workspace path and returns every transitively-reachable external
// package as `<name>@<version>` strings, sorted ascending and deduplicated.
//
// "External" here means anything pnpm resolved to an npm registry tarball,
// i.e. NOT a workspace link or local file dependency. Workspace links
// (`link:...`) and local file specs (`file:...`) are skipped — those go through
// the esbuild lister via extra inputs (the workspace package's own source
// graph).
//
// The walk uses snapshots[<pkg@version>].dependencies (and optionalDependencies)
// to follow transitive edges, mirroring Turborepo's per-package external dep
// hashing. Peer-context suffixes like `lodash@4.17.21(peer@1.0.0)` are kept
// verbatim so peer-version drift invalidates the hash.
func CollectExternals(lf *Lockfile, importerPath string) ([]string, error) {
	importer, ok := lf.Importers[importerPath]
	if !ok {
		return nil, fmt.Errorf("pnpm-local: importer %q not found in pnpm-lock.yaml", importerPath)
	}

	visited := map[string]struct{}{}

	// Roots: the importer's own deps. We seed the queue from all three
	// dependency buckets because codegen tools are commonly devDependencies
	// (runtime not needed in production) and platform-specific peers can show
	// up under optionalDependencies; missing either would silently drop deps
	// from the hash.
	queue := make([]string, 0)
	for _, bucket := range []map[string]ImporterDep{
		importer.Dependencies,
		importer.DevDependencies,
		importer.OptionalDependencies,
	} {
		for name, dep := range bucket {
			if isExternalVersion(dep.Version) {
				queue = append(queue, name+"@"+dep.Version)
			}
		}
	}

	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if _, dup := visited[key]; dup {
			continue
		}
		visited[key] = struct{}{}

		snap, ok := lf.Snapshots[key]
		if !ok {
			// Missing snapshot is tolerable: pnpm sometimes records a dep that
			// only appears in `packages` (no transitive children). The dep
			// itself is still in our visited set, so it shows up in the result.
			continue
		}
		for _, bucket := range []map[string]string{snap.Dependencies, snap.OptionalDependencies} {
			for name, version := range bucket {
				if !isExternalVersion(version) {
					continue
				}
				queue = append(queue, name+"@"+version)
			}
		}
	}

	out := make([]string, 0, len(visited))
	for k := range visited {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// isExternalVersion reports whether the lockfile-recorded version string
// refers to an npm-registry resolution. Workspace `workspace:*` is recorded
// as `link:<path>`; local file deps as `file:<path>`. Both must be skipped.
func isExternalVersion(v string) bool {
	if v == "" {
		return false
	}
	if strings.HasPrefix(v, "link:") || strings.HasPrefix(v, "file:") {
		return false
	}
	return true
}
