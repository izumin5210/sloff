package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

func TestDefaultBuilders_HasBothBackends(t *testing.T) {
	builders := defaultBuilders()
	for _, name := range []fingerprint.BackendName{fingerprint.BackendLocal, fingerprint.BackendS3} {
		if _, ok := builders[name]; !ok {
			t.Errorf("defaultBuilders missing %q", name)
		}
	}
}

func TestLoadStorage_NoConfigDefaultsToLocal(t *testing.T) {
	root := t.TempDir()
	got, err := loadStorage(context.Background(), root)
	if err != nil {
		t.Fatalf("loadStorage: %v", err)
	}
	if got.Name() != "local" {
		t.Errorf("Name() = %q, want local", got.Name())
	}
}

func TestLoadStorage_S3RequiresBucket(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".sloff")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// `backend: s3` with no bucket — the s3 builder should surface the
	// missing-bucket error from s3.New rather than silently fall back to
	// local. This is the only way the s3 branch of defaultBuilders is
	// exercised in unit tests; integration tests in fingerprint/s3 use a
	// real bucket on kumo.
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("fingerprint:\n  backend: s3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadStorage(context.Background(), root)
	if err == nil {
		t.Fatal("expected error for s3 backend without bucket")
	}
}
