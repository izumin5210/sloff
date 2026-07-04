package glob_test

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/izumin5210/sloff/internal/sloff/glob"
)

// benchSinkMatches keeps every measured result observable so the compiler
// cannot elide the expansion work under test.
var benchSinkMatches []string

const benchSvcCount = 300

// benchPatterns is the shared-literal-base shape PR #52 optimises: three
// recursive patterns rooted at the same "svc" base, which the Expander walks
// once and matches in memory instead of re-walking once per pattern.
var benchPatterns = []string{
	"svc/**/*.go",
	"svc/**/cmd/main.gen.go",
	"svc/**/server/server.gen.go",
}

type globBenchTree struct {
	root      string
	wantFiles int
}

// globBenchFixture builds the service tree once per process (shared across
// sub-benchmarks and -count repetitions) so setup never leaks into a timed
// region. The temp dir is deliberately not cleaned up: benchmarks have no
// per-process teardown hook and the OS reclaims it.
var globBenchFixture = sync.OnceValues(func() (*globBenchTree, error) {
	root, err := os.MkdirTemp("", "sloff-glob-bench-*")
	if err != nil {
		return nil, err
	}
	for i := range benchSvcCount {
		svc := filepath.Join(root, "svc", fmt.Sprintf("name-%03d", i))
		for _, rel := range []string{
			filepath.Join("cmd", "main.gen.go"),
			filepath.Join("server", "server.gen.go"),
			"api.go",
			filepath.Join("internal", "x.go"),
		} {
			if err := benchWrite(filepath.Join(svc, rel)); err != nil {
				return nil, err
			}
		}
		// Non-matching noise inside the base: the walk must pay for these
		// files even though no pattern selects them.
		if err := benchWrite(filepath.Join(svc, "README.md")); err != nil {
			return nil, err
		}
	}
	// A sibling tree outside the shared base, so the fixture is not a pure
	// svc/ monoculture.
	for i := range 5 {
		if err := benchWrite(filepath.Join(root, "docs", fmt.Sprintf("d%02d.md", i))); err != nil {
			return nil, err
		}
	}
	// Every .go file in the fixture matches the first pattern; the other two
	// patterns select subsets, so the union is exactly the .go file count.
	return &globBenchTree{root: root, wantFiles: benchSvcCount * 4}, nil
})

func benchWrite(full string) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, nil, 0o644)
}

// benchReferenceExpand reproduces the pre-optimisation expansion — one
// doublestar.Glob walk per pattern — as the informational baseline the
// optimised modes are judged against. It mirrors referenceExpand in
// glob_test.go, which cannot be reused because it is *testing.T-bound.
func benchReferenceExpand(root, specDir string, patterns []string) ([]string, error) {
	fsys := os.DirFS(root)
	seen := map[string]struct{}{}
	for _, p := range patterns {
		joined := path.Join(filepath.ToSlash(specDir), p)
		matches, err := doublestar.Glob(fsys, joined, doublestar.WithFilesOnly())
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			seen[filepath.FromSlash(m)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// BenchmarkExpander pins the two Expander optimisations against the
// pre-optimisation baseline:
//
//   - mode=shared-base-cold: fresh Expander per iteration — one walk of the
//     shared "svc" base serves all three patterns (PR #52). If the base
//     listing reuse regresses to per-pattern walks, this multiplies by the
//     pattern count and drifts toward reference-per-pattern.
//   - mode=memoised-repeat: repeated Expand on one Expander — the per-pattern
//     result cache from PR #47. If that memoisation regresses, this collapses
//     toward shared-base-cold.
//   - mode=reference-per-pattern: one doublestar.Glob walk per pattern — the
//     pre-#52 cost, kept as a contrast so the win stays visible in every run.
func BenchmarkExpander(b *testing.B) {
	fx, err := globBenchFixture()
	if err != nil {
		b.Fatal(err)
	}

	// Validate once, outside any timed region: the optimised expansion must
	// find every targeted fixture file AND agree byte-for-byte with the
	// per-pattern baseline — otherwise the modes would time incomparable work.
	got, err := glob.NewExpander(fx.root).Expand(".", benchPatterns)
	if err != nil {
		b.Fatal(err)
	}
	if len(got) != fx.wantFiles {
		b.Fatalf("Expand matched %d files, want %d (fixture or expansion is broken)", len(got), fx.wantFiles)
	}
	ref, err := benchReferenceExpand(fx.root, ".", benchPatterns)
	if err != nil {
		b.Fatal(err)
	}
	if !slices.Equal(ref, got) {
		b.Fatalf("Expander result diverges from per-pattern reference (%d vs %d files)", len(got), len(ref))
	}

	b.Run("mode=shared-base-cold", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			out, err := glob.NewExpander(fx.root).Expand(".", benchPatterns)
			if err != nil {
				b.Fatal(err)
			}
			benchSinkMatches = out
		}
	})

	b.Run("mode=memoised-repeat", func(b *testing.B) {
		b.ReportAllocs()
		e := glob.NewExpander(fx.root)
		if _, err := e.Expand(".", benchPatterns); err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			out, err := e.Expand(".", benchPatterns)
			if err != nil {
				b.Fatal(err)
			}
			benchSinkMatches = out
		}
	})

	b.Run("mode=reference-per-pattern", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			out, err := benchReferenceExpand(fx.root, ".", benchPatterns)
			if err != nil {
				b.Fatal(err)
			}
			benchSinkMatches = out
		}
	})
}
