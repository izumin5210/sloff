package pnpmlocal

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// DepWalk is the output of a unified workspace-and-external dependency walk
// from one importer entry. Workspaces lists every transitively-reachable
// workspace package directory (OS-native repo-relative); Externals lists
// every transitively-reachable external npm package as
// "<name>@<version-with-peer-suffix>" strings, sorted ascending. Both
// channels are populated by a single pass over the lockfile so we never
// miss an external dep that's only reachable through a workspace link.
type DepWalk struct {
	Workspaces []string
	Externals  []string
}

// WalkDeps walks pnpm-lock.yaml's dependency graph rooted at importerPath.
// The walk has two interleaved fronts:
//
//   - link: edges (workspace dependencies) are followed by joining the link
//     target onto the holding importer's dir, recursing into that package's
//     own importer entry. Each visited workspace dir is recorded.
//   - external version edges (npm registry resolutions) seed an external
//     queue that's then drained against snapshots[<pkg@version>] for
//     transitive externals; peer-context suffixes like
//     "lodash@4.17.21(peer@1.0.0)" round-trip verbatim so peer-version drift
//     still flips tools_hash.
//
// Walking both fronts in one pass matters: if @org/codegen depends on
// @org/util (link:) and @org/util depends on lodash (npm), we must reach
// lodash through util's importer entry — a workspace-blind external walk
// would miss it.
func WalkDeps(lf *Lockfile, importerPath string) (DepWalk, error) {
	if _, ok := lf.Importers[importerPath]; !ok {
		return DepWalk{}, fmt.Errorf("pnpm-local: importer %q not found in pnpm-lock.yaml", importerPath)
	}

	visitedWorkspaces := map[string]struct{}{}
	visitedExternals := map[string]struct{}{}
	wsQueue := []string{importerPath}
	var extQueue []string

	for len(wsQueue) > 0 {
		dir := wsQueue[0]
		wsQueue = wsQueue[1:]
		if _, dup := visitedWorkspaces[dir]; dup {
			continue
		}
		visitedWorkspaces[dir] = struct{}{}

		importer, ok := lf.Importers[dir]
		if !ok {
			// Linked target whose importer entry is missing from the lockfile
			// (rare; could mean the workspace package was removed without
			// regenerating the lockfile). Tolerate it — the dir itself stays
			// in the workspace set so its files still get hashed.
			continue
		}
		for _, bucket := range []map[string]ImporterDep{
			importer.Dependencies,
			importer.DevDependencies,
			importer.OptionalDependencies,
		} {
			for name, dep := range bucket {
				switch {
				case strings.HasPrefix(dep.Version, "link:"):
					target := strings.TrimPrefix(dep.Version, "link:")
					// link target is relative to the holding importer's dir;
					// path.Join + Clean collapses ".." segments deterministic-
					// ally to the canonical workspace dir form.
					resolved := path.Clean(path.Join(filepath.ToSlash(dir), target))
					wsQueue = append(wsQueue, resolved)
				case isExternalVersion(dep.Version):
					extQueue = append(extQueue, name+"@"+dep.Version)
				}
				// `file:` and other non-link non-external entries are skipped:
				// `file:` deps reference local tarballs the user committed,
				// which would tie the cache to that file path; covering them
				// would need a separate channel and is out of scope for now.
			}
		}
	}

	for len(extQueue) > 0 {
		key := extQueue[0]
		extQueue = extQueue[1:]
		if _, dup := visitedExternals[key]; dup {
			continue
		}
		visitedExternals[key] = struct{}{}

		snap, ok := lf.Snapshots[key]
		if !ok {
			// Missing snapshot is tolerable: pnpm sometimes records a dep
			// that only appears in `packages` (no transitive children). The
			// dep is still visited so it surfaces in the result.
			continue
		}
		for _, bucket := range []map[string]string{snap.Dependencies, snap.OptionalDependencies} {
			for name, version := range bucket {
				if !isExternalVersion(version) {
					continue
				}
				extQueue = append(extQueue, name+"@"+version)
			}
		}
	}

	walk := DepWalk{
		Workspaces: make([]string, 0, len(visitedWorkspaces)),
		Externals:  make([]string, 0, len(visitedExternals)),
	}
	for d := range visitedWorkspaces {
		walk.Workspaces = append(walk.Workspaces, d)
	}
	for k := range visitedExternals {
		walk.Externals = append(walk.Externals, k)
	}
	sort.Strings(walk.Workspaces)
	sort.Strings(walk.Externals)
	return walk, nil
}

// CollectExternals is a thin wrapper over WalkDeps that exposes only the
// externals slice. Kept so existing callers and tests don't need to switch
// to the multi-return shape when they only care about the npm side.
func CollectExternals(lf *Lockfile, importerPath string) ([]string, error) {
	walk, err := WalkDeps(lf, importerPath)
	if err != nil {
		return nil, err
	}
	return walk.Externals, nil
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
