package controlapply

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestCoerceB64 covers the {"$b64":…} → []byte convention that lets BLOB
// columns (the secret store's ciphertext etc.) ride the JSON RowsArtifact.
func TestCoerceB64(t *testing.T) {
	raw := []byte{0x00, 0xff, 0x10, 0x99, 0xde, 0xad, 0xbe, 0xef}
	wrapped := map[string]any{"$b64": base64.StdEncoding.EncodeToString(raw)}
	got, ok := coerce(wrapped).([]byte)
	if !ok {
		t.Fatalf("coerce returned %T, want []byte", coerce(wrapped))
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("coerce decoded %x, want %x", got, raw)
	}

	// A multi-key map is NOT a $b64 wrapper — pass through unchanged so a
	// malformed payload fails loudly downstream rather than silently binding.
	multi := map[string]any{"$b64": base64.StdEncoding.EncodeToString(raw), "extra": float64(1)}
	if _, isBytes := coerce(multi).([]byte); isBytes {
		t.Fatal("multi-key map must not be treated as $b64")
	}

	// Bad base64 also passes through unchanged.
	bad := map[string]any{"$b64": "not!!valid!!base64"}
	if _, isBytes := coerce(bad).([]byte); isBytes {
		t.Fatal("invalid base64 must not decode")
	}
}

// TestUpsertRowBinaryColumn proves the real consumer path stores raw bytes
// into a BLOB column (not the base64 text) and integral numbers as ints.
func TestUpsertRowBinaryColumn(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE blobs (id TEXT PRIMARY KEY, body BLOB NOT NULL, n INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	raw := []byte{0x00, 0x01, 0xfe, 0xff, 0x80, 0x7f}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	row := map[string]any{
		"id":   "a",
		"body": map[string]any{"$b64": base64.StdEncoding.EncodeToString(raw)},
		"n":    float64(7),
	}
	if err := upsertRow(context.Background(), tx, "blobs", row); err != nil {
		t.Fatalf("upsertRow: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var got []byte
	var n int
	if err := db.QueryRow(`SELECT body, n FROM blobs WHERE id = 'a'`).Scan(&got, &n); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("stored body %x, want %x", got, raw)
	}
	if n != 7 {
		t.Fatalf("n = %d, want 7", n)
	}
}
