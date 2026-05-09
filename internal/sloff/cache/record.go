// Package cache defines the on-disk record schema and storage backends used by sloff.
//
// The record schema is the protobuf message in proto/sloff/v1/cache.proto;
// generated code lives at internal/proto/sloff/v1. This package re-exports the
// proto types under shorter names (Record / Spec / Input / Output / FileEntry /
// ResolvedVersion) so call sites can stay agnostic of the proto path while the
// proto schema remains the single source of truth (ADR-0009).
package cache

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	sloffv1 "github.com/izumin5210/sloff/internal/proto/sloff/v1"
)

// Record is the in-memory cache record. Aliased onto sloffv1.CacheRecord so
// Marshal/Unmarshal can use proto directly without a translation layer.
type (
	Record          = sloffv1.CacheRecord
	Spec            = sloffv1.CacheRecord_Spec
	Input           = sloffv1.CacheRecord_Input
	Output          = sloffv1.CacheRecord_Output
	FileEntry       = sloffv1.CacheRecord_FileEntry
	ResolvedVersion = sloffv1.CacheRecord_Input_ResolvedVersion
)

// SchemaVersion is the canonical schema version embedded in newly written
// records. Bumped to V2 by ADR-0009 (V1 was the YAML format).
const SchemaVersion = sloffv1.CacheRecord_SCHEMA_VERSION_V2

// FileExt is the on-disk extension of a cache record file. Storage backends
// use this to assemble paths and to filter directory listings.
const FileExt = ".pb"

// Marshal returns the deterministic proto wire format of rec.
//
// proto.MarshalOptions{Deterministic: true} is intentionally only invoked here
// so the option lives in a single location (ADR-0009 §"byte stability の担保").
// Marshal also normalises the order of repeated fields that the schema
// requires sorted, so callers can append entries in any order.
func Marshal(rec *Record) ([]byte, error) {
	if rec == nil {
		return nil, fmt.Errorf("cache: nil record")
	}
	if _, ok := sloffv1.CacheRecord_SchemaVersion_name[int32(rec.GetSchemaVersion())]; !ok {
		return nil, fmt.Errorf("cache: unknown schema version %d", rec.GetSchemaVersion())
	}
	Sort(rec)
	return proto.MarshalOptions{Deterministic: true}.Marshal(rec)
}

// Unmarshal parses a proto-encoded cache record.
func Unmarshal(b []byte) (*Record, error) {
	rec := &Record{}
	if err := proto.Unmarshal(b, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// Sort normalises the order of repeated fields whose schema requires
// deterministic ordering (output.files by path, input.resolved_versions by
// name). Marshal calls this internally; callers comparing two records can
// invoke it explicitly to make sure both sides are in canonical order.
func Sort(rec *Record) {
	if out := rec.GetOutput(); out != nil {
		sort.SliceStable(out.Files, func(i, j int) bool {
			return out.Files[i].GetPath() < out.Files[j].GetPath()
		})
	}
	if in := rec.GetInput(); in != nil {
		sort.SliceStable(in.ResolvedVersions, func(i, j int) bool {
			return in.ResolvedVersions[i].GetName() < in.ResolvedVersions[j].GetName()
		})
	}
}

// FilePaths returns just the path strings of files in their current order.
// Use after Sort if a sorted slice is required.
func FilePaths(files []*FileEntry) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.GetPath()
	}
	return out
}
