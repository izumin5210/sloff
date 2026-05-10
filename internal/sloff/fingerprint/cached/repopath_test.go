package cached

import "testing"

func TestParseGitURL(t *testing.T) {
	cases := []struct {
		name, in, host, path string
		wantErr              bool
	}{
		{name: "ssh alias", in: "git@github.com:izumin5210/sloff.git", host: "github.com", path: "izumin5210/sloff"},
		{name: "ssh alias no .git", in: "git@github.com:izumin5210/sloff", host: "github.com", path: "izumin5210/sloff"},
		{name: "ssh alias deep path", in: "git@gitlab.com:group/sub/repo.git", host: "gitlab.com", path: "group/sub/repo"},
		{name: "https", in: "https://github.com/izumin5210/sloff.git", host: "github.com", path: "izumin5210/sloff"},
		{name: "https no .git", in: "https://github.com/izumin5210/sloff", host: "github.com", path: "izumin5210/sloff"},
		{name: "http", in: "http://example.com/o/r.git", host: "example.com", path: "o/r"},
		{name: "ssh scheme", in: "ssh://git@github.com/izumin5210/sloff", host: "github.com", path: "izumin5210/sloff"},
		{name: "git scheme", in: "git://github.com/o/r", host: "github.com", path: "o/r"},

		{name: "ssh alias missing path", in: "git@github.com:", wantErr: true},
		{name: "ssh alias missing colon", in: "git@github.com/foo/bar", wantErr: true},
		{name: "ssh alias empty host", in: "git@:foo/bar", wantErr: true},
		{name: "https missing path", in: "https://github.com/", wantErr: true},
		{name: "missing scheme + colon", in: "github.com/o/r", wantErr: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			host, path, err := parseGitURL(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got host=%q path=%q", tt.in, host, path)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tt.host || path != tt.path {
				t.Errorf("parseGitURL(%q) = (%q, %q), want (%q, %q)", tt.in, host, path, tt.host, tt.path)
			}
		})
	}
}
