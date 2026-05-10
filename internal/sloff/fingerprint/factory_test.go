package fingerprint_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	fingerprintv1 "github.com/izumin5210/sloff/internal/proto/sloff/fingerprint/v1"
	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

// stubStorage is a minimal fingerprint.Storage used to assert which builder
// LoadStorage selected without dragging the real local / s3 implementations
// into this test (factory.go intentionally has no import on backends, so the
// test mirrors that boundary).
type stubStorage struct{ name string }

func (s stubStorage) Name() string { return s.name }
func (stubStorage) Load(context.Context, fingerprint.Key) (*fingerprintv1.Record, bool, error) {
	return nil, false, nil
}

func (stubStorage) Save(context.Context, fingerprint.Key, *fingerprintv1.Record) error { return nil }

func (stubStorage) Delete(context.Context, fingerprint.Key) error { return nil }

func (stubStorage) List(context.Context, fingerprint.ListFilter) ([]fingerprint.Key, error) {
	return nil, nil
}
func (stubStorage) CollapseDuplicates(context.Context) (int, error) { return 0, nil }

func TestLoadStorage_MissingConfigUsesLocalBuilder(t *testing.T) {
	root := t.TempDir()
	called := ""
	builders := map[fingerprint.BackendName]fingerprint.Builder{
		fingerprint.BackendLocal: func(_ context.Context, _ string, _ fingerprint.Config) (fingerprint.Storage, error) {
			called = "local"
			return stubStorage{name: "local"}, nil
		},
	}
	got, err := fingerprint.LoadStorage(context.Background(), root, builders)
	if err != nil {
		t.Fatalf("LoadStorage: %v", err)
	}
	if called != "local" {
		t.Errorf("expected local builder to be called, got %q", called)
	}
	if got.Name() != "local" {
		t.Errorf("Name() = %q, want local", got.Name())
	}
}

func TestLoadStorage_S3Backend(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `fingerprint:
  backend: s3
  s3:
    bucket: x
`)
	called := ""
	var receivedCfg fingerprint.Config
	builders := map[fingerprint.BackendName]fingerprint.Builder{
		fingerprint.BackendS3: func(_ context.Context, _ string, cfg fingerprint.Config) (fingerprint.Storage, error) {
			called = "s3"
			receivedCfg = cfg
			return stubStorage{name: "s3"}, nil
		},
	}
	got, err := fingerprint.LoadStorage(context.Background(), root, builders)
	if err != nil {
		t.Fatalf("LoadStorage: %v", err)
	}
	if called != "s3" {
		t.Errorf("expected s3 builder to be called, got %q", called)
	}
	if got.Name() != "s3" {
		t.Errorf("Name() = %q, want s3", got.Name())
	}
	if receivedCfg.Fingerprint.S3 == nil || receivedCfg.Fingerprint.S3.Bucket != "x" {
		t.Errorf("builder did not receive parsed S3 config, got %+v", receivedCfg)
	}
}

func TestLoadStorage_UnknownBackendIsError(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `fingerprint:
  backend: hybrid
`)
	_, err := fingerprint.LoadStorage(context.Background(), root, map[fingerprint.BackendName]fingerprint.Builder{
		fingerprint.BackendLocal: func(context.Context, string, fingerprint.Config) (fingerprint.Storage, error) {
			t.Fatal("local builder must not be invoked for an unknown backend")
			return nil, nil
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestLoadStorage_PropagatesConfigParseError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".sloff")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("fingerprint: [oops]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := fingerprint.LoadStorage(context.Background(), root, nil)
	if err == nil {
		t.Fatal("expected parse error to propagate")
	}
}

func TestLoadStorage_PropagatesBuilderError(t *testing.T) {
	root := t.TempDir()
	want := errors.New("boom")
	_, err := fingerprint.LoadStorage(context.Background(), root, map[fingerprint.BackendName]fingerprint.Builder{
		fingerprint.BackendLocal: func(context.Context, string, fingerprint.Config) (fingerprint.Storage, error) {
			return nil, want
		},
	})
	if !errors.Is(err, want) {
		t.Errorf("expected builder error to surface, got %v", err)
	}
}
