package hxid

import (
	"sync"
	"testing"

	"github.com/mr-tron/base58"
)

func TestNewUnique(t *testing.T) {
	const n = 10000
	const workers = 16

	seen := sync.Map{}
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < n/workers; i++ {
				s := New().String()
				if _, dup := seen.LoadOrStore(s, struct{}{}); dup {
					t.Errorf("duplicate id: %s", s)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestStringSortable(t *testing.T) {
	a := New().String()
	b := New().String()
	if !(a < b) {
		t.Fatalf("expected a < b lexicographically, got a=%q b=%q", a, b)
	}
}

func TestStringDecodes(t *testing.T) {
	s := New().String()
	decoded, err := base58.Decode(s)
	if err != nil {
		t.Fatalf("base58.Decode failed: %v", err)
	}
	if len(decoded) != 16 {
		t.Fatalf("expected 16 bytes, got %d", len(decoded))
	}
}

func TestNewTimeSortAlias(t *testing.T) {
	// Both should produce well-formed, decodable IDs of the same shape.
	a := New().String()
	b := NewTimeSort().String()
	for _, s := range []string{a, b} {
		decoded, err := base58.Decode(s)
		if err != nil || len(decoded) != 16 {
			t.Fatalf("invalid id %q: err=%v len=%d", s, err, len(decoded))
		}
	}
}
