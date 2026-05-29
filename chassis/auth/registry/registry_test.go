package registry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// invitationSchemaSQL mirrors db/schema/sqlite/0007_actor_invitations.sql.
// Kept inline so registry tests don't depend on filesystem cwd.
const invitationSchemaSQL = `
CREATE TABLE actor_invitations (
	invitation_id TEXT PRIMARY KEY,
	token_hash    TEXT NOT NULL UNIQUE,
	label         TEXT,
	kind          TEXT,
	tenant_id     TEXT,
	capabilities  TEXT NOT NULL,
	created_by    TEXT NOT NULL,
	created_at    TEXT NOT NULL,
	expires_at    TEXT NOT NULL,
	consumed_at   TEXT,
	consumed_by   TEXT,
	revoked_at    TEXT
);
CREATE INDEX idx_actor_invitations_live ON actor_invitations(token_hash)
	WHERE consumed_at IS NULL AND revoked_at IS NULL;
INSERT INTO tenants (tenant_id, slug, created_at) VALUES ('tnt_default', 'default', '2026-01-01T00:00:00Z');
`

// newRegistryDB builds an in-memory SQLite with just enough of the
// auth schema for the registry's queries. Keeping the schema inline
// (rather than pointing at db/schema/sqlite/) keeps the test
// self-contained and not at the mercy of cwd.
func newRegistryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	stmts := []string{
		`CREATE TABLE actors (
			actor_id    TEXT PRIMARY KEY,
			label       TEXT,
			kind        TEXT,
			subject     TEXT,
			tenant      TEXT,
			stack       TEXT,
			super_admin INTEGER NOT NULL DEFAULT 0,
			created_at  TEXT NOT NULL,
			revoked_at  TEXT,
			meta        TEXT
		);`,
		`CREATE TABLE actor_keys (
			key_id     TEXT PRIMARY KEY,
			actor_id   TEXT NOT NULL,
			public_key BLOB NOT NULL,
			algorithm  TEXT NOT NULL,
			created_at TEXT NOT NULL,
			revoked_at TEXT,
			meta       TEXT
		);`,
		`CREATE TABLE actor_capabilities (
			actor_id   TEXT NOT NULL,
			capability TEXT NOT NULL,
			scope_json TEXT,
			created_at TEXT NOT NULL,
			revoked_at TEXT
		);`,
		`CREATE TABLE tenants (
			tenant_id  TEXT PRIMARY KEY,
			slug       TEXT NOT NULL UNIQUE,
			name       TEXT,
			created_at TEXT NOT NULL,
			revoked_at TEXT
		);`,
		`CREATE TABLE actor_memberships (
			actor_id          TEXT NOT NULL,
			tenant_id         TEXT NOT NULL,
			capabilities_json TEXT NOT NULL,
			created_at        TEXT NOT NULL,
			revoked_at        TEXT,
			PRIMARY KEY (actor_id, tenant_id)
		);`,
		invitationSchemaSQL,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return db
}

// TestHasAnyActiveActorEmpty — empty registry returns false. This is
// the trigger condition for the admin server's first-boot bootstrap.
func TestHasAnyActiveActorEmpty(t *testing.T) {
	r := New(newRegistryDB(t), nil)
	got, err := r.HasAnyActiveActor(context.Background())
	if err != nil {
		t.Fatalf("HasAnyActiveActor: %v", err)
	}
	if got {
		t.Errorf("got true on empty actors table; want false")
	}
}

