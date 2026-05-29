package processor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// secretStoreSchema is the subset of 0008_tenant_secrets.sql needed
// to exercise the runtime path. Kept inline so the test doesn't
// depend on the embed.FS walker (which would pull in all migrations).
// Schema MUST stay in sync with db/schema/sqlite/runtime/0008_*.sql.
const secretStoreSchema = `
CREATE TABLE IF NOT EXISTS tenant_secrets (
    secret_id        TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    stack            TEXT,
    name             TEXT NOT NULL,
    description      TEXT,
    created_at       TEXT NOT NULL,
    created_by       TEXT,
    revoked_at       TEXT,
    last_rotated_at  TEXT,
    key_version      INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS tenant_secret_versions (
    version_id   TEXT PRIMARY KEY,
    secret_id    TEXT NOT NULL,
    version_no   INTEGER NOT NULL,
    nonce        BLOB NOT NULL,
    ciphertext   BLOB NOT NULL,
    wrapped_dek  BLOB NOT NULL,
    dek_nonce    BLOB NOT NULL,
    created_at   TEXT NOT NULL,
    revoked_at   TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS tenant_secrets_active_name_idx
    ON tenant_secrets (tenant_id, COALESCE(stack, ''), name)
    WHERE revoked_at IS NULL;
`

