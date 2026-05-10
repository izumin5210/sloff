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
	for _, name := range []fingerprint.BackendName{fingerprint.BackendLocal, fingerprint.BackendDynamoDB} {
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

func TestLoadStorage_DynamoDBRequiresSection(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".sloff")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// `backend: dynamodb` with no nested `dynamodb:` block — the builder
	// should reject this rather than silently fall back to default values.
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("fingerprint:\n  backend: dynamodb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStorage(context.Background(), root); err == nil {
		t.Fatal("expected error when dynamodb section is missing")
	}
}