// TestHasAnyActiveActorOneActive — a single active actor returns true.
// This is the burn-after-use state: once enrolment writes an actor,
// the next bootstrap attempt must see it.
func TestHasAnyActiveActorOneActive(t *testing.T) {
	r := New(newRegistryDB(t), nil)
	if err := r.CreateActor(context.Background(), Actor{
		ActorID: "actor_a",
		Label:   "test",
	}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	got, err := r.HasAnyActiveActor(context.Background())
	if err != nil {
		t.Fatalf("HasAnyActiveActor: %v", err)
	}
	if !got {
		t.Errorf("got false with one active actor; want true")
	}
}

// TestHasAnyActiveActorOnlyRevoked — a revoked-only registry must
// behave like an empty one. A chassis that revoked its last admin
// should re-enable auto-bootstrap on the next start (recovery path).
func TestHasAnyActiveActorOnlyRevoked(t *testing.T) {
	ctx := context.Background()
	db := newRegistryDB(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO actors (actor_id, created_at, revoked_at) VALUES ('actor_x', ?, ?)`,
		now, now); err != nil {
		t.Fatalf("insert revoked actor: %v", err)
	}
	got, err := New(db, nil).HasAnyActiveActor(ctx)
	if err != nil {
		t.Fatalf("HasAnyActiveActor: %v", err)
	}
	if got {
		t.Errorf("got true with only revoked actors; want false")
	}
}

// --- invitation tests ------------------------------------------------------

// mintInvitation is a small helper for the invitation tests. Returns
// the raw token + the persisted invitation row.
func mintInvitation(t *testing.T, r *Registry, ttl time.Duration) (string, Invitation) {
	t.Helper()
	token := "test-token-" + t.Name()
	hash := HashToken(token)
	inv := Invitation{
		InvitationID: "inv_test",
		TokenHash:    hash,
		Label:        "alice",
		Capabilities: []string{"admin:all"},
		CreatedBy:    "actor_admin",
		ExpiresAt:    time.Now().UTC().Add(ttl),
	}
	// Seed the inviter so the FK is satisfied.
	if _, err := r.DB.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO actors (actor_id, created_at) VALUES ('actor_admin', ?)`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed inviter: %v", err)
	}
	if err := r.CreateInvitation(context.Background(), inv); err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	return token, inv
}

// TestConsumeInvitationHappy — the canonical redeem flow: token →
// new actor + key + capability row, and the invitation row marked
// consumed.
func TestConsumeInvitationHappy(t *testing.T) {
	r := New(newRegistryDB(t), nil)
	ctx := context.Background()
	token, _ := mintInvitation(t, r, time.Hour)

	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.ConsumeInvitation(ctx, HashToken(token), "actor_alice", "key_alice", pub, "alice", "human")
	if err != nil {
		t.Fatalf("ConsumeInvitation: %v", err)
	}
	if got == nil || got.Invitation.InvitationID != "inv_test" {
		t.Fatalf("unexpected consume result: %+v", got)
	}
	if got.Reused {
		t.Errorf("fresh consume should not be Reused")
	}
	if got.ActorID != "actor_alice" || got.KeyID != "key_alice" {
		t.Errorf("expected freshly-minted ids; got actor=%q key=%q", got.ActorID, got.KeyID)
	}

	// Actor row exists.
	a, err := r.LookupActor(ctx, "actor_alice")
	if err != nil {
		t.Fatalf("LookupActor: %v", err)
	}
	if a.Label != "alice" {
		t.Errorf("actor label = %q, want alice", a.Label)
	}
	// Key row exists.
	k, err := r.LookupKey(ctx, "key_alice")
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}
	if !bytes.Equal(k.PublicKey, pub) {
		t.Errorf("pubkey mismatch")
	}
	// Phase 8b: actor_capabilities is gone; the consumer's
	// permissions live in actor_memberships scoped to the
	// invitation's tenant. Confirm the membership row was written.
	m, err := r.LoadMembership(ctx, "actor_alice", DefaultTenantID)
	if err != nil {
		t.Fatalf("LoadMembership: %v", err)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != "admin:all" {
		t.Errorf("membership capabilities = %v, want [admin:all]", m.Capabilities)
	}
	// Invitation now consumed.
	inv2, err := r.LookupInvitationByTokenHash(ctx, HashToken(token))
	if err != nil {
		t.Fatalf("LookupInvitationByTokenHash: %v", err)
	}
	if inv2.ConsumedAt == nil || inv2.ConsumedBy != "actor_alice" {
		t.Errorf("invitation not marked consumed: %+v", inv2)
	}
}

// TestConsumeInvitationSingleUse — second consume of the same token
// must fail with ErrNotFound regardless of who's trying.
func TestConsumeInvitationSingleUse(t *testing.T) {
	r := New(newRegistryDB(t), nil)
	ctx := context.Background()
	token, _ := mintInvitation(t, r, time.Hour)
	pub1, _, _ := ed25519.GenerateKey(nil)
	pub2, _, _ := ed25519.GenerateKey(nil)

	if _, err := r.ConsumeInvitation(ctx, HashToken(token), "actor_a", "key_a", pub1, "a", ""); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := r.ConsumeInvitation(ctx, HashToken(token), "actor_b", "key_b", pub2, "b", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second consume: got %v, want ErrNotFound", err)
	}
}

// TestConsumeInvitationExpired — expired tokens are not honoured.
func TestConsumeInvitationExpired(t *testing.T) {
	r := New(newRegistryDB(t), nil)
	ctx := context.Background()
	token, _ := mintInvitation(t, r, -time.Minute) // already past
	pub, _, _ := ed25519.GenerateKey(nil)
	if _, err := r.ConsumeInvitation(ctx, HashToken(token), "actor_x", "key_x", pub, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestConsumeInvitationRevoked — revoked tokens are not honoured.
func TestConsumeInvitationRevoked(t *testing.T) {
	r := New(newRegistryDB(t), nil)
	ctx := context.Background()
	token, _ := mintInvitation(t, r, time.Hour)
	if err := r.RevokeInvitation(ctx, "inv_test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	if _, err := r.ConsumeInvitation(ctx, HashToken(token), "actor_x", "key_x", pub, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestConsumeInvitationUnknownToken — completely unknown token →
// ErrNotFound.
func TestConsumeInvitationUnknownToken(t *testing.T) {
	r := New(newRegistryDB(t), nil)
	ctx := context.Background()
	pub, _, _ := ed25519.GenerateKey(nil)
	if _, err := r.ConsumeInvitation(ctx, HashToken("never-issued"), "actor_x", "key_x", pub, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestListInvitationsRoundtrip — Create + List returns the row back
// with capabilities decoded.
func TestListInvitationsRoundtrip(t *testing.T) {
	r := New(newRegistryDB(t), nil)
	mintInvitation(t, r, time.Hour)
	out, err := r.ListInvitations(context.Background())
	if err != nil {
		t.Fatalf("ListInvitations: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d rows, want 1", len(out))
	}
	got := out[0]
	if got.Label != "alice" || got.CreatedBy != "actor_admin" {
		t.Errorf("metadata wrong: %+v", got)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "admin:all" {
		t.Errorf("caps = %v, want [admin:all]", got.Capabilities)
	}
}
