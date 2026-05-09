// Package cache defines the on-disk record schema and storage backends used by sloff.
package cache

import (
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	sloffv1 "github.com/izumin5210/sloff/internal/proto/sloff/v1"
)

// SchemaVersion is the current cache record schema version. Bumped to 2 by
// ADR-0009 to indicate the protobuf-encoded format. Schema version 1 was the
// YAML format used pre-release; on-disk readers must reject unknown values.
const SchemaVersion = 2

// FileExt is the on-disk extension of a cache record file. Storage backends
// use this to assemble paths and to filter directory listings.
const FileExt = ".pb"

// Record is the in-memory representation of one task's cache entry.
//
// Field shape mirrors sloffv1.CacheRecord so the proto generator is the SSoT
// for ordering / required-ness. Marshal / Unmarshal go through the generated
// proto types via toProto / fromProto rather than re-deriving the wire layout.
type Record struct {
	GeneratedAt   time.Time
	Input         Input
	Output        Output
	SchemaVersion int
	Spec          RecordSpec
}

// RecordSpec captures the spec coordinates of a record.
type RecordSpec struct {
	Cmd    string
	Dir    string
	TaskID string
}

// Input captures everything that contributes to the cache key for one task.
// Hash equals sha256_concat(FilesHash, CmdHash, ResolvedVersionsHash).
//
// ResolvedVersions stores the per-entry detail of every logical version pin
// that fed ResolvedVersionsHash; it unifies what previous YAML schema kept in
// input.components.tools_hash plus the parallel generator_version_snapshot.
type Input struct {
	Hash                 string
	FilesHash            string
	CmdHash              string
	ResolvedVersionsHash string
	ResolvedVersions     ResolvedVersions
}

// Output is the hashed output descriptor.
type Output struct {
	Hash  string
	Files FileHashes
}

// FileHash pairs an output file path (repo-root relative, slash-separated)
// with its content SHA-256.
type FileHash struct {
	Path string
	Hash string
}

// FileHashes is a deterministic, path-sorted set of FileHash entries.
type FileHashes []FileHash

// Paths returns just the path strings of the entries, in their current order.
func (fh FileHashes) Paths() []string {
	out := make([]string, len(fh))
	for i, e := range fh {
		out[i] = e.Path
	}
	return out
}

// ResolvedVersion is one entry contributing to ResolvedVersionsHash. Covers
// user-declared tools (script resolver), transitive Go module pins (go-local),
// and transitive npm package pins (pnpm-local).
type ResolvedVersion struct {
	Name    string
	Source  string
	Version string
}

// ResolvedVersions is a deterministic, name-sorted list.
type ResolvedVersions []ResolvedVersion

// Marshal returns the deterministic proto wire format of r.
//
// proto.Marshal is intentionally only invoked here so the {Deterministic:
// true} option lives in a single location (ADR-0009 §"byte stability の担保").
func (r *Record) Marshal() ([]byte, error) {
	msg, err := r.toProto()
	if err != nil {
		return nil, err
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(msg)
}

// Unmarshal parses a proto-encoded record.
func Unmarshal(b []byte) (*Record, error) {
	msg := &sloffv1.CacheRecord{}
	if err := proto.Unmarshal(b, msg); err != nil {
		return nil, err
	}
	return fromProto(msg), nil
}

func (r *Record) toProto() (*sloffv1.CacheRecord, error) {
	v, err := schemaVersionToProto(r.SchemaVersion)
	if err != nil {
		return nil, err
	}
	return &sloffv1.CacheRecord{
		SchemaVersion: v,
		Spec: &sloffv1.CacheRecord_Spec{
			Dir:    r.Spec.Dir,
			TaskId: r.Spec.TaskID,
			Cmd:    r.Spec.Cmd,
		},
		Input: &sloffv1.CacheRecord_Input{
			Hash:                 r.Input.Hash,
			FilesHash:            r.Input.FilesHash,
			CmdHash:              r.Input.CmdHash,
			ResolvedVersionsHash: r.Input.ResolvedVersionsHash,
			ResolvedVersions:     resolvedVersionsToProto(r.Input.ResolvedVersions),
		},
		Output: &sloffv1.CacheRecord_Output{
			Hash:  r.Output.Hash,
			Files: fileHashesToProto(r.Output.Files),
		},
		GeneratedAt: timestamppb.New(r.GeneratedAt.UTC()),
	}, nil
}

func fromProto(msg *sloffv1.CacheRecord) *Record {
	rec := &Record{SchemaVersion: int(msg.GetSchemaVersion())}
	if t := msg.GetGeneratedAt(); t != nil {
		rec.GeneratedAt = t.AsTime()
	}
	if s := msg.GetSpec(); s != nil {
		rec.Spec = RecordSpec{Cmd: s.GetCmd(), Dir: s.GetDir(), TaskID: s.GetTaskId()}
	}
	if in := msg.GetInput(); in != nil {
		rec.Input = Input{
			Hash:                 in.GetHash(),
			FilesHash:            in.GetFilesHash(),
			CmdHash:              in.GetCmdHash(),
			ResolvedVersionsHash: in.GetResolvedVersionsHash(),
			ResolvedVersions:     resolvedVersionsFromProto(in.GetResolvedVersions()),
		}
	}
	if out := msg.GetOutput(); out != nil {
		rec.Output = Output{
			Hash:  out.GetHash(),
			Files: fileHashesFromProto(out.GetFiles()),
		}
	}
	return rec
}

func resolvedVersionsToProto(in ResolvedVersions) []*sloffv1.CacheRecord_Input_ResolvedVersion {
	if len(in) == 0 {
		return nil
	}
	sorted := append(ResolvedVersions(nil), in...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	out := make([]*sloffv1.CacheRecord_Input_ResolvedVersion, len(sorted))
	for i, v := range sorted {
		out[i] = &sloffv1.CacheRecord_Input_ResolvedVersion{
			Name:    v.Name,
			Source:  v.Source,
			Version: v.Version,
		}
	}
	return out
}

func resolvedVersionsFromProto(in []*sloffv1.CacheRecord_Input_ResolvedVersion) ResolvedVersions {
	if len(in) == 0 {
		return nil
	}
	out := make(ResolvedVersions, len(in))
	for i, v := range in {
		out[i] = ResolvedVersion{Name: v.GetName(), Source: v.GetSource(), Version: v.GetVersion()}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func fileHashesToProto(in FileHashes) []*sloffv1.CacheRecord_FileEntry {
	if len(in) == 0 {
		return nil
	}
	sorted := append(FileHashes(nil), in...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	out := make([]*sloffv1.CacheRecord_FileEntry, len(sorted))
	for i, e := range sorted {
		out[i] = &sloffv1.CacheRecord_FileEntry{Path: e.Path, Hash: e.Hash}
	}
	return out
}

func fileHashesFromProto(in []*sloffv1.CacheRecord_FileEntry) FileHashes {
	if len(in) == 0 {
		return nil
	}
	out := make(FileHashes, len(in))
	for i, e := range in {
		out[i] = FileHash{Path: e.GetPath(), Hash: e.GetHash()}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func schemaVersionToProto(v int) (sloffv1.CacheRecord_SchemaVersion, error) {
	pv := sloffv1.CacheRecord_SchemaVersion(v)
	if _, ok := sloffv1.CacheRecord_SchemaVersion_name[int32(pv)]; !ok {
		return 0, fmt.Errorf("cache: unknown schema version %d", v)
	}
	return pv, nil
}
