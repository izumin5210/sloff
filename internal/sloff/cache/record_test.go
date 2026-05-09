package cache_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	cachev1 "github.com/izumin5210/sloff/internal/proto/sloff/cache/v1"
	"github.com/izumin5210/sloff/internal/sloff/cache"
)

func sampleRecord() *cachev1.Record {
	return &cachev1.Record{
		GeneratedAt:   timestamppb.New(time.Date(2026, 5, 5, 12, 34, 56, 0, time.UTC)),
		SchemaVersion: cache.SchemaVersion,
		Spec: &cachev1.Spec{
			Cmd:    "buf generate --template buf.gen.yaml",
			Dir:    "path/to/spec",
			TaskId: "protoc-gen-go",
		},
		Input: &cachev1.Input{
			Hash:                 "3f9a1c",
			FilesHash:            "a1b2",
			CmdHash:              "c3d4",
			ResolvedVersionsHash: "e5f6",
			ResolvedVersions: []*cachev1.ResolvedVersion{
				{Name: "protoc-gen-go", Source: "go.mod", Version: "v1.34.2"},
				{Name: "buf", Source: "aqua.yaml", Version: "1.30.0"},
			},
		},
		Output: &cachev1.Output{
			Hash: "7e2b",
			Files: []*cachev1.FileEntry{
				{Path: "path/to/spec/foo.pb.go", Hash: "11aa"},
				{Path: "path/to/spec/bar.pb.go", Hash: "22bb"},
			},
		},
	}
}

// TestMarshalSortsOutputFilesByPath guards the path-sorted invariant on the
// proto wire: even if the in-memory FileEntry slice was unsorted, Marshal /
// Unmarshal must produce a path-ascending sequence in output.files so the
// hash output is reproducible across writers.
func TestMarshalSortsOutputFilesByPath(t *testing.T) {
	b, err := cache.Marshal(sampleRecord())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	msg := &cachev1.Record{}
	if err := proto.Unmarshal(b, msg); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	want := []string{"path/to/spec/bar.pb.go", "path/to/spec/foo.pb.go"}
	got := make([]string, 0, len(msg.GetOutput().GetFiles()))
	for _, f := range msg.GetOutput().GetFiles() {
		got = append(got, f.GetPath())
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("output.files order mismatch (-want +got):\n%s", diff)
	}
}

// TestMarshalSortsResolvedVersionsByName guards the name-sorted invariant on
// the proto wire for input.resolved_versions, which absorbs the previous
// generator_version_snapshot field per ADR-0009.
func TestMarshalSortsResolvedVersionsByName(t *testing.T) {
	b, err := cache.Marshal(sampleRecord())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	msg := &cachev1.Record{}
	if err := proto.Unmarshal(b, msg); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	want := []string{"buf", "protoc-gen-go"}
	got := make([]string, 0, len(msg.GetInput().GetResolvedVersions()))
	for _, v := range msg.GetInput().GetResolvedVersions() {
		got = append(got, v.GetName())
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("input.resolved_versions order mismatch (-want +got):\n%s", diff)
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	want := sampleRecord()
	b, err := cache.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := cache.Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// Marshal sorts repeated fields in place; the want fixture now reflects
	// the canonical order so cmp.Diff sees equivalent records.
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestMarshalIsByteStable(t *testing.T) {
	b1, err := cache.Marshal(sampleRecord())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	rec, err := cache.Unmarshal(b1)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	b2, err := cache.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal #2: %v", err)
	}
	if string(b1) != string(b2) {
		t.Errorf("byte-stable round-trip violated:\n--- b1 (%d bytes) ---\n%x\n--- b2 (%d bytes) ---\n%x", len(b1), b1, len(b2), b2)
	}
}

// TestMarshalRejectsUnknownSchemaVersion guards that ad-hoc enum values
// outside the proto enum registry cannot leak into a cache file. ADR-0009 §
// "schema_version 移行戦略" treats unknown versions as runtime errors rather
// than silently encoded best-effort data.
func TestMarshalRejectsUnknownSchemaVersion(t *testing.T) {
	rec := sampleRecord()
	rec.SchemaVersion = cachev1.SchemaVersion(999)
	if _, err := cache.Marshal(rec); err == nil {
		t.Fatal("Marshal: expected error for unknown schema version, got nil")
	}
}

// TestUnmarshalRejectsZeroBytes guards against silently treating a zero-byte
// or otherwise default-valued record as a usable cache entry. proto.Unmarshal
// happily turns empty input into a Record with SCHEMA_VERSION_UNSPECIFIED;
// the runner would then evaluate it as an existing record and either claim a
// false hit or silently overwrite valid bytes. Surface the corruption instead.
func TestUnmarshalRejectsZeroBytes(t *testing.T) {
	if _, err := cache.Unmarshal(nil); err == nil {
		t.Error("Unmarshal: expected error for empty bytes (decodes to SCHEMA_VERSION_UNSPECIFIED)")
	}
}

// TestUnmarshalRejectsUnknownSchemaVersion is the read-side counterpart of
// TestMarshalRejectsUnknownSchemaVersion: a future binary that emits a newer
// schema_version must not be silently downgraded by an older binary. We
// build the bytes via raw proto.Marshal to bypass cache.Marshal's writer-side
// validation and verify the load path catches it.
func TestUnmarshalRejectsUnknownSchemaVersion(t *testing.T) {
	rec := sampleRecord()
	rec.SchemaVersion = cachev1.SchemaVersion(999)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(rec)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	if _, err := cache.Unmarshal(b); err == nil {
		t.Error("Unmarshal: expected error for unknown schema version")
	}
}

// TestSortCanonicalisesResolvedVersionsAcrossInsertionOrder guards against
// name-only sort ambiguity. ResolvedVersion.Name is not guaranteed unique
// (the script resolver derives it from filepath.Base of exec[0]) so two
// distinct tools can share a Name. Sort must produce the same ordering
// regardless of how the entries were appended, otherwise byte stability
// of the marshaled record depends on insertion order.
func TestSortCanonicalisesResolvedVersionsAcrossInsertionOrder(t *testing.T) {
	// Same set of entries, different insertion orders. After Sort both
	// records must compare equal.
	entries := []*cachev1.ResolvedVersion{
		{Name: "go", Version: "script:go@compile1.x", Source: "script:go-build"},
		{Name: "go", Version: "script:go@go1.26.0", Source: "script:go-runtime"},
		{Name: "buf", Version: "script:buf@1.30.0", Source: "script:buf"},
	}

	forward := &cachev1.Record{Input: &cachev1.Input{ResolvedVersions: append([]*cachev1.ResolvedVersion(nil), entries...)}}
	reverse := &cachev1.Record{Input: &cachev1.Input{ResolvedVersions: []*cachev1.ResolvedVersion{entries[2], entries[1], entries[0]}}}

	cache.Sort(forward)
	cache.Sort(reverse)

	if diff := cmp.Diff(forward, reverse, protocmp.Transform()); diff != "" {
		t.Errorf("Sort must canonicalise regardless of insertion order:\n%s", diff)
	}
}
