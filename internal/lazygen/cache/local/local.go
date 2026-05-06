// Package local is the default Storage backend that persists records as YAML files on
// the local filesystem under <repoRoot>/.lazygen/cache/. It performs no git operations;
// committing the resulting files (or excluding them via .gitignore) is up to the user
// of lazygen. This is the file-on-disk variant; remote backends (e.g. S3) live in
// sibling packages.
package local

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/izumin5210/lazygen/internal/lazygen/cache"
)

const backendName = "local"

// Storage stores cache records under <repoRoot>/.lazygen/cache/.
//
// Layout: .lazygen/cache/<spec_relpath>/<task_id>/<input_hash>.yml.
//
// SpecRelpath in Key uses forward slashes (canonical) and is converted to OS-native
// path on disk. Architecture.md mentions an "_" substitution; we deviate and keep the
// directory hierarchy verbatim so that List can losslessly recover the spec_relpath
// even when names contain underscores.
type Storage struct {
	repoRoot string
}

// New returns a Storage rooted at repoRoot. The repoRoot is the absolute (or relative
// to cwd) path to the repository working tree.
func New(repoRoot string) *Storage {
	return &Storage{repoRoot: repoRoot}
}

// Name implements cache.Storage.
func (s *Storage) Name() string { return backendName }

// Load implements cache.Storage.
func (s *Storage) Load(_ context.Context, key cache.Key) (*cache.Record, bool, error) {
	b, err := os.ReadFile(s.pathFor(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rec, err := cache.Unmarshal(b)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// Save implements cache.Storage.
func (s *Storage) Save(_ context.Context, key cache.Key, record *cache.Record) error {
	p := s.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := record.Marshal()
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// Delete implements cache.Storage. Missing keys are a no-op.
func (s *Storage) Delete(_ context.Context, key cache.Key) error {
	if err := os.Remove(s.pathFor(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// List implements cache.Storage.
func (s *Storage) List(ctx context.Context, filter cache.ListFilter) ([]cache.Key, error) {
	root := filepath.Join(s.repoRoot, ".lazygen", "cache")
	var keys []cache.Key
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
		if d.IsDir() || filepath.Ext(p) != ".yml" {
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
		key := cache.Key{
			SpecRelpath: strings.Join(parts[:n-2], "/"),
			TaskID:      parts[n-2],
			InputHash:   strings.TrimSuffix(parts[n-1], ".yml"),
		}
		if filter.SpecRelpath != "" && key.SpecRelpath != filter.SpecRelpath {
			return nil
		}
		if filter.TaskID != "" && key.TaskID != filter.TaskID {
			return nil
		}
		if !filter.OlderThan.IsZero() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.ModTime().Before(filter.OlderThan) {
				return nil
			}
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func (s *Storage) pathFor(key cache.Key) string {
	specOS := filepath.FromSlash(path.Clean("/"+key.SpecRelpath))[1:] // tolerate empty / leading slash
	return filepath.Join(s.repoRoot, ".lazygen", "cache", specOS, key.TaskID, key.InputHash+".yml")
}
