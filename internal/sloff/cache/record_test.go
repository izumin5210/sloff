package cache_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/cache"
)

func sampleRecord() *cache.Record {
	return &cache.Record{
		GeneratedAt: time.Date(2026, 5, 5, 12, 34, 56, 0, time.UTC),
		GeneratorVersionSnapshot: cache.GeneratorVersions{
			{Name: "protoc-gen-go", Source: "go.mod", Version: "v1.34.2"},
			{Name: "buf", Source: "aqua.yaml", Version: "1.30.0"},
		},
		Input: cache.Input{
			Components: cache.InputComponents{
				CmdHash:   "c3d4",
				FilesHash: "a1b2",
				ToolsHash: "e5f6",
			},
			Hash: "3f9a1c",
		},
		Output: cache.Output{
			Files: cache.FileHashes{
				{Path: "path/to/spec/foo.pb.go", Hash: "11aa"},
				{Path: "path/to/spec/bar.pb.go", Hash: "22bb"},
			},
			Hash: "7e2b",
		},
		SchemaVersion: 1,
		Spec: cache.RecordSpec{
			Cmd:    "buf generate --template buf.gen.yaml",
			Dir:    "path/to/spec",
			TaskID: "protoc-gen-go",
		},
	}
}

func TestMarshalEmitsAlphabeticalTopLevelKeys(t *testing.T) {
	b, err := sampleRecord().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	want := []string{
		"generated_at",
		"generator_version_snapshot",
		"input",
		"output",
		"schema_version",
		"spec",
	}
	got := topLevelKeys(string(b))
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("top-level key order mismatch (-want +got):\n%s\nfull yaml:\n%s", diff, b)
	}
}

func TestMarshalSortsOutputFilesByPath(t *testing.T) {
	b, err := sampleRecord().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)
	bar := strings.Index(s, "path/to/spec/bar.pb.go")
	foo := strings.Index(s, "path/to/spec/foo.pb.go")
	if bar < 0 || foo < 0 {
		t.Fatalf("expected both file paths in output:\n%s", s)
	}
	if bar > foo {
		t.Errorf("output.files must be sorted ascending; bar at %d, foo at %d", bar, foo)
	}
}

func TestMarshalSortsGeneratorVersionSnapshotByName(t *testing.T) {
	b, err := sampleRecord().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)
	bufIdx := strings.Index(s, "name: buf")
	pgIdx := strings.Index(s, "name: protoc-gen-go")
	if bufIdx < 0 || pgIdx < 0 {
		t.Fatalf("expected both names in output:\n%s", s)
	}
	if bufIdx > pgIdx {
		t.Errorf("generator_version_snapshot must be sorted by name; buf at %d, protoc-gen-go at %d", bufIdx, pgIdx)
	}
}

func TestMarshalEndsWithSingleNewline(t *testing.T) {
	b, err := sampleRecord().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Errorf("output must end with LF, got %q", b)
	}
	if len(b) >= 2 && b[len(b)-2] == '\n' {
		t.Errorf("output must end with single LF (no trailing blank line), got %q", b[len(b)-3:])
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
	// Output.Files / GeneratorVersionSnapshot are sorted on Marshal, so we expect sorted form back.
	want.Output.Files = cache.FileHashes{
		{Path: "path/to/spec/bar.pb.go", Hash: "22bb"},
		{Path: "path/to/spec/foo.pb.go", Hash: "11aa"},
	}
	want.GeneratorVersionSnapshot = cache.GeneratorVersions{
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
		t.Errorf("byte-stable round-trip violated\n--- first ---\n%s\n--- second ---\n%s", b1, b2)
	}
}

// topLevelKeys extracts top-level YAML keys (lines starting at column 0 with `key:`).
func topLevelKeys(s string) []string {
	var keys []string
	for line := range strings.SplitSeq(s, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '-' || line[0] == '#' {
			continue
		}
		if i := strings.IndexByte(line, ':'); i > 0 {
			keys = append(keys, line[:i])
		}
	}
	return keys
}
