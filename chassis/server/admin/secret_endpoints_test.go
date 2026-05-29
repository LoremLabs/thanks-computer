package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

const testTenantID = "tnt_default"

// wireSecrets attaches a real secrets.Resolver to the test
// Controller's processor.Unit, backed by a file master key and the
// in-memory test DB. Tests that need the secret store available call
// this; tests that want to exercise the "store unavailable" path
// skip it.
func wireSecrets(t *testing.T, c *Controller) {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := secrets.MintFileMasterKey(keyPath); err != nil {
		t.Fatalf("mint master key: %v", err)
	}
	mk, err := secrets.NewFileMasterKey(keyPath)
	if err != nil {
		t.Fatalf("load master key: %v", err)
	}
	store := secrets.NewStore(c.pu.RuntimeDB, mk)
	slugToID := func(ctx context.Context, slug string) (string, error) {
		var id string
		return id, c.pu.RuntimeDB.QueryRowContext(ctx,
			`SELECT tenant_id FROM tenants WHERE slug = ? AND revoked_at IS NULL`, slug,
		).Scan(&id)
	}
	c.pu.Secrets = secrets.NewResolver(store, slugToID)
}

func newTestControllerWithSecrets(t *testing.T) *Controller {
	t.Helper()
	c := newTestController(t, config.Config{Personalities: "admin"})
	wireSecrets(t, c)
	return c
}

// muxVars injects mux vars onto the request so handlers using
// mux.Vars(r) work in direct-call tests (no router involved).
func muxVars(r *http.Request, vars map[string]string) *http.Request {
	return mux.SetURLVars(r, vars)
}

// withTenantCapsCtx is like withTenantAdminCtx but lets the test
// specify exactly which capabilities the synthetic auth.Context
// carries — useful for exercising the read/write capability split.
func withTenantCapsCtx(req *http.Request, tenantID string, caps []string) *http.Request {
	c := &auth.Context{
		Source:       "signed",
		ActorID:      "actor_test",
		KeyID:        "key_test",
		Capabilities: caps,
		TenantID:     tenantID,
		TenantSlug:   "default",
	}
	return req.WithContext(auth.WithContext(req.Context(), c))
}

// ---------- Happy paths ----------

