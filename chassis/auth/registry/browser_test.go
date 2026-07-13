package registry

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-tron/base58"

	_ "github.com/mattn/go-sqlite3"
)

// newBrowserTestDB builds an in-memory sqlite with the minimum tables
// the browser-auth registry methods touch. Production runs SetMaxOpenConns(1)
// for the same reason the admin test fixture does — a stock :memory: DB
// is per-connection, and the race tests would otherwise see fresh empty
// DBs per goroutine.
func newBrowserTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`
		CREATE TABLE actors (actor_id TEXT PRIMARY KEY);
		CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY);
		INSERT INTO actors VALUES ('actor_test');
		INSERT INTO tenants VALUES ('tnt_default');
		CREATE TABLE browser_bootstrap (
			token_hash         TEXT PRIMARY KEY,
			actor_id           TEXT NOT NULL,
			tenant_id          TEXT NOT NULL,
			capabilities_json  TEXT NOT NULL,
			super_admin        INTEGER NOT NULL DEFAULT 0,
			label              TEXT,
			created_at         TEXT NOT NULL,
			expires_at         TEXT NOT NULL,
			consumed_at        TEXT,
			consumed_ip        TEXT
		);
		CREATE TABLE browser_sessions (
			session_id         TEXT PRIMARY KEY,
			actor_id           TEXT NOT NULL,
			tenant_id          TEXT NOT NULL,
			capabilities_json  TEXT NOT NULL,
			super_admin        INTEGER NOT NULL DEFAULT 0,
			ua                 TEXT,
			ip                 TEXT,
			created_at         TEXT NOT NULL,
			expires_at         TEXT NOT NULL,
			revoked_at         TEXT,
			revoked_by         TEXT,
			last_seen_at       TEXT NOT NULL
		);
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

// TestBootstrapTokenShape — sanity-check the format of plaintext
// tokens. Caller code (and operators eyeballing logs) leans on the
// btk_ prefix.
func TestBootstrapTokenShape(t *testing.T) {
	plaintext, hash, err := newBootstrapToken()
	if err != nil {
		t.Fatalf("newBootstrapToken: %v", err)
	}
	if !strings.HasPrefix(plaintext, "btk_") {
		t.Errorf("plaintext %q missing btk_ prefix", plaintext)
	}
	if len(plaintext) < 30 {
		t.Errorf("plaintext too short for 256 bits of entropy: %d", len(plaintext))
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64 (sha256 hex)", len(hash))
	}
	// Two consecutive mints should not collide.
	p2, _, _ := newBootstrapToken()
	if p2 == plaintext {
		t.Errorf("two consecutive tokens collided")
	}
}

func TestCreateAndConsumeBootstrap(t *testing.T) {
	r := New(newBrowserTestDB(t), nil)
	ctx := context.Background()
	caps := []string{"opstack:*:read", "opstack:*:update"}
	plaintext, expiresAt, err := r.CreateBootstrap(ctx,
		"actor_test", "tnt_default", caps, false, "test-label", 60*time.Second)
	if err != nil {
		t.Fatalf("CreateBootstrap: %v", err)
	}
	if time.Until(expiresAt) > 2*time.Minute {
		t.Errorf("expiresAt too far in future: %v", expiresAt)
	}

	b, err := r.ConsumeBootstrap(ctx, plaintext, "10.0.0.1")
	if err != nil {
		t.Fatalf("ConsumeBootstrap: %v", err)
	}
	if b.ActorID != "actor_test" {
		t.Errorf("ActorID = %q, want actor_test", b.ActorID)
	}
	if b.TenantID != "tnt_default" {
		t.Errorf("TenantID = %q, want tnt_default", b.TenantID)
	}
	if len(b.Capabilities) != 2 || b.Capabilities[0] != "opstack:*:read" {
		t.Errorf("Capabilities = %v, want [opstack:*:read opstack:*:update]", b.Capabilities)
	}
	if b.Label != "test-label" {
		t.Errorf("Label = %q, want test-label", b.Label)
	}
	if b.ConsumedAt == nil {
		t.Errorf("ConsumedAt is nil after consume")
	}
	if b.ConsumedIP != "10.0.0.1" {
		t.Errorf("ConsumedIP = %q, want 10.0.0.1", b.ConsumedIP)
	}
}

func TestConsumeBootstrapInvalid(t *testing.T) {
	r := New(newBrowserTestDB(t), nil)
	ctx := context.Background()
	// Never minted.
	if _, err := r.ConsumeBootstrap(ctx, "btk_not-real", ""); !errors.Is(err, ErrBootstrapInvalid) {
		t.Errorf("unknown token: got %v, want ErrBootstrapInvalid", err)
	}
	// Minted but consumed.
	plaintext, _, _ := r.CreateBootstrap(ctx,
		"actor_test", "tnt_default", []string{"opstack:*:read"}, false, "", time.Minute)
	if _, err := r.ConsumeBootstrap(ctx, plaintext, ""); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := r.ConsumeBootstrap(ctx, plaintext, ""); !errors.Is(err, ErrBootstrapInvalid) {
		t.Errorf("second consume: got %v, want ErrBootstrapInvalid", err)
	}
}

func TestConsumeBootstrapExpired(t *testing.T) {
	r := New(newBrowserTestDB(t), nil)
	ctx := context.Background()
	plaintext, _, err := r.CreateBootstrap(ctx,
		"actor_test", "tnt_default", []string{"opstack:*:read"}, false, "", -1*time.Second)
	if err != nil {
		t.Fatalf("CreateBootstrap: %v", err)
	}
	if _, err := r.ConsumeBootstrap(ctx, plaintext, ""); !errors.Is(err, ErrBootstrapInvalid) {
		t.Errorf("expired token: got %v, want ErrBootstrapInvalid", err)
	}
}

// TestConsumeBootstrapRace — five goroutines try to consume the same
// token; exactly one wins. The conditional UPDATE in ConsumeBootstrap
// is what guarantees this.
func TestConsumeBootstrapRace(t *testing.T) {
	r := New(newBrowserTestDB(t), nil)
	ctx := context.Background()
	plaintext, _, _ := r.CreateBootstrap(ctx,
		"actor_test", "tnt_default", []string{"opstack:*:read"}, false, "", time.Minute)

	var (
		wg      sync.WaitGroup
		wins    int
		losses  int
		mu      sync.Mutex
		winners []*Bootstrap
	)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := r.ConsumeBootstrap(ctx, plaintext, "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				losses++
			} else {
				wins++
				winners = append(winners, b)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("wins = %d, want exactly 1", wins)
	}
	if losses != 4 {
		t.Errorf("losses = %d, want 4", losses)
	}
}

func TestSessionLifecycle(t *testing.T) {
	r := New(newBrowserTestDB(t), nil)
	ctx := context.Background()
	plaintext, _, _ := r.CreateBootstrap(ctx,
		"actor_test", "tnt_default", []string{"opstack:*:read"}, false, "", time.Minute)
	b, _ := r.ConsumeBootstrap(ctx, plaintext, "10.0.0.1")

	sess, err := r.CreateSession(ctx, b, "ua/test", "10.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !strings.HasPrefix(sess.SessionID, "bsn_") {
		t.Errorf("session_id = %q, want bsn_ prefix", sess.SessionID)
	}
	if !sess.IsValid(time.Now().UTC()) {
		t.Errorf("fresh session is not valid")
	}

	got, err := r.GetSession(ctx, sess.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ActorID != "actor_test" {
		t.Errorf("ActorID = %q, want actor_test", got.ActorID)
	}

	// Revoke and verify.
	if err := r.RevokeSession(ctx, sess.SessionID, "actor_test"); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	got, _ = r.GetSession(ctx, sess.SessionID)
	if got.IsValid(time.Now().UTC()) {
		t.Errorf("session is still valid after revoke")
	}
	if got.RevokedAt == nil {
		t.Errorf("RevokedAt is nil after revoke")
	}

	// Idempotent: revoking again is a no-op.
	if err := r.RevokeSession(ctx, sess.SessionID, "actor_test"); err != nil {
		t.Errorf("idempotent revoke: %v", err)
	}
}

func TestRevokeActorSessions(t *testing.T) {
	r := New(newBrowserTestDB(t), nil)
	ctx := context.Background()
	// Two sessions for actor_test, one for actor_other.
	if _, err := r.DB.Exec(`INSERT INTO actors VALUES ('actor_other')`); err != nil {
		t.Fatalf("seed actor: %v", err)
	}
	mkSess := func(actor string) {
		t.Helper()
		plaintext, _, _ := r.CreateBootstrap(ctx,
			actor, "tnt_default", []string{"opstack:*:read"}, false, "", time.Minute)
		b, _ := r.ConsumeBootstrap(ctx, plaintext, "")
		if _, err := r.CreateSession(ctx, b, "ua", "", time.Hour); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	mkSess("actor_test")
	mkSess("actor_test")
	mkSess("actor_other")

	if err := r.RevokeActorSessions(ctx, "actor_test", "admin"); err != nil {
		t.Fatalf("RevokeActorSessions: %v", err)
	}

	sessions, err := r.ListSessions(ctx, "tnt_default")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var revokedTest, activeOther int
	for _, s := range sessions {
		if s.ActorID == "actor_test" && s.RevokedAt != nil {
			revokedTest++
		}
		if s.ActorID == "actor_other" && s.RevokedAt == nil {
			activeOther++
		}
	}
	if revokedTest != 2 {
		t.Errorf("revoked actor_test sessions = %d, want 2", revokedTest)
	}
	if activeOther != 1 {
		t.Errorf("active actor_other sessions = %d, want 1", activeOther)
	}
}

func TestTouchSession(t *testing.T) {
	r := New(newBrowserTestDB(t), nil)
	ctx := context.Background()
	plaintext, _, _ := r.CreateBootstrap(ctx,
		"actor_test", "tnt_default", []string{"opstack:*:read"}, false, "", time.Minute)
	b, _ := r.ConsumeBootstrap(ctx, plaintext, "")
	sess, _ := r.CreateSession(ctx, b, "ua", "", time.Hour)
	initial := sess.LastSeenAt

	// Sleep just enough to step the formatted timestamp.
	time.Sleep(2 * time.Millisecond)

	if err := r.TouchSession(ctx, sess.SessionID, time.Now().UTC()); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, _ := r.GetSession(ctx, sess.SessionID)
	if !got.LastSeenAt.After(initial) {
		t.Errorf("LastSeenAt did not advance after Touch: before=%v after=%v",
			initial, got.LastSeenAt)
	}
}

// TestBootstrapCarriesSuperAdmin — the super_admin flag must round-trip
// bootstrap → session → reload, so verifyCookie can set
// auth.Context.SuperAdmin and RequireSuperAdmin gates on the real flag
// rather than treating every browser session as an operator.
func TestBootstrapCarriesSuperAdmin(t *testing.T) {
	r := New(newBrowserTestDB(t), nil)
	ctx := context.Background()

	// super_admin = true propagates all the way through.
	pt, _, err := r.CreateBootstrap(ctx,
		"actor_test", "tnt_default", []string{"admin:all"}, true, "", time.Minute)
	if err != nil {
		t.Fatalf("CreateBootstrap: %v", err)
	}
	b, err := r.ConsumeBootstrap(ctx, pt, "10.0.0.1")
	if err != nil {
		t.Fatalf("ConsumeBootstrap: %v", err)
	}
	if !b.SuperAdmin {
		t.Fatalf("bootstrap should carry super_admin")
	}
	sess, err := r.CreateSession(ctx, b, "ua", "10.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !sess.SuperAdmin {
		t.Fatalf("created session should carry super_admin")
	}
	if got, err := r.GetSession(ctx, sess.SessionID); err != nil || !got.SuperAdmin {
		t.Fatalf("reloaded session lost super_admin: err=%v", err)
	}

	// Default (false) round-trips false — a tenant member's session.
	pt2, _, _ := r.CreateBootstrap(ctx,
		"actor_test", "tnt_default", []string{"opstack:*:read"}, false, "", time.Minute)
	b2, _ := r.ConsumeBootstrap(ctx, pt2, "10.0.0.1")
	if b2.SuperAdmin {
		t.Fatalf("non-super bootstrap must be false")
	}
	s2, _ := r.CreateSession(ctx, b2, "ua", "", time.Hour)
	if g2, _ := r.GetSession(ctx, s2.SessionID); g2.SuperAdmin {
		t.Fatalf("non-super session must be false")
	}
}

// TestNewSessionID pins the security contract of the session cookie: it
// carries the bsn_ prefix, decodes to sessionRandomBytes of crypto/rand
// entropy, and never repeats. (Regression guard for the pre-fix bug where
// the id came from the process-global math/rand hxid stream.)
func TestNewSessionID(t *testing.T) {
	const n = 2000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id, err := newSessionID()
		if err != nil {
			t.Fatalf("newSessionID: %v", err)
		}
		if !strings.HasPrefix(id, sessionIDPrefix) {
			t.Fatalf("session id %q missing %q prefix", id, sessionIDPrefix)
		}
		raw, err := base58.Decode(strings.TrimPrefix(id, sessionIDPrefix))
		if err != nil {
			t.Fatalf("session id %q not base58: %v", id, err)
		}
		if len(raw) != sessionRandomBytes {
			t.Fatalf("session id %q decodes to %d bytes, want %d", id, len(raw), sessionRandomBytes)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q after %d mints", id, i)
		}
		seen[id] = true
	}
}
