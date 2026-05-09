package cache_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"

	sloffv1 "github.com/izumin5210/sloff/internal/proto/sloff/v1"
	"github.com/izumin5210/sloff/internal/sloff/cache"
)

func sampleRecord() *cache.Record {
	return &cache.Record{
		GeneratedAt:   time.Date(2026, 5, 5, 12, 34, 56, 0, time.UTC),
		SchemaVersion: cache.SchemaVersion,
		Spec: cache.RecordSpec{
			Cmd:    "buf generate --template buf.gen.yaml",
			Dir:    "path/to/spec",
			TaskID: "protoc-gen-go",
		},
		Input: cache.Input{
			Hash:                 "3f9a1c",
			FilesHash:            "a1b2",
			CmdHash:              "c3d4",
			ResolvedVersionsHash: "e5f6",
			ResolvedVersions: cache.ResolvedVersions{
				{Name: "protoc-gen-go", Source: "go.mod", Version: "v1.34.2"},
				{Name: "buf", Source: "aqua.yaml", Version: "1.30.0"},
			},
		},
		Output: cache.Output{
			Hash: "7e2b",
			Files: cache.FileHashes{
				{Path: "path/to/spec/foo.pb.go", Hash: "11aa"},
				{Path: "path/to/spec/bar.pb.go", Hash: "22bb"},
			},
		},
	}
}

// TestMarshalSortsOutputFilesByPath guards the path-sorted invariant on the
// proto wire: even if the in-memory FileHashes were unsorted, Marshal /
// Unmarshal must produce a path-ascending sequence in output.files so the
// hash output is reproducible across writers.
func TestMarshalSortsOutputFilesByPath(t *testing.T) {
	b, err := sampleRecord().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	msg := &sloffv1.CacheRecord{}
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
	b, err := sampleRecord().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	msg := &sloffv1.CacheRecord{}
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
	b, err := want.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := cache.Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// Output.Files / Input.ResolvedVersions are sorted on Marshal, so we expect sorted form back.
	want.Output.Files = cache.FileHashes{
		{Path: "path/to/spec/bar.pb.go", Hash: "22bb"},
		{Path: "path/to/spec/foo.pb.go", Hash: "11aa"},
	}
	want.Input.ResolvedVersions = cache.ResolvedVersions{
		{Name: "buf", Source: "aqua.yaml", Version: "1.30.0"},
		{Name: "protoc-gen-go", Source: "go.mod", Version: "v1.34.2"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
	}
}

func TestMarshalIsByteStable(t *testing.T) {
	b1, err := sampleRecord().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	rec, err := cache.Unmarshal(b1)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	b2, err := rec.Marshal()
	if err != nil {
		t.Fatalf("Marshal #2: %v", err)
	}
	if string(b1) != string(b2) {
		t.Errorf("byte-stable round-trip violated:\n--- b1 (%d bytes) ---\n%x\n--- b2 (%d bytes) ---\n%x", len(b1), b1, len(b2), b2)
	}
}

// TestMarshalRejectsUnknownSchemaVersion guards that ad-hoc integer values
// outside the proto enum cannot leak into a cache file. ADR-0009 §"schema_version
// 移行戦略" treats unknown versions as runtime errors rather than silently
// encoded best-effort data.
func TestMarshalRejectsUnknownSchemaVersion(t *testing.T) {
	rec := sampleRecord()
	rec.SchemaVersion = 999
	if _, err := rec.Marshal(); err == nil {
		t.Fatal("Marshal: expected error for unknown schema version, got nil")
	}
}