func TestCreateSecretHappy(t *testing.T) {
	c := newTestControllerWithSecrets(t)

	body, _ := json.Marshal(createSecretRequest{
		Name:        "STRIPE_API_KEY",
		Value:       "sk_live_abc",
		Description: "stripe live key",
	})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/secrets", bytes.NewBuffer(body)), testTenantID)
	c.handleCreateSecret(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var resp secretResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Secret.Name != "STRIPE_API_KEY" {
		t.Errorf("Name = %q, want STRIPE_API_KEY", resp.Secret.Name)
	}
	if resp.Secret.VersionNo != 1 {
		t.Errorf("VersionNo = %d, want 1", resp.Secret.VersionNo)
	}
	// Reveal-never: response body must not contain `"value"` anywhere.
	if bytes.Contains(w.Body.Bytes(), []byte(`"value"`)) {
		t.Errorf("create response contains 'value' field: %s", w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("sk_live_abc")) {
		t.Errorf("create response leaks cleartext: %s", w.Body.String())
	}
}

func TestGenerateSecretHappy(t *testing.T) {
	c := newTestControllerWithSecrets(t)

	body, _ := json.Marshal(generateSecretRequest{
		Name:        "FRESH_KEY",
		Description: "minted by chassis",
		ByteLen:     32,
	})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/secrets/generate", bytes.NewBuffer(body)), testTenantID)
	c.handleGenerateSecret(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var resp secretWithValueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Secret.Name != "FRESH_KEY" {
		t.Errorf("Name = %q, want FRESH_KEY", resp.Secret.Name)
	}
	if resp.Value == "" {
		t.Errorf("Value is empty — generate must return the cleartext exactly once")
	}
	// Value is base64-url no-padding of 32 random bytes → 43 chars.
	if len(resp.Value) != 43 {
		t.Errorf("Value length = %d, want 43 (base64-url of 32 bytes)", len(resp.Value))
	}
	// Round-trip decode to confirm it's valid base64-url and 32 bytes.
	dec, err := base64.RawURLEncoding.DecodeString(resp.Value)
	if err != nil {
		t.Errorf("Value is not valid base64-url: %v", err)
	}
	if len(dec) != 32 {
		t.Errorf("decoded value = %d bytes, want 32", len(dec))
	}
}

func TestGenerateSecretDefaultByteLen(t *testing.T) {
	c := newTestControllerWithSecrets(t)

	// No byte_len in body → defaults to 32.
	body, _ := json.Marshal(generateSecretRequest{Name: "DEFAULT_LEN"})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/secrets/generate", bytes.NewBuffer(body)), testTenantID)
	c.handleGenerateSecret(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var resp secretWithValueResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Value) != 43 { // 32 bytes → 43 base64-url chars
		t.Errorf("default byte_len: value len = %d, want 43", len(resp.Value))
	}
}

func TestListSecrets(t *testing.T) {
	c := newTestControllerWithSecrets(t)

	// Seed via the Store directly.
	store := c.pu.Secrets.Store()
	if _, err := store.CreateSecret(context.Background(), testTenantID, nil,
		"FIRST", "", "actor_test", []byte("v1")); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if _, err := store.CreateSecret(context.Background(), testTenantID, nil,
		"SECOND", "", "actor_test", []byte("v2")); err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	w := httptest.NewRecorder()
	r := withTenantAdminCtx(httptest.NewRequest(http.MethodGet,
		"/v1/tenants/default/secrets", nil), testTenantID)
	c.handleListSecrets(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var resp listSecretsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Secrets) != 2 {
		t.Errorf("got %d secrets, want 2", len(resp.Secrets))
	}
	// Reveal-never: no value field anywhere in list output.
	if bytes.Contains(w.Body.Bytes(), []byte(`"value"`)) {
		t.Errorf("list response contains 'value' field: %s", w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("v1")) || bytes.Contains(w.Body.Bytes(), []byte("v2")) {
		t.Errorf("list response leaks seeded cleartext: %s", w.Body.String())
	}
}

func TestShowSecret(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	store := c.pu.Secrets.Store()
	if _, err := store.CreateSecret(context.Background(), testTenantID, nil,
		"STRIPE", "stripe live", "actor_test", []byte("v")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxVars(httptest.NewRequest(http.MethodGet,
		"/v1/tenants/default/secrets/STRIPE", nil), map[string]string{"name": "STRIPE"}), testTenantID)
	c.handleShowSecret(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var resp secretResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Secret.Description != "stripe live" {
		t.Errorf("Description = %q, want 'stripe live'", resp.Secret.Description)
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"value"`)) {
		t.Errorf("show response contains 'value' field: %s", w.Body.String())
	}
}

func TestUpdateSecretDescription(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	store := c.pu.Secrets.Store()
	if _, err := store.CreateSecret(context.Background(), testTenantID, nil,
		"STRIPE", "old desc", "actor_test", []byte("v")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(updateSecretDescriptionRequest{Description: "new desc"})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxVars(httptest.NewRequest(http.MethodPatch,
		"/v1/tenants/default/secrets/STRIPE", bytes.NewBuffer(body)),
		map[string]string{"name": "STRIPE"}), testTenantID)
	c.handleUpdateSecretDescription(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var resp secretResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Secret.Description != "new desc" {
		t.Errorf("Description = %q, want 'new desc'", resp.Secret.Description)
	}
}

func TestRotateSecret(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	store := c.pu.Secrets.Store()
	if _, err := store.CreateSecret(context.Background(), testTenantID, nil,
		"STRIPE", "", "actor_test", []byte("v1")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(rotateSecretRequest{Value: "v2"})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxVars(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/secrets/STRIPE/rotate", bytes.NewBuffer(body)),
		map[string]string{"name": "STRIPE"}), testTenantID)
	c.handleRotateSecret(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var resp secretResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Secret.VersionNo != 2 {
		t.Errorf("VersionNo = %d, want 2", resp.Secret.VersionNo)
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"value"`)) {
		t.Errorf("rotate response contains 'value' field: %s", w.Body.String())
	}
}

func TestRotateSecretGenerated(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	store := c.pu.Secrets.Store()
	if _, err := store.CreateSecret(context.Background(), testTenantID, nil,
		"STRIPE", "", "actor_test", []byte("v1")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxVars(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/secrets/STRIPE/rotate-generated", nil),
		map[string]string{"name": "STRIPE"}), testTenantID)
	c.handleRotateSecretGenerated(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var resp secretWithValueResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Secret.VersionNo != 2 {
		t.Errorf("VersionNo = %d, want 2", resp.Secret.VersionNo)
	}
	if resp.Value == "" {
		t.Errorf("rotate-generated must return the value once")
	}
}

func TestRevokeSecret(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	store := c.pu.Secrets.Store()
	if _, err := store.CreateSecret(context.Background(), testTenantID, nil,
		"STRIPE", "", "actor_test", []byte("v")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxVars(httptest.NewRequest(http.MethodDelete,
		"/v1/tenants/default/secrets/STRIPE", nil),
		map[string]string{"name": "STRIPE"}), testTenantID)
	c.handleRevokeSecret(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}

	// And the row should be unfindable on subsequent show.
	w2 := httptest.NewRecorder()
	r2 := withTenantAdminCtx(muxVars(httptest.NewRequest(http.MethodGet,
		"/v1/tenants/default/secrets/STRIPE", nil),
		map[string]string{"name": "STRIPE"}), testTenantID)
	c.handleShowSecret(w2, r2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("after revoke, show: got %d, want 404", w2.Code)
	}
}

// ---------- Structural invariants ----------

// TestCreateRejectsImmutableName covers the design §1.7 invariant:
// the create body must include a `name` (so this isn't really about
// rejecting it), but a PATCH attempt to rename must fail loud.
// Renamed below in TestPatchRejectsRename.

func TestPatchRejectsRename(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	store := c.pu.Secrets.Store()
	if _, err := store.CreateSecret(context.Background(), testTenantID, nil,
		"STRIPE", "old", "actor_test", []byte("v")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Attempt to PATCH with a `name` field in the body — must reject.
	body := []byte(`{"description": "new", "name": "RENAMED"}`)
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxVars(httptest.NewRequest(http.MethodPatch,
		"/v1/tenants/default/secrets/STRIPE", bytes.NewBuffer(body)),
		map[string]string{"name": "STRIPE"}), testTenantID)
	c.handleUpdateSecretDescription(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("rename attempt: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "name_immutable") {
		t.Errorf("error body should include name_immutable; got: %s", w.Body.String())
	}
}

func TestCreateRejectsDuplicate(t *testing.T) {
	c := newTestControllerWithSecrets(t)

	body, _ := json.Marshal(createSecretRequest{Name: "DUP", Value: "v1"})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/secrets", bytes.NewBuffer(body)), testTenantID)
	c.handleCreateSecret(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", w.Code, w.Body.String())
	}

	w2 := httptest.NewRecorder()
	r2 := withTenantAdminCtx(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/secrets", bytes.NewBuffer(body)), testTenantID)
	c.handleCreateSecret(w2, r2)
	if w2.Code != http.StatusConflict {
		t.Errorf("duplicate create: got %d, want 409; body=%s", w2.Code, w2.Body.String())
	}
}

func TestCreateRejectsInvalidName(t *testing.T) {
	c := newTestControllerWithSecrets(t)

	// Case is no longer enforced; the name must still be an identifier
	// (start with a letter, then letters/digits/underscore).
	for _, bad := range []string{"Has-Dash", "1LEAD", "_leading", "Has Space"} {
		t.Run(bad, func(t *testing.T) {
			body, _ := json.Marshal(createSecretRequest{Name: bad, Value: "v"})
			w := httptest.NewRecorder()
			r := withTenantAdminCtx(httptest.NewRequest(http.MethodPost,
				"/v1/tenants/default/secrets", bytes.NewBuffer(body)), testTenantID)
			c.handleCreateSecret(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("bad name %q: got %d, want 400", bad, w.Code)
			}
		})
	}
}

// Lowercase / mixed-case names are accepted — uppercase is convention,
// not enforcement.
func TestCreateAcceptsLowercaseName(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	body, _ := json.Marshal(createSecretRequest{Name: "stripe_key", Value: "v"})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/secrets", bytes.NewBuffer(body)), testTenantID)
	c.handleCreateSecret(w, r)
	if w.Code != http.StatusCreated {
		t.Errorf("lowercase name: got %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

func TestShowSecretNotFound(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxVars(httptest.NewRequest(http.MethodGet,
		"/v1/tenants/default/secrets/MISSING", nil),
		map[string]string{"name": "MISSING"}), testTenantID)
	c.handleShowSecret(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestSecretsStoreUnavailable(t *testing.T) {
	// Controller without wireSecrets — pu.Secrets stays nil.
	c := newTestController(t, config.Config{Personalities: "admin"})

	body, _ := json.Marshal(createSecretRequest{Name: "STRIPE", Value: "v"})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/secrets", bytes.NewBuffer(body)), testTenantID)
	c.handleCreateSecret(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("no master key: got %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "secret_store_unavailable") {
		t.Errorf("expected secret_store_unavailable, got: %s", w.Body.String())
	}
}

// ---------- Capability gating ----------

func TestRequiresCapabilityWrite(t *testing.T) {
	c := newTestControllerWithSecrets(t)

	// Build a request with a deliberately weak context (read-only).
	body, _ := json.Marshal(createSecretRequest{Name: "K", Value: "v"})
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/secrets", bytes.NewBuffer(body))
	req = withTenantCapsCtx(req, testTenantID, []string{"secret:*:read"})

	w := httptest.NewRecorder()
	c.handleCreateSecret(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("read-only ctx + create: got %d, want 403", w.Code)
	}
}

func TestRequiresCapabilityRead(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/default/secrets", nil)
	req = withTenantCapsCtx(req, testTenantID, []string{"secret:*:write"})
	w := httptest.NewRecorder()
	c.handleListSecrets(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("write-only ctx + list: got %d, want 403", w.Code)
	}
}
