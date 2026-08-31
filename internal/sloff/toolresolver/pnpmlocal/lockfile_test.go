package pnpmlocal_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
)

func TestLoadLockfile_v9_ImporterPathsExposed(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), `lockfileVersion: '9.0'
importers:
  .:
    dependencies:
      '@org/codegen':
        specifier: workspace:*
        version: link:packages/codegen
  packages/codegen:
    dependencies:
      typescript:
        specifier: ^5.0.0
        version: 5.0.0
  packages/util:
    {}
`)

	lf, err := pnpmlocal.LoadLockfile(root)
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}

	got := lf.WorkspacePaths()
	want := []string{".", "packages/codegen", "packages/util"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("WorkspacePaths mismatch (-want +got):\n%s", diff)
	}
}

// pnpm12HeadDocument mirrors the self-pin document pnpm 12 prepends to
// pnpm-lock.yaml: it records the resolved pnpm binaries themselves
// (packageManagerDependencies / configDependencies) and — crucially for the
// parser — carries the same lockfileVersion as the real lockfile document
// that follows, so a version guard alone cannot tell the two apart.
const pnpm12HeadDocument = `---
lockfileVersion: '9.0'

importers:

  .:
    configDependencies: {}
    packageManagerDependencies:
      '@pnpm/exe':
        specifier: 12.1.0
        version: 12.1.0
      pnpm:
        specifier: 12.1.0
        version: 12.1.0

packages:

  '@pnpm/exe.darwin-arm64@12.1.0':
    resolution: {integrity: sha512-aaaa}
    cpu: [arm64]
    os: [darwin]

snapshots:

  '@pnpm/exe.darwin-arm64@12.1.0':
    optional: true

---
`

// TestLoadLockfile_Pnpm12MultiDocumentPicksLockfileDocument guards the pnpm 12
// lockfile layout: the stream's final document is the actual lockfile, and the
// leading self-pin document must contribute nothing. Decoding only the first
// document would pass the version guard (both documents say 9.0) yet leave
// Importers/Snapshots pointing at pnpm's own binaries — the exact silent-empty
// failure mode the R4 guard exists to prevent.
func TestLoadLockfile_Pnpm12MultiDocumentPicksLockfileDocument(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), pnpm12HeadDocument+`lockfileVersion: '9.0'
importers:
  .:
    dependencies:
      '@org/codegen':
        specifier: workspace:*
        version: link:packages/codegen
  packages/codegen:
    dependencies:
      typescript:
        specifier: ^5.0.0
        version: 5.0.0
snapshots:
  typescript@5.0.0: {}
`)

	lf, err := pnpmlocal.LoadLockfile(root)
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}

	got := lf.WorkspacePaths()
	want := []string{".", "packages/codegen"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("WorkspacePaths mismatch (-want +got):\n%s", diff)
	}
	if v := lf.Importers["packages/codegen"].Dependencies["typescript"].Version; v != "5.0.0" {
		t.Errorf("typescript version = %q, want %q", v, "5.0.0")
	}
	if _, ok := lf.Snapshots["typescript@5.0.0"]; !ok {
		t.Errorf("snapshots should come from the lockfile document, got %v", lf.Snapshots)
	}
	if _, ok := lf.Snapshots["@pnpm/exe.darwin-arm64@12.1.0"]; ok {
		t.Error("snapshots from the self-pin document must not leak into the parsed lockfile")
	}
}

func TestLoadLockfile_MissingFile(t *testing.T) {
	root := t.TempDir()
	if _, err := pnpmlocal.LoadLockfile(root); err == nil {
		t.Fatal("expected error when pnpm-lock.yaml is missing")
	}
}

func TestLoadLockfile_InvalidYAML(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), "importers: [unbalanced")
	if _, err := pnpmlocal.LoadLockfile(root); err == nil {
		t.Fatal("expected error on invalid YAML")
	}
}

// TestLoadLockfile_UnsupportedVersionFailsFast guards the schema-version
// check: pre-v9 lockfiles lay out dependency data under different keys, so
// parsing them with the v9 view would silently yield empty snapshots and
// external dep bumps would stop invalidating fingerprints (R4). The only
// safe response is to fail fast.
func TestLoadLockfile_UnsupportedVersionFailsFast(t *testing.T) {
	cases := []struct {
		name     string
		lockfile string
		wantMsg  string
	}{
		{
			name:     "v6",
			lockfile: "lockfileVersion: '6.0'\nimporters:\n  .: {}\n",
			wantMsg:  `"6.0"`,
		},
		{
			// v5 writes lockfileVersion as an unquoted YAML float;
			// goccy/go-yaml silently converts it to the string "5.4" instead
			// of returning a type error, so this check is the only place
			// that catches it. The case pins that decode behaviour too.
			name:     "v5 unquoted float",
			lockfile: "lockfileVersion: 5.4\nimporters:\n  .: {}\n",
			wantMsg:  `"5.4"`,
		},
		{
			name:     "missing version key",
			lockfile: "importers:\n  .: {}\n",
			wantMsg:  "missing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mustWrite(t, filepath.Join(root, "pnpm-lock.yaml"), tc.lockfile)
			_, err := pnpmlocal.LoadLockfile(root)
			if err == nil {
				t.Fatal("expected unsupported lockfileVersion error")
			}
			if !strings.Contains(err.Error(), "lockfileVersion") || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error should mention unsupported lockfileVersion %s, got: %v", tc.wantMsg, err)
			}
		})
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
