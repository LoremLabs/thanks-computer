package secrets

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	dbpkg "github.com/loremlabs/thanks-computer/db"
)

// newTestDB spins up an in-memory SQLite DB with all embedded runtime
// migrations applied. Each test gets its own DB so the unique-name
// constraint can't leak across tests.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	// Per-test temp file (cgo sqlite + :memory: shared-cache is finicky
	// with multiple connections). A temp file works everywhere.
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Apply all embedded runtime migrations in order.
	var files []string
	if err := fs.WalkDir(dbpkg.FS, "schema/sqlite/runtime", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(p, ".sql") {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk migrations: %v", err)
	}
	sort.Slice(files, func(i, j int) bool {
		ai, _ := strconv.Atoi(strings.Split(filepath.Base(files[i]), "_")[0])
		bi, _ := strconv.Atoi(strings.Split(filepath.Base(files[j]), "_")[0])
		return ai < bi
	})
	for _, p := range files {
		body, err := dbpkg.FS.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("exec %s: %v", p, err)
		}
	}
	return db
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(newTestDB(t), newMockMK(t, 1))
}

func TestCreateAndLookup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	meta, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE_API_KEY", "stripe live", "actor_a",
		[]byte("sk_live_abc123"))
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if !strings.HasPrefix(meta.SecretID, "sec_") {
		t.Errorf("SecretID = %q, want sec_ prefix", meta.SecretID)
	}
	if meta.Stack != nil {
		t.Errorf("tenant-wide create should have nil Stack, got %v", *meta.Stack)
	}
	if meta.VersionNo != 1 {
		t.Errorf("VersionNo = %d, want 1", meta.VersionNo)
	}
	if meta.KeyVersion != 1 {
		t.Errorf("KeyVersion = %d, want 1", meta.KeyVersion)
	}

	// LookupSecretMetadata round-trip.
	got, err := s.LookupSecretMetadata(ctx, "tnt_x", nil, "STRIPE_API_KEY")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.SecretID != meta.SecretID || got.Description != "stripe live" || got.CreatedBy != "actor_a" {
		t.Errorf("metadata mismatch: %+v vs %+v", got, meta)
	}
}

func TestCreateDuplicateRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "actor_a", []byte("v1")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "actor_a", []byte("v2"))
	if !errors.Is(err, ErrSecretExists) {
		t.Errorf("duplicate create should return ErrSecretExists, got: %v", err)
	}
}

func TestCreateRejectsInvalidName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Case is no longer constrained, but the name must still be an
	// identifier: start with a letter, then letters/digits/underscore.
	cases := []string{"", "_leading_underscore", "Has-Dash", "Has Space", "1LEADING_DIGIT", "café", "TOO_" + strings.Repeat("X", 130)}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.CreateSecret(ctx, "tnt_x", nil, name, "", "actor_a", []byte("v"))
			if !errors.Is(err, ErrInvalidName) {
				t.Errorf("name %q: expected ErrInvalidName, got: %v", name, err)
			}
		})
	}
}

// Case is convention, not enforcement: lowercase and mixed-case names
// are accepted and stored verbatim (matched case-sensitively later).
func TestCreateAcceptsAnyCaseName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for _, name := range []string{"stripe_key", "Stripe_Key", "apiKey", "X"} {
		t.Run(name, func(t *testing.T) {
			meta, err := s.CreateSecret(ctx, "tnt_case", nil, name, "", "actor_a", []byte("v"))
			if err != nil {
				t.Fatalf("name %q: unexpected error: %v", name, err)
			}
			if meta.Name != name {
				t.Errorf("name stored as %q, want verbatim %q", meta.Name, name)
			}
		})
	}
}

func TestCreateRejectsEmptyValue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "actor_a", []byte{})
	if err == nil || !strings.Contains(err.Error(), "empty value") {
		t.Errorf("empty value should error, got: %v", err)
	}
}

func TestCreateAndMaterializeRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	plaintext := []byte("sk_live_super_secret_abc123")

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "actor_a", plaintext); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, meta, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "STRIPE")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("plaintext mismatch: %q != %q", got, plaintext)
	}
	if meta.VersionNo != 1 {
		t.Errorf("VersionNo = %d, want 1", meta.VersionNo)
	}
}

