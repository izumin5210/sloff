package cached

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

// gitInitWithRemote stages a minimal git repo at root and configures the
// supplied origin URL. Used by TestRepoPath / TestCacheRoot to drive the
// shell-out path of readGitRemote without requiring network access.
func gitInitWithRemote(t *testing.T, root, originURL string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if originURL != "" {
		cmd := exec.Command("git", "-C", root, "remote", "add", "origin", originURL)
		if err := cmd.Run(); err != nil {
			t.Fatalf("git remote add: %v", err)
		}
	}
}

func TestRepoPath_FromGitOrigin(t *testing.T) {
	root := t.TempDir()
	gitInitWithRemote(t, root, "https://github.com/izumin5210/sloff.git")

	got, err := RepoPath(root)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	want := filepath.FromSlash("github.com/izumin5210/sloff")
	if got != want {
		t.Errorf("RepoPath = %q, want %q", got, want)
	}
}

func TestRepoPath_NoRemoteIsError(t *testing.T) {
	root := t.TempDir()
	gitInitWithRemote(t, root, "")
	_, err := RepoPath(root)
	if err == nil {
		t.Fatal("expected error when origin remote is not configured")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Errorf("expected error to mention origin, got %v", err)
	}
}

func TestRepoPath_NotAGitRepoIsError(t *testing.T) {
	root := t.TempDir()
	if _, err := RepoPath(root); err == nil {
		t.Fatal("expected error outside a git checkout")
	}
}

func TestRepoPath_MalformedURLIsError(t *testing.T) {
	root := t.TempDir()
	gitInitWithRemote(t, root, "not-a-valid-url")
	if _, err := RepoPath(root); err == nil {
		t.Fatal("expected parse error for malformed remote URL")
	}
}

func TestCacheRoot_HonoursXDGCacheHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", xdg)
	// On macOS os.UserCacheDir uses ~/Library/Caches and ignores
	// XDG_CACHE_HOME, so this assertion is conditional on the runtime
	// honouring the env var (Linux / generic Unix).
	cacheBase, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("os.UserCacheDir: %v", err)
	}
	if cacheBase != xdg {
		t.Skipf("os.UserCacheDir returned %q, not honouring XDG_CACHE_HOME on this platform", cacheBase)
	}

	root := t.TempDir()
	gitInitWithRemote(t, root, "https://github.com/izumin5210/sloff.git")

	got, err := CacheRoot(root)
	if err != nil {
		t.Fatalf("CacheRoot: %v", err)
	}
	want := filepath.Join(xdg, "sloff", "fingerprints", "github.com", "izumin5210", "sloff")
	if got != want {
		t.Errorf("CacheRoot = %q, want %q", got, want)
	}
}

func TestCacheRoot_PropagatesRepoPathError(t *testing.T) {
	root := t.TempDir()
	if _, err := CacheRoot(root); err == nil {
		t.Fatal("expected CacheRoot to fail when RepoPath fails")
	}
}
