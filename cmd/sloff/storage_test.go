package main

import (
	"context"
	"os"
	"os/exec"
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

func gitInitWithOrigin(t *testing.T, root, origin string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		if err := exec.Command("git", append([]string{"-C", root}, args...)...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if err := exec.Command("git", "-C", root, "remote", "add", "origin", origin).Run(); err != nil {
		t.Fatalf("git remote add: %v", err)
	}
}

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
	writeConfig(t, root, "fingerprint:\n  backend: dynamodb\n")
	if _, err := loadStorage(context.Background(), root); err == nil {
		t.Fatal("expected error when dynamodb section is missing")
	}
}

func TestLoadStorage_DynamoDBRejectsMissingTable(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, "fingerprint:\n  backend: dynamodb\n  dynamodb:\n    region: us-east-1\n")
	if _, err := loadStorage(context.Background(), root); err == nil {
		t.Fatal("expected error when dynamodb.table is empty")
	}
}

func TestLoadStorage_DynamoDBRequiresGitRemote(t *testing.T) {
	// dynamodb backend wraps inner with cached.New, which derives its
	// path from the repo's git remote. A repo without `origin` configured
	// must surface that as a clear error rather than silently fall back.
	root := t.TempDir()
	writeConfig(t, root, "fingerprint:\n  backend: dynamodb\n  dynamodb:\n    table: t\n    region: us-east-1\n")
	if _, err := loadStorage(context.Background(), root); err == nil {
		t.Fatal("expected error when repo lacks origin remote")
	}
}

func TestLoadStorage_DynamoDBSuccess(t *testing.T) {
	root := t.TempDir()
	gitInitWithOrigin(t, root, "https://github.com/izumin5210/sloff.git")
	writeConfig(t, root, "fingerprint:\n  backend: dynamodb\n  dynamodb:\n    table: t\n    region: us-east-1\n    endpoint: http://localhost:0\n")

	got, err := loadStorage(context.Background(), root)
	if err != nil {
		t.Fatalf("loadStorage: %v", err)
	}
	// cached decorator passes through inner.Name(); the dynamodb backend
	// reports "dynamodb".
	if got.Name() != "dynamodb" {
		t.Errorf("Name() = %q, want dynamodb", got.Name())
	}
}
