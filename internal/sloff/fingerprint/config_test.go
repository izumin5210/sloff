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
		t.Errorf("expected backend local for absent config, got %q", cfg.ResolvedBackend())
	}
}

func TestLoadConfig_S3(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `fingerprint:
  backend: s3
  s3:
    bucket: my-bucket
    prefix: custom/prefix
    region: ap-northeast-1
    endpoint: http://localhost:4566
    use_path_style: true
`)
	cfg, err := fingerprint.LoadConfig(root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.ResolvedBackend(); got != fingerprint.BackendS3 {
		t.Errorf("backend = %q, want s3", got)
	}
	s3 := cfg.Fingerprint.S3
	if s3 == nil {
		t.Fatal("expected non-nil S3 config")
	}
	if s3.Bucket != "my-bucket" {
		t.Errorf("bucket = %q", s3.Bucket)
	}
	if s3.Prefix != "custom/prefix" {
		t.Errorf("prefix = %q", s3.Prefix)
	}
	if s3.Region != "ap-northeast-1" {
		t.Errorf("region = %q", s3.Region)
	}
	if s3.Endpoint != "http://localhost:4566" {
		t.Errorf("endpoint = %q", s3.Endpoint)
	}
	if s3.UsePathStyle == nil || !*s3.UsePathStyle {
		t.Errorf("use_path_style = %v, want true", s3.UsePathStyle)
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
