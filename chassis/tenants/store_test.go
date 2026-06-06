package tenants

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// newTestStore builds an in-memory SQLite with the tenants table and
// returns a Store against it. Equivalent of the production seed
// migration but inline so the test doesn't depend on filesystem state.
func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE tenants (
			tenant_id  TEXT PRIMARY KEY,
			slug       TEXT NOT NULL UNIQUE,
			name       TEXT,
			created_at TEXT NOT NULL,
			revoked_at TEXT
		);
		CREATE TABLE tenant_hostnames (
			id          TEXT PRIMARY KEY,
			hostname    TEXT NOT NULL,
			tenant_id   TEXT NOT NULL REFERENCES tenants(tenant_id),
			stack       TEXT NOT NULL,
			created_at  TEXT NOT NULL,
			created_by  TEXT,
			revoked_at  TEXT,
			verified_at TEXT,
			dkim_selector    TEXT NOT NULL DEFAULT '',
			dkim_private_pem TEXT NOT NULL DEFAULT '',
			dkim_public_b64  TEXT NOT NULL DEFAULT ''
		);
		CREATE UNIQUE INDEX tenant_hostnames_active_hostname_idx
		    ON tenant_hostnames(hostname)
		    WHERE revoked_at IS NULL;
		CREATE INDEX tenant_hostnames_tenant_idx
		    ON tenant_hostnames(tenant_id);
		CREATE TABLE tenant_hostname_challenges (
			id            TEXT PRIMARY KEY,
			hostname_id   TEXT NOT NULL REFERENCES tenant_hostnames(id),
			method        TEXT NOT NULL CHECK (method IN ('dns-txt','http-01')),
			token         TEXT NOT NULL UNIQUE,
			created_at    TEXT NOT NULL,
			created_by    TEXT,
			expires_at    TEXT NOT NULL,
			attempted_at  TEXT,
			last_error    TEXT,
			verified_at   TEXT,
			revoked_at    TEXT
		);
		CREATE UNIQUE INDEX tenant_hostname_challenges_active_idx
		    ON tenant_hostname_challenges(hostname_id, method)
		    WHERE verified_at IS NULL AND revoked_at IS NULL;
	`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return New(db), db
}

// TestCreateAndLookup — round-trip a tenant row by both id and slug.
// Slug lookups are case-insensitive (we lowercase on write and on read).
func TestCreateAndLookup(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, Tenant{
		TenantID: "tnt_a",
		Slug:     "LoremLabs",
		Name:     "Lorem Labs",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	byID, err := s.Lookup(ctx, "tnt_a")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if byID.Slug != "loremlabs" {
		t.Errorf("slug not lowercased on write: got %q want %q", byID.Slug, "loremlabs")
	}
	if byID.Name != "Lorem Labs" {
		t.Errorf("Name: got %q want %q", byID.Name, "Lorem Labs")
	}

	bySlug, err := s.LookupBySlug(ctx, "LOREMLABS")
	if err != nil {
		t.Fatalf("LookupBySlug: %v", err)
	}
	if bySlug.TenantID != "tnt_a" {
		t.Errorf("slug lookup returned wrong id: %q", bySlug.TenantID)
	}
}

// TestLookupBySlugMissing — unknown slugs return ErrNotFound so the
// admin mux can map cleanly to 404.
func TestLookupBySlugMissing(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.LookupBySlug(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing slug: want ErrNotFound, got %v", err)
	}
}

// TestLookupMissing — same for lookup by id.
func TestLookupMissing(t *testing.T) {
	s, _ := newTestStore(t)
	_, err := s.Lookup(context.Background(), "tnt_nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id: want ErrNotFound, got %v", err)
	}
}

// TestList — returns every non-revoked tenant.
func TestList(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	for _, p := range []struct{ id, slug string }{
		{"tnt_a", "alpha"},
		{"tnt_b", "bravo"},
		{"tnt_c", "charlie"},
	} {
		if err := s.Create(ctx, Tenant{TenantID: p.id, Slug: p.slug}); err != nil {
			t.Fatalf("Create %s: %v", p.id, err)
		}
	}
	// Soft-delete bravo.
	if _, err := db.Exec(`UPDATE tenants SET revoked_at = '2026-01-01T00:00:00Z' WHERE tenant_id = 'tnt_b'`); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 active tenants, got %d", len(got))
	}
	for _, tt := range got {
		if tt.Slug == "bravo" {
			t.Errorf("revoked tenant leaked into List output")
		}
	}
}

// TestCreateRejectsEmpty — defensive: empty id or slug surfaces as an
// error rather than landing a malformed row.
func TestCreateRejectsEmpty(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, Tenant{Slug: "x"}); err == nil {
		t.Errorf("empty tenant_id: want error, got nil")
	}
	if err := s.Create(ctx, Tenant{TenantID: "tnt_x"}); err == nil {
		t.Errorf("empty slug: want error, got nil")
	}
	if err := s.Create(ctx, Tenant{TenantID: "tnt_x", Slug: "   "}); err == nil {
		t.Errorf("whitespace slug: want error, got nil")
	}
}

func TestReservedSlug(t *testing.T) {
	for _, s := range []string{"_sys", "_", "_anything", "_default"} {
		if !ReservedSlug(s) {
			t.Errorf("ReservedSlug(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"sys", "default", "acme", "a_b", "team-1"} {
		if ReservedSlug(s) {
			t.Errorf("ReservedSlug(%q) = true, want false", s)
		}
	}
}

func TestCreateRejectsReservedSlug(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	// The _ prefix is chassis-internal; Create must refuse it even
	// though the slug is otherwise well-formed (defence in depth
	// behind the endpoint's 400).
	if err := s.Create(ctx, Tenant{TenantID: "tnt_x", Slug: "_sys"}); err == nil {
		t.Errorf("reserved slug _sys: want error, got nil")
	}
	if err := s.Create(ctx, Tenant{TenantID: "tnt_y", Slug: "_custom"}); err == nil {
		t.Errorf("reserved slug _custom: want error, got nil")
	}
}

// --- Hostname tests --------------------------------------------------

func mustCreateTenant(t *testing.T, s *Store, tenantID, slug string) {
	t.Helper()
	if err := s.Create(context.Background(), Tenant{TenantID: tenantID, Slug: slug}); err != nil {
		t.Fatalf("Create %s: %v", tenantID, err)
	}
}

// TestCreateHostnameRoundTrip — happy path: create, lookup, list.
func TestCreateHostnameRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mustCreateTenant(t, s, "tnt_a", "alpha")

	got, err := s.CreateHostname(ctx, Hostname{
		Hostname:  "Foo.Local",
		TenantID:  "tnt_a",
		Stack:     "alpha/web",
		CreatedBy: "actor_admin",
	})
	if err != nil {
		t.Fatalf("CreateHostname: %v", err)
	}
	if got.Hostname != "foo.local" {
		t.Errorf("hostname not canonicalized on write: %q", got.Hostname)
	}
	if got.ID == "" || got.ID[:4] != "thn_" {
		t.Errorf("missing or wrong-prefix id: %q", got.ID)
	}

	// Lookup by mixed-case + port returns the same row.
	row, err := s.LookupActiveHostname(ctx, "FOO.LOCAL:8080")
	if err != nil {
		t.Fatalf("LookupActiveHostname: %v", err)
	}
	if row.TenantID != "tnt_a" || row.Stack != "alpha/web" {
		t.Errorf("looked-up row mismatch: %+v", row)
	}

	hs, err := s.ListHostnames(ctx, "tnt_a", false)
	if err != nil {
		t.Fatalf("ListHostnames: %v", err)
	}
	if len(hs) != 1 || hs[0].Hostname != "foo.local" {
		t.Errorf("list: got %+v", hs)
	}
}

// TestCreateHostnameRejectsInvalid — the strict-write predicate gates
// IP literals, bare IPv6, malformed multi-colon, and non-canonical
// shapes.
func TestCreateHostnameRejectsInvalid(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mustCreateTenant(t, s, "tnt_a", "alpha")

	for _, bad := range []string{
		"",
		"1.2.3.4",
		"::1",
		"host:bad:port",
		".",
		"-example.com",
		"example..com",
	} {
		_, err := s.CreateHostname(ctx, Hostname{
			Hostname: bad, TenantID: "tnt_a", Stack: "alpha/web",
		})
		if err == nil {
			t.Errorf("CreateHostname(%q): want error, got nil", bad)
		}
	}
}

// TestCreateHostnameConflict — two tenants try to claim the same
// active hostname; second insert returns ErrHostnameInUse and surfaces
// the existing row's tenant_id.
func TestCreateHostnameConflict(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mustCreateTenant(t, s, "tnt_a", "alpha")
	mustCreateTenant(t, s, "tnt_b", "bravo")

	if _, err := s.CreateHostname(ctx, Hostname{
		Hostname: "shared.local", TenantID: "tnt_a", Stack: "alpha/web",
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	conflict, err := s.CreateHostname(ctx, Hostname{
		Hostname: "shared.local", TenantID: "tnt_b", Stack: "bravo/web",
	})
	if !errors.Is(err, ErrHostnameInUse) {
		t.Fatalf("second create: got %v, want ErrHostnameInUse", err)
	}
	if conflict.TenantID != "tnt_a" {
		t.Errorf("conflict surface: got tenant %q, want tnt_a", conflict.TenantID)
	}
}

// TestRevokeThenReclaim — the load-bearing property of the partial
// unique index. Revoke a hostname, then create a fresh row with the
// same hostname (possibly under a different tenant). Both rows should
// be visible in history mode.
func TestRevokeThenReclaim(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mustCreateTenant(t, s, "tnt_a", "alpha")
	mustCreateTenant(t, s, "tnt_b", "bravo")

	first, err := s.CreateHostname(ctx, Hostname{
		Hostname: "site.local", TenantID: "tnt_a", Stack: "alpha/web",
		CreatedBy: "actor_alice",
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := s.RevokeHostname(ctx, "site.local"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.LookupActiveHostname(ctx, "site.local"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after revoke: want ErrNotFound, got %v", err)
	}
	second, err := s.CreateHostname(ctx, Hostname{
		Hostname: "site.local", TenantID: "tnt_b", Stack: "bravo/web",
		CreatedBy: "actor_bob",
	})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if second.ID == first.ID {
		t.Errorf("reclaim should produce a fresh id; got same %q", first.ID)
	}
	// History mode lists both rows across both tenants.
	historyA, _ := s.ListHostnames(ctx, "tnt_a", true)
	historyB, _ := s.ListHostnames(ctx, "tnt_b", true)
	if len(historyA) != 1 || historyA[0].RevokedAt == nil {
		t.Errorf("tnt_a history: want one revoked row, got %+v", historyA)
	}
	if len(historyB) != 1 || historyB[0].RevokedAt != nil {
		t.Errorf("tnt_b history: want one active row, got %+v", historyB)
	}
}

// --- Challenge tests -------------------------------------------------

// mustCreateHostname seeds a hostname so challenge tests have a
// parent row to attach to.
func mustCreateHostname(t *testing.T, s *Store, tenantID, hostname string) Hostname {
	t.Helper()
	h, err := s.CreateHostname(context.Background(), Hostname{
		Hostname: hostname, TenantID: tenantID, Stack: tenantID + "/web",
	})
	if err != nil {
		t.Fatalf("CreateHostname: %v", err)
	}
	return h
}

// TestCreateChallengeRevokesPrior — the load-bearing property of the
// partial unique index. Issuing a new challenge for the same
// (hostname, method) revokes the prior active row in one transaction.
func TestCreateChallengeRevokesPrior(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mustCreateTenant(t, s, "tnt_a", "alpha")
	h := mustCreateHostname(t, s, "tnt_a", "claim.local")

	first, err := s.CreateChallenge(ctx, h.ID, "dns-txt", "actor_alice", "tcv_first_token")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := s.CreateChallenge(ctx, h.ID, "dns-txt", "actor_alice", "tcv_second_token")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if first.ID == second.ID {
		t.Errorf("expected fresh id, got same %q", first.ID)
	}
	// First row should now be revoked.
	row, err := s.ActiveChallenge(ctx, h.ID, "dns-txt")
	if err != nil {
		t.Fatalf("ActiveChallenge after second create: %v", err)
	}
	if row.ID != second.ID {
		t.Errorf("active is %q, expected second=%q", row.ID, second.ID)
	}
}

// TestCreateChallengeDifferentMethods — dns-txt and http-01 can both
// be active for the same hostname (they're separate state machines).
func TestCreateChallengeDifferentMethods(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mustCreateTenant(t, s, "tnt_a", "alpha")
	h := mustCreateHostname(t, s, "tnt_a", "two-methods.local")

	if _, err := s.CreateChallenge(ctx, h.ID, "dns-txt", "", "tcv_dns_token"); err != nil {
		t.Fatalf("dns-txt: %v", err)
	}
	if _, err := s.CreateChallenge(ctx, h.ID, "http-01", "", "tcv_http_token"); err != nil {
		t.Fatalf("http-01: %v", err)
	}
	if _, err := s.ActiveChallenge(ctx, h.ID, "dns-txt"); err != nil {
		t.Errorf("dns-txt should be active: %v", err)
	}
	if _, err := s.ActiveChallenge(ctx, h.ID, "http-01"); err != nil {
		t.Errorf("http-01 should be active: %v", err)
	}
}

// TestLookupChallengeByTokenAndExpiry — the /.well-known handler's
// read path. Active tokens resolve; revoked or verified tokens don't.
func TestLookupChallengeByTokenAndExpiry(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mustCreateTenant(t, s, "tnt_a", "alpha")
	h := mustCreateHostname(t, s, "tnt_a", "lookup.local")
	c, err := s.CreateChallenge(ctx, h.ID, "http-01", "", "tcv_lookup_token")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.LookupChallengeByToken(ctx, c.Token)
	if err != nil {
		t.Fatalf("lookup active: %v", err)
	}
	if got.ID != c.ID {
		t.Errorf("looked-up id mismatch")
	}
	// Revoking the row hides it from the lookup.
	if _, err := s.CreateChallenge(ctx, h.ID, "http-01", "", "tcv_second"); err != nil {
		t.Fatalf("revoke prior via re-create: %v", err)
	}
	if _, err := s.LookupChallengeByToken(ctx, c.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("after revoke: got %v, want ErrNotFound", err)
	}
}

// TestRecordChallengeAttemptAndMarkVerified — round-trip the verified
// flow: record success, parent row flips verified_at.
func TestRecordChallengeAttemptAndMarkVerified(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mustCreateTenant(t, s, "tnt_a", "alpha")
	h := mustCreateHostname(t, s, "tnt_a", "verify.local")
	c, err := s.CreateChallenge(ctx, h.ID, "dns-txt", "", "tcv_verify_token")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.RecordChallengeAttempt(ctx, c.ID, "", true); err != nil {
		t.Fatalf("record success: %v", err)
	}
	if err := s.MarkHostnameVerified(ctx, h.ID, time.Now().UTC()); err != nil {
		t.Fatalf("mark verified: %v", err)
	}
	got, err := s.LookupActiveHostname(ctx, "verify.local")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.VerifiedAt == nil {
		t.Errorf("parent row should be verified")
	}
	// Active lookup of the challenge should now miss (verified_at
	// took it out of the active partial index).
	if _, err := s.ActiveChallenge(ctx, h.ID, "dns-txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-verify ActiveChallenge: got %v, want ErrNotFound", err)
	}
}

// TestRecordChallengeAttemptFailure — failure path records
// attempted_at + last_error but leaves verified_at NULL.
func TestRecordChallengeAttemptFailure(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mustCreateTenant(t, s, "tnt_a", "alpha")
	h := mustCreateHostname(t, s, "tnt_a", "fail.local")
	c, err := s.CreateChallenge(ctx, h.ID, "dns-txt", "", "tcv_fail_token")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.RecordChallengeAttempt(ctx, c.ID, "NXDOMAIN", false); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	got, err := s.ActiveChallenge(ctx, h.ID, "dns-txt")
	if err != nil {
		t.Fatalf("ActiveChallenge: %v", err)
	}
	if got.AttemptedAt == nil {
		t.Errorf("attempted_at should be set")
	}
	if got.LastError != "NXDOMAIN" {
		t.Errorf("last_error: got %q", got.LastError)
	}
	if got.VerifiedAt != nil {
		t.Errorf("verified_at should still be nil")
	}
}

// TestRevokeHostnameIdempotent — revoking an absent hostname is a
// no-op (no error); revoking an already-revoked one is also a no-op.
func TestRevokeHostnameIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.RevokeHostname(ctx, "never-existed.local"); err != nil {
		t.Errorf("revoke absent: got %v, want nil", err)
	}
	mustCreateTenant(t, s, "tnt_a", "alpha")
	if _, err := s.CreateHostname(ctx, Hostname{
		Hostname: "x.local", TenantID: "tnt_a", Stack: "alpha/web",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.RevokeHostname(ctx, "x.local"); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := s.RevokeHostname(ctx, "x.local"); err != nil {
		t.Errorf("second revoke: got %v, want nil (idempotent)", err)
	}
}
