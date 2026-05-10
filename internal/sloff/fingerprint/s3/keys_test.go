package s3

import (
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

func TestObjectKey_RoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		rootPrefix string
		key        fingerprint.Key
		timestamp  string
		want       string
	}{
		{
			name:       "with prefix",
			rootPrefix: "sloff/fingerprints",
			key:        fingerprint.Key{SpecRelpath: "path/to/spec", TaskID: "gen", InputHash: "abc"},
			timestamp:  "20260510123456789",
			want:       "sloff/fingerprints/path/to/spec/gen/20260510123456789-abc.pb",
		},
		{
			name:       "empty prefix",
			rootPrefix: "",
			key:        fingerprint.Key{SpecRelpath: "spec", TaskID: "t", InputHash: "h"},
			timestamp:  "20260101000000000",
			want:       "spec/t/20260101000000000-h.pb",
		},
		{
			name:       "prefix with surrounding slashes",
			rootPrefix: "/p/",
			key:        fingerprint.Key{SpecRelpath: "spec", TaskID: "t", InputHash: "h"},
			timestamp:  "20260101000000000",
			want:       "p/spec/t/20260101000000000-h.pb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := objectKey(tt.rootPrefix, tt.key, tt.timestamp)
			if got != tt.want {
				t.Errorf("objectKey() = %q, want %q", got, tt.want)
			}
			spec, task, hash, ts, ok := parseObjectKey(tt.rootPrefix, got)
			if !ok {
				t.Fatalf("parseObjectKey(%q): ok=false", got)
			}
			if spec != tt.key.SpecRelpath || task != tt.key.TaskID || hash != tt.key.InputHash || ts != tt.timestamp {
				t.Errorf("parseObjectKey = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
					spec, task, hash, ts, tt.key.SpecRelpath, tt.key.TaskID, tt.key.InputHash, tt.timestamp)
			}
		})
	}
}

func TestParseObjectKey_RejectsForeign(t *testing.T) {
	cases := []string{
		"sloff/fingerprints/spec/t/abc.pb",                   // legacy hash-only filename (ADR-0010 pre-prefix)
		"sloff/fingerprints/spec/t/notatimestamp-abc.pb",     // dash present but not all digits
		"sloff/fingerprints/spec/t/abcdefghijklmnopq-abc.pb", // 17 chars but non-numeric
		"sloff/fingerprints/spec/t/20260510123456789-.pb",    // empty hash
		"sloff/fingerprints/spec/t/20260510123456789-h.txt",  // wrong extension
		"other/path/spec/t/20260510123456789-h.pb",           // outside rootPrefix
		"sloff/fingerprints/abc.pb",                          // missing task segment
	}
	for _, fullKey := range cases {
		if _, _, _, _, ok := parseObjectKey("sloff/fingerprints", fullKey); ok {
			t.Errorf("parseObjectKey(%q) accepted a foreign key", fullKey)
		}
	}
}

func TestObjectPrefix_TrailingSlash(t *testing.T) {
	got := objectPrefix("p", fingerprint.Key{SpecRelpath: "spec", TaskID: "t"})
	if got != "p/spec/t/" {
		t.Errorf("objectPrefix = %q", got)
	}
	if got := objectPrefix("", fingerprint.Key{SpecRelpath: "", TaskID: "t"}); got != "t/" {
		t.Errorf("empty spec/empty prefix = %q, want %q", got, "t/")
	}
}

func TestSuffixForHash(t *testing.T) {
	if got := suffixForHash("deadbeef"); got != "-deadbeef.pb" {
		t.Errorf("suffixForHash = %q", got)
	}
}
