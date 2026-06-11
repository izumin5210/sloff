// Package local is the default Storage backend that persists records as protobuf
// binary files on the local filesystem under <repoRoot>/.sloff/fingerprints/. It performs
// no git operations; committing the resulting files (or excluding them via
// .gitignore) is up to the user of sloff. This is the file-on-disk variant;
// remote backends (e.g. S3) live in sibling packages.
package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

const backendName = "local"

// timestampWidth is the digit count of the filename prefix specified by ADR-0010
// (YYYYMMDDHHMMSSsss = 14 calendar digits + 3 millisecond digits).
// Lexicographic order over well-formed prefixes equals chronological order.
const timestampWidth = 17

// Storage stores fingerprints under <repoRoot>/.sloff/fingerprints/.
//
// Layout: .sloff/fingerprints/<spec_relpath>/<task_id>/<YYYYMMDDHHMMSSsss>-<input_hash>.pb
// (ADR-0009 / ADR-0010). The timestamp prefix is the file's initial creation
// time and is preserved across in-place overwrites; it disambiguates
// independently first-written records on different branches so R5 (no
// conflict) is satisfied by path uniqueness rather than wire-bytes
// equivalence.
//
// SpecRelpath in Key uses forward slashes (canonical) and is converted to OS-native
// path on disk. Architecture.md mentions an "_" substitution; we deviate and keep the
// directory hierarchy verbatim so that List can losslessly recover the spec_relpath
// even when names contain underscores.
type Storage struct {
	repoRoot string
	clock    func() time.Time
}

// Option customises a Storage at construction time.
type Option func(*Storage)

// WithClock injects the wall-clock used for the filename's
// initial-creation prefix. Defaults to time.Now().UTC(); tests inject a
// fixed clock so on-disk filenames are deterministic for golden comparison.
func WithClock(clock func() time.Time) Option {
	return func(s *Storage) { s.clock = clock }
}

