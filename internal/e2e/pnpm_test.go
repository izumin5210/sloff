// Package e2e holds heavy end-to-end tests that exercise sloff against real
// external tools (network downloads, real installs). They are kept out of the
// per-package unit suites so their resource usage cannot destabilize
// timing-sensitive suites sharing a runner (fingerprint/dynamodb's kumo
// startup, notably). Everything here is gated on testing.Short(): the CI
// unit/coverage job runs with -short, and the dedicated test-e2e job runs
// this package on its own runner. The hermetic golden-based E2E tests under
// internal/sloff/runner are a different tier and stay in the unit job.
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/sloff/internal/sloff/toolresolver/pnpmlocal"
)

// End-to-end coverage against real pnpm binaries. The unit tests in
// toolresolver/pnpmlocal pin the lockfile format we BELIEVE pnpm writes;
// these tests pin the format pnpm ACTUALLY writes, so a future pnpm release
// that changes the lockfile layout (as v12 did with its multi-document
// stream) fails here first instead of surfacing as a confusing resolver
// error in a consuming repo. Requires network (GitHub Releases +
// registry.npmjs.org).
const (
	// e2ePnpmV11 pins the single-document lockfile era.
	e2ePnpmV11 = "11.9.0"
	// e2ePnpmV12 pins the multi-document era: a leading self-pin document
	// (packageManagerDependencies) followed by the actual lockfile document,
	// with only the final document copied to node_modules/.pnpm/lock.yaml.
	e2ePnpmV12 = "12.1.0"
)

// docMarker matches a YAML end-of-directives marker line, the same shape
// lastYAMLDocument splits on.
var docMarker = regexp.MustCompile(`(?m)^---[ \t]*\r?$`)

