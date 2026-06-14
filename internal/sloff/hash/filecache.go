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
	"google.golang.org/protobuf/proto"

	filecachev1 "github.com/izumin5210/sloff/internal/proto/sloff/filecache/v1"
)

// FileCache memoises per-file content digests keyed by file identity, so one
// run hashes each unchanged file at most once even when many tasks list the
// same file as input. A run touches the same content repeatedly by design:
// the prefetch pass hashes every task's input set up front, runTask re-hashes
// it at execution time (an upstream task may have rewritten files in
// between), and one task's freshly hashed outputs are usually the next task's
// declared inputs.
//
// Identity is (size, mtime, ctime, inode), read via os.Stat. size + mtime
// catch ordinary edits; ctime + inode harden the cache against operations that
// preserve mtime (rsync --times / tar / cp -p / restore) and against
// delete-then-recreate — this matters because the cache can be persisted
// across runs (see NewPersistentFileCache and ADR-0014). On platforms without
// a portable ctime/inode source the cache degrades to (size, mtime).
//
// A stale digest would produce a wrong fingerprint hit and a wrong SKIP
// (silent stale codegen that output-comparison cannot catch — ADR-0002/0014),
// so every uncertain case re-hashes: stat failures propagate, the identity is
// re-checked after the read, and the persistent layer drops entries whose mtime
// or ctime is too recent to have settled (ctime guards mtime-preserving
// rewrites — see entryIsRacy).
//
// Safe for concurrent use.
type FileCache struct {
	mu      sync.Mutex
	entries map[string]fileCacheEntry // keyed by full (root-joined) path

	savePath string // "" disables cross-run persistence
}

type fileCacheEntry struct {
	size   int64
	mtime  time.Time
	ctime  int64  // unix nanos; 0 when the platform has no ctime
	inode  uint64 // 0 when the platform has no inode
	idOK   bool   // whether ctime/inode are meaningful for this entry
	digest []byte
}

// NewFileCache returns an empty, non-persistent FileCache (single-run memoise).
func NewFileCache() *FileCache {
	return &FileCache{entries: map[string]fileCacheEntry{}}
}

// NewPersistentFileCache returns a FileCache seeded from a prior run's digests
// stored at path, and remembers path for Save. A missing / unreadable /
// version-mismatched file yields an empty (cold) cache — never an error — so a
// first run or a format bump simply rehashes. See ADR-0014.
func NewPersistentFileCache(path string) *FileCache {
	c := &FileCache{entries: map[string]fileCacheEntry{}, savePath: path}
	c.load()
	return c
}

// FileDigest pairs a repo-relative path with the hex SHA-256 of its content.
type FileDigest struct {
	Path string
	Hex  string
}

// Files returns the same digest hash.Files would produce, reading every
// cache-missed file in parallel and reusing cached digests for files whose
// identity is unchanged.
func (c *FileCache) Files(root string, paths []string) (string, error) {
	ds, err := c.computeDigests(root, paths)
	if err != nil {
		return "", err
	}
	return foldDigests(paths, ds)
}

// FilesAndDigests returns the folded file-set digest (identical to Files) and
// the per-file hex digest of every path, computing each file's content digest
// exactly once. Callers that need both the combined hash and the per-file
// entries (e.g. writing a fingerprint record) use this instead of calling
// Files followed by a second per-file pass over the same set.
func (c *FileCache) FilesAndDigests(root string, paths []string) (string, []FileDigest, error) {
	ds, err := c.computeDigests(root, paths)
	if err != nil {
		return "", nil, err
	}
	folded, err := foldDigests(paths, ds)
	if err != nil {
		return "", nil, err
	}
	entries := make([]FileDigest, len(paths))
	for i, p := range paths {
		entries[i] = FileDigest{Path: p, Hex: hex.EncodeToString(ds[i])}
	}
	return folded, entries, nil
}

