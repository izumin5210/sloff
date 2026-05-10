package runner

import (
	"testing"

	cachev1 "github.com/izumin5210/sloff/internal/proto/sloff/cache/v1"
)

// TestOutputsEquivalent exercises every branch of the write-skip helper:
// hash mismatch, length mismatch, per-entry mismatch, and the success case.
// The helper drives whether runTask rewrites a record on disk, so each
// branch needs an explicit fixture.
func TestOutputsEquivalent(t *testing.T) {
	mk := func(hash string, files ...*cachev1.FileEntry) *cachev1.Output {
		return &cachev1.Output{Hash: hash, Files: files}
	}
	fe := func(p, h string) *cachev1.FileEntry { return &cachev1.FileEntry{Path: p, Hash: h} }

	cases := []struct {
		name string
		a, b *cachev1.Output
		want bool
	}{
		{
			name: "identical",
			a:    mk("h", fe("a", "1"), fe("b", "2")),
			b:    mk("h", fe("a", "1"), fe("b", "2")),
			want: true,
		},
		{
			name: "identical_unsorted_input",
			a:    mk("h", fe("a", "1"), fe("b", "2")),
			b:    mk("h", fe("b", "2"), fe("a", "1")),
			want: true,
		},
		{
			name: "different_hash",
			a:    mk("h1", fe("a", "1")),
			b:    mk("h2", fe("a", "1")),
			want: false,
		},
		{
			name: "different_file_count",
			a:    mk("h", fe("a", "1"), fe("b", "2")),
			b:    mk("h", fe("a", "1")),
			want: false,
		},
		{
			name: "same_path_different_hash",
			a:    mk("h", fe("a", "1")),
			b:    mk("h", fe("a", "2")),
			want: false,
		},
		{
			name: "different_path_same_hash",
			a:    mk("h", fe("a", "1")),
			b:    mk("h", fe("c", "1")),
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := outputsEquivalent(c.a, c.b); got != c.want {
				t.Errorf("outputsEquivalent = %v, want %v", got, c.want)
			}
		})
	}
}
