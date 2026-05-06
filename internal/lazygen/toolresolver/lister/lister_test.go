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
	calls    atomic.Int32
	listing  lister.Listing
	err      error
	perEntry map[string]lister.Listing
}

func (s *stubLister) List(_ context.Context, entry string) (lister.Listing, error) {
	s.calls.Add(1)
	if s.err != nil {
		return lister.Listing{}, s.err
	}
	if s.perEntry != nil {
		if l, ok := s.perEntry[entry]; ok {
			return l, nil
		}
	}
	return s.listing, nil
}

func TestMemoized_CachesByEntry(t *testing.T) {
	stub := &stubLister{
		perEntry: map[string]lister.Listing{
			"./cmd/a": {InternalFiles: []string{"cmd/a/main.go"}},
			"./cmd/b": {InternalFiles: []string{"cmd/b/main.go"}},
		},
	}
	m := lister.NewMemoized(stub)

	for range 3 {
		got, err := m.List(context.Background(), "./cmd/a")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := lister.Listing{InternalFiles: []string{"cmd/a/main.go"}}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("a mismatch (-want +got):\n%s", diff)
		}
	}
	if _, err := m.List(context.Background(), "./cmd/b"); err != nil {
		t.Fatalf("List b: %v", err)
	}

	// Same entry hits cache; distinct entries each invoke the inner lister once.
	if got := stub.calls.Load(); got != 2 {
		t.Errorf("inner List invoked %d times, want 2 (one per distinct entry)", got)
	}
}

func TestMemoized_DoesNotCacheErrors(t *testing.T) {
	wantErr := errors.New("transient")
	stub := &stubLister{err: wantErr}
	m := lister.NewMemoized(stub)

	for range 3 {
		if _, err := m.List(context.Background(), "./cmd/x"); !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
	}
	if got := stub.calls.Load(); got != 3 {
		t.Errorf("inner List invoked %d times, want 3 (errors must not be cached)", got)
	}
}
