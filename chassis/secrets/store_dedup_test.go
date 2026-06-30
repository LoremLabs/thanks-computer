package secrets

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// seedActiveSecretRow inserts one ACTIVE tenant_secrets row (+ its v1
// version) directly, mimicking CreateSecret WITHOUT its active-uniqueness
// guard. It's the only way to reproduce the duplicate-active-rows state a
// runtime DB missing tenant_secrets_active_name_idx could accumulate (the
// prod bug), since CreateSecret now refuses to create the second one.
func seedActiveSecretRow(t *testing.T, db *sql.DB, mk MasterKeyProvider, tenantID, secretID, name, value, createdAt string) {
	t.Helper()
	const versionNo = 1
	keyVer := mk.Version()
	es, err := Encrypt(mk, []byte(value),
		outerAAD(tenantID, secretID, versionNo, name, keyVer),
		innerAAD(tenantID, secretID, versionNo, keyVer))
	if err != nil {
		t.Fatalf("seed encrypt: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tenant_secrets
		(secret_id, tenant_id, stack, name, description, created_at, created_by, key_version)
		VALUES (?, ?, NULL, ?, '', ?, 'actor_seed', ?)`,
		secretID, tenantID, name, createdAt, keyVer); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tenant_secret_versions
		(version_id, secret_id, version_no, nonce, ciphertext, wrapped_dek, dek_nonce, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"sec_v_"+secretID, secretID, versionNo, es.Nonce, es.Ciphertext, es.WrappedDEK, es.DEKNonce, createdAt); err != nil {
		t.Fatalf("seed version: %v", err)
	}
}

// dropActiveNameIndex removes the partial unique index so a test DB matches
// a prod runtime DB whose 0008 ran before the index line existed (the
// condition that let `set` accumulate duplicate active rows).
func dropActiveNameIndex(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DROP INDEX IF EXISTS tenant_secrets_active_name_idx`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
}

// Even without the DB index, a second create of the same active name must be
// rejected by the in-code guard so the CLI `set` rotates instead of silently
// inserting a duplicate active row (the prod failure mode).
func TestCreateRejectsDuplicateWithoutIndex(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	dropActiveNameIndex(t, db)
	s := NewStore(db, newMockMK(t, 1))

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE_SECRET_KEY", "", "actor", []byte("sk_test_1")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE_SECRET_KEY", "", "actor", []byte("sk_live_2"))
	if !errors.Is(err, ErrSecretExists) {
		t.Fatalf("duplicate create without index should return ErrSecretExists, got: %v", err)
	}
}

// The guard is exact-scope: a stack-scoped secret and a tenant-wide secret of
// the same name are distinct and must coexist (not be rejected as duplicates).
func TestCreateGuardIsExactScope(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	dropActiveNameIndex(t, db)
	s := NewStore(db, newMockMK(t, 1))

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE_SECRET_KEY", "", "actor", []byte("tenantwide")); err != nil {
		t.Fatalf("tenant-wide create: %v", err)
	}
	pay := "payments"
	if _, err := s.CreateSecret(ctx, "tnt_x", &pay, "STRIPE_SECRET_KEY", "", "actor", []byte("scoped")); err != nil {
		t.Fatalf("stack-scoped create should coexist with tenant-wide: %v", err)
	}
}

// On a DB that already accumulated duplicate active rows, the resolver must
// deterministically materialize the NEWEST — so an operator's re-`set` takes
// effect instead of being shadowed by the oldest row (the exact prod symptom:
// a re-set key never took because the chassis kept resolving the first one).
func TestMaterializePicksNewestAmongDuplicateActives(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	dropActiveNameIndex(t, db)
	mk := newMockMK(t, 1)
	s := NewStore(db, mk)

	// Oldest first, then newer — the order successive re-`set`s would create.
	seedActiveSecretRow(t, db, mk, "tnt_x", "sec_old", "STRIPE_SECRET_KEY", "sk_test_OLD", "2026-01-01T00:00:00Z")
	seedActiveSecretRow(t, db, mk, "tnt_x", "sec_new", "STRIPE_SECRET_KEY", "sk_live_NEW", "2026-02-01T00:00:00Z")

	got, meta, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "STRIPE_SECRET_KEY")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if string(got) != "sk_live_NEW" {
		t.Errorf("resolved %q, want sk_live_NEW (newest active row)", got)
	}
	if meta.SecretID != "sec_new" {
		t.Errorf("resolved secret_id %q, want sec_new", meta.SecretID)
	}
}
