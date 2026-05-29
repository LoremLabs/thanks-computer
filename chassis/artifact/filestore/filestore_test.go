package filestore

import (
	"context"
	"errors"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/artifact"
)

func TestRegisteredAndRoundTrip(t *testing.T) {
	s, err := artifact.Open("file", artifact.StoreConfig{FileDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open via registry: %v", err)
	}
	if s.Name() != "file" {
		t.Errorf("name = %q, want file", s.Name())
	}
	ctx := context.Background()
	ref := "stacks/t_123/web/42"

	if ok, _ := s.Exists(ctx, ref); ok {
		t.Errorf("ref should be absent before Put")
	}
	if _, _, err := s.Get(ctx, ref); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("Get absent = %v, want ErrNotFound", err)
	}

	data := []byte("dump-sql-bytes")
	man := []byte(`{"kind":"bootstrap.sqlite.dump"}`)
	if err := s.Put(ctx, ref, data, man); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok, _ := s.Exists(ctx, ref); !ok {
		t.Errorf("ref should exist after Put")
	}
	gotData, gotMan, err := s.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(gotData) != string(data) || string(gotMan) != string(man) {
		t.Errorf("round-trip mismatch: data=%q man=%q", gotData, gotMan)
	}

	// Idempotent overwrite with identical content is fine.
	if err := s.Put(ctx, ref, data, man); err != nil {
		t.Errorf("re-Put: %v", err)
	}
}

func TestUnknownBackend(t *testing.T) {
	if _, err := artifact.Open("does-not-exist", artifact.StoreConfig{}); err == nil {
		t.Fatalf("expected error for unregistered backend")
	}
}
