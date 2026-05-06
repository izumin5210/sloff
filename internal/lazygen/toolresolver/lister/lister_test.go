package lister_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

// stubLister records every List call and returns a programmable response.
type stubLister struct {
	calls   atomic.Int32
	listing lister.Listing
	err     error
	perKey  map[string]lister.Listing // key: specDir + "\x00" + entry
}

func (s *stubLister) List(_ context.Context, specDir, entry string) (lister.Listing, error) {
	s.calls.Add(1)
	if s.err != nil {
		return lister.Listing{}, s.err
	}
	if s.perKey != nil {
		if l, ok := s.perKey[specDir+"\x00"+entry]; ok {
			return l, nil
		}
	}
	return s.listing, nil
}

func TestMemoized_CachesByKey(t *testing.T) {
	stub := &stubLister{
		perKey: map[string]lister.Listing{
			"\x00./cmd/a":    {InternalFiles: []string{"cmd/a/main.go"}},
			"\x00./cmd/b":    {InternalFiles: []string{"cmd/b/main.go"}},
			"sub\x00./cmd/a": {InternalFiles: []string{"sub/cmd/a/main.go"}},
		},
	}
	m := lister.NewMemoized(stub)

	for range 3 {
		got, err := m.List(context.Background(), "", "./cmd/a")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := lister.Listing{InternalFiles: []string{"cmd/a/main.go"}}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("a mismatch (-want +got):\n%s", diff)
		}
	}
	if _, err := m.List(context.Background(), "", "./cmd/b"); err != nil {
		t.Fatalf("List b: %v", err)
	}
	// Same entry under a different specDir is a distinct cache key.
	if _, err := m.List(context.Background(), "sub", "./cmd/a"); err != nil {
		t.Fatalf("List sub/a: %v", err)
	}

	// Same key hits cache; distinct keys each invoke the inner lister once.
	if got := stub.calls.Load(); got != 3 {
		t.Errorf("inner List invoked %d times, want 3 (one per distinct (specDir, entry))", got)
	}
}

func TestMemoized_DoesNotCacheErrors(t *testing.T) {
	wantErr := errors.New("transient")
	stub := &stubLister{err: wantErr}
	m := lister.NewMemoized(stub)

	for range 3 {
		if _, err := m.List(context.Background(), "", "./cmd/x"); !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
	}
	if got := stub.calls.Load(); got != 3 {
		t.Errorf("inner List invoked %d times, want 3 (errors must not be cached)", got)
	}
}
