package fingerprint_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".sloff")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig_MissingFileIsNotAnError(t *testing.T) {
	cfg, err := fingerprint.LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ResolvedBackend() != fingerprint.BackendLocal {
		t.Errorf("expected local backend on absent config, got %q", cfg.ResolvedBackend())
	}
}

func TestLoadConfig_DynamoDB(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `fingerprint:
  backend: dynamodb
  dynamodb:
    table: sloff-fingerprints
    region: ap-northeast-1
    endpoint: http://localhost:4566
    expires_after_days: 90
`)
	cfg, err := fingerprint.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.ResolvedBackend(); got != fingerprint.BackendDynamoDB {
		t.Errorf("backend = %q, want dynamodb", got)
	}
	d := cfg.Fingerprint.DynamoDB
	if d == nil {
		t.Fatal("expected DynamoDB config to be populated")
	}
	if d.Table != "sloff-fingerprints" {
		t.Errorf("table = %q", d.Table)
	}
	if d.Region != "ap-northeast-1" {
		t.Errorf("region = %q", d.Region)
	}
	if d.Endpoint != "http://localhost:4566" {
		t.Errorf("endpoint = %q", d.Endpoint)
	}
	if d.ExpiresAfterDays != 90 {
		t.Errorf("expires_after_days = %d", d.ExpiresAfterDays)
	}
}

func TestLoadConfig_LocalIsImplicitDefault(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "")
	cfg, err := fingerprint.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ResolvedBackend() != fingerprint.BackendLocal {
		t.Errorf("expected local, got %q", cfg.ResolvedBackend())
	}
}

func TestLoadConfig_ParseError(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "fingerprint: [not-a-mapping]\n")
	if _, err := fingerprint.LoadConfig(root); err == nil {
		t.Fatal("expected parse error")
	}
}
