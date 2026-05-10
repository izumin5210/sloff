package fingerprint

import (
	"context"
	"time"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
)

// Storage is the persistence backend for fingerprints.
//
// Implementations should treat (SpecRelpath, TaskID, InputHash) as the unique key for
// a record and need not concern themselves with hash semantics. Load/Save/Delete are
// expected to be idempotent for missing keys.
type Storage interface {
	// Name returns the backend identifier (e.g. "local").
	Name() string

	// Load returns the record at key. (nil, false, nil) means "no record"; an error is
	// reserved for IO or decoding failures.
	Load(ctx context.Context, key Key) (*fingerprintv1.Record, bool, error)

	// Save persists the record at key. It must overwrite any existing entry atomically
	// from the caller's perspective.
	Save(ctx context.Context, key Key, record *fingerprintv1.Record) error

	// Delete removes the record at key, or is a no-op if it does not exist.
	Delete(ctx context.Context, key Key) error

	// List enumerates keys for GC and reporting. Empty filter fields mean "no filter".
	List(ctx context.Context, filter ListFilter) ([]Key, error)

	// CollapseDuplicates folds every (spec, task, input_hash) Key that has more
	// than one timestamp variant down to its earliest-prefix variant, returning
	// the number of variants removed. Backends that cannot produce duplicates
	// (a single-writer backend with no merge analogue) may return (0, nil)
	// without inspecting the store, but they MUST surface ctx errors.
	CollapseDuplicates(ctx context.Context) (int, error)
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