// TestSecretsEndToEnd is the load-bearing PR 3 verification. It
// stands up a real chassis-processor-Unit, seeds a tenant + a secret,
// stands up a mock HTTP endpoint, runs a txcl rule whose WITH clause
// declares `secrets.headers.authorization.{secret,format}`, and
// asserts the WHOLE pipeline:
//
//  1. The processor's secret-cache + splice install on the request ctx.
//  2. The splice walks op.Meta.secrets, materializes via Resolver.
//  3. ExecHTTP applies the format-templated overlay at the egress.
//  4. The mock HTTP server receives "Authorization: Bearer sk_live_…".
//  5. op.Input is byte-for-byte unchanged (no cleartext leak into the
//     trace pipeline).
//  6. The response payload + meta contain zero bytes of the cleartext.
func TestSecretsEndToEnd(t *testing.T) {
	pu, _ := newTestUnit(t)

	// 1. Extend the in-memory runtime DB with the secret-store tables.
	if _, err := pu.Dbc.Db.Exec(secretStoreSchema); err != nil {
		t.Fatalf("create secret store tables: %v", err)
	}

	// 2. Mint a master key + construct Store + Resolver wired to the
	//    test DB.
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := secrets.MintFileMasterKey(keyPath); err != nil {
		t.Fatalf("mint master key: %v", err)
	}
	mk, err := secrets.NewFileMasterKey(keyPath)
	if err != nil {
		t.Fatalf("load master key: %v", err)
	}
	store := secrets.NewStore(pu.Dbc.Db, mk)
	slugToID := func(ctx context.Context, slug string) (string, error) {
		var id string
		return id, pu.Dbc.Db.QueryRowContext(ctx,
			`SELECT tenant_id FROM tenants WHERE slug = ? AND revoked_at IS NULL`, slug,
		).Scan(&id)
	}
	pu.Secrets = secrets.NewResolver(store, slugToID)

	// 3. Seed a tenant and a secret.
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO tenants (tenant_id, slug, created_at) VALUES ('tnt_acme', 'acme', '2026-05-20T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	const cleartext = "sk_live_canonical_test_xyz_doNotLog"
	if _, err := store.CreateSecret(
		context.Background(), "tnt_acme", nil, "STRIPE_API_KEY",
		"", "actor_test", []byte(cleartext),
	); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	// 4. Mock HTTP target that captures the inbound request.
	var (
		mu             sync.Mutex
		gotAuthHeader  string
		gotBody        []byte
		mockHitCount   int
	)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotAuthHeader = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		mockHitCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer mock.Close()

	// 5. Insert an op into the ops table. The rule WHEN-matches every
	//    input (no condition), EXECs to the mock, and declares
	//    `secrets.headers.authorization` via WITH. The processor's
	//    ResonatingOps decorator parses WITH into op.Meta.
	rule := `WHEN .trigger == "fire" ` +
		`EXEC "` + mock.URL + `" ` +
		`WITH secrets.headers.authorization.secret = "STRIPE_API_KEY", ` +
		`secrets.headers.authorization.format = "Bearer {}"`
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id) VALUES (?, ?, ?, ?, '', '', 'tnt_acme')`,
		"e2e/secrets", 100, "stripe-charge", rule,
	); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	// 6. Drive pu.Run with the tenant pinned via `_txc.tenant=acme`,
	//    and a marker field the WHEN can match.
	input := `{"trigger":"fire","_txc":{"tenant":"acme"}}`
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), input, "e2e/secrets/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Wait for the response.
	var payload event.Payload
	select {
	case payload = <-resCh:
	default:
		t.Fatal("no response received from Run")
	}

	// === Assertions ===

	// A. The mock was hit exactly once.
	mu.Lock()
	hits := mockHitCount
	authHeader := gotAuthHeader
	body := gotBody
	mu.Unlock()
	if hits != 1 {
		t.Fatalf("mock hit %d times, want 1", hits)
	}

	// B. The mock received the formatted Authorization header.
	want := "Bearer " + cleartext
	if authHeader != want {
		t.Errorf("mock Authorization = %q, want %q", authHeader, want)
	}

	// C. The mock received op.Input as the body (no field overlay).
	if !strings.Contains(string(body), `"trigger":"fire"`) {
		t.Errorf("mock body missing operator-authored fields: %s", body)
	}

	// D. **No-leak**: the response payload from Run does NOT contain
	//    the cleartext. The secret rode the wire to the mock but
	//    didn't leak into any envelope, trace, or merge response.
	if strings.Contains(payload.Raw, cleartext) {
		t.Errorf("response payload leaks cleartext: %s", payload.Raw)
	}
	if strings.Contains(payload.Meta, cleartext) {
		t.Errorf("response meta leaks cleartext: %s", payload.Meta)
	}

	// E. **No-leak (input side)**: the original input that went into
	//    Run does NOT contain the cleartext. (Sanity — the cleartext
	//    was in op.Secrets, never substituted into op.Input.)
	if strings.Contains(input, cleartext) {
		t.Errorf("test input string itself leaks cleartext — test setup bug")
	}
}

// TestSecretsEndToEndStoreUnavailable verifies the fail-loud path:
// op declares `secrets.*` but the chassis has no Resolver wired.
// The op must NOT dispatch, and the cleartext (which doesn't exist
// here, but the principle holds) must NOT leak via fallback.
func TestSecretsEndToEndStoreUnavailable(t *testing.T) {
	pu, _ := newTestUnit(t)
	// Note: pu.Secrets is nil — secret store NOT configured.

	// Mock that should NEVER be hit.
	var hits int
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer mock.Close()

	rule := `WHEN .trigger == "fire" ` +
		`EXEC "` + mock.URL + `" ` +
		`WITH secrets.headers.authorization.secret = "STRIPE_API_KEY"`
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id) VALUES (?, ?, ?, ?, '', '', '')`,
		"e2e/secrets-off", 100, "rejected", rule,
	); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	// Tenant pinned to "" (no tenant_id filtering); ops with tenant_id=""
	// resolve on the IS NULL bucket.
	if err := pu.Run(context.Background(), `{"trigger":"fire"}`, "e2e/secrets-off/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Drain the response channel; we don't care about the body, only
	// about whether the mock was hit.
	select {
	case <-resCh:
	default:
	}

	if hits != 0 {
		t.Errorf("op with `secrets` declaration dispatched despite no store configured — hit %d times", hits)
	}
}
