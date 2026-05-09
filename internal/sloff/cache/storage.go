package cache

import (
	"context"
	"time"

	cachev1 "github.com/izumin5210/sloff/internal/proto/sloff/cache/v1"
)

// Storage is the persistence backend for cache records.
//
// Implementations should treat (SpecRelpath, TaskID, InputHash) as the unique key for
// a record and need not concern themselves with hash semantics. Load/Save/Delete are
// expected to be idempotent for missing keys.
type Storage interface {
	// Name returns the backend identifier (e.g. "local").
	Name() string

	// Load returns the record at key. (nil, false, nil) means "no record"; an error is
	// reserved for IO or decoding failures.
	Load(ctx context.Context, key Key) (*cachev1.Record, bool, error)

	// Save persists the record at key. It must overwrite any existing entry atomically
	// from the caller's perspective.
	Save(ctx context.Context, key Key, record *cachev1.Record) error

	// Delete removes the record at key, or is a no-op if it does not exist.
	Delete(ctx context.Context, key Key) error

	// List enumerates keys for GC and reporting. Empty filter fields mean "no filter".
	List(ctx context.Context, filter ListFilter) ([]Key, error)
}

// Key uniquely identifies a record.
type Key struct {
	SpecRelpath string // spec dir relative to repo root, e.g. "path/to/spec"
	TaskID      string // command name, e.g. "protoc-gen-go"
	InputHash   string // input hash hex
}

// ListFilter narrows the set of keys returned by Storage.List.
type ListFilter struct {
	SpecRelpath string    // exact match; empty = any spec
	TaskID      string    // exact match; empty = any task
	OlderThan   time.Time // zero = no time filter; otherwise only keys whose record file mtime predates this
}
