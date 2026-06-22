package admin

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// countingStore is a filecas.Store test double: it records Put count + the peak
// number of concurrent Puts, can pretend some hashes already exist, and can fail
// every Put.
type countingStore struct {
	exists  map[string]bool
	putErr  error
	delay   time.Duration
	puts    int32
	inFlt   int32
	maxInFl int32
}

func (s *countingStore) Put(_ context.Context, _ string, _ []byte) error {
	n := atomic.AddInt32(&s.inFlt, 1)
	for {
		m := atomic.LoadInt32(&s.maxInFl)
		if n <= m || atomic.CompareAndSwapInt32(&s.maxInFl, m, n) {
			break
		}
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	atomic.AddInt32(&s.inFlt, -1)
	atomic.AddInt32(&s.puts, 1)
	return s.putErr
}
func (s *countingStore) Exists(_ context.Context, hash string) (bool, error) { return s.exists[hash], nil }
func (s *countingStore) Get(_ context.Context, _ string) ([]byte, error)     { return nil, nil }
func (s *countingStore) Name() string                                        { return "counting" }

// Unchanged content (hash already present) is skipped — only the delta is Put.
func TestMaterialiseFilesSkipsExisting(t *testing.T) {
	files := map[string]string{"FILES/a.html": "aaa", "FILES/b.html": "bbb", "FILES/c.html": "ccc"}
	s := &countingStore{exists: map[string]bool{sha256Hex("bbb"): true}}
	if me := materialiseFiles(context.Background(), s, files); me != nil {
		t.Fatalf("unexpected error: %+v", me)
	}
	if got := atomic.LoadInt32(&s.puts); got != 2 {
		t.Fatalf("puts = %d, want 2 (the already-present file is skipped)", got)
	}
}

// Puts run concurrently but never exceed the bound.
func TestMaterialiseFilesBoundedConcurrency(t *testing.T) {
	files := make(map[string]string, 200)
	for i := 0; i < 200; i++ {
		files[fmt.Sprintf("FILES/%03d.html", i)] = fmt.Sprintf("content-%d", i)
	}
	s := &countingStore{exists: map[string]bool{}, delay: 3 * time.Millisecond}
	if me := materialiseFiles(context.Background(), s, files); me != nil {
		t.Fatalf("unexpected error: %+v", me)
	}
	if got := atomic.LoadInt32(&s.puts); got != 200 {
		t.Fatalf("puts = %d, want 200", got)
	}
	if max := atomic.LoadInt32(&s.maxInFl); max > materialiseConcurrency {
		t.Fatalf("peak concurrent puts = %d, want <= %d", max, materialiseConcurrency)
	} else if max < 2 {
		t.Fatalf("peak concurrent puts = %d — expected real concurrency", max)
	}
}

// A Put failure aborts and surfaces as a *materialiseError.
func TestMaterialiseFilesPutError(t *testing.T) {
	files := map[string]string{"FILES/a.html": "aaa", "FILES/b.html": "bbb"}
	s := &countingStore{exists: map[string]bool{}, putErr: errors.New("r2 unavailable")}
	me := materialiseFiles(context.Background(), s, files)
	if me == nil {
		t.Fatal("expected a materialiseError, got nil")
	}
	if me.code != "filecas_put" {
		t.Fatalf("code = %q, want filecas_put", me.code)
	}
}
