package aqua_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	preflightaqua "github.com/izumin5210/lazygen/internal/lazygen/preflight/aqua"
)

const validChecksumsJSON = `{
  "checksums": [
    {"id": "github.com/bufbuild/buf/releases/download/v1.30.0/buf-Darwin-arm64.tar.gz", "algorithm": "sha256", "checksum": "abc"},
    {"id": "github.com/kyleconroy/sqlc/releases/download/v1.27.0/sqlc-Darwin-arm64.tar.gz", "algorithm": "sha256", "checksum": "def"}
  ]
}`

func setupRoot(t *testing.T, aquaYAML, checksums string) string {
	t.Helper()
	root := t.TempDir()
	if aquaYAML != "" {
		mustWrite(t, filepath.Join(root, "aqua.yaml"), aquaYAML)
	}
	if checksums != "" {
		mustWrite(t, filepath.Join(root, "aqua-checksums.json"), checksums)
	}
	return root
}

func TestChecker_AllPackagesPresent_OK(t *testing.T) {
	root := setupRoot(t, `packages:
  - name: bufbuild/buf@v1.30.0
  - name: kyleconroy/sqlc@v1.27.0
`, validChecksumsJSON)

	c, err := preflightaqua.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !got.OK {
		t.Errorf("expected OK, got %+v", got)
	}
}

func TestChecker_VersionMismatch_Issue(t *testing.T) {
	root := setupRoot(t, `packages:
  - name: bufbuild/buf@v1.31.0
`, validChecksumsJSON)

	c, err := preflightaqua.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.OK {
		t.Errorf("expected OK=false")
	}
	if len(got.Issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %+v", len(got.Issues), got.Issues)
	}
	if got.Issues[0].Channel != "aqua" {
		t.Errorf("Channel = %q", got.Issues[0].Channel)
	}
	if got.Issues[0].Suggestion != "aqua install" {
		t.Errorf("Suggestion = %q", got.Issues[0].Suggestion)
	}
	if !strings.Contains(got.Issues[0].Detail, "bufbuild/buf") || !strings.Contains(got.Issues[0].Detail, "v1.31.0") {
		t.Errorf("Detail must mention package and version, got %q", got.Issues[0].Detail)
	}
}

func TestChecker_ChecksumsFileMissing_Issue(t *testing.T) {
	root := setupRoot(t, `packages:
  - name: bufbuild/buf@v1.30.0
`, "")

	c, err := preflightaqua.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Check(context.Background(), ".")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.OK || len(got.Issues) == 0 {
		t.Errorf("expected issue when checksums file is missing, got %+v", got)
	}
}

func TestChecker_InvalidChecksumsJSON_Errors(t *testing.T) {
	root := setupRoot(t, `packages:
  - name: bufbuild/buf@v1.30.0
`, "{not json")

	c, err := preflightaqua.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Check(context.Background(), "."); err == nil {
		t.Fatal("expected error for invalid checksums json")
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