// New returns a Storage rooted at repoRoot. The repoRoot is the absolute (or relative
// to cwd) path to the repository working tree.
func New(repoRoot string, opts ...Option) *Storage {
	s := &Storage{
		repoRoot: repoRoot,
		clock:    func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Name implements fingerprint.Storage.
func (s *Storage) Name() string { return backendName }

// Load implements fingerprint.Storage. Returns the latest-timestamp record matching
// the (spec, task, input_hash) tuple. Multiple matches arise transiently
// after merges of branches that independently wrote first-time records for
// the same Key; deterministic-generator scope makes them semantically
// equivalent (ADR-0010), so latest is an arbitrary but well-defined choice.
func (s *Storage) Load(_ context.Context, key fingerprint.Key) (*fingerprintv1.Record, bool, error) {
	matches, err := s.matchingFiles(key)
	if err != nil {
		return nil, false, err
	}
	if len(matches) == 0 {
		return nil, false, nil
	}
	latest := matches[len(matches)-1]
	b, err := os.ReadFile(latest)
	if err != nil {
		return nil, false, err
	}
	rec, err := fingerprint.Unmarshal(b)
	if err != nil {
		// Superseded schema versions read as misses so the runner
		// regenerates them through the normal miss path (ADR-0010) —
		// a hard error here would abort the whole run via the prefetch
		// LoadMany. Corruption stays a hard error.
		if errors.Is(err, fingerprint.ErrUnsupportedSchemaVersion) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return rec, true, nil
}

// Save implements fingerprint.Storage. The on-disk filename carries an initial
// creation timestamp that is created on first write and preserved on
// subsequent overwrites. When the merge of independently first-written
// branches has produced multiple files for the same Key, Save defensively
// collapses them onto the earliest-prefix file (preserving the canonical
// creation time visible in git history) before overwriting its contents.
func (s *Storage) Save(_ context.Context, key fingerprint.Key, record *fingerprintv1.Record) error {
	dir := s.dirFor(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	matches, err := s.matchingFiles(key)
	if err != nil {
		return err
	}
	b, err := fingerprint.Marshal(record)
	if err != nil {
		return err
	}
	var target string
	if len(matches) == 0 {
		target = filepath.Join(dir, formatPrefix(s.clock())+"-"+key.InputHash+fingerprint.FileExt)
	} else {
		target = matches[0]
		for _, p := range matches[1:] {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return os.WriteFile(target, b, 0o644)
}

// LoadMany implements fingerprint.Storage by fanning out per-key Load calls in
// parallel. Local I/O is microseconds-fast, so a naive errgroup parallel for
// loop is enough — the bulk shape exists primarily for remote backends, but
// keeping the contract uniform across backends lets the runner dispatch
// without per-backend branching.
func (s *Storage) LoadMany(ctx context.Context, keys []fingerprint.Key) (map[fingerprint.Key]*fingerprintv1.Record, error) {
	out := make(map[fingerprint.Key]*fingerprintv1.Record, len(keys))
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(localBulkConcurrency)
	for _, key := range keys {
		g.Go(func() error {
			rec, ok, err := s.Load(gctx, key)
			if err != nil {
				return fmt.Errorf("local: load %+v: %w", key, err)
			}
			if !ok {
				return nil
			}
			mu.Lock()
			out[key] = rec
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// SaveMany implements fingerprint.Storage by fanning out per-key Save calls in
// parallel. Same rationale as LoadMany: local fs writes are cheap, so we just
// keep parity with the bulk API contract.
func (s *Storage) SaveMany(ctx context.Context, items []fingerprint.KeyRecord) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(localBulkConcurrency)
	for _, it := range items {
		g.Go(func() error {
			if err := s.Save(gctx, it.Key, it.Record); err != nil {
				return fmt.Errorf("local: save %+v: %w", it.Key, err)
			}
			return nil
		})
	}
	return g.Wait()
}

// localBulkConcurrency caps the goroutines spawned by LoadMany / SaveMany.
// Keeping this proportional to NumCPU mirrors the runner's task concurrency
// budget, since the bulk operations are I/O-bound on the same disk.
var localBulkConcurrency = bulkConcurrencyDefault()

func bulkConcurrencyDefault() int {
	n := runtime.NumCPU()
	if n < 1 {
		return 1
	}
	if n > 16 {
		return 16
	}
	return n
}

// Delete implements fingerprint.Storage. Removes every timestamped file matching
// the Key's (spec, task, input_hash). Missing keys are a no-op.
func (s *Storage) Delete(_ context.Context, key fingerprint.Key) error {
	matches, err := s.matchingFiles(key)
	if err != nil {
		return err
	}
	for _, p := range matches {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// List implements fingerprint.Storage. Duplicate timestamp variants for the same
// Key are collapsed to a single returned entry; the entry's effective mtime
// for OlderThan is the maximum across the duplicates because GC cares about
// the most recent activity, not the oldest.
func (s *Storage) List(ctx context.Context, filter fingerprint.ListFilter) ([]fingerprint.Key, error) {
	root := filepath.Join(s.repoRoot, ".sloff", "fingerprints")
	type bucket struct {
		latest time.Time
	}
	seen := make(map[fingerprint.Key]*bucket)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(p) != fingerprint.FileExt {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		n := len(parts)
		if n < 2 {
			return nil
		}
		_, hash, ok := splitFilename(parts[n-1])
		if !ok {
			return nil
		}
		key := fingerprint.Key{
			SpecRelpath: strings.Join(parts[:n-2], "/"),
			TaskID:      parts[n-2],
			InputHash:   hash,
		}
		if filter.SpecRelpath != "" && key.SpecRelpath != filter.SpecRelpath {
			return nil
		}
		if filter.TaskID != "" && key.TaskID != filter.TaskID {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mt := info.ModTime()
		if b, found := seen[key]; found {
			if mt.After(b.latest) {
				b.latest = mt
			}
		} else {
			seen[key] = &bucket{latest: mt}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	keys := make([]fingerprint.Key, 0, len(seen))
	for k, b := range seen {
		if !filter.OlderThan.IsZero() && !b.latest.Before(filter.OlderThan) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].SpecRelpath != keys[j].SpecRelpath {
			return keys[i].SpecRelpath < keys[j].SpecRelpath
		}
		if keys[i].TaskID != keys[j].TaskID {
			return keys[i].TaskID < keys[j].TaskID
		}
		return keys[i].InputHash < keys[j].InputHash
	})
	return keys, nil
}

// CollapseDuplicates removes every duplicate `<timestamp>-<input_hash>.pb`
// file beyond the earliest-prefix one for each (spec, task, input_hash) Key.
// Returns the number of files removed. Used by `sloff fingerprint gc` as the
// duplicate-collapse safety net described in ADR-0010 §"duplicate collapse
// の責務" (Save's collapse only fires when output actually changes, which is
// rare under deterministic-generator scope, so post-merge duplicates can
// linger until GC sweeps them).
func (s *Storage) CollapseDuplicates(ctx context.Context) (int, error) {
	keys, err := s.List(ctx, fingerprint.ListFilter{})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		matches, err := s.matchingFiles(k)
		if err != nil {
			return removed, err
		}
		for _, p := range matches[1:] {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

func (s *Storage) dirFor(key fingerprint.Key) string {
	specOS := filepath.FromSlash(path.Clean("/" + key.SpecRelpath))[1:] // tolerate empty / leading slash
	return filepath.Join(s.repoRoot, ".sloff", "fingerprints", specOS, key.TaskID)
}

// matchingFiles enumerates `<timestamp>-<key.InputHash>.pb` files in the
// Key's directory, sorted ascending by filename (= chronologically by
// initial-creation time). Foreign files that don't match the prefix shape
// are ignored.
func (s *Storage) matchingFiles(key fingerprint.Key) ([]string, error) {
	dir := s.dirFor(key)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	suffix := "-" + key.InputHash + fingerprint.FileExt
	var matches []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		prefix := strings.TrimSuffix(name, suffix)
		if !looksLikeTimestamp(prefix) {
			continue
		}
		matches = append(matches, filepath.Join(dir, name))
	}
	sort.Strings(matches)
	return matches, nil
}

// splitFilename parses `<timestamp>-<hash>.pb`, returning empty / false when
// the shape doesn't match (so List ignores stray files).
func splitFilename(name string) (timestamp, hash string, ok bool) {
	if !strings.HasSuffix(name, fingerprint.FileExt) {
		return "", "", false
	}
	stem := strings.TrimSuffix(name, fingerprint.FileExt)
	dash := strings.IndexByte(stem, '-')
	if dash <= 0 || dash == len(stem)-1 {
		return "", "", false
	}
	prefix, h := stem[:dash], stem[dash+1:]
	if !looksLikeTimestamp(prefix) {
		return "", "", false
	}
	return prefix, h, true
}

func looksLikeTimestamp(s string) bool {
	if len(s) != timestampWidth {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// formatPrefix renders t in the millisecond-precision YYYYMMDDHHMMSSsss form
// required by ADR-0010. Always UTC so the prefix is comparable across
// developers in different timezones.
func formatPrefix(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf(
		"%04d%02d%02d%02d%02d%02d%03d",
		t.Year(), int(t.Month()), t.Day(),
		t.Hour(), t.Minute(), t.Second(),
		t.Nanosecond()/int(time.Millisecond),
	)
}
