package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPackItemIDs(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "books.jsonl")
	if err := os.WriteFile(good, []byte(
		`{"id":"pooh","vector":[1,0]}

{"id":"alice","vector":[0,1]}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := packItemIDs(good)
	if err != nil {
		t.Fatalf("packItemIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids=%v want 2 (blank line skipped)", ids)
	}
	if _, ok := ids["pooh"]; !ok {
		t.Errorf("missing pooh: %v", ids)
	}

	// Missing id → error.
	bad := filepath.Join(dir, "bad.jsonl")
	_ = os.WriteFile(bad, []byte(`{"vector":[1,0]}`), 0o644)
	if _, err := packItemIDs(bad); err == nil {
		t.Error("packItemIDs(bad) = nil, want error for missing id")
	}

	// Missing file → error.
	if _, err := packItemIDs(filepath.Join(dir, "nope.jsonl")); err == nil {
		t.Error("packItemIDs(nope) = nil, want error")
	}
}