// computeDigests resolves the content digest of every path (joined onto root)
// in parallel, reusing cached digests for unchanged files. The result is
// index-aligned with paths.
func (c *FileCache) computeDigests(root string, paths []string) ([][]byte, error) {
	ds := make([][]byte, len(paths))
	g := new(errgroup.Group)
	g.SetLimit(max(runtime.GOMAXPROCS(0), 1))
	for i, p := range paths {
		g.Go(func() error {
			d, err := c.digest(filepath.Join(root, p))
			if err != nil {
				return err
			}
			ds[i] = d
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return ds, nil
}

// foldDigests composes pre-computed per-file digests into the file-set digest
// via FilesWith (the single definition of the composition rules), looking each
// digest up by path so the caller never has to pre-sort to stay index-aligned
// with FilesWith's internal sort.
func foldDigests(paths []string, ds [][]byte) (string, error) {
	m := make(map[string][]byte, len(paths))
	for i, p := range paths {
		m[p] = ds[i]
	}
	return FilesWith(paths, func(p string) ([]byte, error) {
		return m[p], nil
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
// when the file identity is unchanged.
func (c *FileCache) digest(fullPath string) ([]byte, error) {
	fi, err := os.Stat(fullPath)
	if err != nil {
		return nil, err
	}
	ctime, inode, idOK := fileIdentity(fi)

	c.mu.Lock()
	e, ok := c.entries[fullPath]
	c.mu.Unlock()
	if ok && entryMatches(e, fi, ctime, inode, idOK) {
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
	fi2, err := os.Stat(fullPath)
	if err == nil && fi2.Size() == fi.Size() && fi2.ModTime().Equal(fi.ModTime()) {
		if ct2, in2, ok2 := fileIdentity(fi2); ct2 == ctime && in2 == inode && ok2 == idOK {
			c.mu.Lock()
			c.entries[fullPath] = fileCacheEntry{
				size: fi.Size(), mtime: fi.ModTime(), ctime: ctime, inode: inode, idOK: idOK, digest: d,
			}
			c.mu.Unlock()
		}
	}
	return d, nil
}

// entryMatches reports whether a cached entry is still valid for the current
// stat. size + mtime are always required; ctime + inode are additionally
// required when both the entry and the current stat carry them. If the current
// platform lacks identity (idOK == false) the cache degrades to (size, mtime).
func entryMatches(e fileCacheEntry, fi os.FileInfo, ctime int64, inode uint64, idOK bool) bool {
	if e.size != fi.Size() || !e.mtime.Equal(fi.ModTime()) {
		return false
	}
	if idOK && e.idOK {
		return e.ctime == ctime && e.inode == inode
	}
	// One side lacks identity: fall back to size+mtime (already matched). An
	// entry persisted with identity but read on a platform without it (or vice
	// versa) is rare and degrades safely toward more rehashing, never less.
	return true
}

// --- cross-run persistence (ADR-0014) -------------------------------------

// cacheSchemaVersion is the schema embedded in newly written caches. The
// on-disk format is protobuf (filecachev1.Cache), mirroring the fingerprint
// record (ADR-0009). Bump the enum (V2, …) whenever the on-disk schema OR the
// digest composition changes, so a cache written by an incompatible binary is
// rejected wholesale instead of returning incompatible digests.
const cacheSchemaVersion = filecachev1.SchemaVersion_SCHEMA_VERSION_V1

// racyMargin is the window around the save time within which a file's timestamp
// is considered unsettled: the file may have been rewritten within the same
// (possibly coarse) tick after we hashed it, so trusting it next run could
// serve a stale digest. Such entries are dropped on Save (cheap to rehash).
// Mirrors Git's racy-clean handling.
const racyMargin = 2 * time.Second

// entryIsRacy reports whether e is too fresh to persist: racy when its mtime —
// or, on identity-bearing platforms, its ctime — falls within racyMargin of
// now. Checking ctime is essential, not redundant: mtime-preserving operations
// (rsync --times / tar / cp -p / restore) advance only ctime, so a
// same-(size, mtime) rewrite right after hashing would read back as a cache hit
// and serve a stale digest unless its racy ctime drops the entry here. When the
// platform has no identity (idOK == false) ctime is meaningless and ignored,
// degrading to the mtime-only guard.
func entryIsRacy(e fileCacheEntry, now time.Time) bool {
	if now.Sub(e.mtime) < racyMargin {
		return true
	}
	if e.idOK && now.Sub(time.Unix(0, e.ctime)) < racyMargin {
		return true
	}
	return false
}

// load seeds entries from savePath, best-effort. Any read / decode error or
// schema-version mismatch leaves the cache empty (cold).
func (c *FileCache) load() {
	b, err := os.ReadFile(c.savePath)
	if err != nil {
		return
	}
	var pc filecachev1.Cache
	if err := proto.Unmarshal(b, &pc); err != nil || pc.GetSchemaVersion() != cacheSchemaVersion {
		return
	}
	for _, pe := range pc.GetEntries() {
		c.entries[pe.GetPath()] = fileCacheEntry{
			size:   pe.GetSize(),
			mtime:  time.Unix(0, pe.GetMtimeNanos()),
			ctime:  pe.GetCtimeNanos(),
			inode:  pe.GetInode(),
			idOK:   pe.GetIdentityOk(),
			digest: pe.GetDigest(),
		}
	}
}

// Save persists the current digests to the configured path (atomically),
// dropping racy entries. A no-op when persistence is disabled. Best-effort by
// contract: a stale or missing cache only costs rehashing next run, never
// correctness, so callers may ignore the error.
func (c *FileCache) Save() error {
	return c.saveAt(time.Now())
}

// saveAt is Save with an injectable reference time, so the racy-window logic
// can be tested deterministically without sleeping. Save passes the wall clock.
func (c *FileCache) saveAt(now time.Time) error {
	if c.savePath == "" {
		return nil
	}

	c.mu.Lock()
	pc := &filecachev1.Cache{
		SchemaVersion: cacheSchemaVersion,
		Entries:       make([]*filecachev1.Entry, 0, len(c.entries)),
	}
	for path, e := range c.entries {
		if entryIsRacy(e, now) {
			continue
		}
		pc.Entries = append(pc.Entries, &filecachev1.Entry{
			Path:       path,
			Size:       e.size,
			MtimeNanos: e.mtime.UnixNano(),
			CtimeNanos: e.ctime,
			Inode:      e.inode,
			IdentityOk: e.idOK,
			Digest:     e.digest,
		})
	}
	c.mu.Unlock()

	// Sort by path so the marshalled bytes are reproducible (mirrors the
	// fingerprint Sort discipline); the map iteration order above is otherwise
	// nondeterministic.
	sort.Slice(pc.Entries, func(i, j int) bool {
		return pc.Entries[i].GetPath() < pc.Entries[j].GetPath()
	})

	return writeCacheAtomic(c.savePath, pc)
}

// writeCacheAtomic marshals pc and writes it to a temp file in the destination
// directory, then renames it into place, so a crashed / concurrent run never
// observes a half-written cache.
func writeCacheAtomic(path string, pc *filecachev1.Cache) error {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(pc)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".filehashes-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
