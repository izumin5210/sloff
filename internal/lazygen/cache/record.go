// Package cache defines the on-disk record schema and storage backends used by lazygen.
package cache

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	yaml "github.com/goccy/go-yaml"
)

// SchemaVersion is the current cache record schema version.
const SchemaVersion = 1

// Record is the deterministic on-disk representation of one task's cache entry.
//
// The Go field order is alphabetical because goccy/go-yaml emits struct fields in
// declaration order, and architecture.md requires alphabetical top-level keys.
type Record struct {
	GeneratedAt              time.Time         `yaml:"generated_at"`
	GeneratorVersionSnapshot GeneratorVersions `yaml:"generator_version_snapshot,omitempty"`
	Input                    Input             `yaml:"input"`
	Output                   Output            `yaml:"output"`
	SchemaVersion            int               `yaml:"schema_version"`
	Spec                     RecordSpec        `yaml:"spec"`
}

// RecordSpec captures the spec coordinates of a record.
type RecordSpec struct {
	Cmd    string `yaml:"cmd"`
	Dir    string `yaml:"dir"`
	TaskID string `yaml:"task_id"`
}

// Input is the hashed input descriptor.
type Input struct {
	Components InputComponents `yaml:"components"`
	Hash       string          `yaml:"hash"`
}

// InputComponents are the three sub-hashes that compose the input hash.
type InputComponents struct {
	CmdHash   string `yaml:"cmd_hash"`
	FilesHash string `yaml:"files_hash"`
	ToolsHash string `yaml:"tools_hash"`
}

// Output is the hashed output descriptor.
type Output struct {
	Files FileHashes `yaml:"files"`
	Hash  string     `yaml:"hash"`
}

// FileHash pairs an output file path (repo-root relative, slash-separated) with its content SHA-256.
type FileHash struct {
	Path string
	Hash string
}

// FileHashes is a deterministic, path-sorted set of FileHash entries.
type FileHashes []FileHash

// MarshalYAML emits the entries as a path-sorted YAML mapping.
func (fh FileHashes) MarshalYAML() (any, error) {
	sorted := append(FileHashes(nil), fh...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	m := make(yaml.MapSlice, 0, len(sorted))
	for _, e := range sorted {
		m = append(m, yaml.MapItem{Key: e.Path, Value: e.Hash})
	}
	return m, nil
}

// UnmarshalYAML reads a YAML mapping and converts it to a path-sorted FileHashes slice.
// Empty mappings round-trip back to a nil slice to preserve byte-for-byte identity with
// records that were created without any output files.
func (fh *FileHashes) UnmarshalYAML(b []byte) error {
	var m yaml.MapSlice
	if err := yaml.Unmarshal(b, &m); err != nil {
		return err
	}
	if len(m) == 0 {
		*fh = nil
		return nil
	}
	out := make(FileHashes, 0, len(m))
	for _, item := range m {
		path, ok := item.Key.(string)
		if !ok {
			return fmt.Errorf("output.files key must be a string, got %T", item.Key)
		}
		hash, ok := item.Value.(string)
		if !ok {
			return fmt.Errorf("output.files value for %q must be a string, got %T", path, item.Value)
		}
		out = append(out, FileHash{Path: path, Hash: hash})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	*fh = out
	return nil
}

// GeneratorVersion is one informational entry in generator_version_snapshot.
type GeneratorVersion struct {
	Name    string `yaml:"name"`
	Source  string `yaml:"source"`
	Version string `yaml:"version"`
}

// GeneratorVersions is a deterministic, name-sorted list.
type GeneratorVersions []GeneratorVersion

// MarshalYAML emits the entries sorted by Name.
func (g GeneratorVersions) MarshalYAML() (any, error) {
	sorted := append(GeneratorVersions(nil), g...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return []GeneratorVersion(sorted), nil
}

// Marshal returns the deterministic YAML representation: alphabetical top-level keys,
// path-sorted output.files, name-sorted generator_version_snapshot, and exactly one
// trailing LF.
func (r *Record) Marshal() ([]byte, error) {
	b, err := yaml.Marshal(r)
	if err != nil {
		return nil, err
	}
	return ensureSingleTrailingLF(b), nil
}

// Unmarshal parses a record YAML document.
func Unmarshal(b []byte) (*Record, error) {
	r := &Record{}
	if err := yaml.Unmarshal(b, r); err != nil {
		return nil, err
	}
	return r, nil
}

func ensureSingleTrailingLF(b []byte) []byte {
	b = bytes.TrimRight(b, "\n")
	return append(b, '\n')
}
