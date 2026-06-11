// Package fingerprint defines the storage interface and serialization helpers for
// sloff's on-disk fingerprints. The record schema itself is the protobuf
// message in proto/sloff/fingerprint/v1/fingerprint.proto; generated code lives at
// internal/proto/sloff/fingerprint/v1 (Go package fingerprintv1). Callers operate on
// *fingerprintv1.Record values directly; this package only provides the marshal /
// sort / lookup helpers and the storage backend interface.
package fingerprint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
)

// SchemaVersion is the canonical schema version embedded in newly written
// records. ADR-0009 bumped V1 (YAML) → V2 (protobuf). ADR-0010 bumped V2 → V3
// when Record.generated_at was dropped in favour of the filename's timestamp
// prefix; legacy V2 records become invalid and are rejected on read so callers
// regenerate them through the normal fingerprint-miss path.
const SchemaVersion = fingerprintv1.SchemaVersion_SCHEMA_VERSION_V3

// FileExt is the on-disk extension of a fingerprint file. Storage backends
// use this to assemble paths and to filter directory listings.
const FileExt = ".pb"

// ErrUnsupportedSchemaVersion marks records whose schema_version is a known
// enum value that the current schema has superseded (V2, ADR-0010). Storage
// backends convert it into a fingerprint miss so leftover records regenerate
// through the normal miss path instead of failing the run; corruption and
// unknown (newer-binary) versions stay hard errors.
var ErrUnsupportedSchemaVersion = errors.New("fingerprint: unsupported schema version")

// Marshal returns the deterministic proto wire format of rec.
//
// proto.MarshalOptions{Deterministic: true} is intentionally only invoked here
// so the option lives in a single location (ADR-0009 §"byte stability の担保").
// Marshal also normalises the order of repeated fields that the schema
// requires sorted, so callers can append entries in any order.
func Marshal(rec *fingerprintv1.Record) ([]byte, error) {
	if rec == nil {
		return nil, fmt.Errorf("fingerprint: nil record")
	}
	if err := validateSchemaVersion(rec.GetSchemaVersion()); err != nil {
		return nil, err
	}
	Sort(rec)
	return proto.MarshalOptions{Deterministic: true}.Marshal(rec)
}

// Unmarshal parses a proto-encoded fingerprint. ADR-0009 treats records with
// an unknown or unspecified schema_version as runtime errors rather than
// best-effort decodes, so the validation runs symmetrically on the read path
// — including against zero-byte files, which proto.Unmarshal otherwise turns
// into a default-valued Record.
func Unmarshal(b []byte) (*fingerprintv1.Record, error) {
	rec := &fingerprintv1.Record{}
	if err := proto.Unmarshal(b, rec); err != nil {
		return nil, err
	}
	if err := validateSchemaVersion(rec.GetSchemaVersion()); err != nil {
		return nil, err
	}
	return rec, nil
}

func validateSchemaVersion(v fingerprintv1.SchemaVersion) error {
	if v == fingerprintv1.SchemaVersion_SCHEMA_VERSION_UNSPECIFIED {
		return fmt.Errorf("fingerprint: schema version is unspecified (likely a corrupt or empty record file)")
	}
	if _, ok := fingerprintv1.SchemaVersion_name[int32(v)]; !ok {
		return fmt.Errorf("fingerprint: unknown schema version %d", v)
	}
	// Known but superseded versions (V2) get the sentinel so storage
	// backends can turn them into misses (ADR-0010 §schema_version 移行);
	// unknown versions above stay hard errors so an older binary never
	// clobbers records written by a newer one.
	if v != SchemaVersion {
		return fmt.Errorf("%w: %s is superseded by %s; the record is regenerated on the next run (ADR-0010)", ErrUnsupportedSchemaVersion, v, SchemaVersion)
	}
	return nil
}

// Sort normalises the order of repeated fields whose schema requires
// deterministic ordering (output.files by path, input.resolved_versions by
// name → version → source). Marshal calls this internally; callers
// comparing two records can invoke it explicitly to make sure both sides
// are in canonical order.
//
// resolved_versions sort uses a composite key because ResolvedVersion.Name
// is not guaranteed unique: the script resolver derives Name from
// filepath.Base(exec[0]), so two distinct tools whose exec heads share a
// basename (e.g. ["go", "version"] vs ["go", "tool", "compile", "-V"])
// both produce Name == "go". With a name-only sort the relative order of
// such entries would depend on insertion order, breaking byte stability
// for the same logical input set.
func Sort(rec *fingerprintv1.Record) {
	if out := rec.GetOutput(); out != nil {
		sort.SliceStable(out.Files, func(i, j int) bool {
			return out.Files[i].GetPath() < out.Files[j].GetPath()
		})
	}
	if in := rec.GetInput(); in != nil {
		sort.SliceStable(in.ResolvedVersions, func(i, j int) bool {
			a, b := in.ResolvedVersions[i], in.ResolvedVersions[j]
			if a.GetName() != b.GetName() {
				return a.GetName() < b.GetName()
			}
			if a.GetVersion() != b.GetVersion() {
				return a.GetVersion() < b.GetVersion()
			}
			return a.GetSource() < b.GetSource()
		})
	}
}

// FilePaths returns just the path strings of files in their current order.
// Use after Sort if a sorted slice is required.
func FilePaths(files []*fingerprintv1.FileEntry) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.GetPath()
	}
	return out
}

// MarshalJSON returns the canonical protojson representation of rec.
// Shared by `sloff fingerprint show` and the runner E2E harness so the human-readable
// view of a record is produced from a single set of options.
//
// The output is canonical: repeated fields are sorted via Sort before
// marshalling so a hand-crafted or non-canonical .pb file decodes to the same
// JSON as a runner-written one. Sort works in place on a clone so callers
// that hold a reference to rec don't see their slice order shift.
//
// protojson intentionally randomises the whitespace after every `:` to
// discourage byte-stable comparisons; we re-flow the bytes through
// json.Compact + json.Indent so the output is reproducible across calls
// while still preserving the proto declaration order of keys (encoding/json
// keeps the original token order when transforming a raw JSON byte slice).
func MarshalJSON(rec *fingerprintv1.Record) ([]byte, error) {
	canonical := proto.Clone(rec).(*fingerprintv1.Record)
	Sort(canonical)
	raw, err := protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: false,
	}.Marshal(canonical)
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, fmt.Errorf("fingerprint: compact protojson output: %w", err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, compact.Bytes(), "", "  "); err != nil {
		return nil, fmt.Errorf("fingerprint: re-indent protojson output: %w", err)
	}
	return indented.Bytes(), nil
}
