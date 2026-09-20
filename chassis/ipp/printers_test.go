package ipp

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrinterUpsertGetListDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.GetPrinter(ctx, "acme", "paris"); !errors.Is(err, ErrPrinterNotFound) {
		t.Fatalf("GetPrinter on an empty table: %v", err)
	}

	p, created, err := s.UpsertPrinter(ctx, Printer{
		Tenant: "acme", Label: "paris", PrincipalID: "pony:paris", DisplayName: "Paris", CreatedBy: "web",
	})
	if err != nil || !created {
		t.Fatalf("first upsert: created=%v err=%v", created, err)
	}
	if !p.Active() || p.Name() != "Paris" || p.CreatedBy != "web" || p.CreatedAt.IsZero() {
		t.Fatalf("stored row: %+v", p)
	}

	// Registering again is a no-op that keeps what the row has: an empty
	// display name or status does not clear one, and created_by is the
	// first writer's.
	p, created, err = s.UpsertPrinter(ctx, Printer{Tenant: "acme", Label: "paris", PrincipalID: "pony:paris", CreatedBy: "other"})
	if err != nil || created {
		t.Fatalf("second upsert: created=%v err=%v", created, err)
	}
	if p.DisplayName != "Paris" || p.Status != PrinterActive || p.CreatedBy != "web" {
		t.Fatalf("a bare re-register changed the row: %+v", p)
	}

	// Disable, rename and re-point.
	p, _, err = s.UpsertPrinter(ctx, Printer{
		Tenant: "acme", Label: "paris", PrincipalID: "pony:lyon", DisplayName: "Lyon", Status: PrinterDisabled,
	})
	if err != nil || p.Active() || p.DisplayName != "Lyon" || p.PrincipalID != "pony:lyon" {
		t.Fatalf("update: %+v %v", p, err)
	}

	// Another tenant's label is its own row.
	if _, _, err := s.UpsertPrinter(ctx, Printer{Tenant: "beta", Label: "paris", PrincipalID: "pony:paris"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertPrinter(ctx, Printer{Tenant: "acme", Label: "berlin", PrincipalID: "pony:berlin"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListPrinters(ctx, "acme")
	if err != nil || len(list) != 2 || list[0].Label != "berlin" || list[1].Label != "paris" {
		t.Fatalf("ListPrinters: %+v %v", list, err)
	}
	if list[0].Name() != "berlin" {
		t.Fatalf("a printer with no display name shows its label, got %q", list[0].Name())
	}

	if ok, err := s.DeletePrinter(ctx, "acme", "paris"); err != nil || !ok {
		t.Fatalf("DeletePrinter: %v %v", ok, err)
	}
	if ok, err := s.DeletePrinter(ctx, "acme", "paris"); err != nil || ok {
		t.Fatalf("second DeletePrinter: %v %v, want false", ok, err)
	}
	if _, err := s.GetPrinter(ctx, "beta", "paris"); err != nil {
		t.Fatalf("deleting acme's printer removed beta's: %v", err)
	}
}

func TestUpsertPrinterRefusesWhatNoURLReaches(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, p := range []Printer{
		{Tenant: "", Label: "paris", PrincipalID: "pony:paris"},
		{Tenant: "acme", Label: "Paris", PrincipalID: "pony:paris"},
		{Tenant: "acme", Label: "a/b", PrincipalID: "pony:paris"},
		{Tenant: "acme", Label: "-x", PrincipalID: "pony:paris"},
		{Tenant: "acme", Label: "paris", PrincipalID: ""},
		{Tenant: "acme", Label: "paris", PrincipalID: "pony:paris", Status: "paused"},
	} {
		if _, _, err := s.UpsertPrinter(ctx, p); err == nil {
			t.Errorf("UpsertPrinter(%+v) succeeded", p)
		}
	}
}

func TestClipDisplayName(t *testing.T) {
	if got := ClipDisplayName("Front Desk"); got != "Front Desk" {
		t.Fatalf("short name changed: %q", got)
	}
	long := strings.Repeat("a", 62) + "é" // 64 bytes; the é straddles byte 63
	got := ClipDisplayName(long)
	if len(got) != 62 || strings.ContainsRune(got, '�') {
		t.Fatalf("clip split a rune: %d bytes %q", len(got), got)
	}
	if got := ClipDisplayName(strings.Repeat("b", 200)); len(got) != MaxDisplayName {
		t.Fatalf("clip length = %d", len(got))
	}
}

// Who signed in rides the row, so a dispatcher on another node can pin the
// delivered run to a principal it never saw sign in.
func TestJobPrincipalRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	j, err := s.CreateJob(ctx, NewJob{Tenant: "acme", Printer: "paris", PrincipalID: "pony:paris", CredentialID: "crd_1"})
	if err != nil || j.PrincipalID != "pony:paris" || j.CredentialID != "crd_1" {
		t.Fatalf("CreateJob: %+v %v", j, err)
	}
	got, err := s.GetJob(ctx, "acme", "paris", j.Number)
	if err != nil || got.PrincipalID != "pony:paris" || got.CredentialID != "crd_1" {
		t.Fatalf("GetJob: %+v %v", got, err)
	}
	_, _ = s.SetCommitted(ctx, j.ID, sha, 1, "application/pdf", "")
	leased, _ := s.LeaseCommitted(ctx, "node", 1)
	if len(leased) != 1 || leased[0].PrincipalID != "pony:paris" || leased[0].CredentialID != "crd_1" {
		t.Fatalf("a leased job lost its principal: %+v", leased)
	}
}

// A job table from before principals gains the two columns on the next boot.
// An older build that is still running (a fleet mid-roll, or a rollback)
// keeps inserting without them, and those rows must read back.
func TestEnsureSchemaAddsPrincipalColumnsToAnOldTable(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := schema[0]
	for _, line := range []string{
		"\t   principal_id    TEXT NOT NULL DEFAULT '',\n",
		"\t   credential_id   TEXT NOT NULL DEFAULT '',\n",
	} {
		next := strings.Replace(old, line, "", 1)
		if next == old {
			t.Fatalf("test is stale: %q not found in the schema", strings.TrimSpace(line))
		}
		old = next
	}
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	const oldInsert = `INSERT INTO ipp_jobs (id, tenant, printer, job_number, state, created_at, updated_at)
	                   VALUES (?, 'acme', 'paris', ?, 'committed', '2026-09-18T00:00:00Z', '2026-09-18T00:00:00Z')`
	if _, err := db.Exec(oldInsert, "ipj_before", 1); err != nil {
		t.Fatal(err)
	}
	s := NewStore(db, nil)
	for i := 0; i < 2; i++ { // idempotent
		if err := s.EnsureSchema(ctx); err != nil {
			t.Fatalf("EnsureSchema #%d: %v", i, err)
		}
	}
	// The old build's INSERT still works against the new table.
	if _, err := db.Exec(oldInsert, "ipj_after", 2); err != nil {
		t.Fatalf("an old build's insert after the migration: %v", err)
	}
	for _, id := range []string{"ipj_before", "ipj_after"} {
		got, err := s.GetJobByID(ctx, id)
		if err != nil || got.PrincipalID != "" || got.CredentialID != "" {
			t.Fatalf("%s: %+v %v", id, got, err)
		}
	}
	if _, err := s.GetPrinter(ctx, "acme", "paris"); !errors.Is(err, ErrPrinterNotFound) {
		t.Fatalf("the printers table was not created: %v", err)
	}
}
