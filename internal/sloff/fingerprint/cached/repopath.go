// Package cached is a fingerprint.Storage decorator that mirrors records to a
// host-local directory under XDG_CACHE_HOME. Wrap a remote backend (e.g.
// DynamoDB) with cached.New so per-task LoadMany serves repeat hits from disk
// without a network round-trip; LocalStorage is already a disk-backed store
// and should not be wrapped.
package cached

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoPath extracts the ghq-style "<host>/<owner>/<repo>" suffix from the
// `origin` remote URL of the git repository at repoRoot. The result is what
// the cached backend uses to namespace its on-disk cache so multiple
// worktrees of the same repo share the cache, and unrelated repos don't
// collide on shared cache directory keys.
//
// Returns an error if the working tree is not a git checkout, has no
// `origin` remote, or the URL is in an unrecognised form. Callers that
// require a cache should treat this as a hard configuration error — the
// cached backend cannot function without a stable namespace.
func RepoPath(repoRoot string) (string, error) {
	url, err := readGitRemote(repoRoot, "origin")
	if err != nil {
		return "", err
	}
	host, path, err := parseGitURL(url)
	if err != nil {
		return "", fmt.Errorf("parse remote url %q: %w", url, err)
	}
	return filepath.Join(host, filepath.FromSlash(path)), nil
}

// readGitRemote shells out to `git config --get` rather than parsing
// .git/config directly so submodules / worktrees / GIT_DIR overrides resolve
// the same way they do for any other tool the user runs in this checkout.
func readGitRemote(repoRoot, name string) (string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "config", "--get", "remote."+name+".url")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", fmt.Errorf("git remote %q not configured for %s", name, repoRoot)
		}
		return "", fmt.Errorf("git config remote.%s.url: %w", name, err)
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("git remote %q is configured but empty", name)
	}
	return url, nil
}

// parseGitURL recognises the SSH alias form (git@host:path) plus the schemed
// forms (https / http / ssh / git). Anything else returns an error.
//
// The trailing ".git" is stripped because both ghq and human convention
// treat it as decorative; including it in the cache path would split the
// cache between users who clone with `.git` and those who don't.
func parseGitURL(raw string) (host, path string, err error) {
	if rest, ok := strings.CutPrefix(raw, "git@"); ok {
		// SSH alias: git@github.com:owner/repo.git
		idx := strings.Index(rest, ":")
		if idx <= 0 || idx == len(rest)-1 {
			return "", "", fmt.Errorf("malformed SSH alias")
		}
		host = rest[:idx]
		path = strings.TrimSuffix(strings.TrimPrefix(rest[idx+1:], "/"), ".git")
		if host == "" || path == "" {
			return "", "", fmt.Errorf("empty host or path")
		}
		return host, path, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("missing host")
	}
	cleaned := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	if cleaned == "" {
		return "", "", fmt.Errorf("missing path")
	}
	return u.Host, cleaned, nil
}

// CacheRoot returns the directory under XDG_CACHE_HOME that the cached
// backend should write into for this repo. Callers normally pass it through
// to New; exported so the cmd layer can inspect / pre-create the path
// without importing internal helpers.
func CacheRoot(repoRoot string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	repoPath, err := RepoPath(repoRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sloff", "fingerprints", repoPath), nil
}