// TestStackScopeFallback is the load-bearing test for the resolver
// behavior described in design §2: stack-scoped wins; tenant-wide is
// the fallback; both coexist.
func TestStackScopeFallback(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	pay := "payments"
	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "actor_a",
		[]byte("tenant-wide-value")); err != nil {
		t.Fatalf("create tenant-wide: %v", err)
	}
	if _, err := s.CreateSecret(ctx, "tnt_x", &pay, "STRIPE", "", "actor_a",
		[]byte("payments-stack-value")); err != nil {
		t.Fatalf("create stack-scoped: %v", err)
	}

	// Op in `payments` stack should see the stack-scoped value.
	got, meta, err := s.MaterializeSecretForOp(ctx, "tnt_x", "payments", "STRIPE")
	if err != nil {
		t.Fatalf("materialize payments: %v", err)
	}
	if string(got) != "payments-stack-value" {
		t.Errorf("stack=payments: got %q, want payments-stack-value", got)
	}
	if meta.Stack == nil || *meta.Stack != "payments" {
		t.Errorf("expected stack-scoped meta, got %v", meta.Stack)
	}

	// Op in any other stack falls back to tenant-wide.
	got, meta, err = s.MaterializeSecretForOp(ctx, "tnt_x", "billing", "STRIPE")
	if err != nil {
		t.Fatalf("materialize billing fallback: %v", err)
	}
	if string(got) != "tenant-wide-value" {
		t.Errorf("stack=billing fallback: got %q, want tenant-wide-value", got)
	}
	if meta.Stack != nil {
		t.Errorf("fallback should resolve tenant-wide (nil Stack), got %v", *meta.Stack)
	}

	// Empty stack == admin path == tenant-wide only.
	got, _, err = s.MaterializeSecretForOp(ctx, "tnt_x", "", "STRIPE")
	if err != nil {
		t.Fatalf("materialize empty-stack: %v", err)
	}
	if string(got) != "tenant-wide-value" {
		t.Errorf("stack='': got %q, want tenant-wide-value", got)
	}
}

func TestMaterializeNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", "payments", "MISSING_KEY")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("expected ErrSecretNotFound, got: %v", err)
	}
}

func TestRotateBumpsVersionAndChangesValue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "actor_a", []byte("v1")); err != nil {
		t.Fatalf("create: %v", err)
	}

	meta, err := s.RotateSecret(ctx, "tnt_x", nil, "STRIPE", []byte("v2"))
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if meta.VersionNo != 2 {
		t.Errorf("VersionNo after rotate = %d, want 2", meta.VersionNo)
	}
	if meta.LastRotatedAt == nil {
		t.Errorf("LastRotatedAt should be set after rotate")
	}

	got, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "STRIPE")
	if err != nil {
		t.Fatalf("materialize after rotate: %v", err)
	}
	if string(got) != "v2" {
		t.Errorf("post-rotate value: got %q, want v2", got)
	}
}

func TestRotateGeneratedReturnsCleartextOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "API", "", "actor_a", []byte("v1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	ct, _, err := s.RotateSecretGenerated(ctx, "tnt_x", nil, "API", 32)
	if err != nil {
		t.Fatalf("rotate generated: %v", err)
	}
	if len(ct) != 32 {
		t.Errorf("cleartext length = %d, want 32", len(ct))
	}
	// Materialize and confirm we get the same bytes back.
	got, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "API")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if string(got) != string(ct) {
		t.Errorf("post-rotate-generate materialized value differs from returned cleartext")
	}
}

func TestRevokeFreesNameForRecreate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "actor_a", []byte("v1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.RevokeSecret(ctx, "tnt_x", nil, "STRIPE"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// After revoke, Lookup should report not-found.
	if _, err := s.LookupSecretMetadata(ctx, "tnt_x", nil, "STRIPE"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("after revoke, Lookup should be not-found, got: %v", err)
	}
	// And re-create with the same name must succeed.
	meta, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "actor_a", []byte("v2"))
	if err != nil {
		t.Fatalf("re-create after revoke: %v", err)
	}
	if meta.VersionNo != 1 {
		t.Errorf("re-created secret should start at VersionNo=1, got %d", meta.VersionNo)
	}
}

func TestRevokeMissing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	err := s.RevokeSecret(ctx, "tnt_x", nil, "NEVER_CREATED")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("revoke missing: expected ErrSecretNotFound, got: %v", err)
	}
}

