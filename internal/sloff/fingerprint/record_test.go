package fingerprint_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

func sampleRecord() *fingerprintv1.Record {
	return &fingerprintv1.Record{
		SchemaVersion: fingerprint.SchemaVersion,
		Spec: &fingerprintv1.Spec{
			Cmd:    "buf generate --template buf.gen.yaml",
			Dir:    "path/to/spec",
			TaskId: "protoc-gen-go",
		},
		Input: &fingerprintv1.Input{
			Hash:                 "3f9a1c",
			FilesHash:            "a1b2",
			CmdHash:              "c3d4",
			ResolvedVersionsHash: "e5f6",
			ResolvedVersions: []*fingerprintv1.ResolvedVersion{
				{Name: "protoc-gen-go", Source: "go.mod", Version: "v1.34.2"},
				{Name: "buf", Source: "aqua.yaml", Version: "1.30.0"},
			},
		},
		Output: &fingerprintv1.Output{
			Hash: "7e2b",
			Files: []*fingerprintv1.FileEntry{
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
	b, err := fingerprint.Marshal(sampleRecord())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	msg := &fingerprintv1.Record{}
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
	b, err := fingerprint.Marshal(sampleRecord())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	msg := &fingerprintv1.Record{}
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
	b, err := fingerprint.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := fingerprint.Unmarshal(b)
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
	b1, err := fingerprint.Marshal(sampleRecord())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	rec, err := fingerprint.Unmarshal(b1)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	b2, err := fingerprint.Marshal(rec)
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
	rec.SchemaVersion = fingerprintv1.SchemaVersion(999)
	if _, err := fingerprint.Marshal(rec); err == nil {
		t.Fatal("Marshal: expected error for unknown schema version, got nil")
	}
}

// TestUnmarshalRejectsZeroBytes guards against silently treating a zero-byte
// or otherwise default-valued record as a usable cache entry. proto.Unmarshal
// happily turns empty input into a Record with SCHEMA_VERSION_UNSPECIFIED;
// the runner would then evaluate it as an existing record and either claim a
// false hit or silently overwrite valid bytes. Surface the corruption instead.
func TestUnmarshalRejectsZeroBytes(t *testing.T) {
	if _, err := fingerprint.Unmarshal(nil); err == nil {
		t.Error("Unmarshal: expected error for empty bytes (decodes to SCHEMA_VERSION_UNSPECIFIED)")
	}
}

// TestUnmarshalRejectsUnknownSchemaVersion is the read-side counterpart of
// TestMarshalRejectsUnknownSchemaVersion: a future binary that emits a newer
// schema_version must not be silently downgraded by an older binary. We
// build the bytes via raw proto.Marshal to bypass fingerprint.Marshal's writer-side
// validation and verify the load path catches it.
func TestUnmarshalRejectsUnknownSchemaVersion(t *testing.T) {
	rec := sampleRecord()
	rec.SchemaVersion = fingerprintv1.SchemaVersion(999)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(rec)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	if _, err := fingerprint.Unmarshal(b); err == nil {
		t.Error("Unmarshal: expected error for unknown schema version")
	}
}

// TestMarshalJSONCanonicalisesUnsortedInput guards that MarshalJSON returns
// canonical output even when the caller hands in a record whose repeated
// fields are not pre-sorted (e.g. a hand-crafted .pb file decoded directly
// via proto.Unmarshal). Without the internal Sort, `sloff fingerprint show` and
// the E2E harness would surface incidental ordering as JSON diff noise.
func TestMarshalJSONCanonicalisesUnsortedInput(t *testing.T) {
	rec := sampleRecord()
	// Force non-canonical order: reverse-sort by name + by path.
	rec.Input.ResolvedVersions = []*fingerprintv1.ResolvedVersion{
		{Name: "protoc-gen-go", Source: "go.mod", Version: "v1.34.2"},
		{Name: "buf", Source: "aqua.yaml", Version: "1.30.0"},
	}
	rec.Output.Files = []*fingerprintv1.FileEntry{
		{Path: "path/to/spec/foo.pb.go", Hash: "11aa"},
		{Path: "path/to/spec/bar.pb.go", Hash: "22bb"},
	}

	got, err := fingerprint.MarshalJSON(rec)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	// Canonical order is name asc / path asc — buf precedes protoc-gen-go,
	// bar.pb.go precedes foo.pb.go. Verify by index of name/path strings.
	gotStr := string(got)
	if i, j := index(gotStr, `"name": "buf"`), index(gotStr, `"name": "protoc-gen-go"`); i < 0 || j < 0 || i > j {
		t.Errorf("expected buf before protoc-gen-go in JSON output:\n%s", gotStr)
	}
	if i, j := index(gotStr, "bar.pb.go"), index(gotStr, "foo.pb.go"); i < 0 || j < 0 || i > j {
		t.Errorf("expected bar.pb.go before foo.pb.go in JSON output:\n%s", gotStr)
	}
}

// TestMarshalJSONDoesNotMutateInput documents that callers can hand a record
// to MarshalJSON without losing their preferred slice order. The canonical
// sort happens on a clone.
func TestMarshalJSONDoesNotMutateInput(t *testing.T) {
	rec := sampleRecord()
	originalFirstVersion := rec.Input.ResolvedVersions[0].GetName()
	originalFirstFile := rec.Output.Files[0].GetPath()

	if _, err := fingerprint.MarshalJSON(rec); err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if got := rec.Input.ResolvedVersions[0].GetName(); got != originalFirstVersion {
		t.Errorf("MarshalJSON mutated input.resolved_versions order: first[name] = %q, want %q", got, originalFirstVersion)
	}
	if got := rec.Output.Files[0].GetPath(); got != originalFirstFile {
		t.Errorf("MarshalJSON mutated output.files order: first[path] = %q, want %q", got, originalFirstFile)
	}
}

func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestFilePaths exercises the small helper used by runner fingerprint hit logic.
// It only lives in package cache, so direct callers in the runner do not
// register coverage here.
func TestFilePaths(t *testing.T) {
	got := fingerprint.FilePaths([]*fingerprintv1.FileEntry{
		{Path: "a/x.txt", Hash: "h1"},
		{Path: "b/y.txt", Hash: "h2"},
	})
	want := []string{"a/x.txt", "b/y.txt"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("FilePaths mismatch (-want +got):\n%s", diff)
	}
	if got := fingerprint.FilePaths(nil); len(got) != 0 {
		t.Errorf("FilePaths(nil) should yield empty slice, got %v", got)
	}
}

// TestMarshalRejectsNilRecord guards the explicit nil guard at the top of
// Marshal so a buggy caller surfaces as an error rather than a panic.
func TestMarshalRejectsNilRecord(t *testing.T) {
	if _, err := fingerprint.Marshal(nil); err == nil {
		t.Error("Marshal(nil): expected error")
	}
}

// TestUnmarshalRejectsCorruptBytes covers the proto.Unmarshal error branch
// of fingerprint.Unmarshal — schema_version validation only kicks in once the
// wire bytes parse, so malformed bytes must still surface as an error.
func TestUnmarshalRejectsCorruptBytes(t *testing.T) {
	// Bytes that don't form a valid proto message.
	if _, err := fingerprint.Unmarshal([]byte{0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Error("Unmarshal(corrupt bytes): expected error")
	}
}

// TestSortHandlesEmptyRecord covers the Sort branches that early-return when
// Input or Output is unset, so callers can pass partially-built records
// without nil-dereferencing.
func TestSortHandlesEmptyRecord(t *testing.T) {
	rec := &fingerprintv1.Record{SchemaVersion: fingerprint.SchemaVersion}
	fingerprint.Sort(rec) // must not panic when Input / Output are nil
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
	entries := []*fingerprintv1.ResolvedVersion{
		{Name: "go", Version: "script:go@compile1.x", Source: "script:go-build"},
		{Name: "go", Version: "script:go@go1.26.0", Source: "script:go-runtime"},
		{Name: "buf", Version: "script:buf@1.30.0", Source: "script:buf"},
	}

	forward := &fingerprintv1.Record{Input: &fingerprintv1.Input{ResolvedVersions: append([]*fingerprintv1.ResolvedVersion(nil), entries...)}}
	reverse := &fingerprintv1.Record{Input: &fingerprintv1.Input{ResolvedVersions: []*fingerprintv1.ResolvedVersion{entries[2], entries[1], entries[0]}}}

	fingerprint.Sort(forward)
	fingerprint.Sort(reverse)

	if diff := cmp.Diff(forward, reverse, protocmp.Transform()); diff != "" {
		t.Errorf("Sort must canonicalise regardless of insertion order:\n%s", diff)
	}
}
