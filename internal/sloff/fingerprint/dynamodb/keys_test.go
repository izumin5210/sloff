package dynamodb

import (
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

func TestKeyEncoding_RoundTrip(t *testing.T) {
	cases := []fingerprint.Key{
		{SpecRelpath: "path/to/spec", TaskID: "protoc-gen-go", InputHash: "abc"},
		{SpecRelpath: "single", TaskID: "t", InputHash: "h"},
		{SpecRelpath: "deep/nested/spec/dir", TaskID: "deeply-named-task", InputHash: "0123456789abcdef"},
	}
	for _, k := range cases {
		t.Run(k.SpecRelpath+"/"+k.TaskID, func(t *testing.T) {
			pk := partitionKey(k)
			sk := sortKey(k)
			if pk != k.SpecRelpath {
				t.Errorf("partitionKey = %q, want %q", pk, k.SpecRelpath)
			}
			task, hash, ok := parseSortKey(sk)
			if !ok {
				t.Fatalf("parseSortKey(%q) failed", sk)
			}
			if task != k.TaskID || hash != k.InputHash {
				t.Errorf("parseSortKey = (%q, %q), want (%q, %q)", task, hash, k.TaskID, k.InputHash)
			}
		})
	}
}

func TestParseSortKey_Malformed(t *testing.T) {
	cases := []string{
		"",
		"nohash",
		"#leadingseparator",
		"trailingseparator#",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			if _, _, ok := parseSortKey(s); ok {
				t.Errorf("parseSortKey(%q) accepted a malformed key", s)
			}
		})
	}
}

func TestSortKeyTaskPrefix(t *testing.T) {
	if got := sortKeyTaskPrefix("gen"); got != "gen#" {
		t.Errorf("sortKeyTaskPrefix = %q, want gen#", got)
	}
}
