package hash

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// FileCache memoises per-file content digests keyed by (size, mtime), so one
// run hashes each unchanged file at most once even when many tasks list the
// same file as input. A run touches the same content repeatedly by design:
// the prefetch pass hashes every task's input set up front, runTask re-hashes
// it at execution time (an upstream task may have rewritten files in
// between), and one task's freshly hashed outputs are usually the next task's
// declared inputs.
//
// Staleness is detected through os.Stat: a path whose size or mtime differs
// from the cached entry is re-read. Tasks that rewrite a file necessarily
// advance its mtime (sub-resolution same-size rewrites are the same residual
// risk every mtime-based build tool accepts). Stat failures (e.g. a path
// removed mid-run) propagate as errors exactly like the uncached read path.
//
// Safe for concurrent use.
type FileCache struct {
	mu      sync.Mutex
	entries map[string]fileCacheEntry // keyed by full (root-joined) path
}

type fileCacheEntry struct {
	size   int64
	mtime  time.Time
	digest []byte
}

// NewFileCache returns an empty FileCache.
func NewFileCache() *FileCache {
	return &FileCache{entries: map[string]fileCacheEntry{}}
}

// Files returns the same digest hash.Files would produce, reading every
// cache-missed file in parallel and reusing cached digests for files whose
// (size, mtime) is unchanged.
func (c *FileCache) Files(root string, paths []string) (string, error) {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)

	// Resolve content digests up front (parallel, cache-aware) so the
	// composition pass below never blocks on I/O.
	digests := make([][]byte, len(sorted))
	g := new(errgroup.Group)
	g.SetLimit(max(runtime.GOMAXPROCS(0), 1))
	for i, p := range sorted {
		g.Go(func() error {
			d, err := c.digest(filepath.Join(root, p))
			if err != nil {
				return err
			}
			digests[i] = d
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return "", err
	}

	i := 0
	return FilesWith(sorted, func(string) ([]byte, error) {
		d := digests[i]
		i++
		return d, nil
	})
}

// FileHex returns the hex SHA-256 of a single file located at
// filepath.Join(root, path), equivalent to hash.File but cache-aware.
func (c *FileCache) FileHex(root, path string) (string, error) {
	d, err := c.digest(filepath.Join(root, path))
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(d), nil
}

// digest returns the content digest for fullPath, reusing the cached value
// when (size, mtime) is unchanged.
func (c *FileCache) digest(fullPath string) ([]byte, error) {
	st, err := os.Stat(fullPath)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	e, ok := c.entries[fullPath]
	c.mu.Unlock()
	if ok && e.size == st.Size() && e.mtime.Equal(st.ModTime()) {
		return e.digest, nil
	}

	d, err := fileSHA256(fullPath)
	if err != nil {
		return nil, err
	}
	// Re-stat after reading: if the file is being rewritten concurrently the
	// pre-read stat may not describe the bytes we just hashed; caching under
	// the post-read identity could then serve a digest that never matches the
	// settled content. Only cache when the identity is stable across the read.
	st2, err := os.Stat(fullPath)
	if err == nil && st2.Size() == st.Size() && st2.ModTime().Equal(st.ModTime()) {
		c.mu.Lock()
		c.entries[fullPath] = fileCacheEntry{size: st.Size(), mtime: st.ModTime(), digest: d}
		c.mu.Unlock()
	}
	return d, nil
}
