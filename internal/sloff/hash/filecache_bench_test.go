package hash

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// benchSinkDigest keeps every measured digest observable so the compiler
// cannot elide the hashing work under test.
var benchSinkDigest string

const (
	benchFileCount = 2000
	benchFileSize  = 1024
	benchDirFanout = 50 // spread files across subdirectories like a real repo
)

type hashBenchTree struct {
	root  string
	paths []string
}

// hashBenchFixture builds the file tree once per process (shared across
// sub-benchmarks and -count repetitions) so the expensive 2000-file setup
// never leaks into a timed region. The temp dir is deliberately not cleaned
// up: benchmarks have no per-process teardown hook and the OS reclaims it.
var hashBenchFixture = sync.OnceValues(func() (*hashBenchTree, error) {
	root, err := os.MkdirTemp("", "sloff-hash-bench-*")
	if err != nil {
		return nil, err
	}
	paths := make([]string, benchFileCount)
	for i := range paths {
		rel := filepath.Join(fmt.Sprintf("dir-%02d", i%benchDirFanout), fmt.Sprintf("file-%04d.txt", i))
		if err := benchWriteFile(root, rel, benchFileContent(i)); err != nil {
			return nil, err
		}
		paths[i] = rel
	}
	return &hashBenchTree{root: root, paths: paths}, nil
})

// benchWriteFile mirrors the writeFile test helper but returns an error
// instead of taking *testing.T, so the OnceValues fixture builder can use it.
func benchWriteFile(root, rel string, content []byte) error {
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, content, 0o644)
}

// benchFileContent yields ~1KiB of deterministic, per-index-unique content:
// stable digests across processes, and no dependence on random data.
func benchFileContent(i int) []byte {
	line := fmt.Sprintf("sloff hash bench file %04d 0123456789abcdef\n", i)
	buf := make([]byte, 0, benchFileSize+len(line))
	for len(buf) < benchFileSize {
		buf = append(buf, line...)
	}
	return buf[:benchFileSize]
}

// BenchmarkFileCache pins the two FileCache fast paths against their uncached
// baseline:
//
//   - mode=cold: fresh cache per iteration — the full parallel read+SHA-256
//     cost. This is also the "persistence disabled" (SLOFF_NO_FILE_HASH_CACHE)
//     contrast number.
//   - mode=withinrun-warm: repeated Files on one cache — the within-run
//     memoisation from PR #47. If the entry lookup in digest() regresses,
//     this collapses toward cold speed.
//   - mode=persistent-warm: fresh cache per iteration seeded from an on-disk
//     store — the ADR-0014 cross-run warm path (store load + stat-only
//     validation, zero content reads).
func BenchmarkFileCache(b *testing.B) {
	fx, err := hashBenchFixture()
	if err != nil {
		b.Fatal(err)
	}
	// Uncached (pre-#47) digest: the reference every cached mode must equal.
	coldDigest, err := Files(fx.root, fx.paths)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("mode=cold", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c := NewFileCache()
			d, err := c.Files(fx.root, fx.paths)
			if err != nil {
				b.Fatal(err)
			}
			benchSinkDigest = d
		}
	})

	b.Run("mode=withinrun-warm", func(b *testing.B) {
		b.ReportAllocs()
		c := NewFileCache()
		if _, err := c.Files(fx.root, fx.paths); err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			d, err := c.Files(fx.root, fx.paths)
			if err != nil {
				b.Fatal(err)
			}
			benchSinkDigest = d
		}
	})

	b.Run("mode=persistent-warm", func(b *testing.B) {
		b.ReportAllocs()
		storePath := filepath.Join(b.TempDir(), "filehashes.pb")
		warm := NewPersistentFileCache(storePath)
		if _, err := warm.Files(fx.root, fx.paths); err != nil {
			b.Fatal(err)
		}
		// The fixture files were written moments ago, so their mtime/ctime sit
		// inside the racy window and a plain Save() would drop every entry —
		// the loop below would then silently measure the cold path. Persist
		// with a settled reference time instead (same trick as saveSettled in
		// filecache_test.go).
		if err := warm.saveAt(time.Now().Add(2 * racyMargin)); err != nil {
			b.Fatal(err)
		}

		// Validate outside the timed region that the store really warms a
		// fresh cache: every entry loads and the digest equals the uncached
		// baseline. Without this the benchmark could report a "warm" number
		// measured against an empty store.
		check := NewPersistentFileCache(storePath)
		if len(check.entries) != len(fx.paths) {
			b.Fatalf("persistent store seeded %d entries, want %d", len(check.entries), len(fx.paths))
		}
		got, err := check.Files(fx.root, fx.paths)
		if err != nil {
			b.Fatal(err)
		}
		if got != coldDigest {
			b.Fatalf("persistent-warm digest %q != cold digest %q", got, coldDigest)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			c := NewPersistentFileCache(storePath)
			d, err := c.Files(fx.root, fx.paths)
			if err != nil {
				b.Fatal(err)
			}
			benchSinkDigest = d
		}
	})
}
