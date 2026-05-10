// Package hash computes the deterministic hash inputs that drive sloff's fingerprint lookup.
//
// All exported functions are pure: same arguments yield the same hex SHA-256 digest.
// Files reads file contents from disk; the rest operate purely on their arguments.
package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Files returns the SHA-256 digest of the file set rooted at root, where each path is
// joined onto root before reading. Paths are sorted internally so that input order does
// not matter. Both the path and the file content contribute to the digest, so renames
// and content changes are both detected.
func Files(root string, paths []string) (string, error) {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)

	h := sha256.New()
	for _, p := range sorted {
		contentDigest, err := fileSHA256(filepath.Join(root, p))
		if err != nil {
			return "", err
		}
		// path \0 sha256(content) \0 — NUL separators avoid pathological filename collisions.
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(contentDigest)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Cmd returns the SHA-256 digest of the command-line. Argument boundaries are preserved
// via NUL separators so that ["foo bar", "baz"] and ["foo", "bar", "baz"] yield distinct
// digests even though their space-joined forms collide.
func Cmd(cmd []string) string {
	h := sha256.New()
	for i, a := range cmd {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(a))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ResolvedVersions returns the SHA-256 digest of a sorted concatenation of
// resolved version strings (covers user-declared tools, transitive Go module
// pins, and transitive npm package pins — see ADR-0009 for the rename from
// the older "tools" framing).
// Sort is applied internally so that resolver dispatch order does not affect the digest.
func ResolvedVersions(versions []string) string {
	sorted := append([]string(nil), versions...)
	sort.Strings(sorted)
	h := sha256.New()
	for i, v := range sorted {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(v))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Input combines the three component digests deterministically into the input_hash.
// Components are taken as opaque hex strings; Input does not validate their format.
func Input(filesHash, cmdHash, resolvedVersionsHash string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n", filesHash, cmdHash, resolvedVersionsHash)
	return hex.EncodeToString(h.Sum(nil))
}

// File returns the hex SHA-256 of a single file located at filepath.Join(root, path).
func File(root, path string) (string, error) {
	digest, err := fileSHA256(filepath.Join(root, path))
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest), nil
}

func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return h.Sum(nil), nil
}