func TestE2E_RealPnpm(t *testing.T) {
	if testing.Short() {
		t.Skip("network e2e (downloads pnpm and installs from npm registry); skipped in -short mode")
	}

	cases := []struct {
		version  string
		multiDoc bool
	}{
		{version: e2ePnpmV11, multiDoc: false},
		{version: e2ePnpmV12, multiDoc: true},
	}
	for _, tc := range cases {
		t.Run("v"+tc.version, func(t *testing.T) {
			t.Parallel()

			bin := fetchPnpm(t, tc.version)
			root := t.TempDir()
			writeFixtureWorkspace(t, root, tc.version)
			runPnpmInstall(t, bin, root)

			// Format guard: if this fails, pnpm changed its lockfile layout and
			// every assumption below (and in lastYAMLDocument) must be revisited.
			lockBytes, err := os.ReadFile(filepath.Join(root, pnpmlocal.LockfileName))
			if err != nil {
				t.Fatal(err)
			}
			if gotMultiDoc := docMarker.Match(lockBytes); gotMultiDoc != tc.multiDoc {
				t.Fatalf("pnpm %s wrote multi-document lockfile = %v, want %v; layout changed upstream:\n%s",
					tc.version, gotMultiDoc, tc.multiDoc, head(lockBytes, 20))
			}

			lf, err := pnpmlocal.LoadLockfile(root)
			if err != nil {
				t.Fatalf("LoadLockfile: %v", err)
			}
			wantPaths := []string{".", "packages/tool", "packages/util"}
			if diff := cmp.Diff(wantPaths, lf.WorkspacePaths()); diff != "" {
				t.Errorf("WorkspacePaths mismatch (-want +got):\n%s", diff)
			}
			if _, ok := lf.Snapshots["ms@2.1.3"]; !ok {
				t.Errorf("snapshots should contain ms@2.1.3, got %v", lf.Snapshots)
			}
			// The self-pin document records pnpm's own binaries; none of that may
			// leak into the parsed lockfile view.
			for key := range lf.Snapshots {
				if strings.HasPrefix(key, "@pnpm/exe") || key == "pnpm@"+tc.version {
					t.Errorf("self-pin document snapshot %q leaked into the parsed lockfile", key)
				}
			}

			ws, err := pnpmlocal.LoadWorkspace(root)
			if err != nil {
				t.Fatalf("LoadWorkspace: %v", err)
			}
			pkg, ok := ws.Lookup("@sloff-e2e/tool")
			if !ok || pkg.Dir != filepath.FromSlash("packages/tool") {
				t.Errorf("Lookup(@sloff-e2e/tool) = %+v, %v; want dir packages/tool", pkg, ok)
			}

			walk, err := pnpmlocal.WalkDeps(lf, "packages/tool")
			if err != nil {
				t.Fatalf("WalkDeps: %v", err)
			}
			if diff := cmp.Diff([]string{"packages/tool", "packages/util"}, walk.Workspaces); diff != "" {
				t.Errorf("WalkDeps workspaces mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]string{"ms@2.1.3"}, walk.Externals); diff != "" {
				t.Errorf("WalkDeps externals mismatch (-want +got):\n%s", diff)
			}

			if err := pnpmlocal.AssertInstallInSync(root); err != nil {
				t.Errorf("AssertInstallInSync after real install: %v", err)
			}

			// Editing the lockfile without rerunning install must still register
			// as drift — the per-document comparison may not blunt the check.
			f, err := os.OpenFile(filepath.Join(root, pnpmlocal.LockfileName), os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("\n# manual edit\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if err := pnpmlocal.AssertInstallInSync(root); !errors.Is(err, pnpmlocal.ErrInstallStale) {
				t.Errorf("AssertInstallInSync after lockfile edit = %v, want ErrInstallStale", err)
			}
		})
	}
}

// writeFixtureWorkspace lays out a minimal two-package workspace exercising
// both walk fronts: tool --link--> util --npm--> ms. packageManager pins the
// exact version of the binary under test so pnpm's self-management neither
// switches versions nor (on v12) skips writing the self-pin document.
func writeFixtureWorkspace(t *testing.T, root, pnpmVersion string) {
	t.Helper()
	mustWrite(t, filepath.Join(root, "package.json"), fmt.Sprintf(`{
  "name": "sloff-e2e-root",
  "private": true,
  "packageManager": "pnpm@%s"
}
`, pnpmVersion))
	mustWrite(t, filepath.Join(root, "pnpm-workspace.yaml"), "packages:\n  - packages/*\n")
	mustWrite(t, filepath.Join(root, "packages", "tool", "package.json"), `{
  "name": "@sloff-e2e/tool",
  "version": "0.0.0",
  "dependencies": {
    "@sloff-e2e/util": "workspace:*"
  }
}
`)
	mustWrite(t, filepath.Join(root, "packages", "util", "package.json"), `{
  "name": "@sloff-e2e/util",
  "version": "0.0.0",
  "dependencies": {
    "ms": "2.1.3"
  }
}
`)
}

func runPnpmInstall(t *testing.T, bin, root string) {
	t.Helper()
	cmd := exec.Command(bin, "install", "--ignore-scripts")
	cmd.Dir = root
	// Keep writes inside the fixture and resolution independent of any
	// user-level .npmrc (registry overrides, auth helpers).
	cmd.Env = append(
		os.Environ(),
		"CI=true",
		"npm_config_registry=https://registry.npmjs.org/",
		"npm_config_store_dir="+filepath.Join(root, ".pnpm-store"),
		"npm_config_cache_dir="+filepath.Join(root, ".pnpm-cache"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pnpm install (%s): %v\n%s", bin, err, out)
	}
}

// fetchPnpm returns a runnable standalone pnpm binary of the given version,
// downloading it from GitHub Releases into the user cache dir on first use.
// The standalone build is used so the test needs no Node.js/corepack on the
// host — the same distribution aqua/nix consumers run in production. The
// whole archive is extracted, not just the "pnpm" entry: v11 standalone ships
// the launcher alongside bundled JS it require()s at runtime, while v12 is a
// single binary.
func fetchPnpm(t *testing.T, version string) string {
	t.Helper()
	asset, ok := pnpmAssetName()
	if !ok {
		t.Skipf("no standalone pnpm asset mapping for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("user cache dir: %v", err)
	}
	destDir := filepath.Join(cacheRoot, "sloff-e2e", "pnpm-standalone-"+version)
	if bin, err := findPnpmBinary(destDir); err == nil {
		return bin
	}

	url := fmt.Sprintf("https://github.com/pnpm/pnpm/releases/download/v%s/%s", version, asset)
	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("download %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download %s: HTTP %d", url, resp.StatusCode)
	}

	// Extract into a temp dir, then rename atomically so a concurrent or
	// interrupted run never leaves a half-extracted tree at the cached path.
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		t.Fatal(err)
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(destDir), "extract-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	if err := extractTarGz(resp.Body, tmpDir); err != nil {
		t.Fatalf("extract %s: %v", url, err)
	}
	if err := os.Rename(tmpDir, destDir); err != nil {
		// A parallel subtest may have won the rename; use its result if valid.
		if bin, findErr := findPnpmBinary(destDir); findErr == nil {
			return bin
		}
		t.Fatal(err)
	}

	bin, err := findPnpmBinary(destDir)
	if err != nil {
		t.Fatalf("%s: %v", asset, err)
	}
	return bin
}

// findPnpmBinary locates the extracted executable named "pnpm" under dir.
func findPnpmBinary(dir string) (string, error) {
	var found string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "pnpm" {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", errors.New(`no "pnpm" executable in extracted archive`)
	}
	if err := os.Chmod(found, 0o755); err != nil {
		return "", err
	}
	return found, nil
}

func pnpmAssetName() (string, bool) {
	assets := map[string]string{
		"darwin/arm64": "pnpm-darwin-arm64.tar.gz",
		"darwin/amd64": "pnpm-darwin-x64.tar.gz",
		"linux/amd64":  "pnpm-linux-x64.tar.gz",
		"linux/arm64":  "pnpm-linux-arm64.tar.gz",
	}
	asset, ok := assets[runtime.GOOS+"/"+runtime.GOARCH]
	return asset, ok
}

func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel := filepath.Clean(filepath.FromSlash(hdr.Name))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry escapes destination: %q", hdr.Name)
		}
		target := filepath.Join(destDir, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// The standalone archives ship node_modules/.bin/* as relative symlinks.
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("absolute symlink target in archive: %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			// Deduplicated files (e.g. LICENSE copies) arrive as hard links whose
			// Linkname is archive-root relative.
			linkRel := filepath.Clean(filepath.FromSlash(hdr.Linkname))
			if filepath.IsAbs(linkRel) || linkRel == ".." || strings.HasPrefix(linkRel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("hard link target escapes destination: %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(destDir, linkRel), target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %d for %q", hdr.Typeflag, hdr.Name)
		}
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

// head returns the first n lines for failure messages.
func head(b []byte, n int) []byte {
	lines := bytes.SplitAfterN(b, []byte("\n"), n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return bytes.Join(lines, nil)
}
