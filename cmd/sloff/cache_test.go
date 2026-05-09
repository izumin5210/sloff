package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	cachev1 "github.com/izumin5210/sloff/internal/proto/sloff/cache/v1"
	"github.com/izumin5210/sloff/internal/sloff/cache"
)

// TestRunCacheDiff_TreatsRepeatedFieldOrderAsEqual locks the "semantic diff"
// promise of `sloff cache diff`: two records whose repeated fields are
// shuffled but otherwise identical must compare equal, since cache.Marshal
// already canonicalises that order on write. Bypassing cache.Marshal lets
// us prove the comparison itself is order-insensitive (rather than relying
// on the writer to have sorted).
func TestRunCacheDiff_TreatsRepeatedFieldOrderAsEqual(t *testing.T) {
	dir := t.TempDir()

	makeRecord := func(filesOrder []*cachev1.FileEntry, versionsOrder []*cachev1.ResolvedVersion) *cachev1.Record {
		return &cachev1.Record{
			SchemaVersion: cache.SchemaVersion,
			Spec:          &cachev1.Spec{Dir: "spec", TaskId: "copy", Cmd: "echo hi"},
			Input: &cachev1.Input{
				Hash:                 "deadbeef",
				FilesHash:            "files",
				CmdHash:              "cmd",
				ResolvedVersionsHash: "versions",
				ResolvedVersions:     versionsOrder,
			},
			Output: &cachev1.Output{
				Hash:  "out",
				Files: filesOrder,
			},
		}
	}

	files := []*cachev1.FileEntry{
		{Path: "a.txt", Hash: "h-a"},
		{Path: "b.txt", Hash: "h-b"},
	}
	versions := []*cachev1.ResolvedVersion{
		{Name: "buf", Version: "v1"},
		{Name: "protoc-gen-go", Version: "v2"},
	}

	a := makeRecord(files, versions)
	b := makeRecord(
		[]*cachev1.FileEntry{files[1], files[0]},
		[]*cachev1.ResolvedVersion{versions[1], versions[0]},
	)

	pathA := writePB(t, dir, "a.pb", a)
	pathB := writePB(t, dir, "b.pb", b)

	var out bytes.Buffer
	if err := runCacheDiff(&out, pathA, pathB); err != nil {
		t.Errorf("expected no diff for repeated-field order difference, got err=%v output=%s", err, out.String())
	}
}

// writePB encodes rec via raw proto.Marshal (NOT cache.Marshal) so we can
// preserve the in-memory order of repeated fields the test wants to exercise.
func writePB(t *testing.T, dir, name string, rec *cachev1.Record) string {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(rec)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
