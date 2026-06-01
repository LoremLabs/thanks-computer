package registry

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func newOIDCDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE oidc_subjects (
		issuer     TEXT NOT NULL,
		subject    TEXT NOT NULL,
		tenant_id  TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (issuer, subject)
	);`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func TestOIDCSubjectRoundTrip(t *testing.T) {
	r := New(newOIDCDB(t), nil)
	ctx := context.Background()

	if _, err := r.LookupOIDCSubject(ctx, "iss", "sub"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown subject: got %v, want ErrNotFound", err)
	}
	if err := r.CreateOIDCSubject(ctx, "iss", "sub", "tnt_1"); err != nil {
		t.Fatalf("CreateOIDCSubject: %v", err)
	}
	got, err := r.LookupOIDCSubject(ctx, "iss", "sub")
	if err != nil {
		t.Fatalf("LookupOIDCSubject: %v", err)
	}
	if got != "tnt_1" {
		t.Fatalf("tenant_id = %q, want tnt_1", got)
	}
	// A different issuer with the same subject is a distinct mapping.
	if _, err := r.LookupOIDCSubject(ctx, "other", "sub"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-issuer leak: got %v, want ErrNotFound", err)
	}
}

func TestOIDCSubjectDuplicate(t *testing.T) {
	r := New(newOIDCDB(t), nil)
	ctx := context.Background()
	if err := r.CreateOIDCSubject(ctx, "iss", "sub", "tnt_1"); err != nil {
		t.Fatalf("CreateOIDCSubject: %v", err)
	}
	err := r.CreateOIDCSubject(ctx, "iss", "sub", "tnt_2")
	if !errors.Is(err, ErrSubjectAlreadyMapped) {
		t.Fatalf("duplicate insert: got %v, want ErrSubjectAlreadyMapped", err)
	}
	// The original mapping must be unchanged.
	got, _ := r.LookupOIDCSubject(ctx, "iss", "sub")
	if got != "tnt_1" {
		t.Fatalf("mapping changed to %q after a rejected duplicate", got)
	}
}