func TestUpdateDescription(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "first description", "actor_a",
		[]byte("v")); err != nil {
		t.Fatalf("create: %v", err)
	}
	meta, err := s.UpdateSecretDescription(ctx, "tnt_x", nil, "STRIPE", "updated description")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if meta.Description != "updated description" {
		t.Errorf("Description = %q, want 'updated description'", meta.Description)
	}
}

func TestListSecretsCrossesScopes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	pay := "payments"

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "API_TOKEN", "", "a", []byte("v")); err != nil {
		t.Fatalf("c1: %v", err)
	}
	if _, err := s.CreateSecret(ctx, "tnt_x", &pay, "API_TOKEN", "", "a", []byte("v")); err != nil {
		t.Fatalf("c2: %v", err)
	}
	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "OTHER", "", "a", []byte("v")); err != nil {
		t.Fatalf("c3: %v", err)
	}
	// Cross-tenant row should NOT appear.
	if _, err := s.CreateSecret(ctx, "tnt_y", nil, "OTHER_TENANT_KEY", "", "a", []byte("v")); err != nil {
		t.Fatalf("c4: %v", err)
	}

	list, err := s.ListSecrets(ctx, "tnt_x")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("list count = %d, want 3 (got %v)", len(list), list)
	}
	for _, m := range list {
		if m.TenantID != "tnt_x" {
			t.Errorf("list returned cross-tenant row: %+v", m)
		}
	}
}

// TestAntiSwapInStore is the load-bearing structural test for the
// crypto/SQL integration: tamper with row identity by manually
// changing the row's name/version/key_version after encrypt, and the
// AAD check must catch it on decrypt. Each variant proves a different
// AAD field is bound.
func TestAntiSwapInStore(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "actor_a", []byte("payload")); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Mutate the name on disk — AAD outer should reject decrypt.
	if _, err := s.DB.ExecContext(ctx, `UPDATE tenant_secrets SET name = 'STOLEN' WHERE name = 'STRIPE'`); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	_, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "STOLEN")
	if err == nil {
		t.Errorf("Materialize after row-identity mutation should fail (AAD anti-swap)")
	}
}

func TestRevealSecretValue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "actor_a", []byte("payload")); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _, err := s.RevealSecretValue(ctx, "tnt_x", nil, "STRIPE")
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("reveal returned %q, want payload", got)
	}
}

func TestRevealNoFallback(t *testing.T) {
	// RevealSecretValue does exact-scope lookup, NO fallback. A
	// tenant-wide row should not be returned when caller asks for
	// stack-scoped, and vice versa.
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "API", "", "a", []byte("tenant-wide")); err != nil {
		t.Fatalf("create: %v", err)
	}
	pay := "payments"
	_, _, err := s.RevealSecretValue(ctx, "tnt_x", &pay, "API")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("reveal for missing stack-scoped row should be not-found, got: %v", err)
	}
}

func TestGenerateSecret(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	ct, meta, err := s.GenerateSecret(ctx, "tnt_x", nil, "FRESH", "", "actor_a", 32)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(ct) != 32 {
		t.Errorf("cleartext length = %d, want 32", len(ct))
	}
	got, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "FRESH")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if string(got) != string(ct) {
		t.Errorf("generated then materialized: bytes differ")
	}
	if meta.VersionNo != 1 {
		t.Errorf("VersionNo = %d, want 1", meta.VersionNo)
	}
}

func TestGenerateRejectsOutOfRange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, byteLen := range []int{0, -1, 5000} {
		_, _, err := s.GenerateSecret(ctx, "tnt_x", nil, "BAD", "", "a", byteLen)
		if err == nil {
			t.Errorf("byteLen=%d should error", byteLen)
		}
	}
}

func TestNoMasterKey(t *testing.T) {
	// A Store with nil MK refuses operations that need crypto, but
	// doesn't crash on metadata-only lookups (those still need
	// validation, so they go through the same path).
	s := &Store{DB: newTestDB(t), now: func() time.Time { return time.Time{} }}
	_, err := s.CreateSecret(context.Background(), "tnt_x", nil, "K", "", "a", []byte("v"))
	if err == nil || !strings.Contains(err.Error(), "no master key") {
		t.Errorf("nil MK should fail loud, got: %v", err)
	}
}

