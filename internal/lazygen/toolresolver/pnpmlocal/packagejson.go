package pnpmlocal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// PackageJSON is the subset of <pkg>/package.json the resolver needs.
type PackageJSON struct {
	Name string
	// Bin holds the resolved bin entry paths (package-relative). When package.json
	// declared `bin` as a string, Bin has length 1; for the object form, every
	// value is included sorted ascending.
	Bin []string
	// Main mirrors `main` verbatim (empty if unset). Useful for the preflight
	// checker to decide whether the package needs a build (bin pointing into
	// dist/) vs ts-node/tsx-style direct source execution.
	Main string
	// EntryPoints is what the lister consumes: union of Bin (preferred) or Main
	// when Bin is empty. Sorted ascending so esbuild's input order is stable.
	EntryPoints []string
}

// LoadPackageJSON reads <repoRoot>/<pkgDir>/package.json and parses it.
func LoadPackageJSON(repoRoot, pkgDir string) (*PackageJSON, error) {
	p := filepath.Join(repoRoot, pkgDir, "package.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	pj, err := ParsePackageJSON(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return pj, nil
}

// ParsePackageJSON parses a package.json byte payload.
func ParsePackageJSON(b []byte) (*PackageJSON, error) {
	var raw struct {
		Name string          `json:"name"`
		Bin  json.RawMessage `json:"bin,omitempty"`
		Main string          `json:"main,omitempty"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("invalid package.json: %w", err)
	}
	bin, err := decodeBin(raw.Bin)
	if err != nil {
		return nil, err
	}
	pj := &PackageJSON{Name: raw.Name, Bin: bin, Main: raw.Main}
	switch {
	case len(bin) > 0:
		pj.EntryPoints = append(pj.EntryPoints, bin...)
	case raw.Main != "":
		pj.EntryPoints = []string{raw.Main}
	}
	return pj, nil
}

// decodeBin handles both shapes npm/pnpm allow: a single string entry or
// {name: path} object. Anything else (array, number, ...) is rejected because
// the resolver cannot decide a meaningful entry set from it.
func decodeBin(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	}

	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("package.json: bin must be a string or object {name: path}, got %s", raw)
	}
	out := make([]string, 0, len(m))
	for _, v := range m {
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}
